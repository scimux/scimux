package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeModelMarkerFromStatusLine(t *testing.T) {
	body := []byte(`{"model":{"id":"claude-opus-9-2","display_name":"Opus 9.2"},"cwd":"/x"}`)
	m, err := parseClaudeStatusLineModel(body, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.ID != "claude-opus-9-2" || m.DisplayName != "Opus 9.2" {
		t.Fatalf("got %+v", m)
	}
	if m.At == "" {
		t.Fatal("marker must carry an observation time")
	}
}

// A status line renders on payloads that carry no model block at all; that is
// not a model reading and must not be persisted as one.
func TestClaudeModelMarkerNeedsAModel(t *testing.T) {
	for _, body := range []string{
		`{"cwd":"/x"}`,
		`{"model":{}}`,
		`{"model":{"id":"","display_name":"Opus 9.2"}}`,
		`{"model":null}`,
		`not json`,
	} {
		if _, err := parseClaudeStatusLineModel([]byte(body), time.Now()); err == nil {
			t.Fatalf("expected rejection for %s", body)
		}
	}
}

// The CLI signals an id it does not know by echoing it as the display name.
// That is the only evidence we get that an alias expanded to nothing real.
func TestClaudeModelUnrecognizedWhenDisplayEchoesID(t *testing.T) {
	m := claudeModelMarker{ID: "claude-4-6-opus", DisplayName: "claude-4-6-opus"}
	if _, _, ok := claudeModelResolved(m); ok {
		t.Fatal("an echoed display name means the model is not in the catalog")
	}
}

// claude-4-6-opus is what `--model opus` expands to on 2.1.267: the family
// sits after the version, so it is not a usable id even though it is
// version-shaped. Rejecting it is the whole reason scimux resolves ids.
func TestClaudeModelResolvedFamilies(t *testing.T) {
	cases := []struct {
		id, display, family string
		ok                  bool
	}{
		{"claude-opus-9-2", "Opus 9.2", "opus", true},
		{"claude-sonnet-9-7", "Sonnet 9.7", "sonnet", true},
		{"claude-haiku-9-1-20000101", "Haiku 9.1", "haiku", true},
		{"claude-fable-9-8", "Fable 9.4", "fable", true},
		{"claude-4-6-opus", "Opus", "", false},
		{"claude-opus", "Opus", "", false},
		{"", "Opus 9.2", "", false},
		{"gpt-5", "GPT", "", false},
	}
	for _, c := range cases {
		fam, id, ok := claudeModelResolved(claudeModelMarker{ID: c.id, DisplayName: c.display})
		if ok != c.ok {
			t.Fatalf("%q: ok=%v want %v", c.id, ok, c.ok)
		}
		if ok && (fam != c.family || id != c.id) {
			t.Fatalf("%q: got %q/%q want %q/%q", c.id, fam, id, c.family, c.id)
		}
	}
}

func TestClaudeModelIDFromDisplay(t *testing.T) {
	cases := map[string]string{
		"Opus 9.2":   "claude-opus-9-2",
		"Sonnet 9.7": "claude-sonnet-9-7",
		"Haiku 9.1":  "claude-haiku-9-1",
		"Fable 9.8":  "claude-fable-9-8",
		"Opus":       "",
		"Opus five":  "",
		"":           "",
		"Gemini 3":   "",
	}
	for display, want := range cases {
		if got := claudeModelIDFromDisplay(display); got != want {
			t.Fatalf("%q: got %q want %q", display, got, want)
		}
	}
}

// Independently authored menu; invented versions exercise the parser grammar.
const claudeModelPickerPane = `
Synthetic model menu
    1. Default (recommended)  Sonnet 9.7 · fixture default
    2. Sonnet                 Sonnet 9.7 · fixture choice
    3. Fable                  Fable 9.4 · fixture choice
    4. Opus                   Opus 9.2 · fixture choice
  ❯ 5. Haiku ✔                Haiku 9.1 · fixture selection
    6. Fable 9.8 (disabled)    fixture unavailable choice
`

func TestParseClaudeModelPicker(t *testing.T) {
	got := parseClaudeModelPicker(claudeModelPickerPane)
	want := map[string]string{
		"sonnet": "claude-sonnet-9-7",
		"fable":  "claude-fable-9-4",
		"opus":   "claude-opus-9-2",
		"haiku":  "claude-haiku-9-1",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for fam, id := range want {
		if got[fam] != id {
			t.Fatalf("%s: got %q want %q", fam, got[fam], id)
		}
	}
}

// Row 6 models an unavailable choice and must never override an enabled row.
// Adopting it would hand the launcher an id that fails at the API.
func TestParseClaudeModelPickerSkipsDisabledRows(t *testing.T) {
	got := parseClaudeModelPicker(claudeModelPickerPane)
	if got["fable"] == "claude-fable-9-8" {
		t.Fatal("a (disabled) row must never become the family's id")
	}
}

// Synthetic layouts exercise a wide menu and a wrapped, scrolling menu.
// The hidden Haiku row is intentionally absent from the narrow menu.
const claudeModelPickerWidePane = `
Synthetic wide menu
  ❯ 1. Default (recommended) ✔  Sonnet 9.7 · fixture default
    2. Sonnet                   Sonnet 9.7 · fixture choice
    3. Fable                    Fable 9.8 · fixture description with a deliberately long line
    4. Opus                     Opus 9.2 · fixture choice
    5. Haiku                    Haiku 9.1 · fixture choice
`

const claudeModelPickerNarrowPane = `
Synthetic narrow menu
  ❯ 1. Default (recommended) ✔  Sonnet 9.7 · fixture default
    2. Sonnet                   Sonnet 9.7 · fixture choice
    3. Fable                    Fable 9.8 · fixture description
                                continued on the following line
  ↓ 4. Opus                     Opus 9.2 · fixture choice
     … +1 model
`

// Opus is the family this whole probe exists to name: the alias round cannot
// answer for it (--model opus expands to the mis-ordered claude-4-6-opus), so
// a pane shape the parser cannot read costs exactly the one answer that
// matters.
func TestParseClaudeModelPickerReadsSyntheticLayouts(t *testing.T) {
	for name, pane := range map[string]string{
		"200x50": claudeModelPickerWidePane,
		"narrow": claudeModelPickerNarrowPane,
	} {
		got := parseClaudeModelPicker(pane)
		if got["opus"] != "claude-opus-9-2" {
			t.Fatalf("%s: opus = %q, want claude-opus-9-2 (all rows: %v)", name, got["opus"], got)
		}
		if got["sonnet"] != "claude-sonnet-9-7" {
			t.Fatalf("%s: sonnet = %q (all rows: %v)", name, got["sonnet"], got)
		}
		// A wrapped description must not cost the version on its first line.
		if got["fable"] != "claude-fable-9-8" {
			t.Fatalf("%s: fable = %q (all rows: %v)", name, got["fable"], got)
		}
		// The row the picker itself marks as default is not a family, and
		// reading it would let the user's own default overwrite another
		// family's id.
		wantRows := 4
		if name == "narrow" {
			wantRows = 3
			if _, present := got["haiku"]; present {
				t.Fatalf("%s: invented a hidden row: %v", name, got)
			}
		} else if got["haiku"] != "claude-haiku-9-1" {
			t.Fatalf("%s: missing visible Haiku row: %v", name, got)
		}
		if len(got) != wantRows {
			t.Fatalf("%s: got %d family rows, want %d: %v", name, len(got), wantRows, got)
		}
	}
}

// Unselectable rows must not become candidates even without an enabled row.
func TestParseClaudeModelPickerRejectsDefaultAndDisabledOnly(t *testing.T) {
	pane := "1. Default (recommended) Sonnet 9.7 · fixture default\n" +
		"2. Fable 9.8 (disabled) · fixture unavailable choice\n"
	if got := parseClaudeModelPicker(pane); len(got) != 0 {
		t.Fatalf("unselectable rows yielded candidates: %v", got)
	}
}

// Defensive like every other CLI-output parser here: an unfamiliar pane is
// silently no answer, never an error and never a partial guess.
func TestParseClaudeModelPickerIgnoresJunk(t *testing.T) {
	for _, pane := range []string{"", "a pane with no picker", "1. 2. 3.", "Opus 9.2"} {
		if got := parseClaudeModelPicker(pane); len(got) != 0 {
			t.Fatalf("%q yielded %v", pane, got)
		}
	}
}

func TestClaudeModelMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := claudeModelMarker{ID: "claude-opus-9-2", DisplayName: "Opus 9.2", At: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := writeClaudePermFile(claudeModelMarkerPath(dir), m); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok := readClaudeModelMarker(dir)
	if !ok || got.ID != m.ID || got.DisplayName != m.DisplayName {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	if filepath.Base(claudeModelMarkerPath(dir)) != claudeModelMarkerName {
		t.Fatal("marker must live under its documented name")
	}
}

func TestReadClaudeModelMarkerRejectsJunk(t *testing.T) {
	dir := t.TempDir()
	if _, ok := readClaudeModelMarker(dir); ok {
		t.Fatal("absent marker must not read as present")
	}
	if err := os.WriteFile(filepath.Join(dir, claudeModelMarkerName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readClaudeModelMarker(dir); ok {
		t.Fatal("unparseable marker must not read as present")
	}
	if err := os.WriteFile(filepath.Join(dir, claudeModelMarkerName), []byte(`{"id":"","display_name":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readClaudeModelMarker(dir); ok {
		t.Fatal("a marker with no id is not a reading")
	}
}

// The model block and the quota block are independent readings of the same
// payload. rate_limits appears only after the session's first API response,
// and the model probe never makes one, so a model marker must be written on a
// payload that carries no quota at all.
func TestStatusLineWritesModelMarkerWithoutQuota(t *testing.T) {
	dir := t.TempDir()
	var out, errOut strings.Builder
	body := `{"model":{"id":"claude-sonnet-9-7","display_name":"Sonnet 9.7"},"cwd":"/x"}`
	err := RunClaudeUsageStatusLine(dir, strings.NewReader(body), &out, &errOut)
	if !errors.Is(err, errClaudeUsageNoWindow) {
		t.Fatalf("quota half should still report no window, got %v", err)
	}
	m, ok := readClaudeModelMarker(dir)
	if !ok || m.ID != "claude-sonnet-9-7" || m.DisplayName != "Sonnet 9.7" {
		t.Fatalf("model marker: %+v ok=%v", m, ok)
	}
	if _, ok := readClaudeUsageMarker(dir); ok {
		t.Fatal("a payload with no rate_limits must not produce a usage marker")
	}
	if out.String() != claudeUsageStatusLineText+"\n" {
		t.Fatalf("stdout must stay the fixed literal, got %q", out.String())
	}
}

func TestStatusLineWritesBothMarkers(t *testing.T) {
	dir := t.TempDir()
	var out, errOut strings.Builder
	body := `{"model":{"id":"claude-opus-9-2","display_name":"Opus 9.2"},` +
		`"rate_limits":{"five_hour":{"used_percentage":32,"resets_at":1789029600}}}`
	if err := RunClaudeUsageStatusLine(dir, strings.NewReader(body), &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	if m, ok := readClaudeModelMarker(dir); !ok || m.ID != "claude-opus-9-2" {
		t.Fatalf("model marker: %+v ok=%v", m, ok)
	}
	if _, ok := readClaudeUsageMarker(dir); !ok {
		t.Fatal("usage marker missing")
	}
}

// A model reading must never be able to break the quota path, and a payload
// with no model at all is the ordinary case for a supervised pane.
func TestStatusLineQuotaSurvivesMissingModel(t *testing.T) {
	dir := t.TempDir()
	var out, errOut strings.Builder
	body := `{"rate_limits":{"five_hour":{"used_percentage":7,"resets_at":1789029600}}}`
	if err := RunClaudeUsageStatusLine(dir, strings.NewReader(body), &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, ok := readClaudeUsageMarker(dir); !ok {
		t.Fatal("usage marker missing")
	}
	if _, ok := readClaudeModelMarker(dir); ok {
		t.Fatal("no model block must mean no model marker")
	}
}

func fixedProbe(m map[string]claudeModelMarker) func(context.Context, string) (claudeModelMarker, error) {
	return func(_ context.Context, candidate string) (claudeModelMarker, error) {
		got, ok := m[candidate]
		if !ok {
			return claudeModelMarker{}, errClaudeModelUnreadable
		}
		return got, nil
	}
}

func TestResolveClaudeModelsFromAliases(t *testing.T) {
	pickerCalls := 0
	r := claudeModelResolver{
		probe: fixedProbe(map[string]claudeModelMarker{
			"fable":  {ID: "claude-fable-9-4", DisplayName: "Fable 9.4"},
			"opus":   {ID: "claude-opus-9-2", DisplayName: "Opus 9.2"},
			"sonnet": {ID: "claude-sonnet-9-7", DisplayName: "Sonnet 9.7"},
			"haiku":  {ID: "claude-haiku-9-1-20000101", DisplayName: "Haiku 9.1"},
		}),
		picker: func(context.Context) map[string]string { pickerCalls++; return nil },
	}
	got := r.resolve(context.Background())
	want := map[string]string{
		"fable": "claude-fable-9-4", "opus": "claude-opus-9-2",
		"sonnet": "claude-sonnet-9-7", "haiku": "claude-haiku-9-1-20000101",
	}
	for fam, id := range want {
		if got[fam] != id {
			t.Fatalf("%s: got %q want %q", fam, got[fam], id)
		}
	}
	if pickerCalls != 0 {
		t.Fatal("the picker costs a session; it must not run when every alias answered")
	}
}

// The live case on 2.1.267: --model opus expands to a mis-ordered id, so the
// alias round cannot answer for opus and the picker round must.
func TestResolveClaudeModelsFallsBackToPicker(t *testing.T) {
	pickerCalls := 0
	r := claudeModelResolver{
		probe: fixedProbe(map[string]claudeModelMarker{
			"opus":            {ID: "claude-4-6-opus", DisplayName: "claude-4-6-opus"},
			"sonnet":          {ID: "claude-sonnet-9-7", DisplayName: "Sonnet 9.7"},
			"haiku":           {ID: "claude-haiku-9-1", DisplayName: "Haiku 9.1"},
			"fable":           {ID: "claude-fable-9-4", DisplayName: "Fable 9.4"},
			"claude-opus-9-2": {ID: "claude-opus-9-2", DisplayName: "Opus 9.2"},
		}),
		picker: func(context.Context) map[string]string {
			pickerCalls++
			return map[string]string{"opus": "claude-opus-9-2"}
		},
	}
	got := r.resolve(context.Background())
	if got["opus"] != "claude-opus-9-2" {
		t.Fatalf("opus: got %q", got["opus"])
	}
	if pickerCalls != 1 {
		t.Fatalf("picker ran %d times, want exactly 1", pickerCalls)
	}
}

// A picker candidate is a guess built from a lossy display name, so it is
// validated by the same oracle. One that does not survive is dropped, and the
// family falls back to the bare alias exactly as before this probe existed.
func TestResolveClaudeModelsDropsUnvalidatedCandidate(t *testing.T) {
	r := claudeModelResolver{
		probe: fixedProbe(map[string]claudeModelMarker{
			"opus":          {ID: "claude-4-6-opus", DisplayName: "claude-4-6-opus"},
			"sonnet":        {ID: "claude-sonnet-9-7", DisplayName: "Sonnet 9.7"},
			"claude-opus-9": {ID: "claude-opus-9", DisplayName: "claude-opus-9"},
		}),
		picker: func(context.Context) map[string]string {
			return map[string]string{"opus": "claude-opus-9"}
		},
	}
	got := r.resolve(context.Background())
	if _, ok := got["opus"]; ok {
		t.Fatalf("unvalidated candidate must not be adopted, got %q", got["opus"])
	}
	if got["sonnet"] != "claude-sonnet-9-7" {
		t.Fatal("one family failing must not cost the others")
	}
}

// An alias that resolves into another family would silently relabel a model:
// picking "opus" in scimux would launch Sonnet.
func TestResolveClaudeModelsRejectsCrossFamilyAnswer(t *testing.T) {
	r := claudeModelResolver{
		probe: fixedProbe(map[string]claudeModelMarker{
			"opus": {ID: "claude-sonnet-9-7", DisplayName: "Sonnet 9.7"},
		}),
		picker: func(context.Context) map[string]string { return nil },
	}
	if got := r.resolve(context.Background()); len(got) != 0 {
		t.Fatalf("cross-family answer must be rejected, got %v", got)
	}
}

func TestResolveClaudeModelsSurvivesProbeFailure(t *testing.T) {
	r := claudeModelResolver{
		probe: func(context.Context, string) (claudeModelMarker, error) {
			return claudeModelMarker{}, errClaudeModelUnreadable
		},
		picker: func(context.Context) map[string]string { return nil },
	}
	if got := r.resolve(context.Background()); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// pickerCmd stands in for a Claude TUI that opens its model picker: it renders
// (writes the model marker, which is what "the TUI is up" means to this probe),
// then blocks until something is submitted, then draws the picker. A probe that
// pasted /model before the marker appeared would find its keystrokes discarded,
// so the blocking read is the assertion.
func pickerCmd(t *testing.T, dir string) string {
	t.Helper()
	// The picker runs in a directory of its own (claudeModelPickerDirName), so
	// the stand-in writes its marker and draws its pane there.
	dir = claudeModelPickerDir(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(claudeModelMarker{ID: "claude-sonnet-9-7", DisplayName: "Sonnet 9.7", At: time.Now().UTC().Format(time.RFC3339Nano)})
	pane := filepath.Join(dir, "picker.txt")
	if err := os.WriteFile(pane, []byte(claudeModelPickerPane), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "printf %s " + shellQuote(string(b)) + " > " + shellQuote(claudeModelMarkerPath(dir)) +
		"; read -r x; cat " + shellQuote(pane) + "; sleep 30"
	return "bash --norc -c " + shellQuote(script)
}

func TestCaptureClaudeModelPickerReadsRows(t *testing.T) {
	sv := probeTmux(t)
	opts := probeOpts(t, sv, "")
	opts.Command = pickerCmd(t, opts.Dir)
	got := captureClaudeModelPicker(context.Background(), opts)
	for fam, id := range map[string]string{
		"sonnet": "claude-sonnet-9-7",
		"fable":  "claude-fable-9-4",
		"opus":   "claude-opus-9-2",
		"haiku":  "claude-haiku-9-1",
	} {
		if got[fam] != id {
			t.Fatalf("%s: got %q want %q (pane rows: %v)", fam, got[fam], id, got)
		}
	}
	for _, s := range sv.Sessions() {
		if strings.HasPrefix(s, "scimux-") {
			t.Fatalf("picker session %q outlived the capture", s)
		}
	}
}

// A pane that never draws a picker is no answer, never an error: the resolver
// simply keeps the families it could not name and falls back to bare aliases.
func TestCaptureClaudeModelPickerGivesUpQuietly(t *testing.T) {
	sv := probeTmux(t)
	opts := probeOpts(t, sv, "bash --norc -c "+shellQuote("sleep 30"))
	opts.Timeout = 1500 * time.Millisecond
	if got := captureClaudeModelPicker(context.Background(), opts); len(got) != 0 {
		t.Fatalf("a pane with no picker answered %v", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(sv.Sessions()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("picker session survived: %v", sv.Sessions())
}

// Readiness here is a file's *existence*, where a candidate probe validates a
// marker's contents, so the picker is the one gate a marker written by another
// session can open. Measured on 2.1.267: a candidate probe's status line
// writes once more after its session is killed, into the shared directory the
// picker had just cleared and was waiting on. Readiness fired before the
// picker's own TUI existed, the single /model paste was discarded (it is
// deliberately never retried), the capture burned its whole budget and dropped
// exactly the family the picker exists to name.
func TestCaptureClaudeModelPickerIgnoresAnotherSessionsMarker(t *testing.T) {
	sv := probeTmux(t)
	opts := probeOpts(t, sv, "")
	// A pane that draws the picker once something is submitted but never
	// writes a marker of its own: only the foreign marker below could make
	// this capture believe its TUI is up.
	pane := filepath.Join(opts.Dir, "picker.txt")
	if err := os.WriteFile(pane, []byte(claudeModelPickerPane), 0o600); err != nil {
		t.Fatal(err)
	}
	// The foreign marker is written from inside the pane, not before the
	// capture: the real one lands *after* the capture cleared the directory,
	// which is the whole reason clearing it is not protection.
	foreign, _ := json.Marshal(claudeModelMarker{
		ID:          "claude-haiku-9-1-20000101",
		DisplayName: "Haiku 9.1",
		At:          time.Now().UTC().Format(time.RFC3339Nano),
	})
	opts.Command = "bash --norc -c " + shellQuote(
		"printf %s "+shellQuote(string(foreign))+" > "+shellQuote(claudeModelMarkerPath(opts.Dir))+
			"; read -r x; cat "+shellQuote(pane)+"; sleep 30")
	opts.Timeout = 1500 * time.Millisecond
	if got := captureClaudeModelPicker(context.Background(), opts); len(got) != 0 {
		t.Fatalf("another session's marker proved this pane ready: %v", got)
	}
	if _, ok := readClaudeModelMarker(opts.Dir); !ok {
		t.Fatal("the picker consumed a marker it does not own")
	}
	if _, ok := readClaudeModelMarker(claudeModelPickerDir(opts.Dir)); ok {
		t.Fatal("the picker read its own directory's marker, so this proves nothing")
	}
}

// The picker launch resolves nothing and must therefore pin no model: passing
// one would make the capture depend on the very answer it is trying to find,
// and a candidate the CLI rejects can change what the picker draws.
func TestClaudeModelPickerArgvPinsNoModel(t *testing.T) {
	argv := claudeModelPickerArgv("/probe/settings.json")
	if !strings.HasPrefix(argv, "CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1 claude ") {
		t.Fatalf("model picker does not suppress the feedback survey: %q", argv)
	}
	if !strings.Contains(argv, "--settings "+shellQuote("/probe/settings.json")) {
		t.Fatalf("argv %q does not install the probe settings", argv)
	}
	for _, forbidden := range []string{"--model", " -p ", "--print", claudeUsageProbePrompt} {
		if strings.Contains(argv, forbidden) {
			t.Fatalf("argv %q must not carry %q", argv, forbidden)
		}
	}
}

// Opening the picker is a read. Enter on a highlighted row would write the
// user's default model, so the only thing this probe ever submits is the
// command that opens it — and the session is killed with the picker still open.
func TestClaudeModelPickerCommandIsJustTheSlashCommand(t *testing.T) {
	if claudeModelPickerCommand != "/model" {
		t.Fatalf("picker command = %q", claudeModelPickerCommand)
	}
}

// The refresh policy, without launching anything: a resolver stands in for the
// throwaway sessions, which is also what keeps the suite from ever running a
// real claude.
func refreshApp(t *testing.T, resolve func(context.Context) map[string]string) *app {
	t.Helper()
	a := &app{claudeCachePath: filepath.Join(t.TempDir(), "claude-models.json")}
	a.claudeResolveModels = resolve
	a.claudeVersion = func(context.Context) string { return "2.1.267" }
	return a
}

func TestRefreshClaudeModelsServesUsableCache(t *testing.T) {
	called := false
	a := refreshApp(t, func(context.Context) map[string]string {
		called = true
		return map[string]string{"opus": "claude-opus-9"}
	})
	if err := writeClaudeCache(a.claudeCachePath, "2.1.267", map[string]string{"opus": "claude-opus-9-2"}); err != nil {
		t.Fatal(err)
	}
	a.refreshClaudeModels(context.Background())
	if called {
		t.Error("a usable cache must not cost four throwaway sessions")
	}
	if got := a.resolveClaudeModel("opus"); got != "claude-opus-9-2" {
		t.Fatalf("opus = %q", got)
	}
}

func TestRefreshClaudeModelsProbesAndCaches(t *testing.T) {
	a := refreshApp(t, func(context.Context) map[string]string {
		return map[string]string{"opus": "claude-opus-9", "sonnet": "claude-sonnet-9"}
	})
	a.refreshClaudeModels(context.Background())
	if got := a.resolveClaudeModel("sonnet"); got != "claude-sonnet-9" {
		t.Fatalf("sonnet = %q", got)
	}
	if c := readClaudeCache(a.claudeCachePath); c.IDs["opus"] != "claude-opus-9" {
		t.Fatalf("probe was not cached: %+v", c)
	}
}

// A probe that answers nothing must not blank the launcher. Falling back to a
// stale list is the difference between launching on last week's id — which very
// likely still works — and passing the bare alias the CLI mis-resolves.
func TestRefreshClaudeModelsKeepsStaleIDsWhenTheProbeFails(t *testing.T) {
	a := refreshApp(t, func(context.Context) map[string]string { return nil })
	stale := claudeCache{Version: "1.0.0", ProbedAt: time.Now().Add(-30 * 24 * time.Hour), IDs: map[string]string{"opus": "claude-opus-9-2"}}
	b, _ := json.Marshal(stale)
	if err := os.WriteFile(a.claudeCachePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	a.refreshClaudeModels(context.Background())
	if got := a.resolveClaudeModel("opus"); got != "claude-opus-9-2" {
		t.Fatalf("stale ids dropped after a failed probe: %q", got)
	}
	if c := readClaudeCache(a.claudeCachePath); c.Version != "1.0.0" {
		t.Error("a failed probe must not overwrite the cache it fell back to")
	}
}

// Nothing expensive may be reachable from a request handler by construction:
// the resolver is installed by the serve path, so a handler that asks for a
// refresh in a test launches no session at all.
func TestEnsureClaudeModelsIsInertWithoutAResolver(t *testing.T) {
	a := &app{claudeCachePath: filepath.Join(t.TempDir(), "claude-models.json")}
	a.ensureClaudeModels()
	if a.resolveClaudeModel("opus") != "" {
		t.Error("an app with no resolver resolved a model")
	}
}

// The reason the cache is keyed on the CLI at all: an upgrade is when new
// model ids appear, and a supervisor that has been running for weeks must
// notice one it did not install itself.
func TestRefreshClaudeModelsReprobesAfterACLIUpgrade(t *testing.T) {
	a := refreshApp(t, func(context.Context) map[string]string {
		return map[string]string{"opus": "claude-opus-6"}
	})
	a.claudeVersion = func(context.Context) string { return "2.2.0" }
	if err := writeClaudeCache(a.claudeCachePath, "2.1.267", map[string]string{"opus": "claude-opus-9-2"}); err != nil {
		t.Fatal(err)
	}
	a.refreshClaudeModels(context.Background())
	if got := a.resolveClaudeModel("opus"); got != "claude-opus-6" {
		t.Fatalf("opus = %q, want the answer from the upgraded CLI", got)
	}
	if c := readClaudeCache(a.claudeCachePath); c.Version != "2.2.0" {
		t.Fatalf("cache version = %q, want the build that answered", c.Version)
	}
}
