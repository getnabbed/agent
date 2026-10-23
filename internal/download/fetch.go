package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/getnabbed/agent/internal/auth"
	"github.com/getnabbed/agent/internal/engine"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

const (
	// maxPayloadBytes caps an NZB or .torrent regardless of what the
	// PayloadRef claims.
	maxPayloadBytes = 64 << 20
	fetchAttempts   = 3
)

// PayloadFetcher streams a payload's bytes into w. It returns a non-retryable
// *engine.Failure when the payload is gone for good; other errors are retried.
type PayloadFetcher interface {
	Fetch(ctx context.Context, payloadID string, w io.Writer) error
}

// fetchPayload downloads ref into dir/name unless a verified copy is
// already there. Size and sha256 are checked here, whatever the fetcher.
func fetchPayload(ctx context.Context, f PayloadFetcher, ref *pb.PayloadRef, dir, name string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "open work dir: %v", err)
	}

	defer func() { _ = root.Close() }()

	if verified(root, name, ref) {
		return nil
	}

	var lastErr error

	for attempt := range fetchAttempts {
		if attempt > 0 {
			t := time.NewTimer(time.Duration(attempt*attempt) * time.Second)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}

		lastErr = fetchOnce(ctx, f, ref, root, name)
		if lastErr == nil {
			return nil
		}

		var fail *engine.Failure
		if errors.As(lastErr, &fail) && !fail.Retryable || ctx.Err() != nil {
			return lastErr
		}
	}

	return engine.Fail(pb.FailureClass_FAILURE_CLASS_PAYLOAD, true, "fetch payload: %v", lastErr)
}

func fetchOnce(ctx context.Context, f PayloadFetcher, ref *pb.PayloadRef, root *os.Root, name string) error {
	limit := min(int64(ref.GetSize()), maxPayloadBytes)
	part := name + ".part"

	out, err := root.Create(part)
	if err != nil {
		return engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "create payload: %v", err)
	}

	h := sha256.New()
	lw := &limitWriter{w: io.MultiWriter(out, h), n: limit}

	err = f.Fetch(ctx, ref.GetId(), lw)
	closeErr := out.Close()

	if lw.over {
		err = engine.Fail(pb.FailureClass_FAILURE_CLASS_PAYLOAD, false, "payload larger than its declared %d bytes", limit)
	}

	if err == nil {
		err = closeErr
	}

	if err == nil && lw.written != int64(ref.GetSize()) {
		err = engine.Fail(pb.FailureClass_FAILURE_CLASS_PAYLOAD, true, "payload is %d bytes, expected %d", lw.written, ref.GetSize())
	}

	if err == nil && !bytes.Equal(h.Sum(nil), ref.GetSha256()) {
		err = engine.Fail(pb.FailureClass_FAILURE_CLASS_PAYLOAD, false, "payload sha256 does not match")
	}

	if err != nil {
		_ = root.Remove(part)
		return err
	}

	return root.Rename(part, name)
}

func verified(root *os.Root, name string, ref *pb.PayloadRef) bool {
	f, err := root.Open(name)
	if err != nil {
		return false
	}

	defer func() { _ = f.Close() }()

	h := sha256.New()

	n, err := io.Copy(h, io.LimitReader(f, maxPayloadBytes+1))

	return err == nil && n == int64(ref.GetSize()) && bytes.Equal(h.Sum(nil), ref.GetSha256())
}

// limitWriter accepts at most n bytes and records whether more were offered.
type limitWriter struct {
	w       io.Writer
	n       int64
	written int64
	over    bool
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.written+int64(len(p)) > l.n {
		l.over = true
		return 0, errors.New("payload exceeds declared size")
	}

	n, err := l.w.Write(p)
	l.written += int64(n)

	return n, err
}

// Tokens supplies agent access tokens. *auth.TokenSource implements it.
type Tokens interface {
	Token(ctx context.Context) (auth.AccessToken, error)
	Invalidate(raw string)
}

// HTTPFetcher downloads payloads from cloud-api with the agent's token:
// GET {api}/api/agent/payloads/{id}.
type HTTPFetcher struct {
	API    *url.URL
	Client *http.Client
	Tokens Tokens
}

// Fetch implements PayloadFetcher.
func (h *HTTPFetcher) Fetch(ctx context.Context, payloadID string, w io.Writer) error {
	client := *h.Client
	client.CheckRedirect = payloadRedirectPolicy
	// Payloads can be up to 64 MB on a slow link: allow more than the API's
	// 30s, but never wait forever on a stalled server.
	client.Timeout = 15 * time.Minute

	for attempt := 0; ; attempt++ {
		tok, err := h.Tokens.Token(ctx)
		if err != nil {
			return err
		}

		u := h.API.JoinPath("api", "agent", "payloads", url.PathEscape(payloadID))

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}

		req.Header.Set("Authorization", "Bearer "+tok.Raw)

		resp, err := client.Do(req)
		if err != nil {
			if errors.Is(err, errRedirectRefused) {
				return engine.Fail(pb.FailureClass_FAILURE_CLASS_PAYLOAD, false, "%v", errRedirectRefused)
			}

			return err
		}

		switch resp.StatusCode {
		case http.StatusOK:
			_, err = io.Copy(w, resp.Body)
			_ = resp.Body.Close()

			return err
		case http.StatusUnauthorized:
			_ = resp.Body.Close()
			h.Tokens.Invalidate(tok.Raw)

			if attempt == 0 {
				continue
			}

			return fmt.Errorf("payload fetch unauthorized")
		case http.StatusNotFound:
			_ = resp.Body.Close()
			return engine.Fail(pb.FailureClass_FAILURE_CLASS_PAYLOAD, false, "payload %s not found", payloadID)
		case http.StatusGone:
			_ = resp.Body.Close()
			return engine.Fail(pb.FailureClass_FAILURE_CLASS_PAYLOAD, false, "payload %s expired", payloadID)
		default:
			_ = resp.Body.Close()
			return fmt.Errorf("payload fetch: HTTP %d", resp.StatusCode)
		}
	}
}

// errRedirectRefused marks a redirect the policy refused; retrying cannot help.
var errRedirectRefused = errors.New("payload fetch: refused redirect")

// payloadRedirectPolicy allows a payload to be served from elsewhere (for
// example a presigned object-store URL) but never downgrades to plain
// http and never sends the agent's token to another host. Integrity does
// not depend on the server: the sha256 is checked locally.
func payloadRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return fmt.Errorf("%w: too many redirects", errRedirectRefused)
	}

	first := via[0].URL
	if req.URL.Scheme != "https" && (req.URL.Scheme != first.Scheme || req.URL.Host != first.Host) {
		return fmt.Errorf("%w to %s", errRedirectRefused, req.URL.Scheme)
	}

	if req.URL.Host != first.Host {
		req.Header.Del("Authorization")
	}

	return nil
}
