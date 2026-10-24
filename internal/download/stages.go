package download

import (
	"context"

	"github.com/getnabbed/agent/internal/engine"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Assembler turns engine output into complete files: PAR2 verify and
// repair, archive extraction. It returns the result to store, which may list
// files under new names, or nil to keep res. It must be safe to run again
// on its own output after a crash.
type Assembler interface {
	Assemble(ctx context.Context, job engine.Job, res *engine.Result) (*engine.Result, error)
}

// Importer places completed files into a library root. The manager queues
// the ImportCompleted it returns in the same transaction that marks the
// download done. It must be safe to run again after a crash.
type Importer interface {
	Import(ctx context.Context, job engine.Job, res *engine.Result, d *pb.Download) (*pb.ImportCompleted, error)
}

// PassthroughAssembler accepts complete engine output as-is. It does no PAR2
// repair, so it cannot recover missing usenet articles; postproc.Assembler
// can.
type PassthroughAssembler struct{}

// Assemble implements Assembler.
func (PassthroughAssembler) Assemble(_ context.Context, _ engine.Job, res *engine.Result) (*engine.Result, error) {
	if res.MissingArticles > 0 {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_INCOMPLETE, false,
			"%d articles missing and this assembler does no PAR2 repair", res.MissingArticles)
	}

	return nil, nil
}

// UnimplementedImporter fails every import. It is retryable because the
// release is fine: once an importer exists, RetryDownload finishes the job
// from the data already on disk.
type UnimplementedImporter struct{}

// Import implements Importer.
func (UnimplementedImporter) Import(context.Context, engine.Job, *engine.Result, *pb.Download) (*pb.ImportCompleted, error) {
	return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_IMPORT, true,
		"import is not implemented in this agent build; files remain in the download directory")
}
