package usenet

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sync"
)

const checkpointName = ".nabbed-usenet.json"

// checkpoint records which segments are on disk, so a restarted download
// skips them. It is saved only after the data files have been fsynced.
type checkpoint struct {
	mu    sync.Mutex
	Files map[int]*fileState `json:"files"`
}

type fileState struct {
	// Done is a bitset over the file's segment indexes.
	Done []uint64 `json:"done"`
	// Size is the decoded file size from =ybegin, once known.
	Size int64 `json:"size,omitempty"`
}

func (c *checkpoint) file(i int) *fileState {
	fs, ok := c.Files[i]
	if !ok {
		fs = &fileState{}
		c.Files[i] = fs
	}

	return fs
}

func (c *checkpoint) isDone(file, seg int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	fs, ok := c.Files[file]

	return ok && seg/64 < len(fs.Done) && fs.Done[seg/64]&(1<<(seg%64)) != 0
}

func (c *checkpoint) markDone(file, seg int, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	fs := c.file(file)
	for len(fs.Done) <= seg/64 {
		fs.Done = append(fs.Done, 0)
	}

	fs.Done[seg/64] |= 1 << (seg % 64)

	if size > 0 {
		fs.Size = size
	}
}

func (c *checkpoint) size(file int) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	if fs, ok := c.Files[file]; ok {
		return fs.Size
	}

	return 0
}

func loadCheckpoint(root *os.Root) (*checkpoint, error) {
	c := &checkpoint{Files: map[int]*fileState{}}

	raw, err := root.ReadFile(checkpointName)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}

	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(raw, c); err != nil || c.Files == nil {
		// A corrupt checkpoint only costs a re-download.
		return &checkpoint{Files: map[int]*fileState{}}, nil
	}

	return c, nil
}

// snapshot serialises the checkpoint as it is now.
func (c *checkpoint) snapshot() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return json.Marshal(c)
}

// write stores a snapshot taken earlier.
func writeCheckpoint(root *os.Root, raw []byte) error {
	tmp := checkpointName + ".tmp"
	if err := root.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}

	return root.Rename(tmp, checkpointName)
}
