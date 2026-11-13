# Protocol Spec Conformance Plan: wormhole-mailbox (Go) ↔ magic-wormhole-protocols

**Analysis date:** 2026-11-13
**Spec under audit:** <https://github.com/magic-wormhole/magic-wormhole-protocols> @ `566ee9e9a20f251256cf8b343ec3bbcb599efecd` (2026-08-31, `Merge pull request #50 …`) — authoritative spec document is `server-protocol.md`. Supporting docs consulted for scope: `client-protocol.md`, `dilation-protocol.md`, `transit.md`, `security.md`, `uri-scheme.md`, `changelog.md`.
**Implementation under audit:** this repository @ `main` (working tree clean; all 21 tests pass).
**Interop constraint:** wire compatibility with the unmodified Python reference (`magic-wormhole` 0.24.0 client) and the upstream `magic-wormhole-mailbox-server` (PyPI 0.8.0 / master). See `parity_plan.md` for reference-parity status; this document audits **spec** conformance instead. The two baselines diverge in a few places — flagged explicitly below as **SPEC-VS-REF** items.
**Guiding principle:** where spec and reference disagree, we do *not* silently pick one. Interop wins by default (the spec itself permits ignoring/unknown keys); spec corrections belong upstream in the protocols repo. Nothing here may regress the verified interop results or the documented deviations in the README.
**Status legend:** DONE (conforms) / GAP (spec item missing or non-conforming) / DECIDE (needs owner decision) / WONTFIX (keep, document) / OUT (out of scope).

---

## 1. Scope

In scope: the **Mailbox server** role (`server-protocol.md`) only — the part this repo implements.

- `client-protocol.md` (PAKE, SecretBox key derivation, version exchange, abilities) — client↔client; server is opaque to phases/bodies. **No server action.**
- `dilation-protocol.md` — client-to-client DILATE-n messages over the mailbox channel; server needs zero knowledge of it. **No server action.**
- `transit.md` — a *different* server (transit relay) with its own handshake/framing; this repo is the mailbox, not the transit relay. **Out of scope.** (Note for `wormhole-william` parity work, where a transit *client* lives.)
- `uri-scheme.md` / `file-transfer-protocol.md` — application-level (`wormhole-transfer:` URI, appid `lothar.com/wormhole/text-or-file-xfer`); clients' concern. **No server action.**
- `security.md` — threat model; relevant to the `list`/nameplate-enumeration mitigation (`--disallow-list`, **implemented**) and to the permission/hashcash mitigation (see G1).
- `server-protocol.md` ends with a reference to `journal.md` — **that file does not exist in the protocols repo** (broken link upstream), and Journal Mode is a client-side persistence strategy. Noted; no server action.

---

## 2. Compliance matrix (spec → implementation)

All normative statements in `server-protocol.md`, checked against `internal/mailbox/{websocket.go, server.go, http.go, database.go}` and `cmd/mailbox-server/main.go`.

### 2.1 Wire transport

| # | Spec statement | Status | Notes |
|---|---|---|---|
| W1 | Messages are JSON dicts, `type` key required | **DONE** | `wsMsg.msgType()`; unparseable JSON or missing `type` → `error` frame |
| W2 | Messages from server to client follow the same format | **DONE** | `{type: …, server_tx: …, …}` |
| W3 | Every client→server message gets an immediate `ack`, with the client's `id` copied in | **DONE** | `sendAck` (id `null` when absent — matches reference `msg.get("id")`) |
| W4 | `list`, `allocate`, `claim`, `release`, `close`, `ping` provoke a direct response, and **`id` is copied into the response** | **DONE** (A1, 2026-11-13) | `id` is *never* copied into `nameplates`/`allocated`/`claimed`/`released`/`closed`/`pong`. Matches Python reference, but contradicts the spec text. SPEC-VS-REF. |
| W5 | All server→client messages carry `server_tx` (float seconds since epoch) | **DONE** | added unconditionally in `send()` |
| W6 | Direct responses include `server_rx` (float seconds since client command received) | **DONE** (A1, 2026-11-13) | `server_rx` is only present on `message` frames. Reference never adds it either. SPEC-VS-REF. Spec uses seconds-as-float; implementation uses integer seconds on `message` frames (see W11) — acceptable, additive, documented |
| W7 | All messages are JSON UTF-8 and sent as a single **binary-mode** WebSocket payload | **GAP** (G4) | Go sends `websocket.TextMessage`; Python sends text and its client asserts `not isBinary`. Strict conformance would break the ecosystem. SPEC-VS-REF. |
| W8 | Servers can signal `error` for any unrecognized message type | **DONE** | `unknown type` error frame |
| W9 | Unrecognized keys in otherwise-recognized messages are ignored | **DONE** | only known keys are read |
| W10 | Client reconnect policy (random delay, ×1.5 growth, cap 60s) | client-side | Not server-enforceable; keepalive is server-side at protocol level (W12) |
| W11 | `server_tx`/`server_rx` unit (float seconds) | **DONE** (`server_tx` float; `server_rx` on `message` is int seconds) | Spec doesn't mandate `message` carry `server_rx` at all; int-seconds there is a harmless reference-compatible choice. Document |
| W12 | Keepalive: WebSocket-layer pings every 60 s (not the `ping` command) | **DONE** | Go ping loop `autoPingInterval` (default 60 s), `autoPingTimeout` (600 s) |

### 2.2 Connection-specific messages

| # | Spec statement | Status | Notes |
|---|---|---|---|
| C1 | First thing server sends is `welcome` | **DONE** | sent immediately on connect |
| C2 | `welcome` may carry `current_cli_version` (deprecated, prompt upgrade) | **DONE** | `--advertise-version` |
| C3 | `welcome` may carry `motd` (print prominently) | **DONE** | `--motd` |
| C4 | `welcome` may carry `error` (client shows and terminates) | **DONE** | `--signal-error` |
| C5 | `welcome` may carry `permission-required` (a `dict` of methods) | **GAP** (G1) | Never emitted; see §3 G1. Reference also doesn't implement it |
| C6 | Unknown welcome fields ignored by clients; server may add `your-address` | DONE (extension) | SPEC doesn't name `your-address` (reference extension since 0.12-ish); spec permits additive keys — no client is allowed to choke on them |
| C7 | Server either proceeds anyway (allowing all access) or sends "permission denied" error and closes the connection; without any `submit-permissions` it defaults to the "proceed anyway" branch | **PARTIAL** | Go always proceeds anyway ✓ (permissive mode, spec-conformant as-written), but there is no way to opt *into* challenge mode, so C5/perm flow is unimplemented end-to-end (G1) |
| C8 | `bind` specifies `appid` and `side`; all subsequent commands scoped to them | **DONE** | missing keys → `bind requires 'appid'/'side'`; `already bound` afterwards |
| C9 | Older clients send `bind` immediately without waiting for `welcome`; server responds as if no `submit-permissions` was sent | **DONE** | bind accepted at any time; permission-less mode default; no ordering enforced |
| C10 | `ping` provokes `pong` (used by unit tests for sync) | **DONE** | `pong` echoes raw `ping` value (int or string, verified by tests) |
| C11 | NAT-binding refresh is WebSocket-layer keepalive, not `ping` | **DONE** | see W12 |
| C12 | Invalid command → `error` with `error` string **plus `orig`: full copy of the original message dict** | **DONE** | `sendError` copies parsed dict. One caveat: unparseable-JSON frames get `orig: {}` (nothing intelligible to copy) — practical limit, documented |
| C13 | `ping` allowed before `bind` (test sync) | **DONE** | ping handled pre-bind |
| C14 | Unknown message types after `bind` → `error` ("unknown type") | **DONE** | |

### 2.3 Permission to Use the Server (hashcash proof-of-work)

The spec defines an optional DoS countermeasure: `permission-required` in `welcome` (e.g. `{"none": {}, "hashcash": {"bits": 6, "resource": "resource-string"}}`), a `submit-permissions` client→server message (e.g. `{"method": "hashcash", "stamp": "1:6:210723:resource-string::NmZNF4eIbk87mqYz:000003n"}`), Hashcash **v1 stamp format**, a 20-bit default (servers may raise), and `none` as the permission-less method a permissive server should advertise.

| # | Spec statement | Status | Notes |
|---|---|---|---|
| P1 | Server may include `permission-required` in welcome | **GAP** | not possible with current flags (G1) |
| P2 | `none` method: client sends normal `bind` | GAP | permissive server proceeds anyway (C7) but does not advertise `none` |
| P3 | `hashcash` method: bits/resource input, stamp format v1, server verifies | **GAP** | nothing implemented — not even a verifier |
| P4 | `submit-permissions` message type | **GAP** | would be rejected today with `error: unknown type` |
| P5 | If unacceptable/missing → "permission denied" error and close | **GAP** | never happens |
| P6 | Clients ignore unsupported methods; may choose any supported method | client-side | |
| P7 | TLS strongly encouraged for permission methods | doc | no TLS in-process (by design; see parity_plan §8) — unchanged |

Reality check (recorded for the decision, not to justify the gap): **neither** the installed Python reference (PyPI 0.8.0, verified by grep: no `permission`/`hashcash`/`submit-permissions` anywhere in `wormhole_mailbox_server`) **nor** upstream master implements any of this. The `magic-wormhole` 0.24.0 client does not send `submit-permissions` either. The feature is spec'd but unimplemented across the ecosystem; G1 is a *new* feature, not a parity fix.

### 2.4 Nameplates

| # | Spec statement | Status | Notes |
|---|---|---|---|
| N1 | Nameplate = non-negative integer ("0" allowed, "YOLO" mode), up to 40 digits, string-typed on the wire | **DONE** | `^\d+$`, ≤40 chars; `claim "0"` accepted |
| N2 | Reject non-digits / >40 digits | **DONE** | `CheckValidNameplate` → error frame
| N3 | Nameplates deleted once last side released, or after inactivity | **DONE** | `ReleaseNameplate` deallocates at zero claims; 11-min prune sweeps |
| N4 | `allocate` returns the **shortest possible** nameplate | **DONE** (approximated the same way as reference) | smallest non-empty band (1→2→3 digits, random within band; else random 1000–999999; reference identical) |
| N5 | Allocating automatically claims (race avoidance) | **DONE** | `allocate` = `claim` internally |
| N6 | Clients may invent their own nameplate instead of allocating | **DONE** | `claim` with arbitrary digits |
| N7 | Nameplates live until the second client has learned the mailbox; then reusable | **DONE** (effectively) | lifetime = unreconciled claims + inactivity window; same as reference |
| N8 | `list` → `nameplates` with ≥ `id` per entry | **DONE** | empty list when `--disallow-list` (not in spec; server-operator DoS mitigation per `security.md`) |
| N9 | Future attributes (wordlist id, code length) *may* be recorded | not required | |
| N10 | `claim` by same side counts as one claim | **DONE** | side row upsert semantics like reference |
| N11 | Third side → crowded; a previously-released side re-claiming → reclaimed | **DONE** | `CrowdedError`/`ReclaimedError`; claim-crowding residue kept for reference parity (documented deviation) |

### 2.5 Mailboxes

| # | Spec statement | Status | Notes |
|---|---|---|---|
| M-a | Mailbox = large random string | **DONE** | 64 bits, base32 (reference uses same `b32encode(urandom(8))`) |
| M-b | AppID isolation of mailboxes/nameplates | **DONE** | per-`app_id` namespace |
| M-c | Mailbox holds unordered set; delivered immediately if both sides connected, queued otherwise | **DONE** | listener broadcast + replay |
| M-d | Messages from both clients merged; `side` distinguishes echo from peer | **DONE** | echo-on-`add` to all subscribed sockets |
| M-e | `open` marks mailbox opened by the side **and subscribes the WebSocket**; no implicit ack required | **PARTIAL** | subscription ✓; server *does* send an ack for `open` (see F/G5) |
| M-f | `open` yields one `message` per stored message | **DONE** | replay ordered by `server_rx` |
| M-g | `close` → `closed` | **DONE** | optional `mailbox` key must match open mailbox; mismatch → error |
| M-h | `close` accepts optional `mood` | **DONE** (A2, 2026-11-13: NULL stored when absent) | Go stores `''` when mood absent; reference stores SQL NULL. No functional difference in usage summaries, but on-disk bytes differ (G6) |
| M-i | Moods: `happy` (default), `lonely`, `scary`, `errory` recorded in usage | **DONE** | mood override logic mirrors reference |
| M-j | Server additionally records `pruney` (inactivity delete) and `crowded` (>2 sides) | **DONE** (as usage-summary `result`) | `result=crowded` when >2 side rows; `pruney` when pruned; see usage-db semantics in README |
| M-k | Body = hex-encoded bytestring, opaque to server | **DONE** | no hex validation (matches reference; spec doesn't require it) |
| M-l | `phase` is a string | **DONE** | numeric phases are clients' reserved space |
| M-m | `message` response includes `side`, `phase`, `body`, and `id` copied from the `add` | **DONE** | plus `server_rx` (reference does the same; extra keys are spec-legal) |
| M-n | Server does **not** deduplicate and does **not** retain ordering | **WONTFIX** (keep as is) | no dedup ✓; replay is `ORDER BY server_rx ASC` — stricter than required, cleary harmless, byte-compatible with reference |
| M-o | Mailboxes kept alive by open client or a pointing nameplate; nameplate deletion also deletes mailbox | **DONE** | prune cascade |

### 2.6 Persistence

| # | Spec statement | Status | Notes |
|---|---|---|---|
| PP1 | All messages stored in a DB; no information lost on restart | **DONE** | channel-v2 SQLite, schemas byte-identical to reference |
| PP2 | Server does not send a direct response until side-effects are safely committed | **DONE** | responses are emitted only after the DB `Exec`s return (SQLite autocommit per statement) |
| PP3 | Clients know how to re-resume after reconnect | client-side | server commits before response, enabling idempotent replay |
| PP4 | (Journal Mode — separate client-side mode; see `journal.md`) | OUT | `journal.md` does not exist in the spec repo; nothing for the server to do |

### 2.7 Error semantics / cross-cutting

| # | Spec statement | Status | Notes |
|---|---|---|---|
| E1 | `error` frames: `{error: str, orig: <full original dict>}` | **DONE** | `orig` present and complete for all parseable commands; `orig: {}` for JSON syntax errors (documented) |
| E2 | `ack` id semantics (F/W3) | **DONE** | id copied verbatim (ints stay ints, strings stay strings) |
| E3 | Frame `server_tx` monotonic-ish float seconds | **DONE** | nanosecond precision float |
| E4 | Nameplate digit check uses ASCII or Unicode `\d`? | **DONE** (A/G8, 2026-11-13: ASCII `[0-9]+`) | Go RE2 `\d` = ASCII; Python `\d` matches Unicode Nd (e.g. `"١٢٣"` is a "nameplate" for Python). Spec says "digits" of a non-negative integer; either reading is defensible (G8) |

### Findings summary

| ID | Gap | Severity | Type |
|----|-----|----------|------|
| **G1** | Entire "Permission to Use the Server" section unimplemented: `permission-required` never sent, `submit-permissions` unknown, no hashcash verifier, no `none` advertisement | the largest spec delta | new feature (no reference precedent) |
| **G2** ✅ implemented | `id` of `list`/`allocate`/`claim`/`release`/`close`/`ping` not copied into their direct responses | small, additive | SPEC-VS-REF divergence |
| **G3** ✅ implemented | `server_rx` absent from direct responses | small, additive | SPEC-VS-REF divergence |
| **G4** | Text WebSocket frames sent; spec mandates binary-mode payload | doc-level; ecosystem expects text | SPEC-VS-REF (spec is stale) |
| **G5** | `open` is acked although spec text says "no explicit ack" | doc-level; reference acks | SPEC-VS-REF + spec-internal inconsistency (W3 vs "no explicit ack") |
| **G6** ✅ implemented | Missing mood stored as `''` (Go) instead of SQL `NULL` (reference) | cosmetic, DB-byte parity | parity nit |
| **G7** | Replayed messages ordered by `server_rx`; spec says no ordering retained | WONTFIX | spec is a *floor*, not ceiling; matches reference |
| **G8** ✅ done (ASCII) | ASCII vs Unicode digit definition for nameplates | cosmetic | align or document |
| **G9** | No limit on frame size / connection count (OOM, memory DoS); spec silent, `security.md` cares | hardening | not spec-required |
| **G10** | Spec cites `journal.md`, which doesn't exist upstream | doc | upstream spec bug |
| **G11** | Spec internally inconsistent on `ack` for `open` (W3 "all commands" vs mailbox section "no explicit ack") | doc | upstream spec clarification |
| **G12** | `spec` never documents `your-address` (reference extension) nor `client_version` on `bind` nor `server_rx` on `message` | doc | spec could codify de-facto behavior |

None of G2–G12 affect on-disk DB compatibility. G2/G3/G6 are wire/DB-additive **only if clients ignore extra keys** (mandated by W9), which in practice they do (python client `setdefault`-style parsing, wormhole-william likewise).

---

## 3. Phase plan

Decisions in §4 decide which items land. Tasks are written to be reviewable in one PR each.

### Phase A — additive spec conformance (safe, small) — presumed desired unless you say otherwise

A1. **Direct responses echo `id` and `server_rx`** (G2, G3)
    - Thread `server_rx int64` (and the client `id json.RawMessage`) into `send()`-adjacent response paths in `internal/mailbox/websocket.go`:
      `nameplates` ← `list`; `allocated` ← `allocate`; `claimed` ← `claim`; `released` ← `release`; `closed` ← `close`; `pong` ← `ping`.
    - Response frames gain `id` (echo, or JSON `null` when absent — same convention as `ack`) and `server_rx` (float seconds, `float64(time.Now().UnixNano())/1e9`, same format as `server_tx`).
    - Challenge: `ack` must remain *first*; response order in frame stream unchanged. No DB writes added. `message` frames unchanged.
    - Tests: extend `websocket_test.go` — assert `id` and positive `server_rx` on each direct response; assert both absent-or-`null` id behavior when command lacks `id`.
    - Compatibility: extra keys — allowed by spec (W9) and ignored by python client / wormhole-william.

A2. **Store mood as SQL NULL when omitted** (G6)
    - `handleClose`: pass `*string` (NULL) into `Close`, instead of `""`; only write mood when the client actually sent a string. Adjust `Mailbox.Close` signature and the usage-summary SQL accordingly (`IS NOT NULL` filter stays the same in effect).
    - Tests: add case asserting `mood IS NULL` in `mailbox_sides` when `close` has no mood, and `'happy'` when it does; confirm `mailboxes` usage `result` unchanged for both.
    - Note: rows already written with `''` stay as-is; summarizer treats `''` harmlessly.

A3. **Document the deviations** (G2–G5, G7, G10) in README "Intentional deviations" + this plan: keep-text-frames (G4), ack-for-open (G5), ordered replay (G7), `journal.md` broken link (G10), spec-vs-reference divergence table.

A4. **Size limit (G9)** — pending decision D4; if approved: `conn.SetReadLimit(n)` in `websocketConn.run()` (default 1 MiB, flag `--max-message-size`, `0` = gorilla default/unlimited). Oversize frame → connection dropped by gorilla (spec-friendly: server may signal error by dropping; no client crash, python client raises internally but that's the spec's fault for silence — alternative is CloseMessage with status 1009). Tests: 1 MiB+ payload → connection closed, no crash of server, other clients unaffected.

### Phase B — permission framework (G1) — optional, only if D1 says so

B1. Config (flags, mirroring reference's flag style):
    - `--permission-required` — no arg form of the spec's advertisement; e.g. `--permission-hashcash=BITS` and `--permission-hashcash-resource=STR` may be cleaner; **decision D1b on flag shape**.
    - Default OFF: server behaves as today (proceed-anyway), exactly spec C7 permissive branch; this is the only mode the reference ecosystem can consume today.
B2. `welcome` gains `permission-required` **iff** configured (never otherwise): e.g. `{"hashcash": {"bits": 6, "resource": "<random or configured>"}}` and include `"none": {}` only when still allowing permission-less use (spec suggests permissive servers advertise `none`).
B3. New message `submit-permissions` with `method`, method-specific payload; initial support: `none` (no-op accept) and `hashcash` (verify stamp per Hashcash v1: version nibble `1`, bit count ≥ configured, timestamp within window, `resource` matches, counter field makes SHA-1 leading bits ≥ target; constant-time-ish, no DB needed).
    - On success: mark connection permission-granted; normal flow continues.
    - On failure/invalid/missing: `error` frame "permission denied" (spec: "sends a 'permission denied' error message"), then clean connection close (spec: "and closes the connection").
    - Unsupported-but-offered method: `error` + close (spec: server chooses "denies" over "proceed anyway" in challenge mode).
B4. Olds-client compatibility (spec C9): a client that never sends `submit-permissions` is treated as "no submit-permissions sent" ⇒ if server is in challenge mode, deny+close (spec is explicit); if permissive, proceed. Document loudly: turning this on breaks **all** current python/wormhole-william clients.
B5. Tests: welcome with/without `permission-required`; `submit-permissions` none/hashcash good/bad stamp (use `hashcash` shell tool to mint vectors); denial path closes cleanly; permissive default unchanged; interop smoke (python client still transfers with feature OFF; with feature ON, python client gets permission denied and exits cleanly — this is *spec-correct* behavior).
B6. Usage DB: log permission outcomes? (not in spec; proposal: skip, keep this thin).
B7. Documentation: README section + security-relevant warning.

### Phase C — spec hygiene / upstream (G4, G5, G10–G12) — optional

C1. Open an issue/PR against `magic-wormhole/magic-wormhole-protocols` suggesting: binary→text payload wording (evidence: python `assert not isBinary`, autobahn `sendMessage(payload, False)`); resolve ack-for-open wording W3 vs "no explicit ack"; fix `journal.md` dangling reference; codify `your-address`, `client_version`, `server_rx`-on-`message`.
C2. Alternatively, do nothing upstream and carry the divergence table in this repo. **Decision D5.**

### Phase D — nameplate digit semantics (G8) — trivial

D-phase1 (if approved): replace `^\d+$` with `[0-9]+` explicitly (ASCII, status quo, just spelling) **or** `[\p{Nd}]+` (Python-`re` semantics) — plus a line in the spec-deviation notes. Recommendation: keep ASCII (explicit `[0-9]+`); nameplates are transcribed human strings; Unicode digits would confuse everyone. No behavior change; clarify spelling only. If D6 favors Python parity, use `[\p{Nd}]+`.

### Sequencing & effort estimate

| Phase | Contents | Est. | Risk |
|---|---|---|---|
| A | A1, A2, A3 (+A4 if D4=yes) | 0.5–1 day | low (additive fields; D4 medium) |
| B | B1–B7 | 1–2 days | medium — new crypto-adjacent verifier, ecosystem still can't use it |
| C | upstream PRs | 0.5 day async | n/a |
| D | G8 spelling | 10 min | none |

Validation gates before merge: `go test ./...`, `go test ./... -race`, e2e suite (`e2e/`) green, and (if Phase A shipped) a handshake-inspect smoke test showing a python client transfer still completes. If Phase B ships, e2e must run against permissive default; challenge-mode e2e is unit-level only (no client supports it).

---

## 4. Decisions — ANSWERED 2026-11-13 (owner)

1. **D1 — Phase B (permission/hashcash): declined.** Server stays spec-permissive. G1 remains an open spec gap, carried as documented divergence (README "Conformance"). Revisit when upstream clients speak `submit-permissions`.
2. **D2 — Direct responses echo `id` + carry `server_rx`: follow the spec (A1 implemented 2026-11-13).** Unconditional additive change; `id` echoes the command's raw `id`, or JSON `null` when absent (same convention as `ack`); `server_rx` is float seconds matching `server_tx`. `message` frames also emit `server_rx` as float seconds now (reference sends float; JSON-identical for integral values). Tests added in `websocket_test.go`.
3. **D3 — `ack` for `open`: keep.** Owner leaned toward official-protocol alignment; analysis: the spec is self-contradictory (transport §"ack for all commands" vs mailbox §"no explicit ack to open"). Keeping the ack honors the generic transport clause, matches the reference, and is load-bearing for wormhole-william (`rendezvous/client.go sendAndWait` blocks on the `ack` for `open`). Documented divergence; see README "Conformance with magic-wormhole-protocols". — *Note: owner reported uncertainty; flagging here for veto.*
4. **D4 — Message-size/DoS hardening (G9): not now.** "lets not add anything extra for now". Remainder: silent-timeout/OS-level limits only.
5. **D5 — Upstream spec action: local divergence table, gaps closed in implementation.** README carries the table; no upstream PR yet. Unresolved spec wording kept as documented divergence (binary-vs-text, ack-for-open, `journal.md`, de-facto keys).
6. **D6 — Nameplate digits: follow spec; best judgment → ASCII kept, spelled `[0-9]+`** (spec says "non-negative integer"; codes are human-transcribed). Unicode-Nd behavior intentionally diverges from Python `re \d`; documented.
7. **D7 — Spec-first confirmed**, interop preserved (no text→binary flip, no key removals, DB bytes unchanged).
8. **D8 — Mood `''` → SQL `NULL`: fixed now (A2 implemented 2026-11-13).** `Close` takes `*string`; missing mood ⇒ `NULL` in `mailbox_sides.mood`; summarizer unchanged (already keyed off `Valid`). Test added (`TestMoodStoredAsNullWhenAbsent`).

**Phase status after owner answers:** Phase A — **done** (A1, A2; A4 declined). Phase B — declined. Phase C — carried as README divergence table, nothing upstream yet. Phase D — done (`[0-9]+`). Verification: `go test ./...`, `-race`, and full e2e suite (Python client + wormhole-william) green on 2026-11-13.

---

## 5. Explicit non-goals

- No text→binary frame flip (would break `assert not isBinary` in every python client).
- No removal of `server_tx`/`server_rx` extras, no removal of `your-address`, no change to error-payload shape.
- No TLS in-process (upstream reference likewise assumes a TLS frontend).
- No transit relay or journal-mode work here (separate projects).
- No change to schemas; `channel-v2` stays byte-identical.

## 6. References

- Spec: server-protocol.md @ `566ee9e` (this audit), files listed in §1.
- Reference: PyPI 0.8.0 (`/tmp/wormhole-venv`) and upstream master (`/tmp/mailbox-ref`), cross-checked on 2026-11-13: `server_websocket.py` (no `permission`/`hashcash`/`submit-permissions`; `pong` without `id`; no `server_rx` on direct responses), `server.py` (`NAMEPLATE_RE = ^\d+$`, Unicode `\d`), `web.py`, `util.py`.
- Go anchors: `internal/mailbox/websocket.go` (lines: send/sendAck/handle*/messageFrame), `server.go` (`checkValidNameplate`, `Close` mood path), `websocket_test.go`, `e2e/interop_test.go`.
- `parity_plan.md` (reference-parity audit, adjacent and complementary).
