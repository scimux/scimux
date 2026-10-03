package app

import (
	"os"
	"path/filepath"
	"testing"
)

func writeVibeWheelMetadata(t *testing.T, root, dirVersion, body string) {
	t.Helper()
	dir := filepath.Join(root, "lib", "python3.12", "site-packages", "mistral_vibe-"+dirVersion+".dist-info")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "METADATA"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVibeInventoryReadsAdjacentWheelVersionWithoutLaunching(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(root, "launched")
	writeScript(t, binDir, "vibe-acp", "printf 'launched\\n' >> "+quoteForScript(record)+"\n")
	writeVibeWheelMetadata(t, root, "2.25.0", "Metadata-Version: 2.4\nName: mistral-vibe\nVersion: 2.25.0\n\n")
	pathDir := t.TempDir()
	if err := os.Symlink(filepath.Join(binDir, "vibe-acp"), filepath.Join(pathDir, "vibe-acp")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)

	rows := probeHarnessVersions([]harness{{name: "vibe", bin: "vibe-acp"}})
	if len(rows) != 1 || rows[0].Installed != "2.25.0" || !rows[0].Present || !rows[0].Launchable {
		t.Fatalf("Vibe inventory = %+v, want installed version 2.25.0", rows)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("version discovery launched vibe-acp: %v", err)
	}
}

func TestVibeInstalledVersionRejectsMissingOrUntrustedMetadata(t *testing.T) {
	cases := []struct {
		name       string
		dirVersion string
		body       string
		extra      bool
	}{
		{name: "no metadata"},
		{name: "other package", dirVersion: "2.25.0", body: "Name: another-package\nVersion: 2.25.0\n\n"},
		{name: "directory disagrees", dirVersion: "2.25.0", body: "Name: mistral-vibe\nVersion: 2.26.0\n\n"},
		{name: "malformed version", dirVersion: "bad", body: "Name: mistral-vibe\nVersion: bad\n\n"},
		{name: "conflicting versions", dirVersion: "2.25.0", body: "Name: mistral-vibe\nVersion: 2.25.0\n\n", extra: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			binDir := filepath.Join(root, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeScript(t, binDir, "vibe-acp", "exit 0\n")
			if tc.dirVersion != "" {
				writeVibeWheelMetadata(t, root, tc.dirVersion, tc.body)
			}
			if tc.extra {
				writeVibeWheelMetadata(t, root, "2.26.0", "Name: mistral-vibe\nVersion: 2.26.0\n\n")
			}
			if got := vibeInstalledVersion(filepath.Join(binDir, "vibe-acp")); got != "" {
				t.Fatalf("version = %q, want unknown", got)
			}
		})
	}
}
