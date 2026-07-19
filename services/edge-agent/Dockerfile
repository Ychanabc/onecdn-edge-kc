# syntax=docker/dockerfile:1

# Standalone edge-agent image. Build context is services/edge-agent so this
# image ships ONLY the agent (no OpenResty). Use it for deployments or debugging
# that need the data-plane agent on its own.

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

WORKDIR /src

# edge-agent is a stdlib-only module; copying go.mod first keeps the download
# layer cached and future-proofs the build if dependencies are ever added.
COPY services/edge-agent/go.mod ./
RUN go mod download

COPY services/edge-agent/ .

# TARGETOS/TARGETARCH are populated by buildx so the compiler runs on the native
# BUILDPLATFORM and cross-compiles instead of running under emulation.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/onekccdn-edge-agent \
      ./cmd/edge-agent

FROM alpine:3.20

ARG VERSION=dev

LABEL org.opencontainers.image.title="onekccdn-edge-agent" \
      org.opencontainers.image.description="Standalone onekccdn edge data-plane agent" \
      org.opencontainers.image.source="https://github.com/Ychanabc/onecdn-edge-kc" \
      org.opencontainers.image.version="${VERSION}"

# ca-certificates for outbound mTLS/HTTPS to the control plane; the data dir is
# pre-created and owned by the unprivileged runtime user.
RUN apk add --no-cache ca-certificates \
    && addgroup -S cdn-edge \
    && adduser -S -G cdn-edge -H -s /sbin/nologin cdn-edge \
    && mkdir -p /var/lib/cdn-edge-agent \
    && chown -R cdn-edge:cdn-edge /var/lib/cdn-edge-agent

COPY --from=build /out/onekccdn-edge-agent /usr/local/bin/onekccdn-edge-agent

ENV EDGE_DATA_DIR=/var/lib/cdn-edge-agent

USER cdn-edge
VOLUME ["/var/lib/cdn-edge-agent"]

ENTRYPOINT ["/usr/local/bin/onekccdn-edge-agent"]
