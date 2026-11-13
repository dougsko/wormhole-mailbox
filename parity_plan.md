# Parity Plan: wormhole-mailbox (Go) ↔ magic-wormhole-mailbox-server (Python reference)

**Analysis date:** 2026-11-12
**Go side:** this repository — `github.com/dougsko/wormhole-mailbox`
**Python reference:** `magic-wormhole-mailbox-server` — *two different baselines exist, do not conflate them:*
- **PyPI 0.8.0** (installed in `/tmp/wormhole-venv`, released 2026-05-15): wire protocol + usage DB only. **It has no** `address_id.py`, `connections.py`, or `--addrid-db`/`--generation-duration`.
- **upstream `master`** (unreleased, fetched from `github.com/magic-wormhole/magic-wormhole-mailbox-server`): everything in 0.8.0 **plus** the phase-2 "address-id" and "connections" tracking (`addrid-v1.sql`, `connections`/`connection_messages` tables, `--addrid-db`, `--generation-duration`, rotating generations, connections cleared at startup).

The Go port already tracks **upstream master**, not 0.8.0. All schema files (`channel-v1/v2`, `usage-v1/v2`, `upgrade-*`, `addrid-v1`) are byte-identical to master. This document is written against master.

**Scope exclusions:** the Python `twist` plugin framework itself; `magic-wormhole` client parity (separate repo/plan, see `../wormhole-william/parity_plan.md`); the transit relay (`magic-wormhole-transit-relay`); dilation.
**Guiding constraints:** wire and on-disk format compatibility with the reference; keep the HTTP surface (`/` root page, `/v1` websocket) identical; don't regress the existing 21 tests or the verified interop results (see README "Interop testing").
**Status legend:** DONE / PARTIAL / OPEN / DECIDE / WONTFIX.

---

## 1. Baseline inventory (Go port, verified 2026-11-12)

- HTTP: plain-text `Wormhole Relay\n` at `/`, websocket at `/v1`, `Server: Magic Wormhole Mailbox <version>` header (hardcoded `0.8.0` in `internal/mailbox/http.go`), optional request logging when blur-usage is unset.
- Wire protocol: `welcome` (motd/`current_cli_version`/`error` + `your-address` with X-Real-IP/X-Real-Port override), `bind` (client_version → usage DB `client_versions` + `connections.bound`), `ping`/`pong`, `list`, `allocate`, `claim`, `release`, `open`, `add`, `close`, `ack` before handling, `error` frames with `orig`, `message` echo-on-add, `server_tx` timestamps, `nameplates` listing (`--disallow-list` honored).
- DB: open/create/upgrade machinery with foreign-key enforcement and atomic creation; `channel-v2` created/upgraded from v1; usage-v2; addrid-v1; schemas embedded verbatim.
- Core semantics match the reference: nameplate allocation preferring 1–3 digit ids (random within band), `ReclaimedError`/`CrowdedError` rules, reclaimed-nameplate rejection (3rd side), mailbox sides tracking, 11-minute channel expiry with 5-minute pruning timer (runs immediately at startup), message replay on `open`, `close` with moods → `happy`/`scary`/`lonely`/`errory`/`pruney`/`quiet`, usage summaries (`nameplates`, `mailboxes`, `current` row every 5 min, `client_versions`, 5-minute `--blur-usage=N` bucketing), addrid rotating generations (`--generation-duration`, default 86400s; force-init at startup + check every 5 min), `connections`/`connection_messages` lifecycle (established → bound → deleted on disconnect/restart), `--blur-usage` (0/absent = no blur, matching the reference's falsy-0 behavior), `--disallow-list`, `--advertise-version`, `--signal-error`, `--motd`, rlimit bump.
- Concurrency: single mutex around command handling and pruning, mirroring the single-threaded reactor; per-connection write mutex + 10s write deadline (documented deviation from the reference's reactor buffering).
- Tests: 21 tests across `database_test.go`, `server_test.go`, `phase2_test.go`, `websocket_test.go` (`go test ./...` green; `-race` supported per README).
- Container: alpine, non-root uid 10001, `/data` volume, published to ghcr.io on push to main/tags; README documents container ops. CI runs tests + builds/publishes multi-arch images.
- Known deviations (documented in README): cached `Mailbox` objects dropped on prune; mailbox close deletes the whole nameplate's claim rows (reference leaks one side / FK error); invalid `claim`/bad JSON produce `error` frames instead of dropped connections; claim-crowding residue kept for parity; no free-nameplate crash (error frame instead).

---

## 2. Gap matrix

| # | Area | Reference (master) | Go status | Plan item |
|---|------|--------------------|-----------|-----------|
| 1 | `--websocket-protocol-option OPTION=VALUE` | twisted `usage.Options`, repeat-flag, JSON values; autobahn options incl. `autoPingInterval`/`autoPingTimeout` | **BROKEN**: accepted but *never applied* — main.go parses into unused locals, prints "ignored" for everything; also breaks values containing commas; not repeatable | M1.1 (fix, P0) |
| 2 | Ping/pong liveness | autobahn drops conn after `autoPingTimeout` **without a pong** (default 60s/600s) | **BUG**: `SetReadDeadline(now+pingTimeout)` is never refreshed on pongs (no `SetPongHandler`); idle-but-alive clients dropped after 600s with zero traffic | M1.2 (fix, P0) |
| 3 | `http.Server` hardening (`ReadHeaderTimeout`, `IdleTimeout`, max header bytes) | twisted Site has its own limits | OPEN | M1.3 |
| 4 | `Server` response header version | `Magic Wormhole Mailbox <version>` (version from packaging) | PARTIAL: hardcoded `const Version = "0.8.0"` | M1.4 |
| 5 | `site.ws_factory = wsrf` on the web resource | master added it (test hook) | WONTFIX (test-only hook) | — |
| 6 | `connections` counted via listeners vs real websocket count | reference has same TODO; counts listeners | PARTIAL, matches reference TODO | doc parity, no work |
| 7 | `--log-fd` | removed in master (was inert 0.8.0) | absent (correct for master) | doc note |
| 8 | JSON value parsing & repeat-flag semantics for `--websocket-protocol-option` | Python: `json.loads(value)` | BROKEN (see #1) | M1.1 |
| 9 | `X-Real-IP`/`X-Real-Port` handling | spoofable (documented) | DONE (same semantics) | — |
| 10 | Welcome `your-address` (ipv4/ipv6 + port) | DONE | DONE | — |
| 11 | Schema files | identical to master (verified byte-for-byte) | DONE | — |
| 12 | `ack` before command handling; `pong` echoes raw ping value; `error` frames carry `orig` | matches | DONE | — |
| 13 | `client_versions` rows incl. null implementation/version | matches (nullable) | DONE | — |
| 14 | Interop proof (python client ↔ go server, both DB eras) | verified manually | DONE (documented); not automated | M2.1 |
| 15 | Race coverage | reference uses reactor threads | DONE (`-race` documented) | add `-race` to CI matrix if absent |
| 16 | Usage/addrid behavior tests | python trial suite exists for these paths (test_addrid, test_connectiondb, test_stats) | DONE (phase2_test.go mirrors them) | — |

*(Rows 5–16 are included for completeness; none require behavior changes except where noted.)*

---

## 3. Phase 1 — correctness fixes (M1)

### 3.1 `--websocket-protocol-option` (#1, #8)
- Implement a repeatable flag (e.g. `type wsProtoOptions []string` with `Set`) keeping the current comma-separated form working for back-compat, or document the syntax change in release notes (DECIDE, D1).
- Parse each `OPTION=VALUE`; JSON-decode VALUE with `encoding/json` (match Python's `json.loads`); reject malformed input with usage error.
- Apply the autobahn-relevant subset: `autoPingInterval` (seconds, float) and `autoPingTimeout` (seconds, float) → wire into `NewHandler` → `NewWebSocketServer`. `0` disables (autobahn semantics).
- All other options (e.g. `autoPingSize`, `failByDrop`, …): print "note: protocol option X ignored" and continue, as today.
- Defaults stay 60/600 if unset.
- Tests: flag parsing table (repeated, comma-separated, bad JSON, unknown option); handler constructed with overridden ping interval; connection stays alive past `autoPingTimeout` while receiving pongs — see §3.2.

### 3.2 Pong-driven liveness (#2)
- In `websocketConn.run()`: call `conn.SetPongHandler(func(appData string) error { _ = wc.conn.SetReadDeadline(time.Now().Add(wc.pingTimeout)); return nil })` when `pingTimeout > 0`; keep the existing per-data-frame deadline refresh; keep the ping loop.
- DECIDE (D2): also refresh the deadline on **client pings** (gorilla `SetPingHandler` already auto-responds with pong; refreshing there too is friendlier and harmless). Recommendation: yes.
- Ensure autobahn-parity wording in a comment: "drop connection only when no pong is received within autoPingTimeout".
- Tests: fake client that answers pings but sends no data → connection still open at 2× interval; client that ignores pings → closed at ~pingTimeout (use short timers, e.g. 50/150ms).

### 3.3 `http.Server` limits (#3)
Add `ReadHeaderTimeout: 10s`, `IdleTimeout: 120s` (generous; websocket conns are hijacked so this mostly bounds header reading). Document. No behavior change for normal clients.

### 3.4 Build-stampable `Server` version (#4)
Replace `const Version = "0.8.0"` with `var Version = "0.8.0"` settable via `-ldflags "-X …/mailbox.Version=$(git describe --tags)"`; keep const-like fallback. Note in release docs.

---

## 4. Phase 2 — verification hardening (M2)

### 4.1 CI interop job (not just a library test)
- New GitHub workflow job: checkout, `pip install magic-wormhole==0.24.0`, start the Go mailbox on port 4101 with a fresh `--channel-db`, run a real `wormhole --relay-url ws://127.0.0.1:4101/v1 --accept-file send/recv` of a small file + a directory; assert sha256 of the payload. ~1–2 min runtime; no secrets needed.
- Runs on every PR; failures block merge (or are advisory initially — DECIDE D3).

### 4.2 Gap tests for §2 rows 1–2
Unit tests listed inline in §3 (protocol-option parsing; pong liveness; deadline behavior).

### 4.3 Fuzz/property (stretch)
Native fuzzing for `CheckValidNameplate`, `parseEndpoint`, `wsMsg` parsing; 30s in CI if cheap, else skip.

---

## 5. Phase 3 — docs & release (M3)

- README: add **Compatibility matrix** (Go port vs PyPI 0.8.0 vs upstream master, being explicit that addrid/connections = master-only today); add the §3 fixes to a **Changelog** (new `CHANGELOG.md`); document `--websocket-protocol-option` exact syntax post-fix, `--blur-usage=0` semantics, `Server` header version; note absence of `--log-fd` (removed upstream); keep the documented deviations list in sync.
- Release checklist: run interop suite before tagging; keep `.dockerignore` covering `*.sqlite`; bump `Version`.

---

## 6. Sequencing

| Milestone | Contents | Est. |
|---|---|---|
| M1 | §3.1, §3.2, §3.3, §3.4 | 0.5–1 day |
| M2 | §4.1 CI interop, §4.2 tests, §4.3 stretch | 0.5–1 day |
| M3 | §5 docs, CHANGELOG, tag v1.3.0 | 0.5 day |

Riskiest: §3.2 (subtle liveness semantics — get autobahn parity right; verified by tests in §4.2).

---

## 7. Decision log

| # | Decision | Date | Outcome |
|---|----------|------|---------|
| D1 | Repeat-flag for `--websocket-protocol-option` (break comma hack) vs keep comma-split | | pending (recommend: support both; commas inside JSON values are the common case for nested options, so split properly by parsing top-level) |
| D2 | Refresh read deadline on **client** pings too | | pending (recommend yes) |
| D3 | CI interop job blocking vs advisory | | pending (recommend: blocking after one green week) |
| D4 | Tracking master unreleased python (status quo) vs also publishing a 0.8.0-compat mode | | pending (recommend: keep master parity; document) |

---

## 8. Explicit non-goals

- No changes to wire format, schema files, message types, or ack/error semantics.
- No TLS termination in the server (reference expects a frontend like Caddy; documented upstream).
- No Twisted plugin/`twist` compatibility layer.
- Rendezvous client concerns (`../wormhole-william/parity_plan.md`) not duplicated here.
- No new endpoints beyond `/` and `/v1`.

## 9. References

- Installed reference (PyPI 0.8.0): `/tmp/wormhole-venv/lib/python3.14/site-packages/wormhole_mailbox_server/`
- Master reference (fetched 2026-11-12): `src/wormhole_mailbox_server/{server.py, server_tap.py, server_websocket.py, address_id.py, connections.py, database.py, web.py, util.py}`, `src/twisted/plugins/magic_wormhole_mailbox.py`, `docs/server-protocol.md` — saved under `/tmp/mailbox-ref/` and `/tmp/mailbox-ref/ref/` (byte-identical schemas)
- Go implementation anchors: `cmd/mailbox-server/main.go`, `internal/mailbox/{server.go, websocket.go, http.go, connections.go, address_id.go, database.go, rlimits.go}`, `internal/mailbox/db-schemas/*`
- Python release notes (NEWS.md, master): 0.8.0 = "Server header properly reports version (#27)", "your-address in welcome (#63)"
