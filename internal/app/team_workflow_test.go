package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/team"
)

// TestTeamServerWorkflow runs the seat against an independently built relay.
// All conversation records are synthetic. No agent process is launched.
func TestTeamServerWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-repository integration test")
	}
	binary := os.Getenv("SCIMUX_TEAM_E2E_BINARY")
	if binary == "" {
		t.Skip("set SCIMUX_TEAM_E2E_BINARY to a built scimux-team binary")
	}
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var running *exec.Cmd
	t.Cleanup(func() {
		if running != nil {
			_ = running.Process.Kill()
			_ = running.Wait()
		}
	})
	start := func(addr string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, "-state-dir", state, "-addr", addr, "-name", "Synthetic relay")
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		running = cmd
		ready := make(chan string, 1)
		go func() {
			scan := bufio.NewScanner(out)
			url := ""
			if scan.Scan() {
				url, _ = strings.CutPrefix(scan.Text(), "scimux-team listening on ")
			}
			ready <- url
			_, _ = io.Copy(io.Discard, out)
		}()
		select {
		case url := <-ready:
			if !strings.HasPrefix(url, "http://127.0.0.1:") {
				t.Fatal("relay did not publish its loopback address")
			}
			return url
		case <-ctx.Done():
			t.Fatal("relay startup timed out")
			return ""
		}
	}
	stop := func() {
		t.Helper()
		if err := running.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		if err := running.Wait(); err != nil {
			t.Fatal("relay failed to stop cleanly")
		}
		running = nil
	}
	baseURL := start("127.0.0.1:0")
	operator := func(args ...string) []byte {
		t.Helper()
		out, err := exec.CommandContext(ctx, binary, append([]string{"-state-dir", state}, args...)...).Output()
		if err != nil {
			t.Fatal("relay operator command failed")
		}
		return out
	}
	issue := func() string {
		t.Helper()
		var result struct{ Link string }
		if json.Unmarshal(operator("-url", baseURL, "-json", "issue"), &result) != nil || result.Link == "" {
			t.Fatal("operator returned no join link")
		}
		return result.Link
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	type seat struct {
		handler *teamHandler
		dir     string
		id      string
	}
	newSeat := func() seat {
		h, dir := testTeamSurface(t, team.NewClient())
		th := asTeamHandler(t, h)
		th.now = func() time.Time { return now }
		return seat{th, dir, installationOf(t, dir)}
	}
	owner, writer, reader := newSeat(), newSeat(), newSeat()
	request := func(s seat, method, path string, body any, status int) []byte {
		t.Helper()
		raw := ""
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			raw = string(b)
		}
		got := doTeam(s.handler, method, path, raw)
		if got.Code != status {
			t.Fatalf("%s %s: status %d, want %d", method, path, got.Code, status)
		}
		return got.Body.Bytes()
	}
	decode := func(raw []byte, dest any) {
		t.Helper()
		if err := json.Unmarshal(raw, dest); err != nil {
			t.Fatal("invalid seat response")
		}
	}
	var server teamServerView
	for i, s := range []seat{owner, writer, reader} {
		raw := request(s, "POST", "/api/team/servers", map[string]string{"link": issue(), "name": []string{"Owner", "Writer", "Reader"}[i]}, 201)
		decode(raw, &server)
	}
	appendNode(t, owner.dir, shareNode, "Synthetic study")
	appendOutbox(t, owner.dir, sessionlog.Event{T: "meta", Time: now.Format(time.RFC3339), Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "codex", Created: now.Format(time.RFC3339)}})
	appendOutbox(t, owner.dir, sessionlog.Event{T: "user", Time: now.Format(time.RFC3339), Text: "Private earlier prompt"})
	appendOutbox(t, owner.dir, sessionlog.Event{T: "assistant", Time: now.Format(time.RFC3339), Text: "Private earlier answer"})
	box := &shareOutbox{dataDir: owner.dir, now: func() time.Time { return now }, client: team.NewClient(), files: &owner.handler.mu}
	members := []team.Member{{Seat: writer.id, Access: "write"}, {Seat: reader.id, Access: "read"}}
	share := func(scope string, status int) teamShareView {
		t.Helper()
		var view teamShareView
		decode(request(owner, "PUT", "/api/team/shares/"+shareNode, map[string]any{"server": server.ID, "scope": scope, "members": members}, status), &view)
		return view
	}
	list := func(s seat) subListResponse {
		t.Helper()
		now = now.Add(11 * time.Second)
		var out subListResponse
		decode(request(s, "GET", "/api/team/subscriptions", nil, 200), &out)
		return out
	}
	address := func(sub subscriptionView) string {
		return "/api/team/subscriptions/" + strings.Join([]string{sub.Server, sub.Owner, sub.Chat, sub.Generation}, "/")
	}
	share("from_now", 201)
	box.tick(ctx)
	writing, reading := list(writer), list(reader)
	if len(writing.Subscriptions) != 1 || len(reading.Subscriptions) != 1 {
		t.Fatal("members did not discover the share")
	}
	path := address(writing.Subscriptions[0])
	initial := request(writer, "GET", path, nil, 200)
	if strings.Contains(string(initial), "Private earlier") || !strings.Contains(string(initial), "from_now") {
		t.Fatal("from_now exposed earlier conversation or omitted its boundary")
	}
	appendOutbox(t, owner.dir, sessionlog.Event{T: "assistant", Time: now.Format(time.RFC3339), Text: "Synthetic future answer"})
	box.tick(ctx)
	for _, s := range []seat{writer, reader} {
		if !strings.Contains(string(request(s, "GET", path, nil, 200)), "Synthetic future answer") {
			t.Fatal("member did not receive the future answer")
		}
	}
	beforeComment, err := os.ReadFile(chatLogPath(owner.dir, shareNode))
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"send", "key", "auto-approve", "send/interrupt"} {
		request(writer, "POST", path+"/"+action, map[string]any{"text": "forged"}, 404)
	}
	request(writer, "POST", path+"/comments", map[string]any{"text": "forged", "anchor": 0, "key": "1122334455667788", "prompt": true}, 400)
	comment := map[string]any{"text": "Synthetic comment", "anchor": 0, "key": "1122334455667788"}
	first := request(writer, "POST", path+"/comments", comment, 200)
	if string(request(writer, "POST", path+"/comments", comment, 200)) != string(first) {
		t.Fatal("retry was not idempotent")
	}
	request(reader, "POST", path+"/comments", comment, 403)
	own := list(owner)
	if len(own.Subscriptions) != 1 || own.Subscriptions[0].LocalNode != shareNode || own.Subscriptions[0].LocalFrontier == nil || own.Subscriptions[0].LocalFrontier.Record != 2 {
		t.Fatalf("owner frontier missing: %+v", own.Subscriptions)
	}
	ownPath := address(own.Subscriptions[0])
	var ownerMirror subscriptionReadResponse
	decode(request(owner, "GET", ownPath, nil, 200), &ownerMirror)
	if len(ownerMirror.Entries) == 0 || ownerMirror.Entries[0]["local_record"] != float64(2) {
		t.Fatalf("owner start address missing: %+v", ownerMirror.Entries)
	}
	afterComment, err := os.ReadFile(chatLogPath(owner.dir, shareNode))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeComment, afterComment) {
		t.Fatal("comments changed the owner worker log")
	}
	var comments commentReadResponse
	decode(request(reader, "GET", path+"/comments", nil, 200), &comments)
	if len(comments.Entries) != 1 || comments.Names[writer.id] != "Writer" {
		t.Fatal("comment author or duplicate count is wrong")
	}
	request(writer, "PUT", "/api/team/servers/"+server.ID+"/seat", map[string]string{"name": "Renamed writer"}, 200)
	now = now.Add(61 * time.Second)
	decode(request(reader, "GET", path+"/comments?after=1", nil, 200), &comments)
	if len(comments.Entries) != 0 || comments.Names[writer.id] != "Renamed writer" {
		t.Fatal("incremental comment read did not refresh the author's name")
	}
	members[0].Access, members[1].Access = "read", "write"
	share("from_now", 200)
	if list(writer).Subscriptions[0].Access != "read" || list(reader).Subscriptions[0].Access != "write" {
		t.Fatal("members did not learn the changed permissions")
	}
	request(writer, "POST", path+"/comments", comment, 403)
	stop()
	if !strings.Contains(string(request(reader, "GET", path, nil, 200)), "Synthetic future answer") {
		t.Fatal("offline seat lost its saved copy")
	}
	request(reader, "POST", path+"/comments", comment, 502)
	appendOutbox(t, owner.dir, sessionlog.Event{T: "assistant", Time: now.Format(time.RFC3339), Text: "Synthetic queued answer"})
	box.tick(ctx)
	start(strings.TrimPrefix(baseURL, "http://"))
	now = now.Add(time.Minute)
	box.tick(ctx)
	if !strings.Contains(string(request(reader, "GET", path, nil, 200)), "Synthetic queued answer") {
		t.Fatal("relay restart did not resume the outbox")
	}
	appendNode(t, owner.dir, shareNode, "Renamed synthetic study")
	appendOutbox(t, owner.dir, sessionlog.Event{T: "source", Time: now.Format(time.RFC3339), Source: &sessionlog.SourceEvent{Reason: "clear"}})
	box.tick(ctx)
	if list(reader).Subscriptions[0].Title != "Renamed synthetic study" {
		t.Fatal("chat title change did not reach the member")
	}
	if !strings.Contains(string(request(reader, "GET", path, nil, 200)), `"reason":"clear"`) {
		t.Fatal("page-turn seam did not reach the member")
	}
	request(reader, "DELETE", path, nil, 204)
	if len(list(reader).Subscriptions) != 0 {
		t.Fatal("removed member reappeared")
	}
	request(owner, "DELETE", "/api/team/shares/"+shareNode, nil, 204)
	retained := list(owner)
	if len(retained.Subscriptions) != 1 || retained.Subscriptions[0].State != "ended" || retained.Subscriptions[0].LocalNode != shareNode || retained.Subscriptions[0].LocalFrontier == nil {
		t.Fatalf("owner retained frontier missing: %+v", retained.Subscriptions)
	}
	if list(writer).Subscriptions[0].State != "ended" || !strings.Contains(string(request(writer, "GET", path, nil, 200)), "Synthetic future answer") {
		t.Fatal("stopping sharing discarded received history")
	}
	share("all", 201)
	box.tick(ctx)
	ownerCopies := list(owner)
	if len(ownerCopies.Subscriptions) != 2 {
		t.Fatalf("owner generations = %+v", ownerCopies.Subscriptions)
	}
	var oldOwner, newOwner *subscriptionView
	for i := range ownerCopies.Subscriptions {
		copy := &ownerCopies.Subscriptions[i]
		if copy.State == "ended" {
			oldOwner = copy
		}
		if copy.State == "live" {
			newOwner = copy
		}
	}
	if oldOwner == nil || newOwner == nil || oldOwner.Generation == newOwner.Generation || oldOwner.LocalFrontier == nil || newOwner.LocalFrontier == nil || oldOwner.LocalFrontier.Record != 2 || newOwner.LocalFrontier.Record != -1 {
		t.Fatalf("owner generation frontiers = %+v", ownerCopies.Subscriptions)
	}
	var retainedMirror subscriptionReadResponse
	decode(request(owner, "GET", address(*oldOwner), nil, 200), &retainedMirror)
	if len(retainedMirror.Entries) == 0 || retainedMirror.Entries[0]["local_record"] != float64(2) {
		t.Fatalf("retained owner frontier missing: %+v", retainedMirror.Entries)
	}
	var next subscriptionView
	for _, sub := range list(writer).Subscriptions {
		if sub.State == "live" {
			next = sub
		}
	}
	if next.Generation == "" || next.Generation == writing.Subscriptions[0].Generation {
		t.Fatal("sharing again did not create a fresh generation")
	}
	if !strings.Contains(string(request(writer, "GET", address(next), nil, 200)), "Private earlier answer") {
		t.Fatal("entire-history share omitted the earlier conversation")
	}
	// The relay escapes HTML characters while retaining the exact JSON value.
	// Reconciliation must accept that wire representation without rounding the
	// large anchor or confusing the synthetic attachment name.
	ownerServers, err := team.Load(owner.dir)
	if err != nil || len(ownerServers) != 1 {
		t.Fatal("cannot read synthetic owner credentials")
	}
	body := json.RawMessage(`{"v":1,"text":"Synthetic <caption> & file","anchor":9007199254740993,"files":[{"name":"<synthetic> & file.txt"}]}`)
	seq, err := team.NewClient().AppendPeopleComment(ctx, baseURL, ownerServers[0].Token, next.Owner, next.Chat, next.Generation, "c/0123456789abcdef", owner.id, body)
	if err != nil || seq == 0 {
		t.Fatalf("relay-escaped synthetic publication not reconciled: %v", err)
	}
	boundaryText := "A" + strings.Repeat("\n", 16383)
	boundaryBody, err := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": boundaryText + "\n" + peopleAttachmentNotice, "content_text": boundaryText, "anchor": uint64(9007199254740993), "people_attachments_version": 1, "people_attachments": []map[string]any{{"id": "0123456789abcdef", "sha256": strings.Repeat("a", 64), "size": 1, "name": "<synthetic> & file.txt", "mime": "text/plain"}}})
	if err != nil {
		t.Fatal(err)
	}
	boundarySeq, err := team.NewClient().AppendPeopleComment(ctx, baseURL, ownerServers[0].Token, next.Owner, next.Chat, next.Generation, "c/fedcba9876543210", owner.id, boundaryBody)
	if err != nil || boundarySeq == 0 {
		t.Fatalf("boundary synthetic publication failed: %v", err)
	}
	retrySeq, err := team.NewClient().AppendPeopleComment(ctx, baseURL, ownerServers[0].Token, next.Owner, next.Chat, next.Generation, "c/fedcba9876543210", owner.id, boundaryBody)
	if err != nil || retrySeq != boundarySeq {
		t.Fatalf("boundary exact retry failed: %v", err)
	}
	request(writer, "POST", "/api/team/servers/"+server.ID+"/leave", nil, 204)
	request(writer, "POST", "/api/team/servers", map[string]string{"link": issue(), "name": "Rejoined"}, 201)
	for _, sub := range list(writer).Subscriptions {
		if sub.State == "live" {
			t.Fatal("rejoined seat inherited a membership")
		}
	}
	appendDelete(t, owner.dir, shareNode)
	box.tick(ctx)
	request(owner, "GET", "/api/team/shares/"+shareNode, nil, 404)
	operator("revoke", owner.id)
	var status struct{ Status string }
	decode(request(owner, "GET", "/api/team/servers/"+server.ID+"/status", nil, http.StatusOK), &status)
	if status.Status != "refused" {
		t.Fatal("owner revoke did not invalidate its token")
	}
	// A forgotten server remains registered remotely and can still authenticate.
	entries, err := team.Load(reader.dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("cannot read synthetic reader credentials")
	}
	request(reader, "DELETE", "/api/team/servers/"+server.ID, nil, 204)
	if err := team.NewClient().Check(ctx, baseURL, entries[0].Token); err != nil {
		t.Fatal("local removal unexpectedly revoked the remote seat")
	}
}
