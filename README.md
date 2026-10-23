# agent

[![CI](https://github.com/getnabbed/agent/actions/workflows/ci.yml/badge.svg)](https://github.com/getnabbed/agent/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/getnabbed/agent.svg)](https://pkg.go.dev/github.com/getnabbed/agent)
[![Go Report Card](https://goreportcard.com/badge/github.com/getnabbed/agent)](https://goreportcard.com/report/github.com/getnabbed/agent)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

The Nabbed agent runs on your own hardware. The Nabbed cloud decides what
to download; the agent downloads it natively (usenet and BitTorrent, no
external client) using provider credentials the cloud cannot read.

## Quick start with Docker

```sh
docker run -d --name nabbed-agent --restart unless-stopped \
  -e NABBED_API_URL=https://api.nabbed.example \
  -v nabbed-data:/data \
  -v "$PWD/downloads:/downloads" \
  -p 6881:6881 -p 6881:6881/udp \
  --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
  ghcr.io/getnabbed/agent:latest

docker logs -f nabbed-agent   # shows the pairing code on first start
```

Open the link in the log, enter the code, and the agent is paired. It keeps
its credentials in `/data`, so that volume must persist; never put them in
an environment variable or a Docker secret, because the refresh token
rotates.

The image is `linux/amd64`, `linux/arm64` and `linux/arm/v7`, based on
distroless with no shell, and runs as uid `65532`. A bind-mounted
`downloads` directory must be writable by that uid
(`sudo chown -R 65532:65532 downloads`), or run the container with
`--user "$(id -u):$(id -g)"`.

A ready-to-use [Compose file](contrib/docker/compose.yaml) applies the same
hardening:

```sh
curl -O https://raw.githubusercontent.com/getnabbed/agent/main/contrib/docker/compose.yaml
docker compose up -d && docker compose logs -f agent
```

## Configuration

Settings come from, in increasing order of precedence: built-in defaults,
an optional YAML file, `NABBED_*` environment variables, and command-line
flags. Run `agent config` to print the effective configuration with secrets
redacted.

| Setting | Environment variable | Flag | Default |
|---|---|---|---|
| Config file | `NABBED_CONFIG` | `--config` | see below |
| `api_url` | `NABBED_API_URL` | `--api-url` | none; needed only to pair |
| `name` | `NABBED_NAME` | `--name` | hostname |
| `data_dir` | `NABBED_DATA_DIR` | `--data-dir` | platform data dir; `/data` in Docker |
| `log.level` | `NABBED_LOG_LEVEL` | `--log-level` | `info` |
| `log.format` | `NABBED_LOG_FORMAT` | `--log-format` | `auto` |
| `tls.ca_file` | `NABBED_CA_FILE` | `--ca-file` | system roots only |
| `downloads.dir` | `NABBED_DOWNLOAD_DIR` | `--download-dir` | `<data_dir>/downloads`; `/downloads` in Docker |
| `downloads.max_concurrent` | `NABBED_MAX_DOWNLOADS` | `--max-downloads` | `3` |
| `torrent.port` | `NABBED_TORRENT_PORT` | `--torrent-port` | `0` (random); `6881` in Docker |
| `usenet.provider` | `NABBED_NNTP` | `--nntp` | none (temporary, see below) |

Every environment variable also accepts a `_FILE` variant that names a file
holding the value, for Docker and Kubernetes secrets:
`NABBED_NNTP_FILE=/run/secrets/nntp`.

### Config file

Start from [`contrib/agent.example.yaml`](contrib/agent.example.yaml).
Unknown keys are an error, so typos fail loudly. The agent looks for it at:

| Platform | Path |
|---|---|
| Docker | `/config/agent.yaml` (mount a directory read-only at `/config`) |
| Linux, root or system service | `/etc/nabbed-agent/agent.yaml` |
| Linux, user | `~/.config/nabbed-agent/agent.yaml` |
| macOS | `~/Library/Application Support/Nabbed Agent/agent.yaml` |
| Windows | `%APPDATA%\Nabbed\Agent\agent.yaml` |

A missing file at the default location is fine; a missing file named with
`--config` is an error. Keep the file owned by you and read-only to the
agent: it will also hold the local policy (allowed library roots, limits)
that the cloud cannot override.

### Usenet provider (temporary)

Until usenet providers are configured in the web app and sealed to the
agent, set one with `NABBED_NNTP_FILE` (preferred) or `NABBED_NNTP`:
`nntps://user:password@news.example.com:563?connections=8`. Flag values are
visible in process lists, so avoid `--nntp` for real passwords.

## Other ways to install

Signed binaries for Linux (amd64, arm64, armv7), macOS and Windows are on
each [GitHub release](https://github.com/getnabbed/agent/releases), with a
hardened systemd unit in `contrib/systemd/`. With a Go toolchain:

```sh
go install github.com/getnabbed/agent/cmd/agent@latest
```

### Verifying a release

Releases are built reproducibly in GitHub Actions. Checksums and images are
signed with [cosign](https://docs.sigstore.dev/) using GitHub's OIDC
identity (no long-lived keys), and every archive has an SBOM and a SLSA
build provenance attestation.

```sh
# Archives: provenance, or the signed checksum file.
gh attestation verify agent_1.2.3_linux_amd64.tar.gz --repo getnabbed/agent
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/getnabbed/agent/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt

# Container image.
cosign verify ghcr.io/getnabbed/agent:1.2.3 \
  --certificate-identity-regexp '^https://github.com/getnabbed/agent/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Usage

```sh
agent pair --api-url https://api.nabbed.example   # shows a code to approve in the web app
agent run                                         # connects; pairs first if needed
agent status                                      # identity, gateway, state (no secrets)
agent config                                      # effective configuration (secrets redacted)
```

Exit code 3 means the agent was revoked or its credentials expired and it
must be paired again with `agent pair --force`.

### Logging

Logs go to stderr. `auto` (the default) writes compact human-readable text
on a terminal and one JSON object per line otherwise, which is what Docker
and systemd capture. Set `NO_COLOR` to disable colour. At `debug` level each
record also carries its source file and line.

### Downloads

Downloads go to the download directory, at most `downloads.max_concurrent`
at once. BitTorrent listens on `torrent.port` and does not seed after
completion; forwarding that port improves speeds but is optional.

Import into the library is not implemented yet: completed downloads stay in
the download directory and are reported as failed at the import step.

See [design 0003](docs/design/0003-session-state-and-credentials.md) for how
credentials, sessions and token refresh work.

## Development

Requires Go 1.27.3 or newer and [golangci-lint](https://golangci-lint.run/).

```sh
make build         # build ./bin/agent
make test          # run tests with the race detector
make lint          # run golangci-lint
make vuln          # govulncheck: known vulnerabilities in called code
make proto         # regenerate pkg/proto from proto/ (buf)
make docker        # build the container image from source
make snapshot      # local GoReleaser build of every archive, unsigned
make all           # lint, test, build
```

Releases are built by [GoReleaser](https://goreleaser.com/) when a `v*` tag
is pushed. See [CONTRIBUTING.md](CONTRIBUTING.md) for the contribution
workflow.

## License

[MIT](LICENSE)
