package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/getnabbed/agent/internal/notice"
	"github.com/getnabbed/agent/internal/store"
)

func withNotices(t *testing.T, m *Manager) *notice.Reporter {
	t.Helper()

	r, err := notice.New(context.Background(), notice.Config{Store: m.store, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}

	m.SetNotifier(r)

	return r
}

// noticeState returns whether code+subject is open.
func noticeState(t *testing.T, r *notice.Reporter, code, subject string) bool {
	t.Helper()

	open, err := r.Open(context.Background(), code, subject)
	if err != nil {
		t.Fatal(err)
	}

	return open
}

// waitNotice waits until code for root "movies" reaches want.
func waitNotice(t *testing.T, r *notice.Reporter, code string, want bool) {
	const subject = "movies"

	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for noticeState(t, r, code, subject) != want {
		if time.Now().After(deadline) {
			t.Fatalf("got %s for %q open = %v, want %v", code, subject, !want, want)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func TestCheckFolders(t *testing.T) {
	ctx := context.Background()

	t.Run("none allowed while holding roots", func(t *testing.T) {
		m := newManager(t)
		r := withNotices(t, m)

		m.CheckFolders(ctx)

		if noticeState(t, r, notice.CodeLibraryNoneAllowed, "") {
			t.Fatalf("got none_allowed open with no roots, want closed")
		}

		// Roots the cloud sent before the allowed folders were taken away.
		_, err := m.store.DB().ExecContext(ctx,
			"INSERT INTO library_roots (id, kind, path, folder_template, file_template, specials_folder, updated_at) VALUES ('r1', 1, '/x', '', '', '', 0)")
		if err != nil {
			t.Fatal(err)
		}

		m.CheckFolders(ctx)

		if !noticeState(t, r, notice.CodeLibraryNoneAllowed, "") {
			t.Fatalf("got none_allowed closed with a root and nothing allowed, want open")
		}
	})

	t.Run("allowed folder missing", func(t *testing.T) {
		base := t.TempDir()
		missing := filepath.Join(base, "media")
		m := newManager(t, missing)
		r := withNotices(t, m)

		m.CheckFolders(ctx)

		if !noticeState(t, r, notice.CodeLibraryAllowedMissing, missing) {
			t.Fatalf("got allowed_missing closed for %s, want open", missing)
		}

		if noticeState(t, r, notice.CodeLibraryNoneAllowed, "") {
			t.Fatalf("got none_allowed open with a folder allowed, want closed")
		}

		if err := os.Mkdir(missing, 0o755); err != nil {
			t.Fatal(err)
		}

		m.CheckFolders(ctx)

		if noticeState(t, r, notice.CodeLibraryAllowedMissing, missing) {
			t.Fatalf("got allowed_missing open once %s exists, want closed", missing)
		}
	})
}

func TestScanRaisesRootUnusable(t *testing.T) {
	allowed, _ := layout(t)
	m := newManager(t, allowed)
	n := withNotices(t, m)

	movies := filepath.Join(allowed, "movies")
	if err := m.put(root("movies", movies)); err != nil {
		t.Fatal(err)
	}

	// Requested while the folder was there; gone by the time it runs.
	if err := m.rescan("movies", "s1"); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(movies, movies+".gone"); err != nil {
		t.Fatal(err)
	}

	runScans(t, m)

	if rs := reports(t, m, "s1"); !rs[len(rs)-1].GetIncomplete() {
		t.Fatalf("got a complete scan of a missing root, want incomplete")
	}

	waitNotice(t, n, notice.CodeLibraryRootUnusable, true)
	waitNotice(t, n, notice.CodeLibraryScanIncomplete, true)

	if err := os.Rename(movies+".gone", movies); err != nil {
		t.Fatal(err)
	}

	if err := m.rescan("movies", "s2"); err != nil {
		t.Fatal(err)
	}

	reports(t, m, "s2")
	waitNotice(t, n, notice.CodeLibraryRootUnusable, false)
	waitNotice(t, n, notice.CodeLibraryScanIncomplete, false)
}

func TestRemovingARootResolvesItsNotices(t *testing.T) {
	ctx := context.Background()
	allowed, _ := layout(t)
	m := newManager(t, allowed)
	n := withNotices(t, m)

	if err := m.put(root("movies", filepath.Join(allowed, "movies"))); err != nil {
		t.Fatal(err)
	}

	m.RootFailed(ctx, "movies", ErrNotAllowed)

	if !noticeState(t, n, notice.CodeLibraryRootUnusable, "movies") {
		t.Fatalf("got root_unusable closed after a policy failure, want open")
	}

	err := m.store.Update(ctx, func(tx *store.Tx) error { return m.Remove(ctx, tx, "movies") })
	if err != nil {
		t.Fatal(err)
	}

	if noticeState(t, n, notice.CodeLibraryRootUnusable, "movies") {
		t.Fatalf("got root_unusable open for a removed root, want closed")
	}
}
