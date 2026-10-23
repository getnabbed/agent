package download

import (
	"context"

	"github.com/getnabbed/agent/internal/engine"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Assembler turns engine output into complete files: PAR2 verify and
// repair, archive extraction.
type Assembler interface {
	Assemble(ctx context.Context, job engine.Job, res *engine.Result) error
}

// Importer renames completed files into a library root and reports
// ImportCompleted.
type Importer interface {
	Import(ctx context.Context, job engine.Job, res *engine.Result, d *pb.Download) error
}

// PassthroughAssembler accepts complete engine output as-is. Without PAR2
// repair it cannot recover missing usenet articles.
type PassthroughAssembler struct{}

// Assemble implements Assembler.
func (PassthroughAssembler) Assemble(_ context.Context, _ engine.Job, res *engine.Result) error {
	if res.MissingArticles > 0 {
		return engine.Fail(pb.FailureClass_FAILURE_CLASS_INCOMPLETE, false,
			"%d articles missing and PAR2 repair is not implemented yet", res.MissingArticles)
	}

	return nil
}

// UnimplementedImporter fails every import. It is retryable because the
// release is fine: once an importer exists, RetryDownload finishes the job
// from the data already on disk.
type UnimplementedImporter struct{}

// Import implements Importer.
func (UnimplementedImporter) Import(context.Context, engine.Job, *engine.Result, *pb.Download) error {
	return engine.Fail(pb.FailureClass_FAILURE_CLASS_IMPORT, true,
		"import is not implemented in this agent build; files remain in the download directory")
}
