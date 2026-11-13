# Changelog

All notable changes to wormhole-mailbox are documented here. Newest first.

## Unreleased

Spec-conformance fixes against the `magic-wormhole-protocols` spec
(`server-protocol.md` @ `566ee9e`), keeping wire/DB compatibility with the
Python reference and the magic-wormhole 0.24.0 client:

- Direct responses to `ping`, `list`, `allocate`, `claim`, `release`, `close`
  now echo the command's `id` (or JSON `null` when absent, like `ack`) and
  carry a `server_rx` timestamp (float seconds, same format as `server_tx`),
  as the spec requires. `message` frames now emit `server_rx` as float seconds
  as well (JSON-identical for integral values; matches the reference's float).
- `close` without a `mood` now stores SQL `NULL` in `mailbox_sides.mood`
  (previously an empty string), matching the reference's on-disk format.
- Nameplate validation speeded up: `[0-9]+` (ASCII), same semantics, now explicit.
- `open` remains followed by an `ack` (the spec is self-contradictory; see
  README "Conformance with magic-wormhole-protocols" for the resolution and
  why wormhole-william depends on it staying).

Otherwise no behavior changes; verified: `go test ./...`, `-race`, and the
full Python-client ↔ wormhole-william interop suite pass.
