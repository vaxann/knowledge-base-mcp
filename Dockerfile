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
# stdio only: no ports are exposed on purpose.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/entrypoint.sh"]
CMD ["knowledge-base-mcp"]
