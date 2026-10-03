package acp

import (
	"errors"
	"os"
	"strings"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
)

func vibeCat(category string) *sdk.SessionConfigOptionCategory {
	c := sdk.SessionConfigOptionCategory(category)
	return &c
}

func vibeSelect(id, name, category, current string, options [][2]string) sdk.SessionConfigOption {
	opts := make(sdk.SessionConfigSelectOptionsUngrouped, 0, len(options))
	for _, o := range options {
		opts = append(opts, sdk.SessionConfigSelectOption{
			Value: sdk.SessionConfigValueId(o[0]), Name: o[1],
		})
	}
	sel := &sdk.SessionConfigOptionSelect{
		Id: sdk.SessionConfigId(id), Name: name, CurrentValue: sdk.SessionConfigValueId(current),
		Options: sdk.SessionConfigSelectOptions{Ungrouped: &opts},
	}
	if category != "" {
		sel.Category = vibeCat(category)
	}
	return sdk.SessionConfigOption{Select: sel}
}

func vibeModelMenu() sdk.SessionConfigOption {
	// Id is not the substring "model". Category is what identifies it, so a
	// nearby select whose name merely contains "model" cannot catch the choice.
	return vibeSelect("mdl", "Model", "model", "accidental", [][2]string{
		{"alpha", "Alpha"},
		{"beta", "Beta"},
	})
}

func vibeThinkingMenu() sdk.SessionConfigOption {
	return vibeSelect("thinking", "Thinking", "thinking", "lvl-low", [][2]string{
		{"lvl-low", "Low"},
		{"lvl-high", "High"},
	})
}

// session/new advertises a mode whose name is the effort the user picked.
// Applying thinking through SetSessionMode would switch that mode.
func vibeModeTrap() *sdk.SessionModeState {
	return &sdk.SessionModeState{
		CurrentModeId: "default",
		AvailableModes: []sdk.SessionMode{
			{Id: "default", Name: "Default"},
			{Id: "auto-approve", Name: "High"},
		},
	}
}

func vibeConfiguredAgent() *fakeAgent {
	return &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			return sdk.NewSessionResponse{
				SessionId:     "vibe-sess",
				Modes:         vibeModeTrap(),
				ConfigOptions: []sdk.SessionConfigOption{vibeModelMenu()},
			}
		},
		configReply: func(p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			if p.ValueId != nil && string(p.ValueId.ConfigId) == "mdl" {
				return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{
					vibeModelMenu(), vibeThinkingMenu(),
				}}, nil
			}
			return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{
				vibeModelMenu(), vibeThinkingMenu(),
			}}, nil
		},
	}
}

func TestVibeLaunchAppliesAdvertisedModelThenThinking(t *testing.T) {
	agent := vibeConfiguredAgent()
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "vibe", t.TempDir(), "alpha", "high"); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.lastModeSet != nil {
		t.Fatalf("vibe thinking switched session mode to %q", agent.lastModeSet.ModeId)
	}
	if len(agent.configSets) != 2 {
		t.Fatalf("config sets = %d, want model then thinking", len(agent.configSets))
	}
	first := agent.configSets[0].ValueId
	if string(first.ConfigId) != "mdl" || string(first.Value) != "alpha" {
		t.Fatalf("model set = %s=%s, want mdl=alpha", first.ConfigId, first.Value)
	}
	second := agent.configSets[1].ValueId
	if string(second.ConfigId) != "thinking" || string(second.Value) != "lvl-high" {
		t.Fatalf("thinking set = %s=%s, want the advertised value id lvl-high", second.ConfigId, second.Value)
	}
}

func TestVibeThoughtLevelOutranksTheThinkingCategory(t *testing.T) {
	thought := vibeSelect("level", "Level", string(sdk.SessionConfigOptionCategoryThoughtLevel), "med", [][2]string{
		{"med", "Medium"},
		{"hi", "High"},
	})
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			return sdk.NewSessionResponse{
				SessionId: "vibe-sess",
				ConfigOptions: []sdk.SessionConfigOption{
					vibeModelMenu(), thought, vibeThinkingMenu(),
				},
			}
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "vibe", t.TempDir(), "", "high"); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) != 1 {
		t.Fatalf("config sets = %d, want the thought_level select only", len(agent.configSets))
	}
	got := agent.configSets[0].ValueId
	if string(got.ConfigId) != "level" || string(got.Value) != "hi" {
		t.Fatalf("thinking set = %s=%s, want level=hi", got.ConfigId, got.Value)
	}
}

func TestVibeThinkingNameFallbackIgnoresABroaderName(t *testing.T) {
	level := vibeSelect("knob", "Thinking Level", "", "x", [][2]string{{"x", "Low"}, {"y", "High"}})
	broader := vibeSelect("notes", "Thinking about the weather", "", "z", [][2]string{{"z", "High"}})
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		return sdk.NewSessionResponse{
			SessionId:     "vibe-sess",
			ConfigOptions: []sdk.SessionConfigOption{broader, level},
		}
	}}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "vibe", t.TempDir(), "", "high"); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) != 1 || string(agent.configSets[0].ValueId.ConfigId) != "knob" || string(agent.configSets[0].ValueId.Value) != "y" {
		t.Fatalf("config sets = %+v, want knob=y", agent.configSets)
	}
}

func TestVibeLaunchRefusesAnUnlistedModelBeforeRecordingIt(t *testing.T) {
	agent := vibeConfiguredAgent()
	m := newManager(t, agent)
	_, err := m.Launch("n1", "vibe", t.TempDir(), "not-offered", "high")
	if err == nil || !errors.Is(err, ErrConfigRejected) {
		t.Fatalf("launch error = %v, want a rejected configuration", err)
	}
	if m.session("n1") != nil {
		t.Fatal("a refused launch left a session behind")
	}
	agent.mu.Lock()
	sets := len(agent.configSets)
	agent.mu.Unlock()
	if sets != 0 {
		t.Fatalf("sent %d config requests for a model that is not on the menu", sets)
	}
	if _, statErr := os.Stat(m.logPath("n1")); !os.IsNotExist(statErr) {
		t.Fatalf("meta-only log survived a refused launch: %v", statErr)
	}
}

func TestVibeLaunchRefusesAnUnlistedThinkingLevel(t *testing.T) {
	agent := vibeConfiguredAgent()
	m := newManager(t, agent)
	_, err := m.Launch("n1", "vibe", t.TempDir(), "alpha", "max")
	if err == nil || !errors.Is(err, ErrConfigRejected) {
		t.Fatalf("launch error = %v, want a rejected configuration", err)
	}
	if m.session("n1") != nil {
		t.Fatal("a refused launch left a session behind")
	}
}

func TestVibeLaunchRefusesWhenTheAgentRefusesTheSelection(t *testing.T) {
	agent := vibeConfiguredAgent()
	agent.configReply = func(p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
		return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(map[string]any{"error": "unavailable"})
	}
	m := newManager(t, agent)
	_, err := m.Launch("n1", "vibe", t.TempDir(), "alpha", "")
	if err == nil || !errors.Is(err, ErrConfigRejected) {
		t.Fatalf("launch error = %v, want a rejected configuration", err)
	}
	if m.session("n1") != nil {
		t.Fatal("a refused launch left a session behind")
	}
}

func TestVibeModelMenuMatchesAnExactIdOrName(t *testing.T) {
	t.Run("id", func(t *testing.T) {
		menu := vibeSelect("model", "Menu", "mode", "", [][2]string{{"alpha", "Alpha"}})
		agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
			return sdk.NewSessionResponse{SessionId: "vibe-sess", ConfigOptions: []sdk.SessionConfigOption{menu}}
		}}
		m := newManager(t, agent)
		if _, err := m.Launch("n1", "vibe", t.TempDir(), "alpha", ""); err != nil {
			t.Fatal(err)
		}
		agent.mu.Lock()
		defer agent.mu.Unlock()
		if len(agent.configSets) != 1 || string(agent.configSets[0].ValueId.ConfigId) != "model" || string(agent.configSets[0].ValueId.Value) != "alpha" {
			t.Fatalf("config sets = %+v, want model=alpha", agent.configSets)
		}
	})
	t.Run("name", func(t *testing.T) {
		menu := vibeSelect("mdl", "model", "mode", "", [][2]string{{"alpha", "Alpha"}})
		agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
			return sdk.NewSessionResponse{SessionId: "vibe-sess", ConfigOptions: []sdk.SessionConfigOption{menu}}
		}}
		m := newManager(t, agent)
		if _, err := m.Launch("n1", "vibe", t.TempDir(), "alpha", ""); err != nil {
			t.Fatal(err)
		}
		agent.mu.Lock()
		defer agent.mu.Unlock()
		if len(agent.configSets) != 1 || string(agent.configSets[0].ValueId.ConfigId) != "mdl" {
			t.Fatalf("config sets = %+v, want mdl", agent.configSets)
		}
	})
	t.Run("neither", func(t *testing.T) {
		menu := vibeSelect("mdl", "Menu", "mode", "", [][2]string{{"alpha", "Alpha"}})
		agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
			return sdk.NewSessionResponse{SessionId: "vibe-sess", ConfigOptions: []sdk.SessionConfigOption{menu}}
		}}
		m := newManager(t, agent)
		_, err := m.Launch("n1", "vibe", t.TempDir(), "alpha", "")
		if err == nil || !errors.Is(err, ErrConfigRejected) {
			t.Fatalf("launch error = %v, want a rejected configuration", err)
		}
		if m.session("n1") != nil {
			t.Fatal("a menu that is not the model menu was accepted")
		}
	})
}

func TestVibeThinkingIdOutranksTheNormalizedName(t *testing.T) {
	byID := vibeSelect("thinking", "Reasoning depth", "mode", "lo", [][2]string{{"lo", "Low"}, {"hi", "High"}})
	byName := vibeSelect("knob", "Thinking Level", "", "x", [][2]string{{"x", "Low"}, {"y", "High"}})
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		return sdk.NewSessionResponse{
			SessionId:     "vibe-sess",
			Modes:         vibeModeTrap(),
			ConfigOptions: []sdk.SessionConfigOption{byName, byID},
		}
	}}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "vibe", t.TempDir(), "", "high"); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.lastModeSet != nil {
		t.Fatal("thinking id matched a session mode")
	}
	if len(agent.configSets) != 1 || string(agent.configSets[0].ValueId.ConfigId) != "thinking" || string(agent.configSets[0].ValueId.Value) != "hi" {
		t.Fatalf("config sets = %+v, want thinking=hi", agent.configSets)
	}
}

func TestVibeLaunchRefusesWhenNoThinkingOptionIsAdvertised(t *testing.T) {
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		return sdk.NewSessionResponse{
			SessionId: "vibe-sess",
			ConfigOptions: []sdk.SessionConfigOption{
				{Boolean: &sdk.SessionConfigOptionBoolean{Id: "toggle", Name: "Toggle"}},
			},
		}
	}}
	m := newManager(t, agent)
	_, err := m.Launch("n1", "vibe", t.TempDir(), "", "high")
	if err == nil || !errors.Is(err, ErrConfigRejected) {
		t.Fatalf("launch error = %v, want a rejected configuration", err)
	}
	if m.session("n1") != nil {
		t.Fatal("a launch with no thinking option left a session behind")
	}
}

func TestVibeLaunchReportsAThinkingSelectionError(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		agent := vibeConfiguredAgent()
		agent.configReply = func(p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			if p.ValueId != nil && string(p.ValueId.ConfigId) == "thinking" {
				return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(map[string]any{"error": "unavailable"})
			}
			return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{vibeModelMenu(), vibeThinkingMenu()}}, nil
		}
		m := newManager(t, agent)
		_, err := m.Launch("n1", "vibe", t.TempDir(), "alpha", "high")
		if err == nil || !errors.Is(err, ErrConfigRejected) {
			t.Fatalf("launch error = %v, want a rejected configuration", err)
		}
		if m.session("n1") != nil {
			t.Fatal("a refused thinking level left a session behind")
		}
	})
	t.Run("broken", func(t *testing.T) {
		agent := vibeConfiguredAgent()
		agent.configReply = func(p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			if p.ValueId != nil && string(p.ValueId.ConfigId) == "thinking" {
				return sdk.SetSessionConfigOptionResponse{}, errors.New("transport dropped")
			}
			return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{vibeModelMenu(), vibeThinkingMenu()}}, nil
		}
		m := newManager(t, agent)
		_, err := m.Launch("n1", "vibe", t.TempDir(), "alpha", "high")
		if err == nil || errors.Is(err, ErrConfigRejected) {
			t.Fatalf("launch error = %v, want an agent failure rather than a rejected choice", err)
		}
		if m.session("n1") != nil {
			t.Fatal("a broken thinking selection left a session behind")
		}
	})
}

func TestVibeClearOfDefaultSendsNoConfigSetter(t *testing.T) {
	agent := vibeConfiguredAgent()
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "vibe", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Clear("n1"); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) != 0 || agent.lastModeSet != nil {
		t.Fatalf("default/default clear sent config=%d mode=%v", len(agent.configSets), agent.lastModeSet)
	}
}

func TestVibeSessionCreationFailureIsTheAgentError(t *testing.T) {
	agent := &fakeAgent{newSessionErr: errors.New("authentication is unusable")}
	m := newManager(t, agent)
	_, err := m.Launch("n1", "vibe", t.TempDir(), "", "")
	if err == nil || !strings.Contains(err.Error(), "acp new session") || !strings.Contains(err.Error(), "authentication is unusable") {
		t.Fatalf("launch error = %v, want the agent's session failure on the existing path", err)
	}
	if m.HasSession("n1") {
		t.Fatal("a failed vibe session was recorded")
	}
}

func TestVibeEmptyChoiceSendsNoConfig(t *testing.T) {
	agent := vibeConfiguredAgent()
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "vibe", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) != 0 || agent.lastModeSet != nil {
		t.Fatalf("empty choice sent config=%d mode=%v", len(agent.configSets), agent.lastModeSet)
	}
}

func TestVibeClearReappliesModelThenThinking(t *testing.T) {
	var n int
	agent := vibeConfiguredAgent()
	agent.newSession = func() sdk.NewSessionResponse {
		n++
		return sdk.NewSessionResponse{
			SessionId:     sdk.SessionId("sess-" + string(rune('0'+n))),
			Modes:         vibeModeTrap(),
			ConfigOptions: []sdk.SessionConfigOption{vibeModelMenu()},
		}
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "vibe", t.TempDir(), "beta", "low"); err != nil {
		t.Fatal(err)
	}
	if err := m.Clear("n1"); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) != 4 {
		t.Fatalf("config sets = %d, want model and thinking on both sessions", len(agent.configSets))
	}
	lastModel := agent.configSets[2].ValueId
	lastThink := agent.configSets[3].ValueId
	if string(lastModel.SessionId) != "sess-2" || string(lastModel.Value) != "beta" {
		t.Fatalf("replacement model = %s %s", lastModel.SessionId, lastModel.Value)
	}
	if string(lastThink.ConfigId) != "thinking" || string(lastThink.Value) != "lvl-low" {
		t.Fatalf("replacement thinking = %s=%s", lastThink.ConfigId, lastThink.Value)
	}
}

func TestVibeClearRefusalLeavesTheOldSession(t *testing.T) {
	var n int
	agent := vibeConfiguredAgent()
	agent.newSession = func() sdk.NewSessionResponse {
		n++
		opts := []sdk.SessionConfigOption{vibeModelMenu()}
		if n > 1 {
			opts = []sdk.SessionConfigOption{vibeSelect("model", "Model", "model", "alpha", [][2]string{{"alpha", "Alpha"}})}
		}
		return sdk.NewSessionResponse{
			SessionId: sdk.SessionId("sess-" + string(rune('0'+n))), Modes: vibeModeTrap(), ConfigOptions: opts,
		}
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "vibe", t.TempDir(), "beta", ""); err != nil {
		t.Fatal(err)
	}
	err := m.Clear("n1")
	if err == nil || !errors.Is(err, ErrConfigRejected) {
		t.Fatalf("Clear error = %v, want a rejected configuration", err)
	}
	s := m.session("n1")
	if s == nil || string(s.sessionID) != "sess-1" || !s.alive() {
		t.Fatal("a refused clear replaced or killed the original session")
	}
	if log := Peek(m, "n1"); strings.Contains(log, "sess-2") {
		t.Fatalf("a refused clear wrote a seam: %q", log)
	}
}

func TestPiGrokAndCursorStayTolerant(t *testing.T) {
	for _, agentName := range []string{"pi", "grok", "cursor"} {
		t.Run(agentName, func(t *testing.T) {
			agent := vibeConfiguredAgent()
			m := newManager(t, agent)
			if _, err := m.Launch("n1", agentName, t.TempDir(), "not-offered", "high"); err != nil {
				t.Fatalf("%s launch refused a mismatch it tolerates: %v", agentName, err)
			}
			agent.mu.Lock()
			defer agent.mu.Unlock()
			if len(agent.configSets) != 0 {
				t.Fatalf("%s sent %d config requests", agentName, len(agent.configSets))
			}
		})
	}
}
