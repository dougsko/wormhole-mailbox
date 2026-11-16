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

# The driver (mattn/go-sqlite3) is cgo, so the build needs a C toolchain —
# which is fine for linux/amd64,linux/arm64 because the arm64 image is built
# by cross-compiling the C parts with the Debian aarch64 toolchain. The C
# compiler never runs an arm64 binary, so no QEMU emulation is involved.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26-bookworm AS build
WORKDIR /src
# TARGETOS/TARGETARCH are provided automatically by buildx (and by podman
# build --platform); default to linux/amd64 when building without them
ARG TARGETOS TARGETARCH
# Install the cross toolchain only when the target arch differs from the
# build host arch (native on the amd64 CI runner for amd64, cross for arm64).
RUN set -eux; \
    target="${TARGETARCH:-amd64}"; \
    host="$(go env GOARCH)"; \
    if [ "$target" != "$host" ]; then \
        apt-get update; \
        case "$target" in \
            arm64) pkgs="gcc-aarch64-linux-gnu libc6-dev-arm64-cross" ;; \
            amd64) pkgs="gcc-x86-64-linux-gnu libc6-dev-amd64-cross" ;; \
            *) echo "unsupported target arch '$target'"; exit 1 ;; \
        esac; \
        apt-get install -y --no-install-recommends $pkgs; \
        rm -rf /var/lib/apt/lists/*; \
    fi
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN set -eux; \
    target="${TARGETARCH:-amd64}"; \
    host="$(go env GOARCH)"; \
    if [ "$target" = "$host" ]; then \
        CC=gcc; \
    else \
        case "$target" in \
            arm64) CC=aarch64-linux-gnu-gcc ;; \
            amd64) CC=x86_64-linux-gnu-gcc ;; \
        esac; \
    fi; \
    CGO_ENABLED=1 GOOS=${TARGETOS:-linux} GOARCH="$target" CC="$CC" go build -trimpath -ldflags="-s -w" \
    -o /out/wormhole-mailbox ./cmd/mailbox-server \
    && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH="$target" go build -trimpath -ldflags="-s -w" \
    -o /out/healthcheck ./cmd/healthcheck \
    && useradd -u 10001 wormhole \
    && mkdir -p /data \
    && chown 10001:10001 /data

FROM docker.io/library/debian:bookworm-slim
LABEL org.opencontainers.image.source="https://github.com/dougsko/wormhole-mailbox" \
      org.opencontainers.image.description="Go port of magic-wormhole-mailbox-server (rendezvous server)" \
      org.opencontainers.image.licenses="MIT"
# /etc/passwd and /etc/group bring in the wormhole user (uid 10001); they were
# prepared on the build platform as plain text, so no arm64 binary runs here
COPY --from=build /etc/passwd /etc/group /etc/
COPY --from=build --chown=10001:10001 /data /data
COPY --from=build /out/wormhole-mailbox /usr/local/bin/wormhole-mailbox
COPY --from=build /out/healthcheck /usr/local/bin/healthcheck
USER wormhole
VOLUME ["/data"]
EXPOSE 4000
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/healthcheck"]
ENTRYPOINT ["/usr/local/bin/wormhole-mailbox"]
# default mirrors the Python plugin's defaults (channel db only, port 4000)
CMD ["--port", "tcp:4000:interface=::", "--channel-db", "/data/relay.sqlite"]
