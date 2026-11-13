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
- The WebSocket endpoint is also served at `/v1/` (exact trailing-slash path),
  so clients configured with a trailing slash no longer fall through to the
  plain-text root handler and fail the handshake with "got 200". A WebSocket
  upgrade request to `/` is likewise served as `/v1`, so clients configured
  with just the server root work too; ordinary HTTP GETs at `/` still return
  the plain-text root document.
- Add Podman Quadlet units under `packaging/quadlet/` for easy systemd-managed
  deployment of the published container image and its data volume.

Otherwise no behavior changes; verified: `go test ./...`, `-race`, and the
full Python-client ↔ wormhole-william interop suite pass.

### Container images

- The Dockerfile now cross-compiles the server on the build platform for the
  requested `--platform` (`TARGETOS`/`TARGETARCH`), so the `linux/arm64`
  variant ships in both `ghcr.io/dougsko/wormhole-mailbox:latest` and semver
  tags without any QEMU emulation (the `arm64` build of the Go module is
  `CGO_ENABLED=0` and deterministic, identical compiler/flags for both arches).
  The final stage performs no emulated steps: `/etc/passwd` and `/etc/group`
  (with the `wormhole` user) and the `/data` directory are copied from the
  build stage. CI additionally registers QEMU (`docker/setup-qemu-action`) as a
  safety net for any future arm64-executed steps.
