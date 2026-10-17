# Settings for `make deploy` can live in .env (git-ignored).
-include .env

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
DEV_DIR ?= var/dev

.PHONY: build test lint dev deploy clean

build: ## build both binaries for this machine into bin/
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

test: ## run the test suite with the race detector
	go test -race ./...

lint: ## gofmt and go vet
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run gofmt -w ."; exit 1; }
	go vet ./...

dev: build ## run the panel locally with the unsandboxed runtime (no gVisor)
	@mkdir -p $(DEV_DIR)
	@test -f $(DEV_DIR)/master.key || bin/hermitator keygen > $(DEV_DIR)/master.key
	HERMITATOR_DATA_DIR=$(DEV_DIR) HERMITATOR_RUNTIME=unsafe-local \
	HERMITATOR_MASTER_KEY=$$(cat $(DEV_DIR)/master.key) \
	HERMITATOR_LISTEN=127.0.0.1:8080 HERMITATOR_LOG=debug \
	bin/hermitator serve

deploy: ## build for Linux and install on $(DEPLOY_HOST), served at $(DEPLOY_DOMAIN)
	@test -n "$(DEPLOY_HOST)" || { echo "set DEPLOY_HOST (and DEPLOY_DOMAIN) in .env"; exit 1; }
	WITH_WHISPER=$(WITH_WHISPER) deploy/deploy.sh $(DEPLOY_HOST) $(DEPLOY_DOMAIN)

clean:
	rm -rf bin dist

help:
	@grep -E '^[a-z]+:.*## ' $(MAKEFILE_LIST) | sed 's/:.*## /\t/' | column -t -s "$$(printf '\t')"
