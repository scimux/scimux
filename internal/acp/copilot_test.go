package acp

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

// Copilot's ACP initialize names scimux as a person-driven client, and states
// the same thing in _meta, which the installed `copilot help acp` says takes
// precedence. Other harnesses keep the bare initialize they always had, so
// this table pins both sides of that boundary.
func TestInitializeClientInfoByAgent(t *testing.T) {
	SetClientVersion("v1.2.3")
	t.Cleanup(func() { SetClientVersion("dev") })
	cases := []struct {
		agent    string
		wantInfo bool
	}{
		{agent: "copilot", wantInfo: true},
		{agent: "vibe"},
		{agent: "opencode"},
	}
	for _, c := range cases {
		t.Run(c.agent, func(t *testing.T) {
			agent := &fakeAgent{}
			m := NewManagerWithRunner(t.TempDir(), fakeRunner(agent))
			if _, err := m.Launch("n-"+c.agent, c.agent, t.TempDir(), "", ""); err != nil {
				t.Fatalf("Launch(%s): %v", c.agent, err)
			}
			t.Cleanup(m.Shutdown)
			agent.mu.Lock()
			init := agent.lastInit
			agent.mu.Unlock()
			info := init.ClientInfo
			interactive, hasMeta := init.ClientCapabilities.Meta["github.com/copilot"]
			if !c.wantInfo {
				if info != nil || hasMeta {
					t.Fatalf("initialize identity = %+v meta=%v, want none for %s", info, init.ClientCapabilities.Meta, c.agent)
				}
				return
			}
			if info == nil || info.Name != "scimux" || info.Version != "v1.2.3" {
				t.Fatalf("clientInfo = %+v, want scimux v1.2.3", info)
			}
			if info.Title == nil || *info.Title != "scimux" {
				t.Fatalf("clientInfo title = %v, want scimux", info.Title)
			}
			if got, ok := interactive.(map[string]any); !ok || got["interactive"] != true {
				t.Fatalf("copilot _meta = %v, want interactive true", interactive)
			}
		})
	}
}

// copilotMenus is a session/new answer with a model select and, optionally, an
// effort select. Ids are synthetic; they only need to be distinct from the
// real Copilot option names.
func copilotMenus(current string, models []string, effortCurrent string, efforts []string) []sdk.SessionConfigOption {
	var model sdk.SessionConfigOptionSelect
	mopts := make(sdk.SessionConfigSelectOptionsUngrouped, 0, len(models))
	for _, m := range models {
		mopts = append(mopts, sdk.SessionConfigSelectOption{Value: sdk.SessionConfigValueId(m), Name: m})
	}
	cat := sdk.SessionConfigOptionCategoryModel
	model = sdk.SessionConfigOptionSelect{
		Id: "model", Name: "Model", CurrentValue: sdk.SessionConfigValueId(current), Category: &cat,
		Options: sdk.SessionConfigSelectOptions{Ungrouped: &mopts},
	}
	out := []sdk.SessionConfigOption{{Select: &model}}
	if efforts != nil {
		eopts := make(sdk.SessionConfigSelectOptionsUngrouped, 0, len(efforts))
		for _, e := range efforts {
			eopts = append(eopts, sdk.SessionConfigSelectOption{Value: sdk.SessionConfigValueId(e), Name: e})
		}
		effort := sdk.SessionConfigOptionSelect{
			Id: "reasoning_effort", Name: "Reasoning effort", CurrentValue: sdk.SessionConfigValueId(effortCurrent),
			Options: sdk.SessionConfigSelectOptions{Ungrouped: &eopts},
		}
		out = append(out, sdk.SessionConfigOption{Select: &effort})
	}
	return out
}

// copilotCatalogAgent answers the disposable probe with the given menus.
func copilotCatalogAgent(opts []sdk.SessionConfigOption) *fakeAgent {
	return &fakeAgent{newSession: func() sdk.NewSessionResponse {
		return sdk.NewSessionResponse{SessionId: "probe", ConfigOptions: opts}
	}}
}

// probeCopilotWith runs the probe against an in-process fake agent and returns
// the agent's close count and the process it was given, so each case can prove
// the session was closed and the process reaped.
func probeCopilotWith(t *testing.T, agent *fakeAgent) (CopilotCatalog, error, *countingProcess) {
	t.Helper()
	var cp *countingProcess
	start := func(dir string) (Process, error) {
		r1, w1 := io.Pipe()
		r2, w2 := io.Pipe()
		agent.conn = sdk.NewAgentSideConnection(agent, w2, r1)
		p := &countingProcess{fakeProcess: &fakeProcess{stdin: w1, stdout: r2, r1: r1, w2: w2, closed: make(chan struct{})}}
		cp = p
		return p, nil
	}
	cat, err := probeCopilotCatalog(context.Background(), start, copilotProbeTotal, copilotProbeInitialize, copilotProbePerCall)
	return cat, err, cp
}

func TestCopilotCatalogProbe(t *testing.T) {
	cases := []struct {
		name       string
		opts       []sdk.SessionConfigOption
		newErr     error
		wantModels []string
		wantEffort map[string][]string
		wantErr    bool
	}{
		{
			name:       "auto only, no effort menu",
			opts:       copilotMenus("auto", []string{"auto"}, "", nil),
			wantModels: []string{"auto"},
		},
		{
			name:       "duplicates and blanks are dropped",
			opts:       copilotMenus("auto", []string{"auto", "", "alpha", "alpha", "auto"}, "", nil),
			wantModels: []string{"auto", "alpha"},
		},
		{
			name:       "effort menu applies only to the current model",
			opts:       copilotMenus("alpha", []string{"alpha", "beta"}, "low", []string{"low", "high"}),
			wantModels: []string{"alpha", "beta"},
			wantEffort: map[string][]string{"alpha": {"low", "high"}},
		},
		{
			name: "no model select publishes nothing",
			opts: []sdk.SessionConfigOption{},
		},
		{
			name:    "session/new failure is an error and reaps",
			newErr:  errors.New("refused"),
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agent := copilotCatalogAgent(c.opts)
			agent.newSessionErr = c.newErr
			cat, err, cp := probeCopilotWith(t, agent)
			if cp == nil {
				t.Fatal("probe never started the process")
			}
			waitFor(t, "probe process reaped", func() bool {
				return atomic.LoadInt32(&cp.kills) >= 1 && atomic.LoadInt32(&cp.waits) >= 1
			})
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if strings.Join(cat.Models, ",") != strings.Join(c.wantModels, ",") {
				t.Fatalf("models = %v, want %v", cat.Models, c.wantModels)
			}
			if len(cat.Efforts) != len(c.wantEffort) {
				t.Fatalf("efforts = %v, want %v", cat.Efforts, c.wantEffort)
			}
			for model, levels := range c.wantEffort {
				if strings.Join(cat.Efforts[model], ",") != strings.Join(levels, ",") {
					t.Fatalf("efforts[%s] = %v, want %v", model, cat.Efforts[model], levels)
				}
			}
			agent.mu.Lock()
			closes := agent.closes
			agent.mu.Unlock()
			if closes != 1 {
				t.Fatalf("session closes = %d, want 1 (probe must close what it opened)", closes)
			}
			agent.mu.Lock()
			prompts := agent.prompts
			agent.mu.Unlock()
			if prompts != 0 {
				t.Fatalf("probe sent %d prompts; discovery must never prompt", prompts)
			}
		})
	}
}

// The probe's budget is enforced by the same clock seam as Vibe's: a probe
// that has no room for its next call stops before making it.
func TestCopilotCatalogProbeBudgetStopsBeforeSessionNew(t *testing.T) {
	agent := copilotCatalogAgent(copilotMenus("auto", []string{"auto"}, "", nil))
	prev := copilotProbeClock
	t.Cleanup(func() { copilotProbeClock = prev })
	start := time.Now()
	copilotProbeClock = func() time.Time { return start.Add(time.Hour) }
	_, err, _ := probeCopilotWith(t, agent)
	if err == nil {
		t.Fatal("exhausted budget was not reported")
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.lastNew != nil {
		t.Fatal("probe opened a session after its budget was spent")
	}
}

// Launch applies the model and effort the user chose through the advertised
// menus only. A choice the agent does not offer is refused, never dropped.
func TestCopilotLaunchConfigIsStrict(t *testing.T) {
	cases := []struct {
		name    string
		opts    []sdk.SessionConfigOption
		model   string
		effort  string
		wantErr bool
		wantSet []string
	}{
		{name: "defaults set nothing", opts: copilotMenus("auto", []string{"auto", "alpha"}, "", nil)},
		{name: "advertised model is set", opts: copilotMenus("auto", []string{"auto", "alpha"}, "", nil), model: "alpha", wantSet: []string{"model=alpha"}},
		{name: "unknown model is refused", opts: copilotMenus("auto", []string{"auto"}, "", nil), model: "gamma", wantErr: true},
		{name: "model without a menu is refused", opts: []sdk.SessionConfigOption{}, model: "alpha", wantErr: true},
		{name: "effort without a menu is refused", opts: copilotMenus("auto", []string{"auto"}, "", nil), effort: "high", wantErr: true},
		{name: "advertised effort is set", opts: copilotMenus("auto", []string{"auto"}, "low", []string{"low", "high"}), effort: "high", wantSet: []string{"reasoning_effort=high"}},
		{name: "effort outside the menu is refused", opts: copilotMenus("auto", []string{"auto"}, "low", []string{"low", "high"}), effort: "max", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
				return sdk.NewSessionResponse{SessionId: "s1", ConfigOptions: c.opts}
			}}
			agent.configReply = func(p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
				return sdk.SetSessionConfigOptionResponse{ConfigOptions: c.opts}, nil
			}
			m := NewManagerWithRunner(t.TempDir(), fakeRunner(agent))
			_, err := m.Launch("cop-cfg", "copilot", t.TempDir(), c.model, c.effort)
			if (err != nil) != c.wantErr {
				t.Fatalf("Launch err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				if !IsConfigRejection(err) && !strings.Contains(err.Error(), "copilot") {
					t.Fatalf("refusal error = %v", err)
				}
				return
			}
			t.Cleanup(m.Shutdown)
			agent.mu.Lock()
			defer agent.mu.Unlock()
			var got []string
			for _, set := range agent.configSets {
				got = append(got, string(set.ValueId.ConfigId)+"="+string(set.ValueId.Value))
			}
			if strings.Join(got, ";") != strings.Join(c.wantSet, ";") {
				t.Fatalf("config sets = %v, want %v", got, c.wantSet)
			}
		})
	}
}

// Copilot's model and effort travel as session options. The argv carries
// neither, and the process identity is the same for every choice.
func TestCopilotNeverPutsModelOnArgv(t *testing.T) {
	got, err := agentArgv("copilot", "alpha", "high")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "copilot --acp --stdio --no-auto-update" {
		t.Fatalf("argv = %v", got)
	}
}

// copilotWaiting is how many permission requests the session is holding: the
// one shown plus the bounded queue behind it. Tests wait on this count, which
// is an observable state, instead of sleeping for an ordering.
func copilotWaiting(s *Session) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.permQueue)
	if s.pending != nil {
		n++
	}
	return n
}

// admitCopilot starts one RequestPermission call on its own goroutine, the way
// the SDK dispatches each agent request, and waits until it is admitted.
func admitCopilot(t *testing.T, s *Session, id string, opts []sdk.PermissionOption) (*parkedPerm, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &parkedPerm{resp: make(chan sdk.RequestPermissionResponse, 1), cancel: cancel}
	before := copilotWaiting(s)
	go func() {
		resp, _ := s.RequestPermission(ctx, sdk.RequestPermissionRequest{
			ToolCall: sdk.ToolCallUpdate{ToolCallId: sdk.ToolCallId(id)},
			Options:  opts,
		})
		p.resp <- resp
	}()
	waitFor(t, "admitted "+id, func() bool { return copilotWaiting(s) > before })
	// The shown request is the head, not necessarily this one, so the id this
	// call admitted is read from its sequence number.
	s.mu.Lock()
	p.pending = PendingPermission{RequestID: formatRequestID(s.incarn, s.pendingSeq)}
	s.mu.Unlock()
	return p, cancel
}

// copilotSession is a copilot session inside one active turn with a live
// process: the state every real permission request arrives in.
func copilotSession(t *testing.T) *Session {
	t.Helper()
	s := newPermSession(t, "copilot")
	s.procAlive = true
	s.turnActive = true
	return s
}

// answer resolves the currently shown request with a key, the way the app
// layer does: prepare against the displayed id, then deliver the token.
func answer(t *testing.T, s *Session, requestID, key string) (string, error) {
	t.Helper()
	tok, _, err := s.prepareResolve(requestID, key)
	if err != nil {
		return "", err
	}
	return string(tok), s.deliver(tok)
}

func selectedOption(t *testing.T, p *parkedPerm) string {
	t.Helper()
	resp := p.take(t)
	if resp.Outcome.Selected == nil {
		t.Fatalf("outcome = %+v, want a selected option", resp.Outcome)
	}
	return string(resp.Outcome.Selected.OptionId)
}

// Two and three concurrent requests are independently answered, in admission
// order, and reuse the same tool and option ids without crosstalk.
func TestCopilotConcurrentPermissionsAreFIFO(t *testing.T) {
	for _, n := range []int{2, 3} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			s := copilotSession(t)
			var parked []*parkedPerm
			for i := 0; i < n; i++ {
				p, cancel := admitCopilot(t, s, "shared-tool", vibePermOptions())
				t.Cleanup(cancel)
				parked = append(parked, p)
			}
			seqs := make([]uint64, 0, n)
			for i := 0; i < n; i++ {
				cur, ok := s.pendingInfo()
				if !ok {
					t.Fatalf("no request shown at step %d", i)
				}
				_, sq, ok2 := ParseRequestID(cur.RequestID)
				if !ok2 {
					t.Fatalf("bad request id %q", cur.RequestID)
				}
				seqs = append(seqs, sq)
				// Answer each shown request with a different option, so a
				// crossed answer would show up in the response.
				key := []string{"1", "4", "2"}[i%3]
				if _, err := answer(t, s, cur.RequestID, key); err != nil {
					t.Fatalf("answer %d: %v", i, err)
				}
				want := []string{"allow-once", "reject-once", "allow-always-session"}[i%3]
				if got := selectedOption(t, parked[i]); got != want {
					t.Fatalf("request %d selected %q, want %q", i, got, want)
				}
			}
			for i := 1; i < len(seqs); i++ {
				if seqs[i] != seqs[i-1]+1 {
					t.Fatalf("sequence ids %v are not consecutive in admission order", seqs)
				}
			}
			if copilotWaiting(s) != 0 {
				t.Fatalf("%d requests still held after every answer", copilotWaiting(s))
			}
		})
	}
}

// Sequence ids are assigned at admission, so a queued request already owns the
// id it will carry when promoted, and the auto-approval boundary covers it.
func TestCopilotQueuedRequestsCarryAdmissionSequence(t *testing.T) {
	s := copilotSession(t)
	first, c1 := admitCopilot(t, s, "a", oneAllow())
	defer c1()
	second, c2 := admitCopilot(t, s, "b", oneAllow())
	defer c2()
	s.mu.Lock()
	incarn, boundary := s.incarn, s.pendingSeq
	s.mu.Unlock()
	if incarn == "" || boundary != 2 {
		t.Fatalf("boundary = %q/%d, want two admitted requests", incarn, boundary)
	}
	// The queued request cannot be answered through the shown request's id.
	shown, _ := s.pendingInfo()
	if _, _, err := s.prepareResolve(formatRequestID(incarn, 2), "1"); err != ErrStalePermission {
		t.Fatalf("queued id answered while the first is shown: err=%v", err)
	}
	if _, err := answer(t, s, shown.RequestID, "1"); err != nil {
		t.Fatal(err)
	}
	first.take(t)
	promoted, ok := s.pendingInfo()
	if !ok || promoted.RequestID != formatRequestID(incarn, 2) {
		t.Fatalf("promoted request = %+v, want admission sequence 2", promoted)
	}
	if _, err := answer(t, s, promoted.RequestID, "1"); err != nil {
		t.Fatal(err)
	}
	if got := selectedOption(t, second); got != "allow-once" {
		t.Fatalf("second selected %q", got)
	}
}

// Invalid option keys and stale ids leave the shown request untouched, and a
// delivered token cannot be delivered again.
func TestCopilotPermissionAnswerFencing(t *testing.T) {
	s := copilotSession(t)
	p, cancel := admitCopilot(t, s, "t", vibePermOptions())
	defer cancel()
	shown, _ := s.pendingInfo()
	if _, _, err := s.prepareResolve(shown.RequestID, "9"); err == nil {
		t.Fatal("invalid option key was accepted")
	}
	if _, _, err := s.prepareResolve("stale:1", "1"); err != ErrStalePermission {
		t.Fatalf("stale request id err = %v", err)
	}
	tok, _, err := s.prepareResolve(shown.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.deliver(tok); err != nil {
		t.Fatal(err)
	}
	if err := s.deliver(tok); err == nil {
		t.Fatal("repeated delivery of the same token was accepted")
	}
	if got := selectedOption(t, p); got != "allow-once" {
		t.Fatalf("selected %q", got)
	}
}

// Cancelling the shown request promotes the next one; cancelling a queued
// request removes it without touching the shown one or sending a decision.
func TestCopilotContextCancellationCleansOnlyItsOwnRequest(t *testing.T) {
	t.Run("head cancelled promotes successor", func(t *testing.T) {
		s := copilotSession(t)
		head, cancelHead := admitCopilot(t, s, "h", oneAllow())
		next, cancelNext := admitCopilot(t, s, "n", oneAllow())
		defer cancelNext()
		cancelHead()
		head.take(t)
		waitFor(t, "successor promoted", func() bool {
			cur, ok := s.pendingInfo()
			return ok && cur.RequestID == next.pending.RequestID
		})
		if _, err := answer(t, s, next.pending.RequestID, "1"); err != nil {
			t.Fatal(err)
		}
		if got := selectedOption(t, next); got != "allow-once" {
			t.Fatalf("successor selected %q", got)
		}
	})
	t.Run("queued cancelled is removed", func(t *testing.T) {
		s := copilotSession(t)
		head, cancelHead := admitCopilot(t, s, "h", oneAllow())
		defer cancelHead()
		queued, cancelQueued := admitCopilot(t, s, "q", oneAllow())
		cancelQueued()
		queued.take(t)
		waitFor(t, "queued removed", func() bool { return copilotWaiting(s) == 1 })
		shown, _ := s.pendingInfo()
		if shown.RequestID != head.pending.RequestID {
			t.Fatalf("cancelling a queued request changed the shown one: %q", shown.RequestID)
		}
		if _, err := answer(t, s, shown.RequestID, "1"); err != nil {
			t.Fatal(err)
		}
		if got := selectedOption(t, head); got != "allow-once" {
			t.Fatalf("head selected %q", got)
		}
	})
}

// Ending the turn, interrupting it, losing the process, stopping the session
// and retiring it on /clear all release every held request as cancelled.
func TestCopilotReleasesEveryWaiter(t *testing.T) {
	cases := map[string]func(s *Session){
		"turn completion": func(s *Session) { s.endTurn(sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil) },
		"turn interrupted": func(s *Session) {
			s.endTurn(sdk.PromptResponse{}, errors.New("turn interrupted by supervisor"))
		},
		"turn failure": func(s *Session) { s.endTurn(sdk.PromptResponse{}, errors.New("agent error")) },
		"process exit": func(s *Session) { s.markExited() },
		"stop":         func(s *Session) { s.closeDone() },
		"retirement":   func(s *Session) { s.setRetired(true); s.closeDone() },
	}
	for name, release := range cases {
		t.Run(name, func(t *testing.T) {
			s := copilotSession(t)
			s.turnActive = true
			var parked []*parkedPerm
			for _, id := range []string{"a", "b", "c"} {
				p, cancel := admitCopilot(t, s, id, oneAllow())
				t.Cleanup(cancel)
				parked = append(parked, p)
			}
			release(s)
			for i, p := range parked {
				if resp := p.take(t); resp.Outcome.Cancelled == nil {
					t.Fatalf("waiter %d outcome = %+v, want cancelled", i, resp.Outcome)
				}
			}
			if copilotWaiting(s) != 0 {
				t.Fatalf("%d requests survived %s", copilotWaiting(s), name)
			}
		})
	}
}

// A bounded queue refuses overflow as a cancelled request with a visible
// error, and releasing the turn leaves nothing behind.
func TestCopilotQueueIsBounded(t *testing.T) {
	s := copilotSession(t)
	s.turnActive = true
	var parked []*parkedPerm
	// The shown request plus a queue of cap is the whole held set; one more is
	// the overflow.
	for i := 0; i < copilotPermissionQueueCap+1; i++ {
		p, cancel := admitCopilot(t, s, "t"+strconv.Itoa(i), oneAllow())
		t.Cleanup(cancel)
		parked = append(parked, p)
	}
	// The overflow is refused at once, so it is not waited on for admission.
	last, cancelLast := admitCopilotAgent(t, s, "overflow")
	t.Cleanup(cancelLast)
	parked = append(parked, last)
	if resp := last.take(t); resp.Outcome.Cancelled == nil {
		t.Fatalf("overflow outcome = %+v, want cancelled", resp.Outcome)
	}
	if copilotWaiting(s) != copilotPermissionQueueCap+1 {
		t.Fatalf("held %d, want the shown request plus the cap %d", copilotWaiting(s), copilotPermissionQueueCap)
	}
	sawError := false
	for _, ev := range readEvents(s.logw.Path) {
		if ev.T == "error" && strings.Contains(ev.Error, "permission queue full") {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("overflow left no visible error record")
	}
	s.endTurn(sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil)
	for _, p := range parked[:len(parked)-1] {
		if resp := p.take(t); resp.Outcome.Cancelled == nil {
			t.Fatalf("held request outcome = %+v after turn end", resp.Outcome)
		}
	}
}

// Late tool metadata refreshes the title of a queued request for the same tool
// id, and an unrelated tool does not.
func TestCopilotQueuedRequestRefreshesLateTitle(t *testing.T) {
	s := copilotSession(t)
	head, cancelHead := admitCopilot(t, s, "h", oneAllow())
	defer cancelHead()
	_, cancelQueued := admitCopilot(t, s, "late", oneAllow())
	defer cancelQueued()
	noteTool(t, s, "other", "read", sdk.ToolKindRead, "private")
	noteTool(t, s, "late", "bash", sdk.ToolKindExecute, "git status")
	if _, err := answer(t, s, head.pending.RequestID, "1"); err != nil {
		t.Fatal(err)
	}
	head.take(t)
	promoted, ok := s.pendingInfo()
	if !ok || promoted.Title != "bash `git status`" || promoted.ToolKind != "execute" {
		t.Fatalf("promoted title = %+v, want the late metadata", promoted)
	}
}

// The other harnesses keep the single-request contract: a second concurrent
// request is cancelled with the duplicate error and never queued.
func TestSingleSlotHarnessesStillCancelDuplicates(t *testing.T) {
	for _, agent := range []string{"vibe", "opencode"} {
		t.Run(agent, func(t *testing.T) {
			s := newPermSession(t, agent)
			_, cancelFirst := admitCopilot(t, s, "a", oneAllow())
			defer cancelFirst()
			second, cancelSecond := admitCopilotAgent(t, s, "b")
			defer cancelSecond()
			if resp := second.take(t); resp.Outcome.Cancelled == nil {
				t.Fatalf("second outcome = %+v, want cancelled", resp.Outcome)
			}
			if copilotWaiting(s) != 1 {
				t.Fatalf("held %d requests, want the first only", copilotWaiting(s))
			}
		})
	}
}

// admitCopilotAgent is admitCopilot without the wait: a single-slot harness
// cancels a duplicate immediately, so the caller waits for that cancellation.
func admitCopilotAgent(t *testing.T, s *Session, id string) (*parkedPerm, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &parkedPerm{resp: make(chan sdk.RequestPermissionResponse, 1), cancel: cancel}
	go func() {
		resp, _ := s.RequestPermission(ctx, sdk.RequestPermissionRequest{
			ToolCall: sdk.ToolCallUpdate{ToolCallId: sdk.ToolCallId(id)},
			Options:  oneAllow(),
		})
		p.resp <- resp
	}()
	return p, cancel
}

// A permission request is an approval for the turn that asked for it. Outside
// a live turn, or once its caller or session has gone, it is refused without
// being admitted: the admission counter never moves and nothing is shown.
//
// The call runs on its own goroutine with a bounded wait. A broken gate that
// admits the request blocks it on the approval, so the wait expires and the
// test fails promptly; cleanup then cancels the caller and joins the goroutine,
// so a red run leaves nothing blocked behind.
func TestCopilotRejectsRequestsOutsideALiveTurn(t *testing.T) {
	cases := map[string]func(s *Session, cancel context.CancelFunc){
		"no turn yet": func(s *Session, _ context.CancelFunc) { s.turnActive = false },
		"turn already ended": func(s *Session, _ context.CancelFunc) {
			s.endTurn(sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil)
		},
		"session retired":     func(s *Session, _ context.CancelFunc) { s.setRetired(true) },
		"process exited":      func(s *Session, _ context.CancelFunc) { s.markExited() },
		"session stopped":     func(s *Session, _ context.CancelFunc) { s.closeDone() },
		"caller already gone": func(_ *Session, cancel context.CancelFunc) { cancel() },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			s := copilotSession(t)
			ctx, cancel := context.WithCancel(context.Background())
			setup(s, cancel)
			var resp sdk.RequestPermissionResponse
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				resp, _ = s.RequestPermission(ctx, sdk.RequestPermissionRequest{
					ToolCall: sdk.ToolCallUpdate{ToolCallId: "stale"},
					Options:  oneAllow(),
				})
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-returned:
				case <-time.After(3 * time.Second):
					t.Errorf("stale request goroutine never returned")
				}
			})
			select {
			case <-returned:
			case <-time.After(2 * time.Second):
				t.Fatalf("stale request was admitted and is blocking an approval (pendingSeq=%d)", s.pendingSeq)
			}
			if resp.Outcome.Cancelled == nil {
				t.Fatalf("outcome = %+v, want cancelled", resp.Outcome)
			}
			s.mu.Lock()
			seq := s.pendingSeq
			s.mu.Unlock()
			if seq != 0 {
				t.Fatalf("stale request was admitted: sequence advanced to %d", seq)
			}
			if _, ok := s.pendingInfo(); ok {
				t.Fatal("a stale request became the shown approval")
			}
		})
	}
}

// A late request from a finished turn cannot touch a request admitted in the
// next turn: release is by identity, so the old request's cleanup finds only
// its own (already released) entry.
func TestCopilotStaleCleanupCannotTouchNewTurn(t *testing.T) {
	s := copilotSession(t)
	old, cancelOld := admitCopilot(t, s, "old", oneAllow())
	defer cancelOld()
	s.endTurn(sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil)
	if resp := old.take(t); resp.Outcome.Cancelled == nil {
		t.Fatalf("old turn's request outcome = %+v, want cancelled", resp.Outcome)
	}
	s.mu.Lock()
	s.turnActive = true
	s.mu.Unlock()
	fresh, cancelFresh := admitCopilot(t, s, "fresh", oneAllow())
	defer cancelFresh()
	// Whichever order the old request's deferred cleanup runs in, the new
	// turn's shown request must survive it.
	for i := 0; i < 50; i++ {
		cur, ok := s.pendingInfo()
		if !ok || cur.RequestID != fresh.pending.RequestID {
			t.Fatalf("new turn's request displaced by stale cleanup: %+v", cur)
		}
	}
	shown, _ := s.pendingInfo()
	if _, err := answer(t, s, shown.RequestID, "1"); err != nil {
		t.Fatal(err)
	}
	if got := selectedOption(t, fresh); got != "allow-once" {
		t.Fatalf("fresh selected %q", got)
	}
}

// A queued request cancelled before promotion is never shown, and the shown
// request still promotes the next live one, not the cancelled entry.
func TestCopilotCancelledQueuedRequestIsNeverPromoted(t *testing.T) {
	s := copilotSession(t)
	head, cancelHead := admitCopilot(t, s, "h", oneAllow())
	defer cancelHead()
	_, cancelQueued := admitCopilot(t, s, "q", oneAllow())
	next, cancelNext := admitCopilot(t, s, "n", oneAllow())
	defer cancelNext()
	cancelQueued()
	waitFor(t, "cancelled entry removed", func() bool { return copilotWaiting(s) == 2 })
	shown, _ := s.pendingInfo()
	if _, err := answer(t, s, shown.RequestID, "1"); err != nil {
		t.Fatal(err)
	}
	head.take(t)
	promoted, ok := s.pendingInfo()
	if !ok || promoted.RequestID != next.pending.RequestID {
		t.Fatalf("promoted %+v, want the live request %q", promoted, next.pending.RequestID)
	}
}

// A token prepared for a request that is cancelled before delivery cannot be
// delivered to its successor.
func TestCopilotTokenPreparedBeforeCancellationIsRefused(t *testing.T) {
	s := copilotSession(t)
	head, cancelHead := admitCopilot(t, s, "h", oneAllow())
	defer cancelHead()
	next, cancelNext := admitCopilot(t, s, "n", oneAllow())
	defer cancelNext()
	shown, _ := s.pendingInfo()
	tok, _, err := s.prepareResolve(shown.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	cancelHead()
	head.take(t)
	waitFor(t, "successor shown", func() bool {
		cur, ok := s.pendingInfo()
		return ok && cur.RequestID == next.pending.RequestID
	})
	if err := s.deliver(tok); err == nil {
		t.Fatal("token for a cancelled request was delivered to its successor")
	}
	if got, _ := s.pendingInfo(); got.RequestID != next.pending.RequestID {
		t.Fatalf("successor changed by a stale token: %+v", got)
	}
}

// permOutcome is what the fake agent's blocked permission call returned.
type permOutcome struct {
	resp sdk.RequestPermissionResponse
	err  error
}

// heldPrompt is a turn whose only step is one permission request. It blocks
// until the client answers or releases it, then reports what it got.
func heldPrompt(outcomes chan<- permOutcome) func(*fakeAgent, context.Context, sdk.PromptRequest) (sdk.PromptResponse, error) {
	return func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
		resp, err := a.conn.RequestPermission(ctx, sdk.RequestPermissionRequest{
			SessionId: p.SessionId,
			ToolCall:  sdk.ToolCallUpdate{ToolCallId: "held"},
			Options:   oneAllow(),
		})
		outcomes <- permOutcome{resp: resp, err: err}
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	}
}

// awaitOutcome waits, bounded, for the agent to receive the held request's
// outcome, so a red run fails rather than hanging.
func awaitOutcome(t *testing.T, outcomes <-chan permOutcome) permOutcome {
	t.Helper()
	select {
	case got := <-outcomes:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("held permission was never released to the agent")
		return permOutcome{}
	}
}

// assertReleasedUnanswered checks the agent's view of a released request: no
// option was chosen, and the call ended as a cancelled outcome, the protocol's
// request-cancelled error (the SDK's answer to a cancelled prompt), or a peer
// that left because the session was stopped.
func assertReleasedUnanswered(t *testing.T, got permOutcome) {
	t.Helper()
	if got.resp.Outcome.Selected != nil {
		t.Fatalf("released request was answered: %+v", got.resp.Outcome)
	}
	cancelledOutcome := got.err == nil && got.resp.Outcome.Cancelled != nil
	cancelledError := got.err != nil && strings.Contains(got.err.Error(), "-32800")
	// A stop tears the transport down, so the agent sees the peer leave.
	peerGone := got.err != nil && strings.Contains(got.err.Error(), "peer disconnected")
	if !cancelledOutcome && !cancelledError && !peerGone {
		t.Fatalf("released request outcome = %+v err=%v, want cancelled", got.resp.Outcome, got.err)
	}
}

// Through the real manager, a held copilot request is released as cancelled
// when the turn is interrupted and when the session is stopped, and nothing is
// left shown afterwards.
func TestCopilotHeldPermissionLifecycleThroughManager(t *testing.T) {
	cases := map[string]func(m *Manager, node string) error{
		"interrupt": func(m *Manager, node string) error { return m.Interrupt(node) },
		"kill":      func(m *Manager, node string) error { return m.Kill(node) },
	}
	for name, release := range cases {
		t.Run(name, func(t *testing.T) {
			outcomes := make(chan permOutcome, 4)
			agent := &fakeAgent{prompt: heldPrompt(outcomes)}
			m := NewManagerWithRunner(t.TempDir(), fakeRunner(agent))
			node := "cop-" + name
			if _, err := m.Launch(node, "copilot", t.TempDir(), "", ""); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(m.Shutdown)
			if err := m.Send(node, "question"); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "permission held", func() bool { _, ok := m.Pending(node); return ok })
			sess := m.session(node)
			if err := release(m, node); err != nil {
				t.Fatal(err)
			}
			assertReleasedUnanswered(t, awaitOutcome(t, outcomes))
			// The turn's own writer (endTurn) must finish before the temporary log
			// directory is removed; wait for the turn state, bounded.
			waitFor(t, "turn writer settled", func() bool {
				sess.mu.Lock()
				defer sess.mu.Unlock()
				return !sess.turnActive
			})
			// The agent can see its outcome before the client has cleared the
			// shown request, so the clearing is awaited as a state, bounded.
			waitFor(t, "released request no longer shown", func() bool { _, ok := m.Pending(node); return !ok })
		})
	}
}

// /clear cannot replace a copilot session while its turn holds a permission:
// the held approval belongs to that turn. After the turn is interrupted the
// held request is released and the clear proceeds.
func TestCopilotClearWaitsForHeldPermission(t *testing.T) {
	outcomes := make(chan permOutcome, 4)
	agent := &fakeAgent{prompt: heldPrompt(outcomes)}
	m := NewManagerWithRunner(t.TempDir(), fakeRunner(agent))
	const node = "cop-clear"
	if _, err := m.Launch(node, "copilot", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	if err := m.Send(node, "question"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "permission held", func() bool { _, ok := m.Pending(node); return ok })
	if err := m.Clear(node); !errors.Is(err, ErrTurnActive) {
		t.Fatalf("Clear during a held permission = %v, want ErrTurnActive", err)
	}
	if _, ok := m.Pending(node); !ok {
		t.Fatal("refused clear released the held request")
	}
	if err := m.Interrupt(node); err != nil {
		t.Fatal(err)
	}
	assertReleasedUnanswered(t, awaitOutcome(t, outcomes))
	// The agent can see the release before the client finishes ending the turn;
	// wait for the turn's own state, bounded, before the clear that needs it.
	waitFor(t, "turn ended", func() bool {
		sess := m.session(node)
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return !sess.turnActive
	})
	if err := m.Clear(node); err != nil {
		t.Fatalf("Clear after release = %v", err)
	}
}

// copilotOptionWithMeta is one model menu entry carrying the given _meta. The
// id and name are synthetic; only the metadata shape is under test.
func copilotOptionWithMeta(id string, meta map[string]any) sdk.SessionConfigSelectOption {
	return sdk.SessionConfigSelectOption{Value: sdk.SessionConfigValueId(id), Name: id, Meta: meta}
}

func copilotModelMenuOf(current string, opts ...sdk.SessionConfigSelectOption) []sdk.SessionConfigOption {
	cat := sdk.SessionConfigOptionCategoryModel
	list := sdk.SessionConfigSelectOptionsUngrouped(opts)
	return []sdk.SessionConfigOption{{Select: &sdk.SessionConfigOptionSelect{
		Id: "model", Name: "Model", CurrentValue: sdk.SessionConfigValueId(current), Category: &cat,
		Options: sdk.SessionConfigSelectOptions{Ungrouped: &list},
	}}}
}

// Copilot marks each advertised option with _meta.copilotEnablement. "enabled"
// is the value observed on an offered model; an option with no enablement key
// makes no availability claim and is offered. Any other value, or a non-string
// one, is not offered: scimux does not guess what an unverified state allows.
// Synthetic ids only; the metadata shape is the one the CLI sends.
func TestCopilotCatalogRespectsEnablementMetadata(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]any
		kept bool
	}{
		{name: "no enablement key is offered", meta: map[string]any{"copilotUsage": "1x"}, kept: true},
		{name: "enabled is offered", meta: map[string]any{"copilotEnablement": "enabled"}, kept: true},
		{name: "other state is not offered", meta: map[string]any{"copilotEnablement": "disabled"}},
		{name: "empty state is not offered", meta: map[string]any{"copilotEnablement": ""}},
		{name: "non-string state is not offered", meta: map[string]any{"copilotEnablement": true}},
		{name: "null state is not offered", meta: map[string]any{"copilotEnablement": nil}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cat := copilotCatalogFrom(copilotModelMenuOf("auto",
				copilotOptionWithMeta("auto", nil),
				copilotOptionWithMeta("probe", c.meta),
			))
			if got := slices.Contains(cat.Models, "probe"); got != c.kept {
				t.Fatalf("models = %v, offered=%v want %v", cat.Models, got, c.kept)
			}
		})
	}
}

// A duplicate id is offered as the enabled entry when one exists, so a stale
// unenabled copy never hides an available model.
func TestCopilotDuplicateIdKeepsTheEnabledEntry(t *testing.T) {
	cat := copilotCatalogFrom(copilotModelMenuOf("auto",
		copilotOptionWithMeta("auto", map[string]any{"copilotEnablement": "disabled"}),
		copilotOptionWithMeta("auto", map[string]any{"copilotEnablement": "enabled"}),
	))
	if strings.Join(cat.Models, ",") != "auto" {
		t.Fatalf("models = %v, want the enabled auto once", cat.Models)
	}
	only := copilotCatalogFrom(copilotModelMenuOf("auto",
		copilotOptionWithMeta("auto", map[string]any{"copilotEnablement": "disabled"}),
	))
	if len(only.Models) != 0 {
		t.Fatalf("a model with only unenabled entries was offered: %v", only.Models)
	}
}

// The same availability rule governs an explicit choice: a model the catalog
// would not offer is refused at launch, and so is an effort that is not offered.
func TestCopilotExplicitChoiceFollowsAvailability(t *testing.T) {
	cases := []struct {
		name    string
		opts    []sdk.SessionConfigOption
		model   string
		effort  string
		wantErr bool
	}{
		{name: "offered model is set", opts: copilotModelMenuOf("auto",
			copilotOptionWithMeta("auto", nil),
			copilotOptionWithMeta("alpha", map[string]any{"copilotEnablement": "enabled"})), model: "alpha"},
		{name: "unenabled model is refused", opts: copilotModelMenuOf("auto",
			copilotOptionWithMeta("auto", nil),
			copilotOptionWithMeta("beta", map[string]any{"copilotEnablement": "disabled"})), model: "beta", wantErr: true},
		{name: "unenabled effort is refused", opts: copilotMenusWithEffortMeta("auto", "high",
			map[string]any{"copilotEnablement": "disabled"}), effort: "high", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
				return sdk.NewSessionResponse{SessionId: "s1", ConfigOptions: c.opts}
			}}
			agent.configReply = func(sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
				return sdk.SetSessionConfigOptionResponse{ConfigOptions: c.opts}, nil
			}
			m := NewManagerWithRunner(t.TempDir(), fakeRunner(agent))
			_, err := m.Launch("cop-avail", "copilot", t.TempDir(), c.model, c.effort)
			if (err != nil) != c.wantErr {
				t.Fatalf("Launch err = %v, wantErr %v", err, c.wantErr)
			}
			if err == nil {
				t.Cleanup(m.Shutdown)
			}
		})
	}
}

// copilotMenusWithEffortMeta is an effort menu for the current model whose one
// level carries the given _meta, with a plain offered "auto" model.
func copilotMenusWithEffortMeta(current, level string, meta map[string]any) []sdk.SessionConfigOption {
	opts := copilotMenus(current, []string{current}, level, []string{level})
	list := sdk.SessionConfigSelectOptionsUngrouped{copilotOptionWithMeta(level, meta)}
	opts[1].Select.Options.Ungrouped = &list
	return opts
}

// A disabled current model cannot carry an effort menu: the menu describes a
// model the user cannot choose.
func TestCopilotDisabledCurrentModelGetsNoEffortMenu(t *testing.T) {
	opts := copilotMenus("probe", []string{"auto", "probe"}, "low", []string{"low", "high"})
	// Re-issue the model menu with probe not offered.
	opts[0].Select.Options.Ungrouped = &sdk.SessionConfigSelectOptionsUngrouped{
		copilotOptionWithMeta("auto", nil),
		copilotOptionWithMeta("probe", map[string]any{"copilotEnablement": "disabled"}),
	}
	cat := copilotCatalogFrom(opts)
	if len(cat.Efforts) != 0 {
		t.Fatalf("efforts = %v for a disabled current model", cat.Efforts)
	}
}

// After a model switch the effort menu is the one the switch returned, not
// session/new's. An effort valid only before the switch is refused; one the new
// menu offers is applied. A failing config RPC is never swallowed.
func TestCopilotLaunchEffortUsesPostSwitchMenu(t *testing.T) {
	before := copilotMenus("auto", []string{"auto", "alpha"}, "low", []string{"low"})
	after := copilotMenus("alpha", []string{"auto", "alpha"}, "high", []string{"high", "max"})
	cases := []struct {
		name    string
		effort  string
		setErr  error
		wantErr bool
		wantSet []string
	}{
		{name: "post-switch level applies", effort: "max", wantSet: []string{"model=alpha", "reasoning_effort=max"}},
		{name: "pre-switch level is refused", effort: "low", wantErr: true},
		{name: "effort RPC failure propagates", effort: "max", setErr: errors.New("transport refused"), wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
				return sdk.NewSessionResponse{SessionId: "s1", ConfigOptions: before}
			}}
			var sets []string
			agent.configReply = func(p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
				if p.ValueId == nil {
					return sdk.SetSessionConfigOptionResponse{}, errors.New("unexpected config request")
				}
				sets = append(sets, string(p.ValueId.ConfigId)+"="+string(p.ValueId.Value))
				if string(p.ValueId.ConfigId) == "reasoning_effort" && c.setErr != nil {
					return sdk.SetSessionConfigOptionResponse{}, c.setErr
				}
				if string(p.ValueId.ConfigId) == "model" {
					return sdk.SetSessionConfigOptionResponse{ConfigOptions: after}, nil
				}
				return sdk.SetSessionConfigOptionResponse{ConfigOptions: after}, nil
			}
			m := NewManagerWithRunner(t.TempDir(), fakeRunner(agent))
			_, err := m.Launch("cop-eff", "copilot", t.TempDir(), "alpha", c.effort)
			if (err != nil) != c.wantErr {
				t.Fatalf("Launch err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			t.Cleanup(m.Shutdown)
			if strings.Join(sets, ";") != strings.Join(c.wantSet, ";") {
				t.Fatalf("config sets = %v, want %v", sets, c.wantSet)
			}
		})
	}
}

// A failing model switch fails the launch with the agent's error, rather than
// being read as "the default model".
func TestCopilotLaunchModelRPCFailurePropagates(t *testing.T) {
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		return sdk.NewSessionResponse{SessionId: "s1", ConfigOptions: copilotMenus("auto", []string{"auto", "alpha"}, "", nil)}
	}}
	agent.configReply = func(sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
		return sdk.SetSessionConfigOptionResponse{}, errors.New("transport refused")
	}
	m := NewManagerWithRunner(t.TempDir(), fakeRunner(agent))
	if _, err := m.Launch("cop-mfail", "copilot", t.TempDir(), "alpha", ""); err == nil {
		t.Fatal("a failed model switch launched anyway")
	}
	if m.HasSession("cop-mfail") {
		t.Fatal("a failed launch registered a session")
	}
}

// Copilot advertises an "autopilot" session mode. scimux never enables it: an
// effort choice is matched against the effort menu only, and a name that
// happens to match a mode is refused rather than switching the mode.
func TestCopilotNeverSwitchesSessionMode(t *testing.T) {
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		return sdk.NewSessionResponse{
			SessionId:     "s1",
			ConfigOptions: copilotMenus("auto", []string{"auto"}, "", nil),
			Modes: &sdk.SessionModeState{CurrentModeId: "https://agentclientprotocol.com/protocol/session-modes#agent",
				AvailableModes: []sdk.SessionMode{{Id: "https://agentclientprotocol.com/protocol/session-modes#autopilot", Name: "autopilot"}}},
		}
	}}
	m := NewManagerWithRunner(t.TempDir(), fakeRunner(agent))
	if _, err := m.Launch("cop-mode", "copilot", t.TempDir(), "", "autopilot"); err == nil {
		t.Fatal("an effort named like a mode was accepted")
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.lastModeSet != nil {
		t.Fatalf("copilot switched session mode to %q", agent.lastModeSet.ModeId)
	}
}

// A model id that appears twice is offered when any of its entries is offered,
// and an explicit choice of it is accepted by the same rule. The first entry's
// availability never decides alone, in either order.
func TestCopilotMixedDuplicateAvailability(t *testing.T) {
	disabled := map[string]any{"copilotEnablement": "disabled"}
	enabled := map[string]any{"copilotEnablement": "enabled"}
	cases := []struct {
		name    string
		entries []sdk.SessionConfigSelectOption
		offered bool
	}{
		{name: "disabled then enabled is offered", offered: true, entries: []sdk.SessionConfigSelectOption{
			copilotOptionWithMeta("alpha", disabled), copilotOptionWithMeta("alpha", enabled)}},
		{name: "enabled then disabled is offered", offered: true, entries: []sdk.SessionConfigSelectOption{
			copilotOptionWithMeta("alpha", enabled), copilotOptionWithMeta("alpha", disabled)}},
		{name: "only disabled entries are not offered", entries: []sdk.SessionConfigSelectOption{
			copilotOptionWithMeta("alpha", disabled), copilotOptionWithMeta("alpha", disabled)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			menu := copilotModelMenuOf("auto", append([]sdk.SessionConfigSelectOption{copilotOptionWithMeta("auto", nil)}, c.entries...)...)
			if got := slices.Contains(copilotCatalogFrom(menu).Models, "alpha"); got != c.offered {
				t.Fatalf("catalog offers alpha=%v, want %v", got, c.offered)
			}
			agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
				return sdk.NewSessionResponse{SessionId: "s1", ConfigOptions: menu}
			}}
			agent.configReply = func(sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
				return sdk.SetSessionConfigOptionResponse{ConfigOptions: menu}, nil
			}
			m := NewManagerWithRunner(t.TempDir(), fakeRunner(agent))
			_, err := m.Launch("cop-dup", "copilot", t.TempDir(), "alpha", "")
			if (err == nil) != c.offered {
				t.Fatalf("explicit alpha launch err = %v, want offered=%v", err, c.offered)
			}
			if err == nil {
				t.Cleanup(m.Shutdown)
			}
		})
	}
}

// The public catalog probe refuses an empty binary and reports a missing one
// as a start failure that names the path it tried. Nothing is executed: the
// path does not exist, so the start fails before any agent could run.
func TestProbeCopilotCatalogRefusesMissingBinaries(t *testing.T) {
	if _, err := ProbeCopilotCatalog(context.Background(), ""); err == nil {
		t.Fatal("an empty binary was probed")
	}
	missing := filepath.Join(t.TempDir(), "no-copilot-here")
	_, err := ProbeCopilotCatalog(context.Background(), missing)
	if err == nil || !strings.Contains(err.Error(), "start copilot ACP") || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing binary error = %v, want a start failure naming %s", err, missing)
	}
}

// The effort menu is identified by its standard thought-level category or by
// an id or name spelled as effort. Any other select is not an effort menu, so
// a requested level is refused rather than set on the wrong option.
func TestCopilotEffortMenuIdentification(t *testing.T) {
	thought := sdk.SessionConfigOptionCategoryThoughtLevel
	mode := sdk.SessionConfigOptionCategoryMode
	levels := func(sel *sdk.SessionConfigOptionSelect) {
		list := sdk.SessionConfigSelectOptionsUngrouped{
			{Value: "lvl-low", Name: "Low"}, {Value: "lvl-high", Name: "High"},
		}
		sel.Options.Ungrouped = &list
	}
	cases := []struct {
		name    string
		sel     sdk.SessionConfigOptionSelect
		effort  string
		wantSet string
		wantErr bool
	}{
		{name: "thought-level category is the effort menu", effort: "lvl-high", wantSet: "lvl-high",
			sel: sdk.SessionConfigOptionSelect{Id: "lvl", Name: "Level", CurrentValue: "lvl-low", Category: &thought}},
		{name: "an id spelled as effort is the effort menu", effort: "lvl-high", wantSet: "lvl-high",
			sel: sdk.SessionConfigOptionSelect{Id: "effort", Name: "Level", CurrentValue: "lvl-low"}},
		{name: "a name spelled as effort is the effort menu", effort: "lvl-high", wantSet: "lvl-high",
			sel: sdk.SessionConfigOptionSelect{Id: "lvl", Name: "Reasoning Effort", CurrentValue: "lvl-low"}},
		{name: "a mode-category select is not an effort menu", effort: "lvl-high", wantErr: true,
			sel: sdk.SessionConfigOptionSelect{Id: "lvl", Name: "Level", CurrentValue: "lvl-low", Category: &mode}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sel := c.sel
			levels(&sel)
			menu := append(copilotMenus("auto", []string{"auto"}, "", nil), sdk.SessionConfigOption{Select: &sel})
			agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
				return sdk.NewSessionResponse{SessionId: "s1", ConfigOptions: menu}
			}}
			var set string
			agent.configReply = func(p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
				if p.ValueId != nil && string(p.ValueId.ConfigId) == string(sel.Id) {
					set = string(p.ValueId.Value)
				}
				return sdk.SetSessionConfigOptionResponse{ConfigOptions: menu}, nil
			}
			m := NewManagerWithRunner(t.TempDir(), fakeRunner(agent))
			_, err := m.Launch("cop-cat", "copilot", t.TempDir(), "", c.effort)
			if (err != nil) != c.wantErr {
				t.Fatalf("Launch err = %v, wantErr %v", err, c.wantErr)
			}
			if err == nil {
				t.Cleanup(m.Shutdown)
				if set != c.wantSet {
					t.Fatalf("effort set to %q, want %q", set, c.wantSet)
				}
			}
		})
	}
}

// The catalog probe reports a failed initialize, a session/new that returns no
// session id, and a cancelled parent as errors, closes nothing it did not open,
// and never prompts. Each runs over an in-process fake peer.
func TestCopilotCatalogProbeFailureOutcomes(t *testing.T) {
	cases := map[string]struct {
		agent  *fakeAgent
		cancel bool
	}{
		"session/new returns no session": {agent: copilotCatalogAgent(nil)},
		"cancelled parent context":       {agent: copilotCatalogAgent(copilotMenus("auto", []string{"auto"}, "", nil)), cancel: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "session/new returns no session" {
				c.agent.newSession = func() sdk.NewSessionResponse { return sdk.NewSessionResponse{} }
			}
			ctx, cancel := context.WithCancel(context.Background())
			if c.cancel {
				cancel()
			} else {
				t.Cleanup(cancel)
			}
			var cp *countingProcess
			start := func(dir string) (Process, error) {
				r1, w1 := io.Pipe()
				r2, w2 := io.Pipe()
				c.agent.conn = sdk.NewAgentSideConnection(c.agent, w2, r1)
				p := &countingProcess{fakeProcess: &fakeProcess{stdin: w1, stdout: r2, r1: r1, w2: w2, closed: make(chan struct{})}}
				cp = p
				return p, nil
			}
			_, err := probeCopilotCatalog(ctx, start, copilotProbeTotal, copilotProbeInitialize, copilotProbePerCall)
			if err == nil {
				t.Fatal("the probe reported success")
			}
			waitFor(t, "probe process reaped", func() bool {
				return atomic.LoadInt32(&cp.kills) >= 1 && atomic.LoadInt32(&cp.waits) >= 1
			})
			c.agent.mu.Lock()
			defer c.agent.mu.Unlock()
			if c.agent.prompts != 0 {
				t.Fatalf("probe prompted %d times", c.agent.prompts)
			}
		})
	}
}

// The catalog probe's own failure paths, each with a meaningful outcome: a
// process that cannot start is an error and nothing is reaped; a peer that
// leaves before answering initialize is an error; and a probe whose budget is
// spent after initialize opens no session at all.
func TestCopilotCatalogProbeStartAndInitializeOutcomes(t *testing.T) {
	t.Run("process fails to start", func(t *testing.T) {
		start := func(string) (Process, error) { return nil, errors.New("exec refused") }
		_, err := probeCopilotCatalog(context.Background(), start, copilotProbeTotal, copilotProbeInitialize, copilotProbePerCall)
		if err == nil || !strings.Contains(err.Error(), "exec refused") {
			t.Fatalf("start failure = %v, want the start error", err)
		}
	})
	t.Run("peer leaves before initialize", func(t *testing.T) {
		start := func(string) (Process, error) {
			r1, w1 := io.Pipe()
			r2, w2 := io.Pipe()
			// The agent side closes at once, so initialize can never be answered.
			_ = w2.Close()
			_ = r1.Close()
			return &fakeProcess{stdin: w1, stdout: r2, r1: r1, w2: w2, closed: make(chan struct{})}, nil
		}
		_, err := probeCopilotCatalog(context.Background(), start, copilotProbeTotal, copilotProbeInitialize, copilotProbePerCall)
		if err == nil {
			t.Fatal("an unanswered initialize reported success")
		}
	})
	t.Run("budget spent after initialize opens no session", func(t *testing.T) {
		agent := copilotCatalogAgent(copilotMenus("auto", []string{"auto"}, "", nil))
		agent.initDelay = 210 * time.Millisecond
		var cp *countingProcess
		start := func(string) (Process, error) {
			r1, w1 := io.Pipe()
			r2, w2 := io.Pipe()
			agent.conn = sdk.NewAgentSideConnection(agent, w2, r1)
			p := &countingProcess{fakeProcess: &fakeProcess{stdin: w1, stdout: r2, r1: r1, w2: w2, closed: make(chan struct{})}}
			cp = p
			return p, nil
		}
		// 300ms total: initialize (up to 280ms) leaves about 90ms, short of the
		// 100ms session/new slice, so the probe must stop before session/new.
		_, err := probeCopilotCatalog(context.Background(), start, 300*time.Millisecond, 280*time.Millisecond, 100*time.Millisecond)
		if err == nil {
			t.Fatal("a spent budget reported success")
		}
		waitFor(t, "probe process reaped", func() bool {
			return atomic.LoadInt32(&cp.kills) >= 1 && atomic.LoadInt32(&cp.waits) >= 1
		})
		agent.mu.Lock()
		defer agent.mu.Unlock()
		if agent.lastNew != nil {
			t.Fatal("the probe opened a session after its budget was spent")
		}
	})
}

// A config option that is not a select menu is not a model or effort menu and
// is ignored, while the real menus are still read.
func TestCopilotCatalogIgnoresNonSelectOptions(t *testing.T) {
	menu := append([]sdk.SessionConfigOption{{}}, copilotMenus("auto", []string{"auto", "alpha"}, "", nil)...)
	cat := copilotCatalogFrom(menu)
	if strings.Join(cat.Models, ",") != "auto,alpha" {
		t.Fatalf("models = %v, want the model select's offer", cat.Models)
	}
}

// When the scratch directory cannot be created, the probe stops before it
// starts any process: start is never called and the caller gets an error.
func TestCopilotCatalogProbeStopsWhenNoScratchDirectory(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	started := false
	start := func(string) (Process, error) {
		started = true
		return nil, errors.New("must not start without a scratch directory")
	}
	if _, err := probeCopilotCatalog(context.Background(), start, copilotProbeTotal, copilotProbeInitialize, copilotProbePerCall); err == nil {
		t.Fatal("a probe without a scratch directory reported success")
	}
	if started {
		t.Fatal("the probe started a process without its scratch directory")
	}
}
