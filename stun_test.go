package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/stun/v3"
)

func TestSTUNAnswersABindingRequestWithTheSendersAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := probe.LocalAddr().String()
	probe.Close()
	go serveSTUN(ctx, server, NewRateLimiter(60, time.Minute))
	time.Sleep(100 * time.Millisecond)

	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	to, _ := net.ResolveUDPAddr("udp", server)
	request := stun.MustBuild(stun.TransactionID, stun.BindingRequest, stun.Fingerprint)
	if _, err := client.WriteTo(request.Raw, to); err != nil {
		t.Fatal(err)
	}
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := client.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no reply: %s", err)
	}
	reply := &stun.Message{Raw: buf[:n]}
	if err := reply.Decode(); err != nil {
		t.Fatal(err)
	}
	if reply.Type != stun.BindingSuccess || reply.TransactionID != request.TransactionID {
		t.Fatalf("reply %v for transaction %x", reply.Type, reply.TransactionID)
	}
	var mapped stun.XORMappedAddress
	if err := mapped.GetFrom(reply); err != nil {
		t.Fatal(err)
	}
	local := client.LocalAddr().(*net.UDPAddr)
	if !mapped.IP.Equal(local.IP) || mapped.Port != local.Port {
		t.Fatalf("mapped %s, want %s", mapped.String(), local)
	}
	if len(reply.Raw) > 2*len(request.Raw)+16 {
		t.Fatalf("reply of %d bytes amplifies a %d byte request", len(reply.Raw), len(request.Raw))
	}
}

func TestSTUNIgnoresAnythingButABindingRequest(t *testing.T) {
	from := &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 4000}
	if _, ok := stunReply([]byte("GET / HTTP/1.1\r\n\r\n"), from); ok {
		t.Fatal("answered junk")
	}
	indication := stun.MustBuild(stun.TransactionID, stun.NewType(stun.MethodBinding, stun.ClassIndication))
	if _, ok := stunReply(indication.Raw, from); ok {
		t.Fatal("answered an indication")
	}
	response := stun.MustBuild(stun.TransactionID, stun.BindingSuccess)
	if _, ok := stunReply(response.Raw, from); ok {
		t.Fatal("answered a response (reflection loop)")
	}
}
