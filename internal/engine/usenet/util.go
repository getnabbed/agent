package usenet

import (
	"bytes"
	"io"
	"path/filepath"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// baseName keeps only the final element of a path the state machine gave
// us, so the payload is always opened inside the work dir root.
func baseName(p string) string { return filepath.Base(p) }
