package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sampleSub(n int) Subscription {
	return Subscription{
		Server:     fmt.Sprintf("%032x", n),
		Owner:      fmt.Sprintf("%032x", n+100),
		Chat:       fmt.Sprintf("%016x", n),
		Generation: fmt.Sprintf("%016x", n+200),
		ServerName: "Lab One",
		OwnerName:  "Ada",
		Title:      "Synthetic title",
		Access:     "read",
		State:      "live",
		MirrorLast: 42,
		SeenMirror: 40,
		FirstSeen:  "2026-11-14T09:30:00Z",
	}
}

func richSub() Subscription {
	s := sampleSub(1)
	s.OwnerName = ""
	s.Title = `North <relay> "a"`
	s.Access = "write"
	s.State = "ended"
	s.MirrorLast = 0
	s.SeenMirror = 7
	return s
}

func snapshotSubs(t *testing.T) {
	t.Helper()
	ops := storeOps
	marshal := marshalSubs
	t.Cleanup(func() {
		storeOps = ops
		marshalSubs = marshal
	})
}

func writeSubsRaw(t *testing.T, dir string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, SubsFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func subsDocument(t *testing.T, subs []Subscription) []byte {
	t.Helper()
	items := make([]map[string]any, 0, len(subs))
	for _, sub := range subs {
		obj := subObject(sub)
		delete(obj, "comments_last")
		delete(obj, "seen_comments")
		items = append(items, obj)
	}
	raw, err := json.Marshal(struct {
		V             int              `json:"v"`
		Subscriptions []map[string]any `json:"subscriptions"`
	}{V: 1, Subscriptions: items})
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func subObject(s Subscription) map[string]any {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		panic(err)
	}
	return m
}

func TestLoadSubsMissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := LoadSubs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("subs = %#v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, SubsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, File)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("team file stat = %v", err)
	}
}

func TestSubsRoundTripKeepsFieldsAndMode(t *testing.T) {
	dir := t.TempDir()
	teamBefore := writeTeamFile(t, dir, []Entry{sampleEntry(1)})
	first := richSub()
	second := sampleSub(2)
	second.Server = first.Server
	second.Owner = first.Owner
	second.Chat = first.Chat
	second.Title = ""
	second.OwnerName = "Ada"
	second.Access = "read"
	second.State = "live"
	second.MirrorLast = ^uint64(0)
	second.SeenMirror = 0
	third := sampleSub(3)
	third.ServerName = "Lab One West"
	saved := []Subscription{first, third, second}
	if err := SaveSubs(dir, saved); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSubs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, saved) {
		t.Fatalf("subs = %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	first.State = "live"
	first.Title = "Renamed"
	saved[0] = first
	if err := SaveSubs(dir, saved); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(filepath.Join(dir, SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode after rewrite = %o", info.Mode().Perm())
	}
	got, err = LoadSubs(dir)
	if err != nil || !reflect.DeepEqual(got, saved) {
		t.Fatalf("after rewrite = %+v err = %v", got, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("token")) || bytes.Contains(raw, []byte("synthetic prompt")) {
		t.Fatal("file holds content or a token")
	}
	teamAfter, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(teamBefore, teamAfter) {
		t.Fatal("subscription write changed team.json")
	}
	names := map[string]bool{SubsFile: true, File: true}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("files = %d", len(entries))
	}
	for _, e := range entries {
		if !names[e.Name()] {
			t.Fatalf("unexpected file %s", e.Name())
		}
	}
}

func TestSaveSubsOfNilWritesAnEmptyList(t *testing.T) {
	dir := t.TempDir()
	if err := SaveSubs(dir, []Subscription{sampleSub(1)}); err != nil {
		t.Fatal(err)
	}
	if err := SaveSubs(dir, nil); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSubs(dir)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("subs = %#v err = %v", got, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"subscriptions":[]`)) {
		t.Fatalf("document = %s", raw)
	}
	info, err := os.Stat(filepath.Join(dir, SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	assertOnlyFile(t, dir, SubsFile)
}

func TestSaveSubsRejectsThe4097th(t *testing.T) {
	t.Run("no file yet", func(t *testing.T) {
		empty := t.TempDir()
		over := make([]Subscription, maxSubs+1)
		for i := range over {
			over[i] = sampleSub(i + 1)
		}
		err := SaveSubs(empty, over)
		if !errors.Is(err, ErrTooManySubs) {
			t.Fatalf("error = %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(empty, SubsFile)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("stat = %v", statErr)
		}
	})
	t.Run("full file", func(t *testing.T) {
		dir := t.TempDir()
		subs := make([]Subscription, maxSubs)
		for i := range subs {
			subs[i] = sampleSub(i + 1)
		}
		if err := SaveSubs(dir, subs); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filepath.Join(dir, SubsFile))
		if err != nil {
			t.Fatal(err)
		}
		extra := append(append([]Subscription{}, subs...), sampleSub(maxSubs+1))
		err = SaveSubs(dir, extra)
		if !errors.Is(err, ErrTooManySubs) {
			t.Fatalf("error = %v", err)
		}
		if strings.Contains(err.Error(), "Synthetic title") {
			t.Fatalf("error quotes a title: %v", err)
		}
		after, err := os.ReadFile(filepath.Join(dir, SubsFile))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("the rejected subscription changed the file")
		}
		got, err := LoadSubs(dir)
		if err != nil || len(got) != maxSubs {
			t.Fatalf("len = %d err = %v", len(got), err)
		}
		subs[0].Title = "Still room to replace"
		if err := SaveSubs(dir, subs); err != nil {
			t.Fatal(err)
		}
		got, err = LoadSubs(dir)
		if err != nil || len(got) != maxSubs || got[0].Title != subs[0].Title {
			t.Fatalf("len = %d title = %q err = %v", len(got), got[0].Title, err)
		}
		assertOnlyFile(t, dir, SubsFile)
	})
}

func TestSaveSubsAcceptsAFullSizedList(t *testing.T) {
	dir := t.TempDir()
	subs := make([]Subscription, maxSubs)
	for i := range subs {
		subs[i] = sampleSub(i + 1)
		subs[i].ServerName = strings.Repeat("<", 64)
		subs[i].OwnerName = strings.Repeat("<", 64)
		subs[i].Title = strings.Repeat("<", 200)
		subs[i].Access = "write"
		subs[i].State = "ended"
		subs[i].MirrorLast = ^uint64(0)
		subs[i].SeenMirror = ^uint64(0)
	}
	if err := SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if info.Size() > maxSubsFile {
		t.Fatalf("size = %d", info.Size())
	}
	got, err := LoadSubs(dir)
	if err != nil || len(got) != maxSubs || got[0].Title != subs[0].Title || got[maxSubs-1].Server != subs[maxSubs-1].Server {
		t.Fatalf("len = %d err = %v", len(got), err)
	}
	assertOnlyFile(t, dir, SubsFile)
}

func TestDamagedSubsFileIsLeftUntouched(t *testing.T) {
	secret := strings.Repeat("ab", 32)
	dupA := sampleSub(1)
	dupB := sampleSub(2)
	dupB.Server, dupB.Owner, dupB.Chat, dupB.Generation = dupA.Server, dupA.Owner, dupA.Chat, dupA.Generation
	dupB.Title = "Other synthetic"
	many := make([]Subscription, maxSubs+1)
	for i := range many {
		many[i] = sampleSub(i + 1)
	}
	cases := []struct {
		name string
		body []byte
	}{
		{"empty", []byte{}},
		{"blank", []byte("\n")},
		{"not json", []byte("nope")},
		{"version 3", []byte(`{"v":3,"subscriptions":[]}`)},
		{"missing version", []byte(`{"subscriptions":[]}`)},
		{"null version", []byte(`{"v":null,"subscriptions":[]}`)},
		{"missing subscriptions", []byte(`{"v":1}`)},
		{"null subscriptions", []byte(`{"v":1,"subscriptions":null}`)},
		{"subscriptions object", []byte(`{"v":1,"subscriptions":{}}`)},
		{"unknown field", []byte(`{"v":1,"subscriptions":[],"token":"` + secret + `"}`)},
		{"trailing value", []byte(`{"v":1,"subscriptions":[]}{}`)},
		{"trailing junk", []byte(`{"v":1,"subscriptions":[]} nope`)},
		{"duplicate identity", subsDocument(t, []Subscription{dupA, dupB})},
		{"too many", subsDocument(t, many)},
		{"bad utf-8", []byte("{\"v\":1,\"subscriptions\":[{\"title\":\"bad\xff\"}]}")},
		{"oversized", append([]byte(secret), bytes.Repeat([]byte("x"), maxSubsFile)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSubsRaw(t, dir, tc.body)
			assertSubsDamageUntouched(t, dir, tc.body, secret)
		})
	}

	base := subObject(sampleSub(1))
	fieldCases := []struct {
		name string
		edit func(map[string]any)
	}{
		{"missing server", func(m map[string]any) { delete(m, "server") }},
		{"null server", func(m map[string]any) { m["server"] = nil }},
		{"short server", func(m map[string]any) { m["server"] = "abcd" }},
		{"uppercase server", func(m map[string]any) { m["server"] = "0123456789ABCDEF0123456789ABCDEF" }},
		{"missing owner", func(m map[string]any) { delete(m, "owner") }},
		{"short owner", func(m map[string]any) { m["owner"] = "abcd" }},
		{"uppercase owner", func(m map[string]any) { m["owner"] = "0123456789ABCDEF0123456789ABCDEF" }},
		{"missing chat", func(m map[string]any) { delete(m, "chat") }},
		{"protocol chat", func(m map[string]any) { m["chat"] = "main" }},
		{"uppercase chat", func(m map[string]any) { m["chat"] = "0123456789ABCDEF" }},
		{"short chat", func(m map[string]any) { m["chat"] = "0123456789abcde" }},
		{"missing generation", func(m map[string]any) { delete(m, "generation") }},
		{"short generation", func(m map[string]any) { m["generation"] = "abcd" }},
		{"uppercase generation", func(m map[string]any) { m["generation"] = "0123456789ABCDEF" }},
		{"missing server_name", func(m map[string]any) { delete(m, "server_name") }},
		{"empty server_name", func(m map[string]any) { m["server_name"] = "" }},
		{"long server_name", func(m map[string]any) { m["server_name"] = strings.Repeat("a", 65) }},
		{"server_name newline", func(m map[string]any) { m["server_name"] = "bad\n" }},
		{"missing owner_name", func(m map[string]any) { delete(m, "owner_name") }},
		{"null owner_name", func(m map[string]any) { m["owner_name"] = nil }},
		{"long owner_name", func(m map[string]any) { m["owner_name"] = strings.Repeat("a", 65) }},
		{"owner_name space", func(m map[string]any) { m["owner_name"] = " Ada" }},
		{"owner_name newline", func(m map[string]any) { m["owner_name"] = "bad\n" }},
		{"missing title", func(m map[string]any) { delete(m, "title") }},
		{"null title", func(m map[string]any) { m["title"] = nil }},
		{"long title", func(m map[string]any) { m["title"] = strings.Repeat("a", 201) }},
		{"title newline", func(m map[string]any) { m["title"] = "bad\n" }},
		{"missing access", func(m map[string]any) { delete(m, "access") }},
		{"null access", func(m map[string]any) { m["access"] = nil }},
		{"access unknown", func(m map[string]any) { m["access"] = "observer" }},
		{"missing state", func(m map[string]any) { delete(m, "state") }},
		{"null state", func(m map[string]any) { m["state"] = nil }},
		{"bad state", func(m map[string]any) { m["state"] = "paused" }},
		{"missing mirror_last", func(m map[string]any) { delete(m, "mirror_last") }},
		{"null mirror_last", func(m map[string]any) { m["mirror_last"] = nil }},
		{"fractional mirror_last", func(m map[string]any) { m["mirror_last"] = 1.5 }},
		{"negative mirror_last", func(m map[string]any) { m["mirror_last"] = -1 }},
		{"missing seen_mirror", func(m map[string]any) { delete(m, "seen_mirror") }},
		{"null seen_mirror", func(m map[string]any) { m["seen_mirror"] = nil }},
		{"negative seen_mirror", func(m map[string]any) { m["seen_mirror"] = -1 }},
		{"missing first_seen", func(m map[string]any) { delete(m, "first_seen") }},
		{"null first_seen", func(m map[string]any) { m["first_seen"] = nil }},
		{"bad time", func(m map[string]any) { m["first_seen"] = "yesterday" }},
		{"offset time", func(m map[string]any) { m["first_seen"] = "2026-11-14T09:30:00+00:00" }},
		{"fractional time", func(m map[string]any) { m["first_seen"] = "2026-11-14T09:30:00.000Z" }},
		{"item token", func(m map[string]any) { m["token"] = secret }},
	}
	for _, tc := range fieldCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			obj := map[string]any{}
			for k, v := range base {
				obj[k] = v
			}
			tc.edit(obj)
			body, err := json.Marshal(map[string]any{"v": 1, "subscriptions": []any{obj}})
			if err != nil {
				t.Fatal(err)
			}
			writeSubsRaw(t, dir, body)
			assertSubsDamageUntouched(t, dir, body, secret)
		})
	}
}

func assertSubsDamageUntouched(t *testing.T, dir string, before []byte, secret string) {
	t.Helper()
	_, err := LoadSubs(dir)
	if !errors.Is(err, ErrSubsDamaged) {
		t.Fatalf("Load = %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "Synthetic title") {
		t.Fatalf("Load quotes the file: %v", err)
	}
	if err := SaveSubs(dir, []Subscription{sampleSub(4)}); !errors.Is(err, ErrSubsDamaged) {
		t.Fatalf("Save = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("damaged file was rewritten")
	}
	assertOnlyFile(t, dir, SubsFile)
}

func TestSaveSubsRejectsAnInvalidSubscription(t *testing.T) {
	long := strings.Repeat("a", 65)
	cases := []struct {
		name string
		edit func(*Subscription)
	}{
		{"empty server", func(s *Subscription) { s.Server = "" }},
		{"short server", func(s *Subscription) { s.Server = "abcd" }},
		{"uppercase server", func(s *Subscription) { s.Server = "0123456789ABCDEF0123456789ABCDEF" }},
		{"empty owner", func(s *Subscription) { s.Owner = "" }},
		{"short owner", func(s *Subscription) { s.Owner = "abcd" }},
		{"uppercase owner", func(s *Subscription) { s.Owner = "0123456789ABCDEF0123456789ABCDEF" }},
		{"empty chat", func(s *Subscription) { s.Chat = "" }},
		{"protocol chat", func(s *Subscription) { s.Chat = "main" }},
		{"uppercase chat", func(s *Subscription) { s.Chat = "0123456789ABCDEF" }},
		{"short chat", func(s *Subscription) { s.Chat = "0123456789abcde" }},
		{"long chat", func(s *Subscription) { s.Chat = "0123456789abcdef0" }},
		{"empty generation", func(s *Subscription) { s.Generation = "" }},
		{"short generation", func(s *Subscription) { s.Generation = "abcd" }},
		{"uppercase generation", func(s *Subscription) { s.Generation = "0123456789ABCDEF" }},
		{"empty server name", func(s *Subscription) { s.ServerName = "" }},
		{"long server name", func(s *Subscription) { s.ServerName = long }},
		{"server name newline", func(s *Subscription) { s.ServerName = "bad\n" }},
		{"server name utf-8", func(s *Subscription) { s.ServerName = "bad\xff" }},
		{"long owner name", func(s *Subscription) { s.OwnerName = long }},
		{"owner name space", func(s *Subscription) { s.OwnerName = " Ada" }},
		{"owner name trailing space", func(s *Subscription) { s.OwnerName = "Ada " }},
		{"owner name newline", func(s *Subscription) { s.OwnerName = "bad\n" }},
		{"owner name utf-8", func(s *Subscription) { s.OwnerName = "bad\xff" }},
		{"long title", func(s *Subscription) { s.Title = strings.Repeat("a", 201) }},
		{"title newline", func(s *Subscription) { s.Title = "bad\n" }},
		{"title utf-8", func(s *Subscription) { s.Title = "bad\xff" }},
		{"empty access", func(s *Subscription) { s.Access = "" }},
		{"access unknown", func(s *Subscription) { s.Access = "observer" }},
		{"empty state", func(s *Subscription) { s.State = "" }},
		{"bad state", func(s *Subscription) { s.State = "paused" }},
		{"bad time", func(s *Subscription) { s.FirstSeen = "yesterday" }},
		{"offset time", func(s *Subscription) { s.FirstSeen = "2026-11-14T09:30:00+00:00" }},
		{"fractional time", func(s *Subscription) { s.FirstSeen = "2026-11-14T09:30:00.000Z" }},
		{"empty time", func(s *Subscription) { s.FirstSeen = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			before := subsDocument(t, []Subscription{sampleSub(1)})
			writeSubsRaw(t, dir, before)
			s := sampleSub(2)
			s.Title = `North <relay> "a"`
			tc.edit(&s)
			err := SaveSubs(dir, []Subscription{sampleSub(1), s})
			if !errors.Is(err, ErrSubsDamaged) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "North") || (s.Server != "" && strings.Contains(err.Error(), s.Server)) {
				t.Fatalf("error quotes the subscription: %v", err)
			}
			assertSavedSubsUntouched(t, dir, before, []Subscription{sampleSub(1)})
		})
	}
	t.Run("duplicate identity", func(t *testing.T) {
		dir := t.TempDir()
		before := subsDocument(t, []Subscription{sampleSub(1)})
		writeSubsRaw(t, dir, before)
		a := sampleSub(2)
		b := sampleSub(3)
		b.Server, b.Owner, b.Chat, b.Generation = a.Server, a.Owner, a.Chat, a.Generation
		b.Title = "Other synthetic"
		if err := SaveSubs(dir, []Subscription{a, b}); !errors.Is(err, ErrSubsDamaged) {
			t.Fatalf("error = %v", err)
		}
		assertSavedSubsUntouched(t, dir, before, []Subscription{sampleSub(1)})
	})
	t.Run("no file yet", func(t *testing.T) {
		dir := t.TempDir()
		s := sampleSub(1)
		s.Chat = "main"
		if err := SaveSubs(dir, []Subscription{s}); !errors.Is(err, ErrSubsDamaged) {
			t.Fatalf("error = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, SubsFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat = %v", err)
		}
		got, err := LoadSubs(dir)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("subs = %#v err = %v", got, err)
		}
	})
}

func assertSavedSubsUntouched(t *testing.T, dir string, before []byte, want []Subscription) {
	t.Helper()
	after, err := os.ReadFile(filepath.Join(dir, SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("file changed")
	}
	got, err := LoadSubs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("subs = %+v", got)
	}
	assertOnlyFile(t, dir, SubsFile)
}

func TestSubsWriteFailuresLeaveThePreviousFile(t *testing.T) {
	cases := []struct {
		name string
		set  func(real storeIO) error
	}{
		{"create", func(real storeIO) error {
			boom := errors.New("create failed")
			storeOps.createTemp = func(string) (*os.File, error) { return nil, boom }
			return boom
		}},
		{"chmod", func(real storeIO) error {
			boom := errors.New("chmod failed")
			storeOps.chmod = func(*os.File, os.FileMode) error { return boom }
			return boom
		}},
		{"write", func(real storeIO) error {
			boom := errors.New("write failed")
			storeOps.write = func(*os.File, []byte) (int, error) { return 0, boom }
			return boom
		}},
		{"short write", func(real storeIO) error {
			storeOps.write = func(*os.File, []byte) (int, error) { return 1, nil }
			return io.ErrShortWrite
		}},
		{"sync", func(real storeIO) error {
			boom := errors.New("sync failed")
			storeOps.syncFile = func(*os.File) error { return boom }
			return boom
		}},
		{"close", func(real storeIO) error {
			boom := errors.New("close failed")
			storeOps.close = func(f *os.File) error {
				_ = real.close(f)
				return boom
			}
			return boom
		}},
		{"rename", func(real storeIO) error {
			boom := errors.New("rename failed")
			storeOps.rename = func(string, string) error { return boom }
			return boom
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshotSubs(t)
			dir := t.TempDir()
			before := subsDocument(t, []Subscription{sampleSub(1)})
			writeSubsRaw(t, dir, before)
			want := tc.set(storeOps)
			if err := SaveSubs(dir, []Subscription{sampleSub(2)}); !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
			after, err := os.ReadFile(filepath.Join(dir, SubsFile))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed write changed the file")
			}
			assertOnlyFile(t, dir, SubsFile)
		})
	}
	t.Run("missing directory", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "missing")
		err := SaveSubs(missing, []Subscription{sampleSub(1)})
		if err == nil || !strings.Contains(err.Error(), "no such file") {
			t.Fatalf("error = %v", err)
		}
		if _, statErr := os.Stat(missing); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("stat = %v", statErr)
		}
	})
}

func TestSubsMarshalFailureWritesNothing(t *testing.T) {
	snapshotSubs(t)
	boom := errors.New("marshal failed")
	marshalSubs = func(any) ([]byte, error) { return nil, boom }
	dir := t.TempDir()
	before := subsDocument(t, []Subscription{sampleSub(1)})
	writeSubsRaw(t, dir, before)
	if err := SaveSubs(dir, []Subscription{sampleSub(2)}); !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("marshal failure changed the file")
	}
	assertOnlyFile(t, dir, SubsFile)
}

func TestLoadSubsOfADirectoryIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SubsFile)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSubs(dir)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("Load = %v", err)
	}
	if err := SaveSubs(dir, []Subscription{sampleSub(1)}); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("Save = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("directory was replaced")
	}
	assertOnlyFile(t, dir, SubsFile)
}

func TestLoadSubsOfAFileDataDirIsAnError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(file, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSubs(file)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Load = %v", err)
	}
	if err := SaveSubs(file, []Subscription{sampleSub(1)}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Save = %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "synthetic" {
		t.Fatalf("file = %q", raw)
	}
}

func TestSubsVersionTwoAndLegacyMigration(t *testing.T) {
	t.Run("legacy reads zero and saves version two", func(t *testing.T) {
		dir := t.TempDir()
		legacy := subsDocument(t, []Subscription{sampleSub(1)})
		writeSubsRaw(t, dir, legacy)
		subs, err := LoadSubs(dir)
		if err != nil || len(subs) != 1 {
			t.Fatal("legacy state was not read")
		}
		if subs[0].CommentsLast != 0 || subs[0].SeenComments != 0 {
			t.Fatal("legacy counts were not zero")
		}
		if err := SaveSubs(dir, subs); err != nil {
			t.Fatal("legacy state was not saved")
		}
		raw, err := os.ReadFile(filepath.Join(dir, SubsFile))
		if err != nil {
			t.Fatal("saved state unavailable")
		}
		var doc struct {
			V             int                          `json:"v"`
			Subscriptions []map[string]json.RawMessage `json:"subscriptions"`
		}
		if json.Unmarshal(raw, &doc) != nil || doc.V != 2 || len(doc.Subscriptions) != 1 {
			t.Fatal("save did not write version two")
		}
		if _, ok := doc.Subscriptions[0]["comments_last"]; !ok {
			t.Error("missing comments_last")
		}
		if _, ok := doc.Subscriptions[0]["seen_comments"]; !ok {
			t.Error("missing seen_comments")
		}
	})
	t.Run("version two owner and counters", func(t *testing.T) {
		dir := t.TempDir()
		s := sampleSub(1)
		s.Access, s.CommentsLast, s.SeenComments = "owner", 9, 4
		if err := SaveSubs(dir, []Subscription{s}); err != nil {
			t.Fatal("owner state was rejected")
		}
		got, err := LoadSubs(dir)
		if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], s) {
			t.Fatal("version two did not round trip")
		}
	})
}

func TestSubsVersionTwoNeedsBothValidCounters(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"missing comments_last", func(m map[string]any) { delete(m, "comments_last") }},
		{"missing seen_comments", func(m map[string]any) { delete(m, "seen_comments") }},
		{"null comments_last", func(m map[string]any) { m["comments_last"] = nil }},
		{"null seen_comments", func(m map[string]any) { m["seen_comments"] = nil }},
		{"negative comments_last", func(m map[string]any) { m["comments_last"] = -1 }},
		{"fractional seen_comments", func(m map[string]any) { m["seen_comments"] = 1.5 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := subObject(sampleSub(1))
			obj["comments_last"], obj["seen_comments"] = 0, 0
			tc.edit(obj)
			raw, err := json.Marshal(map[string]any{"v": 2, "subscriptions": []any{obj}})
			if err != nil {
				t.Fatal("cannot encode state")
			}
			if _, err := parseSubs(raw); !errors.Is(err, ErrSubsDamaged) {
				t.Error("damaged version two accepted")
			}
		})
	}
}
