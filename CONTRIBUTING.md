# Contributing

Thanks for your interest in contributing to `agent`.

## Getting started

1. Fork and clone the repository.
2. Install Go 1.27.3 or newer and [golangci-lint](https://golangci-lint.run/).
3. Run `make all` to lint, test and build.

## Making changes

- Open an issue first for anything beyond a small fix so the approach can be
  discussed before you invest significant time.
- Keep pull requests focused. Separate unrelated changes into separate PRs.
- Add or update tests for any behaviour you change.
- Run `make fmt` and `make lint` before pushing. CI runs the same checks.
- Write commit messages in the imperative mood with a short summary line
  (for example `cli: add run command`).

## Changing the wire contract

The agent talks to the Nabbed cloud over the gRPC contract in `proto/`. If
your change touches it:

1. Install [buf](https://buf.build/docs/cli/installation/). CI uses 1.75.0.
2. Run `make proto` to regenerate `pkg/proto`, and commit the result with
   your change. Never edit `pkg/proto` by hand.
3. Run `make proto-lint proto-breaking`. The breaking check compares your
   change with `main` on GitHub, so it needs no Buf account or token.
4. Keep changes backwards compatible: add fields, messages and enum values,
   and never remove, rename, renumber or retype existing ones. The
   breaking check catches these.

CI runs the same checks on your pull request. You never need access to the
Buf Schema Registry: the contract is published from `main` after your
change merges.

### Publishing (maintainers)

- Merging a change to `proto/` publishes it. The Proto publish workflow
  pushes labels `main` and `git-<sha12>` to `buf.build/nabbed/agent`.
- If the cloud needs a contract before the agent PR merges, push the branch
  to this repository and run `make proto-push-label`. It pushes only the
  `git-<sha12>` label and never moves `main`.
- If BSR `main` falls behind `main`, run the Proto publish workflow on
  `main` from the Actions tab, or run
  `gh workflow run proto-publish.yml --repo getnabbed/agent --ref main`.
- `make proto-push` is the manual fallback. It refuses to run anywhere but a
  clean `main` that matches this repository's `main`.

## Reporting bugs

Open a GitHub issue with the output of `agent version`, the exact command you
ran, what you expected, and what happened instead.

## Security issues

Please do not open public issues for security vulnerabilities. See
[SECURITY.md](SECURITY.md).

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
