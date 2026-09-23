package app

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentAssetsAreNeutralTextBadges(t *testing.T) {
	want := map[string]struct {
		badge string
		name  string
	}{
		"claude.svg":   {"Cld", "Claude"},
		"openai.svg":   {"Cdx", "Codex"},
		"grok.svg":     {"Grk", "Grok"},
		"meta.svg":     {"Mus", "Muse"},
		"pi.svg":       {"Pi", "pi"},
		"opencode.svg": {"OC", "OpenCode"},
		"cursor.svg":   {"Cur", "Cursor"},
		"deepseek.svg": {"Dsh", "dsh"},
	}
	for file, expected := range want {
		t.Run(file, func(t *testing.T) {
			body, err := webFS.ReadFile("web/assets/agents/" + file)
			if err != nil {
				t.Fatal(err)
			}
			svg := string(body)
			for _, fragment := range []string{
				`viewBox="0 0 64 64"`, `<rect`, `rx="12"`, `fill="none"`,
				`stroke="currentColor"`, `fill="currentColor"`,
				`<title id="title">` + expected.name + `</title>`,
				`>` + expected.badge + `</text>`,
			} {
				if !strings.Contains(svg, fragment) {
					t.Errorf("%s missing neutral-badge fragment %q", file, fragment)
				}
			}
			lower := strings.ToLower(svg)
			for _, forbidden := range []string{"<script", "<image", "<foreignobject", "<use", "href=", "url(", "onload=", "onclick="} {
				if strings.Contains(lower, forbidden) {
					t.Errorf("%s contains executable or external-resource fragment %q", file, forbidden)
				}
			}
		})
	}
}

func TestSupersededAgentArtworkLicensesAreRemoved(t *testing.T) {
	rec := httptest.NewRecorder()
	handleLicenses(rec, httptest.NewRequest("GET", "/api/licenses", nil))
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"lobeicons", "opencode"} {
		if _, ok := got[key]; ok {
			t.Errorf("superseded agent-artwork license %q is still served", key)
		}
	}
	for _, key := range []string{"scimux", "fontawesome", "qrcodegen"} {
		if got[key] == "" {
			t.Errorf("remaining license %q disappeared", key)
		}
	}
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{`data-lic="lobeicons"`, `data-lic="opencode"`} {
		if strings.Contains(string(index), stale) {
			t.Errorf("About sheet still registers %s", stale)
		}
	}
}
