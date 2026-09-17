package app

import (
	"strings"
	"testing"
)

// The tab icon is the approved Orbit mark from the scimux logo package
// (orbit-logo-package-v4 / favicon.svg), inlined as a data URI rather than
// served as a file. Two reasons it must stay inline, both load-bearing:
// web/js/bootstrap.js rewrites only rel=stylesheet links to channel blob
// URLs, so a same-origin href would 404 against the rendezvous origin on a
// remote session; and /favicon.ico is pinned 404 by the frozen static
// characterization suite. remoteCSP allows img-src data: for exactly this.
//
// The assertions below are the mark's geometry, not a byte compare: the
// three-part Orbit construction is what a hand-redrawn approximation gets
// wrong, and each of these fragments is one part of it.
func TestLocalFaviconIsApprovedOrbitMark(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	index := getCharacterization(t, h, "/")
	body := index.Body.String()

	for _, want := range []struct{ frag, why string }{
		{`<link rel="icon" type="image/svg+xml" href='data:image/svg+xml,`, "inline data URI"},
		{`viewBox="0 0 1000 1000"`, "the package's own viewBox, so a revised master drops in"},
		{`stroke="%230E7C86" stroke-width="92"`, "petrol ring arc"},
		{`stroke="%23BFEAF0" stroke-width="92"`, "pale ring arc: the ring is two-tone, not one gradient"},
		{`d="M 500.000,171.000 A 329,329 0 1 1 267.362,732.638"`, "petrol sweeps 225 degrees, 12 o'clock to 7:30"},
		{`d="M 267.362,732.638 A 329,329 0 0 1 500.000,171.000"`, "pale closes the ring: no gap"},
		{`<circle cx="500" cy="500" r="179"`, "centre disc"},
		{`<circle cx="500.000" cy="171.000" r="71"`, "the 12 o'clock station node is its own circle"},
		{`<rect x="452.0" y="590" width="96" height="329" rx="48.0"`, "the stem hangs below the disc"},
	} {
		if !strings.Contains(body, want.frag) {
			t.Errorf("favicon missing %s: %q", want.why, want.frag)
		}
	}

	// Fingerprints of the redrawn approximation this replaced: a notched
	// single-gradient ring and a stem spiked through the whole mark.
	for _, stale := range []string{
		`viewBox="0 0 512 512"`,
		`d="M137 375a168 168 0 1 0-49-119"`,
		`d="M256 88v356"`,
		`%231599A4`,
		`%23B1E6EC`,
		`M18 12v40M32 12v40M46 12v40`,
	} {
		if strings.Contains(body, stale) {
			t.Errorf("local index still carries superseded favicon artwork %q", stale)
		}
	}
}
