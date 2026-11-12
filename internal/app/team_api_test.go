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
		for _, path := range []string{"/api/team", "/api/team/", "/api/team/servers"} {
			got := recordedText(h, method, path)
			if got != unknown {
				t.Fatalf("%s %s\n%s\nwant the unknown-route response\n%s", method, path, got, unknown)
			}
		}
	}
}

func assertNoTeamFile(t *testing.T, dir string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, team.File))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("team file stat = %v", err)
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
		"only the muxer reads and writes it",
		"explicit join or forget",
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
