package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
)

// dsh takes no model or effort on argv: both are session config options set
// after session/new. That makes the apply step part of the launch rather than
// a decoration on it — a launch that "succeeded" while the agent kept its own
// model would have the node record, the session-log header and the running
// agent all disagreeing, with nothing on screen to say so. These tests pin the
// refusal, and pin that /clear reapplies rather than quietly reverting.

func dshModelSelect(current string, values ...string) sdk.SessionConfigOption {
	opts := make(sdk.SessionConfigSelectOptionsUngrouped, 0, len(values))
	for _, v := range values {
		opts = append(opts, sdk.SessionConfigSelectOption{Value: sdk.SessionConfigValueId(v), Name: v})
	}
	return sdk.SessionConfigOption{Select: &sdk.SessionConfigOptionSelect{
		Id: "model", Name: "Model", CurrentValue: sdk.SessionConfigValueId(current),
		Options: sdk.SessionConfigSelectOptions{Ungrouped: &opts},
	}}
}

// dshEffortSelect mirrors the real dsh menu, which is off/low/high/max and
// appears only for a model whose route supports reasoning effort.
func dshEffortSelect(current string) sdk.SessionConfigOption {
	opts := sdk.SessionConfigSelectOptionsUngrouped{
		{Value: "off", Name: "Off"}, {Value: "low", Name: "Low"},
		{Value: "high", Name: "High"}, {Value: "max", Name: "Max"},
	}
	return sdk.SessionConfigOption{Select: &sdk.SessionConfigOptionSelect{
		Id: "reasoning_effort", Name: "Reasoning effort", CurrentValue: sdk.SessionConfigValueId(current),
		Options: sdk.SessionConfigSelectOptions{Ungrouped: &opts},
	}}
}

func dshSession(id string, opts ...sdk.SessionConfigOption) sdk.NewSessionResponse {
	return sdk.NewSessionResponse{SessionId: sdk.SessionId(id), ConfigOptions: opts}
}

func TestDshLaunchRefusesAModelTheAgentNeverOffered(t *testing.T) {
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		return dshSession("sess_1", dshModelSelect(
			`["local","only-this"]`, `["local","only-this"]`))
	}}
	m := newManager(t, agent)
	_, err := m.Launch("n1", "dsh", t.TempDir(), "deepseek-official/absent", "")
	if err == nil {
		t.Fatal("launch succeeded with a model the agent never offered")
	}
	if !errors.Is(err, ErrConfigRejected) {
		t.Fatalf("error %v does not report a rejected configuration", err)
	}
	if s := m.session("n1"); s != nil {
		t.Fatal("a refused launch left a session behind")
	}
	agent.mu.Lock()
	sets := len(agent.configSets)
	agent.mu.Unlock()
	if sets != 0 {
		t.Fatalf("sent %d config requests for a model that is not on the menu", sets)
	}
	// The meta-only session log must go with it, like every other failed
	// launch: a leftover header burns the slug forever (R20.3).
	if _, err := os.Stat(m.logPath("n1")); !os.IsNotExist(err) {
		t.Fatalf("meta-only log survived a refused launch: %v", err)
	}
}

func TestDshLaunchRefusesAModelTheAgentRejects(t *testing.T) {
	// The value is on the menu but the agent says no — a stale catalog, a
	// profile edited since, a provider that lost its credential. dsh answers
	// -32602 "unknown model option"; scimux must not read that as success.
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			return dshSession("sess_1", dshModelSelect(`["local","a"]`, `["local","a"]`, `["local","b"]`))
		},
		configReply: func(sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(
				map[string]any{"error": "unknown model option"})
		},
	}
	m := newManager(t, agent)
	_, err := m.Launch("n1", "dsh", t.TempDir(), "local/b", "")
	if err == nil || !errors.Is(err, ErrConfigRejected) {
		t.Fatalf("launch error = %v, want a rejected configuration", err)
	}
	if s := m.session("n1"); s != nil {
		t.Fatal("a refused launch left a session behind")
	}
}

func TestDshLaunchReadsTheEffortMenuTheModelSwitchReturned(t *testing.T) {
	// dsh advertises reasoning_effort only when the *selected* model supports
	// it, and set_config_option answers with the full post-switch option set.
	// Reading the session/new options instead would miss the menu entirely.
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			return dshSession("sess_1", dshModelSelect(`["local","plain"]`, `["local","plain"]`, `["ds","thinker"]`))
		},
		configReply: func(p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			if p.ValueId != nil && p.ValueId.ConfigId == "model" {
				return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{
					dshModelSelect(`["ds","thinker"]`, `["local","plain"]`, `["ds","thinker"]`),
					dshEffortSelect("high"),
				}}, nil
			}
			return sdk.SetSessionConfigOptionResponse{}, nil
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "dsh", t.TempDir(), "ds/thinker", "max"); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) != 2 {
		t.Fatalf("config sets = %d, want model then effort", len(agent.configSets))
	}
	if got := string(agent.configSets[0].ValueId.ConfigId); got != "model" {
		t.Fatalf("first config set was %q, want the model", got)
	}
	second := agent.configSets[1].ValueId
	if string(second.ConfigId) != "reasoning_effort" || string(second.Value) != "max" {
		t.Fatalf("effort set = %s=%s, want reasoning_effort=max", second.ConfigId, second.Value)
	}
}

func TestDshLaunchRefusesAnEffortTheModelDoesNotHave(t *testing.T) {
	// The browser's generic low/medium/high menu is exactly how this happens:
	// dsh's own levels are off/low/high/max, and a model without a reasoning
	// route has none at all.
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		return dshSession("sess_1", dshModelSelect(`["local","plain"]`, `["local","plain"]`))
	}}
	m := newManager(t, agent)
	_, err := m.Launch("n1", "dsh", t.TempDir(), "", "medium")
	if err == nil || !errors.Is(err, ErrConfigRejected) {
		t.Fatalf("launch error = %v, want a rejected configuration", err)
	}
	if s := m.session("n1"); s != nil {
		t.Fatal("a refused launch left a session behind")
	}
}

func TestDshClearReappliesTheModel(t *testing.T) {
	// /clear replaces the process, and dsh's argv carries no model — so the
	// replacement starts on the profile default unless the choice is applied
	// again. Without this the gauge, the node record and the log header all
	// keep naming a model the live session is no longer running.
	var sessions int
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		sessions++
		id := "sess_1"
		if sessions > 1 {
			id = "sess_2"
		}
		return dshSession(id, dshModelSelect(`["local","a"]`, `["local","a"]`, `["ds","b"]`))
	}}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "dsh", t.TempDir(), "ds/b", ""); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := m.Clear("n1"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) != 2 {
		t.Fatalf("config sets = %d, want one per session", len(agent.configSets))
	}
	last := agent.configSets[1]
	if string(last.ValueId.SessionId) != "sess_2" {
		t.Fatalf("model reapplied to session %q, want the replacement", last.ValueId.SessionId)
	}
	if string(last.ValueId.Value) != `["ds","b"]` {
		t.Fatalf("replacement session got model %q, want the node's own choice", last.ValueId.Value)
	}
}

func TestDshClearRefusesWhenTheModelCannotBeReapplied(t *testing.T) {
	// A refusal here must leave the old session exactly as it was: no seam,
	// no swap. A page turn that silently changes model is worse than no page
	// turn at all.
	var sessions int
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		sessions++
		if sessions > 1 {
			return dshSession("sess_2", dshModelSelect(`["local","a"]`, `["local","a"]`))
		}
		return dshSession("sess_1", dshModelSelect(`["local","a"]`, `["local","a"]`, `["ds","b"]`))
	}}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "dsh", t.TempDir(), "ds/b", ""); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	err := m.Clear("n1")
	if err == nil || !errors.Is(err, ErrConfigRejected) {
		t.Fatalf("Clear error = %v, want a rejected configuration", err)
	}
	s := m.session("n1")
	if s == nil || string(s.sessionID) != "sess_1" {
		t.Fatalf("session after a refused clear = %v, want the original", s)
	}
	if !s.alive() {
		t.Fatal("a refused clear killed the session it was supposed to leave alone")
	}
	if log := Peek(m, "n1"); strings.Contains(log, "sess_2") {
		t.Fatalf("a refused clear still wrote a seam: %q", log)
	}
}

func TestNonDshAgentsKeepTheirTolerantApply(t *testing.T) {
	// pi keeps treating an unusable effort as "keep your default". Making
	// dsh strict must not change pi's tolerant model and effort handling.
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		return dshSession("sess_1", dshModelSelect(`["local","a"]`, `["local","a"]`))
	}}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "pi", t.TempDir(), "something/else", "medium"); err != nil {
		t.Fatalf("pi launch refused a mismatch it used to tolerate: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) != 0 {
		t.Fatalf("pi sent %d config requests for options it cannot place", len(agent.configSets))
	}
}

// configSetError is the boundary between "your choice was wrong" (400) and
// "the agent is broken" (500). Only JSON-RPC -32602 says the request itself
// was unacceptable; an internal error, a cancellation and a half-closed pipe
// all describe a failure the user cannot fix by picking another model, and
// reporting them as a rejected configuration would point at an innocent field.
func TestOnlyInvalidParamsIsARejectedConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		rejected bool
	}{
		{"invalid params", sdk.NewInvalidParams(map[string]any{"error": "unknown model option"}), true},
		{"internal error", sdk.NewInternalError(map[string]any{"error": "provider exploded"}), false},
		{"request cancelled", sdk.NewRequestCancelled(map[string]any{"error": "cancelled"}), false},
		{"context cancelled", context.Canceled, false},
		{"transport died", io.ErrUnexpectedEOF, false},
		{"wrapped transport death", fmt.Errorf("write config: %w", io.ErrUnexpectedEOF), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := configSetError(tc.err, "model", "ds/b")
			if err == nil {
				t.Fatal("a failed config set must stay an error")
			}
			if got := IsConfigRejection(err); got != tc.rejected {
				t.Fatalf("IsConfigRejection(%v) = %v, want %v", err, got, tc.rejected)
			}
			// Either way the cause has to survive: the 500 path logs it, and
			// the 400 path shows it to the user.
			if !errors.Is(err, tc.err) && !strings.Contains(err.Error(), tc.err.Error()) {
				t.Fatalf("error %v lost its cause %v", err, tc.err)
			}
			if !strings.Contains(err.Error(), "ds/b") {
				t.Fatalf("error %v does not name the value that failed", err)
			}
		})
	}
}

func TestDshLaunchDoesNotBlameTheUserForABrokenAgent(t *testing.T) {
	// The model is on the advertised menu and the user picked it correctly;
	// the agent then fails the call for its own reasons. Classifying that as a
	// rejected configuration would answer 400 and send the supervisor back to
	// the model picker to fix a choice that was never wrong.
	for _, tc := range []struct {
		name  string
		reply error
	}{
		{"internal error", sdk.NewInternalError(map[string]any{"error": "provider unreachable"})},
		{"cancelled", sdk.NewRequestCancelled(map[string]any{"error": "cancelled"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeAgent{
				newSession: func() sdk.NewSessionResponse {
					return dshSession("sess_1", dshModelSelect(`["local","a"]`, `["local","a"]`, `["ds","b"]`))
				},
				configReply: func(sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
					return sdk.SetSessionConfigOptionResponse{}, tc.reply
				},
			}
			m := newManager(t, agent)
			_, err := m.Launch("n1", "dsh", t.TempDir(), "ds/b", "")
			if err == nil {
				t.Fatal("a failed model apply must still fail the launch")
			}
			if IsConfigRejection(err) {
				t.Fatalf("broken agent reported as a rejected configuration: %v", err)
			}
			if s := m.session("n1"); s != nil {
				t.Fatal("a failed launch left a session behind")
			}
		})
	}
}

func TestDshEffortFailureClassifiesLikeTheModel(t *testing.T) {
	// The effort knob takes the same two answers as the model knob, and has
	// to split them the same way: -32602 is a level this model does not have,
	// anything else is the agent failing mid-launch.
	for _, tc := range []struct {
		name     string
		reply    error
		rejected bool
	}{
		{"invalid params", sdk.NewInvalidParams(map[string]any{"error": "no such level"}), true},
		{"internal error", sdk.NewInternalError(map[string]any{"error": "route gone"}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeAgent{
				newSession: func() sdk.NewSessionResponse {
					return dshSession("sess_1", dshModelSelect(`["ds","b"]`, `["ds","b"]`), dshEffortSelect("high"))
				},
				configReply: func(p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
					if p.ValueId != nil && p.ValueId.ConfigId == "reasoning_effort" {
						return sdk.SetSessionConfigOptionResponse{}, tc.reply
					}
					return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{
						dshModelSelect(`["ds","b"]`, `["ds","b"]`), dshEffortSelect("high"),
					}}, nil
				},
			}
			m := newManager(t, agent)
			_, err := m.Launch("n1", "dsh", t.TempDir(), "ds/b", "max")
			if err == nil {
				t.Fatal("a refused effort must fail a dsh launch")
			}
			if got := IsConfigRejection(err); got != tc.rejected {
				t.Fatalf("IsConfigRejection(%v) = %v, want %v", err, got, tc.rejected)
			}
		})
	}
}
