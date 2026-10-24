package source

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/getnabbed/agent/internal/engine/usenet"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

var quiet = slog.New(slog.DiscardHandler)

// fakeEngine records provider changes. Tests look up the source "src-1".
type fakeEngine struct {
	mu        sync.Mutex
	providers map[string]usenet.Provider
	sets      int
}

func (f *fakeEngine) SetProvider(id string, p usenet.Provider) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.providers[id] = p
	f.sets++
}

func (f *fakeEngine) RemoveProvider(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.providers, id)
}

func (f *fakeEngine) src1() (usenet.Provider, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	p, ok := f.providers["src-1"]

	return p, ok
}

type fixture struct {
	key    *ecdh.PrivateKey
	store  *store.Store
	engine *fakeEngine
	m      *Manager
	dir    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	f := &fixture{key: key, dir: t.TempDir()}
	f.reopen(t)

	return f
}

// reopen simulates an agent restart: a new store handle, engine and manager.
func (f *fixture) reopen(t *testing.T) {
	t.Helper()

	if f.store != nil {
		_ = f.store.Close()
	}

	st, err := store.Open(context.Background(), f.dir, "agent-1", quiet)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	f.store = st
	f.engine = &fakeEngine{providers: map[string]usenet.Provider{}}
	f.m = New(Config{Store: st, Key: f.key, Engine: f.engine, Log: quiet})

	if err := f.m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) put(s *pb.Source) error {
	return f.store.Update(context.Background(), func(tx *store.Tx) error { return f.m.Put(context.Background(), tx, s) })
}

func sealJSON(t *testing.T, pub *ecdh.PublicKey, v any) []byte {
	t.Helper()

	raw, ok := v.([]byte)
	if !ok {
		var err error
		if raw, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}

	sealed, err := Seal(pub, raw)
	if err != nil {
		t.Fatal(err)
	}

	return sealed
}

func validConfig() map[string]any {
	return map[string]any{"v": 1, "host": "news.newshosting.com", "port": 563, "tls": true, "username": "u", "password": "p@ss", "connections": 30}
}

func (f *fixture) source(t *testing.T, id string, cfg any) *pb.Source {
	t.Helper()

	return &pb.Source{
		Id: id, Kind: KindNNTP, DisplayName: "Newshosting", Enabled: true,
		SealedConfig: sealJSON(t, f.key.PublicKey(), cfg),
		SealedToKey:  f.key.PublicKey().Bytes(),
	}
}

func TestPutAppliesAfterCommitAndSurvivesRestart(t *testing.T) {
	f := newFixture(t)

	if err := f.put(f.source(t, "src-1", validConfig())); err != nil {
		t.Fatal(err)
	}

	p, ok := f.engine.src1()
	if !ok || p.Host != "news.newshosting.com" || p.Port != 563 || !p.TLS || p.Username != "u" || p.Password != "p@ss" || p.Connections != 30 || p.Dial == nil {
		t.Fatalf("got provider %+v (ok=%v), want the unsealed config with a guarded dialer", p, ok)
	}

	f.reopen(t)

	if p, ok := f.engine.src1(); !ok || p.Password != "p@ss" {
		t.Fatalf("got provider %+v (ok=%v) after restart, want it reloaded", p, ok)
	}
}

func TestStoredConfigStaysSealed(t *testing.T) {
	f := newFixture(t)

	if err := f.put(f.source(t, "src-1", validConfig())); err != nil {
		t.Fatal(err)
	}

	var blob []byte
	if err := f.store.DB().QueryRow("SELECT sealed_config FROM sources WHERE id = 'src-1'").Scan(&blob); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(blob), "p@ss") || strings.Contains(string(blob), "newshosting") {
		t.Fatal("the stored source contains plaintext")
	}
}

func TestRolledBackPutIsNotApplied(t *testing.T) {
	f := newFixture(t)

	err := f.store.Update(context.Background(), func(tx *store.Tx) error {
		if err := f.m.Put(context.Background(), tx, f.source(t, "src-1", validConfig())); err != nil {
			return err
		}

		return errors.New("later step failed")
	})
	if err == nil {
		t.Fatal("got nil error from the failing transaction")
	}

	if _, ok := f.engine.src1(); ok {
		t.Fatal("got the provider applied although the transaction rolled back")
	}
}

func TestRepeatedPutKeepsLivePool(t *testing.T) {
	f := newFixture(t)
	s := f.source(t, "src-1", validConfig())

	for range 3 {
		if err := f.put(s); err != nil {
			t.Fatal(err)
		}
	}

	if f.engine.sets != 1 {
		t.Fatalf("got %d SetProvider calls for an unchanged source, want 1", f.engine.sets)
	}

	if err := f.put(f.source(t, "src-1", validConfig())); err != nil { // resealed: new bytes
		t.Fatal(err)
	}

	if f.engine.sets != 2 {
		t.Fatalf("got %d SetProvider calls after a changed config, want 2", f.engine.sets)
	}
}

func TestDisableAndRemove(t *testing.T) {
	f := newFixture(t)
	s := f.source(t, "src-1", validConfig())

	if err := f.put(s); err != nil {
		t.Fatal(err)
	}

	s.Enabled = false
	if err := f.put(s); err != nil {
		t.Fatal(err)
	}

	if _, ok := f.engine.src1(); ok {
		t.Fatal("got a disabled source still configured")
	}

	s.Enabled = true
	if err := f.put(s); err != nil {
		t.Fatal(err)
	}

	err := f.store.Update(context.Background(), func(tx *store.Tx) error { return f.m.Remove(context.Background(), tx, "src-1") })
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := f.engine.src1(); ok {
		t.Fatal("got a removed source still configured")
	}

	f.reopen(t)

	if _, ok := f.engine.src1(); ok {
		t.Fatal("got a removed source back after restart")
	}

	err = f.store.Update(context.Background(), func(tx *store.Tx) error { return f.m.Remove(context.Background(), tx, "never-existed") })
	if err != nil {
		t.Fatalf("got %v removing an unknown source, want success", err)
	}
}

func TestRejections(t *testing.T) {
	f := newFixture(t)

	other, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	with := func(k string, v any) map[string]any {
		c := validConfig()
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}

		return c
	}

	cases := []struct {
		name string
		src  func() *pb.Source
		want error
	}{
		{"unknown kind", func() *pb.Source { s := f.source(t, "a", validConfig()); s.Kind = "sabnzbd"; return s }, ErrKindUnsupported},
		{"bad id", func() *pb.Source { return f.source(t, "a/b", validConfig()) }, ErrInvalid},
		{"sealed to another key", func() *pb.Source {
			s := f.source(t, "a", validConfig())
			s.SealedToKey = other.PublicKey().Bytes()
			return s
		}, ErrUnsealable},
		{"sealed with another key, no sealed_to_key", func() *pb.Source {
			s := f.source(t, "a", validConfig())
			s.SealedConfig, s.SealedToKey = sealJSON(t, other.PublicKey(), validConfig()), nil
			return s
		}, ErrUnsealable},
		{"tampered", func() *pb.Source {
			s := f.source(t, "a", validConfig())
			s.SealedConfig[len(s.SealedConfig)-1] ^= 1
			return s
		}, ErrUnsealable},
		{"empty", func() *pb.Source { s := f.source(t, "a", validConfig()); s.SealedConfig = nil; return s }, ErrInvalid},
		{"not json", func() *pb.Source { return f.source(t, "a", []byte("host=x")) }, ErrInvalid},
		{"unknown field", func() *pb.Source { return f.source(t, "a", with("user", "u")) }, ErrInvalid},
		{"version 2", func() *pb.Source { return f.source(t, "a", with("v", 2)) }, ErrInvalid},
		{"plaintext", func() *pb.Source { return f.source(t, "a", with("tls", false)) }, ErrInvalid},
		{"no host", func() *pb.Source { return f.source(t, "a", with("host", nil)) }, ErrInvalid},
		{"host with path", func() *pb.Source { return f.source(t, "a", with("host", "news.example.com/x")) }, ErrInvalid},
		{"password only", func() *pb.Source { return f.source(t, "a", with("username", nil)) }, ErrInvalid},
		{"crlf in password", func() *pb.Source { return f.source(t, "a", with("password", "p\r\nQUIT")) }, ErrInvalid},
		{"too many connections", func() *pb.Source { return f.source(t, "a", with("connections", 101)) }, ErrInvalid},
		{"bad port", func() *pb.Source { return f.source(t, "a", with("port", 70000)) }, ErrInvalid},
		{"private ip", func() *pb.Source { return f.source(t, "a", with("host", "192.168.1.10")) }, ErrHostBlocked},
		{"loopback v6", func() *pb.Source { return f.source(t, "a", with("host", "::1")) }, ErrHostBlocked},
		{"metadata", func() *pb.Source { return f.source(t, "a", with("host", "169.254.169.254")) }, ErrHostBlocked},
		{"localhost", func() *pb.Source { return f.source(t, "a", with("host", "LocalHost")) }, ErrHostBlocked},
		{"mdns", func() *pb.Source { return f.source(t, "a", with("host", "nas.local")) }, ErrHostBlocked},
	}

	for _, c := range cases {
		err := f.put(c.src())
		if !errors.Is(err, c.want) {
			t.Fatalf("%s: got err = %v, want %v", c.name, err, c.want)
		}

		if strings.Contains(fmt.Sprint(err), "p@ss") {
			t.Fatalf("%s: error leaks the password: %v", c.name, err)
		}
	}

	if len(f.engine.providers) != 0 {
		t.Fatalf("got %d providers configured from rejected sources, want 0", len(f.engine.providers))
	}
}

func TestSourceLimit(t *testing.T) {
	f := newFixture(t)

	for i := range maxSources {
		if err := f.put(f.source(t, fmt.Sprintf("s%d", i), validConfig())); err != nil {
			t.Fatal(err)
		}
	}

	if err := f.put(f.source(t, "one-more", validConfig())); !errors.Is(err, ErrLimit) {
		t.Fatalf("got err = %v, want ErrLimit", err)
	}

	if err := f.put(f.source(t, "s0", validConfig())); err != nil {
		t.Fatalf("got err = %v updating an existing source at the limit, want success", err)
	}
}

func TestDefaults(t *testing.T) {
	p, err := parseNNTP([]byte(`{"v":1,"host":"News.Example.COM.","tls":true}`))
	if err != nil {
		t.Fatal(err)
	}

	if p.Host != "news.example.com" || p.Port != 563 || p.Connections != 8 || p.Username != "" {
		t.Fatalf("got %+v, want lowercased host without the dot, port 563, 8 connections", p)
	}
}
