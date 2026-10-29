VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
COMMANDS := discover api alerts portmap mcpbridge agentupdate agentsign
AGENT_PLATFORMS ?= linux/amd64,linux/arm64
export AGENT_PLATFORMS

.PHONY: all vet test build dist agent-dist docker api ports alerts release keygen hold promote auto clean fmt api-linux
all: vet test build
vet:
	go vet ./...
test:
	go test ./...
build:
	@mkdir -p bin
	@for name in $(COMMANDS); do \
	  CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$$name ./cmd/$$name || exit; \
	done
# The signer parses the supported platform list once for all distribution builds.
dist: build
	@for platform in $$(./bin/agentsign -platforms); do \
	  os=$${platform%-*}; arch=$${platform#*-}; \
	  for name in discover agentupdate; do \
	    CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$$name-$$platform ./cmd/$$name || exit; \
	  done; \
	done
	@cd bin && sha256sum discover-* agentupdate-* > SHA256SUMS
agent-dist: dist
api-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/api-linux-amd64 ./cmd/api
docker:
	docker build -f deploy/api/Dockerfile -t homelab-inventory:local .
# Explicit open mode is only a local development convenience.
api: build
	./bin/api -addr 127.0.0.1:8080 -allow-unauthenticated
ports: build
	./bin/portmap
alerts: build
	./bin/alerts
keygen: build
	@./scripts/release-agent.sh keygen
release: dist
	./scripts/release-agent.sh release "$(VERSION)"
hold promote auto: build
	./scripts/release-agent.sh $@
fmt:
	gofmt -w cmd internal deploy
clean:
	rm -rf bin
