ARG GO_VERSION=1.26
# sing-box builds in its own stage with its own, older Go on purpose.
#
# sing-box reaches into crypto/tls internals via //go:linkname (its badlinkname
# and tfogo_checklinkname0 tags exist to permit that), so it only compiles against
# the Go versions whose internals it expects. Both 1.26.5 and 1.25.12 fail with
#   link: common/badtls: invalid reference to crypto/tls.(*Conn).handlePostHandshakeMessage
# because that method was renamed out from under it. Upstream's own Dockerfile
# still says 1.25, but that pin has rotted with 1.25 patch releases.
#
# Verified building on go1.24.13. Raise it only after confirming this stage still
# builds — the manager stage is free to use whatever is current.
ARG SINGBOX_GO_VERSION=1.24
ARG SINGBOX_VERSION=v1.13.14

# ---------------------------------------------------------------------------
# sing-box, built with with_v2ray_api.
#
# This stage is the whole reason for owning an image. Official sing-box releases
# omit with_v2ray_api, and without it there are no per-user traffic counters at
# all — so no usage reporting and no quota enforcement.
# ---------------------------------------------------------------------------
# --platform=$BUILDPLATFORM pins this stage to the machine doing the building, and Go
# cross-compiles to $TARGETARCH from there. The alternative — letting buildkit emulate the
# target and compiling under QEMU — turns a two minute build of gvisor, quic-go and
# tailscale into a very long one.
FROM --platform=$BUILDPLATFORM golang:${SINGBOX_GO_VERSION}-alpine AS singbox
ARG SINGBOX_VERSION TARGETOS TARGETARCH
RUN apk add --no-cache git
RUN git clone --depth 1 -b "${SINGBOX_VERSION}" https://github.com/SagerNet/sing-box /src
WORKDIR /src
# The upstream default tag set plus with_v2ray_api. Reading the tags from the
# checkout rather than hardcoding them means a version bump picks up whatever
# upstream considers default, instead of silently dropping a feature.
RUN TAGS="$(cat release/DEFAULT_BUILD_TAGS_OTHERS),with_v2ray_api" && \
    echo "building sing-box ${SINGBOX_VERSION} for ${TARGETOS}/${TARGETARCH} with tags: $TAGS" && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -v -trimpath -tags "$TAGS" \
      -ldflags "-s -w -buildid= -X github.com/sagernet/sing-box/constant.Version=${SINGBOX_VERSION#v}" \
      -o /out/sing-box ./cmd/sing-box
# Fail the build here, not in production. A missing tag is otherwise invisible until an
# operator notices usage stuck at zero.
#
# Inspects the binary rather than running it: cross-compiled output is for another
# architecture and cannot be executed on this builder. The tag name is embedded as a
# literal by the build-tag-guarded file that registers it, so grep is a valid check.
RUN grep -q with_v2ray_api /out/sing-box

# ---------------------------------------------------------------------------
# The manager.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS manager
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# Copied path by path rather than `COPY . .` with a .dockerignore: an allowlist
# cannot accidentally admit a private key, a local config.json or a stray
# database into the build context.
COPY main.go ./
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X vlessvmore/internal/cli.Version=${VERSION}" \
      -o /out/vlessvmore .

# ---------------------------------------------------------------------------
# Runtime.
# ---------------------------------------------------------------------------
# The only stage that runs on the target architecture. Under a multi-platform build that
# means one short emulated step (apk) rather than an emulated compile.
FROM alpine
RUN apk add --no-cache ca-certificates tzdata

COPY --from=singbox /out/sing-box    /usr/local/bin/sing-box
COPY --from=manager /out/vlessvmore  /usr/local/bin/vlessvmore

# argv[0] dispatch, so `docker exec vlessvmore user add alice` works as well as
# the canonical `docker exec vlessvmore vlessvmore user add alice`.
RUN ln -s vlessvmore /usr/local/bin/user && \
    ln -s vlessvmore /usr/local/bin/token

# Users, tokens and traffic history live here. Mount a named volume over it or
# everything is lost when the container is replaced.
VOLUME /var/lib/vlessvmore

# `status` dials the CLI socket, which exercises the manager end to end rather
# than just checking that a port is open.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["vlessvmore", "status"]

# Split so `docker compose up` runs the daemon while
# `docker run --rm IMAGE init --host x` replaces the command instead of
# appending to it.
ENTRYPOINT ["vlessvmore"]
CMD ["serve"]
