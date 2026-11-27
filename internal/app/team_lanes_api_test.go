package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if lanes, err := team.LoadLaneShares(dir); err != nil || len(lanes) != 0 {
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
	lanes, err := team.LoadLaneShares(dir)
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
