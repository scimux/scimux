package sessionlog

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	logUID     = "0123456789abcdef"
	logOther   = "fedcba9876543210"
	logThird   = "0011223344556677"
	userRecord = `{"t":"user","time":"2026-01-02T03:04:06Z","text":"synthetic question"}`
)

func TestReadLogIdentityHeaderFirst(t *testing.T) {
	path := writeIdentityLog(t, metaRecord(logUID), userRecord)
	id := ReadLogIdentity(path)
	if id.Status != IdentityOK || id.UID != logUID || id.Record != 0 {
		t.Fatalf("%+v", id)
	}
}

func TestReadLogIdentityHeaderAfterAttention(t *testing.T) {
	path := writeIdentityLog(t, attentionRecord("start"), attentionRecord("end"), metaRecord(logUID), userRecord)
	id := ReadLogIdentity(path)
	if id.Status != IdentityOK || id.UID != logUID || id.Record != 2 {
		t.Fatalf("%+v", id)
	}
}

func TestReadLogIdentityAgreesWithExistingReaders(t *testing.T) {
	cases := []struct {
		name   string
		lines  []string
		uid    string
		record int
	}{
		{"header first", []string{metaRecord(logUID), userRecord}, logUID, 0},
		{"header after attention", []string{attentionRecord("start"), attentionRecord("end"), metaRecord(logOther), userRecord}, logOther, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeIdentityLog(t, tc.lines...)
			id := ReadLogIdentity(path)
			if id.Status != IdentityOK || id.UID != tc.uid || id.Record != tc.record {
				t.Fatalf("%+v", id)
			}
			scanned := ScanLog(path, "synthetic", ScanOptions{})
			if scanned.UID != id.UID {
				t.Fatalf("ScanLog UID = %q, ReadLogIdentity UID = %q", scanned.UID, id.UID)
			}
			seg := ReadSegment(path)
			if len(seg.Turns) != 1 || seg.Turns[0].UID != id.UID {
				t.Fatalf("ReadSegment turns = %+v", seg.Turns)
			}
		})
	}
}

func TestReadLogIdentityNoHeader(t *testing.T) {
	path := writeIdentityLog(t, userRecord)
	id := ReadLogIdentity(path)
	if id.Status != IdentityNone || id.UID != "" || id.Record != -1 {
		t.Fatalf("%+v", id)
	}
}

func TestReadLogIdentityMissingAndDirectory(t *testing.T) {
	missing := ReadLogIdentity(filepath.Join(t.TempDir(), "missing.jsonl"))
	if missing.Status != IdentityNone || missing.UID != "" || missing.Record != -1 {
		t.Fatalf("missing %+v", missing)
	}
	dir := ReadLogIdentity(t.TempDir())
	if dir.Status != IdentityNone || dir.UID != "" || dir.Record != -1 {
		t.Fatalf("directory %+v", dir)
	}
}

func TestReadLogIdentityEmptyAndWhitespace(t *testing.T) {
	for _, body := range []string{"", " \n\t\n \r\n"} {
		path := filepath.Join(t.TempDir(), "session.jsonl")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		id := ReadLogIdentity(path)
		if id.Status != IdentityNone || id.UID != "" || id.Record != -1 {
			t.Fatalf("body %q -> %+v", body, id)
		}
	}
}

func TestReadLogIdentityRepeatedHeader(t *testing.T) {
	path := writeIdentityLog(t, metaRecord(logUID), userRecord, metaRecord(logUID))
	id := ReadLogIdentity(path)
	if id.Status != IdentityOK || id.UID != logUID || id.Record != 0 {
		t.Fatalf("%+v", id)
	}
}

func TestReadLogIdentityConflict(t *testing.T) {
	path := writeIdentityLog(t, userRecord, metaRecord(logUID), metaRecord(logOther), metaRecord(logThird))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	id := ReadLogIdentity(path)
	if id.Status != IdentityConflict || id.UID != "" || id.Record != 1 {
		t.Fatalf("%+v", id)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("reader changed the log")
	}
}

func TestReadLogIdentityEmptyUIDThenReal(t *testing.T) {
	empty := `{"t":"meta","time":"2026-01-02T03:04:05Z","meta":{"node":"n1","uid":"","agent":"claude","created":"2026-01-02T03:04:05Z"}}`
	path := writeIdentityLog(t, empty, metaRecord(logUID))
	id := ReadLogIdentity(path)
	if id.Status != IdentityOK || id.UID != logUID || id.Record != 1 {
		t.Fatalf("%+v", id)
	}
}

func TestReadLogIdentityNullMeta(t *testing.T) {
	nullMeta := `{"t":"meta","time":"2026-01-02T03:04:05Z","meta":null}`
	t.Run("alone", func(t *testing.T) {
		id := ReadLogIdentity(writeIdentityLog(t, nullMeta))
		if id.Status != IdentityNone || id.UID != "" || id.Record != -1 {
			t.Fatalf("%+v", id)
		}
	})
	t.Run("then a header", func(t *testing.T) {
		id := ReadLogIdentity(writeIdentityLog(t, nullMeta, metaRecord(logUID)))
		if id.Status != IdentityOK || id.UID != logUID || id.Record != 1 {
			t.Fatalf("%+v", id)
		}
	})
}

func TestReadLogIdentitySkipsMalformedLine(t *testing.T) {
	path := writeIdentityLog(t, "{not-json", metaRecord(logUID))
	id := ReadLogIdentity(path)
	if id.Status != IdentityOK || id.UID != logUID || id.Record != 0 {
		t.Fatalf("%+v", id)
	}
}

func TestReadLogIdentityIgnoresUnterminatedConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	body := metaRecord(logUID) + "\n" + metaRecord(logOther)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	id := ReadLogIdentity(path)
	if id.Status != IdentityOK || id.UID != logUID || id.Record != 0 {
		t.Fatalf("%+v", id)
	}
}

func TestReadLogIdentityCRLF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	body := strings.Join([]string{metaRecord(logUID), userRecord}, "\r\n") + "\r\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	id := ReadLogIdentity(path)
	if id.Status != IdentityOK || id.UID != logUID || id.Record != 0 {
		t.Fatalf("%+v", id)
	}
}

func TestReadLogIdentityReadErrorAfterHeaderIsNone(t *testing.T) {
	prev := openIdentityLog
	t.Cleanup(func() { openIdentityLog = prev })
	body := metaRecord(logUID) + "\n" + metaRecord(logOther) + "\n"
	openIdentityLog = func(string) (io.ReadCloser, error) {
		return io.NopCloser(&prefixThenError{data: []byte(body), err: errors.New("disk read failed")}), nil
	}
	id := ReadLogIdentity(filepath.Join(t.TempDir(), "unused.jsonl"))
	if id.Status != IdentityNone || id.UID != "" || id.Record != -1 {
		t.Fatalf("%+v", id)
	}
}

func metaRecord(uid string) string {
	return `{"t":"meta","time":"2026-01-02T03:04:05Z","meta":{"node":"n1","uid":"` + uid + `","agent":"claude","created":"2026-01-02T03:04:05Z"}}`
}

func attentionRecord(status string) string {
	return `{"t":"attention","time":"2026-01-02T03:04:05Z","attention":{"kind":"question","status":"` + status + `"}}`
}

func writeIdentityLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type prefixThenError struct {
	data []byte
	err  error
}

func (r *prefixThenError) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}
