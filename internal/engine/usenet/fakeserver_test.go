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
				if val == s.pass {
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
