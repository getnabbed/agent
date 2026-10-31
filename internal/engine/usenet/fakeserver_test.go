package usenet

import (
	"net"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeNNTP serves yEnc articles from memory.
type fakeNNTP struct {
	ln       net.Listener
	user     string
	pass     string
	mu       sync.Mutex
	articles map[string][]byte
	bodies   atomic.Int64

	// maxConns, when set, refuses connections beyond it the way providers
	// do: "502 Too many connections" at the greeting.
	maxConns int
	active   int
	peak     int
	accepted int
}

func newFakeNNTP(t *testing.T, user, pass string) *fakeNNTP {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s := &fakeNNTP{ln: ln, user: user, pass: pass, articles: map[string][]byte{}}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go s.serve(c)
		}
	}()

	return s
}

func (s *fakeNNTP) provider(user, pass string, conns int) Provider {
	addr := s.ln.Addr().(*net.TCPAddr)

	return Provider{Host: "127.0.0.1", Port: addr.Port, Username: user, Password: pass, Connections: conns}
}

func (s *fakeNNTP) add(id string, body []byte) {
	s.mu.Lock()
	s.articles[id] = body
	s.mu.Unlock()
}

func (s *fakeNNTP) serve(c net.Conn) {
	defer func() { _ = c.Close() }()

	tp := textproto.NewConn(c)

	s.mu.Lock()
	s.accepted++
	full := s.maxConns > 0 && s.active >= s.maxConns
	if !full {
		s.active++
		s.peak = max(s.peak, s.active)
	}
	s.mu.Unlock()

	if full {
		_ = tp.PrintfLine("502 Too many connections")
		return
	}

	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()

	_ = tp.PrintfLine("200 fake ready")

	authed := s.user == ""
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}

		verb, arg, _ := strings.Cut(line, " ")

		switch strings.ToUpper(verb) {
		case "AUTHINFO":
			kind, val, _ := strings.Cut(arg, " ")
			switch strings.ToUpper(kind) {
			case "USER":
				if val == s.user {
					_ = tp.PrintfLine("381 password required")
				} else {
					_ = tp.PrintfLine("481 bad user")
				}
			case "PASS":
				s.mu.Lock()
				ok := val == s.pass
				s.mu.Unlock()

				if ok {
					authed = true
					_ = tp.PrintfLine("281 ok")
				} else {
					_ = tp.PrintfLine("481 bad pass")
				}
			}
		case "BODY":
			if !authed {
				_ = tp.PrintfLine("480 auth required")
				continue
			}

			s.bodies.Add(1)

			s.mu.Lock()
			body, ok := s.articles[strings.Trim(arg, "<>")]
			s.mu.Unlock()

			if !ok {
				_ = tp.PrintfLine("430 no such article")
				continue
			}

			_ = tp.PrintfLine("222 0 %s", arg)
			w := tp.DotWriter()
			_, _ = w.Write(body)
			_ = w.Close()
		case "QUIT":
			_ = tp.PrintfLine("205 bye")
			return
		default:
			_ = tp.PrintfLine("500 unknown")
		}
	}
}

func (s *fakeNNTP) stats() (peak, accepted int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.peak, s.accepted
}

func (s *fakeNNTP) limit(n int) {
	s.mu.Lock()
	s.maxConns = n
	s.mu.Unlock()
}

// setPass changes the password the server accepts, as when a user renews
// an account.
func (s *fakeNNTP) setPass(pass string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pass = pass
}

// stallServer accepts connections and never greets, like a provider that
// hangs. close drops every connection it accepted.
type stallServer struct {
	ln       net.Listener
	accepted chan struct{}

	mu    sync.Mutex
	conns []net.Conn
}

func newStallServer(t *testing.T) *stallServer {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s := &stallServer{ln: ln, accepted: make(chan struct{}, 1)}
	t.Cleanup(s.close)

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.mu.Unlock()

			select {
			case s.accepted <- struct{}{}:
			default:
			}
		}
	}()

	return s
}

func (s *stallServer) provider(conns int) Provider {
	return Provider{Host: "127.0.0.1", Port: s.ln.Addr().(*net.TCPAddr).Port, Connections: conns}
}

func (s *stallServer) close() {
	_ = s.ln.Close()

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, c := range s.conns {
		_ = c.Close()
	}
}
