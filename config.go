package main

import (
	"log"
	"os"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
)

type Config struct {
	RelayName        string `envconfig:"RELAY_NAME" default:"noscat relay"`
	RelayPubkey      string `envconfig:"RELAY_PUBKEY"`
	RelayDescription string `envconfig:"RELAY_DESCRIPTION" default:"A members-only relay for a noscat mesh"`
	// The public wss:// URL. Required: NIP-42 logins name the relay they are
	// for, and a login for any other URL is refused.
	RelayURL     string `envconfig:"RELAY_URL"`
	RelayContact string `envconfig:"RELAY_CONTACT"`
	RelayIcon    string `envconfig:"RELAY_ICON"`
	RelayBanner  string `envconfig:"RELAY_BANNER"`

	WorkingDirectory string `envconfig:"WORKING_DIR" default:"./noscat-relay-data"`
	// Plain WebSocket listen address; TLS comes from a reverse proxy (Caddy).
	RelayPort string `envconfig:"RELAY_PORT" default:"127.0.0.1:3340"`

	// Who may use the relay: npubs or hex pubkeys from a file, the output of
	// a command (e.g. `noscat mesh members`), or both, re-read every
	// MEMBERS_REFRESH_SECONDS.
	MembersFile           string `envconfig:"MEMBERS_FILE"`
	MembersCommand        string `envconfig:"MEMBERS_COMMAND"`
	MembersRefreshSeconds int    `envconfig:"MEMBERS_REFRESH_SECONDS" default:"60"`

	// Comma-separated #h routing ids (hex) of the groups served; empty = any
	// group a member posts to.
	Groups []string `envconfig:"GROUPS"`

	// Group messages and gift wraps older than this are deleted. A device
	// offline for longer cannot catch up and must rejoin.
	RetentionDays int `envconfig:"RETENTION_DAYS" default:"30"`
	// Accepted clock skew for created_at, in minutes (gift wraps may also be
	// backdated up to two days, per NIP-59).
	AcceptWindowMinutes int `envconfig:"ACCEPT_WINDOW_IN_MINUTES" default:"15"`
	// Events per minute: per member, and per joining device and per IP.
	MemberRatePerMinute int `envconfig:"MEMBER_RATE_PER_MINUTE" default:"120"`
	JoinRatePerMinute   int `envconfig:"JOIN_RATE_PER_MINUTE" default:"10"`
	MaxContentKB        int `envconfig:"MAX_CONTENT_KB" default:"256"`

	// STUN on UDP, for the mesh's devices to learn their public address from
	// this relay instead of a third party (e.g. ":3478"). Empty = off. Caddy
	// proxies TCP only: this port is opened on the host directly.
	STUNAddr string `envconfig:"STUN_ADDR"`
	// STUN requests per minute per source IP.
	STUNRatePerMinute int `envconfig:"STUN_RATE_PER_MINUTE" default:"60"`
}

func LoadConfig() Config {
	// A local .env is read for bare runs; real environment variables (systemd
	// EnvironmentFile, docker env_file) take precedence.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: could not load .env: %s", err)
	}
	var c Config
	if err := envconfig.Process("", &c); err != nil {
		log.Fatalf("failed to read from env: %s", err)
	}
	return c
}
