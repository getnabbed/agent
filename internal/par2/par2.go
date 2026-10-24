// Package par2 reads PAR 2.0 recovery sets, verifies files against them and
// repairs damaged or missing files from recovery slices.
//
// Parsing reads the packets in .par2 files. Verification finds which
// downloaded files match which recovery-set files (by name, or by size and
// the hash of their first 16 KiB when names are obfuscated) and which slices
// of each are damaged. Repair rebuilds those slices with Reed–Solomon
// decoding over GF(2^16), exactly as PAR 2.0 specifies, reading recovery
// slices from the volume files where the parser found them rather than
// holding them in memory. Design 0005 describes how the agent uses it.
//
// All input is untrusted: packets are bounded in size, checked against their
// MD5, and ignored when they belong to another recovery set. Repair bounds
// its memory and refuses sets larger than PAR 2.0 allows.
package par2

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/md5" //nolint:gosec // PAR2 is defined in terms of MD5; it is used for integrity, not security.
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
)

const (
	headerSize = 64
	// maxPacket bounds every packet except recovery slices.
	maxPacket = 16 << 20
	// maxSlice bounds a recovery slice packet.
	maxSlice = 128 << 20
	maxFiles = 32768
	// hash16k is how much of each file the 16k hash covers.
	hash16k = 16 << 10
	// maxRecoveryRecords bounds the recovery slice locations kept per set.
	maxRecoveryRecords = 1 << 16
)

var magic = []byte("PAR2\x00PKT")

// Packet types (16 bytes, NUL-padded).
var (
	typeMain     = packetType("PAR 2.0\x00Main")
	typeFileDesc = packetType("PAR 2.0\x00FileDesc")
	typeIFSC     = packetType("PAR 2.0\x00IFSC")
	typeRecv     = packetType("PAR 2.0\x00RecvSlic")
	typeCreator  = packetType("PAR 2.0\x00Creator")
)

func packetType(s string) (t [16]byte) {
	copy(t[:], s)
	return t
}

var (
	// ErrNoRecoverySet means no valid main packet was found.
	ErrNoRecoverySet = errors.New("par2: no recovery set found")
	// ErrIncomplete means the main packet lists files whose descriptions are
	// missing.
	ErrIncomplete = errors.New("par2: recovery set is missing file descriptions")
	// ErrTooManySlices means the recovery set has more input slices than
	// PAR 2.0 allows.
	ErrTooManySlices = errors.New("par2: recovery set has too many input slices")
)

// ID is a 16-byte PAR2 identifier: a recovery set or a file.
type ID [16]byte

// Set is one recovery set.
type Set struct {
	ID        ID
	SliceSize int64
	// Files are protected by recovery data, in main-packet order, which is
	// also the order of their input slices.
	Files []*File
	// Unprotected are listed for verification only.
	Unprotected []*File
	// Exponents of the recovery slices seen in the parsed input.
	RecoveryExponents map[uint32]bool
	// Recovery locates the recovery slices read with ReadFile, one per
	// exponent, sorted by exponent.
	Recovery []RecoverySlice
	Creator  string
}

// RecoverySlice locates one recovery slice's data in a volume file.
type RecoverySlice struct {
	Exponent uint32
	// Source is the name passed to ReadFile.
	Source string
	// Offset is where the slice's SliceSize bytes of data start in Source.
	Offset int64
}

// File is one file of a recovery set.
type File struct {
	ID      ID
	Name    string
	Size    int64
	MD5     [16]byte
	MD5_16k [16]byte
	// Slices holds per-slice checksums, or nil without an IFSC packet.
	Slices []Slice
}

// SliceCount is the number of slices the file occupies.
func (f *File) SliceCount(sliceSize int64) int {
	if sliceSize <= 0 {
		return 0
	}

	return int((f.Size + sliceSize - 1) / sliceSize)
}

// Slice is the checksum of one slice; the last slice of a file is hashed
// padded with zeros to the full slice size.
type Slice struct {
	MD5   [16]byte
	CRC32 uint32
}

// Parser accumulates packets from any number of .par2 files.
type Parser struct {
	sets  map[ID]*pending
	order []ID // sets in the order their main packet was seen
}

type pending struct {
	main      []byte
	descs     map[ID]*File
	slices    map[ID][]Slice
	exponents map[uint32]bool
	recovery  []located
	creator   string
}

// located is a verified recovery slice whose data length is checked against
// the slice size once the main packet is known.
type located struct {
	RecoverySlice
	length int64
}

// NewParser returns an empty Parser.
func NewParser() *Parser { return &Parser{sets: map[ID]*pending{}} }

// Read consumes one .par2 file. Damaged or foreign bytes between packets are
// skipped, as the format allows. Recovery slices are counted but cannot be
// used for repair; read volumes with ReadFile for that.
func (p *Parser) Read(r io.Reader) error { return p.ReadFile("", r) }

// ReadFile is Read for the file called name, and also records where each
// recovery slice's data lies in it so Repair can read it later. r must
// start at the beginning of the file.
func (p *Parser) ReadFile(name string, r io.Reader) error {
	sc := &scanner{br: bufio.NewReaderSize(r, 64<<10), name: name}

	for {
		ok, err := sc.seekMagic()
		if err != nil {
			return err
		}

		if !ok {
			return nil
		}

		if err := p.packet(sc); err != nil {
			return err
		}
	}
}

// scanner reads one file and tracks the offset of the next unread byte.
type scanner struct {
	br   *bufio.Reader
	name string
	pos  int64
}

func (s *scanner) Read(b []byte) (int, error) {
	n, err := s.br.Read(b)
	s.pos += int64(n)

	return n, err
}

func (s *scanner) discard(n int) error {
	d, err := s.br.Discard(n)
	s.pos += int64(d)

	return err
}

// seekMagic advances to the next packet magic. It returns false at EOF.
func (s *scanner) seekMagic() (bool, error) {
	for {
		b, err := s.br.Peek(len(magic))
		if errors.Is(err, io.EOF) {
			return false, nil
		}

		if err != nil {
			return false, err
		}

		if bytes.Equal(b, magic) {
			return true, nil
		}

		// Skip to the next byte that could start a magic.
		i := bytes.IndexByte(b[1:], magic[0])
		if i < 0 {
			i = len(b) - 1
		}

		if err := s.discard(i + 1); err != nil {
			return false, err
		}
	}
}

// packet reads one packet starting at the magic. A packet that fails its
// checks is skipped by advancing past the magic, so the scan resyncs.
func (p *Parser) packet(sc *scanner) error {
	hdr, err := sc.br.Peek(headerSize)
	if err != nil {
		// Truncated header at the end of the file.
		_ = sc.discard(len(magic))
		return nil
	}

	length := binary.LittleEndian.Uint64(hdr[8:16])

	var (
		sum   [16]byte
		set   ID
		ptype [16]byte
	)

	copy(sum[:], hdr[16:32])
	copy(set[:], hdr[32:48])
	copy(ptype[:], hdr[48:64])

	limit := uint64(maxPacket)
	if ptype == typeRecv {
		limit = maxSlice
	}

	if length < headerSize || length%4 != 0 || length > limit {
		return sc.discard(len(magic))
	}

	bodyLen := int64(length - headerSize)

	if ptype == typeRecv {
		return p.recovery(sc, sum, set, bodyLen)
	}

	if err := sc.discard(headerSize); err != nil {
		return err
	}

	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(sc, body); err != nil {
		return nil //nolint:nilerr // truncated final packet: ignore it.
	}

	h := md5.New() //nolint:gosec // see import
	h.Write(set[:])
	h.Write(ptype[:])
	h.Write(body)

	if !bytes.Equal(h.Sum(nil), sum[:]) {
		return nil // damaged packet; other copies may survive
	}

	p.apply(set, ptype, body)

	return nil
}

// recovery checks a recovery slice's MD5 while discarding its data, and
// records its exponent and where its data lies.
func (p *Parser) recovery(sc *scanner, sum [16]byte, set ID, bodyLen int64) error {
	if bodyLen < 4 {
		return sc.discard(len(magic))
	}

	if err := sc.discard(headerSize); err != nil {
		return err
	}

	h := md5.New() //nolint:gosec // see import
	h.Write(set[:])
	h.Write(typeRecv[:])

	var exp [4]byte
	if _, err := io.ReadFull(sc, exp[:]); err != nil {
		return nil //nolint:nilerr // truncated final packet
	}

	h.Write(exp[:])
	offset := sc.pos

	if _, err := io.CopyN(h, sc, bodyLen-4); err != nil {
		return nil //nolint:nilerr // truncated final packet
	}

	if !bytes.Equal(h.Sum(nil), sum[:]) {
		return nil
	}

	s := p.get(set)
	e := binary.LittleEndian.Uint32(exp[:])
	s.exponents[e] = true

	// Only 65535 exponents are distinct, so more records are padding.
	if sc.name != "" && len(s.recovery) < maxRecoveryRecords {
		s.recovery = append(s.recovery, located{
			RecoverySlice: RecoverySlice{Exponent: e, Source: sc.name, Offset: offset},
			length:        bodyLen - 4,
		})
	}

	return nil
}

func (p *Parser) get(set ID) *pending {
	s, ok := p.sets[set]
	if !ok {
		s = &pending{descs: map[ID]*File{}, slices: map[ID][]Slice{}, exponents: map[uint32]bool{}}
		p.sets[set] = s
	}

	return s
}

func (p *Parser) apply(set ID, ptype [16]byte, body []byte) {
	switch ptype {
	case typeMain:
		// The set id is the MD5 of the main packet body.
		if md5.Sum(body) != set || len(body) < 12 || (len(body)-12)%16 != 0 { //nolint:gosec // see import
			return
		}

		s := p.get(set)
		if s.main == nil {
			s.main = body
			p.order = append(p.order, set)
		}
	case typeFileDesc:
		// Volumes repeat the descriptions; keep the first so a File stays
		// the same value across calls to Set.
		f, ok := parseFileDesc(body)
		if s := p.get(set); ok && s.descs[f.ID] == nil {
			s.descs[f.ID] = f
		}
	case typeIFSC:
		if len(body) < 16 || (len(body)-16)%20 != 0 {
			return
		}

		var id ID
		copy(id[:], body[:16])

		var slices []Slice
		for b := body[16:]; len(b) >= 20; b = b[20:] {
			var s Slice
			copy(s.MD5[:], b[:16])
			s.CRC32 = binary.LittleEndian.Uint32(b[16:20])
			slices = append(slices, s)
		}

		p.get(set).slices[id] = slices
	case typeCreator:
		p.get(set).creator = string(bytes.TrimRight(body, "\x00"))
	}
}

func parseFileDesc(body []byte) (*File, bool) {
	if len(body) < 56 {
		return nil, false
	}

	f := &File{}
	copy(f.ID[:], body[0:16])
	copy(f.MD5[:], body[16:32])
	copy(f.MD5_16k[:], body[32:48])

	size := binary.LittleEndian.Uint64(body[48:56])
	if size > 1<<50 {
		return nil, false
	}

	f.Size = int64(size)
	name := bytes.TrimRight(body[56:], "\x00")
	f.Name = string(name)

	// The file id is MD5(16k hash, length, name); check it so a description
	// cannot claim another file's id.
	h := md5.New() //nolint:gosec // see import
	h.Write(f.MD5_16k[:])
	h.Write(body[48:56])
	h.Write(name)

	return f, bytes.Equal(h.Sum(nil), f.ID[:])
}

// Set returns the first complete recovery set read. It can be called again
// after reading more files; the Files it returns stay the same values.
func (p *Parser) Set() (*Set, error) {
	if len(p.order) == 0 {
		return nil, ErrNoRecoverySet
	}

	id := p.order[0]
	s := p.sets[id]

	sliceSize := binary.LittleEndian.Uint64(s.main[0:8])
	count := binary.LittleEndian.Uint32(s.main[8:12])
	ids := s.main[12:]

	if sliceSize == 0 || sliceSize%4 != 0 || sliceSize > maxSlice {
		return nil, fmt.Errorf("par2: invalid slice size %d", sliceSize)
	}

	if count > maxFiles || int(count)*16 > len(ids) {
		return nil, fmt.Errorf("par2: main packet lists %d recovery files", count)
	}

	out := &Set{ID: id, SliceSize: int64(sliceSize), RecoveryExponents: s.exponents, Creator: s.creator}
	inputs := 0

	for i := 0; i+16 <= len(ids); i += 16 {
		var fid ID
		copy(fid[:], ids[i:i+16])

		f, ok := s.descs[fid]
		if !ok {
			return nil, fmt.Errorf("%w: %x", ErrIncomplete, fid[:4])
		}

		f.Slices = s.slices[fid]
		if f.Slices != nil && len(f.Slices) != f.SliceCount(out.SliceSize) {
			f.Slices = nil // inconsistent; treat as absent
		}

		if i/16 < int(count) {
			out.Files = append(out.Files, f)
			inputs += f.SliceCount(out.SliceSize)
		} else {
			out.Unprotected = append(out.Unprotected, f)
		}

		// Checked as we go so a huge declared size cannot overflow the sum.
		if inputs > maxInputSlices {
			return nil, fmt.Errorf("%w: more than %d", ErrTooManySlices, maxInputSlices)
		}
	}

	out.Recovery = recoverySlices(s.recovery, out.SliceSize)

	return out, nil
}

// recoverySlices keeps one usable location per exponent, the first read.
func recoverySlices(all []located, sliceSize int64) []RecoverySlice {
	var out []RecoverySlice

	seen := map[uint32]bool{}
	for _, l := range all {
		if l.length != sliceSize || seen[l.Exponent] {
			continue
		}

		seen[l.Exponent] = true
		out = append(out, l.RecoverySlice)
	}

	slices.SortFunc(out, func(a, b RecoverySlice) int { return cmp.Compare(a.Exponent, b.Exponent) })

	return out
}

var volumeName = regexp.MustCompile(`(?i)\.vol(\d+)\+(\d+)\.par2$`)

// VolumeBlocks returns the number of recovery blocks a volume's file name
// promises, as in "name.vol07+08.par2" (8 blocks).
func VolumeBlocks(name string) (int, bool) {
	m := volumeName.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}

	n, err := strconv.Atoi(m[2])
	if err != nil || n < 0 || n > 1<<16 {
		return 0, false
	}

	return n, true
}

// IsVolume reports whether name is a PAR2 recovery volume rather than an
// index file.
func IsVolume(name string) bool {
	_, ok := VolumeBlocks(name)
	return ok
}
