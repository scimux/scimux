package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/team"
)

func TestReplayIdentityRestoreSyntheticLifecycle(t *testing.T) {
	root := t.TempDir()
	id := team.CopyID{Server: strings.Repeat("a", 32), Owner: strings.Repeat("b", 32), Chat: strings.Repeat("c", 16), Generation: strings.Repeat("d", 16)}
	first := strings.Repeat("1", 32)
	second := strings.Repeat("2", 32)
	if err := restoreReplayIdentity(root, id, first); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := team.ReadPeopleCopyInstance(root, id)
		if err != nil || got != first {
			t.Fatal("restored identity not stable")
		}
	}
	if err := restoreReplayIdentity(root, id, first); err != nil {
		t.Fatal("exact retry failed")
	}
	if err := restoreReplayIdentity(root, id, second); err == nil {
		t.Fatal("identity replaced")
	}
	if err := team.RemoveCopy(root, id); err != nil {
		t.Fatal(err)
	}
	if err := restoreReplayIdentity(root, id, second); err != nil {
		t.Fatal(err)
	}
	got, err := team.ReadPeopleCopyInstance(root, id)
	if err != nil || got != second || got == first {
		t.Fatal("recreated identity invalid")
	}
	path := filepath.Join(root, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-identity", "0000000000000000.json")
	if err := os.WriteFile(path, []byte("synthetic damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restoreReplayIdentity(root, id, second); err == nil {
		t.Fatal("damaged identity replaced")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "synthetic damaged" {
		t.Fatal("damaged data changed")
	}
	if err := restoreReplayIdentity(t.TempDir(), id, "not-valid"); err == nil {
		t.Fatal("invalid identity accepted")
	}
	bad := id
	bad.Server = "invalid"
	if err := restoreReplayIdentity(t.TempDir(), bad, first); err == nil {
		t.Fatal("invalid copy accepted")
	}
}

func TestReplayIdentityRejectsInvalidCopyWithoutWrites(t *testing.T) {
	valid := team.CopyID{Server: strings.Repeat("a", 32), Owner: strings.Repeat("b", 32), Chat: strings.Repeat("c", 16), Generation: strings.Repeat("d", 16)}
	cases := []struct {
		name   string
		change func(*team.CopyID)
	}{
		{"server", func(id *team.CopyID) { id.Server = "invalid" }},
		{"owner", func(id *team.CopyID) { id.Owner = "invalid" }},
		{"chat", func(id *team.CopyID) { id.Chat = "invalid" }},
		{"generation", func(id *team.CopyID) { id.Generation = "invalid" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			id := valid
			tc.change(&id)
			if err := restoreReplayIdentity(root, id, strings.Repeat("1", 32)); err == nil {
				t.Fatal("invalid copy accepted")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatal("invalid copy changed storage")
			}
		})
	}
}
