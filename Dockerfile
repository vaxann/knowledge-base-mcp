# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/knowledge-base-mcp ./cmd/knowledge-base-mcp

FROM alpine:3.21
RUN apk add --no-cache git openssh-client ca-certificates tini su-exec \
    && adduser -D -u 10001 kb \
    && mkdir -p /data/vault /data/index /home/kb/.ssh \
    && chown -R kb:kb /data /home/kb
COPY --from=build /out/knowledge-base-mcp /usr/local/bin/knowledge-base-mcp
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
# The entrypoint starts as root only to read mounted secrets and fix volume
# ownership, then drops to the unprivileged 'kb' user before exec.
ENV KB_VAULT_PATH=/data/vault \
    KB_INDEX_DIR=/data/index \
    HOME=/home/kb
VOLUME ["/data/vault", "/data/index"]
# Only used when KB_HTTP_LISTEN is set (compose does); stdio mode opens no port.
EXPOSE 8765
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
  CMD [ -z "$KB_HTTP_LISTEN" ] || wget -qO- "http://127.0.0.1:${KB_HTTP_LISTEN##*:}/healthz" >/dev/null || exit 1
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/entrypoint.sh"]
CMD ["knowledge-base-mcp"]
