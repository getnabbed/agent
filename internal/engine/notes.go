package engine

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"sync"
	"unicode/utf8"

	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Limits on what one DownloadUpdate carries (design 0008).
const (
	maxNotes       = 20
	maxNoteMessage = 256
	maxNoteAttrs   = 8
)

// Notes collects what happened during a stage that is worth showing the
// user: a PAR2 repair, an extraction, how files were placed. The download
// manager sends them with the update that leaves the stage. The log lines
// stay; notes are the part the cloud sees. A nil *Notes discards
// everything, so callers needn't check. It is safe for concurrent use.
type Notes struct {
	mu   sync.Mutex
	list []*pb.Note
}

// Info adds an informational note. attrs are key, value pairs.
func (n *Notes) Info(code, message string, attrs ...any) {
	n.add(pb.Severity_SEVERITY_INFO, code, message, attrs)
}

// Warn adds a warning: something went wrong but the stage carried on.
func (n *Notes) Warn(code, message string, attrs ...any) {
	n.add(pb.Severity_SEVERITY_WARNING, code, message, attrs)
}

// Error adds a note for a failure the stage ended with.
func (n *Notes) Error(code, message string, attrs ...any) {
	n.add(pb.Severity_SEVERITY_ERROR, code, message, attrs)
}

func (n *Notes) add(sev pb.Severity, code, message string, attrs []any) {
	if n == nil {
		return
	}

	note := &pb.Note{Code: code, Message: truncate(message, maxNoteMessage), Severity: sev}

	for i := 0; i+1 < len(attrs); i += 2 {
		k, ok := attrs[i].(string)
		if !ok {
			continue
		}

		if note.Attrs == nil {
			note.Attrs = map[string]string{}
		}

		note.Attrs[k] = truncate(format(attrs[i+1]), maxNoteMessage)
	}

	if len(note.Attrs) > maxNoteAttrs {
		for _, k := range slices.Sorted(maps.Keys(note.Attrs))[maxNoteAttrs:] {
			delete(note.Attrs, k)
		}
	}

	n.mu.Lock()
	n.list = append(n.list, note)
	n.mu.Unlock()
}

// Drain returns the notes collected so far and forgets them. Beyond the
// limit, the most severe are kept, in the order they happened.
func (n *Notes) Drain() []*pb.Note {
	if n == nil {
		return nil
	}

	n.mu.Lock()
	out := n.list
	n.list = nil
	n.mu.Unlock()

	if len(out) <= maxNotes {
		return out
	}

	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}

	slices.SortStableFunc(idx, func(a, b int) int { return cmp.Compare(out[b].GetSeverity(), out[a].GetSeverity()) })
	idx = idx[:maxNotes]
	slices.Sort(idx)

	kept := make([]*pb.Note, 0, maxNotes)
	for _, i := range idx {
		kept = append(kept, out[i])
	}

	return kept
}

func format(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case bool:
		return strconv.FormatBool(v)
	case interface{ String() string }:
		return v.String()
	default:
		return ""
	}
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}
