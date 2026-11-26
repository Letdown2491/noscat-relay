package main

import (
	"context"
	"errors"
	"log"
	"net"
	"time"

	"github.com/pion/stun/v3"
)

// serveSTUN answers STUN binding requests (RFC 8489) on a UDP address, so a
// mesh's devices learn their public address from the mesh's own relay
// instead of a third party's STUN server. It answers binding requests and
// nothing else, with XOR-MAPPED-ADDRESS only: no relaying (that is TURN),
// no authentication, no state. A reply is about the size of the request,
// so it cannot amplify; the per-IP limit keeps a spoofed flood from turning
// it into a reflector.
func serveSTUN(ctx context.Context, addr string, limit *RateLimiter) error {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	log.Printf("STUN on udp %s", conn.LocalAddr())

	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			log.Printf("stun: read: %s", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		udp, ok := from.(*net.UDPAddr)
		if !ok || !limit.Allow(udp.IP.String()) {
			continue
		}
		reply, ok := stunReply(buf[:n], udp)
		if !ok {
			continue
		}
		if _, err := conn.WriteTo(reply, from); err != nil {
			log.Printf("stun: write to %s: %s", from, err)
		}
	}
}

// stunReply builds the success response for a binding request from `from`,
// or reports false for anything that is not a well-formed binding request.
func stunReply(packet []byte, from *net.UDPAddr) ([]byte, bool) {
	if !stun.IsMessage(packet) {
		return nil, false
	}
	request := &stun.Message{Raw: append([]byte(nil), packet...)}
	if err := request.Decode(); err != nil || request.Type != stun.BindingRequest {
		return nil, false
	}
	response, err := stun.Build(
		stun.NewTransactionIDSetter(request.TransactionID),
		stun.BindingSuccess,
		&stun.XORMappedAddress{IP: from.IP, Port: from.Port},
		stun.Fingerprint,
	)
	if err != nil {
		return nil, false
	}
	return response.Raw, true
}
