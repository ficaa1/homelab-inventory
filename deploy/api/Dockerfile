# Pin both base images by digest when preparing a release.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /api ./cmd/api

RUN set -eu; mkdir -p /agent; \
    for arch in amd64 arm64; do \
      mkdir -p /agent/linux-$arch; \
      for name in discover agentupdate; do \
        CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags='-s -w' -o /agent/linux-$arch/$name ./cmd/$name; \
      done; \
    done; \
    cd /agent && sha256sum linux-*/* > SHA256SUMS

FROM alpine:3
# Docker clients are inert without the optional operator-mounted socket.
RUN apk add --no-cache ca-certificates docker-cli docker-cli-compose
RUN addgroup -S -g 10001 inventory && adduser -S -D -H -u 10001 -G inventory inventory \
    && mkdir -p /data && chown inventory:inventory /data
COPY --from=build /api /api
COPY --from=build /agent /usr/share/homelab-inventory/agent
USER 10001:10001
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/api", "-dir", "/data"]
