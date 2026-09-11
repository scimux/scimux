package muse

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewCommandIDVersionAndVariant(t *testing.T) {
	id, err := NewCommandID()
	if err != nil {
		t.Fatalf("NewCommandID: %v", err)
	}
	assertUUIDv7(t, id)
}

func TestNewCommandIDCanonicalLowercase(t *testing.T) {
	id, err := NewCommandID()
	if err != nil {
		t.Fatalf("NewCommandID: %v", err)
	}
	if id != strings.ToLower(id) {
		t.Fatalf("id is not lowercase: %q", id)
	}
	if !uuidRE.MatchString(id) {
		t.Fatalf("id %q is not canonical UUIDv7 text", id)
	}
}

func TestSameMillisecondMonotonic(t *testing.T) {
	var ms int64 = 1_700_000_000_000
	g := newUUIDGen(func() int64 { return ms }, bytes.NewReader(bytes.Repeat([]byte{0x11}, 1024)))
	var prev string
	for i := 0; i < 64; i++ {
		id, err := g.Next()
		if err != nil {
			t.Fatalf("Next #%d: %v", i, err)
		}
		assertUUIDv7(t, id)
		if prev != "" && id <= prev {
			t.Fatalf("id #%d %q is not greater than previous %q", i, id, prev)
		}
		gotMS, seq := uuidTimestampAndSeq(t, id)
		if gotMS != uint64(ms) {
			t.Fatalf("id #%d timestamp %d, want %d", i, gotMS, ms)
		}
		if seq != uint16(i) {
			t.Fatalf("id #%d seq %d, want %d", i, seq, i)
		}
		prev = id
	}
}

func TestClockRollbackKeepsLogicalTime(t *testing.T) {
	var wall int64 = 5_000
	g := newUUIDGen(func() int64 { return wall }, io.LimitReader(zeroReader{}, 1<<20))
	first, err := g.Next()
	if err != nil {
		t.Fatal(err)
	}
	wall = 1 // wall clock jumps backwards
	second, err := g.Next()
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Fatalf("rollback produced non-increasing ids: %q then %q", first, second)
	}
	ms1, seq1 := uuidTimestampAndSeq(t, first)
	ms2, seq2 := uuidTimestampAndSeq(t, second)
	if ms1 != 5_000 || ms2 != 5_000 {
		t.Fatalf("logical ms drifted on rollback: %d/%d", ms1, ms2)
	}
	if seq2 != seq1+1 {
		t.Fatalf("seq after rollback = %d, want %d", seq2, seq1+1)
	}
}

func TestCounterOverflowAdvancesMillisecond(t *testing.T) {
	const ms int64 = 42
	g := newUUIDGen(func() int64 { return ms }, io.LimitReader(zeroReader{}, 1<<20))
	var last string
	for i := 0; i < 4096; i++ {
		id, err := g.Next()
		if err != nil {
			t.Fatalf("Next #%d: %v", i, err)
		}
		gotMS, seq := uuidTimestampAndSeq(t, id)
		if gotMS != uint64(ms) {
			t.Fatalf("id #%d timestamp %d, want %d", i, gotMS, ms)
		}
		if seq != uint16(i) {
			t.Fatalf("id #%d seq %d, want %d", i, seq, i)
		}
		if last != "" && id <= last {
			t.Fatalf("id #%d %q is not greater than %q", i, id, last)
		}
		last = id
	}
	overflow, err := g.Next()
	if err != nil {
		t.Fatal(err)
	}
	gotMS, seq := uuidTimestampAndSeq(t, overflow)
	if gotMS != uint64(ms)+1 {
		t.Fatalf("overflow timestamp %d, want %d", gotMS, ms+1)
	}
	if seq != 0 {
		t.Fatalf("overflow seq %d, want 0", seq)
	}
	if overflow <= last {
		t.Fatalf("overflow id %q is not greater than last same-ms id %q", overflow, last)
	}
}

func TestDeterministicEntropy(t *testing.T) {
	src := &seqReader{}
	g := newUUIDGen(func() int64 { return 0x010203040506 }, src)
	id, err := g.Next()
	if err != nil {
		t.Fatal(err)
	}
	raw := parseUUID(t, id)
	// rand_b occupies the low 62 bits of the last 8 bytes (variant occupies the top 2).
	if raw[8]&0xc0 != 0x80 {
		t.Fatalf("variant bits = %02x", raw[8])
	}
	// The remaining 6 bits of byte 8 plus bytes 9-15 come from entropy.
	if raw[8]&0x3f != 0x00 {
		t.Fatalf("first entropy nibble unexpected: %02x", raw[8])
	}
	want := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}
	if !bytes.Equal(raw[9:], want) {
		t.Fatalf("rand_b bytes = %x, want %x", raw[9:], want)
	}
	id2, err := g.Next()
	if err != nil {
		t.Fatal(err)
	}
	raw2 := parseUUID(t, id2)
	if bytes.Equal(raw[8:], raw2[8:]) {
		t.Fatal("second id reused the same entropy bytes")
	}
}

func TestEntropyReadFailure(t *testing.T) {
	boom := errors.New("no entropy")
	g := newUUIDGen(func() int64 { return 1 }, failReader{err: boom})
	id, err := g.Next()
	if err == nil {
		t.Fatalf("Next succeeded with failed entropy, id=%q", id)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapping %v", err, boom)
	}
	if id != "" {
		t.Fatalf("failed Next returned id %q", id)
	}
	// A later successful read must still produce a well-formed id; the
	// failed attempt must not have advanced into a half-built value.
	g.rand = bytes.NewReader(bytes.Repeat([]byte{0xaa}, 16))
	id, err = g.Next()
	if err != nil {
		t.Fatalf("Next after entropy recovery: %v", err)
	}
	assertUUIDv7(t, id)
}

func TestNoDuplicatesInSubstantialSample(t *testing.T) {
	seen := make(map[string]struct{}, 8192)
	for i := 0; i < 8192; i++ {
		id, err := NewCommandID()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("duplicate id %q at i=%d", id, i)
		}
		seen[id] = struct{}{}
	}
}

func TestConcurrentGeneration(t *testing.T) {
	const n = 256
	const each = 32
	var (
		mu   sync.Mutex
		seen = make(map[string]struct{}, n*each)
		wg   sync.WaitGroup
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				id, err := NewCommandID()
				if err != nil {
					t.Errorf("NewCommandID: %v", err)
					return
				}
				mu.Lock()
				if _, ok := seen[id]; ok {
					t.Errorf("duplicate concurrent id %q", id)
				}
				seen[id] = struct{}{}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != n*each {
		t.Fatalf("got %d unique ids, want %d", len(seen), n*each)
	}
}

func TestUUIDTimestampLayout(t *testing.T) {
	const ms int64 = 0x0000_aabb_ccdd_eeff & 0xffffffffffff
	g := newUUIDGen(func() int64 { return ms }, io.LimitReader(zeroReader{}, 16))
	id, err := g.Next()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := uuidTimestampAndSeq(t, id)
	if got != uint64(ms) {
		t.Fatalf("timestamp bits = %d, want %d", got, ms)
	}
}

func assertUUIDv7(t *testing.T, id string) {
	t.Helper()
	if !uuidRE.MatchString(id) {
		t.Fatalf("id %q is not canonical lowercase UUIDv7", id)
	}
	raw := parseUUID(t, id)
	if ver := raw[6] >> 4; ver != 7 {
		t.Fatalf("version = %d, want 7", ver)
	}
	if v := raw[8] >> 6; v != 0b10 {
		t.Fatalf("variant = %02b, want 10", v)
	}
}

func uuidTimestampAndSeq(t *testing.T, id string) (uint64, uint16) {
	t.Helper()
	raw := parseUUID(t, id)
	var buf [8]byte
	copy(buf[2:], raw[0:6])
	ms := binary.BigEndian.Uint64(buf[:])
	seq := uint16(raw[6]&0x0f)<<8 | uint16(raw[7])
	return ms, seq
}

func parseUUID(t *testing.T, id string) []byte {
	t.Helper()
	hexes := strings.ReplaceAll(id, "-", "")
	raw, err := hex.DecodeString(hexes)
	if err != nil {
		t.Fatalf("decode %q: %v", id, err)
	}
	if len(raw) != 16 {
		t.Fatalf("decoded %d bytes, want 16", len(raw))
	}
	return raw
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

type seqReader struct{ n byte }

func (s *seqReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = s.n
		s.n++
	}
	return len(p), nil
}

type failReader struct{ err error }

func (f failReader) Read([]byte) (int, error) { return 0, f.err }
