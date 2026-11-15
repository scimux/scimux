package app

// P4 — the pairing UI's presence in the shipped page.
//
// The browser half of pairing is specified by the Node suites; what those
// cannot see is whether the page the binary actually serves contains the
// elements the feature reaches for, and whether the sole browser entry
// imports it at all. A feature that is perfectly tested and never
// constructed is exactly the hole web/test/reachability.test.js exists to
// close, and this is its computer-side mirror for the markup.

import (
	"io/fs"
	"strings"
	"testing"
)

// pairSlots are the ids web/js/pairing-ui.js queries. It degrades quietly on
// a missing slot (every write is guarded), which is right for a browser and
// wrong for a build: a typo'd id would ship as a blank sheet with no error.
var pairSlots = []string{
	"m_remote",
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
	// The note that stands in for a hidden "Pair a device": a missing slot
	// would ship as a button that silently is not there.
	"m_pair_note",
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

// TestMenuSectionOrderAndInitialStates pins the agreed native disclosure
// order. Remote remains hidden until the server reports availability.
func TestMenuSectionOrderAndInitialStates(t *testing.T) {
	html := readWebFile(t, "web/index.html")
	menu := html[indexAfter(t, html, `<div class="sheet" id="menu">`):]
	menu = menu[:indexAfter(t, menu, `<div class="assetpreview"`)]
	want := []struct {
		id      string
		summary string
		open    bool
		hidden  bool
	}{
		{"m_about", "ABOUT", true, false},
		{"m_remote", "REMOTE", false, true},
		{"m_sharing", "SHARING", false, true},
		{"m_harness_section", "HARNESSES", false, false},
		{"m_attachment_settings", "PREFERENCES", false, false},
	}
	if got := strings.Count(menu, `class="sect menu-section"`); got != len(want) {
		t.Fatalf("menu sections = %d, want %d", got, len(want))
	}
	previous := -1
	for _, section := range want {
		i := strings.Index(menu, `id="`+section.id+`"`)
		if i <= previous {
			t.Fatalf("section %s missing or out of order", section.summary)
		}
		previous = i
		start := strings.LastIndex(menu[:i], "<")
		end := i + strings.Index(menu[i:], ">") + 1
		tag := menu[start:end]
		if !strings.HasPrefix(tag, "<details ") {
			t.Errorf("%s must be a native details section: %s", section.summary, tag)
		}
		fields := strings.Fields(strings.TrimSuffix(tag, ">"))
		has := func(attribute string) bool {
			for _, field := range fields {
				if field == attribute {
					return true
				}
			}
			return false
		}
		if has("open") != section.open || has("hidden") != section.hidden {
			t.Errorf("%s initial attributes = %s, want open=%t hidden=%t", section.summary, tag, section.open, section.hidden)
		}
		if !strings.HasPrefix(strings.TrimSpace(menu[end:]), "<summary>"+section.summary+"</summary>") {
			t.Errorf("%s must begin with its native summary", section.summary)
		}
	}
	remote := strings.Index(menu, `id="m_remote"`)
	harnesses := strings.Index(menu, `id="m_harness_section"`)
	for _, id := range []string{"m_pair", "m_devices"} {
		i := strings.Index(menu, `id="`+id+`"`)
		if i < remote || i > harnesses {
			t.Errorf("%s must remain inside REMOTE", id)
		}
	}
}

// TestSharingSectionSlotsAndConstruction pins the Sharing section the
// browser module writes into, and that the composition root constructs it.
func TestSharingSectionSlotsAndConstruction(t *testing.T) {
	html := readWebFile(t, "web/index.html")
	start := strings.Index(html, `id="m_sharing"`)
	endRel := -1
	if start < 0 {
		t.Error(`index.html has no id="m_sharing"`)
	} else {
		endRel = strings.Index(html[start:], `id="m_harness_section"`)
		if endRel < 0 {
			t.Error("SHARING is not before HARNESSES")
		}
	}
	if start >= 0 && endRel >= 0 {
		section := html[start : start+endRel]
		for _, id := range []string{
			"m_team_servers",
			"m_team_add",
			"m_team_form",
			"m_team_link",
			"m_team_name",
			"m_team_join",
			"m_team_cancel",
			"m_team_note",
			"m_team_live",
		} {
			if !strings.Contains(section, `id="`+id+`"`) {
				t.Errorf("SHARING section has no id=%q", id)
			}
		}
	}
	app := readWebFile(t, "web/js/app.js")
	if !strings.Contains(app, "createSharingSection") {
		t.Error("web/js/app.js does not name createSharingSection")
	}
}

func TestRemoteAccessSectionShipsHidden(t *testing.T) {
	html := readWebFile(t, "web/index.html")
	i := strings.Index(html, `id="m_remote"`)
	if i < 0 {
		t.Fatal(`index.html has no id="m_remote"`)
	}
	tag := html[strings.LastIndex(html[:i], "<"):]
	tag = tag[:strings.Index(tag, ">")+1]
	if !strings.Contains(tag, "hidden") {
		t.Errorf("the experimental Remote access section ships visible (%s)", tag)
	}
}

// TestPairingFeatureIsConstructedByTheBrowserEntry is the reachability
// assertion for the computer side. web/test/reachability.test.js can prove a
// test imports served code; only the entry module can prove the served code
// ever runs.
func TestPairingFeatureIsConstructedByTheBrowserEntry(t *testing.T) {
	app := readWebFile(t, "web/js/app.js")
	for _, want := range []string{
		`from "./pairing-ui.js"`,
		"createPairingFeature",
		"createDeviceList",
		"createPairControl",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("web/js/app.js does not mention %q: the pairing feature is "+
				"served, locked and fully tested, and never constructed", want)
		}
	}
}

// TestPairButtonShipsHidden pins the fail-closed half of the gate.
//
// Minting is entirely local — a code, a locally minted RID, a link off the
// configured origin — so an unenrolled computer produces a code and a QR for
// a meeting that can never happen, and the human reads the expiry as a
// timing problem. The button is therefore markup that starts hidden and is
// revealed only by a status read that says pairing could complete
// (createPairControl). Shipping it visible puts the hole back, and the
// browser suite cannot see it: its DOM stub starts every element visible.
func TestPairButtonShipsHidden(t *testing.T) {
	html := readWebFile(t, "web/index.html")
	i := strings.Index(html, `id="m_pair"`)
	if i < 0 {
		t.Fatal(`index.html has no id="m_pair"`)
	}
	tag := html[strings.LastIndex(html[:i], "<"):]
	tag = tag[:strings.Index(tag, ">")+1]
	if !strings.Contains(tag, "hidden") {
		t.Errorf("the pair button ships visible (%s): a computer with no enrollment "+
			"would offer a pairing code that nothing can ever meet", tag)
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

// TestPairingDigitsAreTypedNotShown pins FR-38's direction. The six digits
// are read on the device and typed on the computer; a computer that
// displays them reduces the confirmation to a button, and a button is what
// an attacker taps when the real device is not in the room to disagree with
// it. The browser suite proves the state machine never discloses the
// digits. What it cannot see is whether the shipped page carries a field to
// type them into -- without one the flow has no way forward at all.
func TestPairingDigitsAreTypedNotShown(t *testing.T) {
	html := readWebFile(t, "web/index.html")
	i := strings.Index(html, `id="pair_sas"`)
	if i < 0 {
		t.Fatal(`index.html has no id="pair_sas"`)
	}
	tag := html[strings.LastIndex(html[:i], "<"):]
	if !strings.HasPrefix(tag, "<input") {
		t.Errorf("pair_sas is not an <input> but %.24q; the digits are typed here, never shown", tag)
	}
}

func TestPairingFeatureIsGivenTheClipboard(t *testing.T) {
	app := readWebFile(t, "web/js/app.js")
	if !strings.Contains(app, `createPairingFeature({ api, doc: document, copy: copyText })`) {
		t.Error("web/js/app.js does not give the pairing feature copyText: the Copy link action would ship permanently failing, because the adapter treats a missing copy as a refused clipboard")
	}
}

// TestShareSheetSlotsAndConstruction pins the share button, the status line
// and the share sheet, and that the composition root constructs the feature.
func TestShareSheetSlotsAndConstruction(t *testing.T) {
	html := readWebFile(t, "web/index.html")
	info := strings.Index(html, `id="infobtn"`)
	share := strings.Index(html, `id="sharebtn"`)
	if info < 0 || share < info {
		t.Error("share button must follow the info button in the chat header")
	}
	if !strings.Contains(html, `<button class="headtoggle" id="sharebtn" aria-label="share chat" title="Share chat" hidden></button>`) {
		t.Error("share button markup drifted")
	}
	toprow := strings.Index(html, `class="toprow"`)
	status := strings.Index(html, `id="sharestatus"`)
	mission := strings.Index(html, `id="chatmission"`)
	if toprow < 0 || status < toprow || mission < 0 || status > mission {
		t.Error("share status must sit between the header row and the mission")
	}
	if !strings.Contains(html, `<button id="sharestatus" hidden></button>`) {
		t.Error("share status markup drifted")
	}
	sendto := strings.Index(html, `id="sendto"`)
	sheet := strings.Index(html, `id="sharesheet"`)
	fare := strings.Index(html, `id="faresheet"`)
	if sendto < 0 || sheet < sendto || fare < 0 || fare < sheet {
		t.Error("share sheet must follow send-to and precede the fare sheet")
	}
	for _, id := range []string{"sh_body", "sh_note", "sh_apply", "sh_stop", "sh_live"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("share sheet has no id=%q", id)
		}
	}
	app := readWebFile(t, "web/js/app.js")
	if !strings.Contains(app, "createShareFeature") {
		t.Error("web/js/app.js does not name createShareFeature")
	}
}

func TestSharedViewSlotsAndConstruction(t *testing.T) {
	html := readWebFile(t, "web/index.html")
	for _, id := range []string{"sharedview", "sv_scrim", "sv_panel", "sv_head", "sv_back", "sv_title", "sv_from", "sv_state", "sv_body", "sv_more", "sv_note"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html has no id=%q", id)
		}
	}
	if !strings.Contains(readWebFile(t, "web/js/app.js"), "createGuestFeature") {
		t.Error("web/js/app.js does not name createGuestFeature")
	}
}
