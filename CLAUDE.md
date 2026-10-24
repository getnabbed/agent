# CLAUDE.md

Open source Go agent for Nabbed. Module `github.com/getnabbed/agent`.
Design docs live in `docs/design/`. 0003 covers sessions, credentials and
token refresh; 0005 covers the usenet pipeline (NNTP, yEnc, PAR2, extraction); 0006 the sealed
usenet sources contract with the cloud; 0007 library import (allowed roots,
naming, placement).

## Commands

```sh
make all          # lint, test, build
make test         # go test -race -cover ./...
make lint         # golangci-lint (config in .golangci.yml)
make proto        # regenerate pkg/proto from proto/ with buf
make proto-lint   # buf lint + format check
make proto-breaking # buf breaking against the published module
make proto-push   # publish proto/ to buf.build/nabbed/agent (labels main, git-<sha>)
make vuln         # govulncheck on code paths the agent calls
make docker       # build the container image from source
make snapshot     # unsigned GoReleaser build of every release archive
```

Base images are pinned by digest in literal `FROM` lines (Dependabot updates
them); GitHub Actions are pinned by commit SHA. Keep it that way.

Generated code in `pkg/proto/` is committed and never edited by hand. Change
`proto/` and run `make proto`.

The wire contract is published to the Buf Schema Registry as
`buf.build/nabbed/agent`, and the cloud generates its code from there. After
changing `proto/`: run `make proto proto-lint proto-breaking`, commit, then
`make proto-push`. Tell the cloud the `git-<sha>` label to pin. Never make a
breaking change in `v2`.

## Go style guide

### Separate blocks of work with blank lines

A function body is a sequence of steps. Each step is a statement plus the
code that handles its result, usually the error check. Put one blank line
between steps, and one before the final `return`.

```go
func openLocked(e *env) (*credstore.Store, *lock.Lock, error) {
	store, err := credstore.Open(e.dataDir, e.log)
	if err != nil {
		return nil, nil, err
	}

	l, err := lock.Acquire(store.Dir())
	if errors.Is(err, lock.ErrLocked) {
		return nil, nil, fmt.Errorf("%w (%s); stop the running agent first", err, store.Dir())
	}
	if err != nil {
		return nil, nil, err
	}

	return store, l, nil
}
```

Rules:

- A call and its error handling are one block. No blank line between
  `x, err := f()` and `if err != nil`.
- Checks on the same result stay together, as with `errors.Is` followed by
  `err != nil` above.
- Put a blank line after a closing `}` when the next statement starts a
  new step.
- Put a blank line before the final `return` of a function with more than
  one step.
- Keep tightly coupled statements together: a lock, its critical section
  and its unlock. Short assignments that build one value, such as
  `tr := ...; tr.TLSClientConfig = cfg`, also stay together.
- One-line functions and single-step functions need no blank lines.
- The same applies inside loop bodies, `case` bodies and closures.

### Errors

- Wrap with context using `%w`: `fmt.Errorf("persist refresh intent: %w", err)`.
  Use `%w: %w` when both a sentinel and the cause matter.
- Compare with `errors.Is` and `errors.As`, never `==`.
- Sentinel errors are exported `var ErrX = errors.New("...")` with lowercase
  messages and no trailing punctuation.
- Ignore errors only explicitly: `_ = f.Close()`, or
  `defer func() { _ = l.Release() }()`.

### Logging

- Use `log/slog` with structured key/value attributes, never `fmt.Printf`
  for diagnostics. Messages are lowercase phrases.
- Never log secrets. Types holding secrets implement `slog.LogValuer` and
  redact, as `credstore.Credentials` does.
- Prefer a `*slog.Logger` passed in, falling back to `slog.Default()`.

### Packages and layout

- `cmd/agent` holds only `main`. Behaviour lives in `internal/...`.
  Generated protobuf lives in `pkg/proto/...`, imported as
  `pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"`.
- No cgo. The binary must cross-compile with `CGO_ENABLED=0` for Linux,
  macOS and Windows. Use `golang.org/x/sys` for platform APIs, and split
  platform code with `_unix.go` and `_windows.go` build-tagged files.
- Imports go in three groups: stdlib, third party, then
  `github.com/getnabbed/agent` (enforced by goimports `local-prefixes`).
- Every package has a package doc comment, and every exported identifier
  has a doc comment that starts with its name.

### Concurrency

- Exactly one goroutine calls `Send` on a gRPC stream.
- Every goroutine has a clear owner and a way to stop, through context
  cancellation. Cancel before you `wg.Wait()`.
- Do not use timers for wall-clock deadlines such as token expiry. Go's
  monotonic clock pauses during sleep on Linux and macOS. Check
  `time.Now()` at the point of use.

### Tests

- Use the standard `testing` package with no assertion libraries. Fail with
  `t.Fatalf("got X = %v, want %v", ...)`.
- Prefer in-memory fakes, such as `httptest` and `bufconn`, over mocks.
- Run the race detector: `make test` does this.
