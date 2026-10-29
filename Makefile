VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
COMMANDS := discover api alerts portmap mcpbridge agentupdate agentsign
AGENT_PLATFORMS ?= linux/amd64,linux/arm64
export AGENT_PLATFORMS

.PHONY: all vet test build dist agent-dist docker api ports alerts release keygen hold promote auto clean fmt api-linux release-artifacts test-install-server
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

# Each server archive carries both agent platforms for wizard enrollment.
release-artifacts: override AGENT_PLATFORMS := linux/amd64,linux/arm64
release-artifacts: dist
	@printf '%s\n' "$(VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$$' || { echo 'VERSION must be a release tag (vX.Y.Z)' >&2; exit 1; }
	@set -eu; mkdir -p dist; rm -f dist/homelab-inventory_*_linux_*.tar.gz dist/SHA256SUMS dist/get.sh; stage=$$(mktemp -d); trap 'rm -rf "$$stage"' EXIT HUP INT TERM; \
	  mkdir -p "$$stage/agent/linux-amd64" "$$stage/agent/linux-arm64"; \
	  for arch in amd64 arm64; do \
	    for name in discover agentupdate; do \
	      cp "bin/$$name-linux-$$arch" "$$stage/agent/linux-$$arch/$$name"; \
	    done; \
	  done; \
	  (cd "$$stage/agent" && sha256sum linux-*/* > SHA256SUMS); \
	  cp LICENSE README.md "$$stage/"; \
	  for arch in amd64 arm64; do \
	    CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o "$$stage/homelab-inventory" ./cmd/api; \
	    CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o "$$stage/mcpbridge" ./cmd/mcpbridge; \
	    cp "bin/discover-linux-$$arch" "$$stage/discover"; \
	    cp "bin/agentupdate-linux-$$arch" "$$stage/agentupdate"; \
	    tar -czf "dist/homelab-inventory_$(VERSION)_linux_$$arch.tar.gz" -C "$$stage" homelab-inventory discover agentupdate mcpbridge LICENSE README.md agent; \
	  done
	@cp scripts/get.sh dist/get.sh
	@cd dist && sha256sum homelab-inventory_$(VERSION)_linux_*.tar.gz get.sh > SHA256SUMS

test-install-server:
	python3 scripts/test-install-server.py
