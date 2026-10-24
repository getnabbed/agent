package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/getnabbed/agent/internal/engine"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

type importSetup struct {
	m     *Manager
	media string
	job   engine.Job
	res   *engine.Result
}

func newImport(t *testing.T, files map[string]string) *importSetup {
	t.Helper()

	allowed, _ := layout(t)
	m := newManager(t, allowed)

	tvRoot := &pb.LibraryRoot{
		Id: "tv", Kind: pb.LibraryKind_LIBRARY_KIND_TV, Path: filepath.Join(allowed, "tv"),
		FolderTemplate: "{series_title} ({year})/Season {season:02}",
		FileTemplate:   "{series_title} - S{season:02}E{episode:02} - {episode_title}",
	}
	if err := m.put(tvRoot); err != nil {
		t.Fatal(err)
	}

	job := engine.Job{ID: "d1", Dir: t.TempDir()}
	res := &engine.Result{}

	for name, data := range files {
		p := filepath.Join(job.Dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}

		res.Files = append(res.Files, engine.File{Path: name, Size: int64(len(data))})
	}

	return &importSetup{m: m, media: filepath.Join(allowed, "tv"), job: job, res: res}
}

func (s *importSetup) policy(t *testing.T, p *pb.SetImportPolicy) {
	t.Helper()

	if err := s.m.store.Update(context.Background(), func(tx *store.Tx) error { return s.m.SetImportPolicy(context.Background(), tx, p) }); err != nil {
		t.Fatal(err)
	}
}

func episode(protocol pb.Protocol) *pb.Download {
	season := int32(3)

	return &pb.Download{
		Id: "d1", RootId: "tv",
		Release: &pb.Release{Protocol: protocol, Quality: &pb.Quality{Resolution: "1080p", Source: "WEB-DL"}},
		Naming:  &pb.NamingContext{Title: "The Bear", Year: 2022, Season: &season, Episodes: []int32{1}, EpisodeTitles: []string{"Tomorrow"}},
	}
}

func (s *importSetup) run(d *pb.Download) (*pb.ImportCompleted, error) {
	return (&Importer{Library: s.m, Log: quiet}).Import(context.Background(), s.job, s.res, d)
}

const bearDir = "The Bear (2022)/Season 03/The Bear - S03E01 - Tomorrow"

func TestImportsTheEpisodeWithItsExtras(t *testing.T) {
	s := newImport(t, map[string]string{
		"extracted/the.bear.s03e01.mkv":        "the episode",
		"extracted/Sample/the.bear.sample.mkv": "s",
		"extracted/the.bear.s03e01.srt":        "subs",
		"extracted/eng.SRT":                    "more subs",
		"extracted/info.nfo":                   "nfo",
		"extracted/readme.txt":                 "x",
	})
	s.policy(t, &pb.SetImportPolicy{Mode: pb.ImportMode_IMPORT_MODE_AUTO, Extras: []string{".srt", "NFO"}})

	done, err := s.run(episode(pb.Protocol_PROTOCOL_USENET))
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, f := range done.GetFiles() {
		got = append(got, f.GetRelativePath())
	}

	want := []string{bearDir + ".mkv", bearDir + ".srt", bearDir + ".2.srt", bearDir + ".nfo"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}

	for _, w := range want {
		if _, err := os.Stat(filepath.Join(s.media, filepath.FromSlash(w))); err != nil {
			t.Errorf("got %v for %s", err, w)
		}
	}

	if done.GetFiles()[0].GetSize() != uint64(len("the episode")) || done.GetQuality().GetSource() != "WEB-DL" || done.GetRootId() != "tv" {
		t.Fatalf("got %v", done)
	}

	// Usenet moves: the download is gone.
	if _, err := os.Stat(filepath.Join(s.job.Dir, "extracted", "the.bear.s03e01.mkv")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v, want the usenet download moved", err)
	}

	// A rerun, after a crash before DONE was recorded, reports the same.
	again, err := s.run(episode(pb.Protocol_PROTOCOL_USENET))
	if err != nil || len(again.GetFiles()) != len(done.GetFiles()) {
		t.Fatalf("got %v, %v on a rerun", again, err)
	}
}

func TestRerunAfterAPartialMoveFinishes(t *testing.T) {
	s := newImport(t, map[string]string{"a.mkv": "video", "a.srt": "subs"})
	s.policy(t, &pb.SetImportPolicy{Mode: pb.ImportMode_IMPORT_MODE_MOVE, Extras: []string{"srt"}})

	// The video was moved before a crash; no marker was written.
	dst := filepath.Join(s.media, filepath.FromSlash(bearDir+".mkv"))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(filepath.Join(s.job.Dir, "a.mkv"), dst); err != nil {
		t.Fatal(err)
	}

	done, err := s.run(episode(pb.Protocol_PROTOCOL_USENET))
	if err != nil || len(done.GetFiles()) != 2 {
		t.Fatalf("got %v, %v; want the import finished", done, err)
	}
}

func TestTorrentImportsKeepSeeding(t *testing.T) {
	s := newImport(t, map[string]string{"data/ep.mkv": "video"})

	if _, err := s.run(episode(pb.Protocol_PROTOCOL_TORRENT)); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(s.job.Dir, "data", "ep.mkv")); err != nil {
		t.Fatalf("got %v, want the torrent's file kept for seeding", err)
	}
}

func wantImportFailure(t *testing.T, err error, retryable bool) {
	t.Helper()

	var f *engine.Failure
	if !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_IMPORT || f.Retryable != retryable {
		t.Fatalf("got %v, want an IMPORT failure with retryable=%v", err, retryable)
	}
}

func TestImportFailures(t *testing.T) {
	s := newImport(t, map[string]string{"notes.txt": "x"})

	_, err := s.run(episode(pb.Protocol_PROTOCOL_USENET))
	wantImportFailure(t, err, false) // no video: a new release is needed

	s = newImport(t, map[string]string{"ep.mkv": "video"})
	d := episode(pb.Protocol_PROTOCOL_USENET)
	d.RootId = "unknown"

	_, err = s.run(d)
	wantImportFailure(t, err, true) // the root may still arrive

	d = episode(pb.Protocol_PROTOCOL_USENET)
	d.Naming.Episodes = nil

	_, err = s.run(d)
	wantImportFailure(t, err, false)

	// The name is taken by a file already in the library.
	dst := filepath.Join(s.media, filepath.FromSlash(bearDir+".mkv"))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(dst, []byte("someone else's"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = s.run(episode(pb.Protocol_PROTOCOL_USENET))
	wantImportFailure(t, err, false)

	if got, _ := os.ReadFile(dst); string(got) != "someone else's" {
		t.Fatal("got the existing library file overwritten")
	}
}
