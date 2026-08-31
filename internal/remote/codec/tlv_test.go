package codec

import (
	"bytes"
	"errors"
	"testing"
)

// tunnel-v2 §4. Every payload in the protocol is a sequence of records
// sharing the frame header's shape: tag uint8, length uint24 BE, value.
// These tests pin the record layer on its own, because everything in §5
// is built out of it and a defect here is a defect in every payload.

func rawRecord(tag uint8, val []byte) []byte {
	n := len(val)
	out := []byte{tag, byte(n >> 16), byte(n >> 8), byte(n)}
	return append(out, val...)
}

func collect(t *testing.T, p []byte) []record {
	t.Helper()
	var got []record
	err := eachRecord(p, func(tag uint8, val []byte) error {
		// Copy without append-to-nil: that would turn a present-and-empty
		// value into a nil one and quietly defeat the very distinction
		// TestZeroLengthRecordIsPresentAndEmpty is checking.
		cp := make([]byte, len(val))
		copy(cp, val)
		got = append(got, record{tag: tag, val: cp})
		return nil
	})
	if err != nil {
		t.Fatalf("eachRecord: %v", err)
	}
	return got
}

func wantMalformedRecords(t *testing.T, p []byte, why string) {
	t.Helper()
	err := eachRecord(p, func(uint8, []byte) error { return nil })
	if err == nil {
		t.Fatalf("%s: accepted, want malformed", why)
	}
	var rej *RejectError
	if !errors.As(err, &rej) || rej.Class != ClassMalformed {
		t.Fatalf("%s: err = %v, want %s", why, err, ClassMalformed)
	}
}

// §3.2: the two header sizes are the same number because they are the
// same shape. A future edit that grows one and not the other would break
// the claim the spec makes, so assert it rather than trusting the prose.
func TestRecordHeaderIsTheFrameHeaderShape(t *testing.T) {
	if recordHeaderSize != frameHeaderSize {
		t.Fatalf("recordHeaderSize = %d, frameHeaderSize = %d; §3.2 says one shape",
			recordHeaderSize, frameHeaderSize)
	}
}

func TestRecordRoundTrip(t *testing.T) {
	b, err := putRecordString(nil, 3, "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	b, err = putRecordU16(b, 2, 206)
	if err != nil {
		t.Fatal(err)
	}
	b, err = putRecordU32(b, 7, 66560)
	if err != nil {
		t.Fatal(err)
	}
	b, err = putRecordU8(b, 9, 1)
	if err != nil {
		t.Fatal(err)
	}

	got := collect(t, b)
	if len(got) != 4 {
		t.Fatalf("got %d records, want 4", len(got))
	}
	if string(got[0].val) != "/api/state" || got[0].tag != 3 {
		t.Errorf("record 0 = %d/%q", got[0].tag, got[0].val)
	}
	if v, err := recordU16(got[1].val); err != nil || v != 206 {
		t.Errorf("record 1 = %v, %v", v, err)
	}
	if v, err := recordU32(got[2].val); err != nil || v != 66560 {
		t.Errorf("record 2 = %v, %v", v, err)
	}
	if v, err := recordU8(got[3].val); err != nil || v != 1 {
		t.Errorf("record 3 = %v, %v", v, err)
	}
}

// The property the whole semver contract rests on (§2.1). A tag this
// build has never heard of is stepped over by its length; the records
// around it decode normally.
func TestUnknownTagIsSkippedWithoutDisturbingTheRest(t *testing.T) {
	var b []byte
	b = append(b, rawRecord(1, []byte("keep-me"))...)
	b = append(b, rawRecord(200, bytes.Repeat([]byte{0xAB}, 300))...)
	b = append(b, rawRecord(2, []byte("also-keep"))...)

	got := collect(t, b)
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3 (the unknown one is still yielded)", len(got))
	}
	if string(got[0].val) != "keep-me" || string(got[2].val) != "also-keep" {
		t.Fatalf("records around the unknown tag decoded wrong: %q / %q", got[0].val, got[2].val)
	}
	if got[1].tag != 200 || len(got[1].val) != 300 {
		t.Fatalf("unknown record = tag %d, %d bytes", got[1].tag, len(got[1].val))
	}
}

// §4: tag 0x00 is reserved in every record namespace and is the one
// rejection the record layer is allowed to make. It costs no
// extensibility because no future minor can assign it.
func TestRecordTagZeroIsMalformed(t *testing.T) {
	wantMalformedRecords(t, rawRecord(0, []byte("x")), "tag 0")
	wantMalformedRecords(t, rawRecord(0, nil), "tag 0, empty value")
	// And it is rejected even when it follows a perfectly good record,
	// so a decoder cannot get away with only checking the first one.
	wantMalformedRecords(t, append(rawRecord(1, []byte("ok")), rawRecord(0, nil)...), "tag 0 after tag 1")
}

func TestRecordEncoderRefusesTagZero(t *testing.T) {
	if _, err := putRecord(nil, 0, []byte("x")); err == nil {
		t.Fatal("putRecord accepted tag 0")
	}
}

// §4: a repeated tag is a repeated field. There is no separate list
// encoding, which is why header maps no longer need a count.
func TestRepeatedTagIsRepeatedNotOverwritten(t *testing.T) {
	var b []byte
	for _, v := range []string{"a", "b", "c"} {
		b = append(b, rawRecord(2, []byte(v))...)
	}
	got := collect(t, b)
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if string(got[i].val) != want {
			t.Errorf("record %d = %q, want %q", i, got[i].val, want)
		}
	}
}

// §4: a zero-length record is a present, empty value — not an absent
// field. The distinction matters for query, which is legitimately empty.
func TestZeroLengthRecordIsPresentAndEmpty(t *testing.T) {
	got := collect(t, rawRecord(4, nil))
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if got[0].val == nil {
		t.Fatal("a zero-length record decoded as absent; §4 says present and empty")
	}
	if len(got[0].val) != 0 {
		t.Fatalf("value = %q, want empty", got[0].val)
	}
}

// Tolerance is for whole records this version does not know, never for
// truncation. A decoder that treated a short tail as "trailing bytes to
// ignore" would silently accept a cut-off frame.
func TestTruncationIsMalformedNotTolerated(t *testing.T) {
	full := rawRecord(1, []byte("0123456789"))

	wantMalformedRecords(t, full[:len(full)-1], "value one byte short")
	wantMalformedRecords(t, full[:recordHeaderSize], "header with no value")
	for n := 1; n < recordHeaderSize; n++ {
		wantMalformedRecords(t, full[:n], "partial record header")
	}
	// A record that claims more than the enclosing value holds.
	wantMalformedRecords(t, []byte{1, 0xff, 0xff, 0xff, 'x'}, "length beyond the value")
}

// §4: nesting is free — a value may itself be records, and nothing about
// the format changes at depth. This is what §5.7 header records are.
func TestRecordsNest(t *testing.T) {
	inner, err := putRecordString(nil, 1, "Content-Type")
	if err != nil {
		t.Fatal(err)
	}
	inner, err = putRecordString(inner, 2, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	outer, err := putRecord(nil, 5, inner)
	if err != nil {
		t.Fatal(err)
	}

	got := collect(t, outer)
	if len(got) != 1 || got[0].tag != 5 {
		t.Fatalf("outer = %#v", got)
	}
	nested := collect(t, got[0].val)
	if len(nested) != 2 {
		t.Fatalf("nested = %d records, want 2", len(nested))
	}
	if string(nested[0].val) != "Content-Type" || string(nested[1].val) != "application/json" {
		t.Fatalf("nested = %q / %q", nested[0].val, nested[1].val)
	}
}

// §4: a fixed-width numeric record whose length is not exactly the width
// is malformed. Without this a peer could shorten a status to one byte
// and have it read as something else entirely.
func TestFixedWidthRecordsRejectTheWrongLength(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func([]byte) error
		ok   int
	}{
		{"u8", func(v []byte) error { _, err := recordU8(v); return err }, 1},
		{"u16", func(v []byte) error { _, err := recordU16(v); return err }, 2},
		{"u32", func(v []byte) error { _, err := recordU32(v); return err }, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for n := 0; n <= 8; n++ {
				err := tc.fn(make([]byte, n))
				if n == tc.ok {
					if err != nil {
						t.Errorf("length %d rejected: %v", n, err)
					}
					continue
				}
				if err == nil {
					t.Errorf("length %d accepted, want malformed", n)
				}
			}
		})
	}
}

// There is no count prefix anywhere in this protocol (§4). A claimed
// count is a number a hostile peer picks and a decoder is tempted to
// allocate from; reading to exhaustion cannot be lied to. Assert the
// absence rather than trusting that nobody reintroduces one: 64 KiB of
// header records must not be preceded by any length that is not a
// record's own.
func TestHeaderRecordsCarryNoCount(t *testing.T) {
	var inner []byte
	var err error
	inner, err = putRecordString(inner, tagHeaderName, "Content-Type")
	if err != nil {
		t.Fatal(err)
	}
	inner, err = putRecordString(inner, tagHeaderValue, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	one, err := putRecord(nil, tagHeader, inner)
	if err != nil {
		t.Fatal(err)
	}
	// The encoded header begins immediately with the header record's own
	// tag. Anything else in front of it is a count.
	if one[0] != tagHeader {
		t.Fatalf("encoded header starts with 0x%02x, not the header tag 0x%02x", one[0], tagHeader)
	}
}
