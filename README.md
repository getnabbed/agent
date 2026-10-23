# agent

[![CI](https://github.com/getnabbed/agent/actions/workflows/ci.yml/badge.svg)](https://github.com/getnabbed/agent/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/getnabbed/agent.svg)](https://pkg.go.dev/github.com/getnabbed/agent)
[![Go Report Card](https://goreportcard.com/badge/github.com/getnabbed/agent)](https://goreportcard.com/report/github.com/getnabbed/agent)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Content automation and downloader agent.

## Install

Prebuilt binaries for Linux, macOS and Windows are attached to each
[GitHub release](https://github.com/getnabbed/agent/releases).

With a Go toolchain:

```sh
go install github.com/getnabbed/agent/cmd/agent@latest
```

## Usage

```sh
agent version
agent help
```

### Logging

Logs are written to stderr using Go's `log/slog`. Two formats are built in:

- `text`: compact, human-readable lines with colour when stderr is a terminal
  (disable colour by setting `NO_COLOR`).
- `json`: one JSON object per line, for log shippers and CI.

The default (`auto`) picks `text` on a terminal and `json` otherwise.

```sh
agent --log-format json --log-level debug version
```

`--log-level` accepts `debug`, `info` (default), `warn` or `error`. At `debug`
level each record also carries its source file and line.

## Development

Requires Go 1.26 or newer and [golangci-lint](https://golangci-lint.run/).

```sh
make build   # build ./bin/agent
make test    # run tests with the race detector
make lint    # run golangci-lint
make all     # lint, test, build
```

Releases are built by [GoReleaser](https://goreleaser.com/) when a `v*` tag is
pushed. See [CONTRIBUTING.md](CONTRIBUTING.md) for the contribution workflow.

## License

[MIT](LICENSE)
