package main

import (
	"bytes"
	"io/fs"
	"os"
	"slices"
	"testing"
)

// productionJSModules is the exact Packet 6F inventory of flat production
// JavaScript modules under web/js. Keep in sync with the sole module entry's
// imports in web/index.html and with jsFileHandler serving.
var productionJSModules = []string{
	"web/js/api.js",
	"web/js/format.js",
	"web/js/lanes.js",
	"web/js/map-model.js",
	"web/js/navigation.js",
	"web/js/state.js",
}

func TestWebFSEmbeddedProductionFiles(t *testing.T) {
	expected := []string{
		"web/assets/agents/claude.svg",
		"web/assets/agents/openai.svg",
		"web/assets/agents/opencode.svg",
		"web/assets/agents/pi.svg",
		"web/css/accessibility.css",
		"web/css/base.css",
		"web/css/cards.css",
		"web/css/chat.css",
		"web/css/layout.css",
		"web/css/map.css",
		"web/css/notes.css",
		"web/css/sheets.css",
		"web/css/tokens.css",
		"web/index.html",
	}
	expected = append(expected, productionJSModules...)
	slices.Sort(expected)

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
	slices.Sort(got)

	if !slices.Equal(got, expected) {
		t.Fatalf("embedded regular files:\n got: %v\nwant: %v", got, expected)
	}

	// web/test and package metadata must never enter the production embed.
	for _, forbidden := range []string{
		"web/package.json",
		"web/test/smoke.test.js",
		"web/test/format.test.js",
		"web/test/api.test.js",
	} {
		if _, err := webFS.ReadFile(forbidden); err == nil {
			t.Fatalf("forbidden path %s is embedded", forbidden)
		}
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
