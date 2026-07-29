# The backup sidecar: fetches GET /backup from vlessvmore and forwards it to backio.
#
# Built from the repo root, so it can see go.mod:
#   docker build -f backup/Dockerfile -t ghcr.io/reeywhaar/vlessvmore-backup .
#
# A separate image rather than a second command in the main one. It has nothing to do with
# serving traffic, it wants a different restart cadence, and keeping it out means the
# proxy image gains neither 7zip nor a reason to hold a backio credential.
ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder
ARG TARGETOS TARGETARCH
WORKDIR /src
# No `go mod download`: backup/ imports only the standard library, so nothing in go.sum
# has to be fetched to build it.
COPY go.mod go.sum ./
COPY backup ./backup
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -buildid=" -o /out/backup ./backup

FROM alpine
# ca-certificates because BACKIO_URL may be https; 7zip only for the optional
# BACKUP_PASSWORD path, which is the only thing that can encrypt the archive.
RUN apk add --no-cache ca-certificates 7zip

COPY --from=builder /out/backup /backup
COPY --chmod=0755 backup/backup_loop.sh /backup_loop.sh

# Local copies, rotated by the same retention policy as the remote. Mount a volume here
# to keep a backup that does not depend on the remote being reachable.
VOLUME /backups

CMD ["/backup_loop.sh"]
