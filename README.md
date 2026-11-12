# wormhole-mailbox

A Go port of [magic-wormhole-mailbox-server](https://github.com/magic-wormhole/magic-wormhole-mailbox-server),
the rendezvous server for [Magic Wormhole](https://magic-wormhole.io). It performs
store-and-forward delivery of small key-exchange and control messages between the two
sides of a wormhole. Bulk data flows elsewhere (direct TCP or a
[transit relay](https://github.com/magic-wormhole/magic-wormhole-transit-relay)).

**Status: MVP (phase 1) complete.** Phase 2 items are listed at the bottom.

## Compatibility

- Wire-compatible JSON-over-WebSocket protocol at `/v1`: `welcome`, `bind`, `ping`/`pong`,
  `list`, `allocate`, `claim`, `release`, `open`, `add`, `close`, `ack`/`error` semantics,
  `server_tx`/`server_rx` timestamps, and `your-address` in the welcome (including
  `X-Real-IP`/`X-Real-Port` override, like the reference).
- Database-compatible `channel-v2` SQLite schema with the same `version` row and the same
  `upgrade-channel-to-v2.sql` migration chain. DBs created by either implementation can be
  opened and served by the other (verified; see "Interop testing" below).
- Nameplate allocation, allocation preference for 1–3 digit ids, claim crowding rules,
  reclaimed-nameplate rejection, 11-minute channel expiry (5-minute check interval),
  message replay on `open`, echo-on-`add` delivery semantics, and mood handling on
  `close` all match the reference.

Tested with the unmodified Python `magic-wormhole` 0.24.0 CLI as both client and server peers.

## Building and running

```
go build -o mailbox-server ./cmd/mailbox-server
./mailbox-server --port tcp:4000 --channel-db relay.sqlite
```

Flags (names match the Python `twist wormhole-mailbox` plugin):

- `--port` — twisted-style endpoint (default `tcp:4000:interface=::`), or `host:port`/`PORT`
- `--channel-db` — path for the state database (default `relay.sqlite`), created/upgraded automatically
- `--blur-usage` — round logged access times to N seconds (affects future usage-db logging)
- `--advertise-version` — `current_cli_version` to put in `welcome`
- `--signal-error` — `error` message to put in `welcome` (clients terminate)
- `--motd` — `motd` to put in `welcome`
- `--disallow-list` — respond to `list` with an empty set
- `--websocket-protocol-option OPTION=VALUE` — accepted; autobahn-only options are ignored with a note
- `--usage-db`, `--addrid-db`, `--generation-duration` — phase 2, not implemented yet (the server exits with an error if used)

The server bumps `RLIMIT_NOFILE` to at least 10,000 if possible (best effort).

## Architecture

- `cmd/mailbox-server` — CLI entry point, pruning timer, signal handling
- `internal/mailbox/database.go` — open/create/upgrade machinery (`channel-v2`, `usage-v2`,
  `addrid-v1`), embedded verbatim SQL schemas (`db-schemas/`)
- `internal/mailbox/server.go` — `Server`/`AppNamespace`/`Mailbox` core state machine:
  nameplates, claims, mailboxes, messages, replay, close/summarize, pruning
- `internal/mailbox/websocket.go` — the wire protocol
- `internal/mailbox/http.go` — plain-text root at `/`, WebSocket upgrade at `/v1`,
  `Server: Magic Wormhole Mailbox <version>` header
- `internal/mailbox/rlimits.go` — rlimit bump

Concurrency model: a single mutex serializes all client commands, mirroring the Python
implementation's single-threaded reactor. Broadcasts happen synchronously under the same lock,
so a slow client can delay other clients (writes go to local sockets with a 10-second write
deadline; the Python implementation buffers writes in the reactor without a deadline, so it is
equally serialized for CPU-bound work).

## Testing

```
go test ./...            # unit + in-process end-to-end tests
go test ./... -race      # race detector
```

Interop testing (both directions of database compatibility, plus plain client/server):

```
python3 -m venv /tmp/wormhole-venv
/tmp/wormhole-venv/bin/pip install magic-wormhole magic-wormhole-mailbox-server
/tmp/wormhole-venv/bin/wormhole --relay-url ws://127.0.0.1:4101/v1 --transit-helper tcp:127.0.0.1:4001 send FILE
# ... receive similarly with --accept-file --output-file ...
```

Verified: Python file sent through the Go mailbox (fresh DB, Python-created DB), and a
Go-created DB served by the reference Python server at the same commit — full transfers with
md5-verified payloads.

## Intentional deviations from the reference

- **Pruning drops cached `Mailbox` objects** for deleted channels (the reference leaves the
  objects in memory; DB behavior and file contents are identical).
- **Mailbox close with outstanding nameplate claims**: the reference's `DELETE FROM
  nameplate_sides WHERE side=?` only deletes the closing side's claim rows and can violate the
  `nameplates(id)` foreign key, losing the mailbox contents. Instead we delete the whole
  nameplate's claim rows first; the outcome is the intended one with no leak and no FK error.
- **Invalid `claim` nameplate** and **JSON parse errors** produce an `error` frame instead of
  dropping the client (the reference kills the connection for a `ValueError`; for invalid JSON
  it drops the frame silently-ish). Clients get a structured error either way.
- **Claim crowding leaves residue** exactly like the reference (the third side keeps its
  claimed row and its mailbox side) — this is a latent reference bug we keep for parity; see
  `test_one_happy_one_jilted` analog in the relay project for how we handled similar cases.
- Nameplate-id allocation: random selection among available ids (reference does the same).
- When no free nameplate exists, the reference crashes the connection; we return an error frame.

## Phase 2 (planned, not yet implemented)

- `--usage-db`: `nameplates`/`mailboxes` retirement summaries, `client_versions`, `current` row
- `--addrid-db`: address-id tracker with rotating generations
- `connections`/`connection_messages` table writes (the tables exist; writes are stubbed out for now)
- `x-real-ip` spoof handling: already honored for `your-address` and logging
- Munin-style plugin data (`current` usage row) and richer stats
- `--usage-db`-based request logging blur behavior parity
