# syntax=docker/dockerfile:1
#
# wormhole-mailbox, packaged for podman (or docker). Build with:
#
#   podman build --format docker -t wormhole-mailbox .
#
# (--format docker keeps the HEALTHCHECK; podman's default OCI format drops it)
#
# Run with:
#
#   podman run -d --name wormhole-mailbox \
#       -p 4000:4000 -v wormhole-mailbox-data:/data \
#       wormhole-mailbox [extra flags...]
#
# Extra flags replace the default CMD entirely, e.g. enable usage tracking:
#
#   podman run -d --name wormhole-mailbox \
#       -p 4000:4000 -v wormhole-mailbox-data:/data \
#       wormhole-mailbox --port tcp:4000:interface=:: \
#       --channel-db /data/relay.sqlite --usage-db /data/usage.sqlite \
#       --addrid-db /data/addrid.sqlite

# Cross-compiling on the native build platform means the arm64 image needs
# no QEMU emulation, so builds behave the same on CI and local machines.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
# TARGETOS/TARGETARCH are provided automatically by buildx (and by podman
# build --platform); default to linux/amd64 when building without them
ARG TARGETOS TARGETARCH
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w" \
    -o /out/wormhole-mailbox ./cmd/mailbox-server \
    && adduser -D -u 10001 wormhole \
    && mkdir -p /data \
    && chown 10001:10001 /data

FROM docker.io/library/alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/dougsko/wormhole-mailbox" \
      org.opencontainers.image.description="Go port of magic-wormhole-mailbox-server (rendezvous server)" \
      org.opencontainers.image.licenses="MIT"
# /etc/passwd and /etc/group bring in the wormhole user (uid 10001); they were
# prepared on the build platform as plain text, so no arm64 binary runs here
COPY --from=build /etc/passwd /etc/group /etc/
COPY --from=build --chown=10001:10001 /data /data
COPY --from=build /out/wormhole-mailbox /usr/local/bin/wormhole-mailbox
USER wormhole
VOLUME ["/data"]
EXPOSE 4000
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:4000/ || exit 1
ENTRYPOINT ["/usr/local/bin/wormhole-mailbox"]
# default mirrors the Python plugin's defaults (channel db only, port 4000)
CMD ["--port", "tcp:4000:interface=::", "--channel-db", "/data/relay.sqlite"]
