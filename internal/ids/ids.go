// Package ids generates identifiers for protocol messages and outbox rows.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// NewV7 returns an RFC 9562 UUIDv7 string. The 48-bit millisecond
// timestamp prefix makes ids sort by creation time, which keeps outbox and
// log ordering readable in the cloud.
func NewV7() string {
	var b [16]byte
	_, _ = rand.Read(b[:])

	ms := uint64(time.Now().UnixMilli())
	binary.BigEndian.PutUint16(b[0:2], uint16(ms>>32))
	binary.BigEndian.PutUint32(b[2:6], uint32(ms))
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 9562 variant

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])

	return string(out[:])
}
