package usenet

import (
	"testing"

	"github.com/getnabbed/agent/internal/engine"
)

// TestServer exposes the fake NNTP server to the external tests in this
// directory, which use the engine together with other packages.
type TestServer struct{ s *fakeNNTP }

// NewTestServer starts a fake NNTP server without authentication.
func NewTestServer(t *testing.T) *TestServer { return &TestServer{s: newFakeNNTP(t, "", "")} }

// Engine returns an engine whose only provider is the server.
func (ts *TestServer) Engine(t *testing.T, conns int) *Engine {
	return newTestEngine(t, ts.s.provider("", "", conns))
}

// Post posts files in articles of partSize bytes, leaving out the message
// ids in drop, and returns a job for their NZB. Message ids are
// "<name>-<part>@test", counting parts from 1.
func (ts *TestServer) Post(t *testing.T, files map[string][]byte, partSize int, drop map[string]bool) engine.Job {
	job, _ := buildJob(t, ts.s, files, partSize, drop)
	return job
}

// Bodies counts the articles served so far.
func (ts *TestServer) Bodies() int64 { return ts.s.bodies.Load() }
