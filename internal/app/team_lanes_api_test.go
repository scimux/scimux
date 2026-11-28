package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

const (
	laneLocal = "north"
	laneColor = "#1a2b3c"
)

func lanePutBody(scope, name, color string, members []team.Member, excluded []string) string {
	raw, err := json.Marshal(struct {
		Server   string        `json:"server"`
		Scope    string        `json:"scope"`
		Members  []team.Member `json:"members"`
		Name     string        `json:"name"`
		Color    string        `json:"color"`
		Excluded []string      `json:"excluded"`
	}{teamServerID, scope, members, name, color, excluded})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func appendLaneNode(t *testing.T, dir, id, title, lane, created string) {
	t.Helper()
	raw, err := json.Marshal(storeRecord{Type: "node", Node: &Node{
		ID: id, Title: title, Agent: "claude", Dir: "/tmp/synthetic", LaneID: lane, CreatedAt: created,
	}})
	if err != nil {
		t.Fatal(err)
	}
	appendStoreLine(t, dir, raw)
}

type laneView struct {
	Lane     string        `json:"lane"`
	ID       string        `json:"id"`
	Server   string        `json:"server"`
	Scope    string        `json:"scope"`
	Name     string        `json:"name"`
	Color    string        `json:"color"`
	Members  []team.Member `json:"members"`
	State    string        `json:"state"`
	Delivery string        `json:"delivery"`
	Chats    int           `json:"chats"`
	Excluded int           `json:"excluded"`
	SharedAt string        `json:"shared_at"`
}

func decodeLaneView(t *testing.T, rec *httptest.ResponseRecorder) laneView {
	t.Helper()
	var got laneView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("view %s: %v", rec.Body.String(), err)
	}
	return got
}

func putLane(h http.Handler, lane, body string) *httptest.ResponseRecorder {
	return doTeam(h, http.MethodPut, "/api/team/lane-shares/"+lane, body)
}

func TestLaneShareCreateAnswersCreated(t *testing.T) {
	script := &shareScript{}
	h, dir, calls, inst := readySurface(t, script)
	script.channel = channelJSON(inst, shareUID, "Shared lane · North", shareGen)
	appendLaneNode(t, dir, shareNode, "North", laneLocal, "2026-11-12T09:30:00Z")
	writeChatLog(t, dir, shareNode, shareUID, 1)
	appendLaneNode(t, dir, "old", "Old", laneLocal, "2026-11-11T09:30:00Z")
	body := lanePutBody("from_now", "North", laneColor, []team.Member{{Seat: shareSeat, Access: "write"}}, []string{"old"})
	rec := putLane(h, laneLocal, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	got := decodeLaneView(t, rec)
	if got.Lane != laneLocal || got.Server != teamServerID || got.Scope != "from_now" || got.Name != "North" || got.Color != laneColor || got.State != "active" || got.Delivery != "ok" || got.Chats != 0 || got.Excluded != 1 || got.SharedAt != "2026-11-12T09:30:00Z" || len(got.ID) != 16 || got.ID == laneLocal {
		t.Fatalf("view = %+v", got)
	}
	assertMembersEqual(t, got.Members, []team.Member{{Seat: shareSeat, Access: "write"}})
	lanes, err := team.LoadLaneShares(dir)
	if err != nil || len(lanes) != 1 || lanes[0].Rev != 1 || lanes[0].IndexRev != 1 || lanes[0].Starts[shareNode] != 2 || len(lanes[0].Starts) != 1 || lanes[0].ID != got.ID || lanes[0].Generation != shareGen {
		t.Fatalf("stored = %+v err %v", lanes, err)
	}
	if _, ok := storedShare(t, dir, shareUID); ok {
		t.Fatal("create stored a chat share")
	}
	var titled bool
	for _, c := range snapshotCalls(calls) {
		if c.method == http.MethodPut && strings.Contains(c.body, "Shared lane · North") {
			titled = true
		}
	}
	if !titled {
		t.Fatalf("calls = %+v", snapshotCalls(calls))
	}
}

func TestLaneShareListAndGet(t *testing.T) {
	h, dir, _, _ := readySurface(t, &shareScript{})
	rec := doTeam(h, http.MethodGet, "/api/team/lane-shares", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"lane_shares":[]}` {
		t.Fatalf("empty = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, team.LaneSharesFile)); !os.IsNotExist(err) {
		t.Fatal("list created the lane file")
	}
	missing := doTeam(h, http.MethodGet, "/api/team/lane-shares/"+laneLocal, "")
	assertAPIError(t, missing, http.StatusNotFound, "not_found")
	if err := team.SaveLaneShare(dir, team.LaneShare{
		ID: shareUID, Lane: laneLocal, Server: teamServerID, Scope: "all", Name: "North", Color: laneColor,
		Members: []team.Member{{Seat: shareSeat, Access: "read"}}, Rev: 2, Starts: map[string]int{}, Excluded: []string{"old"},
		Generation: shareGen, IndexRev: 2, State: "active", SharedAt: "2026-11-12T09:30:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: shareNode, Lane: shareUID, Members: []team.Member{}})
	rec = doTeam(h, http.MethodGet, "/api/team/lane-shares/"+laneLocal, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	got := decodeLaneView(t, rec)
	if got.ID != shareUID || got.Chats != 1 || got.Excluded != 1 || got.Scope != "all" || got.Members[0].Access != "read" {
		t.Fatalf("view = %+v", got)
	}
	list := doTeam(h, http.MethodGet, "/api/team/lane-shares", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"lane_shares":[`) || !strings.Contains(list.Body.String(), shareUID) {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
}

func TestLaneShareRejectsABadBody(t *testing.T) {
	h, dir, _, inst := readySurface(t, &shareScript{})
	good := lanePutBody("from_now", "North", laneColor, []team.Member{{Seat: shareSeat, Access: "write"}}, []string{})
	for _, body := range []string{
		strings.Replace(good, `"from_now"`, `"sideways"`, 1),
		`{"server":"` + teamServerID + `","scope":"all","name":"North","color":"` + laneColor + `","excluded":[]}`,
		lanePutBody("all", "North\n", laneColor, []team.Member{}, []string{}),
		lanePutBody("all", "North", "#1A2B3C", []team.Member{}, []string{}),
		lanePutBody("all", "North", laneColor, []team.Member{}, []string{"bad id"}),
		good[:len(good)-1] + `,"extra":1}`,
		lanePutBody("all", "North", laneColor, []team.Member{{Seat: inst, Access: "write"}}, []string{}),
	} {
		rec := putLane(h, laneLocal, body)
		assertAPIError(t, rec, http.StatusBadRequest, "bad_request")
	}
	if _, err := os.Stat(filepath.Join(dir, team.LaneSharesFile)); !os.IsNotExist(err) {
		t.Fatal("a rejected put created the lane file")
	}
}

func TestLaneShareUpdateKeepsServerAndScope(t *testing.T) {
	script := &shareScript{}
	h, dir, _, inst := readySurface(t, script)
	script.channel = channelJSON(inst, shareUID, "Shared lane · North", shareGen)
	if err := team.SaveLaneShare(dir, team.LaneShare{
		ID: shareUID, Lane: laneLocal, Server: teamServerID, Scope: "from_now", Name: "North", Color: laneColor,
		Members: []team.Member{{Seat: shareSeat, Access: "read"}}, Rev: 1, Starts: map[string]int{}, Excluded: []string{},
		Generation: shareGen, IndexRev: 1, State: "active", SharedAt: "2026-11-12T09:30:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	rec := putLane(h, laneLocal, lanePutBody("all", "North", laneColor, []team.Member{{Seat: shareSeat, Access: "read"}}, []string{}))
	assertAPIError(t, rec, http.StatusConflict, "share_fixed")
	rec = putLane(h, laneLocal, lanePutBody("from_now", "Renamed", "#aabbcc", []team.Member{{Seat: shareSeat, Access: "read"}}, []string{}))
	if rec.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body.String())
	}
	lanes, err := team.LoadLaneShares(dir)
	if err != nil || lanes[0].Name != "Renamed" || lanes[0].Color != "#aabbcc" || lanes[0].Rev != 1 {
		t.Fatalf("rename stored = %+v %v", lanes, err)
	}
	rec = putLane(h, laneLocal, lanePutBody("from_now", "Renamed", "#aabbcc", []team.Member{{Seat: shareSeat, Access: "write"}, {Seat: shareSeatB, Access: "read"}}, []string{}))
	if rec.Code != http.StatusOK {
		t.Fatalf("members = %d %s", rec.Code, rec.Body.String())
	}
	lanes, err = team.LoadLaneShares(dir)
	if err != nil || lanes[0].Rev != 2 || len(lanes[0].Members) != 2 {
		t.Fatalf("members stored = %+v %v", lanes, err)
	}
	stopping := lanes[0]
	stopping.State = "stopping"
	if err := team.SaveLaneShare(dir, stopping); err != nil {
		t.Fatal(err)
	}
	rec = putLane(h, laneLocal, lanePutBody("from_now", "Renamed", "#aabbcc", lanes[0].Members, []string{}))
	assertAPIError(t, rec, http.StatusConflict, "not_active")
}

func TestLaneShareDeleteAnswersLikeAChatShare(t *testing.T) {
	script := &shareScript{}
	h, dir, _, inst := readySurface(t, script)
	script.channel = channelJSON(inst, shareUID, "Shared lane · North", shareGen)
	save := func(state string) {
		t.Helper()
		if err := team.SaveLaneShare(dir, team.LaneShare{
			ID: shareUID, Lane: laneLocal, Server: teamServerID, Scope: "all", Name: "North", Color: laneColor,
			Members: []team.Member{}, Rev: 1, Starts: map[string]int{}, Excluded: []string{},
			Generation: shareGen, IndexRev: 1, State: state, SharedAt: "2026-11-12T09:30:00Z",
		}); err != nil {
			t.Fatal(err)
		}
	}
	save("active")
	mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: shareNode, Lane: shareUID, Members: []team.Member{}})
	rec := doTeam(h, http.MethodDelete, "/api/team/lane-shares/"+laneLocal, "")
	view := decodeLaneView(t, rec)
	if rec.Code != http.StatusAccepted || view.State != "stopping" || view.Chats != 1 {
		t.Fatalf("delete = %d %+v %s", rec.Code, view, rec.Body.String())
	}
	lanes, err := team.LoadLaneShares(dir)
	if err != nil || len(lanes) != 1 || lanes[0].State != "stopping" {
		t.Fatalf("lanes = %+v %v", lanes, err)
	}
	stored, ok := storedShare(t, dir, shareUIDB)
	if !ok || stored.State != "stopping" {
		t.Fatalf("chat = %+v ok %v", stored, ok)
	}
	save("active")
	script.failPath = "/v1/hello"
	rec = doTeam(h, http.MethodDelete, "/api/team/lane-shares/"+laneLocal, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unreachable = %d %s", rec.Code, rec.Body.String())
	}
	lanes, err = team.LoadLaneShares(dir)
	if err != nil || len(lanes) != 1 || lanes[0].State != "stopping" {
		t.Fatalf("stopping = %+v %v", lanes, err)
	}
}

func TestLaneShareOwnedChatRejectsMutation(t *testing.T) {
	h, dir, _, _ := readySurface(t, &shareScript{})
	appendNode(t, dir, shareNode, "North")
	writeChatLog(t, dir, shareNode, shareUID, 1)
	if err := team.SaveLaneShare(dir, team.LaneShare{
		ID: shareUID, Lane: laneLocal, Server: teamServerID, Scope: "all", Name: "North", Color: laneColor,
		Members: []team.Member{{Seat: shareSeat, Access: "write"}}, Rev: 1, Starts: map[string]int{}, Excluded: []string{},
		Generation: shareGen, IndexRev: 1, State: "active", SharedAt: "2026-11-12T09:30:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Lane: shareUID, Members: []team.Member{}})
	rec := putShare(h, shareNode, sharePutBody(teamServerID, "all", []team.Member{{Seat: shareSeat, Access: "write"}}))
	assertAPIError(t, rec, http.StatusConflict, "lane_share")
	rec = doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
	assertAPIError(t, rec, http.StatusConflict, "lane_share")
	rec = doTeam(h, http.MethodGet, "/api/team/shares/"+shareNode, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"lane":"`+shareUID+`"`) || !strings.Contains(rec.Body.String(), shareSeat) {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
}

func TestLaneShareEdges(t *testing.T) {
	script := &shareScript{}
	h, dir, calls, inst := readySurface(t, script)
	script.channel = channelJSON(inst, shareUID, "Shared lane · North", shareGen)
	th := asTeamHandler(t, h)
	if rec := doTeam(h, http.MethodPost, "/api/team/lane-shares", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("post list = %d", rec.Code)
	}
	for _, path := range []string{"/api/team/lane-shares/", "/api/team/lane-shares/north/x", "/api/team/lane-shares/lane!", "/api/team/lane-shares/" + strings.Repeat("a", 81)} {
		if rec := doTeam(h, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}
	if rec := doTeam(h, http.MethodPost, "/api/team/lane-shares/"+laneLocal, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("post one = %d", rec.Code)
	}
	body := lanePutBody("all", "North", laneColor, []team.Member{{Seat: shareSeat, Access: "read"}}, []string{})
	rec := putLane(h, laneLocal, strings.Replace(body, `"excluded":[]`, `"excluded":["old","old"]`, 1))
	assertAPIError(t, rec, http.StatusBadRequest, "bad_request")
	rec = putLane(h, laneLocal, strings.Replace(body, `"color":"`+laneColor+`"`, `"color":"blue"`, 1))
	assertAPIError(t, rec, http.StatusBadRequest, "bad_request")
	rec = putLane(h, laneLocal, strings.Replace(body, `"name":"North"`, `"name":"`+strings.Repeat("n", 81)+`"`, 1))
	assertAPIError(t, rec, http.StatusBadRequest, "bad_request")
	rec = putLane(h, laneLocal, strings.Replace(body, `,"excluded":[]`, ``, 1))
	assertAPIError(t, rec, http.StatusBadRequest, "bad_request")
	rec = putLane(h, "missing", lanePutBody("all", "North", laneColor, []team.Member{}, []string{"", "ok"}))
	assertAPIError(t, rec, http.StatusBadRequest, "bad_request")
	if err := os.Remove(filepath.Join(dir, identity.InstallationFile)); err != nil {
		t.Fatal(err)
	}
	rec = putLane(h, laneLocal, body)
	assertAPIError(t, rec, http.StatusInternalServerError, "identity_unavailable")
	if _, err := identity.LoadOrCreateInstallation(dir); err != nil {
		t.Fatal(err)
	}
	rec = putLane(h, laneLocal, strings.Replace(body, teamServerID, teamOtherID, 1))
	assertAPIError(t, rec, http.StatusNotFound, "not_found")
	unreachable := &shareScript{failPath: "/v1/hello"}
	uh, _, _, _ := readySurface(t, unreachable)
	rec = putLane(uh, laneLocal, body)
	assertAPIError(t, rec, http.StatusBadGateway, "unreachable")
	script.helloID = teamOtherID
	rec = putLane(h, laneLocal, body)
	assertAPIError(t, rec, http.StatusBadGateway, "bad_server")
	script.helloID = ""
	if err := os.Mkdir(filepath.Join(dir, "nodes.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	rec = putLane(h, laneLocal, lanePutBody("from_now", "North", laneColor, []team.Member{}, []string{}))
	assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
	if err := os.Remove(filepath.Join(dir, "nodes.jsonl")); err != nil {
		t.Fatal(err)
	}
	kicked := 0
	th.kickShares = func() { kicked++ }
	rec = putLane(h, laneLocal, lanePutBody("all", "North", laneColor, []team.Member{{Seat: shareSeat, Access: "read"}}, []string{"a_b-c"}))
	if sameMembers([]team.Member{{Seat: shareSeat, Access: "read"}}, []team.Member{{Seat: shareSeat, Access: "write"}}) {
		t.Fatal("different access compared equal")
	}
	if rec.Code != http.StatusCreated || kicked != 1 {
		t.Fatalf("all = %d kicked %d %s", rec.Code, kicked, rec.Body.String())
	}
	lanes, err := team.LoadLaneShares(dir)
	if err != nil || len(lanes) != 1 || len(lanes[0].Starts) != 0 || lanes[0].Scope != "all" {
		t.Fatalf("all stored = %+v %v", lanes, err)
	}
	only := fmt.Sprintf(`{"server":%q,"scope":"all","members":[{"seat":%q,"access":"write"}],"name":"North","color":%q,"excluded":[],"membership_only":{"uid":%q,"generation":"","baseline":[]}}`, teamServerID, shareSeat, laneColor, lanes[0].ID)
	rec = putLane(h, laneLocal, only)
	assertAPIError(t, rec, http.StatusConflict, "share_changed")
	only = fmt.Sprintf(`{"server":%q,"scope":"all","members":[{"seat":%q,"access":"read"}],"name":"Kept","color":%q,"excluded":[],"membership_only":{"uid":%q,"generation":%q,"baseline":[%q]}}`, teamServerID, shareSeat, laneColor, lanes[0].ID, shareGen, shareSeat)
	script.channelsBody = ownerChannels(inst, ownerChannel(inst, lanes[0].ID, "North", shareGen, "[]"))
	before := callCount(calls, http.MethodGet, "/v1/channels")
	rec = putLane(h, laneLocal, only)
	if callCount(calls, http.MethodGet, "/v1/channels") != before {
		t.Fatalf("same members read the index %+v", snapshotCalls(calls))
	}
	if rec.Code != http.StatusOK || kicked != 2 {
		t.Fatalf("fold = %d %s", rec.Code, rec.Body.String())
	}
	lanes, err = team.LoadLaneShares(dir)
	if err != nil || lanes[0].Name != "Kept" || lanes[0].Rev != 1 || lanes[0].Members[0].Access != "read" {
		t.Fatalf("fold stored = %+v %v", lanes, err)
	}
	rec = putLane(h, "other", only)
	assertAPIError(t, rec, http.StatusConflict, "share_changed")
	script.helloID = teamOtherID
	rec = doTeam(h, http.MethodDelete, "/api/team/lane-shares/"+laneLocal, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("mismatch = %d %s", rec.Code, rec.Body.String())
	}
	script.helloID = ""
	if err := team.SaveLaneShare(dir, lanes[0]); err != nil {
		t.Fatal(err)
	}
	script.deleteStatus = http.StatusNotFound
	rec = doTeam(h, http.MethodDelete, "/api/team/lane-shares/"+laneLocal, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("missing channel = %d %s", rec.Code, rec.Body.String())
	}
	if lanes, err = team.LoadLaneShares(dir); err != nil || lanes[0].State != "stopping" {
		t.Fatalf("kept = %+v %v", lanes, err)
	}
	gone := doTeam(h, http.MethodDelete, "/api/team/lane-shares/absent", "")
	assertAPIError(t, gone, http.StatusNotFound, "not_found")
	forgotten := lanes[0]
	forgotten.Lane = "gone"
	forgotten.Server = teamOtherID
	forgotten.State = "active"
	if err := team.SaveLaneShare(dir, forgotten); err != nil {
		t.Fatal(err)
	}
	rec = doTeam(h, http.MethodDelete, "/api/team/lane-shares/gone", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("forgotten = %d %s", rec.Code, rec.Body.String())
	}
	lanes, err = team.LoadLaneShares(dir)
	kept := false
	for _, lane := range lanes {
		if lane.Lane == "gone" && lane.State == "stopping" {
			kept = true
		}
	}
	if err != nil || !kept {
		t.Fatalf("forgotten stored = %+v %v", lanes, err)
	}
	th.mintChat = func() (string, error) { return "0123456789abcdef", nil }
	if _, err := th.freshIndexID("0123456789abcdef"); err == nil {
		t.Fatal("collision accepted")
	}
	th.mintChat = func() (string, error) { return "short", nil }
	if _, err := th.freshIndexID(laneLocal); err == nil {
		t.Fatal("short id accepted")
	}
	th.mintChat = func() (string, error) { return "", errors.New("boom") }
	if _, err := th.freshIndexID(laneLocal); err == nil {
		t.Fatal("mint error accepted")
	}
	th.mintChat = nil
	prev := readChatEntropy
	readChatEntropy = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	t.Cleanup(func() { readChatEntropy = prev })
	if _, err := th.freshIndexID(laneLocal); err == nil {
		t.Fatal("entropy error accepted")
	}
	readChatEntropy = prev
	starts, err := th.laneStarts(laneLocal, "from_now")
	if err != nil || len(starts) != 0 {
		t.Fatalf("missing nodes = %+v %v", starts, err)
	}
	appendLaneNode(t, dir, "live", "Live", laneLocal, "2026-11-12T09:30:00Z")
	appendLaneNode(t, dir, "other", "Other", "south", "2026-11-12T09:30:00Z")
	appendDelete(t, dir, "live")
	starts, err = th.laneStarts(laneLocal, "from_now")
	if err != nil || len(starts) != 0 {
		t.Fatalf("deleted = %+v %v", starts, err)
	}
	if err := os.WriteFile(filepath.Join(dir, team.LaneSharesFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec = doTeam(h, http.MethodGet, "/api/team/lane-shares", "")
	assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Lane: shareUIDB, Members: []team.Member{}})
	rec = doTeam(h, http.MethodGet, "/api/team/shares/"+shareNode, "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), shareSeat) {
		t.Fatalf("damaged lane members = %d %s", rec.Code, rec.Body.String())
	}
}

func seedLane(t *testing.T, dir, lane, id string) {
	t.Helper()
	if err := team.SaveLaneShare(dir, team.LaneShare{
		ID: id, Lane: lane, Server: teamServerID, Scope: "all", Name: "North", Color: laneColor,
		Members: []team.Member{}, Rev: 1, Starts: map[string]int{}, Excluded: []string{},
		Generation: shareGen, IndexRev: 1, State: "active", SharedAt: "2026-11-12T09:30:00Z",
	}); err != nil {
		t.Fatal(err)
	}
}

func denyDataWrites(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
}

func TestLaneShareStoreEdges(t *testing.T) {
	t.Run("damaged files", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		if err := os.WriteFile(filepath.Join(dir, team.LaneSharesFile), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		body := lanePutBody("all", "North", laneColor, []team.Member{}, []string{})
		for _, call := range []struct{ method, path string }{
			{http.MethodGet, "/api/team/lane-shares/" + laneLocal},
			{http.MethodPut, "/api/team/lane-shares/" + laneLocal},
			{http.MethodDelete, "/api/team/lane-shares/" + laneLocal},
		} {
			rec := doTeam(h, call.method, call.path, body)
			assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		}
		if err := os.Remove(filepath.Join(dir, team.LaneSharesFile)); err != nil {
			t.Fatal(err)
		}
		seedLane(t, dir, laneLocal, shareUID)
		if err := os.WriteFile(filepath.Join(dir, team.SharesFile), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := doTeam(h, http.MethodGet, "/api/team/lane-shares", "")
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		rec = putLane(h, laneLocal, lanePutBody("all", "Kept", laneColor, []team.Member{}, []string{}))
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		lanes, err := team.LoadLaneShares(dir)
		if err != nil || lanes[0].Name != "Kept" {
			t.Fatalf("name saved before the share read = %+v %v", lanes, err)
		}
	})

	t.Run("full list", func(t *testing.T) {
		script := &shareScript{}
		h, dir, _, inst := readySurface(t, script)
		const id = "ffffffffffffffff"
		script.channel = channelJSON(inst, id, "Shared lane · North", shareGen)
		for i := 0; i < 64; i++ {
			seedLane(t, dir, fmt.Sprintf("n%d", i), fmt.Sprintf("%016x", i+1))
		}
		th := asTeamHandler(t, h)
		th.mintChat = func() (string, error) { return id, nil }
		rec := putLane(h, "extra", lanePutBody("all", "North", laneColor, []team.Member{}, []string{}))
		assertAPIError(t, rec, http.StatusInsufficientStorage, "too_many")
		lanes, err := team.LoadLaneShares(dir)
		if err != nil || len(lanes) != 64 {
			t.Fatalf("lanes = %d %v", len(lanes), err)
		}
	})

	t.Run("unwritable", func(t *testing.T) {
		script := &shareScript{}
		h, dir, _, inst := readySurface(t, script)
		const id = "ffffffffffffffff"
		script.channel = channelJSON(inst, id, "Shared lane · North", shareGen)
		th := asTeamHandler(t, h)
		th.mintChat = func() (string, error) { return id, nil }
		denyDataWrites(t, dir)
		rec := putLane(h, laneLocal, lanePutBody("all", "North", laneColor, []team.Member{}, []string{}))
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
	})

	t.Run("unwritable revise", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		seedLane(t, dir, laneLocal, shareUID)
		denyDataWrites(t, dir)
		rec := putLane(h, laneLocal, lanePutBody("all", "Kept", laneColor, []team.Member{}, []string{}))
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		_ = os.Chmod(dir, 0o700)
		lanes, err := team.LoadLaneShares(dir)
		if err != nil || lanes[0].Name != "North" {
			t.Fatalf("revise wrote = %+v %v", lanes, err)
		}
	})

	t.Run("unwritable stop", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		seedLane(t, dir, laneLocal, shareUID)
		denyDataWrites(t, dir)
		rec := doTeam(h, http.MethodDelete, "/api/team/lane-shares/"+laneLocal, "")
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		_ = os.Chmod(dir, 0o700)
		lanes, err := team.LoadLaneShares(dir)
		if err != nil || lanes[0].State != "active" {
			t.Fatalf("state = %+v %v", lanes, err)
		}
	})

	t.Run("unwritable chat", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		seedLane(t, dir, laneLocal, shareUID)
		mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: shareNode, Lane: shareUID, Members: []team.Member{}})
		denyDataWrites(t, dir)
		rec := doTeam(h, http.MethodDelete, "/api/team/lane-shares/"+laneLocal, "")
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		_ = os.Chmod(dir, 0o700)
		stored, ok := storedShare(t, dir, shareUIDB)
		lanes, err := team.LoadLaneShares(dir)
		if !ok || stored.State != "active" || err != nil || lanes[0].State != "active" {
			t.Fatalf("chat = %+v lanes %+v %v", stored, lanes, err)
		}
	})

	t.Run("delete ladder", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		seedLane(t, dir, laneLocal, shareUID)
		if err := os.Remove(filepath.Join(dir, identity.InstallationFile)); err != nil {
			t.Fatal(err)
		}
		rec := doTeam(h, http.MethodDelete, "/api/team/lane-shares/"+laneLocal, "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("no installation = %d %s", rec.Code, rec.Body.String())
		}
		if _, err := identity.LoadOrCreateInstallation(dir); err != nil {
			t.Fatal(err)
		}
		seedLane(t, dir, laneLocal, shareUID)
		if err := os.WriteFile(filepath.Join(dir, team.File), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec = doTeam(h, http.MethodDelete, "/api/team/lane-shares/"+laneLocal, "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("damaged servers = %d %s", rec.Code, rec.Body.String())
		}
		lanes, err := team.LoadLaneShares(dir)
		if err != nil || lanes[0].State != "stopping" {
			t.Fatalf("stopping = %+v %v", lanes, err)
		}
	})

	t.Run("delete answers", func(t *testing.T) {
		script := &shareScript{}
		h, dir, _, inst := readySurface(t, script)
		for _, setup := range []struct {
			name  string
			apply func()
		}{
			{"hello", func() { script.helloStatus = http.StatusInternalServerError }},
			{"delete", func() { script.deleteStatus = http.StatusInternalServerError }},
			{"unauthorized", func() { script.deleteStatus = http.StatusUnauthorized }},
			{"conflict", func() { script.deleteStatus = http.StatusConflict }},
			{"dropped", func() { script.failPath = channelBase(inst, shareUID) }},
		} {
			seedLane(t, dir, laneLocal, shareUID)
			script.helloStatus = 0
			script.deleteStatus = 0
			script.failPath = ""
			setup.apply()
			rec := doTeam(h, http.MethodDelete, "/api/team/lane-shares/"+laneLocal, "")
			lanes, err := team.LoadLaneShares(dir)
			if rec.Code != http.StatusAccepted || err != nil || len(lanes) != 1 || lanes[0].State != "stopping" {
				t.Fatalf("%s = %d %+v %v %s", setup.name, rec.Code, lanes, err, rec.Body.String())
			}
		}
	})

	t.Run("mixed chats", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		seedLane(t, dir, laneLocal, shareUID)
		mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: shareNode, Lane: shareUID, Members: []team.Member{}})
		mustSaveShare(t, dir, team.Share{UID: shareUIDC, Node: "other", Members: []team.Member{}})
		mustSaveShare(t, dir, team.Share{UID: "0000000000000002", Node: "third", Lane: shareUID, State: "stopping", Members: []team.Member{}})
		rec := doTeam(h, http.MethodDelete, "/api/team/lane-shares/"+laneLocal, "")
		lanes, err := team.LoadLaneShares(dir)
		if rec.Code != http.StatusAccepted || err != nil || len(lanes) != 1 || lanes[0].State != "stopping" {
			t.Fatalf("delete = %d %+v %v %s", rec.Code, lanes, err, rec.Body.String())
		}
		matched, ok := storedShare(t, dir, shareUIDB)
		plain, plainOK := storedShare(t, dir, shareUIDC)
		stopped, stopOK := storedShare(t, dir, "0000000000000002")
		if !ok || matched.State != "stopping" || !plainOK || plain.State != "active" || plain.Lane != "" || !stopOK || stopped.State != "stopping" {
			t.Fatalf("matched %+v plain %+v stopped %+v", matched, plain, stopped)
		}
	})

	t.Run("fold cap", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		if err := team.SaveLaneShare(dir, team.LaneShare{
			ID: shareUID, Lane: laneLocal, Server: teamServerID, Scope: "all", Name: "North", Color: laneColor,
			Members: []team.Member{{Seat: shareSeat, Access: "read"}}, Rev: 1, Starts: map[string]int{}, Excluded: []string{},
			Generation: shareGen, IndexRev: 1, State: "active", SharedAt: "2026-11-12T09:30:00Z",
		}); err != nil {
			t.Fatal(err)
		}
		members := make([]team.Member, 200)
		for i := range members {
			members[i] = team.Member{Seat: fmt.Sprintf("%032x", i+1), Access: "write"}
		}
		raw, err := json.Marshal(map[string]any{
			"server": teamServerID, "scope": "all", "name": "North", "color": laneColor, "excluded": []string{},
			"members": members, "membership_only": map[string]any{"uid": shareUID, "generation": shareGen, "baseline": []string{}},
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := putLane(h, laneLocal, string(raw))
		assertAPIError(t, rec, http.StatusBadRequest, "bad_request")
		lanes, err := team.LoadLaneShares(dir)
		if err != nil || lanes[0].Rev != 1 || len(lanes[0].Members) != 1 {
			t.Fatalf("fold cap stored = %+v %v", lanes, err)
		}
	})

	t.Run("missing lane members", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		if err := team.SaveLaneShare(dir, team.LaneShare{
			ID: shareUID, Lane: laneLocal, Server: teamServerID, Scope: "all", Name: "North", Color: laneColor,
			Members: []team.Member{{Seat: shareSeat, Access: "read"}}, Rev: 1, Starts: map[string]int{}, Excluded: []string{},
			Generation: shareGen, IndexRev: 1, State: "active", SharedAt: "2026-11-12T09:30:00Z",
		}); err != nil {
			t.Fatal(err)
		}
		mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: shareNode, Lane: shareUIDC, Members: []team.Member{}})
		rec := doTeam(h, http.MethodGet, "/api/team/shares/"+shareNode, "")
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), shareSeat) || !strings.Contains(rec.Body.String(), `"lane":"`+shareUIDC+`"`) {
			t.Fatalf("missing lane = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("index id", func(t *testing.T) {
		h, _, _, _ := readySurface(t, &shareScript{})
		asTeamHandler(t, h).mintChat = func() (string, error) { return "", errors.New("boom") }
		rec := putLane(h, laneLocal, lanePutBody("all", "North", laneColor, []team.Member{}, []string{}))
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
	})

	t.Run("unwritable remove", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		if err := team.SaveLaneShare(dir, team.LaneShare{
			ID: shareUID, Lane: laneLocal, Server: teamOtherID, Scope: "all", Name: "North", Color: laneColor,
			Members: []team.Member{}, Rev: 1, Starts: map[string]int{}, Excluded: []string{},
			Generation: shareGen, IndexRev: 1, State: "active", SharedAt: "2026-11-12T09:30:00Z",
		}); err != nil {
			t.Fatal(err)
		}
		denyDataWrites(t, dir)
		rec := doTeam(h, http.MethodDelete, "/api/team/lane-shares/"+laneLocal, "")
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
	})

	t.Run("damaged chats", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		seedLane(t, dir, laneLocal, shareUID)
		if err := os.WriteFile(filepath.Join(dir, team.SharesFile), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := doTeam(h, http.MethodDelete, "/api/team/lane-shares/"+laneLocal, "")
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		lanes, err := team.LoadLaneShares(dir)
		if err != nil || lanes[0].State != "active" {
			t.Fatalf("lane = %+v %v", lanes, err)
		}
		if err := asTeamHandler(t, h).markLaneChatsStopping(shareUID); err == nil {
			t.Fatal("damaged share file marked")
		}
	})

	t.Run("other chat", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		seedLane(t, dir, laneLocal, shareUID)
		mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: shareNode, Lane: shareUID, Members: []team.Member{}})
		mustSaveShare(t, dir, team.Share{UID: "0000000000000002", Node: "other", Lane: shareUIDC, Members: []team.Member{}})
		rec := doTeam(h, http.MethodGet, "/api/team/lane-shares/"+laneLocal, "")
		got := decodeLaneView(t, rec)
		if rec.Code != http.StatusOK || got.Chats != 1 {
			t.Fatalf("chats = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("helpers", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		th := asTeamHandler(t, h)
		seedLane(t, dir, laneLocal, shareUID)
		if err := th.removeLane(shareUID); err != nil {
			t.Fatalf("remove = %v", err)
		}
		if err := th.removeLane(shareUID); err != nil {
			t.Fatalf("missing remove = %v", err)
		}
		got := channelMembers(openedShare{Channel: []team.Member{{Seat: shareSeat, Access: "read"}}})
		if len(got) != 1 || got[0].Seat != shareSeat {
			t.Fatalf("channel list = %+v", got)
		}
		got = channelMembers(openedShare{Members: []team.Member{{Seat: shareSeatB, Access: "write"}}})
		if len(got) != 1 || got[0].Seat != shareSeatB {
			t.Fatalf("stored list = %+v", got)
		}
	})
}

func TestLaneLeaveSurvivesAMemberEdit(t *testing.T) {
	const dana = "aabbccddeeff00112233445566778899"
	script := &shareScript{}
	h, dir, calls, inst := readySurface(t, script)
	script.channel = channelJSON(inst, shareUID, "Shared lane · North", shareGen)
	if err := team.SaveLaneShare(dir, team.LaneShare{
		ID: shareUID, Lane: laneLocal, Server: teamServerID, Scope: "all", Name: "North", Color: laneColor,
		Members: []team.Member{{Seat: shareSeat, Access: "write"}, {Seat: shareSeatB, Access: "read"}},
		Rev:     1, Starts: map[string]int{}, Excluded: []string{}, Generation: shareGen, IndexRev: 1,
		State: "active", SharedAt: "2026-11-12T09:30:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	saveQuietChat(t, dir, "keep", "Keep", shareUIDB, shareUID, shareGen, "active", "2026-11-12T09:00:00Z", 1)
	script.channelsBody = ownerChannels(inst,
		ownerChannel(inst, shareUIDB, "Other", shareGen, `[{"seat":"`+shareSeatB+`","access":"read"}]`),
		ownerChannel(inst, shareUID, "North", shareGen, `[{"seat":"`+shareSeat+`","access":"write"}]`),
	)
	raw, err := json.Marshal(map[string]any{
		"server": teamServerID, "scope": "all", "name": "North", "color": laneColor, "excluded": []string{},
		"members": []team.Member{
			{Seat: shareSeat, Access: "write"},
			{Seat: shareSeatB, Access: "write"},
			{Seat: dana, Access: "write"},
		},
		"membership_only": map[string]any{"uid": shareUID, "generation": shareGen, "baseline": []string{shareSeat, shareSeatB}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := putLane(h, laneLocal, string(raw))
	lanes, err := team.LoadLaneShares(dir)
	if err != nil || rec.Code != http.StatusOK || len(lanes) != 1 || len(lanes[0].Members) != 2 || lanes[0].Members[0].Seat != shareSeat || lanes[0].Members[1].Seat != dana {
		t.Fatalf("stored %d %+v %v", rec.Code, lanes, err)
	}
	th := asTeamHandler(t, h)
	box := &shareOutbox{dataDir: dir, now: th.now, client: th.client, shareOps: &th.shareOps, files: &th.mu}
	box.tick(context.Background())
	pushed := 0
	for _, c := range snapshotCalls(calls) {
		if c.method != http.MethodPut || !strings.HasSuffix(c.path, "/members") {
			continue
		}
		pushed++
		if strings.Contains(c.body, shareSeatB) {
			t.Fatalf("push %s", c.body)
		}
	}
	if pushed != 2 {
		t.Fatalf("pushes %d %+v", pushed, snapshotCalls(calls))
	}
}

func TestLaneMemberEditReportsAFailedIndexRead(t *testing.T) {
	stored := team.LaneShare{
		ID: shareUID, Lane: laneLocal, Server: teamServerID, Scope: "all", Name: "North", Color: laneColor,
		Members: []team.Member{{Seat: shareSeat, Access: "write"}, {Seat: shareSeatB, Access: "read"}},
		Rev:     1, Starts: map[string]int{}, Excluded: []string{}, Generation: shareGen, IndexRev: 1,
		State: "active", SharedAt: "2026-11-12T09:30:00Z",
	}
	body, err := json.Marshal(map[string]any{
		"server": teamServerID, "scope": "all", "name": "North", "color": laneColor, "excluded": []string{},
		"members":         []team.Member{{Seat: shareSeat, Access: "write"}},
		"membership_only": map[string]any{"uid": shareUID, "generation": shareGen, "baseline": []string{shareSeatB}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, code string
		script     *shareScript
	}{
		{"hello", "unreachable", &shareScript{failPath: "/v1/hello"}},
		{"channels", "bad_server", &shareScript{channelsStatus: http.StatusInternalServerError}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, dir, _, _ := readySurface(t, tc.script)
			if err := team.SaveLaneShare(dir, stored); err != nil {
				t.Fatal(err)
			}
			rec := putLane(h, laneLocal, string(body))
			lanes, err := team.LoadLaneShares(dir)
			if err != nil || len(lanes) != 1 || lanes[0].Rev != 1 || len(lanes[0].Members) != 2 {
				t.Fatalf("stored %+v %v", lanes, err)
			}
			assertAPIError(t, rec, http.StatusBadGateway, tc.code)
		})
	}
}

func TestLaneShareRoutesAreDocumented(t *testing.T) {
	root := repoRootFromTest(t)
	api, err := os.ReadFile(filepath.Join(root, "docs", "http-api.md"))
	if err != nil {
		t.Fatal(err)
	}
	section := string(api)
	if i := strings.Index(section, "## Team\n"); i >= 0 {
		section = section[i:]
	}
	for _, needle := range []string{
		"GET /api/team/lane-shares",
		"GET /api/team/lane-shares/{lane}",
		"PUT /api/team/lane-shares/{lane}",
		"DELETE /api/team/lane-shares/{lane}",
		"lane_share",
		"Shared lane · ",
	} {
		if !strings.Contains(section, needle) {
			t.Errorf("docs/http-api.md Team section missing %q", needle)
		}
	}
}
