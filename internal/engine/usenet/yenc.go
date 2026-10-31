package usenet

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
	"strings"
)

// maxPartBytes bounds one decoded yEnc part. Posters use ~700 KB parts.
const maxPartBytes = 16 << 20

// Part is one decoded yEnc article.
type Part struct {
	Name string
	// Begin and End are 1-based inclusive byte offsets in the target file.
	Begin, End int64
	// FileSize is the whole file's size from =ybegin.
	FileSize int64
	Data     []byte
}

// ErrYEncCRC is returned when a part's checksum does not match its data.
var ErrYEncCRC = errors.New("yenc: part crc mismatch")

// DecodeYEnc decodes a single article body that has already been
// dot-unstuffed. It verifies the part length against =ypart/=yend and the
// part checksum when present.
func DecodeYEnc(body io.Reader) (*Part, error) {
	r := bufio.NewReaderSize(body, 64<<10)

	var (
		p       Part
		inData  bool
		ended   bool
		yendKV  map[string]string
		hasPart bool
	)

	data := bytes.NewBuffer(make([]byte, 0, 768<<10))

	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Overlong line: keep reading it as data.
			if !inData {
				return nil, errors.New("yenc: header line too long")
			}
		} else if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}

		trimmed := bytes.TrimRight(line, "\r\n")

		switch {
		case !inData && bytes.HasPrefix(trimmed, []byte("=ybegin ")):
			kv := headerKV(string(trimmed[len("=ybegin "):]))
			p.Name = kv["name"]

			p.FileSize, _ = strconv.ParseInt(kv["size"], 10, 64)
			if p.FileSize <= 0 {
				return nil, errors.New("yenc: =ybegin without size")
			}

			inData = true
		case inData && !hasPart && data.Len() == 0 && bytes.HasPrefix(trimmed, []byte("=ypart ")):
			kv := headerKV(string(trimmed[len("=ypart "):]))
			p.Begin, _ = strconv.ParseInt(kv["begin"], 10, 64)
			p.End, _ = strconv.ParseInt(kv["end"], 10, 64)
			hasPart = true
		case inData && bytes.HasPrefix(trimmed, []byte("=yend")):
			yendKV = headerKV(strings.TrimPrefix(string(trimmed), "=yend"))
			ended = true
		case inData && !ended:
			decodeLine(data, trimmed)

			if data.Len() > maxPartBytes {
				return nil, errors.New("yenc: part too large")
			}
		}

		if err != nil || ended {
			break
		}
	}

	if !inData {
		return nil, errors.New("yenc: no =ybegin header")
	}

	if !ended {
		return nil, errors.New("yenc: truncated article, no =yend")
	}

	if !hasPart {
		p.Begin, p.End = 1, p.FileSize
	}

	n := int64(data.Len())
	if p.Begin < 1 || p.End < p.Begin || p.End > p.FileSize || p.End-p.Begin+1 != n {
		return nil, fmt.Errorf("yenc: part range %d-%d of %d does not match %d decoded bytes", p.Begin, p.End, p.FileSize, n)
	}

	if size, err := strconv.ParseInt(yendKV["size"], 10, 64); err == nil && size != n {
		return nil, fmt.Errorf("yenc: =yend size %d, decoded %d", size, n)
	}

	crcKey := "pcrc32"
	if !hasPart {
		crcKey = "crc32"
	}

	// The checksum is optional, but one that is present must parse: a
	// garbled one would otherwise pass the part unchecked. It counts as a
	// mismatch, so the article is tried on the next provider.
	if want, ok := yendKV[crcKey]; ok {
		w, err := strconv.ParseUint(strings.TrimSpace(want), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: unreadable %s %q", ErrYEncCRC, crcKey, truncateRunes(want, 16))
		}

		if uint32(w) != crc32.ChecksumIEEE(data.Bytes()) {
			return nil, ErrYEncCRC
		}
	}

	p.Data = data.Bytes()

	return &p, nil
}

// decodeLine appends the decoded bytes of one yEnc data line.
func decodeLine(dst *bytes.Buffer, line []byte) {
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '=' {
			i++
			if i >= len(line) {
				break // dangling escape at line end; ignore like other decoders
			}

			c = line[i] - 64
		}

		dst.WriteByte(c - 42)
	}
}

// headerKV parses "k=v k=v name=rest of line". name is always last and may
// contain spaces.
func headerKV(s string) map[string]string {
	kv := map[string]string{}

	if i := strings.Index(s, "name="); i >= 0 {
		kv["name"] = strings.TrimSpace(s[i+len("name="):])
		s = s[:i]
	}

	for _, field := range strings.Fields(s) {
		if k, v, ok := strings.Cut(field, "="); ok {
			kv[strings.ToLower(k)] = v
		}
	}

	return kv
}

// EncodeYEnc encodes data as a yEnc article part. It exists for tests and
// the fake NNTP server.
func EncodeYEnc(name string, fileSize, begin int64, data []byte, part, total int) []byte {
	var b bytes.Buffer

	fmt.Fprintf(&b, "=ybegin part=%d total=%d line=128 size=%d name=%s\r\n", part, total, fileSize, name)
	fmt.Fprintf(&b, "=ypart begin=%d end=%d\r\n", begin, begin+int64(len(data))-1)

	col := 0
	for _, c := range data {
		e := c + 42

		switch e {
		case 0, '\n', '\r', '=':
			b.WriteByte('=')
			e += 64
			col++
		case '.':
			if col == 0 {
				b.WriteByte('=')
				e += 64
				col++
			}
		}

		b.WriteByte(e)
		col++

		if col >= 128 {
			b.WriteString("\r\n")
			col = 0
		}
	}

	if col > 0 {
		b.WriteString("\r\n")
	}

	fmt.Fprintf(&b, "=yend size=%d part=%d pcrc32=%08x\r\n", len(data), part, crc32.ChecksumIEEE(data))

	return b.Bytes()
}
