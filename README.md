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

### Pairing and running

```sh
agent pair --api-url https://api.nabbed.example   # shows a code to approve in the web app
agent run                                         # connects; pairs first if needed
agent status                                      # identity, gateway, fingerprint (no secrets)
```

Credentials live in an owner-only data directory (`--data-dir`, env
`NABBED_AGENT_DATA_DIR`). Exit code 3 means the agent was revoked or its
credentials expired and it must be paired again.

### Docker

```sh
docker run -d --name nabbed-agent \
  -e NABBED_API_URL=https://api.nabbed.example \
  -v nabbed-agent:/data \
  ghcr.io/getnabbed/agent:latest
docker logs nabbed-agent   # shows the pairing code on first start
```

Use a persistent volume for `/data`, never a secret or env var: the refresh
token rotates. A systemd unit is in `contrib/systemd/`. See
[design 0003](docs/design/0003-session-state-and-credentials.md) for how
credentials and token refresh work.

### Downloads

The agent downloads natively: usenet over NNTP and BitTorrent, with no
external client. Downloads go to `--download-dir` (default `<data-dir>/downloads`),
at most `--max-downloads` at once. BitTorrent listens on `--torrent-port`
(random by default) and does not seed after completion. Until usenet
providers can be configured from the web app, set one with
`NABBED_NNTP=nntps://user:pass@host:563?connections=8`.

Import into the library is not implemented yet: completed downloads stay in
the download directory and are reported as failed at the import step.

## Development

Requires Go 1.26 or newer and [golangci-lint](https://golangci-lint.run/).

```sh
make build   # build ./bin/agent
make test    # run tests with the race detector
make lint    # run golangci-lint
make proto   # regenerate pkg/proto from proto/ (buf)
make all     # lint, test, build
```

Releases are built by [GoReleaser](https://goreleaser.com/) when a `v*` tag is
pushed. See [CONTRIBUTING.md](CONTRIBUTING.md) for the contribution workflow.

## License

[MIT](LICENSE)
