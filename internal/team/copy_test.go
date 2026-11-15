package team

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/scimux/scimux/internal/storagebudget"
)

func sampleCopyID() CopyID {
	return CopyID{
		Server:     fmt.Sprintf("%032x", 1),
		Owner:      fmt.Sprintf("%032x", 2),
		Chat:       fmt.Sprintf("%016x", 3),
		Generation: fmt.Sprintf("%016x", 4),
	}
}

func copyRec(seq uint64, body string) MirrorRecord {
	if body == "" {
		body = `{"v":1}`
	}
	return MirrorRecord{
		Seq:    seq,
		Key:    fmt.Sprintf("k-%d", seq),
		Author: fmt.Sprintf("%032x", 9),
		At:     "2026-11-14T09:30:00.000Z",
		Body:   []byte(body),
	}
}

func copyPathOf(dir string, id CopyID) string {
	return filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, mirrorFile)
}

func snapshotCopy(t *testing.T) {
	t.Helper()
	ops := copyOps
	t.Cleanup(func() { copyOps = ops })
}

func readCopyFile(t *testing.T, dir string, id CopyID) []byte {
	t.Helper()
	raw, err := os.ReadFile(copyPathOf(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCopyAppendsInOrderAndSkipsARepeat(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	first := copyRec(1, `{"v":1,"text":"North <relay>"}`)
	second := copyRec(2, `{"v":1}`)
	if err := AppendCopy(dir, id, []MirrorRecord{first, second}); err != nil {
		t.Fatal(err)
	}
	got, last, err := ReadCopy(dir, id, 0, 256, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if last != 2 || len(got) != 2 || got[0].Seq != 1 || string(got[0].Body) != string(first.Body) || got[1].Seq != 2 {
		t.Fatalf("read = %+v last = %d err = %v", got, last, err)
	}
	raw := readCopyFile(t, dir, id)
	if !bytes.Contains(raw, []byte(`"text":"North <relay>"`)) || bytes.Contains(raw, []byte(`\u003c`)) {
		t.Fatal("body was rewritten")
	}
	if !bytes.Contains(raw, []byte(`"key":"k-1"`)) || !bytes.Contains(raw, []byte(first.Author)) || !bytes.Contains(raw, []byte(first.At)) {
		t.Fatal("line dropped a field")
	}
	if bytes.Contains(raw, []byte("token")) {
		t.Fatal("file holds a token")
	}
	info, err := os.Stat(copyPathOf(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o", info.Mode().Perm())
	}
	for p := filepath.Dir(copyPathOf(dir, id)); ; p = filepath.Dir(p) {
		di, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Fatalf("dir %s mode = %o", p, di.Mode().Perm())
		}
		if filepath.Base(p) == CopiesDir {
			break
		}
	}
	before := append([]byte(nil), raw...)
	if err := AppendCopy(dir, id, []MirrorRecord{first, second}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, readCopyFile(t, dir, id)) {
		t.Fatal("repeat append stored a second copy")
	}
	fourth := copyRec(3, "null")
	overlap := []MirrorRecord{second, fourth}
	if err := AppendCopy(dir, id, overlap); err != nil {
		t.Fatal(err)
	}
	got, last, err = ReadCopy(dir, id, 1, 256, 4<<20)
	if err != nil || last != 3 || len(got) != 2 || got[0].Seq != 2 || got[1].Seq != 3 || string(got[1].Body) != "null" {
		t.Fatalf("after overlap = %+v last = %d err = %v", got, last, err)
	}
	if !bytes.HasPrefix(readCopyFile(t, dir, id), before) {
		t.Fatal("overlap append rewrote earlier lines")
	}
	none, last, err := ReadCopy(dir, id, 3, 256, 4<<20)
	if err != nil || last != 3 || len(none) != 0 {
		t.Fatalf("after last = %+v last = %d err = %v", none, last, err)
	}
	none, last, err = ReadCopy(dir, id, 9, 256, 4<<20)
	if err != nil || last != 3 || len(none) != 0 {
		t.Fatalf("past last = %+v last = %d err = %v", none, last, err)
	}
}

func TestCopyNilAppendCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := AppendCopy(dir, sampleCopyID(), nil); err != nil {
		t.Fatal(err)
	}
	if err := AppendCopy(dir, sampleCopyID(), []MirrorRecord{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, CopiesDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v", err)
	}
	got, last, err := ReadCopy(dir, sampleCopyID(), 0, 256, 4<<20)
	if err != nil || last != 0 || len(got) != 0 {
		t.Fatalf("read = %+v last = %d err = %v", got, last, err)
	}
}

func TestCopyGapStoresNothing(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(2, "")}); !errors.Is(err, ErrCopyRange) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(copyPathOf(dir, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v", err)
	}
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, ""), copyRec(3, "")}); !errors.Is(err, ErrCopyRange) {
		t.Fatalf("batch error = %v", err)
	}
	if _, err := os.Stat(copyPathOf(dir, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("batch stat = %v", err)
	}
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")}); err != nil {
		t.Fatal(err)
	}
	before := readCopyFile(t, dir, id)
	bad := [][]MirrorRecord{
		{copyRec(3, "")},
		{copyRec(1, ""), copyRec(3, "")},
		{copyRec(0, "")},
		{{Seq: 2, Key: "k-2", Author: copyRec(2, "").Author, At: copyRec(2, "").At, Body: []byte("{")}},
		{{Seq: 2, Key: "k-2", Author: copyRec(2, "").Author, At: copyRec(2, "").At}},
	}
	for i, entries := range bad {
		if err := AppendCopy(dir, id, entries); !errors.Is(err, ErrCopyRange) {
			t.Fatalf("case %d error = %v", i, err)
		}
		if !bytes.Equal(before, readCopyFile(t, dir, id)) {
			t.Fatalf("case %d stored bytes", i)
		}
	}
	if strings.Contains(ErrCopyRange.Error(), "North") {
		t.Fatal("range error quotes text")
	}
}

func TestCopyDamageIsLeftUntouched(t *testing.T) {
	secret := strings.Repeat("ab", 32)
	author := fmt.Sprintf("%032x", 9)
	good := `{"seq":1,"key":"k-1","author":"` + author + `","at":"2026-11-14T09:30:00.000Z","body":{"v":1}}`
	cases := []struct {
		name string
		body string
	}{
		{"unfinished", good},
		{"not json", "nope\n"},
		{"empty line", "\n"},
		{"unknown field", `{"seq":1,"key":"k-1","author":"` + author + `","at":"2026-11-14T09:30:00.000Z","body":{"v":1},"token":"` + secret + `"}\n`},
		{"seq string", `{"seq":"1","key":"k-1","author":"` + author + `","at":"2026-11-14T09:30:00.000Z","body":{"v":1}}\n`},
		{"negative seq", `{"seq":-1,"key":"k-1","author":"` + author + `","at":"2026-11-14T09:30:00.000Z","body":{"v":1}}\n`},
		{"missing seq", `{"key":"k-1","author":"` + author + `","at":"2026-11-14T09:30:00.000Z","body":{"v":1}}\n`},
		{"seq zero", `{"seq":0,"key":"k-1","author":"` + author + `","at":"2026-11-14T09:30:00.000Z","body":{"v":1}}\n`},
		{"bad key", `{"seq":1,"key":"","author":"` + author + `","at":"2026-11-14T09:30:00.000Z","body":{"v":1}}\n`},
		{"bad author", `{"seq":1,"key":"k-1","author":"abcd","at":"2026-11-14T09:30:00.000Z","body":{"v":1}}\n`},
		{"bad time", `{"seq":1,"key":"k-1","author":"` + author + `","at":"yesterday","body":{"v":1}}\n`},
		{"missing body", `{"seq":1,"key":"k-1","author":"` + author + `","at":"2026-11-14T09:30:00.000Z"}\n`},
		{"bad body", `{"seq":1,"key":"k-1","author":"` + author + `","at":"2026-11-14T09:30:00.000Z","body":}\n`},
		{"bad utf-8", "{\"seq\":1,\"key\":\"bad\xff\"}\n"},
		{"seq two", strings.Replace(good, `"seq":1`, `"seq":2`, 1) + "\n"},
		{"gap", good + "\n" + strings.Replace(good, `"seq":1`, `"seq":3`, 1) + "\n"},
		{"duplicate", good + "\n" + good + "\n"},
		{"second bad", good + "\nnope\n"},
		{"cut second", good + "\n{\"seq\":2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.HasSuffix(tc.body, `\n`) {
				tc.body = strings.TrimSuffix(tc.body, `\n`) + "\n"
			}
			dir := t.TempDir()
			id := sampleCopyID()
			path := copyPathOf(dir, id)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			before := []byte(tc.body)
			_, _, err := ReadCopy(dir, id, 0, 256, 4<<20)
			if !errors.Is(err, ErrCopyDamaged) {
				t.Fatalf("Read = %v", err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "North") {
				t.Fatalf("Read quotes the copy: %v", err)
			}
			_, _, err = ReadCopy(dir, id, 0, 256, 4<<20)
			if !errors.Is(err, ErrCopyDamaged) {
				t.Fatalf("second Read = %v", err)
			}
			if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")}); !errors.Is(err, ErrCopyDamaged) {
				t.Fatalf("Append = %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("damaged copy was rewritten")
			}
		})
	}
}

func TestCopyEmptyFileIsAnEmptyCopy(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	path := copyPathOf(dir, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, last, err := ReadCopy(dir, id, 0, 256, 4<<20)
	if err != nil || last != 0 || len(got) != 0 {
		t.Fatalf("read = %+v last = %d err = %v", got, last, err)
	}
	got, last, err = ReadCopy(dir, id, 0, 256, 4<<20)
	if err != nil || last != 0 || len(got) != 0 {
		t.Fatalf("second = %+v last = %d err = %v", got, last, err)
	}
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")}); err != nil {
		t.Fatal(err)
	}
	got, last, err = ReadCopy(dir, id, 0, 256, 4<<20)
	if err != nil || last != 1 || len(got) != 1 {
		t.Fatalf("after append = %+v last = %d err = %v", got, last, err)
	}
}

func TestCopyReadNearTheEndDoesNotReadTheWholeFile(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	const n = 10000
	entries := make([]MirrorRecord, n)
	for i := range entries {
		entries[i] = copyRec(uint64(i+1), `{"v":1}`)
	}
	snapshotCopy(t)
	var readBytes atomic.Int64
	real := copyOps.readAt
	copyOps.readAt = func(f *os.File, p []byte, off int64) (int, error) {
		nn, err := real(f, p, off)
		readBytes.Add(int64(nn))
		return nn, err
	}
	if err := AppendCopy(dir, id, entries); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(copyPathOf(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	readBytes.Store(0)
	const after = 9900
	got, last, err := ReadCopy(dir, id, after, 256, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if last != n || len(got) != n-after || got[0].Seq != after+1 || got[len(got)-1].Seq != n {
		t.Fatalf("len = %d first = %d last = %d err = %v", len(got), got[0].Seq, last, err)
	}
	if readBytes.Load() == 0 || readBytes.Load() >= info.Size()/2 {
		t.Fatalf("read %d bytes of a %d byte copy", readBytes.Load(), info.Size())
	}
}

func TestCopyReadScansAFileItDidNotWrite(t *testing.T) {
	src := t.TempDir()
	id := sampleCopyID()
	if err := AppendCopy(src, id, []MirrorRecord{copyRec(1, `{"v":1}`), copyRec(2, `{"v":1}`)}); err != nil {
		t.Fatal(err)
	}
	raw := readCopyFile(t, src, id)
	dst := t.TempDir()
	path := copyPathOf(dst, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, last, err := ReadCopy(dst, id, 1, 256, 4<<20)
	if err != nil || last != 2 || len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("read = %+v last = %d err = %v", got, last, err)
	}
}

func TestCopyRefusesAReservation(t *testing.T) {
	dir := t.TempDir()
	body := `{"storage_min_free_bytes":0,"storage_global_limit_bytes":1}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	err := AppendCopy(dir, sampleCopyID(), []MirrorRecord{copyRec(1, `{"text":"North <relay>"}`)})
	if !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "North") || strings.Contains(err.Error(), "<relay>") {
		t.Fatalf("error quotes the body: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, CopiesDir)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stat = %v", statErr)
	}
}

func TestCopyRejectsABadPath(t *testing.T) {
	id := sampleCopyID()
	cases := []CopyID{
		{},
		{Server: "abcd", Owner: id.Owner, Chat: id.Chat, Generation: id.Generation},
		{Server: strings.ToUpper("a" + id.Server[1:]), Owner: id.Owner, Chat: id.Chat, Generation: id.Generation},
		{Server: id.Server, Owner: "abcd", Chat: id.Chat, Generation: id.Generation},
		{Server: id.Server, Owner: id.Owner, Chat: "main", Generation: id.Generation},
		{Server: id.Server, Owner: id.Owner, Chat: strings.ToUpper("a" + id.Chat[1:]), Generation: id.Generation},
		{Server: id.Server, Owner: id.Owner, Chat: id.Chat, Generation: "abcd"},
		{Server: id.Server + "/x", Owner: id.Owner, Chat: id.Chat, Generation: id.Generation},
	}
	for _, id := range cases {
		dir := t.TempDir()
		if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")}); !errors.Is(err, ErrCopyPath) {
			t.Fatalf("Append %v error = %v", id, err)
		}
		if _, _, err := ReadCopy(dir, id, 0, 256, 4<<20); !errors.Is(err, ErrCopyPath) {
			t.Fatalf("Read %v error = %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(dir, CopiesDir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat = %v", err)
		}
	}
	missing := filepath.Join(t.TempDir(), "missing")
	bad := sampleCopyID()
	bad.Chat = "main"
	if err := AppendCopy(missing, bad, []MirrorRecord{copyRec(1, "")}); !errors.Is(err, ErrCopyPath) {
		t.Fatalf("missing dir error = %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v", err)
	}
}

func TestCopySameLengthEditIsDamaged(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, `{"v":1}`), copyRec(2, `{"v":1}`)}); err != nil {
		t.Fatal(err)
	}
	path := copyPathOf(dir, id)
	raw := readCopyFile(t, dir, id)
	// Flip one author nibble so the line stays the same length and stops being hex.
	edited := bytes.Replace(raw, []byte(fmt.Sprintf("%032x", 9)), []byte(strings.Repeat("A", 32)), 1)
	if len(edited) != len(raw) {
		t.Fatal("edit changed the length")
	}
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := ReadCopy(dir, id, 0, 256, 4<<20)
	if !errors.Is(err, ErrCopyDamaged) {
		t.Fatalf("Read = %v", err)
	}
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(3, "")}); !errors.Is(err, ErrCopyDamaged) {
		t.Fatalf("Append = %v", err)
	}
	if !bytes.Equal(edited, readCopyFile(t, dir, id)) {
		t.Fatal("edited copy was rewritten")
	}
}

func TestCopyTamperRescans(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")}); err != nil {
		t.Fatal(err)
	}
	path := copyPathOf(dir, id)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before := readCopyFile(t, dir, id)
	_, _, err = ReadCopy(dir, id, 0, 256, 4<<20)
	if !errors.Is(err, ErrCopyDamaged) {
		t.Fatalf("Read = %v", err)
	}
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(2, "")}); !errors.Is(err, ErrCopyDamaged) {
		t.Fatalf("Append = %v", err)
	}
	if !bytes.Equal(before, readCopyFile(t, dir, id)) {
		t.Fatal("tampered copy was rewritten")
	}
}

func TestCopyWriteFailuresLeaveTheCopy(t *testing.T) {
	snapshotCopy(t)
	realWrite := copyOps.write
	realChmod := copyOps.chmod
	realOpen := copyOps.open
	dir := t.TempDir()
	id := sampleCopyID()
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")}); err != nil {
		t.Fatal(err)
	}
	before := readCopyFile(t, dir, id)

	t.Run("write", func(t *testing.T) {
		defer func() { copyOps.write = realWrite }()
		boom := errors.New("write failed")
		copyOps.write = func(*os.File, []byte) (int, error) { return 0, boom }
		if err := AppendCopy(dir, id, []MirrorRecord{copyRec(2, "")}); !errors.Is(err, boom) {
			t.Fatalf("error = %v", err)
		}
		if !bytes.Equal(before, readCopyFile(t, dir, id)) {
			t.Fatal("failed write changed the copy")
		}
		copyOps.write = realWrite
		if err := AppendCopy(dir, id, []MirrorRecord{copyRec(2, "")}); err != nil {
			t.Fatal(err)
		}
		got, last, err := ReadCopy(dir, id, 0, 256, 4<<20)
		if err != nil || last != 2 || len(got) != 2 {
			t.Fatalf("retry = %+v last = %d err = %v", got, last, err)
		}
	})
	// The write subtest stored seq 2. Later subtests use a fresh directory.
	t.Run("short write", func(t *testing.T) {
		defer func() { copyOps.write = realWrite }()
		fresh := t.TempDir()
		if err := AppendCopy(fresh, id, []MirrorRecord{copyRec(1, "")}); err != nil {
			t.Fatal(err)
		}
		kept := readCopyFile(t, fresh, id)
		copyOps.write = func(*os.File, []byte) (int, error) { return 1, nil }
		if err := AppendCopy(fresh, id, []MirrorRecord{copyRec(2, "")}); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("error = %v", err)
		}
		if !bytes.Equal(kept, readCopyFile(t, fresh, id)) {
			t.Fatal("short write changed the copy")
		}
		copyOps.write = realWrite
		if err := AppendCopy(fresh, id, []MirrorRecord{copyRec(2, "")}); err != nil {
			t.Fatal(err)
		}
		got, last, err := ReadCopy(fresh, id, 0, 256, 4<<20)
		if err != nil || last != 2 || len(got) != 2 || got[1].Seq != 2 {
			t.Fatalf("retry = %+v last = %d err = %v", got, last, err)
		}
	})
	t.Run("chmod directory", func(t *testing.T) {
		defer func() { copyOps.chmod = realChmod }()
		fresh := t.TempDir()
		boom := errors.New("chmod failed")
		copyOps.chmod = func(string, os.FileMode) error { return boom }
		if err := AppendCopy(fresh, id, []MirrorRecord{copyRec(1, "")}); !errors.Is(err, boom) {
			t.Fatalf("error = %v", err)
		}
		if _, err := os.Stat(copyPathOf(fresh, id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat = %v", err)
		}
	})
	t.Run("chmod file", func(t *testing.T) {
		defer func() { copyOps.chmod = realChmod }()
		fresh := t.TempDir()
		boom := errors.New("chmod file failed")
		copyOps.chmod = func(name string, mode os.FileMode) error {
			if filepath.Base(name) == mirrorFile {
				return boom
			}
			return realChmod(name, mode)
		}
		if err := AppendCopy(fresh, id, []MirrorRecord{copyRec(1, "")}); !errors.Is(err, boom) {
			t.Fatalf("error = %v", err)
		}
		raw, err := os.ReadFile(copyPathOf(fresh, id))
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) != 0 {
			t.Fatalf("file = %q", raw)
		}
	})
	t.Run("open", func(t *testing.T) {
		defer func() { copyOps.open = realOpen }()
		fresh := t.TempDir()
		boom := errors.New("open failed")
		copyOps.open = func(string, int, os.FileMode) (*os.File, error) { return nil, boom }
		if err := AppendCopy(fresh, id, []MirrorRecord{copyRec(1, "")}); !errors.Is(err, boom) {
			t.Fatalf("error = %v", err)
		}
		if _, err := os.Stat(copyPathOf(fresh, id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat = %v", err)
		}
	})
	t.Run("marshal", func(t *testing.T) {
		fresh := t.TempDir()
		boom := errors.New("marshal failed")
		copyOps.marshal = func(any) ([]byte, error) { return nil, boom }
		if err := AppendCopy(fresh, id, []MirrorRecord{copyRec(1, "")}); !errors.Is(err, boom) {
			t.Fatalf("error = %v", err)
		}
		if _, err := os.Stat(filepath.Join(fresh, CopiesDir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat = %v", err)
		}
	})
}

func TestCopyMkdirFailureStoresNothing(t *testing.T) {
	snapshotCopy(t)
	dir := t.TempDir()
	boom := errors.New("mkdir failed")
	copyOps.mkdir = func(string, os.FileMode) error { return boom }
	if err := AppendCopy(dir, sampleCopyID(), []MirrorRecord{copyRec(1, "")}); !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, CopiesDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v", err)
	}
}

func TestCopyReadFailureLeavesTheCopy(t *testing.T) {
	snapshotCopy(t)
	dir := t.TempDir()
	id := sampleCopyID()
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, ""), copyRec(2, "")}); err != nil {
		t.Fatal(err)
	}
	before := readCopyFile(t, dir, id)
	real := copyOps.readAt
	t.Run("error", func(t *testing.T) {
		boom := errors.New("read failed")
		copyOps.readAt = func(*os.File, []byte, int64) (int, error) { return 0, boom }
		_, _, err := ReadCopy(dir, id, 0, 256, 4<<20)
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v", err)
		}
		if !bytes.Equal(before, readCopyFile(t, dir, id)) {
			t.Fatal("read error changed the copy")
		}
	})
	t.Run("short", func(t *testing.T) {
		copyOps.readAt = func(*os.File, []byte, int64) (int, error) { return 1, nil }
		_, _, err := ReadCopy(dir, id, 0, 256, 4<<20)
		if !errors.Is(err, ErrCopyDamaged) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("eof", func(t *testing.T) {
		copyOps.readAt = func(f *os.File, p []byte, off int64) (int, error) {
			n, err := real(f, p, off)
			if err != nil {
				return n, err
			}
			return n, io.EOF
		}
		got, last, err := ReadCopy(dir, id, 0, 256, 4<<20)
		if err != nil || last != 2 || len(got) != 2 {
			t.Fatalf("read = %+v last = %d err = %v", got, last, err)
		}
	})
	t.Run("open", func(t *testing.T) {
		boom := errors.New("open read failed")
		copyOps.open = func(string, int, os.FileMode) (*os.File, error) { return nil, boom }
		_, _, err := ReadCopy(dir, id, 0, 256, 4<<20)
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v", err)
		}
		if !bytes.Equal(before, readCopyFile(t, dir, id)) {
			t.Fatal("open error changed the copy")
		}
	})
}

func TestCopyDirectoryFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	path := copyPathOf(dir, id)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err := ReadCopy(dir, id, 0, 256, 4<<20)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("Read = %v", err)
	}
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")}); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("Append = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("directory was replaced")
	}
}

func TestCopyFileDataDirIsAnError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(file, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := ReadCopy(file, sampleCopyID(), 0, 256, 4<<20)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Read = %v", err)
	}
	if err := AppendCopy(file, sampleCopyID(), []MirrorRecord{copyRec(1, "")}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Append = %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "synthetic" {
		t.Fatalf("file = %q", raw)
	}
}

func TestCopyConcurrentAppendStoresOnce(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	errc := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			errc <- AppendCopy(dir, id, []MirrorRecord{copyRec(1, `{"v":1}`)})
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}
	got, last, err := ReadCopy(dir, id, 0, 256, 4<<20)
	if err != nil || last != 1 || len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("read = %+v last = %d err = %v", got, last, err)
	}
	raw := readCopyFile(t, dir, id)
	if bytes.Count(raw, []byte("\n")) != 1 {
		t.Fatalf("lines = %d", bytes.Count(raw, []byte("\n")))
	}
}

func TestCopySameLengthDamageIsDetectedByFreshIndex(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, `{"v":1}`)}); err != nil {
		t.Fatal(err)
	}
	path := copyPathOf(dir, id)
	raw := readCopyFile(t, dir, id)
	raw[0] = '!'
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Discard this process's cached index to model opening the copy after restart.
	copyMetas.Delete(path)
	if _, _, err := ReadCopy(dir, id, 0, 256, 4<<20); !errors.Is(err, ErrCopyDamaged) {
		t.Fatalf("read error = %v", err)
	}
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(2, `{"v":1}`)}); !errors.Is(err, ErrCopyDamaged) {
		t.Fatalf("append error = %v", err)
	}
	if !bytes.Equal(raw, readCopyFile(t, dir, id)) {
		t.Fatal("damaged copy was changed")
	}
}

func TestCopySizeChangeMakesWarmIndexRescan(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, `{"v":1}`)}); err != nil {
		t.Fatal(err)
	}
	path := copyPathOf(dir, id)
	raw := append(readCopyFile(t, dir, id), '!')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadCopy(dir, id, 0, 256, 4<<20); !errors.Is(err, ErrCopyDamaged) {
		t.Fatalf("read error = %v", err)
	}
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(2, `{"v":1}`)}); !errors.Is(err, ErrCopyDamaged) {
		t.Fatalf("append error = %v", err)
	}
	if !bytes.Equal(raw, readCopyFile(t, dir, id)) {
		t.Fatal("damaged copy was changed")
	}
}

func TestCopySameLengthMissingNewlineIsDamaged(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, `{"v":1}`)}); err != nil {
		t.Fatal(err)
	}
	path := copyPathOf(dir, id)
	raw := readCopyFile(t, dir, id)
	raw[len(raw)-1] = '!'
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadCopy(dir, id, 0, 256, 4<<20); !errors.Is(err, ErrCopyDamaged) {
		t.Fatalf("read error = %v", err)
	}
}

func TestCopyReadIsBoundedAtEveryPosition(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	const total = 4000
	records := make([]MirrorRecord, total)
	for i := range records {
		records[i] = copyRec(uint64(i+1), `{"v":1,"text":"`+strings.Repeat("x", 64)+`"}`)
	}
	if err := AppendCopy(dir, id, records); err != nil {
		t.Fatal(err)
	}
	snapshotCopy(t)
	real := copyOps.readAt
	var bytesRead int64
	copyOps.readAt = func(f *os.File, p []byte, off int64) (int, error) {
		n, e := real(f, p, off)
		bytesRead += int64(n)
		return n, e
	}
	info, err := os.Stat(copyPathOf(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	for _, after := range []uint64{0, 2000, 3900} {
		bytesRead = 0
		got, last, err := ReadCopy(dir, id, after, 256, 4<<20)
		if err != nil || last != total || len(got) == 0 || len(got) > 256 {
			t.Fatalf("after %d count %d last %d err %v", after, len(got), last, err)
		}
		if bytesRead >= info.Size()/2 {
			t.Fatalf("after %d read %d of %d", after, bytesRead, info.Size())
		}
	}
	bytesRead = 0
	_, _, err = ReadCopy(dir, id, ^uint64(0), 256, 4<<20)
	if err != nil || bytesRead != 0 {
		t.Fatalf("last read bytes %d err %v", bytesRead, err)
	}
}

func TestCopyReadReturnsOversizedFirstBody(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	body := `"` + strings.Repeat("x", (4<<20)+1) + `"`
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, body), copyRec(2, `{}`)}); err != nil {
		t.Fatal(err)
	}
	got, last, err := ReadCopy(dir, id, 0, 256, 4<<20)
	if err != nil || last != 2 || len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("count %d last %d err %v", len(got), last, err)
	}
}

func TestCopyInitialScanUsesPiecesAndFindsBoundaryDamage(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	records := make([]MirrorRecord, 1200)
	for i := range records {
		records[i] = copyRec(uint64(i+1), `{"text":"`+strings.Repeat("x", 100)+`"}`)
	}
	if err := AppendCopy(dir, id, records); err != nil {
		t.Fatal(err)
	}
	path := copyPathOf(dir, id)
	copyMetas.Delete(path)
	snapshotCopy(t)
	real := copyOps.readAt
	var calls int
	var largest int
	copyOps.readAt = func(f *os.File, p []byte, off int64) (int, error) {
		calls++
		if len(p) > largest {
			largest = len(p)
		}
		return real(f, p, off)
	}
	_, last, err := ReadCopy(dir, id, ^uint64(0), 256, 4<<20)
	info, e := os.Stat(path)
	if err != nil || e != nil || last != 1200 || calls < 2 || int64(largest) >= info.Size() {
		t.Fatalf("scan calls %d largest %d last %d err %v", calls, largest, last, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	boundary := bytes.IndexByte(raw[65535:], '\n')
	if boundary < 0 {
		t.Fatal("missing boundary newline")
	}
	raw[65535+boundary] = '!'
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	copyMetas.Delete(path)
	if _, _, err = ReadCopy(dir, id, 0, 256, 4<<20); !errors.Is(err, ErrCopyDamaged) {
		t.Fatalf("boundary error %v", err)
	}
}

func TestCopyScanOpenAndStatFailures(t *testing.T) {
	for _, mode := range []string{"open", "stat"} {
		t.Run(mode, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, `{}`)}); err != nil {
				t.Fatal(err)
			}
			path := copyPathOf(dir, id)
			copyMetas.Delete(path)
			snapshotCopy(t)
			real := copyOps.open
			boom := errors.New("synthetic open failure")
			copyOps.open = func(path string, flag int, perm os.FileMode) (*os.File, error) {
				if mode == "open" {
					return nil, boom
				}
				f, err := real(path, flag, perm)
				if err != nil {
					return nil, err
				}
				_ = f.Close()
				return f, nil
			}
			_, _, err := ReadCopy(dir, id, 0, 256, 4<<20)
			if err == nil {
				t.Fatal("scan accepted failed open or stat")
			}
			if mode == "open" && !errors.Is(err, boom) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCopyFailedIndexScanDoesNotCachePartialIndex(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	records := make([]MirrorRecord, 100)
	for i := range records {
		records[i] = copyRec(uint64(i+1), `{"text":"`+strings.Repeat("x", 100)+`"}`)
	}
	if err := AppendCopy(dir, id, records); err != nil {
		t.Fatal(err)
	}
	path := copyPathOf(dir, id)
	copyMetas.Delete(path)
	snapshotCopy(t)
	real := copyOps.readAt
	boom := errors.New("synthetic read failure")
	copyOps.readAt = func(f *os.File, p []byte, off int64) (int, error) {
		if off >= 4096 {
			return 0, boom
		}
		return real(f, p, off)
	}
	if _, _, err := ReadCopy(dir, id, 0, 256, 4<<20); !errors.Is(err, boom) {
		t.Fatalf("first scan error = %v", err)
	}
	copyOps.readAt = real
	got, last, err := ReadCopy(dir, id, 0, 256, 4<<20)
	if err != nil || last != 100 || len(got) != 100 {
		t.Fatalf("retry last %d count %d err %v", last, len(got), err)
	}
}

func TestCopyBranchShortReadWithoutError(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(strconv.FormatBool(fresh), func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, `{}`)}); err != nil {
				t.Fatal(err)
			}
			if fresh {
				copyMetas.Delete(copyPathOf(dir, id))
			}
			snapshotCopy(t)
			copyOps.readAt = func(*os.File, []byte, int64) (int, error) { return 1, nil }
			if _, _, err := ReadCopy(dir, id, 0, 256, 4<<20); !errors.Is(err, ErrCopyDamaged) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCopyShortValidPrefixWithoutErrorIsDamaged(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(strconv.FormatBool(fresh), func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, `{"v":1}`)}); err != nil {
				t.Fatal(err)
			}
			if fresh {
				copyMetas.Delete(copyPathOf(dir, id))
			}
			snapshotCopy(t)
			real := copyOps.readAt
			calls := 0
			copyOps.readAt = func(f *os.File, p []byte, off int64) (int, error) {
				calls++
				if calls == 1 {
					if len(p) < 2 {
						t.Fatal("read too small")
					}
					n, err := real(f, p[:len(p)-1], off)
					if err != nil {
						t.Fatalf("prefix read: %v", err)
					}
					return n, nil
				}
				return real(f, p, off)
			}
			if _, _, err := ReadCopy(dir, id, 0, 256, 4<<20); !errors.Is(err, ErrCopyDamaged) {
				t.Fatalf("error = %v", err)
			}
			if calls != 1 {
				t.Fatalf("read calls = %d", calls)
			}
		})
	}
}

func TestRemoveCopyForgetsIndexAndPrunesParents(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")}); err != nil {
		t.Fatal("append failed")
	}
	path := copyPathOf(dir, id)
	if err := RemoveCopy(dir, id); err != nil {
		t.Fatal("remove failed")
	}
	if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Error("generation remains")
	}
	if _, ok := copyMetas.Load(path); ok {
		t.Error("copy index remains")
	}
	if _, err := os.Stat(filepath.Join(dir, CopiesDir, id.Server)); !errors.Is(err, os.ErrNotExist) {
		t.Error("empty parents remain")
	}
	if _, err := os.Stat(filepath.Join(dir, CopiesDir)); err != nil {
		t.Error("copy root was removed")
	}
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")}); err != nil {
		t.Fatal("fresh append did not start at one")
	}
	_, last, err := ReadCopy(dir, id, 0, 256, 4<<20)
	if err != nil {
		t.Fatal("fresh copy cannot be read")
	}
	if last != 1 {
		t.Errorf("last = %d, want 1", last)
	}
}

func TestRemoveCopyPreservesSiblingsAndMissing(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	sibling := id
	sibling.Generation = fmt.Sprintf("%016x", 5)
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")}); err != nil {
		t.Fatal("append failed")
	}
	if err := AppendCopy(dir, sibling, []MirrorRecord{copyRec(1, "")}); err != nil {
		t.Fatal("sibling append failed")
	}
	if err := RemoveCopy(dir, id); err != nil {
		t.Fatal("remove failed")
	}
	if _, err := os.Stat(copyPathOf(dir, sibling)); err != nil {
		t.Error("sibling was lost")
	}
	if err := RemoveCopy(dir, id); err != nil {
		t.Error("missing copy must succeed")
	}
	if err := RemoveCopy(t.TempDir(), id); err != nil {
		t.Error("missing root must succeed")
	}
}

func TestRemoveCopyRejectsEachBadPathPart(t *testing.T) {
	for _, part := range []string{"server", "owner", "chat", "generation"} {
		t.Run(part, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			switch part {
			case "server":
				id.Server = "../bad"
			case "owner":
				id.Owner = "../bad"
			case "chat":
				id.Chat = "../bad"
			case "generation":
				id.Generation = "../bad"
			}
			if err := RemoveCopy(dir, id); !errors.Is(err, ErrCopyPath) {
				t.Error("bad path accepted")
			}
			if _, err := os.Stat(filepath.Join(dir, CopiesDir)); !errors.Is(err, os.ErrNotExist) {
				t.Error("bad path touched the copy root")
			}
		})
	}
}

func TestRemoveCopyRemovesLinkWithoutFollowingIt(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	target := t.TempDir()
	marker := filepath.Join(target, "marker")
	if err := os.WriteFile(marker, []byte("synthetic"), 0o600); err != nil {
		t.Fatal("cannot create marker")
	}
	generation := filepath.Dir(copyPathOf(dir, id))
	if err := os.MkdirAll(filepath.Dir(generation), 0o700); err != nil {
		t.Fatal("cannot create parents")
	}
	if err := os.Symlink(target, generation); err != nil {
		t.Fatal("cannot create symlink")
	}
	if err := RemoveCopy(dir, id); err != nil {
		t.Fatal("remove failed")
	}
	if _, err := os.Lstat(generation); !errors.Is(err, os.ErrNotExist) {
		t.Error("link remains")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("link target was changed")
	}
}

func TestRemoveCopySurfacesRemovalFailure(t *testing.T) {
	snapshotCopy(t)
	dir, id := t.TempDir(), sampleCopyID()
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")}); err != nil {
		t.Fatal("append failed")
	}
	boom := errors.New("synthetic failure")
	copyOps.removeAll = func(*os.Root, string) error { return boom }
	if err := RemoveCopy(dir, id); !errors.Is(err, boom) {
		t.Error("remove failure not returned")
	}
	if _, err := os.Stat(copyPathOf(dir, id)); err != nil {
		t.Error("failed removal lost the copy")
	}
}

func TestRemoveCopyDoesNotTraverseParentLinks(t *testing.T) {
	for _, part := range []string{"root", "server", "owner", "chat"} {
		t.Run(part, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			target := t.TempDir()
			link := filepath.Join(dir, CopiesDir)
			below := filepath.Join(id.Server, id.Owner, id.Chat, id.Generation)
			switch part {
			case "server":
				link = filepath.Join(link, id.Server)
				below = filepath.Join(id.Owner, id.Chat, id.Generation)
			case "owner":
				link = filepath.Join(link, id.Server, id.Owner)
				below = filepath.Join(id.Chat, id.Generation)
			case "chat":
				link = filepath.Join(link, id.Server, id.Owner, id.Chat)
				below = id.Generation
			}
			marker := filepath.Join(target, below, "marker")
			if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
				t.Fatal("cannot create target")
			}
			if err := os.WriteFile(marker, []byte("synthetic"), 0o600); err != nil {
				t.Fatal("cannot create marker")
			}
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal("cannot create parents")
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal("cannot create symlink")
			}
			_ = RemoveCopy(dir, id)
			if _, err := os.Stat(marker); err != nil {
				t.Error("link target was changed")
			}
		})
	}
}

func TestRemoveCopySerializesWaitingAppend(t *testing.T) {
	snapshotCopy(t)
	dir, id := t.TempDir(), sampleCopyID()
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")}); err != nil {
		t.Fatal("initial append failed")
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	copyOps.removeAll = func(root *os.Root, name string) error {
		close(entered)
		<-release
		return root.RemoveAll(name)
	}
	removed := make(chan error, 1)
	go func() { removed <- RemoveCopy(dir, id) }()
	select {
	case <-entered:
	case <-removed:
		t.Fatal("removal did not enter the filesystem seam")
	}
	started := make(chan struct{})
	appended := make(chan error, 1)
	go func() {
		close(started)
		appended <- AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")})
	}()
	<-started
	close(release)
	if err := <-removed; err != nil {
		t.Fatal("removal failed")
	}
	if err := <-appended; err != nil {
		t.Fatal("waiting append failed")
	}
	_, last, err := ReadCopy(dir, id, 0, 256, 4<<20)
	if err != nil {
		t.Fatal("cannot read new copy")
	}
	if last != 1 {
		t.Errorf("last = %d, want 1", last)
	}
}

func TestRemoveCopyReportsFilesystemFailures(t *testing.T) {
	for _, part := range []string{"open root", "open child", "lstat", "stat", "remove parent"} {
		t.Run(part, func(t *testing.T) {
			snapshotCopy(t)
			dir, id := t.TempDir(), sampleCopyID()
			if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")}); err != nil {
				t.Fatal("initial append failed")
			}
			boom := errors.New("synthetic filesystem failure")
			switch part {
			case "open root":
				copyOps.openRoot = func(string) (*os.Root, error) { return nil, boom }
			case "open child":
				copyOps.openChild = func(*os.Root, string) (*os.Root, error) { return nil, boom }
			case "lstat":
				copyOps.lstat = func(*os.Root, string) (os.FileInfo, error) { return nil, boom }
			case "stat":
				copyOps.stat = func(*os.Root, string) (os.FileInfo, error) { return nil, boom }
			case "remove parent":
				copyOps.remove = func(*os.Root, string) error { return boom }
			}
			if err := RemoveCopy(dir, id); !errors.Is(err, boom) {
				t.Error("filesystem failure not returned")
			}
		})
	}
}

func TestRemoveCopyRejectsChangedRoot(t *testing.T) {
	snapshotCopy(t)
	dir, id := t.TempDir(), sampleCopyID()
	if err := AppendCopy(dir, id, []MirrorRecord{copyRec(1, "")}); err != nil {
		t.Fatal("initial append failed")
	}
	other, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal("cannot stat other directory")
	}
	copyOps.stat = func(*os.Root, string) (os.FileInfo, error) { return other, nil }
	if err := RemoveCopy(dir, id); !errors.Is(err, ErrCopyPath) {
		t.Error("changed copy root was not refused")
	}
	if _, err := os.Stat(copyPathOf(dir, id)); err != nil {
		t.Error("copy was removed")
	}
}

func TestRemoveCopyMissingDataDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	if err := RemoveCopy(path, sampleCopyID()); err != nil {
		t.Error("missing data directory must succeed")
	}
}
