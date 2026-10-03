package app

import (
	"encoding/json"
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentAssetsAreAccentTextBadges(t *testing.T) {
	want := map[string]struct {
		badge  string
		name   string
		accent string
	}{
		"claude.svg":   {"Cld", "Claude", "#fb8c63"},
		"openai.svg":   {"Cdx", "Codex", "#79e9c4"},
		"cursor.svg":   {"Cur", "Cursor", "#dfe1e5"},
		"deepseek.svg": {"Dsh", "DeepSeek Harness", "#5c9df8"},
		"grok.svg":     {"Grk", "Grok", "#66d9f6"},
		"mistral.svg":  {"Mst", "Mistral Vibe", "#fa616b"},
		"meta.svg":     {"Mus", "Meta Muse", "#b979ed"},
		"opencode.svg": {"OpC", "OpenCode", "#fac42e"},
		"pi.svg":       {"Pi.", "Pi.dev", "#f9aae0"},
	}
	for file, expected := range want {
		t.Run(file, func(t *testing.T) {
			body, err := webFS.ReadFile("web/assets/agents/" + file)
			if err != nil {
				t.Fatal(err)
			}
			svg := string(body)
			var root struct{ XMLName xml.Name }
			if err := xml.Unmarshal(body, &root); err != nil || root.XMLName.Local != "svg" {
				t.Fatalf("invalid SVG: %v", err)
			}
			for _, fragment := range []string{
				`viewBox="0 0 64 64"`, `<rect`, `rx="7"`, `<linearGradient`,
				`fill="url(#bg)"`, `stop-color="` + expected.accent + `"`,
				`transform="rotate(-7 32 32)"`,
				`<title id="title">` + expected.name + `</title>`,
				`>` + expected.badge + `</text>`,
			} {
				if !strings.Contains(svg, fragment) {
					t.Errorf("%s missing accent-badge fragment %q", file, fragment)
				}
			}
			lower := strings.ToLower(svg)
			lower = strings.Replace(lower, `xmlns="http://www.w3.org/2000/svg"`, "", 1)
			for _, forbidden := range []string{"<script", "<image", "<foreignobject", "<use", "href=", "http:", "https:", "data:", "onload=", "onclick="} {
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
