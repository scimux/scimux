package app

// P4 — the pairing UI's presence in the shipped page.
//
// The browser half of pairing is specified by the Node suites; what those
// cannot see is whether the page the binary actually serves contains the
// elements the feature reaches for, and whether the sole browser entry
// imports it at all. A feature that is perfectly tested and never
// constructed is exactly the hole web/test/reachability.test.js exists to
// close, and this is its laptop-side mirror for the markup.

import (
	"io/fs"
	"strings"
	"testing"
)

// pairSlots are the ids web/js/pairing-ui.js queries. It degrades quietly on
// a missing slot (every write is guarded), which is right for a browser and
// wrong for a build: a typo'd id would ship as a blank sheet with no error.
var pairSlots = []string{
	"pairsheet",
	"pair_title",
	"pair_body",
	"pair_qr",
	"pair_sas",
	"pair_actions",
	"pair_live",
	"pair_x",
	// F1: the unlink control, which is guarded the same way and would
	// therefore ship as a Remote access section with no way out.
	"m_unlink",
	"m_unlink_note",
}

func readWebFile(t *testing.T, path string) string {
	t.Helper()
	b, err := fs.ReadFile(webFS, path)
	if err != nil {
		t.Fatalf("read embedded %s: %v", path, err)
	}
	return string(b)
}

// TestPairingSheetSlotsExist pins every element the adapter owns.
func TestPairingSheetSlotsExist(t *testing.T) {
	html := readWebFile(t, "web/index.html")
	for _, id := range pairSlots {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html has no id=%q; pairing-ui.js writes into it and "+
				"degrades silently when it is absent, so this ships as a blank sheet", id)
		}
	}
}

// TestRemoteAccessIsTheFirstMenuSection pins placement, not decoration.
// Pairing hands out SSH-equivalent authority on this laptop, and the list of
// who currently holds it is the first thing a burger menu should show —
// below the version check and the licence notices it is a setting nobody
// finds until they are looking for it, which is too late.
func TestRemoteAccessIsTheFirstMenuSection(t *testing.T) {
	html := readWebFile(t, "web/index.html")
	menu := html[indexAfter(t, html, `<div class="sheet" id="menu">`):]
	first := strings.Index(menu, `<div class="sect">`)
	if first < 0 {
		t.Fatal(`#menu contains no <div class="sect">`)
	}
	pair := strings.Index(menu, `id="m_pair"`)
	if pair < 0 {
		t.Fatal(`#menu has no id="m_pair"; there is no way into the pairing flow`)
	}
	devices := strings.Index(menu, `id="m_devices"`)
	if devices < 0 {
		t.Fatal(`#menu has no id="m_devices"; a grant nobody can see cannot be revoked`)
	}
	second := strings.Index(menu[first+1:], `<div class="sect">`)
	if second < 0 {
		t.Fatal("#menu has only one section; Remote access cannot be shown to be first")
	}
	second += first + 1
	if pair > second || devices > second {
		t.Errorf("m_pair (%d) / m_devices (%d) are not inside the FIRST menu section "+
			"(which spans %d..%d): the list of devices holding SSH-equivalent "+
			"authority is below the version check", pair, devices, first, second)
	}
}

// TestPairingFeatureIsConstructedByTheBrowserEntry is the reachability
// assertion for the laptop side. web/test/reachability.test.js can prove a
// test imports served code; only the entry module can prove the served code
// ever runs.
func TestPairingFeatureIsConstructedByTheBrowserEntry(t *testing.T) {
	app := readWebFile(t, "web/js/app.js")
	for _, want := range []string{
		`from "./pairing-ui.js"`,
		"createPairingFeature",
		"createDeviceList",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("web/js/app.js does not mention %q: the pairing feature is "+
				"served, locked and fully tested, and never constructed", want)
		}
	}
}

func indexAfter(t *testing.T, s, sub string) int {
	t.Helper()
	i := strings.Index(s, sub)
	if i < 0 {
		t.Fatalf("index.html does not contain %q", sub)
	}
	return i + len(sub)
}
