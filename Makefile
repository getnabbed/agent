MODULE   := github.com/getnabbed/agent
BIN      := agent
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse HEAD 2>/dev/null || echo "")
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

.PHONY: all build install test lint fmt vet tidy clean proto proto-lint proto-breaking proto-push vuln docker snapshot helm

all: lint test build

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BIN) ./cmd/$(BIN)

install:
	go install -trimpath -ldflags '$(LDFLAGS)' ./cmd/$(BIN)

test:
	go test -race -cover ./...

lint:
	golangci-lint run ./...

proto:
	buf generate

proto-lint:
	buf lint
	buf format --diff --exit-code

# The agent publishes the wire contract to the Buf Schema Registry; the
# cloud generates its code from there.
BSR_MODULE := buf.build/nabbed/agent

proto-breaking:
	buf breaking --against $(BSR_MODULE)

# Labels: main, and git-<commit> so consumers can pin an exact contract.
proto-push: proto-lint proto-breaking
	@test -z "$$(git status --porcelain proto)" || { echo "proto/ has uncommitted changes; commit them first" >&2; exit 1; }
	buf push --label main --label git-$$(git rev-parse --short=12 HEAD)

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

docker:
	docker buildx build --load -t nabbed-agent:dev \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

snapshot:
	go run github.com/goreleaser/goreleaser/v2@v2.19.0 release --snapshot --clean --skip=publish,sign,docker

# Lint the Helm chart, validate its manifests and the agent.yaml it generates.
helm:
	./helm/check.sh

fmt:
	gofmt -s -w .

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/ dist/
