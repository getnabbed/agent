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
  ghcr.io/getnabbed/agent:1

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
| `torrent.allow_lan` | `NABBED_TORRENT_ALLOW_LAN` | `--torrent-allow-lan` | `false` |
| `usenet.provider` | `NABBED_NNTP` | `--nntp` | none (temporary, see below) |
| `library.allowed_roots` | `NABBED_LIBRARY_ROOTS` | | none: no library folders allowed (see below) |
| `library.file_mode` | `NABBED_LIBRARY_FILE_MODE` | | `0644` |
| `library.dir_mode` | `NABBED_LIBRARY_DIR_MODE` | | `0755` |

Every environment variable also accepts a `_FILE` variant that names a file
holding the value, for Docker and Kubernetes secrets:
`NABBED_NNTP_FILE=/run/secrets/nntp`.

### Library folders

The web app chooses where imported files go, but only inside folders you
allow on this machine. By default none are allowed, and the web app shows
why each library folder was refused. Allow the parents of your libraries:

```yaml
library:
  allowed_roots:
    - /media/movies
    - /media/tv
```

Or set `NABBED_LIBRARY_ROOTS=/media/movies:/media/tv` (`;`-separated on
Windows). Symlinks are resolved, so a link cannot lead outside. In Docker,
mount your media and allow the mount point, for example
`-v /srv/media:/media -e NABBED_LIBRARY_ROOTS=/media`.

Imported files are created `0644` and folders `0755`, so a media server
running as another user can read them. Tighten them with `library.file_mode`
and `library.dir_mode` if your media server shares the agent's user or
group.

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

### Usenet providers

Add usenet providers in the web app. The browser encrypts the credentials
to this agent's key, so the cloud stores them but cannot read them, and the
agent keeps them encrypted on disk. Providers from the web app must use TLS
and a public host. Pairing the agent again gives it a new key, so its
providers must be entered again.

A provider on your local network, or one you prefer to keep off the cloud
entirely, can be set locally with `NABBED_NNTP_FILE` (preferred) or
`NABBED_NNTP`:
`nntps://user:password@news.example.com:563?connections=8`. Flag values are
visible in process lists, so avoid `--nntp` for real passwords.

Separate several providers with spaces, in priority order. Each article is
tried on them in turn when one lacks it or serves it damaged. Add
`backup=true` to a block account so it is used only for those gaps.
`connections` is the account's limit and is shared by every download; if
the provider allows fewer, the agent notices and uses fewer.

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

Verify against the exact release workflow and tag, not just the repository:
a pattern that accepts any workflow in the repository would also accept one
run from an unreviewed branch.

```sh
VERSION=1.2.3
IDENTITY="https://github.com/getnabbed/agent/.github/workflows/release.yml@refs/tags/v$VERSION"
ISSUER=https://token.actions.githubusercontent.com

# Archives: build provenance, then the signed checksum file.
gh attestation verify agent_${VERSION}_linux_amd64.tar.gz --repo getnabbed/agent \
  --signer-workflow getnabbed/agent/.github/workflows/release.yml
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" \
  --certificate-github-workflow-trigger push
sha256sum --ignore-missing -c checksums.txt

# Container image.
cosign verify ghcr.io/getnabbed/agent:$VERSION \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" \
  --certificate-github-workflow-trigger push
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

### Torrent network policy

Torrent metadata comes from public indexers and peers, so it is untrusted.
By default the agent will not contact anything on your local network on a
torrent's behalf: peers, DHT nodes, trackers and web seeds with private,
loopback, link-local (including cloud metadata), CGNAT or multicast
addresses are refused, checked after DNS so a hostname cannot sneak one
through. Set `torrent.allow_lan: true` only if you deliberately seed or
fetch from machines on your LAN.

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
