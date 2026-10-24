package command

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getnabbed/agent/internal/library"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

func libraryApplier(t *testing.T, allowed ...string) *Applier {
	t.Helper()

	log := slog.New(slog.DiscardHandler)

	s, err := store.Open(context.Background(), t.TempDir(), "agent-1", log)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	p, _, err := library.NewPolicy(allowed, 0o644, 0o755)
	if err != nil {
		t.Fatal(err)
	}

	return New(Config{Store: s, Library: library.New(s, p, log), Log: log})
}

func putRoot(id, path string) *pb.Command {
	return &pb.Command{Id: id, Kind: &pb.Command_PutLibraryRoot{PutLibraryRoot: &pb.PutLibraryRoot{Root: &pb.LibraryRoot{
		Id: "root-1", Kind: pb.LibraryKind_LIBRARY_KIND_TV, Path: path, FolderTemplate: "{title}", FileTemplate: "{title}",
	}}}}
}

func TestLibraryRootCommands(t *testing.T) {
	media := t.TempDir()
	if err := os.Mkdir(filepath.Join(media, "tv"), 0o755); err != nil {
		t.Fatal(err)
	}

	// With nothing allowed, the first-run state, the user is told what to do.
	res := libraryApplier(t).HandleCommand(context.Background(), putRoot("c1", filepath.Join(media, "tv")))
	if res.GetErrorCode() != pb.CommandErrorCode_COMMAND_ERROR_CODE_ROOT_NOT_ALLOWED || !strings.Contains(res.GetMessage(), "library.allowed_roots") {
		t.Fatalf("got %v, want ROOT_NOT_ALLOWED with how to allow it", res)
	}

	a := libraryApplier(t, media)

	if res := a.HandleCommand(context.Background(), putRoot("c2", filepath.Join(media, "tv"))); res.GetOutcome() != pb.CommandOutcome_COMMAND_OUTCOME_APPLIED {
		t.Fatalf("got %v, want APPLIED inside the allowed folder", res)
	}

	bad := putRoot("c3", filepath.Join(media, "tv"))
	bad.GetPutLibraryRoot().GetRoot().FileTemplate = "../../{title}"

	if res := a.HandleCommand(context.Background(), bad); res.GetErrorCode() != pb.CommandErrorCode_COMMAND_ERROR_CODE_TEMPLATE_UNSAFE {
		t.Fatalf("got %v, want TEMPLATE_UNSAFE", res)
	}

	policy := &pb.Command{Id: "c4", Kind: &pb.Command_SetImportPolicy{SetImportPolicy: &pb.SetImportPolicy{Mode: pb.ImportMode_IMPORT_MODE_MOVE}}}
	if res := a.HandleCommand(context.Background(), policy); res.GetOutcome() != pb.CommandOutcome_COMMAND_OUTCOME_APPLIED {
		t.Fatalf("got %v for SetImportPolicy", res)
	}

	remove := &pb.Command{Id: "c5", Kind: &pb.Command_RemoveLibraryRoot{RemoveLibraryRoot: &pb.RemoveLibraryRoot{RootId: "root-1"}}}
	if res := a.HandleCommand(context.Background(), remove); res.GetOutcome() != pb.CommandOutcome_COMMAND_OUTCOME_APPLIED {
		t.Fatalf("got %v for RemoveLibraryRoot", res)
	}
}

func TestBrowseFilesystemQuery(t *testing.T) {
	media := t.TempDir()
	if err := os.Mkdir(filepath.Join(media, "movies"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := libraryApplier(t, media)

	res := a.HandleQuery(context.Background(), &pb.Query{Id: "q1", Kind: &pb.Query_BrowseFilesystem{BrowseFilesystem: &pb.BrowseFilesystem{Path: media}}})

	var got library.BrowseResult
	if !res.GetOk() || json.Unmarshal(res.GetPayload(), &got) != nil || len(got.Entries) != 1 || got.Entries[0].Name != "movies" {
		t.Fatalf("got %v (%+v), want the movies folder", res, got)
	}

	res = a.HandleQuery(context.Background(), &pb.Query{Id: "q2", Kind: &pb.Query_BrowseFilesystem{BrowseFilesystem: &pb.BrowseFilesystem{Path: "/"}}})
	if res.GetOk() || !strings.Contains(res.GetError(), "not allowed") {
		t.Fatalf("got %v browsing /, want refused", res)
	}
}
