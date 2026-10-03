package acp

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

func opencodeEffortSelect(current string) sdk.SessionConfigOption {
	opts := sdk.SessionConfigSelectOptionsUngrouped{
		{Value: "low", Name: "Low"}, {Value: "high", Name: "High"},
		{Value: "max", Name: "Max"}, {Value: "default", Name: "Default"},
	}
	return sdk.SessionConfigOption{Select: &sdk.SessionConfigOptionSelect{
		Id: "effort", Name: "Effort", CurrentValue: sdk.SessionConfigValueId(current),
		Options: sdk.SessionConfigSelectOptions{Ungrouped: &opts},
	}}
}

func TestOpencodeLaunchAppliesAnAdvertisedModel(t *testing.T) {
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		// Unrelated and non-select options cannot absorb a model choice.
		return dshSession("sess_1", sdk.SessionConfigOption{Boolean: &sdk.SessionConfigOptionBoolean{
			Id: "toggle", Name: "Toggle", CurrentValue: false,
		}}, opencodeEffortSelect("high"),
			dshModelSelect("zen/house", "zen/house", "local/coder"))
	}}
	m := newManager(t, agent)
	t.Cleanup(func() { _ = m.Kill("n1") })
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "local/coder", ""); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) != 1 {
		t.Fatalf("config sets = %d, want 1", len(agent.configSets))
	}
	got := agent.configSets[0].ValueId
	if got == nil || got.ConfigId != "model" || got.Value != "local/coder" {
		t.Fatalf("model set = %+v, want model=local/coder", got)
	}
}

func TestOpencodeLaunchRefusesAListedModelTheAgentRejects(t *testing.T) {
	// Keep the default settle period: listed models must never enter the
	// retry path for providers that are still loading.
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			return dshSession("sess_1", dshModelSelect("zen/house", "zen/house", "local/coder"))
		},
		configReply: func(sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(nil)
		},
	}
	m := newManager(t, agent)
	t.Cleanup(func() { _ = m.Kill("n1") })
	start := time.Now()
	_, err := m.Launch("n1", "opencode", t.TempDir(), "local/coder", "")
	if !errors.Is(err, ErrConfigRejected) {
		t.Fatalf("Launch = %v, want ErrConfigRejected", err)
	}
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("listed model rejection took %v", elapsed)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) != 1 {
		t.Fatalf("attempts = %d, want 1", len(agent.configSets))
	}
}

func TestOpencodeLaunchWaitsForAProviderStillLoading(t *testing.T) {
	oldSettle, oldEvery := opencodeModelSettle, opencodeModelRetryEvery
	opencodeModelSettle, opencodeModelRetryEvery = time.Second, time.Millisecond
	t.Cleanup(func() { opencodeModelSettle, opencodeModelRetryEvery = oldSettle, oldEvery })
	calls := 0
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse { return dshSession("sess_1", dshModelSelect("zen/house", "zen/house")) },
		configReply: func(sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			calls++
			if calls < 3 {
				return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(nil)
			}
			return sdk.SetSessionConfigOptionResponse{}, nil
		},
	}
	m := newManager(t, agent)
	t.Cleanup(func() { _ = m.Kill("n1") })
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "local/coder", ""); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) != 3 {
		t.Fatalf("config sets = %d, want 3", len(agent.configSets))
	}
	for _, req := range agent.configSets {
		if req.ValueId == nil || req.ValueId.ConfigId != "model" || req.ValueId.Value != "local/coder" {
			t.Fatalf("retry lost raw model value: %+v", req)
		}
	}
}

func TestOpencodeLaunchRefusesAModelThatNeverAppears(t *testing.T) {
	oldSettle, oldEvery := opencodeModelSettle, opencodeModelRetryEvery
	opencodeModelSettle, opencodeModelRetryEvery = 50*time.Millisecond, time.Millisecond
	t.Cleanup(func() { opencodeModelSettle, opencodeModelRetryEvery = oldSettle, oldEvery })
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse { return dshSession("sess_1", dshModelSelect("zen/house", "zen/house")) },
		configReply: func(sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(nil)
		},
	}
	m := newManager(t, agent)
	t.Cleanup(func() { _ = m.Kill("n1") })
	start := time.Now()
	_, err := m.Launch("n1", "opencode", t.TempDir(), "local/missing", "")
	if !errors.Is(err, ErrConfigRejected) {
		t.Fatalf("Launch = %v, want ErrConfigRejected", err)
	}
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("launch took %v", elapsed)
	}
	if m.session("n1") != nil {
		t.Fatal("refused launch left a session behind")
	}
	if _, err := os.Stat(m.logPath("n1")); !os.IsNotExist(err) {
		t.Fatalf("meta-only log survived: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) < 2 {
		t.Fatalf("attempts = %d, want at least 2", len(agent.configSets))
	}
}

func TestOpencodeLaunchRefusesAnUnknownModelOfALoadedProvider(t *testing.T) {
	for _, model := range []string{"zen/typo", "bare"} {
		t.Run(model, func(t *testing.T) {
			agent := &fakeAgent{
				newSession: func() sdk.NewSessionResponse { return dshSession("sess_1", dshModelSelect("zen/a", "zen/a")) },
				configReply: func(sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
					return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(nil)
				},
			}
			m := newManager(t, agent)
			t.Cleanup(func() { _ = m.Kill("n1") })
			_, err := m.Launch("n1", "opencode", t.TempDir(), model, "")
			if !errors.Is(err, ErrConfigRejected) {
				t.Fatalf("Launch = %v, want ErrConfigRejected", err)
			}
			agent.mu.Lock()
			defer agent.mu.Unlock()
			if len(agent.configSets) != 1 {
				t.Fatalf("attempts = %d, want 1", len(agent.configSets))
			}
			if got := agent.configSets[0].ValueId; got == nil || string(got.Value) != model {
				t.Fatalf("raw model set = %+v", got)
			}
		})
	}
}

func TestOpencodeLaunchDoesNotBlameTheUserForABrokenAgent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply error
	}{
		{"internal error", sdk.NewInternalError(nil)}, {"cancelled", sdk.NewRequestCancelled(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeAgent{
				newSession: func() sdk.NewSessionResponse { return dshSession("sess_1", dshModelSelect("zen/house", "zen/house")) },
				configReply: func(sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
					return sdk.SetSessionConfigOptionResponse{}, tc.reply
				},
			}
			m := newManager(t, agent)
			t.Cleanup(func() { _ = m.Kill("n1") })
			// A missing provider would permit retries, but these errors never do.
			_, err := m.Launch("n1", "opencode", t.TempDir(), "local/coder", "")
			if err == nil || IsConfigRejection(err) {
				t.Fatalf("Launch = %v, want server error", err)
			}
			agent.mu.Lock()
			defer agent.mu.Unlock()
			if len(agent.configSets) != 1 {
				t.Fatalf("attempts = %d, want 1", len(agent.configSets))
			}
		})
	}
}

func TestOpencodeLaunchWithoutAModelMenuKeepsItsDefault(t *testing.T) {
	agent := &fakeAgent{}
	m := newManager(t, agent)
	t.Cleanup(func() { _ = m.Kill("n1") })
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "cheap", ""); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) != 0 {
		t.Fatalf("config sets = %d, want 0", len(agent.configSets))
	}
}

func TestOpencodeLaunchWithoutAModelSendsNoModelSet(t *testing.T) {
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		return dshSession("sess_1", dshModelSelect("zen/house", "zen/house"))
	}}
	m := newManager(t, agent)
	t.Cleanup(func() { _ = m.Kill("n1") })
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	for _, req := range agent.configSets {
		if req.ValueId != nil && req.ValueId.ConfigId == "model" {
			t.Fatal("empty model caused a model set")
		}
	}
}

func TestOpencodeEffortIsReadFromThePostSwitchMenu(t *testing.T) {
	for _, effortOffered := range []bool{false, true} {
		name := "effort disappears"
		if effortOffered {
			name = "effort offered after switch"
		}
		t.Run(name, func(t *testing.T) {
			agent := &fakeAgent{
				newSession: func() sdk.NewSessionResponse {
					return dshSession("sess_1", dshModelSelect("zen/house", "zen/house", "local/coder"), opencodeEffortSelect("high"))
				},
				configReply: func(p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
					if p.ValueId != nil && p.ValueId.ConfigId == "model" {
						opts := []sdk.SessionConfigOption{dshModelSelect("local/coder", "local/coder")}
						if effortOffered {
							opts = append(opts, opencodeEffortSelect("high"))
						}
						return sdk.SetSessionConfigOptionResponse{ConfigOptions: opts}, nil
					}
					return sdk.SetSessionConfigOptionResponse{}, nil
				},
			}
			m := newManager(t, agent)
			t.Cleanup(func() { _ = m.Kill("n1") })
			if _, err := m.Launch("n1", "opencode", t.TempDir(), "local/coder", "low"); err != nil {
				t.Fatal(err)
			}
			agent.mu.Lock()
			defer agent.mu.Unlock()
			want := 1
			if effortOffered {
				want = 2
			}
			if len(agent.configSets) != want {
				t.Fatalf("config sets = %d, want %d", len(agent.configSets), want)
			}
			if got := agent.configSets[0].ValueId; got == nil || got.ConfigId != "model" {
				t.Fatalf("first set = %+v, want model", got)
			}
			if effortOffered {
				if got := agent.configSets[1].ValueId; got == nil || got.ConfigId != "effort" || got.Value != "low" {
					t.Fatalf("second set = %+v, want effort=low", got)
				}
			}
		})
	}
}

func TestOpencodeEffortStaysTolerant(t *testing.T) {
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			return dshSession("sess_1", dshModelSelect("zen/house", "zen/house", "local/coder"))
		},
		configReply: func(p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			if p.ValueId != nil && p.ValueId.ConfigId == "model" {
				return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{
					dshModelSelect("local/coder", "local/coder"), opencodeEffortSelect("high"),
				}}, nil
			}
			return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(nil)
		},
	}
	m := newManager(t, agent)
	t.Cleanup(func() { _ = m.Kill("n1") })
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "local/coder", "low"); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) != 2 {
		t.Fatalf("config sets = %d, want model then effort", len(agent.configSets))
	}
	if got := agent.configSets[1].ValueId; got == nil || got.ConfigId != "effort" || got.Value != "low" {
		t.Fatalf("effort set = %+v", got)
	}
}

func TestOpencodeClearReappliesTheModel(t *testing.T) {
	sessions := 0
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		sessions++
		id := "sess_1"
		if sessions > 1 {
			id = "sess_2"
		}
		return dshSession(id, dshModelSelect("zen/house", "zen/house", "local/coder"))
	}}
	m := newManager(t, agent)
	t.Cleanup(func() { _ = m.Kill("n1") })
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "local/coder", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Clear("n1"); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.configSets) != 2 {
		t.Fatalf("model sets = %d, want 2", len(agent.configSets))
	}
	for i, req := range agent.configSets {
		id := "sess_1"
		if i == 1 {
			id = "sess_2"
		}
		if got := req.ValueId; got == nil || got.ConfigId != "model" || got.Value != "local/coder" || string(got.SessionId) != id {
			t.Fatalf("model set %d = %+v", i, got)
		}
	}
}

func TestOpencodeClearRefusalLeavesTheOldSession(t *testing.T) {
	oldSettle := opencodeModelSettle
	opencodeModelSettle = 0
	t.Cleanup(func() { opencodeModelSettle = oldSettle })
	sessions := 0
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			sessions++
			if sessions > 1 {
				return dshSession("sess_2", dshModelSelect("zen/house", "zen/house"))
			}
			return dshSession("sess_1", dshModelSelect("zen/house", "zen/house", "local/coder"))
		},
		configReply: func(p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			if p.ValueId != nil && p.ValueId.SessionId == "sess_2" {
				return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(nil)
			}
			return sdk.SetSessionConfigOptionResponse{}, nil
		},
	}
	m := newManager(t, agent)
	t.Cleanup(func() { _ = m.Kill("n1") })
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "local/coder", ""); err != nil {
		t.Fatal(err)
	}
	old := m.session("n1")
	before, err := os.ReadFile(m.logPath("n1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Clear("n1"); !errors.Is(err, ErrConfigRejected) {
		t.Fatalf("Clear = %v, want ErrConfigRejected", err)
	}
	if got := m.session("n1"); got != old || !old.alive() || old.sessionID != "sess_1" {
		t.Fatal("refused clear replaced or killed the original session")
	}
	after, err := os.ReadFile(m.logPath("n1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("refused clear changed the log: %q", after)
	}
}

func TestSettleConfigSet(t *testing.T) {
	invalid := sdk.NewInvalidParams(nil)
	internal := sdk.NewInternalError(nil)
	for _, tc := range []struct {
		name    string
		settle  time.Duration
		replies []error
		want    error
	}{
		{"first success", time.Second, []error{nil}, nil},
		{"server error", time.Second, []error{internal}, internal},
		{"zero settle", 0, []error{invalid}, invalid},
		{"later success", time.Second, []error{invalid, invalid, nil}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			wantResp := sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{dshModelSelect("zen/house", "zen/house")}}
			resp, err := settleConfigSet(context.Background(), tc.settle, time.Millisecond, func() (sdk.SetSessionConfigOptionResponse, error) {
				calls++
				if calls > len(tc.replies) {
					t.Fatal("unexpected retry")
				}
				return wantResp, tc.replies[calls-1]
			})
			if !errors.Is(err, tc.want) || calls != len(tc.replies) {
				t.Fatalf("error = %v, calls = %d", err, calls)
			}
			if !reflect.DeepEqual(resp, wantResp) {
				t.Fatalf("response lost: %+v", resp)
			}
		})
	}
	t.Run("cancel during wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		_, err := settleConfigSet(ctx, time.Hour, time.Hour, func() (sdk.SetSessionConfigOptionResponse, error) {
			calls++
			// Cancel during the long retry wait, rather than waiting for it.
			time.AfterFunc(time.Millisecond, cancel)
			return sdk.SetSessionConfigOptionResponse{}, invalid
		})
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("error = %v, calls = %d", err, calls)
		}
	})
}

func TestOpencodeProvider(t *testing.T) {
	for _, tc := range []struct {
		model, provider string
		ok              bool
	}{
		{"local/coder", "local", true}, {"bare", "", false}, {"/x", "", false},
		{"a/b/c", "a", true}, {"a/", "", false}, {"", "", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			provider, ok := opencodeProvider(tc.model)
			if provider != tc.provider || ok != tc.ok {
				t.Fatalf("opencodeProvider(%q) = (%q, %v)", tc.model, provider, ok)
			}
		})
	}
}
