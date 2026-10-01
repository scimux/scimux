package acp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

// Vibe's permission request carries a tool-call id and no title. The title and
// kind the chat, the key audit, and the decision record show come from the
// latest tool_call / tool_call_update with that id. These frames are synthetic.

func TestSparsePermissionUsesLatestToolCall(t *testing.T) {
	s := newPermSession(t, "vibe")
	s.assistant.WriteString("please run the user's prompt literally")
	noteTool(t, s, "c1", "bash", sdk.ToolKindExecute, map[string]any{"command": "git", "args": "status --short"})
	noteToolUpdate(t, s, "c1", "bash", "execute", map[string]any{"command": "git", "args": "status"})

	parked := parkSparse(t, s, "c1", nil, nil, vibePermOptions())
	defer parked.stop(t)
	p := parked.pending

	if p.Title != "bash `git status`" {
		t.Fatalf("pending title = %q, want the latest command", p.Title)
	}
	if p.ToolKind != "execute" {
		t.Fatalf("pending kind = %q, want execute", p.ToolKind)
	}
	if strings.Contains(p.Title, "please run") {
		t.Fatalf("title inferred from the prompt: %q", p.Title)
	}
	wantNames := []string{"Allow once", "Always for this session", "Always for this project", "Reject"}
	wantKinds := []string{"allow", "allow_always", "allow_always", "reject"}
	if len(p.Options) != len(wantNames) {
		t.Fatalf("options = %+v", p.Options)
	}
	for i := range wantNames {
		if p.Options[i].Name != wantNames[i] || p.Options[i].Kind != wantKinds[i] || p.Options[i].Key != strconv.Itoa(i+1) {
			t.Fatalf("option %d = %+v", i, p.Options[i])
		}
	}
	tok2, evidence, err := s.prepareResolve(parked.pending.RequestID, "2")
	if err != nil {
		t.Fatal(err)
	}
	tok3, _, err := s.prepareResolve(parked.pending.RequestID, "3")
	if err != nil {
		t.Fatal(err)
	}
	if evidence != "permission: bash `git status`" {
		t.Fatalf("audit evidence = %q", evidence)
	}
	if !strings.HasSuffix(string(tok2), ":allow-always-session") || !strings.HasSuffix(string(tok3), ":allow-always-project") {
		t.Fatalf("tokens lost the agent's option ids: %q %q", tok2, tok3)
	}
	if _, _, err := s.prepareResolve("stale-request", "2"); err != ErrStalePermission {
		t.Fatalf("stale request id err = %v", err)
	}
	if err := s.deliver(sdk.PermissionOptionId("other-incarn:1:allow-always-session")); err == nil {
		t.Fatal("stale deliver token was accepted")
	}
	still, ok := s.pendingInfo()
	if !ok || still.Title != parked.pending.Title || still.RequestID != parked.pending.RequestID {
		t.Fatalf("stale deliver changed the pending request: %+v", still)
	}
	if err := s.deliver(tok2); err != nil {
		t.Fatal(err)
	}
	resp := parked.take(t)
	if resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId != "allow-always-session" {
		t.Fatalf("selected = %+v, want allow-always-session", resp.Outcome.Selected)
	}

	var logged []string
	for _, ev := range readEvents(s.logw.Path) {
		if ev.T == "tool" && ev.Tool != nil {
			logged = append(logged, ev.Tool.Title)
		}
	}
	if len(logged) != 2 || logged[0] != "bash" || logged[1] != "bash" {
		t.Fatalf("tool log was rewritten: %q", logged)
	}
}

func TestSparsePermissionRefreshesAfterLateToolCall(t *testing.T) {
	s := newPermSession(t, "vibe")
	parked := parkSparse(t, s, "late", nil, nil, oneAllow())
	defer parked.stop(t)
	if parked.pending.Title != UnknownToolTitle {
		t.Fatalf("initial title = %q", parked.pending.Title)
	}
	noteTool(t, s, "other", "read", sdk.ToolKindRead, "private")
	before, ok := s.pendingInfo()
	if !ok || before.Title != UnknownToolTitle {
		t.Fatalf("unrelated update changed pending: %+v", before)
	}
	noteTool(t, s, "late", "bash", sdk.ToolKindExecute, "git status")
	after, ok := s.pendingInfo()
	if !ok || after.RequestID != parked.pending.RequestID || after.Title != "bash `git status`" || after.ToolKind != "execute" {
		t.Fatalf("late tool update = %+v", after)
	}
}

func TestPermissionToolRecallEdges(t *testing.T) {
	t.Run("multiple ids and out of order", func(t *testing.T) {
		s := newPermSession(t, "pi")
		noteToolUpdate(t, s, "late", "", "", map[string]any{"cmd": "echo", "args": "late"})
		noteTool(t, s, "late", "bash", sdk.ToolKindExecute, nil)
		noteTool(t, s, "other", "read", sdk.ToolKindRead, map[string]any{"command": "cat notes"})
		parked := parkSparse(t, s, "late", nil, nil, oneAllow())
		defer parked.stop(t)
		p := parked.pending
		if p.Title != "bash `echo late`" || p.ToolKind != "execute" {
			t.Fatalf("pending = title %q kind %q", p.Title, p.ToolKind)
		}
	})

	t.Run("request title and kind win", func(t *testing.T) {
		s := newPermSession(t, "vibe")
		noteTool(t, s, "c", "bash", sdk.ToolKindExecute, "rm -rf /tmp/synthetic")
		reqKind := sdk.ToolKindRead
		parked := parkSparse(t, s, "c", sdk.Ptr("read"), &reqKind, oneAllow())
		defer parked.stop(t)
		p := parked.pending
		if p.Title != "read" || p.ToolKind != "read" {
			t.Fatalf("request lost to the cache: title %q kind %q", p.Title, p.ToolKind)
		}
		parked.stop(t)
		blank := "  "
		kindOnly := parkSparse(t, s, "c", &blank, nil, oneAllow())
		defer kindOnly.stop(t)
		if kindOnly.pending.Title != "bash `rm -rf /tmp/synthetic`" || kindOnly.pending.ToolKind != "execute" {
			t.Fatalf("blank request title = %q kind %q", kindOnly.pending.Title, kindOnly.pending.ToolKind)
		}
	})

	t.Run("kind survives when the title does not", func(t *testing.T) {
		s := newPermSession(t, "vibe")
		noteTool(t, s, "c", "", sdk.ToolKindExecute, nil)
		parked := parkSparse(t, s, "c", nil, nil, oneAllow())
		defer parked.stop(t)
		if parked.pending.Title != "Unknown tool" || parked.pending.ToolKind != "execute" {
			t.Fatalf("pending = title %q kind %q", parked.pending.Title, parked.pending.ToolKind)
		}
	})

	t.Run("titles that are not short verbs stay literal", func(t *testing.T) {
		s := newPermSession(t, "vibe")
		noteTool(t, s, "bare", "bash", sdk.ToolKindExecute, nil)
		noteTool(t, s, "digit", "1ls", "", "pwd")
		noteTool(t, s, "bang", "bash!", "", "pwd")
		noteTool(t, s, "tick", "go`build", "", "test")
		noteTool(t, s, "", "bash", sdk.ToolKindExecute, "ignored")
		bare := parkSparse(t, s, "bare", nil, nil, oneAllow())
		defer bare.stop(t)
		if bare.pending.Title != "bash" || bare.pending.ToolKind != "execute" {
			t.Fatalf("title without a command = %q kind %q", bare.pending.Title, bare.pending.ToolKind)
		}
		bare.stop(t)
		for _, tc := range []struct{ id, want string }{
			{"digit", "1ls"},
			{"bang", "bash!"},
			{"tick", "go`build"},
		} {
			p := parkSparse(t, s, tc.id, nil, nil, oneAllow())
			if p.pending.Title != tc.want {
				p.stop(t)
				t.Fatalf("%s title = %q, want %q", tc.id, p.pending.Title, tc.want)
			}
			p.stop(t)
		}
	})

	t.Run("short title keeps a backtick command visible", func(t *testing.T) {
		s := newPermSession(t, "vibe")
		noteTool(t, s, "c", "bash", "", "echo `date`")
		parked := parkSparse(t, s, "c", nil, nil, oneAllow())
		defer parked.stop(t)
		if parked.pending.Title != "bash echo `date`" {
			t.Fatalf("title = %q", parked.pending.Title)
		}
	})

	t.Run("missing metadata is an unknown tool", func(t *testing.T) {
		s := newPermSession(t, "vibe")
		s.assistant.WriteString("rm -rf /")
		noteTool(t, s, "known", "bash", sdk.ToolKindExecute, "ls")
		parked := parkSparse(t, s, "missing", nil, nil, oneAllow())
		defer parked.stop(t)
		p := parked.pending
		if p.Title != "Unknown tool" || p.ToolKind != "" {
			t.Fatalf("missing id → title %q kind %q", p.Title, p.ToolKind)
		}
		if strings.Contains(p.Title, "rm") {
			t.Fatalf("title inferred from the prompt: %q", p.Title)
		}
	})

	t.Run("empty id does not borrow another tool", func(t *testing.T) {
		s := newPermSession(t, "vibe")
		noteTool(t, s, "known", "bash", sdk.ToolKindExecute, "ls")
		parked := parkSparse(t, s, "", nil, nil, oneAllow())
		defer parked.stop(t)
		if parked.pending.Title != "Unknown tool" {
			t.Fatalf("empty id title = %q", parked.pending.Title)
		}
	})

	t.Run("command shapes", func(t *testing.T) {
		cases := []struct {
			name string
			raw  any
			want string
		}{
			{"string", "  pwd  ", "bash `pwd`"},
			{"cmd key", map[string]any{"cmd": "whoami"}, "bash `whoami`"},
			{"command wins over cmd", map[string]any{"command": "true", "cmd": "false"}, "bash `true`"},
			{"blank command falls through to cmd", map[string]any{"command": "  ", "cmd": "whoami"}, "bash `whoami`"},
			{"array args stay off the command", map[string]any{"command": "ls", "args": []any{"-l"}}, "bash `ls`"},
			{"blank args stay off the command", map[string]any{"command": "ls", "args": "  "}, "bash `ls`"},
			{"number is not a command", map[string]any{"command": 12}, "Unknown tool"},
			{"backtick is not wrapped", "echo `date`", "echo `date`"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newPermSession(t, "vibe")
				noteTool(t, s, "c", "", "", tc.raw)
				parked := parkSparse(t, s, "c", nil, nil, oneAllow())
				defer parked.stop(t)
				if parked.pending.Title != tc.want {
					t.Fatalf("title = %q, want %q", parked.pending.Title, tc.want)
				}
				if parked.pending.ToolKind != "" {
					t.Fatalf("kind inferred as %q", parked.pending.ToolKind)
				}
			})
		}
	})

	t.Run("short title joins the command and a long one does not", func(t *testing.T) {
		s := newPermSession(t, "vibe")
		noteTool(t, s, "short", "Run tests", "", map[string]any{"command": "go", "args": "test"})
		long := "Inspect the repository tree"
		noteTool(t, s, "long", long, "", "ls")
		already := newPermSession(t, "vibe")
		noteTool(t, already, "has", "bash ls", "", "ls")
		ps := parkSparse(t, s, "short", nil, nil, oneAllow())
		defer ps.stop(t)
		if ps.pending.Title != "Run tests `go test`" {
			t.Fatalf("short title = %q", ps.pending.Title)
		}
		ps.stop(t)
		pl := parkSparse(t, s, "long", nil, nil, oneAllow())
		defer pl.stop(t)
		if pl.pending.Title != long {
			t.Fatalf("long title = %q", pl.pending.Title)
		}
		pa := parkSparse(t, already, "has", nil, nil, oneAllow())
		defer pa.stop(t)
		if pa.pending.Title != "bash ls" {
			t.Fatalf("title that already contains the command = %q", pa.pending.Title)
		}
	})

	t.Run("verb length boundary", func(t *testing.T) {
		exact := "A" + strings.Repeat("b", 20)
		over := exact + "c"
		s := newPermSession(t, "vibe")
		noteTool(t, s, "fit", exact, "", "ls")
		noteTool(t, s, "over", over, "", "ls")
		pf := parkSparse(t, s, "fit", nil, nil, oneAllow())
		defer pf.stop(t)
		if pf.pending.Title != exact+" `ls`" {
			t.Fatalf("21-char verb = %q", pf.pending.Title)
		}
		pf.stop(t)
		po := parkSparse(t, s, "over", nil, nil, oneAllow())
		defer po.stop(t)
		if po.pending.Title != over {
			t.Fatalf("22-char title = %q", po.pending.Title)
		}
	})

	t.Run("empty update does not wipe or refresh", func(t *testing.T) {
		s := newPermSession(t, "vibe")
		noteTool(t, s, "keep", "bash", sdk.ToolKindExecute, "ls")
		noteToolUpdate(t, s, "keep", "", "", nil)
		for i := 0; i < 32; i++ {
			noteTool(t, s, idn(i), "bash", "", "echo")
		}
		parked := parkSparse(t, s, "keep", nil, nil, oneAllow())
		defer parked.stop(t)
		if parked.pending.Title != "Unknown tool" {
			t.Fatalf("status-only update refreshed an evicted id into %q", parked.pending.Title)
		}
	})

	t.Run("bound evicts the oldest and a refresh keeps it", func(t *testing.T) {
		s := newPermSession(t, "vibe")
		noteTool(t, s, "oldest", "bash", "", "echo oldest")
		noteTool(t, s, "second", "bash", "", "echo second")
		for i := 0; i < 30; i++ {
			noteTool(t, s, idn(i), "bash", "", "echo")
		}
		noteTool(t, s, "oldest", "bash", "", "echo oldest")
		noteTool(t, s, "newest", "bash", "", "echo newest")
		pOld := parkSparse(t, s, "second", nil, nil, oneAllow())
		defer pOld.stop(t)
		if pOld.pending.Title != "Unknown tool" {
			t.Fatalf("second id survived the cap: %q", pOld.pending.Title)
		}
		pOld.stop(t)
		pKeep := parkSparse(t, s, "oldest", nil, nil, oneAllow())
		defer pKeep.stop(t)
		if pKeep.pending.Title != "bash `echo oldest`" {
			t.Fatalf("refreshed id = %q", pKeep.pending.Title)
		}
		pKeep.stop(t)
		pNew := parkSparse(t, s, "newest", nil, nil, oneAllow())
		defer pNew.stop(t)
		if pNew.pending.Title != "bash `echo newest`" {
			t.Fatalf("newest = %q", pNew.pending.Title)
		}
	})

	t.Run("id reuse across a new turn", func(t *testing.T) {
		s := newPermSession(t, "vibe")
		noteTool(t, s, "c1", "bash", sdk.ToolKindExecute, "rm -rf /tmp/synthetic")
		if _, ok := s.reserveTurn(); !ok {
			t.Fatal("reserveTurn")
		}
		parked := parkSparse(t, s, "c1", nil, nil, oneAllow())
		defer parked.stop(t)
		if parked.pending.Title != "Unknown tool" {
			t.Fatalf("new turn inherited %q", parked.pending.Title)
		}
	})

	t.Run("end turn and abort drop the cache", func(t *testing.T) {
		s := newPermSession(t, "vibe")
		noteTool(t, s, "c1", "bash", "", "ls")
		s.endTurn(sdk.PromptResponse{}, nil)
		parked := parkSparse(t, s, "c1", nil, nil, oneAllow())
		defer parked.stop(t)
		if parked.pending.Title != "Unknown tool" {
			t.Fatalf("endTurn kept %q", parked.pending.Title)
		}
		s2 := newPermSession(t, "vibe")
		noteTool(t, s2, "c1", "bash", "", "ls")
		s2.abortTurn()
		p2 := parkSparse(t, s2, "c1", nil, nil, oneAllow())
		defer p2.stop(t)
		if p2.pending.Title != "Unknown tool" {
			t.Fatalf("abortTurn kept %q", p2.pending.Title)
		}
	})

	t.Run("cancel clears the request and a stale token cannot answer the next one", func(t *testing.T) {
		s := newPermSession(t, "vibe")
		noteTool(t, s, "c1", "bash", sdk.ToolKindExecute, "ls")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan sdk.RequestPermissionResponse, 1)
		go func() {
			resp, _ := s.RequestPermission(ctx, sdk.RequestPermissionRequest{
				ToolCall: sdk.ToolCallUpdate{ToolCallId: "c1"},
				Options:  oneAllow(),
			})
			done <- resp
		}()
		waitFor(t, "pending", func() bool { _, ok := s.pendingInfo(); return ok })
		first, _ := s.pendingInfo()
		tok, _, err := s.prepareResolve(first.RequestID, "1")
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		select {
		case resp := <-done:
			if resp.Outcome.Cancelled == nil {
				t.Fatalf("cancel outcome = %+v", resp.Outcome)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("cancel did not unblock")
		}
		if _, ok := s.pendingInfo(); ok {
			t.Fatal("cancelled request still pending")
		}
		if err := s.deliver(tok); err == nil {
			t.Fatal("cancelled token was delivered")
		}
		noteTool(t, s, "c2", "bash", sdk.ToolKindExecute, "pwd")
		parked := parkSparse(t, s, "c2", nil, nil, oneAllow())
		defer parked.stop(t)
		p := parked.pending
		if p.Title != "bash `pwd`" || p.RequestID == first.RequestID {
			t.Fatalf("next request = %+v, previous %s", p, first.RequestID)
		}
		if err := s.deliver(tok); err == nil {
			t.Fatal("previous turn token answered the new request")
		}
	})
}

func newPermSession(t *testing.T, agent string) *Session {
	t.Helper()
	return &Session{
		nodeID: agent + "-perm",
		agent:  agent,
		incarn: "inc",
		done:   make(chan struct{}),
		logw:   &logWriter{Path: t.TempDir() + "/n.jsonl"},
	}
}

func noteTool(t *testing.T, s *Session, id, title string, kind sdk.ToolKind, raw any) {
	t.Helper()
	err := s.SessionUpdate(context.Background(), sdk.SessionNotification{Update: sdk.SessionUpdate{ToolCall: &sdk.SessionUpdateToolCall{
		ToolCallId: sdk.ToolCallId(id),
		Title:      title,
		Kind:       kind,
		RawInput:   raw,
	}}})
	if err != nil {
		t.Fatal(err)
	}
}

func noteToolUpdate(t *testing.T, s *Session, id, title, kind string, raw any) {
	t.Helper()
	tu := &sdk.SessionToolCallUpdate{ToolCallId: sdk.ToolCallId(id), RawInput: raw}
	if title != "" {
		tu.Title = &title
	}
	if kind != "" {
		k := sdk.ToolKind(kind)
		tu.Kind = &k
	}
	if err := s.SessionUpdate(context.Background(), sdk.SessionNotification{Update: sdk.SessionUpdate{ToolCallUpdate: tu}}); err != nil {
		t.Fatal(err)
	}
}

func oneAllow() []sdk.PermissionOption {
	return []sdk.PermissionOption{{OptionId: "allow-once", Name: "Allow once", Kind: sdk.PermissionOptionKindAllowOnce}}
}

func vibePermOptions() []sdk.PermissionOption {
	return []sdk.PermissionOption{
		{OptionId: "allow-once", Name: "Allow once", Kind: sdk.PermissionOptionKindAllowOnce},
		{OptionId: "allow-always-session", Name: "Always for this session", Kind: sdk.PermissionOptionKindAllowAlways},
		{OptionId: "allow-always-project", Name: "Always for this project", Kind: sdk.PermissionOptionKindAllowAlways},
		{OptionId: "reject-once", Name: "Reject", Kind: sdk.PermissionOptionKindRejectOnce},
	}
}

func idn(i int) string {
	return fmt.Sprintf("id-%02d", i)
}

// parkedPerm is one blocked RequestPermission. stop cancels it, or returns
// the selected outcome after deliver. Both are safe to call twice.
type parkedPerm struct {
	pending PendingPermission
	resp    chan sdk.RequestPermissionResponse
	cancel  context.CancelFunc
	once    sync.Once
}

func (p *parkedPerm) stop(t *testing.T) {
	t.Helper()
	p.once.Do(func() {
		p.cancel()
		select {
		case <-p.resp:
		case <-time.After(3 * time.Second):
			t.Errorf("permission goroutine did not return")
		}
	})
}

func (p *parkedPerm) take(t *testing.T) sdk.RequestPermissionResponse {
	t.Helper()
	var resp sdk.RequestPermissionResponse
	p.once.Do(func() {
		select {
		case resp = <-p.resp:
		case <-time.After(3 * time.Second):
			t.Errorf("permission goroutine did not return")
		}
	})
	return resp
}

func parkSparse(t *testing.T, s *Session, id string, title *string, kind *sdk.ToolKind, opts []sdk.PermissionOption) *parkedPerm {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &parkedPerm{resp: make(chan sdk.RequestPermissionResponse, 1), cancel: cancel}
	go func() {
		resp, _ := s.RequestPermission(ctx, sdk.RequestPermissionRequest{
			ToolCall: sdk.ToolCallUpdate{ToolCallId: sdk.ToolCallId(id), Title: title, Kind: kind},
			Options:  opts,
		})
		p.resp <- resp
	}()
	waitFor(t, "pending "+id, func() bool { _, ok := s.pendingInfo(); return ok })
	p.pending, _ = s.pendingInfo()
	return p
}
