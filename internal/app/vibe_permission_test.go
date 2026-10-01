package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
)

// A permission whose title is missing or the explicit unknown-tool sentinel
// stays a human choice. A real title still auto-approves, including when the
// kind is empty, and the chat payload and decision audit show that title.

func TestUnknownToolTitleIsNotAutoApproved(t *testing.T) {
	for _, title := range []string{"Unknown tool", "", "   "} {
		t.Run(title, func(t *testing.T) {
			a, n, proc := armVibeApproval(t, "vibe-unknown")
			pending := allowPending("incarn:2")
			pending.Title = title
			proc.pending = pending
			a.maybeAutoApprove(n, proc)
			if proc.prepareCalls != 0 || proc.deliverCalls != 0 {
				t.Fatalf("title %q was auto-approved prepare=%d deliver=%d", title, proc.prepareCalls, proc.deliverCalls)
			}
			if decision := lastDecision(t, a, n.ID); decision != nil {
				t.Fatalf("title %q wrote a decision: %+v", title, decision)
			}
		})
	}
}

func TestRealTitleWithEmptyKindStillAutoApproves(t *testing.T) {
	a, n, proc := armVibeApproval(t, "vibe-titled")
	pending := allowPending("incarn:2")
	pending.Title = "bash `git status`"
	pending.ToolKind = ""
	proc.pending = pending
	a.maybeAutoApprove(n, proc)
	if proc.prepareCalls != 1 || proc.deliverCalls != 1 {
		t.Fatalf("real title was not delivered: prepare=%d deliver=%d", proc.prepareCalls, proc.deliverCalls)
	}
	decision := lastDecision(t, a, n.ID)
	if decision == nil || decision.Title != "bash `git status`" || decision.ToolKind != "" || decision.Source != "auto" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestVibeAllowAlwaysPairIsNotAnAutomaticApproval(t *testing.T) {
	a, n, proc := armVibeApproval(t, "vibe-always")
	proc.pending = PendingPermission{
		RequestID: "incarn:2",
		Title:     "bash `git status`",
		ToolKind:  "execute",
		Options: []PermOption{
			{Key: "1", Name: "Always for this session", Kind: "allow_always"},
			{Key: "2", Name: "Always for this project", Kind: "allow_always"},
			{Key: "3", Name: "Reject", Kind: "reject"},
		},
	}
	a.maybeAutoApprove(n, proc)
	if proc.prepareCalls != 0 || proc.deliverCalls != 0 {
		t.Fatalf("two allow_always options were automated: prepare=%d deliver=%d", proc.prepareCalls, proc.deliverCalls)
	}
}

func TestChatAndManualAuditShowThePermissionTitle(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "vibe-chat", "vibe", "acp")
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: PendingPermission{
			RequestID: "inc:1",
			Title:     "bash `git status`",
			ToolKind:  "execute",
			Options: []PermOption{
				{Key: "1", Name: "Allow once", Kind: "allow"},
				{Key: "2", Name: "Always for this session", Kind: "allow_always"},
				{Key: "3", Name: "Always for this project", Kind: "allow_always"},
			},
		},
	}
	resp := map[string]any{}
	a.procChatInto(resp, n, stub, sessionlog.Segment{})
	if resp["perm_title"] != "bash `git status`" || resp["perm_tool_kind"] != "execute" || resp["waiting_on"] != "bash `git status`" {
		t.Fatalf("chat projection = %#v", resp)
	}
	opts, _ := resp["perm_options"].([]PermOption)
	if len(opts) != 3 || opts[1].Kind != "allow_always" || opts[2].Kind != "allow_always" || opts[1].Name == opts[2].Name {
		t.Fatalf("options collapsed: %+v", opts)
	}

	a.testProc = stub
	req := httptest.NewRequest(http.MethodPost, "/api/nodes/"+n.ID+"/key", strings.NewReader(`{"key":"2","request_id":"inc:1"}`))
	req.SetPathValue("id", n.ID)
	rec := httptest.NewRecorder()
	a.handleKey(rec, req)
	if rec.Code != 200 {
		t.Fatalf("key = %d %s", rec.Code, rec.Body)
	}
	recs := keyRecords(t, a.storePath)
	var found *storeRecord
	for i := range recs {
		if recs[i].Type == "key" && recs[i].ID == n.ID {
			found = &recs[i]
		}
	}
	if found == nil || found.Excerpt != "permission: bash `git status`" || found.Key != "2" {
		t.Fatalf("manual audit = %+v", found)
	}
}

func armVibeApproval(t *testing.T, id string) (*app, *Node, *stubProc) {
	t.Helper()
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, id, "vibe", "acp")
	proc := &stubProc{live: "active", hasSession: true, hasPending: true, clearOnDeliver: true}
	a.mu.Lock()
	a.autoApprove[n.ID] = &autoApproveState{
		Phase:        autoPhaseArmed,
		LeaseID:      "lease",
		EnableIncarn: "incarn",
		EnableMaxSeq: 0,
		Attempted:    map[string]bool{},
	}
	a.mu.Unlock()
	return a, n, proc
}

func lastDecision(t *testing.T, a *app, id string) *sessionlog.DecisionEvent {
	t.Helper()
	var decision *sessionlog.DecisionEvent
	for _, ev := range sessionlog.ReadEvents(a.sessionLogPath(id)) {
		if ev.T == "decision" {
			decision = ev.Decision
		}
	}
	return decision
}
