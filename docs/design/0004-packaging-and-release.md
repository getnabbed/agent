# 0004: Packaging and release

- **Status:** Accepted, 2026-10-23
- **Scope:** How the agent is built, signed, shipped and configured, with
  Docker as the primary install path.

## Decisions

| Area | Decision | Why |
|---|---|---|
| Runtime image | `gcr.io/distroless/static-debian12:nonroot`, pinned by digest | No shell, package manager or libc to patch; the binary is static (no cgo). |
| User | uid/gid `65532`, set in the image | Never root. `/data` and `/downloads` ship owned by it so new named volumes inherit ownership. |
| Platforms | `linux/amd64`, `linux/arm64`, `linux/arm/v7` | Covers x86 servers, Raspberry Pi and ARM NAS, including older 32-bit models. Distroless publishes all three. |
| Build images | Two Dockerfiles with an identical runtime stage | `Dockerfile` builds from source for contributors. `release.Dockerfile` packages the binaries GoReleaser already built and signed, so a release never compiles twice. |
| Pinning | Base images by digest in literal `FROM` lines; Actions by commit SHA; tool versions explicit | Dependabot updates all three. No `latest` anywhere in the build path. |
| Toolchain | `go 1.27.3` minimum; `GOTOOLCHAIN=local` in builds | The minimum carries the standard-library fixes; `local` stops a build from silently fetching another toolchain. |
| Reproducibility | `-trimpath`, commit timestamps, no build-host data | Verified: two clean builds of a commit are byte-identical. |
| Signing | cosign keyless (GitHub OIDC) for `checksums.txt` and every image | No long-lived keys to leak or rotate. |
| Supply chain | SPDX SBOM per archive, BuildKit SBOM attestation per image, SLSA provenance for archives | Users can audit contents and verify where a binary was built. |
| Vulnerabilities | `govulncheck` in CI | Fails only on vulnerabilities in code the agent calls, so it stays actionable. |
| Runtime hardening | Read-only root, all capabilities dropped, `no-new-privileges`, `/tmp` tmpfs | Documented in the README and set in `contrib/docker/compose.yaml`. The agent needs none of what is removed; SQLite keeps temp data in memory. |
| Health check | None in the image | Docker already restarts a dead process. A check that tracked cloud connectivity would restart healthy agents during cloud outages. Revisit with the local control socket. |

## Configuration

One `Config` struct, filled in increasing precedence from defaults, an
optional YAML file, `NABBED_*` environment variables and flags.

- **Every variable has a `_FILE` variant** for Docker and Kubernetes
  secrets. Setting both is an error rather than a silent choice.
- **Unknown YAML keys are errors**, so a typo cannot silently drop a
  setting.
- **The file is optional at its default path and required when named with
  `--config`.** The image sets `NABBED_CONFIG=/config/agent.yaml`, so
  mounting a directory there just works.
- **Secrets never print.** They are redacted in `agent config`, in logs
  and in `-h` output. Flag defaults never come from secret-bearing
  environment variables.
- **The file belongs to the host.** It should be read-only to the agent,
  because the local policy from design 0001 will live in it.
- **Credentials are not configuration.** They stay in the data directory
  (design 0003). The refresh token rotates, so it can never live in an
  environment variable or a read-only secret.

## Restart behaviour

Exit code 3 means "pair again". systemd honours it with
`RestartPreventExitStatus=3`. Docker restart policies cannot exclude an exit
code, so a revoked container under `unless-stopped` restarts with Docker's
increasing back-off and logs the reason each time.

## Not verified locally

No Docker daemon was available while writing this, so the images have not
been built here. Both Dockerfiles pass BuildKit's parser and linter. CI
builds all three platforms and smoke-tests the amd64 image under the
hardening flags on every pull request. The release workflow needs one real
tag to prove signing and pushing end to end.
