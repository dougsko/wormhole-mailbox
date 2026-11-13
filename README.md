# wormhole-mailbox

Public home: <https://github.com/dougsko/wormhole-mailbox>

A Go port of [magic-wormhole-mailbox-server](https://github.com/magic-wormhole/magic-wormhole-mailbox-server),
the rendezvous server for [Magic Wormhole](https://magic-wormhole.io). It performs
store-and-forward delivery of small key-exchange and control messages between the two
sides of a wormhole. Bulk data flows elsewhere (direct TCP or a
[transit relay](https://github.com/magic-wormhole/magic-wormhole-transit-relay)).

**Status: feature-complete with the reference** (MVP + phase 2 usage/connections/addrid
tracking). The wire protocol and both databases are compatible with the Python reference.

## Compatibility

- Wire-compatible JSON-over-WebSocket protocol at `/v1`: `welcome`, `bind`, `ping`/`pong`,
  `list`, `allocate`, `claim`, `release`, `open`, `add`, `close`, `ack`/`error` semantics,
  `server_tx`/`server_rx` timestamps, and `your-address` in the welcome (including
  `X-Real-IP`/`X-Real-Port` override, like the reference).
- Database-compatible `channel-v2` SQLite schema with the same `version` row and the same
  `upgrade-channel-to-v2.sql` migration chain. DBs created by either implementation can be
  opened and served by the other (verified; see "Interop testing" below).
- Usage tracking to `usage-v2`: `nameplates`/`mailboxes` retirement summaries (with mood
  handling), `client_versions` on bind, and a `current` row every 5 minutes (with
  `connections_websocket`). `--blur-usage=N` rounds logged times down to N-second buckets,
  exactly like the reference; `total_time` is computed from the unblurred start time as the
  reference does.
- Address-id tracker to `addrid-v1`: rotating generations (`--generation-duration`, default
  86400s; check runs at startup and every 5 minutes) so distinct peers can be counted per
  generation without retaining addresses indefinitely.
- `connections`/`connection_messages` rows in the channel DB for every live connection
  (established → bound on `bind` → message log → deleted on disconnect or restart).
- Nameplate allocation, allocation preference for 1–3 digit ids, claim crowding rules,
  reclaimed-nameplate rejection, 11-minute channel expiry (5-minute check interval),
  message replay on `open`, echo-on-`add` delivery semantics, and mood handling on
  `close` all match the reference.

Tested with the unmodified Python `magic-wormhole` 0.24.0 CLI as both client and server peers.

## Building and running

```
git clone https://github.com/dougsko/wormhole-mailbox.git
cd wormhole-mailbox
go build -o mailbox-server ./cmd/mailbox-server
./mailbox-server --port tcp:4000 --channel-db relay.sqlite
```

Or install the binary directly with Go:

```
go install github.com/dougsko/wormhole-mailbox/cmd/mailbox-server@latest
```

Flags — the names, short flags, and defaults match the Python `twist wormhole-mailbox` plugin (twisted `usage.Options`), and the `--port` endpoint uses twisted endpoint syntax (`tcp:PORT[:interface=ADDR]`; a bare port or `host:port` is also accepted):

| Flag | Default | Python equivalent | Description |
|------|---------|-------------------|-------------|
| `-p`, `--port` | `tcp:4000:interface=::` | `-p`, `--port` | endpoint to listen on |
| `--channel-db` | `relay.sqlite` | `--channel-db` | state database, created/upgraded automatically |
| `--usage-db` | (none) | `--usage-db` | record usage data (SQLite): retirement summaries, `client_versions`, `current` stats row |
| `--addrid-db` | (none) | `--addrid-db` | address-id tracking database (SQLite); enables the rotating address tracker |
| `--generation-duration` | `86400` | `--generation-duration` | lifetime of IP-address tracking table (seconds) |
| `--blur-usage` | off | `--blur-usage` | round logged times down to N seconds (affects usage-db rows and `client_versions`) |
| `--advertise-version` | (none) | `--advertise-version` | `current_cli_version` to put in `welcome` |
| `--signal-error` | (none) | `--signal-error` | `error` message to put in `welcome` (clients terminate) |
| `--motd` | (none) | `--motd` | `motd` to put in `welcome` |
| `--disallow-list` | `false` | `--disallow-list` | respond to `list` with an empty set |
| `--websocket-protocol-option OPTION=VALUE` | (none) | `--websocket-protocol-option` | accepted; autobahn-only options are ignored with a note |

Notes on parity with the Python plugin:

- `--blur-usage` on the Python side takes an integer and treats `0` (falsy) as
  "no blur" — we mirror that: blurring happens only when N > 0, and the
  default disables it.
- Neither the Python plugin nor this server reads any environment variables;
  configuration is flags only. The client-side environment variables
  `WORMHOLE_RELAY_URL` (`ws://HOST:PORT/v1`, same as `--relay-url`) and
  `WORMHOLE_TRANSIT_HELPER` (`tcp:HOST:PORT`, same as `--transit-helper`) are
  honored by the `wormhole` CLI/library the same regardless of which server
  implementation is used — they're how clients find *any* wormhole server.
- Usage rows are written at the same lifecycle points as the reference
  (close/prune/release, and for the mailbox also on prune). The `current`
  usage row is written at startup and every 5 minutes (the Python plugin uses
  twisted's `TimerService`, which runs immediately at service start).

The server bumps `RLIMIT_NOFILE` to at least 10,000 if possible (best effort).

## Architecture

- `cmd/mailbox-server` — CLI entry point, pruning timer, signal handling
- `internal/mailbox/database.go` — open/create/upgrade machinery (`channel-v2`, `usage-v2`,
  `addrid-v1`), embedded verbatim SQL schemas (`db-schemas/`)
- `internal/mailbox/server.go` — `Server`/`AppNamespace`/`Mailbox` core state machine:
  nameplates, claims, mailboxes, messages, replay, close/summarize, pruning
- `internal/mailbox/address_id.go` — rotating address-id generation tracker (mirrors `address_id.py`)
- `internal/mailbox/connections.go` — live-connection table (mirrors `connections.py`)
- `internal/mailbox/websocket.go` — the wire protocol
- `internal/mailbox/http.go` — plain-text root at `/` (WebSocket upgrades at
  `/` are accepted as a client-compatibility extension), WebSocket upgrade at `/v1`
  (also `/v1/` tolerated),
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

Automated interoperability suite (Python magic-wormhole client and the wormhole-william
Go library, against a real `mailbox-server` binary; text/file/directory both directions;
sha256-verified):

```
cd e2e && go test -tags=e2e -v .
```

See [e2e/README.md](e2e/README.md) for requirements (Python client + wormhole-william
checkout) and the environment knobs. Runs in CI as the `e2e` job.

Interop testing (both directions of database compatibility, plus plain client/server):

```
python3 -m venv /tmp/wormhole-venv
/tmp/wormhole-venv/bin/pip install magic-wormhole magic-wormhole-mailbox-server
/tmp/wormhole-venv/bin/wormhole --relay-url ws://127.0.0.1:4101/v1 --transit-helper tcp:127.0.0.1:4001 send FILE
# ... receive similarly with --accept-file --output-file ...
```

Verified: Python file sent through the Go mailbox (fresh DB, Python-created DB), and a
Go-created DB served by the reference Python server at the same commit — full transfers with
md5-verified payloads. Also verified with `--usage-db`/`--addrid-db` enabled: a Python→Python
transfer (sender + receiver CLI) through the Go mailbox wrote `nameplates`/`mailboxes`
`happy` rows, `client_versions` rows for both sides (`python 0.24.0`), `addrid_generation`
row (generation 1), and `address_ids` rows; `connections`/`connection_messages` are empty
after disconnect (rows are deleted on disconnect, like the reference).

## Conformance with magic-wormhole-protocols

The server was audited statement-by-statement against the
[magic-wormhole-protocols](https://github.com/magic-wormhole/magic-wormhole-protocols) spec
(@ `566ee9e`, `server-protocol.md`). Result: conformant except for the items below, all of
which are deliberate. Full audit matrix: `protocol_spec_plan.md`.

Conformant and spec-mandated (verified by tests):

- Direct responses to `ping`, `list`, `allocate`, `claim`, `release`, `close` echo the
  command's `id` (or JSON `null` when absent, like `ack`) and carry a `server_rx` timestamp
  (float seconds, same format as `server_tx`), as the spec requires.
- `mood` is stored as SQL `NULL` when the `close` command omits it (not an empty string).
- Nameplates are ASCII digits only (`[0-9]+`), ≤40 chars; `0` is accepted (YOLO mode); the
  `type` key is required on every command; unknown types/keys handled per spec.

Spec ambiguities and stale text, with our resolutions:

- **No ack for `open`?** The spec's mailbox section says "There is no explicit ack to the
  `open` command", but its transport section says the `ack` is "immediately sent back to the
  client for all commands". We **keep** the `ack` for `open`: it matches the reference
  implementation, and `wormhole-william` (this repository's companion Go client) *blocks* on
  the `ack` in `sendAndWait`, so dropping it would break the Go client against this server.
  Also spec-legal (clients must ignore unrecognized/extra frames only from the *server*; none
  ignore `ack`), merely redundant.
- **Binary-mode payload?** The spec mandates a binary-mode WebSocket payload for messages;
  the reference and both mainstream clients use **text** frames (the Python client asserts
  `not isBinary`). We send text frames. The spec's payload-mode sentence is stale.
- **Ordered replay**: the spec says the server does not retain ordering; we replay `open`
  history with `ORDER BY server_rx ASC` (same as the reference) — a refinement, not a
  violation (clients may not *rely* on order, but it may).
- **`server_rx` on `message` frames**: carried (float seconds), matching the reference; the
  spec lists only `side`/`phase`/`body`/`id` for `message`, extra keys are explicitly
  permitted.
- **Acknowledgment** for every command remains **before** handling, and `ack.id` is echoed
  verbatim; both are spec-mandated and kept.
- **`your-address`**, **`client_version`** on `bind`, and the `Server` header are reference
  (de-facto) extensions the spec does not name; extra keys are spec-legal ("Clients and
  Servers must ignore unrecognized keys").
- **"Permission to Use the Server"** (welcome `permission-required`, `submit-permissions`,
  hashcash proof-of-work) is the one spec section deliberately **not implemented**; the
  Python reference does not implement it either and no released client can speak it. The
  server therefore always behaves as the spec's permissive case (proceed anyway). It may be
  added later; the audit notes what's needed.
- The spec cites `journal.md`, which does not exist in the protocols repository — it is a
  client-side persistence concept; nothing for this server to implement.

Out of scope for this server: `client-protocol.md` (PAKE/version/abilities — done by
clients), `dilation-protocol.md`, `transit.md` (the transit *relay* is a separate server),
`uri-scheme.md` and `file-transfer-protocol.md` (application-level).

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

## Notes and remaining phase-2 gaps

- `MAX_WAIT_TIME`/usage timers are intentionally not implemented (the reference has that
  timer unused/commented out too).
- Usage rows are written at the same lifecycle points as the reference (close/prune/release,
  and for the mailbox also on prune). The reference deletes the nameplate rows on mailbox
  close without recording a `nameplates` usage summary, so the `nameplates` row is written
  when the nameplate is retired by release or prune instead (same behavior as the reference).
- `x-real-ip` spoofing is not blocked, matching the reference (the value is only used for
  hole-punching assistance and stats).
- One reference quirk kept for parity: a mailbox close deletes nameplate rows without a
  usage summary, so a later `release` finds no nameplate and is a silent no-op.
- The `current` usage row is written once at startup and every 5 minutes by the timer,
  mirroring twisted's TimerService (which runs its callable immediately at service start).

## Running in a container (podman)

Pre-built multi-arch images are published to `ghcr.io/dougsko/wormhole-mailbox` by CI on
every push to `main` (tag `latest` + `<git-sha>`) and on version tags (`v1.2.3` → `1.2.3`,
`1.2`). Pull with:

```sh
podman pull ghcr.io/dougsko/wormhole-mailbox:latest
```

or build locally:

```sh
podman build --format docker -t wormhole-mailbox .
podman run -d --name wormhole-mailbox \
    -p 4000:4000 -v wormhole-mailbox-data:/data \
    wormhole-mailbox
```

- `--format docker` keeps the `HEALTHCHECK` (podman's default OCI image
  format drops it; the build still succeeds without it, you just lose the
  health probe).
- The image is alpine-based and runs as non-root uid 10001. `/data` is a
  volume holding `relay.sqlite` (the default channel DB), `usage.sqlite`, etc.
- If host port 4000 is taken on your machine, map any host port, e.g.
  `-p 4500:4000`, and point clients at `--relay-url ws://localhost:4500/v1`.
- Extra flags replace the default CMD entirely. To enable usage tracking and
  the IP-address addrid DB:

  ```sh
  podman run -d --name wormhole-mailbox \
      -p 4000:4000 -v wormhole-mailbox-data:/data \
      wormhole-mailbox --port tcp:4000:interface=:: \
      --channel-db /data/relay.sqlite \
      --usage-db /data/usage.sqlite \
      --addrid-db /data/addrid.sqlite
  ```

- `.dockerignore` keeps `.git`, local databases (`*.sqlite`), and editor
  noise out of the build context.
- Verified end-to-end: the unmodified Python client (`magic-wormhole`
  0.24.0) completes a file transfer against the containerized mailbox and
  relay (via `--no-listen` on both sides to force the relay path).

## Running with Podman Quadlet

Quadlet files are provided under `packaging/quadlet/` for easy deployment as
systemd services using Podman. They pull the published image and use a
named `wormhole-mailbox-data` volume, publish TCP port
4000, and start the container on boot via the generated systemd unit.

Rootful installation:

```sh
sudo cp packaging/quadlet/*.container /etc/containers/systemd/
sudo systemctl daemon-reload
sudo systemctl start wormhole-mailbox.service
```

Rootless installation:

```sh
mkdir -p ~/.config/containers/systemd
cp packaging/quadlet/*.container ~/.config/containers/systemd/
systemctl --user daemon-reload
systemctl --user start wormhole-mailbox.service
loginctl enable-linger "$USER"   # if it should start before login
```

The generated units have an `[Install]` section, so no separate
`systemctl enable` is needed. Logs are available with:

```sh
sudo journalctl -u wormhole-mailbox.service -f      # rootful
journalctl --user -u wormhole-mailbox.service -f    # rootless
```

The default `Exec=` uses the published image's defaults:
`--port tcp:4000:interface=:: --channel-db /data/relay.sqlite`. Add
`--usage-db`/`--addrid-db` flags to the Quadlet file if you want usage or
address-IP tracking. Ensure host TCP port 4000 is open in the firewall if
accessed over the internet.

## License

MIT — see [LICENSE](LICENSE). This is a Go port of the MIT-licensed Python
[magic-wormhole-mailbox-server](https://github.com/magic-wormhole/magic-wormhole-mailbox-server);
the schema files under `internal/mailbox/` (e.g. the `channel-v2` schema) are
derived verbatim from the reference.
