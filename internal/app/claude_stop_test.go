package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stopHookBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, sub := range []string{"stop", "perm", filepath.Join("perm", "req"), filepath.Join("perm", "ans")} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func writeStopLease(t *testing.T, root, lease string) {
	t.Helper()
	if err := writeClaudePermFile(filepath.Join(root, "perm", "lease"), claudePermLease{
		Lease:   lease,
		Expires: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
}

func stopEventJSON(event string, active bool) []byte {
	doc := map[string]any{
		"hook_event_name":  event,
		"session_id":       hookSIDOwn,
		"stop_hook_active": active,
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return b
}

func readyStopNotices(t *testing.T, root string) []claudeStopNotice {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(root, "stop"))
	if err != nil {
		t.Fatal(err)
	}
	var out []claudeStopNotice
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, "stop", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var n claudeStopNotice
		if err := json.Unmarshal(b, &n); err != nil {
			t.Fatalf("notice %s: %v\n%s", e.Name(), err, b)
		}
		out = append(out, n)
	}
	return out
}

func TestClaudeStopHookWritesLeaseTaggedNotice(t *testing.T) {
	root := stopHookBundle(t)
	writeStopLease(t, root, "lease-live")
	var stdout bytes.Buffer
	if err := RunClaudeStopHook(root, bytes.NewReader(stopEventJSON("Stop", false)), &stdout, io.Discard); err != nil {
		t.Fatalf("helper: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("Stop helper wrote stdout %q; that would risk blocking the turn", stdout.Bytes())
	}
	got := readyStopNotices(t, root)
	if len(got) != 1 {
		t.Fatalf("ready stop notices = %d, want 1", len(got))
	}
	if got[0].Lease != "lease-live" {
		t.Fatalf("notice lease = %q, want lease-live", got[0].Lease)
	}
	if got[0].HookEventName != "Stop" {
		t.Fatalf("notice event = %q, want Stop", got[0].HookEventName)
	}
	if got[0].SessionID != hookSIDOwn {
		t.Fatalf("notice session = %q", got[0].SessionID)
	}
	if got[0].At == "" {
		t.Fatal("notice must carry a timestamp so drain can apply a TTL")
	}
}

func TestClaudeStopHookWritesSessionNoticeWithoutMarker(t *testing.T) {
	root := stopHookBundle(t)
	if err := RunClaudeStopHook(root, bytes.NewReader(stopEventJSON("Stop", false)), io.Discard, io.Discard); err != nil {
		t.Fatalf("helper: %v", err)
	}
	got := readyStopNotices(t, root)
	if len(got) != 1 {
		t.Fatalf("unarmed Stop notices = %d, want 1 so drain can clear epochs", len(got))
	}
	if got[0].Lease != "" {
		t.Fatalf("unarmed notice lease = %q, want empty", got[0].Lease)
	}
	if got[0].SessionID != hookSIDOwn || got[0].HookEventName != "Stop" {
		t.Fatalf("unarmed notice = %+v", got[0])
	}
}

func TestClaudeStopHookWritesNothingWhileContinuationIsActive(t *testing.T) {
	root := stopHookBundle(t)
	writeStopLease(t, root, "lease-live")
	if err := RunClaudeStopHook(root, bytes.NewReader(stopEventJSON("Stop", true)), io.Discard, io.Discard); err != nil {
		t.Fatalf("helper: %v", err)
	}
	if got := readyStopNotices(t, root); len(got) != 0 {
		t.Fatalf("stop_hook_active wrote %+v; the turn is still running", got)
	}
}

func TestClaudeStopFailureWritesLeaseTaggedNotice(t *testing.T) {
	root := stopHookBundle(t)
	writeStopLease(t, root, "lease-fail")
	if err := RunClaudeStopHook(root, bytes.NewReader(stopEventJSON("StopFailure", false)), io.Discard, io.Discard); err != nil {
		t.Fatalf("helper: %v", err)
	}
	got := readyStopNotices(t, root)
	if len(got) != 1 || got[0].Lease != "lease-fail" || got[0].HookEventName != "StopFailure" {
		t.Fatalf("StopFailure notice = %+v", got)
	}
}

func TestClaudeStopHookRejectsBadInput(t *testing.T) {
	valid := stopHookBundle(t)
	var padded map[string]any
	if err := json.Unmarshal(stopEventJSON("Stop", false), &padded); err != nil {
		t.Fatal(err)
	}
	padded["pad"] = strings.Repeat("x", 64*1024)
	oversize, err := json.Marshal(padded)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		dir  string
		body []byte
	}{
		{"malformed", valid, []byte("{")},
		{"oversized", valid, oversize},
		{"wrong event", valid, []byte(`{"hook_event_name":"SessionStart","session_id":"` + hookSIDOwn + `"}`)},
		{"subagent stop", valid, []byte(`{"hook_event_name":"SubagentStop","session_id":"` + hookSIDOwn + `"}`)},
		{"relative dir", "relative", stopEventJSON("Stop", false)},
		{"missing directory", filepath.Join(t.TempDir(), "gone"), stopEventJSON("Stop", false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := RunClaudeStopHook(tc.dir, bytes.NewReader(tc.body), &stdout, io.Discard)
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q", stdout.Bytes())
			}
			if errors.Is(err, errClaudeHookNotImplemented) {
				t.Fatal("helper must classify invalid input, not return not implemented")
			}
			if err == nil {
				t.Fatal("invalid input must fail")
			}
			if tc.dir != "" && filepath.IsAbs(tc.dir) {
				if ents, _ := os.ReadDir(filepath.Join(tc.dir, "stop")); len(ents) != 0 {
					t.Fatalf("ready events after reject: %v", ents)
				}
			}
		})
	}
}

func TestClaudeStopHookMainAlwaysExitsZero(t *testing.T) {
	if code := runClaudeStopHookMain([]string{"--dir", "relative"}); code != 0 {
		t.Fatalf("exit code = %d, want 0 even on the reject path", code)
	}
}

func TestClaudeStopNoticeDisarmsMatchingLease(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, list: []string{"n1"}, capture: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	lease := leaseIDOf(a, n.ID)
	if lease == "" {
		t.Fatal("precondition: expected an armed lease")
	}
	if _, ok := markerLease(t, bundle); !ok {
		t.Fatal("precondition: expected an arm marker")
	}

	if err := RunClaudeStopHook(bundle, bytes.NewReader(stopEventJSON("Stop", false)), io.Discard, io.Discard); err != nil {
		t.Fatalf("helper: %v", err)
	}
	a.drainClaudeHooks()

	if got := phaseOf(a, n.ID); got != autoPhaseOff {
		t.Fatalf("phase after Stop = %q, want off", got)
	}
	if got, ok := markerLease(t, bundle); ok {
		t.Fatalf("Stop left marker %q armed", got)
	}
	ents, err := os.ReadDir(filepath.Join(bundle, "stop"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("stop inbox still has %v after drain", ents)
	}
}

func TestClaudeStopNoticeIgnoresStaleLease(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, list: []string{"n1"}, capture: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	current := leaseIDOf(a, n.ID)

	notice := claudeStopNotice{Lease: "stale-lease", HookEventName: "Stop", SessionID: hookSIDOwn}
	raw, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeHookInboxFile(filepath.Join(bundle, "stop"), raw); err != nil {
		t.Fatal(err)
	}
	a.drainClaudeHooks()

	if got := phaseOf(a, n.ID); got != autoPhaseArmed {
		t.Fatalf("phase after stale Stop = %q, want armed", got)
	}
	if got := leaseIDOf(a, n.ID); got != current {
		t.Fatalf("lease after stale Stop = %q, want %q", got, current)
	}
}

func TestClaudeStopNoticeEmptyLeaseDoesNotDisarm(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, list: []string{"n1"}, capture: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)

	if err := writeClaudeHookInboxFile(filepath.Join(bundle, "stop"), []byte(`{"hook_event_name":"Stop","session_id":"`+hookSIDOwn+`"}`)); err != nil {
		t.Fatal(err)
	}
	a.drainClaudeHooks()
	if got := phaseOf(a, n.ID); got != autoPhaseArmed {
		t.Fatalf("phase after empty-lease Stop = %q, want armed", got)
	}
}

func TestClaudeHookBundleCarriesStopCapability(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(a.claudeHooksDir(), hookID)
	if !bundleSupportsStop(bundle) {
		t.Fatal("a freshly prepared bundle must advertise the Stop capability")
	}
	caps, ok := readClaudeHookCapabilities(bundle)
	if !ok || caps.Stop < 1 {
		t.Fatalf("capabilities.json stop = %d, want 1 (ok=%v)", caps.Stop, ok)
	}
	old := t.TempDir()
	if err := os.WriteFile(filepath.Join(old, "capabilities.json"),
		[]byte(`{"permission":1,"asked":1,"exec":"/bin/true"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if bundleSupportsStop(old) {
		t.Fatal("a pre-Stop bundle (no stop member) must not advertise Stop support")
	}
}

func TestClaudeStopNoticeLandsInProcessedStop(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, list: []string{"n1"}, capture: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)

	if err := RunClaudeStopHook(bundle, bytes.NewReader(stopEventJSON("Stop", false)), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	a.drainClaudeHooks()
	ents, err := os.ReadDir(filepath.Join(bundle, "processed", "stop"))
	if err != nil {
		t.Fatalf("processed/stop: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("processed/stop entries = %d, want 1 (not mixed with SessionStart)", len(ents))
	}
}

func TestClaudeStopNoticeIgnoresForeignSession(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, list: []string{"n1"}, capture: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)

	notice := claudeStopNotice{
		Lease: leaseIDOf(a, n.ID), HookEventName: "Stop",
		SessionID: "00000000-0000-4000-8000-ffffffffffff",
		At:        time.Now().UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeHookInboxFile(filepath.Join(bundle, "stop"), raw); err != nil {
		t.Fatal(err)
	}
	a.drainClaudeHooks()
	if got := phaseOf(a, n.ID); got != autoPhaseArmed {
		t.Fatalf("phase after foreign-session Stop = %q, want armed", got)
	}
}

func TestClaudeStopNoticeExpiresPastTTL(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, list: []string{"n1"}, capture: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)

	notice := claudeStopNotice{
		Lease: leaseIDOf(a, n.ID), HookEventName: "Stop",
		SessionID: hookSIDOwn,
		At:        time.Now().Add(-claudeStopNoticeTTL - time.Minute).UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeHookInboxFile(filepath.Join(bundle, "stop"), raw); err != nil {
		t.Fatal(err)
	}
	a.drainClaudeHooks()
	if got := phaseOf(a, n.ID); got != autoPhaseArmed {
		t.Fatalf("phase after expired Stop = %q, want armed", got)
	}
}

func TestClaudeLeaseMarkerRefreshedWhileArmed(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, list: []string{"n1"}, capture: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	lease := leaseIDOf(a, n.ID)
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "lease"), claudePermLease{
		Lease:   lease,
		Expires: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	a.refreshClaudeLeaseExpiry(n)
	got, ok := readClaudePermLease(filepath.Join(bundle, "perm"), time.Now().Add(claudeLeaseTTL-time.Minute))
	if !ok || got != lease {
		t.Fatalf("refreshed marker lease = %q ok=%v, want %q still valid near the TTL", got, ok, lease)
	}
}

func TestClaudeLeaseMarkerNotRewrittenWhileFresh(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, list: []string{"n1"}, capture: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	lease := leaseIDOf(a, n.ID)
	fresh := time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339Nano)
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "lease"), claudePermLease{
		Lease: lease, Expires: fresh,
	}); err != nil {
		t.Fatal(err)
	}
	a.refreshClaudeLeaseExpiry(n)
	b, err := os.ReadFile(filepath.Join(bundle, "perm", "lease"))
	if err != nil {
		t.Fatal(err)
	}
	var l claudePermLease
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	if l.Expires != fresh {
		t.Fatalf("fresh marker was rewritten (exp %s → %s); refresh must wait until remaining < TTL/2", fresh, l.Expires)
	}
}

func TestClaudeStopHookCommandIsQuoted(t *testing.T) {
	cmd, err := claudeStopHookCommand("/tmp/scimux dir/scimux", "/tmp/scimux hooks/hook-id")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, claudeStopHookCmd) {
		t.Fatalf("command missing %s: %s", claudeStopHookCmd, cmd)
	}
	if !strings.Contains(cmd, shellQuote("/tmp/scimux dir/scimux")) {
		t.Fatalf("executable path must be shell-quoted: %s", cmd)
	}
	if !strings.Contains(cmd, shellQuote("/tmp/scimux hooks/hook-id")) {
		t.Fatalf("hook dir must be shell-quoted: %s", cmd)
	}
}
