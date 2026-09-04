package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A file-change approval must name the file. "File changes" is what the
// supervisor sees in the transcript when auto-approve answers for them, and
// a decision record that does not say which file was changed is not a
// record. The two methods carry the paths differently:
//
//   - applyPatchApproval (v1) carries them in params.fileChanges, keyed by
//     path.
//   - item/fileChange/requestApproval (v2) carries only itemId; the paths
//     are on the fileChange item, which arrives on item/started before the
//     approval is asked.
//
// Field names are from `codex app-server generate-json-schema` (codex-cli
// 0.147.0): ApplyPatchApprovalParams{callId, conversationId, fileChanges,
// reason?, grantRoot?}, FileChangeRequestApprovalParams{itemId, threadId,
// turnId, startedAtMs, reason?, grantRoot?}, FileChangeThreadItem{id, type,
// status, changes[{path, kind, diff}]}.

func TestApplyPatchApprovalNamesTheFile(t *testing.T) {
	raw := `{"callId":"c1","conversationId":"t1","fileChanges":{` +
		`"/w/internal/app/router.go":{"type":"update","unified_diff":"@@"}}}`
	a := decodeApproval("applyPatchApproval", []byte(raw))
	if got, want := approvalTitle(a), "Edit `/w/internal/app/router.go`"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
}

func TestApplyPatchApprovalVerbFollowsTheChangeKind(t *testing.T) {
	cases := []struct {
		name  string
		files string
		want  string
	}{
		{"add", `{"/w/new.go":{"type":"add","content":"x"}}`, "Create `/w/new.go`"},
		{"delete", `{"/w/old.go":{"type":"delete","content":"x"}}`, "Delete `/w/old.go`"},
		{"mixed", `{"/w/a.go":{"type":"add","content":"x"},"/w/b.go":{"type":"delete","content":"y"}}`,
			"Edit `/w/a.go, /w/b.go`"},
	}
	for _, tc := range cases {
		a := decodeApproval("applyPatchApproval", []byte(`{"callId":"c","conversationId":"t","fileChanges":`+tc.files+`}`))
		if got := approvalTitle(a); got != tc.want {
			t.Errorf("%s: title = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestFileChangeApprovalTitleCapsTheFileList(t *testing.T) {
	a := Approval{Method: "item/fileChange/requestApproval", FileChanges: []FileChangePath{
		{Path: "a.go", Kind: "update"}, {Path: "b.go", Kind: "update"},
		{Path: "c.go", Kind: "update"}, {Path: "d.go", Kind: "update"},
		{Path: "e.go", Kind: "update"},
	}}
	if got, want := approvalTitle(a), "Edit `a.go, b.go, c.go and 2 more`"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
}

func TestFileChangeApprovalGrantRootStillGovernsTheAsk(t *testing.T) {
	// A grantRoot asks for writes under a whole root for the rest of the
	// session — a larger grant than the files in flight. Naming only the
	// files would understate it.
	raw := `{"itemId":"i1","threadId":"t","turnId":"u","startedAtMs":1,"grantRoot":"/w"}`
	a := decodeApproval("item/fileChange/requestApproval", []byte(raw))
	a.FileChanges = []FileChangePath{{Path: "/w/a.go", Kind: "update"}}
	if got, want := approvalTitle(a), "File changes under /w"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
}

// The v2 approval names no file; the client must have kept the item's
// changes from item/started so the ask can still say which file.
func TestFileChangeApprovalNamesTheItemsFile(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	seen := make(chan Approval, 1)
	c.SetApprovalHandler(func(a Approval) (string, json.RawMessage, bool) {
		select {
		case seen <- a:
		default:
		}
		return "accept", nil, true
	})
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, _ = c.Initialize(ctx, "t", "1")
		ti, _ := c.StartThread(ctx, StartThreadParams{})
		done <- c.RunTurn(ctx, ti.ID, "x")
	}()
	ms.reply(t, ms.nextReq(t).ID, `{}`)
	ms.reply(t, ms.nextReq(t).ID, threadStartResult)
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`)

	ms.note(t, "item/started", `{"threadId":"t","turnId":"u","startedAtMs":1,"item":{`+
		`"id":"item_7","type":"fileChange","status":"in_progress","changes":[`+
		`{"path":"/w/web/js/pairing.js","kind":{"type":"update"},"diff":"@@"}]}}`)
	ms.serverRequest(t, 0, "item/fileChange/requestApproval",
		`{"itemId":"item_7","threadId":"t","turnId":"u","startedAtMs":2}`)

	a := <-seen
	if len(a.FileChanges) != 1 || a.FileChanges[0].Path != "/w/web/js/pairing.js" {
		t.Fatalf("approval carried %+v, want the item's one path", a.FileChanges)
	}
	if got, want := approvalTitle(a), "Edit `/w/web/js/pairing.js`"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
	ms.nextResp(t)
	ms.note(t, "turn/completed", `{"turn":{}}`)
	<-done
}

// An approval for an item nobody announced must degrade to the generic
// label, never to a guessed path.
func TestFileChangeApprovalWithoutItemStaysGeneric(t *testing.T) {
	a := decodeApproval("item/fileChange/requestApproval",
		[]byte(`{"itemId":"unknown","threadId":"t","turnId":"u","startedAtMs":1}`))
	if got := approvalTitle(a); got != "File changes" {
		t.Fatalf("title = %q, want the generic label", got)
	}
	if strings.Contains(approvalTitle(a), "/") {
		t.Fatal("a path appeared for an item the server never described")
	}
}
