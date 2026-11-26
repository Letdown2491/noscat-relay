# noscat-relay

A members-only Nostr relay for one [noscat](https://github.com/Letdown2491/noscat) mesh. It carries only noscat's events, and only for the mesh's devices.

Run in place of public relays, it keeps a mesh's metadata off third-party infrastructure: which devices are in it, who gets invited, when each device is online. The storage load (adverts every two minutes per device) lands on your box instead of someone else's. Design and rationale: [noscat#18](https://github.com/Letdown2491/noscat/issues/18).

Forked from [nip46-relay](https://github.com/Letdown2491/nip46-relay), itself based on [Bunklay](https://github.com/dezh-tech/ddsr) from dezh-tech. Built on [khatru](https://github.com/fiatjaf/khatru).

## What it accepts

Every connection logs in with its device key (NIP-42) before it reads or writes anything.

| Kind | What | Write | Read |
|---|---|---|---|
| 445 | Group messages (commits, adverts) | members | members, scoped by `#h` |
| 1059 | Gift wraps (invitations, join requests) | anyone logged in | only those addressed to you (`#p`) |
| 30443 | Key packages | your own | members, scoped by author |
| 10050, 10002 | Inbox and relay lists | your own | anyone logged in, scoped by author |

Everything else is refused. A device that is not a member yet can still join through this relay alone: it publishes its key package, relay lists and a join request, and picks up its invitation. Group traffic stays members only.

- **Members** come from the mesh itself: the output of `noscat mesh members` (or a file), re-read every minute, and at once (at most every two seconds) when a logged-in key the relay does not know asks for group messages or key packages, so a device that just joined is let in right away. A failed read keeps the previous list.
- **Removal is immediate**: a removed device's open subscriptions stop receiving group messages, and its next query or event is refused.
- **Retention**: group messages and gift wraps are deleted after 30 days (configurable). A device offline for longer cannot catch up and must rejoin. Relay lists and key packages are replaceable and kept.
- **Timestamps**: within 15 minutes of now; gift wraps may be backdated up to two days, as NIP-59 allows.
- **Rate limits** per logged-in member, and per joining device and per IP. Group messages and gift wraps are signed by a fresh random key per event, so limits by author would not work.
- **Storage**: badger on disk, synchronous writes (a lost commit would split the mesh), tuned for a 1 GB box.

## Running it

On the server that runs the mesh's reachable member (`noscat --reachable service install`), as the same user:

```bash
git clone https://github.com/Letdown2491/noscat-relay ~/noscat-relay
cd ~/noscat-relay
go build -trimpath -ldflags="-s -w" -o noscat-relay .
cp .env.example .env
nano .env   # RELAY_URL, and MEMBERS_COMMAND if noscat is elsewhere
```

As a systemd user service, next to noscat's:

```bash
mkdir -p ~/.config/systemd/user
cp noscat-relay.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now noscat-relay
journalctl --user -u noscat-relay -f
```

Behind Caddy for TLS (see `Caddyfile.example`):

```caddyfile
relay.example.com {
    reverse_proxy localhost:3340
}
```

`RELAY_URL` must be exactly the public `wss://` URL: NIP-42 logins name the relay they are for.

Docker works too (`docker compose up -d`), with `MEMBERS_FILE` instead of `MEMBERS_COMMAND`, since a container cannot reach the host's noscat.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `RELAY_URL` | (required) | Public `wss://` URL |
| `MEMBERS_COMMAND` | | Command whose output lists the members (npubs or hex), e.g. `noscat mesh members` |
| `MEMBERS_FILE` | | File listing the members; merged with the command's output |
| `MEMBERS_REFRESH_SECONDS` | `60` | How often the member list is re-read |
| `GROUPS` | | Comma-separated `#h` routing ids to serve; empty = any |
| `RETENTION_DAYS` | `30` | Age at which group messages and gift wraps are deleted |
| `ACCEPT_WINDOW_IN_MINUTES` | `15` | Accepted clock skew |
| `MEMBER_RATE_PER_MINUTE` | `120` | Events per minute per member |
| `JOIN_RATE_PER_MINUTE` | `10` | Events per minute per joining device and per IP |
| `MAX_CONTENT_KB` | `256` | Largest event content |
| `RELAY_PORT` | `127.0.0.1:3340` | Listen address (plain WebSocket) |
| `STUN_ADDR` | (off) | UDP address for STUN, e.g. `:3478`. The mesh's devices learn their public address here instead of from a third party's STUN server. Open this UDP port on the host: Caddy proxies TCP only |
| `STUN_RATE_PER_MINUTE` | `60` | STUN requests per minute per source IP |
| `WORKING_DIR` | `./noscat-relay-data` | Data directory |
| `RELAY_NAME`, `RELAY_DESCRIPTION`, `RELAY_PUBKEY`, `RELAY_CONTACT`, `RELAY_ICON`, `RELAY_BANNER` | | NIP-11 information |

## Moving a mesh onto it

Needs noscat 0.5.0 or later. On any member:

```bash
noscat mesh relays add wss://relay.example.com
noscat mesh private        # devices use only the mesh's relays, and log in to them
noscat mesh relays remove wss://relay.snort.social   # and the other public ones
noscat mesh relays         # check
```

`mesh private` publishes on the relays in use until then, so members that are offline pick it up from the public relays when they come back. From then on every device uses only the mesh's relays and logs in to them with its device key; noscat never logs in to any other relay.

New devices join with a member's QR (`noscat mesh qr`, or the one `noscat up` prints), which now names the relay: `noscat:npub1...?relay=wss%3A%2F%2Frelay.example.com`. Scanning it in the apps, or `noscat mesh join '<that code>'`, joins through this relay alone. When the member whose QR was scanned is the noscat service on this server, approve the request there with `noscat mesh requests` (it asks y/n for each device that asked to join).

A new mesh can start here directly: `noscat --relay wss://relay.example.com up`, then `noscat mesh create <name>` and `noscat mesh private`.

## Development

```bash
go test -race ./...
```

The tests include an end-to-end run against a live relay: a member, a joining device and an outsider log in and exercise every rule.
