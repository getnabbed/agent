MODULE   := github.com/getnabbed/agent
BIN      := agent
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse HEAD 2>/dev/null || echo "")
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

.PHONY: all build install test lint fmt vet tidy clean proto proto-lint proto-breaking proto-breaking-bsr proto-push proto-push-label proto-require-main proto-require-pushed vuln docker snapshot helm

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
# cloud generates its code from there and pins git-<sha12> labels, the
# first 12 characters of the agent commit. CI publishes main on merge
# (.github/workflows/proto-publish.yml). proto-lint and proto-breaking need
# no BSR access; the rest are for maintainers.
BSR_MODULE := buf.build/nabbed/agent
UPSTREAM   := https://github.com/getnabbed/agent.git

# Against main on GitHub, as CI checks pull requests.
proto-breaking:
	buf breaking --against '$(UPSTREAM)#branch=main'

# Against what the BSR serves as main, as CI checks before it publishes.
proto-breaking-bsr:
	buf breaking --against $(BSR_MODULE):main

# Only the git-<sha12> label, for cloud work that needs the contract before
# the agent PR merges. Never moves main.
proto-push-label: proto-require-pushed proto-lint proto-breaking
	buf push --label git-$$(git rev-parse HEAD | cut -c1-12)

# Moves main. The fallback for when CI cannot publish; prefer running the
# Proto publish workflow on main.
proto-push: proto-require-main proto-lint proto-breaking-bsr
	buf push --label main --label git-$$(git rev-parse HEAD | cut -c1-12)

# A label must name a commit anyone can look up: committed, and the tip of a
# branch in getnabbed/agent.
proto-require-pushed:
	@test -z "$$(git status --porcelain)" || { echo "the working tree is not clean; commit your changes first" >&2; exit 1; }
	@git ls-remote $(UPSTREAM) 'refs/heads/*' | cut -f1 | grep -qx "$$(git rev-parse HEAD)" || { echo "HEAD is not the tip of a branch in getnabbed/agent; push it first" >&2; exit 1; }

# Only reviewed code moves main: a clean main that matches getnabbed/agent
# main, whatever the local remotes are called.
proto-require-main:
	@test "$$(git symbolic-ref --short -q HEAD)" = main || { echo "proto-push moves the BSR main label, so run it on main; for a branch, use make proto-push-label" >&2; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "the working tree is not clean; commit or remove your changes first" >&2; exit 1; }
	@git fetch --quiet --no-tags $(UPSTREAM) main
	@test "$$(git rev-parse HEAD)" = "$$(git rev-parse FETCH_HEAD)" || { echo "HEAD is not getnabbed/agent main; pull, or wait for your change to merge" >&2; exit 1; }

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
