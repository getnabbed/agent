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

## Reporting bugs

Open a GitHub issue with the output of `agent version`, the exact command you
ran, what you expected, and what happened instead.

## Security issues

Please do not open public issues for security vulnerabilities. See
[SECURITY.md](SECURITY.md).

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
