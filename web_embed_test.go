package main

import (
	"bytes"
	"io/fs"
	"os"
	"slices"
	"testing"
)

func TestWebFSEmbeddedProductionFiles(t *testing.T) {
	expected := []string{
		"web/assets/agents/claude.svg",
		"web/assets/agents/openai.svg",
		"web/assets/agents/opencode.svg",
		"web/assets/agents/pi.svg",
		"web/index.html",
	}

	var got []string
	if err := fs.WalkDir(webFS, "web", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			got = append(got, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk embedded web files: %v", err)
	}

	if !slices.Equal(got, expected) {
		t.Fatalf("embedded regular files:\n got: %v\nwant: %v", got, expected)
	}

	for _, path := range expected {
		embedded, err := webFS.ReadFile(path)
		if err != nil {
			t.Fatalf("read embedded %s: %v", path, err)
		}
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read source %s: %v", path, err)
		}
		if !bytes.Equal(embedded, source) {
			t.Fatalf("embedded bytes for %s differ from checked-out source: embedded=%d bytes source=%d bytes", path, len(embedded), len(source))
		}
	}
}
