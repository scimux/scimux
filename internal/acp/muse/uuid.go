package muse

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"
)

// Command IDs are client-minted RFC 9562 UUIDv7 values. Item IDs arriving
// from the server remain opaque strings and are never validated as UUIDs.

// NewCommandID returns a canonical lowercase UUIDv7 command identifier.
func NewCommandID() (string, error) {
	return defaultUUID.Next()
}

var defaultUUID = newUUIDGen(func() int64 { return time.Now().UnixMilli() }, cryptorand.Reader)

type uuidGen struct {
	now  func() int64
	rand io.Reader

	mu     sync.Mutex
	lastMS uint64
	seq    uint16
	have   bool
}

func newUUIDGen(now func() int64, r io.Reader) *uuidGen {
	return &uuidGen{now: now, rand: r}
}

func (g *uuidGen) Next() (string, error) {
	var entropy [8]byte
	if _, err := io.ReadFull(g.rand, entropy[:]); err != nil {
		return "", fmt.Errorf("uuid entropy: %w", err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	if now < 0 {
		now = 0
	}
	ms := uint64(now) & 0xffffffffffff

	switch {
	case !g.have || ms > g.lastMS:
		g.lastMS = ms
		g.seq = 0
		g.have = true
	case g.seq == 0x0fff:
		// Advance the logical millisecond rather than wrapping the 12-bit
		// counter into a duplicate or lower identifier.
		g.lastMS++
		g.seq = 0
	default:
		g.seq++
	}

	var raw [16]byte
	binary.BigEndian.PutUint64(raw[0:8], (g.lastMS&0xffffffffffff)<<16)
	raw[6] = 0x70 | byte(g.seq>>8)
	raw[7] = byte(g.seq)
	copy(raw[8:], entropy[:])
	raw[8] = raw[8]&0x3f | 0x80
	return formatUUID(raw), nil
}

func formatUUID(raw [16]byte) string {
	var buf [36]byte
	hex.Encode(buf[0:8], raw[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], raw[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], raw[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], raw[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], raw[10:16])
	return string(buf[:])
}
