# syntax=docker/dockerfile:1

# HotLoop Flow.
#
# Mirrors the industrial-dashboard build: a Go builder producing a static binary,
# then distroless. nodered/node-red:latest is roughly 450MB because it carries a
# Node.js runtime, an npm tree and a package manager; this carries a binary.

# ── Editor bundle ────────────────────────────────────────────────────────────
# Separate stage so the Go layers do not rebuild when only the editor changes,
# and so Node never appears in the runtime image.
FROM node:24-alpine AS editor

WORKDIR /build
# Copy the manifests first so the dependency layer caches independently of the
# editor source.
COPY web/package.json web/package-lock.json* ./
RUN --mount=type=cache,target=/root/.npm \
    if [ -f package-lock.json ]; then npm ci; else npm install; fi

COPY web/ ./
RUN npm run build

# The npm packages esbuild actually put in dist (today, the two self-hosted
# fonts), with their license texts, from the build's own metafile. Fails the
# build on a license we don't ship. The Go stage appends it to
# THIRD_PARTY_LICENSES.
RUN node third-party-licenses.mjs meta.json /build/THIRD_PARTY_LICENSES.editor

# ── Go build ─────────────────────────────────────────────────────────────────
FROM golang:1.26.4 AS builder

WORKDIR /src

# go.mod and go.sum first: the module download layer then survives every source
# change, which is most of the build time on a cold runner.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Reads the binary's import graph to write THIRD_PARTY_LICENSES below. The
# binary lands in /go/bin, outside the cache mount, so it stays in the layer.
RUN --mount=type=cache,target=/go/pkg/mod \
    go install github.com/google/go-licenses/v2@v2.0.1

COPY . .
COPY --from=editor /build/dist ./web/dist
COPY --from=editor /build/THIRD_PARTY_LICENSES.editor /out/THIRD_PARTY_LICENSES.editor

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# CGO_ENABLED=0 is load-bearing, not a habit: it is what makes the binary static
# and lets it run on distroless/static with no libc. Every dependency here —
# goja, wazero, pgx, paho — is pure Go specifically to keep this true.
#
# -trimpath strips build paths out of the binary so it does not leak the layout
# of the machine that built it, and makes the build reproducible.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -buildvcs=false \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/hotloop-flow \
      ./cmd/hotloop-flow

# Every Go module compiled into that binary, with its full license text, from
# the same package, platform and cgo setting as the build above, then the
# editor bundle's section. MIT, BSD and Apache all say the notice ships with
# the binary, so it does. GPL, AGPL, LGPL or a license nobody can identify
# fails the build; see build/third-party-licenses.sh.
RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    bash build/third-party-licenses.sh \
      -n "HotLoop Flow" \
      -b /out/hotloop-flow \
      -o /out/THIRD_PARTY_LICENSES \
      -a /out/THIRD_PARTY_LICENSES.editor \
      ./cmd/hotloop-flow

# Fail the build rather than ship something that will not start on distroless.
RUN test -x /out/hotloop-flow && \
    ! ldd /out/hotloop-flow 2>/dev/null | grep -q "=>" || \
    (echo "binary is dynamically linked; it will not run on distroless/static" && exit 1)

# ── Runtime ──────────────────────────────────────────────────────────────────
# distroless/static: no shell, no package manager, no libc. Nothing for an
# attacker who gets code execution to pivot with, and nothing to patch on a CVE
# in a base image nobody is using.
FROM gcr.io/distroless/static-debian13:nonroot

LABEL org.opencontainers.image.title="HotLoop Flow" \
      org.opencontainers.image.description="A visual flow engine for the plant floor. One static Go binary." \
      org.opencontainers.image.vendor="HotLoop" \
      org.opencontainers.image.source="https://github.com/HotLoop-io/hotloop-flow" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /out/hotloop-flow /usr/local/bin/hotloop-flow

# The binary carries code ported from Node-RED, cronosjs and a handful of MIT
# libraries. Apache 2.0 wants the NOTICE to go wherever the work goes, and the
# ISC and MIT licenses want their notices in every copy. The image is a copy.
COPY --from=builder /src/LICENSE /src/NOTICE /out/THIRD_PARTY_LICENSES /usr/share/doc/hotloop-flow/

# 65532 is distroless's nonroot user, and it matches the chart's securityContext.
# The chart's fsGroup makes the PVC writable by it.
USER 65532:65532

# /data is the PVC. Declared so a bare `podman run` without a mount still keeps
# flows somewhere rather than writing into the read-only layer.
VOLUME ["/data"]

EXPOSE 1880

ENTRYPOINT ["/usr/local/bin/hotloop-flow"]
