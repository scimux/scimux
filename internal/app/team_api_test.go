package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

const (
	teamServerID = "0123456789abcdef0123456789abcdef"
	teamOtherID  = "fedcba9876543210fedcba9876543210"
	teamToken    = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	teamCode     = "SECRET-JOIN-CODE"
)

func TestExperimentalTeamGateIsExactAndFailClosed(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"true", false},
		{"01", false},
		{"1", true},
	} {
		got := experimentalTeamEnabled(func(string) string { return tc.value })
		if got != tc.want {
			t.Errorf("value %q: enabled = %v, want %v", tc.value, got, tc.want)
		}
	}
	if experimentalTeamEnabled(nil) {
		t.Fatal("nil environment reader enabled the team gate")
	}
}

func startTeamCommand(t *testing.T, teamOn, muxer bool) (*Command, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	data := t.TempDir()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd := &Command{
		Args:             []string{"scimux", "-addr", "127.0.0.1:0", "-data", data},
		Home:             t.TempDir(),
		Stdout:           stdout,
		Stderr:           stderr,
		ExperimentalTeam: teamOn,
		muxerOnly:        muxer,
	}
	if err := cmd.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cmd.closeListener)
	t.Cleanup(cmd.closeOwnership)
	return cmd, data, stdout, stderr
}

func recordedText(h http.Handler, method, target string) string {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "http://127.0.0.1"+target, nil)
	req.Host = "127.0.0.1"
	h.ServeHTTP(rec, req)
	return dumpRecorder(rec)
}

func dumpRecorder(rec *httptest.ResponseRecorder) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status %d\n", rec.Code)
	keys := make([]string, 0, len(rec.Header()))
	for k := range rec.Header() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", k, strings.Join(rec.Header().Values(k), ","))
	}
	b.Write(rec.Body.Bytes())
	return b.String()
}

func TestTeamGateOffMatchesUnknownRoute(t *testing.T) {
	cmd, data, _, _ := startTeamCommand(t, false, true)
	h, err := newCoreMux(cmd.application)
	if err != nil {
		t.Fatal(err)
	}
	assertTeamPathsMatchUnknown(t, h)
	assertNoTeamFile(t, data)
}

func TestTeamGateOnOutsideTheMuxerMatchesUnknownRoute(t *testing.T) {
	cmd, data, _, _ := startTeamCommand(t, true, false)
	assertTeamPathsMatchUnknown(t, cmd.Handler())
	assertNoTeamFile(t, data)
	if cmd.application == nil || cmd.application.teamHandler != nil {
		t.Fatal("a non-muxer installed the team handler")
	}

	for _, arg := range []string{webChildCmd, sessionWorkerCmd} {
		t.Run(arg, func(t *testing.T) {
			dir := t.TempDir()
			cmd := &Command{
				Args:             []string{"scimux", arg, "-data", dir},
				Home:             t.TempDir(),
				Stdout:           io.Discard,
				Stderr:           io.Discard,
				ExperimentalTeam: true,
				muxerOnly:        true,
			}
			if err := cmd.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if cmd.application != nil {
				t.Fatal("child process built an application")
			}
			assertNoTeamFile(t, dir)
		})
	}
}

func assertTeamPathsMatchUnknown(t *testing.T, h http.Handler) {
	t.Helper()
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		unknown := recordedText(h, method, "/api/not-a-route")
		for _, path := range []string{
			"/api/team",
			"/api/team/",
			"/api/team/servers",
			"/api/team/servers/" + teamServerID + "/status",
			"/api/team/servers/" + teamServerID + "/leave",
			"/api/team/servers/" + teamServerID + "/seats",
			"/api/team/shares",
			"/api/team/shares/",
			"/api/team/shares/chat",
		} {
			got := recordedText(h, method, path)
			if got != unknown {
				t.Fatalf("%s %s\n%s\nwant the unknown-route response\n%s", method, path, got, unknown)
			}
		}
	}
}

func assertNoTeamFile(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{team.File, team.SharesFile} {
		_, err := os.Stat(filepath.Join(dir, name))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s stat = %v", name, err)
		}
	}
}

func TestTeamMuxerStartupMakesNoConnection(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	t.Cleanup(srv.Close)
	orig := newTeamClient
	t.Cleanup(func() { newTeamClient = orig })
	newTeamClient = func() *team.Client {
		c := orig()
		c.Transport = srv.Client().Transport
		return c
	}
	cmd, data, _, _ := startTeamCommand(t, true, true)
	if cmd.application == nil || cmd.application.teamHandler == nil {
		t.Fatal("muxer did not install the team handler")
	}
	if hits != 0 {
		t.Fatalf("startup contacted the team server %d times", hits)
	}
	assertNoTeamFile(t, data)
}

func fixedTeamClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC) }
}

func testTeamSurface(t *testing.T, client *team.Client) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := identity.LoadOrCreateInstallation(dir); err != nil {
		t.Fatal(err)
	}
	return newTeamHandler(dir, fixedTeamClock(), client), dir
}

func helloJSON(id, name string) string {
	return `{"protocol":1,"server":{"id":"` + id + `","name":"` + name + `"}}`
}

func registerJSON(token, id, name string) string {
	return `{"token":"` + token + `","server":{"id":"` + id + `","name":"` + name + `"}}`
}

func scriptedTeam(t *testing.T, helloStatus int, helloBody string, regStatus int, regBody string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/v1/register" {
			w.WriteHeader(regStatus)
			_, _ = io.WriteString(w, regBody)
			return
		}
		w.WriteHeader(helloStatus)
		_, _ = io.WriteString(w, helloBody)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func clientFor(srv *httptest.Server) *team.Client {
	c := team.NewClient()
	c.Transport = srv.Client().Transport
	return c
}

type teamRoundTrip func(*http.Request) (*http.Response, error)

func (f teamRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func joinPayload(link, name string) string {
	raw, err := json.Marshal(struct {
		Link string `json:"link"`
		Name string `json:"name"`
	}{Link: link, Name: name})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func doTeam(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, rdr)
	req.Host = "127.0.0.1"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func assertAPIError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %s: %v", rec.Body.String(), err)
	}
	if got.Error != code {
		t.Fatalf("error = %q, want %q", got.Error, code)
	}
}

func assertNoSecrets(t *testing.T, body, code, token string) {
	t.Helper()
	if code != "" && strings.Contains(body, code) {
		t.Fatalf("body contains the join code: %s", body)
	}
	if token != "" && strings.Contains(body, token) {
		t.Fatalf("body contains the token: %s", body)
	}
}

func TestTeamListEmpty(t *testing.T) {
	h, _ := testTeamSurface(t, team.NewClient())
	rec := doTeam(h, http.MethodGet, "/api/team/servers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Servers []json.RawMessage `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Servers == nil || len(got.Servers) != 0 {
		t.Fatalf("servers = %s", rec.Body.String())
	}
}

func TestTeamJoinListAndForget(t *testing.T) {
	srv, got := scriptedTeam(t, http.StatusOK, helloJSON(teamServerID, "Lab relay"), http.StatusCreated, registerJSON(teamToken, teamServerID, "Lab relay"))
	h, dir := testTeamSurface(t, clientFor(srv))
	link := srv.URL + "/join#" + teamCode
	body := joinPayload(link, "Ada")

	rec := doTeam(h, http.MethodPost, "/api/team/servers", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("join status = %d body %s", rec.Code, rec.Body.String())
	}
	assertNoSecrets(t, rec.Body.String(), teamCode, teamToken)
	var one struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		URL      string `json:"url"`
		SeatName string `json:"seat_name"`
		JoinedAt string `json:"joined_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if one.ID != teamServerID || one.Name != "Lab relay" || one.URL != srv.URL || one.SeatName != "Ada" || one.JoinedAt != "2026-11-12T09:30:00Z" {
		t.Fatalf("server = %+v", one)
	}
	if strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("response names a token: %s", rec.Body.String())
	}
	info, err := os.Stat(filepath.Join(dir, team.File))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	stored, err := team.Load(dir)
	if err != nil || len(stored) != 1 || stored[0].Token != teamToken {
		t.Fatalf("stored = %+v err %v", stored, err)
	}

	listed := doTeam(h, http.MethodGet, "/api/team/servers", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d", listed.Code)
	}
	assertNoSecrets(t, listed.Body.String(), teamCode, teamToken)
	if !strings.Contains(listed.Body.String(), teamServerID) || strings.Contains(listed.Body.String(), `"token"`) {
		t.Fatalf("list = %s", listed.Body.String())
	}

	again := doTeam(h, http.MethodPost, "/api/team/servers", body)
	assertAPIError(t, again, http.StatusConflict, "already_joined")
	assertNoSecrets(t, again.Body.String(), teamCode, teamToken)
	var registers int
	for _, path := range *got {
		if path == "POST /v1/register" {
			registers++
		}
	}
	if registers != 1 {
		t.Fatalf("register calls = %d, paths %v", registers, *got)
	}

	forgotten := doTeam(h, http.MethodDelete, "/api/team/servers/"+teamServerID, "")
	if forgotten.Code != http.StatusNoContent || forgotten.Body.Len() != 0 {
		t.Fatalf("forget = %d %q", forgotten.Code, forgotten.Body.String())
	}
	pathsBeforeSecondForget := len(*got)
	missing := doTeam(h, http.MethodDelete, "/api/team/servers/"+teamServerID, "")
	assertAPIError(t, missing, http.StatusNotFound, "not_found")
	if len(*got) != pathsBeforeSecondForget {
		t.Fatalf("forget contacted the server: %v", (*got)[pathsBeforeSecondForget:])
	}
	left, err := team.Load(dir)
	if err != nil || len(left) != 0 {
		t.Fatalf("after forget = %+v err %v", left, err)
	}
}

func TestTeamJoinAcceptsA64ByteSeatName(t *testing.T) {
	name := strings.Repeat("é", 32)
	srv, _ := scriptedTeam(t, http.StatusOK, helloJSON(teamServerID, "Lab relay"), http.StatusCreated, registerJSON(teamToken, teamServerID, "Lab relay"))
	h, _ := testTeamSurface(t, clientFor(srv))
	rec := doTeam(h, http.MethodPost, "/api/team/servers", joinPayload(srv.URL+"/join#"+teamCode, name))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	var got struct {
		SeatName string `json:"seat_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SeatName != name {
		t.Fatalf("seat_name = %q", got.SeatName)
	}
}

func TestTeamJoinRejectsTheBody(t *testing.T) {
	link := "http://relay.example/join#" + teamCode
	cases := []struct {
		name string
		body string
	}{
		{"not json", "{"},
		{"unknown field", `{"link":"` + link + `","name":"Ada","extra":true}`},
		{"second value", `{"link":"` + link + `","name":"Ada"}{}`},
		{"name not a string", `{"link":"` + link + `","name":1}`},
		{"too big", `{"link":"` + link + `","name":"` + strings.Repeat("a", 5000) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int
			c := team.NewClient()
			c.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) {
				hits++
				return nil, errors.New("should not dial")
			})
			h, dir := testTeamSurface(t, c)
			rec := doTeam(h, http.MethodPost, "/api/team/servers", tc.body)
			assertAPIError(t, rec, http.StatusBadRequest, "bad_request")
			assertNoSecrets(t, rec.Body.String(), teamCode, "")
			if hits != 0 {
				t.Fatalf("dialed %d times", hits)
			}
			assertNoTeamFile(t, dir)
		})
	}
}

func TestTeamJoinRejectsTheName(t *testing.T) {
	link := "http://relay.example/join#" + teamCode
	cases := []struct {
		name string
		seat string
	}{
		{"empty", ""},
		{"long", strings.Repeat("a", 65)},
		{"leading space", " Ada"},
		{"trailing space", "Ada "},
		{"control", "Lab\nrelay"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int
			c := team.NewClient()
			c.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) {
				hits++
				return nil, errors.New("should not dial")
			})
			h, _ := testTeamSurface(t, c)
			raw, err := json.Marshal(struct {
				Link string `json:"link"`
				Name string `json:"name"`
			}{Link: link, Name: tc.seat})
			if err != nil {
				t.Fatal(err)
			}
			rec := doTeam(h, http.MethodPost, "/api/team/servers", string(raw))
			assertAPIError(t, rec, http.StatusBadRequest, "bad_request")
			assertNoSecrets(t, rec.Body.String(), teamCode, "")
			if hits != 0 {
				t.Fatalf("dialed %d times", hits)
			}
		})
	}
}

func TestTeamJoinRejectsTheLink(t *testing.T) {
	var hits int
	c := team.NewClient()
	c.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) {
		hits++
		return nil, errors.New("should not dial")
	})
	h, _ := testTeamSurface(t, c)
	rec := doTeam(h, http.MethodPost, "/api/team/servers", joinPayload("http://relay.example/welcome#"+teamCode, "Ada"))
	assertAPIError(t, rec, http.StatusBadRequest, "invalid_link")
	assertNoSecrets(t, rec.Body.String(), teamCode, "")
	if strings.Contains(rec.Body.String(), "relay.example") {
		t.Fatalf("body contains the link: %s", rec.Body.String())
	}
	if hits != 0 {
		t.Fatalf("dialed %d times", hits)
	}
}

func TestTeamJoinReportsIdentityUnavailable(t *testing.T) {
	var hits int
	c := team.NewClient()
	c.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) {
		hits++
		return nil, errors.New("should not dial")
	})
	h, dir := testTeamSurface(t, c)
	if err := os.Remove(filepath.Join(dir, identity.InstallationFile)); err != nil {
		t.Fatal(err)
	}
	rec := doTeam(h, http.MethodPost, "/api/team/servers", joinPayload("http://relay.example/join#"+teamCode, "Ada"))
	assertAPIError(t, rec, http.StatusInternalServerError, "identity_unavailable")
	assertNoSecrets(t, rec.Body.String(), teamCode, "")
	if hits != 0 {
		t.Fatalf("dialed %d times", hits)
	}
}

func TestTeamJoinReportsStoreUnavailable(t *testing.T) {
	var hits int
	c := team.NewClient()
	c.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) {
		hits++
		return nil, errors.New("should not dial")
	})
	h, dir := testTeamSurface(t, c)
	if err := os.WriteFile(filepath.Join(dir, team.File), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := doTeam(h, http.MethodPost, "/api/team/servers", joinPayload("http://relay.example/join#"+teamCode, "Ada"))
	assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
	assertNoSecrets(t, rec.Body.String(), teamCode, "")
	if hits != 0 {
		t.Fatalf("dialed %d times", hits)
	}
	after, err := os.ReadFile(filepath.Join(dir, team.File))
	if err != nil || string(after) != "{" {
		t.Fatalf("file = %q err %v", after, err)
	}
}

func TestTeamJoinMapsHelloErrors(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		var paths []string
		c := team.NewClient()
		c.Transport = teamRoundTrip(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.URL.Path)
			return nil, errors.New("dial failed")
		})
		h, dir := testTeamSurface(t, c)
		rec := doTeam(h, http.MethodPost, "/api/team/servers", joinPayload("http://relay.example/join#"+teamCode, "Ada"))
		assertAPIError(t, rec, http.StatusBadGateway, "unreachable")
		assertNoSecrets(t, rec.Body.String(), teamCode, "")
		if len(paths) != 1 || paths[0] != "/v1/hello" {
			t.Fatalf("paths = %v", paths)
		}
		assertNoTeamFile(t, dir)
	})
	t.Run("bad server", func(t *testing.T) {
		srv, got := scriptedTeam(t, http.StatusOK, `{"protocol":0,"server":{"id":"`+teamServerID+`","name":"Lab relay"}}`, http.StatusCreated, registerJSON(teamToken, teamServerID, "Lab relay"))
		h, dir := testTeamSurface(t, clientFor(srv))
		rec := doTeam(h, http.MethodPost, "/api/team/servers", joinPayload(srv.URL+"/join#"+teamCode, "Ada"))
		assertAPIError(t, rec, http.StatusBadGateway, "bad_server")
		assertNoSecrets(t, rec.Body.String(), teamCode, teamToken)
		if len(*got) != 1 || (*got)[0] != "GET /v1/hello" {
			t.Fatalf("paths = %v", *got)
		}
		assertNoTeamFile(t, dir)
	})
}

func TestTeamJoinDoesNotSpendACodeOnAnExistingServer(t *testing.T) {
	srv, got := scriptedTeam(t, http.StatusOK, helloJSON(teamServerID, "Lab relay"), http.StatusCreated, registerJSON(teamToken, teamServerID, "Lab relay"))
	h, dir := testTeamSurface(t, clientFor(srv))
	if err := team.Add(dir, team.Entry{
		ID: teamServerID, Name: "Lab relay", URL: "http://relay.example",
		SeatName: "Ada", Token: strings.Repeat("ab", 32), JoinedAt: "2026-11-12T09:30:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, team.File))
	if err != nil {
		t.Fatal(err)
	}
	rec := doTeam(h, http.MethodPost, "/api/team/servers", joinPayload(srv.URL+"/join#"+teamCode, "Ada"))
	assertAPIError(t, rec, http.StatusConflict, "already_joined")
	assertNoSecrets(t, rec.Body.String(), teamCode, teamToken)
	if len(*got) != 1 || (*got)[0] != "GET /v1/hello" {
		t.Fatalf("paths = %v", *got)
	}
	after, err := os.ReadFile(filepath.Join(dir, team.File))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("already-joined rewrote the file")
	}
}

func TestTeamJoinMapsRegisterErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   int
		code   string
	}{
		{"join refused", http.StatusForbidden, `{"error":"join_refused"}`, http.StatusForbidden, "join_refused"},
		{"already registered", http.StatusConflict, `{"error":"already_registered"}`, http.StatusConflict, "already_registered"},
		{"quota", http.StatusInsufficientStorage, `{"error":"quota"}`, http.StatusInsufficientStorage, "server_full"},
		{"bad request", http.StatusBadRequest, `{"error":"bad_request"}`, http.StatusBadRequest, "bad_request"},
		{"bad server", http.StatusInternalServerError, `{"error":"internal"}`, http.StatusBadGateway, "bad_server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := scriptedTeam(t, http.StatusOK, helloJSON(teamServerID, "Lab relay"), tc.status, tc.body)
			h, dir := testTeamSurface(t, clientFor(srv))
			rec := doTeam(h, http.MethodPost, "/api/team/servers", joinPayload(srv.URL+"/join#"+teamCode, "Ada"))
			assertAPIError(t, rec, tc.want, tc.code)
			assertNoSecrets(t, rec.Body.String(), teamCode, teamToken)
			if len(*got) != 2 || (*got)[0] != "GET /v1/hello" || (*got)[1] != "POST /v1/register" {
				t.Fatalf("paths = %v", *got)
			}
			assertNoTeamFile(t, dir)
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		srv, got := scriptedTeam(t, http.StatusOK, helloJSON(teamServerID, "Lab relay"), http.StatusCreated, registerJSON(teamToken, teamServerID, "Lab relay"))
		c := clientFor(srv)
		base := c.Transport
		c.Transport = teamRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/v1/register" {
				return nil, errors.New("dial failed")
			}
			return base.RoundTrip(r)
		})
		h, dir := testTeamSurface(t, c)
		rec := doTeam(h, http.MethodPost, "/api/team/servers", joinPayload(srv.URL+"/join#"+teamCode, "Ada"))
		assertAPIError(t, rec, http.StatusBadGateway, "unreachable")
		assertNoSecrets(t, rec.Body.String(), teamCode, teamToken)
		if len(*got) != 1 || (*got)[0] != "GET /v1/hello" {
			t.Fatalf("paths = %v", *got)
		}
		assertNoTeamFile(t, dir)
	})
}

func TestTeamJoinReportsAServerIDMismatch(t *testing.T) {
	srv, got := scriptedTeam(t, http.StatusOK, helloJSON(teamServerID, "Lab relay"), http.StatusCreated, registerJSON(teamToken, teamOtherID, "Lab relay"))
	h, dir := testTeamSurface(t, clientFor(srv))
	rec := doTeam(h, http.MethodPost, "/api/team/servers", joinPayload(srv.URL+"/join#"+teamCode, "Ada"))
	assertAPIError(t, rec, http.StatusBadGateway, "bad_server")
	assertNoSecrets(t, rec.Body.String(), teamCode, teamToken)
	if len(*got) != 2 || (*got)[1] != "POST /v1/register" {
		t.Fatalf("paths = %v", *got)
	}
	assertNoTeamFile(t, dir)
}

func TestTeamJoinReportsJoinedButNotSaved(t *testing.T) {
	srv, got := scriptedTeam(t, http.StatusOK, helloJSON(teamServerID, "Lab relay"), http.StatusCreated, registerJSON(teamToken, teamServerID, "Lab relay"))
	h, dir := testTeamSurface(t, clientFor(srv))
	for i := 1; i <= 16; i++ {
		if err := team.Add(dir, team.Entry{
			ID: fmt.Sprintf("%032x", i), Name: "Lab relay", URL: "http://other.example",
			SeatName: "Ada", Token: fmt.Sprintf("%064x", i), JoinedAt: "2026-11-12T09:30:00Z",
		}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(filepath.Join(dir, team.File))
	if err != nil {
		t.Fatal(err)
	}
	rec := doTeam(h, http.MethodPost, "/api/team/servers", joinPayload(srv.URL+"/join#"+teamCode, "Ada"))
	assertAPIError(t, rec, http.StatusInternalServerError, "joined_but_not_saved")
	assertNoSecrets(t, rec.Body.String(), teamCode, teamToken)
	if len(*got) != 2 || (*got)[0] != "GET /v1/hello" || (*got)[1] != "POST /v1/register" {
		t.Fatalf("paths = %v", *got)
	}
	after, err := os.ReadFile(filepath.Join(dir, team.File))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("unsaved join changed the file")
	}
}

func TestTeamUnknownRoutesAreJSONNotFound(t *testing.T) {
	h, _ := testTeamSurface(t, team.NewClient())
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/team"},
		{http.MethodPost, "/api/team"},
		{http.MethodPut, "/api/team/servers"},
		{http.MethodDelete, "/api/team/servers"},
		{http.MethodDelete, "/api/team/servers/"},
		{http.MethodDelete, "/api/team/servers/abc/def"},
		{http.MethodOptions, "/api/team/servers"},
		{http.MethodGet, "/api/team/nope"},
		{http.MethodGet, "/api/team/servers/" + teamServerID},
		{http.MethodPost, "/api/team/servers/" + teamServerID + "/status"},
		{http.MethodPut, "/api/team/servers/" + teamServerID + "/status"},
		{http.MethodDelete, "/api/team/servers/" + teamServerID + "/status"},
		{http.MethodGet, "/api/team/servers/" + teamServerID + "/leave"},
		{http.MethodPut, "/api/team/servers/" + teamServerID + "/leave"},
		{http.MethodDelete, "/api/team/servers/" + teamServerID + "/leave"},
		{http.MethodGet, "/api/team/servers/" + teamServerID + "/status/extra"},
		{http.MethodPost, "/api/team/servers/" + teamServerID + "/leave/extra"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := doTeam(h, tc.method, tc.path, "")
			assertAPIError(t, rec, http.StatusNotFound, "not_found")
		})
	}
}

func TestTeamListAndForgetOfADamagedFile(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	before := []byte("{")
	if err := os.WriteFile(filepath.Join(dir, team.File), before, 0o600); err != nil {
		t.Fatal(err)
	}
	listed := doTeam(h, http.MethodGet, "/api/team/servers", "")
	assertAPIError(t, listed, http.StatusInternalServerError, "store_unavailable")
	forgotten := doTeam(h, http.MethodDelete, "/api/team/servers/"+teamServerID, "")
	assertAPIError(t, forgotten, http.StatusInternalServerError, "store_unavailable")
	after, err := os.ReadFile(filepath.Join(dir, team.File))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("file = %q err %v", after, err)
	}
}

func TestTeamTokenStaysOutOfTheCommand(t *testing.T) {
	srv, _ := scriptedTeam(t, http.StatusOK, helloJSON(teamServerID, "Lab relay"), http.StatusCreated, registerJSON(teamToken, teamServerID, "Lab relay"))
	orig := newTeamClient
	t.Cleanup(func() { newTeamClient = orig })
	newTeamClient = func() *team.Client {
		c := orig()
		c.Transport = srv.Client().Transport
		return c
	}
	cmd, data, stdout, stderr := startTeamCommand(t, true, true)
	if cmd.application == nil || cmd.application.teamHandler == nil {
		t.Fatal("muxer did not install the team handler")
	}
	core, err := newCoreMux(cmd.application)
	if err != nil {
		t.Fatal(err)
	}
	rec := doTeam(core, http.MethodPost, "/api/team/servers", joinPayload(srv.URL+"/join#"+teamCode, "Ada"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	joined := stdout.String() + stderr.String() + rec.Body.String()
	assertNoSecrets(t, joined, teamCode, teamToken)
	if strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("response names a token: %s", rec.Body.String())
	}
	info, err := os.Stat(filepath.Join(data, team.File))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestTeamConcurrentJoins(t *testing.T) {
	srvA, _ := scriptedTeam(t, http.StatusOK, helloJSON(teamServerID, "Lab relay"), http.StatusCreated, registerJSON(teamToken, teamServerID, "Lab relay"))
	otherToken := strings.Repeat("cd", 32)
	srvB, _ := scriptedTeam(t, http.StatusOK, helloJSON(teamOtherID, "Other relay"), http.StatusCreated, registerJSON(otherToken, teamOtherID, "Other relay"))
	c := team.NewClient()
	c.Transport = teamRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case hostOf(srvA.URL):
			return srvA.Client().Transport.RoundTrip(r)
		case hostOf(srvB.URL):
			return srvB.Client().Transport.RoundTrip(r)
		default:
			return nil, errors.New("unknown host")
		}
	})
	h, dir := testTeamSurface(t, c)
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	join := func(link string) {
		defer wg.Done()
		rec := doTeam(h, http.MethodPost, "/api/team/servers", joinPayload(link, "Ada"))
		if rec.Code != http.StatusCreated {
			errCh <- fmt.Errorf("status %d body %s", rec.Code, rec.Body.String())
			return
		}
		if strings.Contains(rec.Body.String(), teamToken) || strings.Contains(rec.Body.String(), otherToken) {
			errCh <- fmt.Errorf("response contains a token: %s", rec.Body.String())
		}
	}
	wg.Add(2)
	go join(srvA.URL + "/join#" + teamCode)
	go join(srvB.URL + "/join#other-code")
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	stored, err := team.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, e := range stored {
		ids[e.ID] = true
	}
	if !ids[teamServerID] || !ids[teamOtherID] || len(stored) != 2 {
		t.Fatalf("stored = %+v", stored)
	}
}

func hostOf(raw string) string {
	return strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
}

func blockingTeamServer(path string) (*httptest.Server, <-chan struct{}, func()) {
	entered := make(chan struct{})
	var once sync.Once
	release := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == path {
			once.Do(func() { close(entered) })
			<-release
		}
		if r.URL.Path == "/v1/register" {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, registerJSON(teamToken, teamServerID, "Lab relay"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
	}))
	return srv, entered, letGo
}

func recvTeam(t *testing.T, ch <-chan *httptest.ResponseRecorder, what string) *httptest.ResponseRecorder {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case rec := <-ch:
		return rec
	case <-timer.C:
		t.Fatalf("%s did not finish", what)
		return nil
	}
}

func TestTeamListAndForgetFinishWhileTheServerIsBlocked(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"hello", "/v1/hello"},
		{"register", "/v1/register"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, entered, letGo := blockingTeamServer(tc.path)
			h, dir := testTeamSurface(t, clientFor(srv))
			t.Cleanup(srv.Close)
			t.Cleanup(letGo)
			otherToken := strings.Repeat("ab", 32)
			if err := team.Add(dir, team.Entry{
				ID: teamOtherID, Name: "Other relay", URL: "http://other.example",
				SeatName: "Ada", Token: otherToken, JoinedAt: "2026-11-12T09:30:00Z",
			}); err != nil {
				t.Fatal(err)
			}
			joinDone := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				joinDone <- doTeam(h, http.MethodPost, "/api/team/servers", joinPayload(srv.URL+"/join#"+teamCode, "Ada"))
			}()
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()
			select {
			case <-entered:
			case <-timer.C:
				t.Fatal("the join did not reach the team server")
			}

			listDone := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				listDone <- doTeam(h, http.MethodGet, "/api/team/servers", "")
			}()
			listed := recvTeam(t, listDone, "list")
			if listed.Code != http.StatusOK {
				t.Fatalf("list status = %d body %s", listed.Code, listed.Body.String())
			}
			if !strings.Contains(listed.Body.String(), teamOtherID) || strings.Contains(listed.Body.String(), teamServerID) {
				t.Fatalf("list = %s", listed.Body.String())
			}
			assertNoSecrets(t, listed.Body.String(), teamCode, teamToken)
			assertNoSecrets(t, listed.Body.String(), "", otherToken)

			forgetDone := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				forgetDone <- doTeam(h, http.MethodDelete, "/api/team/servers/"+teamOtherID, "")
			}()
			forgotten := recvTeam(t, forgetDone, "forget")
			if forgotten.Code != http.StatusNoContent || forgotten.Body.Len() != 0 {
				t.Fatalf("forget = %d %q", forgotten.Code, forgotten.Body.String())
			}
			mid, err := team.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(mid) != 0 {
				t.Fatalf("stored while the join is in flight = %+v", mid)
			}

			letGo()
			joined := recvTeam(t, joinDone, "join")
			if joined.Code != http.StatusCreated {
				t.Fatalf("join status = %d body %s", joined.Code, joined.Body.String())
			}
			assertNoSecrets(t, joined.Body.String(), teamCode, teamToken)
			stored, err := team.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(stored) != 1 || stored[0].ID != teamServerID || stored[0].Token != teamToken {
				t.Fatalf("stored = %+v", stored)
			}
		})
	}
}

func TestTeamConcurrentJoinsOfOneServer(t *testing.T) {
	const codeA = "alpha-join"
	const codeB = "beta-join"
	entered := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/register" {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				return
			}
			entered <- string(raw)
			<-release
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, registerJSON(teamToken, teamServerID, "Lab relay"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
	}))
	h, dir := testTeamSurface(t, clientFor(srv))
	t.Cleanup(srv.Close)
	t.Cleanup(letGo)

	done := make(chan *httptest.ResponseRecorder, 2)
	join := func(code string) {
		done <- doTeam(h, http.MethodPost, "/api/team/servers", joinPayload(srv.URL+"/join#"+code, "Ada"))
	}
	go join(codeA)
	go join(codeB)

	var bodies []string
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for len(bodies) < 2 {
		select {
		case body := <-entered:
			bodies = append(bodies, body)
		case <-timer.C:
			t.Fatalf("register calls in flight = %d, want 2", len(bodies))
		}
	}
	if !strings.Contains(bodies[0]+bodies[1], codeA) || !strings.Contains(bodies[0]+bodies[1], codeB) {
		t.Fatalf("register bodies = %q", bodies)
	}

	letGo()
	var recs []*httptest.ResponseRecorder
	doneTimer := time.NewTimer(3 * time.Second)
	defer doneTimer.Stop()
	for len(recs) < 2 {
		select {
		case rec := <-done:
			recs = append(recs, rec)
		case <-doneTimer.C:
			t.Fatalf("finished joins = %d, want 2", len(recs))
		}
	}
	var created, conflict int
	for _, rec := range recs {
		switch rec.Code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
			assertAPIError(t, rec, http.StatusConflict, "already_joined")
		default:
			t.Errorf("status = %d body %s", rec.Code, rec.Body.String())
		}
		assertNoSecrets(t, rec.Body.String(), codeA, teamToken)
		assertNoSecrets(t, rec.Body.String(), codeB, "")
	}
	if created != 1 || conflict != 1 {
		t.Fatalf("created = %d conflict = %d", created, conflict)
	}
	stored, err := team.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].ID != teamServerID || stored[0].Token != teamToken {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestTeamRoutesAndStorageAreDocumented(t *testing.T) {
	root := repoRootFromTest(t)
	api, err := os.ReadFile(filepath.Join(root, "docs", "http-api.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"SCIMUX_ENABLE_EXPERIMENTAL_FEATURE_TEAM",
		"GET /api/team/servers",
		"POST /api/team/servers",
		"DELETE /api/team/servers/{id}",
		"bad_request",
		"invalid_link",
		"identity_unavailable",
		"store_unavailable",
		"unreachable",
		"bad_server",
		"already_joined",
		"join_refused",
		"already_registered",
		"server_full",
		"joined_but_not_saved",
		"not_found",
		"never leaves the server process",
		"does not contact the team server",
		"stays registered there until the operator revokes it",
		"GET /api/team/servers/{id}/status",
		"POST /api/team/servers/{id}/leave",
		"left_but_not_removed",
		"when leaving is not possible",
	} {
		if !strings.Contains(string(api), needle) {
			t.Errorf("docs/http-api.md missing %q", needle)
		}
	}
	storage, err := os.ReadFile(filepath.Join(root, "docs", "invariants", "storage.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"team.json",
		"credentials",
		"Only the muxer reads and writes it",
		"explicit join, forget, or changing",
		"damaged file is never overwritten",
		"gate off it is neither read nor written",
	} {
		if !strings.Contains(string(storage), needle) {
			t.Errorf("docs/invariants/storage.md missing %q", needle)
		}
	}
}

func TestTeamRoutesShareTheSettingsBoundary(t *testing.T) {
	srv, _ := scriptedTeam(t, http.StatusOK, helloJSON(teamServerID, "Lab relay"), http.StatusCreated, registerJSON(teamToken, teamServerID, "Lab relay"))
	orig := newTeamClient
	t.Cleanup(func() { newTeamClient = orig })
	newTeamClient = func() *team.Client {
		c := orig()
		c.Transport = srv.Client().Transport
		return c
	}
	cmd, _, _, _ := startTeamCommand(t, true, true)
	if cmd.application == nil || cmd.application.teamHandler == nil {
		t.Fatal("muxer did not install the team handler")
	}
	core, err := newCoreMux(cmd.application)
	if err != nil {
		t.Fatal(err)
	}
	web, err := newWebBackend(webBackendConfig{Web: webFS, Core: core, RequestPolicy: cmd.application.requestPolicy})
	if err != nil {
		t.Fatal(err)
	}
	teamReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/team/servers", strings.NewReader(joinPayload(srv.URL+"/join#"+teamCode, "Ada")))
	teamReq.Host = "127.0.0.1"
	teamReq.Header.Set("Content-Type", "application/json")
	settingsReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/settings", strings.NewReader(`{"theme":"dark"}`))
	settingsReq.Host = "127.0.0.1"
	settingsReq.Header.Set("Content-Type", "application/json")
	teamRec := httptest.NewRecorder()
	settingsRec := httptest.NewRecorder()
	web.local.ServeHTTP(teamRec, teamReq)
	web.local.ServeHTTP(settingsRec, settingsReq)
	if dumpRecorder(teamRec) != dumpRecorder(settingsRec) {
		t.Fatalf("team without CSRF\n%s\nsettings without CSRF\n%s", dumpRecorder(teamRec), dumpRecorder(settingsRec))
	}

	okReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/team/servers", strings.NewReader(joinPayload(srv.URL+"/join#"+teamCode, "Ada")))
	okReq.Host = "127.0.0.1"
	okReq.Header.Set("Content-Type", "application/json")
	okReq.Header.Set("X-Scimux-CSRF", csrfToken)
	okRec := httptest.NewRecorder()
	web.local.ServeHTTP(okRec, okReq)
	if okRec.Code != http.StatusCreated {
		t.Fatalf("join behind the boundary = %d %s", okRec.Code, okRec.Body.String())
	}
	assertNoSecrets(t, okRec.Body.String(), teamCode, teamToken)
}

func TestTeamCoreMuxLeavesOtherRoutesAlone(t *testing.T) {
	h, _ := testTeamSurface(t, team.NewClient())
	cmd, _, _, _ := startTeamCommand(t, true, true)
	cmd.application.teamHandler = h
	core, err := newCoreMux(cmd.application)
	if err != nil {
		t.Fatal(err)
	}
	teamRec := doTeam(core, http.MethodGet, "/api/team", "")
	assertAPIError(t, teamRec, http.StatusNotFound, "not_found")
	other := doTeam(core, http.MethodGet, "/api/teams", "")
	if other.Code != http.StatusNotFound || strings.Contains(other.Body.String(), `"not_found"`) {
		t.Fatalf("/api/teams = %d %s", other.Code, other.Body.String())
	}
	plain := doTeam(core, http.MethodGet, "/api/not-a-route", "")
	if other.Body.String() != plain.Body.String() || other.Code != plain.Code {
		t.Fatalf("/api/teams and /api/not-a-route differ\n%s\n%s", other.Body.String(), plain.Body.String())
	}
}

func asTeamHandler(t *testing.T, h http.Handler) *teamHandler {
	t.Helper()
	th, ok := h.(*teamHandler)
	if !ok {
		t.Fatalf("handler = %T", h)
	}
	return th
}

func labEntry(id, rawURL, token string) team.Entry {
	return team.Entry{
		ID: id, Name: "Lab relay", URL: rawURL, SeatName: "Ada",
		Token: token, JoinedAt: "2026-11-12T09:30:00Z",
	}
}

func seedTeam(t *testing.T, dir string, e team.Entry) {
	t.Helper()
	if err := team.Add(dir, e); err != nil {
		t.Fatal(err)
	}
}

func teamDirSnapshot(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s dir=%t\n", e.Name(), e.IsDir())
		if e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
	}
	return b.String()
}

func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, state string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	var got struct {
		Status string `json:"status"`
	}
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("body %s: %v", rec.Body.String(), err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("trailing status document: %v", err)
	}
	if got.Status != state {
		t.Fatalf("state = %q, want %q", got.Status, state)
	}
}

type teamHit struct {
	method string
	path   string
	auth   string
	body   string
}

func scriptedClient(sc struct {
	helloStatus int
	helloBody   string
	helloErr    bool
	nextStatus  int
	nextBody    string
	nextErr     bool
}) (*team.Client, *[]teamHit) {
	var mu sync.Mutex
	var got []teamHit
	c := team.NewClient()
	c.Transport = teamRoundTrip(func(r *http.Request) (*http.Response, error) {
		var raw []byte
		if r.Body != nil {
			raw, _ = io.ReadAll(r.Body)
		}
		mu.Lock()
		got = append(got, teamHit{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: string(raw)})
		mu.Unlock()
		if r.URL.Path == "/v1/hello" {
			if sc.helloErr {
				return nil, errors.New("down")
			}
			return &http.Response{
				StatusCode: sc.helloStatus,
				Body:       io.NopCloser(strings.NewReader(sc.helloBody)),
				Header:     make(http.Header),
				Request:    r,
			}, nil
		}
		if sc.nextErr {
			return nil, errors.New("down")
		}
		return &http.Response{
			StatusCode: sc.nextStatus,
			Body:       io.NopCloser(strings.NewReader(sc.nextBody)),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})
	return c, &got
}

func assertNoTokenSent(t *testing.T, hits []teamHit, token string) {
	t.Helper()
	for _, hit := range hits {
		if strings.Contains(hit.auth, token) || strings.Contains(hit.body, token) {
			t.Fatalf("token left the seat: %+v", hits)
		}
	}
}

func TestTeamStatusBoundIsEightSeconds(t *testing.T) {
	h, _ := testTeamSurface(t, team.NewClient())
	if got := asTeamHandler(t, h).statusBound; got != 8*time.Second {
		t.Fatalf("bound = %s, want 8s", got)
	}
}

func TestTeamStatusReportsEachState(t *testing.T) {
	seats := `{"seats":[{"id":"` + teamOtherID + `","name":"Ada"}]}`
	cases := []struct {
		name        string
		helloStatus int
		helloBody   string
		checkStatus int
		checkBody   string
		want        string
		check       bool
	}{
		{"connected", http.StatusOK, helloJSON(teamServerID, "Lab relay"), http.StatusOK, seats, "connected", true},
		{"refused", http.StatusOK, helloJSON(teamServerID, "Other name"), http.StatusUnauthorized, `{"error":"unauthorized"}`, "refused", true},
		{"changed id", http.StatusOK, helloJSON(teamOtherID, "Lab relay"), http.StatusOK, seats, "changed", false},
		{"changed protocol", http.StatusOK, `{"protocol":0,"server":{"id":"` + teamServerID + `","name":"Lab relay"}}`, http.StatusOK, seats, "changed", false},
		{"changed check", http.StatusOK, helloJSON(teamServerID, "Lab relay"), http.StatusInternalServerError, `{"error":"internal"}`, "changed", true},
		{"changed seats", http.StatusOK, helloJSON(teamServerID, "Lab relay"), http.StatusOK, `{}`, "changed", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, got := scriptedClient(struct {
				helloStatus int
				helloBody   string
				helloErr    bool
				nextStatus  int
				nextBody    string
				nextErr     bool
			}{helloStatus: tc.helloStatus, helloBody: tc.helloBody, nextStatus: tc.checkStatus, nextBody: tc.checkBody})
			h, dir := testTeamSurface(t, client)
			decoyToken := strings.Repeat("cd", 32)
			seedTeam(t, dir, labEntry(teamOtherID, "http://other.example", decoyToken))
			seedTeam(t, dir, labEntry(teamServerID, "http://relay.example", teamToken))
			before := teamDirSnapshot(t, dir)
			rec := doTeam(h, http.MethodGet, "/api/team/servers/"+teamServerID+"/status", "")
			assertStatus(t, rec, tc.want)
			assertNoSecrets(t, rec.Body.String(), "", teamToken)
			assertNoSecrets(t, rec.Body.String(), "", decoyToken)
			if strings.Contains(rec.Body.String(), `"token"`) {
				t.Fatalf("response names a token: %s", rec.Body.String())
			}
			if teamDirSnapshot(t, dir) != before {
				t.Fatal("status wrote the data directory")
			}
			hits := *got
			if len(hits) == 0 || hits[0].method != http.MethodGet || hits[0].path != "/v1/hello" || hits[0].auth != "" || hits[0].body != "" {
				t.Fatalf("hello = %+v", hits)
			}
			if strings.Contains(hits[0].path, teamToken) {
				t.Fatalf("token in the hello path: %+v", hits[0])
			}
			if tc.check {
				if len(hits) != 2 || hits[1].method != http.MethodGet || hits[1].path != "/v1/seats" || hits[1].body != "" || hits[1].auth != "Bearer "+teamToken {
					t.Fatalf("check = %+v", hits)
				}
			} else {
				if len(hits) != 1 {
					t.Fatalf("check ran: %+v", hits)
				}
				assertNoTokenSent(t, hits, teamToken)
			}
		})
	}
}

func TestTeamStatusOfflineWhenUnreachable(t *testing.T) {
	for _, hello := range []bool{true, false} {
		name := "check"
		if hello {
			name = "hello"
		}
		t.Run(name, func(t *testing.T) {
			client, got := scriptedClient(struct {
				helloStatus int
				helloBody   string
				helloErr    bool
				nextStatus  int
				nextBody    string
				nextErr     bool
			}{
				helloStatus: http.StatusOK,
				helloBody:   helloJSON(teamServerID, "Lab relay"),
				helloErr:    hello,
				nextStatus:  http.StatusOK,
				nextBody:    `{"seats":[]}`,
				nextErr:     !hello,
			})
			h, dir := testTeamSurface(t, client)
			seedTeam(t, dir, labEntry(teamServerID, "http://relay.example", teamToken))
			before := teamDirSnapshot(t, dir)
			rec := doTeam(h, http.MethodGet, "/api/team/servers/"+teamServerID+"/status", "")
			assertStatus(t, rec, "offline")
			assertNoSecrets(t, rec.Body.String(), "", teamToken)
			if teamDirSnapshot(t, dir) != before {
				t.Fatal("status wrote the data directory")
			}
			hits := *got
			if hello {
				if len(hits) != 1 || hits[0].path != "/v1/hello" {
					t.Fatalf("hits = %+v", hits)
				}
				assertNoTokenSent(t, hits, teamToken)
			} else if len(hits) != 2 || hits[1].path != "/v1/seats" || hits[1].auth != "Bearer "+teamToken {
				t.Fatalf("hits = %+v", hits)
			}
		})
	}
}

func TestTeamStatusHonorsItsTimeBound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(time.Hour)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	asTeamHandler(t, h).statusBound = 40 * time.Millisecond
	seedTeam(t, dir, labEntry(teamServerID, srv.URL, teamToken))
	before := teamDirSnapshot(t, dir)
	start := time.Now()
	rec := doTeam(h, http.MethodGet, "/api/team/servers/"+teamServerID+"/status", "")
	if time.Since(start) > 2*time.Second {
		t.Fatal("status waited longer than its bound")
	}
	assertStatus(t, rec, "offline")
	assertNoSecrets(t, rec.Body.String(), "", teamToken)
	if teamDirSnapshot(t, dir) != before {
		t.Fatal("status wrote the data directory")
	}
}

func TestTeamStatusAndLeaveUnknownID(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	seedTeam(t, dir, labEntry(teamServerID, srv.URL, teamToken))
	missing := strings.Repeat("ab", 16)
	before := teamDirSnapshot(t, dir)
	status := doTeam(h, http.MethodGet, "/api/team/servers/"+missing+"/status", "")
	assertAPIError(t, status, http.StatusNotFound, "not_found")
	leave := doTeam(h, http.MethodPost, "/api/team/servers/"+missing+"/leave", "")
	assertAPIError(t, leave, http.StatusNotFound, "not_found")
	assertNoSecrets(t, status.Body.String()+leave.Body.String(), "", teamToken)
	if hits != 0 {
		t.Fatalf("unknown id contacted the server %d times", hits)
	}
	if teamDirSnapshot(t, dir) != before {
		t.Fatal("unknown id wrote the data directory")
	}
}

func TestTeamStatusAndLeaveStoreUnavailable(t *testing.T) {
	for _, dirShape := range []string{"damaged", "unreadable"} {
		t.Run(dirShape, func(t *testing.T) {
			h, dir := testTeamSurface(t, team.NewClient())
			path := filepath.Join(dir, team.File)
			if dirShape == "unreadable" {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := teamDirSnapshot(t, dir)
			status := doTeam(h, http.MethodGet, "/api/team/servers/"+teamServerID+"/status", "")
			assertAPIError(t, status, http.StatusInternalServerError, "store_unavailable")
			leave := doTeam(h, http.MethodPost, "/api/team/servers/"+teamServerID+"/leave", "")
			assertAPIError(t, leave, http.StatusInternalServerError, "store_unavailable")
			assertNoSecrets(t, status.Body.String()+leave.Body.String(), "", teamToken)
			if teamDirSnapshot(t, dir) != before {
				t.Fatal("a failed read changed the data directory")
			}
		})
	}
}

func TestTeamLeaveRemovesTheEntry(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client, got := scriptedClient(struct {
				helloStatus int
				helloBody   string
				helloErr    bool
				nextStatus  int
				nextBody    string
				nextErr     bool
			}{
				helloStatus: http.StatusOK,
				helloBody:   helloJSON(teamServerID, "Lab relay"),
				nextStatus:  status,
				nextBody:    teamToken,
			})
			h, dir := testTeamSurface(t, client)
			decoyToken := strings.Repeat("cd", 32)
			seedTeam(t, dir, labEntry(teamOtherID, "http://other.example", decoyToken))
			seedTeam(t, dir, labEntry(teamServerID, "http://relay.example", teamToken))
			rec := doTeam(h, http.MethodPost, "/api/team/servers/"+teamServerID+"/leave", "")
			if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Fatalf("leave = %d %q", rec.Code, rec.Body.String())
			}
			assertNoSecrets(t, rec.Body.String(), "", teamToken)
			left, err := team.Load(dir)
			if err != nil || len(left) != 1 || left[0].ID != teamOtherID || left[0].Token != decoyToken {
				t.Fatalf("stored = %+v err %v", left, err)
			}
			hits := *got
			if len(hits) != 2 || hits[0].path != "/v1/hello" || hits[0].method != http.MethodGet || hits[0].auth != "" || hits[0].body != "" {
				t.Fatalf("hello = %+v", hits)
			}
			if hits[1].path != "/v1/seat" || hits[1].method != http.MethodDelete || hits[1].body != "" || hits[1].auth != "Bearer "+teamToken {
				t.Fatalf("leave call = %+v", hits[1])
			}
		})
	}
}

func TestTeamLeaveKeepsTheEntryOnFailure(t *testing.T) {
	cases := []struct {
		name        string
		helloStatus int
		helloBody   string
		helloErr    bool
		nextStatus  int
		nextErr     bool
		want        string
		called      bool
	}{
		{"hello unreachable", http.StatusOK, helloJSON(teamServerID, "Lab relay"), true, http.StatusNoContent, false, "unreachable", false},
		{"leave unreachable", http.StatusOK, helloJSON(teamServerID, "Lab relay"), false, http.StatusNoContent, true, "unreachable", true},
		{"other server", http.StatusOK, helloJSON(teamOtherID, "Lab relay"), false, http.StatusNoContent, false, "bad_server", false},
		{"bad hello", http.StatusOK, `not-json`, false, http.StatusNoContent, false, "bad_server", false},
		{"leave not found", http.StatusOK, helloJSON(teamServerID, "Lab relay"), false, http.StatusNotFound, false, "bad_server", true},
		{"leave server error", http.StatusOK, helloJSON(teamServerID, "Lab relay"), false, http.StatusInternalServerError, false, "bad_server", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, got := scriptedClient(struct {
				helloStatus int
				helloBody   string
				helloErr    bool
				nextStatus  int
				nextBody    string
				nextErr     bool
			}{
				helloStatus: tc.helloStatus,
				helloBody:   tc.helloBody,
				helloErr:    tc.helloErr,
				nextStatus:  tc.nextStatus,
				nextBody:    teamToken,
				nextErr:     tc.nextErr,
			})
			h, dir := testTeamSurface(t, client)
			seedTeam(t, dir, labEntry(teamServerID, "http://relay.example", teamToken))
			before := teamDirSnapshot(t, dir)
			rec := doTeam(h, http.MethodPost, "/api/team/servers/"+teamServerID+"/leave", "")
			assertAPIError(t, rec, http.StatusBadGateway, tc.want)
			assertNoSecrets(t, rec.Body.String(), "", teamToken)
			if teamDirSnapshot(t, dir) != before {
				t.Fatal("a failed leave changed the entry")
			}
			hits := *got
			if tc.called {
				if len(hits) != 2 || hits[1].path != "/v1/seat" || hits[1].auth != "Bearer "+teamToken || hits[1].body != "" {
					t.Fatalf("leave call = %+v", hits)
				}
			} else if len(hits) != 1 || hits[0].path != "/v1/hello" {
				t.Fatalf("leave ran: %+v", hits)
			}
			if !tc.called {
				assertNoTokenSent(t, hits, teamToken)
			}
		})
	}
}

func TestTeamLeaveReportsAnEntryItCouldNotRemove(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	release := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/seat" {
			if r.Header.Get("Authorization") != "Bearer "+teamToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			once.Do(func() { close(entered) })
			<-release
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(letGo)
	h, dir := testTeamSurface(t, clientFor(srv))
	seedTeam(t, dir, labEntry(teamServerID, srv.URL, teamToken))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doTeam(h, http.MethodPost, "/api/team/servers/"+teamServerID+"/leave", "")
	}()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-entered:
	case <-timer.C:
		t.Fatal("leave did not reach the team server")
	}
	path := filepath.Join(dir, team.File)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	letGo()
	rec := recvTeam(t, done, "leave")
	assertAPIError(t, rec, http.StatusInternalServerError, "left_but_not_removed")
	assertNoSecrets(t, rec.Body.String(), "", teamToken)
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("entry path after the failed remove: dir=%v err=%v", info, err)
	}
}

func TestTeamLeaveSucceedsWhenForgetWinsTheRace(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	release := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/seat" {
			once.Do(func() { close(entered) })
			<-release
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(letGo)
	h, dir := testTeamSurface(t, clientFor(srv))
	seedTeam(t, dir, labEntry(teamServerID, srv.URL, teamToken))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doTeam(h, http.MethodPost, "/api/team/servers/"+teamServerID+"/leave", "")
	}()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-entered:
	case <-timer.C:
		t.Fatal("leave did not reach the team server")
	}
	forgotten := doTeam(h, http.MethodDelete, "/api/team/servers/"+teamServerID, "")
	if forgotten.Code != http.StatusNoContent {
		t.Fatalf("forget = %d %s", forgotten.Code, forgotten.Body.String())
	}
	mid, err := team.Load(dir)
	if err != nil || len(mid) != 0 {
		t.Fatalf("stored while leave is in flight = %+v err %v", mid, err)
	}
	letGo()
	rec := recvTeam(t, done, "leave")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("leave = %d %q", rec.Code, rec.Body.String())
	}
	assertNoSecrets(t, rec.Body.String(), "", teamToken)
	left, err := team.Load(dir)
	if err != nil || len(left) != 0 {
		t.Fatalf("stored = %+v err %v", left, err)
	}
}

func TestTeamStatusAndLeaveDoNotHoldTheFileLock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		method string
		route  string
	}{
		{"status hello", "/v1/hello", http.MethodGet, "/status"},
		{"status check", "/v1/seats", http.MethodGet, "/status"},
		{"leave hello", "/v1/hello", http.MethodPost, "/leave"},
		{"leave call", "/v1/seat", http.MethodPost, "/leave"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			var once sync.Once
			release := make(chan struct{})
			var releaseOnce sync.Once
			letGo := func() { releaseOnce.Do(func() { close(release) }) }
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.path {
					once.Do(func() { close(entered) })
					<-release
				}
				switch r.URL.Path {
				case "/v1/hello":
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
				case "/v1/seats":
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, `{"seats":[]}`)
				case "/v1/seat":
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)
			t.Cleanup(letGo)
			h, dir := testTeamSurface(t, clientFor(srv))
			otherToken := strings.Repeat("ab", 32)
			seedTeam(t, dir, labEntry(teamServerID, srv.URL, teamToken))
			seedTeam(t, dir, labEntry(teamOtherID, "http://other.example", otherToken))
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- doTeam(h, tc.method, "/api/team/servers/"+teamServerID+tc.route, "")
			}()
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()
			select {
			case <-entered:
			case <-timer.C:
				t.Fatal("the call did not reach the team server")
			}
			listed := doTeam(h, http.MethodGet, "/api/team/servers", "")
			if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), teamServerID) || !strings.Contains(listed.Body.String(), teamOtherID) {
				t.Fatalf("list = %d %s", listed.Code, listed.Body.String())
			}
			assertNoSecrets(t, listed.Body.String(), "", teamToken)
			assertNoSecrets(t, listed.Body.String(), "", otherToken)
			forgotten := doTeam(h, http.MethodDelete, "/api/team/servers/"+teamOtherID, "")
			if forgotten.Code != http.StatusNoContent {
				t.Fatalf("forget = %d %s", forgotten.Code, forgotten.Body.String())
			}
			letGo()
			rec := recvTeam(t, done, tc.name)
			if tc.route == "/status" {
				assertStatus(t, rec, "connected")
				assertNoSecrets(t, rec.Body.String(), "", teamToken)
				left, err := team.Load(dir)
				if err != nil || len(left) != 1 || left[0].ID != teamServerID || left[0].Token != teamToken {
					t.Fatalf("stored = %+v err %v", left, err)
				}
			} else if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Fatalf("leave = %d %q", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestTeamStatusAndLeaveShareTheSettingsBoundary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
		case "/v1/seats":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"seats":[]}`)
		case "/v1/seat":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	orig := newTeamClient
	t.Cleanup(func() { newTeamClient = orig })
	newTeamClient = func() *team.Client {
		c := orig()
		c.Transport = srv.Client().Transport
		return c
	}
	cmd, data, _, _ := startTeamCommand(t, true, true)
	if cmd.application == nil || cmd.application.teamHandler == nil {
		t.Fatal("muxer did not install the team handler")
	}
	seedTeam(t, data, labEntry(teamServerID, srv.URL, teamToken))
	core, err := newCoreMux(cmd.application)
	if err != nil {
		t.Fatal(err)
	}
	web, err := newWebBackend(webBackendConfig{Web: webFS, Core: core, RequestPolicy: cmd.application.requestPolicy})
	if err != nil {
		t.Fatal(err)
	}
	compare := func(method, teamPath, settingsPath string, header string) {
		t.Helper()
		teamReq := httptest.NewRequest(method, "http://127.0.0.1"+teamPath, nil)
		settingsReq := httptest.NewRequest(method, "http://127.0.0.1"+settingsPath, nil)
		for _, req := range []*http.Request{teamReq, settingsReq} {
			req.Host = "127.0.0.1"
			if header != "" {
				req.Header.Set("Sec-Fetch-Site", header)
			}
		}
		teamRec := httptest.NewRecorder()
		settingsRec := httptest.NewRecorder()
		web.local.ServeHTTP(teamRec, teamReq)
		web.local.ServeHTTP(settingsRec, settingsReq)
		if dumpRecorder(teamRec) != dumpRecorder(settingsRec) {
			t.Fatalf("%s %s\n%s\nsettings\n%s", method, teamPath, dumpRecorder(teamRec), dumpRecorder(settingsRec))
		}
	}
	compare(http.MethodPost, "/api/team/servers/"+teamServerID+"/leave", "/api/settings", "")
	compare(http.MethodGet, "/api/team/servers/"+teamServerID+"/status", "/api/settings", "cross-site")

	statusReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/team/servers/"+teamServerID+"/status", nil)
	statusReq.Host = "127.0.0.1"
	statusRec := httptest.NewRecorder()
	web.local.ServeHTTP(statusRec, statusReq)
	assertStatus(t, statusRec, "connected")
	assertNoSecrets(t, statusRec.Body.String(), "", teamToken)

	leaveReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/team/servers/"+teamServerID+"/leave", nil)
	leaveReq.Host = "127.0.0.1"
	leaveReq.Header.Set("X-Scimux-CSRF", csrfToken)
	leaveRec := httptest.NewRecorder()
	web.local.ServeHTTP(leaveRec, leaveReq)
	if leaveRec.Code != http.StatusNoContent || leaveRec.Body.Len() != 0 {
		t.Fatalf("leave behind the boundary = %d %q", leaveRec.Code, leaveRec.Body.String())
	}
	assertNoSecrets(t, leaveRec.Body.String(), "", teamToken)
}

func TestTeamRenameSeatRoute(t *testing.T) {
	cases := []struct {
		name       string
		helloBody  string
		helloErr   bool
		nextStatus int
		nextErr    bool
		status     int
		code       string
		hits       int
	}{
		{"success", helloJSON(teamServerID, "Lab relay"), false, 200, false, 200, "", 2},
		{"hello offline", "", true, 200, false, 502, "unreachable", 1},
		{"hello bad", `{}`, false, 200, false, 502, "bad_server", 1},
		{"other server", helloJSON(teamOtherID, "Lab relay"), false, 200, false, 502, "bad_server", 1},
		{"rename offline", helloJSON(teamServerID, "Lab relay"), false, 200, true, 502, "unreachable", 2},
		{"refused", helloJSON(teamServerID, "Lab relay"), false, 401, false, 502, "refused", 2},
		{"bad name", helloJSON(teamServerID, "Lab relay"), false, 400, false, 422, "name_refused", 2},
		{"rejected name", helloJSON(teamServerID, "Lab relay"), false, 422, false, 422, "name_refused", 2},
		{"bad server", helloJSON(teamServerID, "Lab relay"), false, 500, false, 502, "bad_server", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, hits := scriptedClient(struct {
				helloStatus int
				helloBody   string
				helloErr    bool
				nextStatus  int
				nextBody    string
				nextErr     bool
			}{
				helloStatus: 200, helloBody: tc.helloBody, helloErr: tc.helloErr, nextStatus: tc.nextStatus, nextErr: tc.nextErr,
			})
			h, dir := testTeamSurface(t, client)
			seedTeam(t, dir, labEntry(teamServerID, "http://relay.example", teamToken))
			rec := doTeam(h, http.MethodPut, "/api/team/servers/"+teamServerID+"/seat", `{"name":"Synthetic"}`)
			if tc.code != "" {
				assertAPIError(t, rec, tc.status, tc.code)
			} else if rec.Code != tc.status {
				t.Fatalf("status = %d", rec.Code)
			}
			if len(*hits) != tc.hits {
				t.Fatalf("request count = %d", len(*hits))
			}
			if tc.hits == 2 {
				if (*hits)[0].auth != "" || (*hits)[1].auth != "Bearer "+teamToken {
					t.Fatal("wrong request authentication")
				}
				if (*hits)[1].method != http.MethodPut || (*hits)[1].path != "/v1/seat" || (*hits)[1].body != `{"name":"Synthetic"}` {
					t.Fatal("wrong rename request")
				}
			}
			stored, err := team.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			want := "Ada"
			if tc.status == 200 {
				want = "Synthetic"
			}
			if len(stored) != 1 || stored[0].SeatName != want {
				t.Fatal("wrong stored seat name")
			}
			if tc.status == 200 {
				var got teamServerView
				if json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.ID != teamServerID || got.SeatName != want {
					t.Fatal("wrong server view")
				}
				assertNoSecrets(t, rec.Body.String(), "", teamToken)
			}
		})
	}
}

func TestTeamRenameSeatRejectsInvalidInput(t *testing.T) {
	client, hits := scriptedClient(struct {
		helloStatus int
		helloBody   string
		helloErr    bool
		nextStatus  int
		nextBody    string
		nextErr     bool
	}{})
	h, dir := testTeamSurface(t, client)
	seedTeam(t, dir, labEntry(teamServerID, "http://relay.example", teamToken))
	path := "/api/team/servers/" + teamServerID + "/seat"
	for _, body := range []string{"", `{}`, `{"name":""}`, `{"name":" X"}`, `{"name":"X "}`, `{"name":"X\n"}`, `{"name":1}`, `{"name":"X","extra":true}`, `{"name":"X"}{}`, `{"name":"` + strings.Repeat("x", 65) + `"}`, `{"name":"` + strings.Repeat("x", 1024) + `"}`} {
		rec := doTeam(h, http.MethodPut, path, body)
		assertAPIError(t, rec, http.StatusBadRequest, "bad_request")
	}
	assertAPIError(t, doTeam(h, http.MethodPut, "/api/team/servers/"+teamOtherID+"/seat", `{"name":"Synthetic"}`), http.StatusNotFound, "not_found")
	if len(*hits) != 0 {
		t.Fatal("invalid input reached the server")
	}
}

func TestTeamRenameSeatConcurrentRemovalAndDamage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage bool
		status int
		code   string
	}{
		{"removed", false, http.StatusNotFound, "not_found"},
		{"damaged", true, http.StatusInternalServerError, "store_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := team.NewClient()
			h, dir := testTeamSurface(t, client)
			seedTeam(t, dir, labEntry(teamServerID, "http://relay.example", teamToken))
			calls := 0
			client.Transport = teamRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path == "/v1/hello" {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(helloJSON(teamServerID, "Lab relay"))), Header: make(http.Header), Request: r}, nil
				}
				if tc.damage {
					if err := os.WriteFile(filepath.Join(dir, team.File), []byte("broken"), 0600); err != nil {
						t.Fatal(err)
					}
				} else if err := team.Remove(dir, teamServerID); err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header), Request: r}, nil
			})
			rec := doTeam(h, http.MethodPut, "/api/team/servers/"+teamServerID+"/seat", `{"name":"Synthetic"}`)
			assertAPIError(t, rec, tc.status, tc.code)
			if calls != 2 {
				t.Fatalf("request count = %d", calls)
			}
			raw, err := os.ReadFile(filepath.Join(dir, team.File))
			if err != nil {
				t.Fatal(err)
			}
			if tc.damage && string(raw) != "broken" {
				t.Fatal("damaged state was overwritten")
			}
			if !tc.damage {
				entries, err := team.Load(dir)
				if err != nil || len(entries) != 0 {
					t.Fatal("removed server was restored")
				}
			}
		})
	}
}

func TestTeamRenameStorageInvariantDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "docs", "invariants", "storage.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "changing this seat's own name on a server") {
		t.Fatal("rename missing from team storage invariant")
	}
}

func TestTeamRenameSeatSaveTooLargeKeepsOriginalFile(t *testing.T) {
	client, hits := scriptedClient(struct {
		helloStatus int
		helloBody   string
		helloErr    bool
		nextStatus  int
		nextBody    string
		nextErr     bool
	}{helloStatus: 200, helloBody: helloJSON(teamServerID, "Lab relay"), nextStatus: 200})
	h, dir := testTeamSurface(t, client)
	entry := labEntry(teamServerID, "http://x", teamToken)
	entry.SeatName = "A"
	shape := struct {
		V       int          `json:"v"`
		Servers []team.Entry `json:"servers"`
	}{V: 1, Servers: []team.Entry{entry}}
	encoded, err := json.Marshal(shape)
	if err != nil {
		t.Fatal(err)
	}
	entry.URL = "http://" + strings.Repeat("x", 65520-len(encoded))
	seedTeam(t, dir, entry)
	before, err := os.ReadFile(filepath.Join(dir, team.File))
	if err != nil {
		t.Fatal(err)
	}
	rec := doTeam(h, http.MethodPut, "/api/team/servers/"+teamServerID+"/seat", `{"name":"`+strings.Repeat("n", 64)+`"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want store failure", rec.Code)
	}
	assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
	if len(*hits) != 2 {
		t.Fatalf("request count = %d", len(*hits))
	}
	after, err := os.ReadFile(filepath.Join(dir, team.File))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed save changed the old file")
	}
}

func TestTeamRenameRouteDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "docs", "http-api.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"PUT /api/team/servers/{id}/seat", "name_refused", "next successful change"} {
		if !strings.Contains(string(raw), needle) {
			t.Errorf("rename documentation missing %q", needle)
		}
	}
}

func TestTeamRenameSeatAtomicIOFailures(t *testing.T) {
	for _, which := range []string{"open", "write", "rename"} {
		t.Run(which, func(t *testing.T) {
			h, dir := testTeamSurface(t, team.NewClient())
			entry := labEntry(teamServerID, "http://relay.example", teamToken)
			seedTeam(t, dir, entry)
			before, err := os.ReadFile(filepath.Join(dir, team.File))
			if err != nil {
				t.Fatal(err)
			}
			th := asTeamHandler(t, h)
			seam, ok := any(th).(interface {
				setSeatIO(func(string) (*os.File, error), func(string, string) error)
			})
			if !ok {
				t.Fatal("seat file error seam is missing")
			}
			seam.setSeatIO(func(dir string) (*os.File, error) {
				if which == "open" {
					return nil, errors.New("synthetic open failure")
				}
				f, err := os.CreateTemp(dir, ".team-test-*.tmp")
				if err == nil && which == "write" {
					_ = f.Close()
				}
				return f, err
			}, func(from, to string) error {
				if which == "rename" {
					return errors.New("synthetic rename failure")
				}
				return os.Rename(from, to)
			})
			if _, err := th.saveSeatName(entry, "Synthetic"); err == nil {
				t.Fatal("save unexpectedly succeeded")
			}
			after, err := os.ReadFile(filepath.Join(dir, team.File))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed save changed the old file")
			}
			files, err := filepath.Glob(filepath.Join(dir, ".team-test-*.tmp"))
			if err != nil || len(files) != 0 {
				t.Fatal("temporary file remained")
			}
		})
	}
}

func TestTeamRenameSeatRejectsChangedServerIdentity(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	other := labEntry(teamOtherID, "http://other.example", strings.Repeat("ab", 32))
	seedTeam(t, dir, other)
	entry := labEntry(teamServerID, "http://relay.example", teamToken)
	seedTeam(t, dir, entry)
	th := asTeamHandler(t, h)
	for _, old := range []team.Entry{other, entry} {
		changed := old
		changed.Token = strings.Repeat("cd", 32)
		if _, err := th.saveSeatName(changed, "Synthetic"); !errors.Is(err, team.ErrNotFound) {
			t.Fatal("changed token was accepted")
		}
		changed = old
		changed.URL = "http://changed.example"
		if _, err := th.saveSeatName(changed, "Synthetic"); !errors.Is(err, team.ErrNotFound) {
			t.Fatal("changed address was accepted")
		}
	}
}
