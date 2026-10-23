package usenet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"strconv"
	"time"
)

// maxArticleBytes bounds a raw article body before decoding.
const maxArticleBytes = 24 << 20

var (
	// ErrArticleNotFound is NNTP 430: the provider does not have it.
	ErrArticleNotFound = errors.New("nntp: article not found")
	// ErrAuth means the provider rejected the credentials.
	ErrAuth = errors.New("nntp: authentication failed")
)

// Provider is a usenet server account.
type Provider struct {
	Host        string
	Port        int
	TLS         bool
	Username    string
	Password    string
	Connections int
	// TLSConfig overrides the default verification, for tests.
	TLSConfig *tls.Config
}

// String describes the provider without its password.
func (p Provider) String() string {
	scheme := "nntp"
	if p.TLS {
		scheme = "nntps"
	}

	return fmt.Sprintf("%s://%s@%s:%d (%d connections)", scheme, p.Username, p.Host, p.Port, p.Connections)
}

// ParseProviderURL parses nntps://user:pass@host:563?connections=8.
func ParseProviderURL(raw string) (Provider, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// url.Parse errors quote the whole URL, password included: never wrap.
		return Provider{}, errors.New("invalid nntp url: check that special characters in the user name and password are percent-encoded")
	}

	p := Provider{Host: u.Hostname(), Connections: 8}

	switch u.Scheme {
	case "nntps":
		p.TLS, p.Port = true, 563
	case "nntp":
		p.Port = 119
	default:
		return Provider{}, fmt.Errorf("nntp url scheme must be nntp or nntps, got %q", u.Scheme)
	}

	if p.Host == "" {
		return Provider{}, errors.New("nntp url has no host")
	}

	if port := u.Port(); port != "" {
		if p.Port, err = strconv.Atoi(port); err != nil {
			return Provider{}, fmt.Errorf("invalid nntp port: %w", err)
		}
	}

	if u.User != nil {
		p.Username = u.User.Username()
		p.Password, _ = u.User.Password()
	}

	if !p.TLS && p.Username != "" && !isLoopbackHost(p.Host) {
		return Provider{}, errors.New("refusing to send usenet credentials over plaintext nntp://; use nntps://")
	}

	if c := u.Query().Get("connections"); c != "" {
		if p.Connections, err = strconv.Atoi(c); err != nil || p.Connections < 1 || p.Connections > 100 {
			return Provider{}, errors.New("connections must be 1-100")
		}
	}

	return p, nil
}

// conn is one authenticated NNTP connection.
type conn struct {
	nc      net.Conn
	tp      *textproto.Conn
	timeout time.Duration
}

// dial connects, reads the greeting and authenticates.
func dial(ctx context.Context, p Provider, timeout time.Duration) (*conn, error) {
	addr := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}

	var (
		nc  net.Conn
		err error
	)

	if p.TLS {
		cfg := p.TLSConfig
		if cfg == nil {
			cfg = &tls.Config{ServerName: p.Host, MinVersion: tls.VersionTLS12}
		}

		nc, err = (&tls.Dialer{NetDialer: d, Config: cfg}).DialContext(ctx, "tcp", addr)
	} else {
		nc, err = d.DialContext(ctx, "tcp", addr)
	}

	if err != nil {
		return nil, err
	}

	c := &conn{nc: nc, tp: textproto.NewConn(&lineLimitConn{Conn: nc}), timeout: timeout}

	if err := c.handshake(p); err != nil {
		_ = c.close()
		return nil, err
	}

	return c, nil
}

func (c *conn) handshake(p Provider) error {
	c.deadline()

	if _, _, err := c.tp.ReadCodeLine(20); err != nil { // 200 or 201
		return fmt.Errorf("nntp greeting: %w", err)
	}

	if p.Username == "" {
		return nil
	}

	code, _, err := c.cmd("AUTHINFO USER %s", p.Username)
	if err != nil {
		return err
	}

	if code == 381 {
		code, _, err = c.cmd("AUTHINFO PASS %s", p.Password)
		if err != nil {
			return err
		}
	}

	if code != 281 {
		return fmt.Errorf("%w (code %d)", ErrAuth, code)
	}

	return nil
}

func (c *conn) cmd(format string, args ...any) (int, string, error) {
	c.deadline()

	id, err := c.tp.Cmd(format, args...)
	if err != nil {
		return 0, "", err
	}

	c.tp.StartResponse(id)
	defer c.tp.EndResponse(id)

	code, msg, err := c.tp.ReadCodeLine(0)
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) {
		return protoErr.Code, protoErr.Msg, nil
	}

	return code, msg, err
}

// body fetches an article body, dot-unstuffed and size-capped.
func (c *conn) body(messageID string) ([]byte, error) {
	code, msg, err := c.cmd("BODY <%s>", messageID)
	if err != nil {
		return nil, err
	}

	switch code {
	case 222:
	case 430, 423:
		return nil, ErrArticleNotFound
	case 480, 481:
		return nil, ErrAuth
	default:
		return nil, fmt.Errorf("nntp BODY: %d %s", code, msg)
	}

	c.deadline()

	data, err := io.ReadAll(io.LimitReader(c.tp.DotReader(), maxArticleBytes+1))
	if err != nil {
		return nil, err
	}

	if len(data) > maxArticleBytes {
		return nil, errors.New("nntp: article too large")
	}

	return data, nil
}

func (c *conn) deadline() { _ = c.nc.SetDeadline(time.Now().Add(c.timeout)) }

func (c *conn) close() error {
	_ = c.nc.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = c.tp.Cmd("QUIT")

	return c.tp.Close()
}

// maxLineBytes bounds any single line from the server. yEnc lines are ~130
// bytes and status lines are short; textproto itself has no limit.
const maxLineBytes = 64 << 10

var errLineTooLong = errors.New("nntp: server sent an overlong line")

// lineLimitConn fails reads once a line exceeds maxLineBytes, so a hostile
// server cannot make the client buffer unbounded data.
type lineLimitConn struct {
	net.Conn

	run int
}

func (c *lineLimitConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)

	for _, b := range p[:n] {
		if b == '\n' {
			c.run = 0
			continue
		}

		c.run++
		if c.run > maxLineBytes {
			return 0, errLineTooLong
		}
	}

	return n, err
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}
