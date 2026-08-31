package remote

// S5 R1 helpers. Test-only: fake rendezvous, invite-spec parser, TTY/sys/clock
// seams. Not product code. Unknown fake protocol requests fail loudly.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
)

// Vector invite from docs/protocol testdata/vectors/admission.json
// (enroll-request-ok). 15 bytes entropy 010203…0f, grouped Crockford.
const (
	vectorInviteGrouped = "0410-6105-0R3G-G28A-1C60-T3GF"
	vectorInviteCanon   = "041061050R3GG28A1C60T3GF"
	crockfordAlphabet   = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	inviteCodeLength    = 24
	inviteGroupSize     = 4
	inviteEntropyBytes  = 15
)

func classOf(err error) Class {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return ""
}

func guidanceOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Guidance
	}
	return ""
}

func requireClass(t *testing.T, err error, want Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want class %s", want)
	}
	if errors.Is(err, ErrUnimplemented) {
		t.Fatalf("got ErrUnimplemented, want class %s", want)
	}
	if got := classOf(err); got != want {
		t.Fatalf("error class = %q (%v), want %s", got, err, want)
	}
}

func requireNoInviteLeak(t *testing.T, invite string, texts ...string) {
	t.Helper()
	if invite == "" {
		return
	}
	forms := []string{invite, vectorInviteCanon, vectorInviteGrouped}
	norm, err := specParseInvite(invite)
	if err == nil {
		forms = append(forms, norm, specFormatInvite(norm))
	}
	for _, text := range texts {
		if text == "" {
			continue
		}
		for _, form := range forms {
			if form != "" && strings.Contains(text, form) {
				t.Fatalf("invite plaintext leaked into output %q", text)
			}
		}
	}
}

func specParseInvite(s string) (string, error) {
	var b strings.Builder
	b.Grow(inviteCodeLength)
	for _, r := range s {
		switch r {
		case '-', ' ':
			continue
		case 'o', 'O':
			r = '0'
		case 'i', 'I', 'l', 'L':
			r = '1'
		default:
			r = unicode.ToUpper(r)
		}
		if strings.IndexRune(crockfordAlphabet, r) < 0 {
			return "", errors.New("invalid invite code")
		}
		b.WriteRune(r)
	}
	if b.Len() != inviteCodeLength {
		return "", errors.New("invalid invite code")
	}
	return b.String(), nil
}

func specFormatInvite(normalized string) string {
	if len(normalized) != inviteCodeLength {
		return normalized
	}
	var b strings.Builder
	for i := 0; i < len(normalized); i += inviteGroupSize {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(normalized[i : i+inviteGroupSize])
	}
	return b.String()
}

func specEncodeInvite(src []byte) string {
	var acc uint64
	var bits uint
	out := make([]byte, 0, inviteCodeLength)
	for _, b := range src {
		acc = (acc << 8) | uint64(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, crockfordAlphabet[(acc>>bits)&31])
		}
	}
	if bits > 0 {
		out = append(out, crockfordAlphabet[(acc<<(5-bits))&31])
	}
	return string(out)
}

func otherInvite() string {
	raw := bytes.Repeat([]byte{0xab}, inviteEntropyBytes)
	return specFormatInvite(specEncodeInvite(raw))
}

// helloOK is rendezvous-v1 §4.0 as every rendezvous double must answer
// it. The pre-flight probe is unauthenticated and runs before enrollment,
// so a double that leaves it to its default 404 makes an otherwise
// healthy rendezvous look unreachable and fails tests about something
// else entirely.
func helloOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{\"min\":1,\"v\":1}\n"))
}

func writeInviteFile(t *testing.T, dir, invite string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, "invite")
	if err := os.WriteFile(path, []byte(invite+"\n"), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func scanRootForInvite(t *testing.T, root, invite string) {
	t.Helper()
	forms := []string{invite}
	if n, err := specParseInvite(invite); err == nil {
		forms = append(forms, n, specFormatInvite(n))
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, form := range forms {
			if form != "" && bytes.Contains(data, []byte(form)) {
				return fmt.Errorf("invite plaintext %q present in %s", form, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func scanRootForPrivateKey(t *testing.T, root string, priv ed25519.PrivateKey) {
	t.Helper()
	if len(priv) == 0 {
		return
	}
	hexKey := hex.EncodeToString(priv)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// The identity file is allowed to hold the private key; it must not
		// leave the client via any other file (temps of invite, logs, etc.).
		rel, _ := filepath.Rel(root, path)
		if rel == filepath.Join(PrivateDirName, StateFileName) {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(data, priv) || bytes.Contains(data, []byte(hexKey)) {
			return fmt.Errorf("private key present in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeState(t *testing.T, c *Client, st PersistedState) {
	t.Helper()
	if err := os.MkdirAll(c.PrivateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.StatePath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readStateFile(t *testing.T, path string) (PersistedState, []byte, bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return PersistedState{}, nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var st PersistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("state file is not valid JSON (%q): %v", raw, err)
	}
	return st, raw, true
}

func assertCompleteValidState(t *testing.T, st PersistedState) {
	t.Helper()
	switch st.Status {
	case StateEnrolled:
		if st.Handle == "" || st.PublicKey == "" || st.PrivateKey == "" {
			t.Fatalf("incomplete enrolled state: %+v", st)
		}
		if !strings.HasPrefix(st.Handle, "ih_") {
			t.Fatalf("handle %q is not ih_+crockford", st.Handle)
		}
	case StateAmbiguous, StatePartial:
		if st.PublicKey == "" || st.PrivateKey == "" {
			t.Fatalf("incomplete %s state: %+v", st.Status, st)
		}
	case StateDisabled, StateRevoked:
		// identity may remain; status must be explicit
		if st.Status == "" {
			t.Fatal("missing status")
		}
	case StateAbsent:
		t.Fatal("absent is not a persisted complete state")
	default:
		t.Fatalf("unknown status %q", st.Status)
	}
}

func newEd25519(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func enrolledFixture(t *testing.T, handle string) PersistedState {
	t.Helper()
	pub, priv := newEd25519(t)
	return PersistedState{
		V:          1,
		Status:     StateEnrolled,
		Handle:     handle,
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		Origin:     DefaultOrigin,
	}
}

func modePerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func clientCfg(t *testing.T, fake *fakeRV) Config {
	t.Helper()
	data := t.TempDir()
	cfg := Config{
		DataDir:       data,
		Origin:        DefaultOrigin,
		Remote:        true,
		RendezvousURL: fake.URL(),
		HTTPClient:    fake.Client(),
		Stdout:        io.Discard,
		Stderr:        new(bytes.Buffer),
	}
	if fake != nil {
		cfg.RendezvousURL = fake.URL()
		cfg.HTTPClient = fake.Client()
	}
	return cfg
}

func startClient(ctx context.Context, cfg Config) (*Client, error) {
	c := NewClient(cfg)
	return c, c.Start(ctx)
}

// ---------- fake rendezvous (S1 surfaces only) ----------

type recordedReq struct {
	Method string
	Path   string
	Body   []byte
	Header http.Header
}

type fakeInstall struct {
	handle  string
	pubkey  string
	revoked bool
}

type fakeInvite struct {
	normalized string
	redeemed   bool
	pubkey     string
}

type fakeRV struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	reqs     []recordedReq
	invites  map[string]*fakeInvite
	installs map[string]*fakeInstall
	handles  int

	dropEnrollResponse atomic.Bool
	hostile            atomic.Bool
	unexpected         []string
	lastHandle         string
	released           []string

	// The version window /v1/hello reports. Defaults to this build's, so
	// every existing test keeps a compatible rendezvous without saying so.
	helloV   atomic.Int64
	helloMin atomic.Int64
	// helloUnsupported makes the fake a rendezvous that predates §4.0:
	// reachable, speaking protocol, answering the probe with its own §7
	// constant rejection.
	helloUnsupported atomic.Bool
}

func newFakeRV(t *testing.T) *fakeRV {
	t.Helper()
	f := &fakeRV{
		t:        t,
		invites:  map[string]*fakeInvite{},
		installs: map[string]*fakeInstall{},
	}
	f.helloV.Store(ProtocolVersion)
	f.helloMin.Store(MinRequestV)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/hello", f.handleHello)
	mux.HandleFunc("/v1/enroll", f.handleEnroll)
	mux.HandleFunc("/v1/challenge", f.handleChallenge)
	mux.HandleFunc("/v1/verify", f.handleVerify)
	mux.HandleFunc("/v1/wait", f.handleWait)
	mux.HandleFunc("/v1/unenroll", f.handleUnenroll)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello", "/v1/enroll", "/v1/challenge", "/v1/verify", "/v1/wait", "/v1/unenroll":
			mux.ServeHTTP(w, r)
		default:
			f.mu.Lock()
			f.unexpected = append(f.unexpected, r.Method+" "+r.URL.Path)
			f.mu.Unlock()
			t.Errorf("fake rendezvous: unknown protocol request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unknown protocol request", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(func() {
		f.srv.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.unexpected) > 0 {
			t.Errorf("fake rendezvous saw unexpected requests: %v", f.unexpected)
		}
	})
	f.Issue(vectorInviteGrouped)
	return f
}

func (f *fakeRV) URL() string {
	if f == nil || f.srv == nil {
		return "http://127.0.0.1:1"
	}
	return f.srv.URL
}

func (f *fakeRV) Client() *http.Client {
	if f == nil || f.srv == nil {
		return unreachableClient()
	}
	return f.srv.Client()
}

func (f *fakeRV) Close() {
	if f != nil && f.srv != nil {
		f.srv.Close()
	}
}

func (f *fakeRV) Issue(code string) {
	norm, err := specParseInvite(code)
	if err != nil {
		f.t.Fatalf("fake Issue: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invites[norm] = &fakeInvite{normalized: norm}
}

func (f *fakeRV) DropEnrollResponse(v bool) { f.dropEnrollResponse.Store(v) }
func (f *fakeRV) SetHostile(v bool)         { f.hostile.Store(v) }

func (f *fakeRV) Requests() []recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedReq, len(f.reqs))
	copy(out, f.reqs)
	return out
}

func (f *fakeRV) RequestCount() int { return len(f.Requests()) }

func (f *fakeRV) Paths() []string {
	var out []string
	for _, r := range f.Requests() {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

func (f *fakeRV) EnrollCount() int {
	n := 0
	for _, r := range f.Requests() {
		if r.Path == "/v1/enroll" {
			n++
		}
	}
	return n
}

func (f *fakeRV) BoundKey(code string) string {
	norm, err := specParseInvite(code)
	if err != nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	inv := f.invites[norm]
	if inv == nil {
		return ""
	}
	return inv.pubkey
}

func (f *fakeRV) Redeemed(code string) bool {
	norm, _ := specParseInvite(code)
	f.mu.Lock()
	defer f.mu.Unlock()
	inv := f.invites[norm]
	return inv != nil && inv.redeemed
}

func (f *fakeRV) RevokeHandle(handle string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if inst := f.installs[handle]; inst != nil {
		inst.revoked = true
	}
}

func (f *fakeRV) LastHandle() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastHandle
}

// CooperateWait posts POST /v1/wait. A hostile hub accepts even a revoked
// or unknown handle; a non-hostile hub constant-rejects those.
func (f *fakeRV) CooperateWait(handle string) *http.Response {
	f.t.Helper()
	body := fmt.Sprintf(`{"v":1,"handle":%q,"challenge":"%s","sig":"%s","id":"%s","max_ms":1}`,
		handle, strings.Repeat("2", 64), strings.Repeat("ab", 64), strings.Repeat("ab", 32))
	req, err := http.NewRequest(http.MethodPost, f.URL()+"/v1/wait", strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	resp, err := f.Client().Do(req)
	if err != nil {
		f.t.Fatalf("CooperateWait: %v", err)
	}
	return resp
}

func (f *fakeRV) record(r *http.Request, body []byte) {
	h := r.Header.Clone()
	f.mu.Lock()
	f.reqs = append(f.reqs, recordedReq{Method: r.Method, Path: r.URL.Path, Body: append([]byte(nil), body...), Header: h})
	f.mu.Unlock()
}

// handleHello is rendezvous-v1 §4.0: unauthenticated, stateless, and the
// only route a client may call before it holds an invite.
func (f *fakeRV) handleHello(w http.ResponseWriter, r *http.Request) {
	f.record(r, nil)
	if r.Method != http.MethodGet || f.helloUnsupported.Load() {
		constantReject(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"v":   f.helloV.Load(),
		"min": f.helloMin.Load(),
	})
}

// SetHelloWindow makes the fake report a version window other than this
// build's, which is the only way to reach the mismatch branch without a
// second rendezvous implementation.
func (f *fakeRV) SetHelloWindow(v, min int) {
	f.helloV.Store(int64(v))
	f.helloMin.Store(int64(min))
}

func (f *fakeRV) SetHelloUnsupported(v bool) { f.helloUnsupported.Store(v) }

func (f *fakeRV) handleEnroll(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.record(r, body)
	if r.Method != http.MethodPost {
		constantReject(w)
		return
	}
	var req struct {
		V      json.Number `json:"v"`
		Code   string      `json:"code"`
		PubKey string      `json:"pubkey"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		constantReject(w)
		return
	}
	v, err := req.V.Int64()
	if err != nil || v < MinRequestV {
		constantReject(w)
		return
	}
	norm, err := specParseInvite(req.Code)
	if err != nil {
		constantReject(w)
		return
	}
	if len(req.PubKey) != 64 {
		constantReject(w)
		return
	}
	f.mu.Lock()
	inv := f.invites[norm]
	if inv == nil || inv.redeemed {
		f.mu.Unlock()
		constantReject(w)
		return
	}
	inv.redeemed = true
	inv.pubkey = req.PubKey
	f.handles++
	handle := fakeHandle(f.handles)
	f.installs[handle] = &fakeInstall{handle: handle, pubkey: req.PubKey}
	f.lastHandle = handle
	drop := f.dropEnrollResponse.Load()
	f.mu.Unlock()
	if drop {
		hj, ok := w.(http.Hijacker)
		if !ok {
			f.t.Fatal("fake RV: ResponseWriter is not a Hijacker; cannot drop enroll response")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			f.t.Fatalf("hijack: %v", err)
		}
		_ = conn.Close()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "{\"handle\":%q,\"v\":1}\n", handle)
}

func (f *fakeRV) handleChallenge(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.record(r, body)
	var req struct {
		V      json.Number `json:"v"`
		Handle string      `json:"handle"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		constantReject(w)
		return
	}
	f.mu.Lock()
	inst := f.installs[req.Handle]
	hostile := f.hostile.Load()
	f.mu.Unlock()
	if !hostile && (inst == nil || inst.revoked) {
		constantReject(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "{\"challenge\":\"2222222222222222222222222222222222222222222222222222222222222222\",\"v\":1}\n")
}

func (f *fakeRV) handleVerify(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.record(r, body)
	w.WriteHeader(http.StatusNoContent)
}

// handleUnenroll is rendezvous-v1 §4.4, and unlike handleVerify it really
// checks the signature. The route segment of the signed message is the
// entire security of this endpoint — it is what stops a captured verify
// signature deleting the installation that made it — so the fake is the
// place that proves the client puts the right one in.
func (f *fakeRV) handleUnenroll(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.record(r, body)
	var req struct {
		V         json.Number `json:"v"`
		Handle    string      `json:"handle"`
		Challenge string      `json:"challenge"`
		Sig       string      `json:"sig"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		constantReject(w)
		return
	}
	f.mu.Lock()
	inst := f.installs[req.Handle]
	f.mu.Unlock()
	if inst == nil || inst.revoked {
		constantReject(w)
		return
	}
	chal, cerr := hex.DecodeString(req.Challenge)
	sig, serr := hex.DecodeString(req.Sig)
	pub, perr := hex.DecodeString(inst.pubkey)
	if cerr != nil || serr != nil || perr != nil || len(pub) != ed25519.PublicKeySize {
		constantReject(w)
		return
	}
	if !ed25519.Verify(pub, buildAuthMessage(DefaultOrigin, ProtocolVersion, "/v1/unenroll", req.Handle, chal), sig) {
		constantReject(w)
		return
	}
	// Released, not revoked: the handle is gone, and §4.4 is explicit that
	// the code it was redeemed from stays redeemed.
	f.mu.Lock()
	delete(f.installs, req.Handle)
	f.released = append(f.released, req.Handle)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// Released is the handles §4.4 actually let go of, in order.
func (f *fakeRV) Released() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.released...)
}

func (f *fakeRV) handleWait(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.record(r, body)
	var req struct {
		Handle string `json:"handle"`
	}
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	inst := f.installs[req.Handle]
	hostile := f.hostile.Load()
	f.mu.Unlock()
	if !hostile && (inst == nil || inst.revoked) {
		constantReject(w)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Rv-Challenge", strings.Repeat("5", 64))
	w.WriteHeader(http.StatusNoContent)
}

func constantReject(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	io.WriteString(w, "not found\n")
}

func fakeHandle(n int) string {
	raw := sha256.Sum256([]byte(fmt.Sprintf("handle-%d", n)))
	var acc uint64
	var bits uint
	out := make([]byte, 0, 16)
	for i := 0; len(out) < 16 && i < len(raw); i++ {
		acc = (acc << 8) | uint64(raw[i])
		bits += 8
		for bits >= 5 && len(out) < 16 {
			bits -= 5
			out = append(out, crockfordAlphabet[(acc>>bits)&31])
		}
	}
	return "ih_" + string(out)
}

func unreachableClient() *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("rendezvous unreachable")
		}),
		Timeout: 50 * time.Millisecond,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func bodiesContain(reqs []recordedReq, needle []byte) bool {
	for _, r := range reqs {
		if bytes.Contains(r.Body, needle) {
			return true
		}
	}
	return false
}

func enrollBodies(reqs []recordedReq) []map[string]any {
	var out []map[string]any
	for _, r := range reqs {
		if r.Path != "/v1/enroll" {
			continue
		}
		var m map[string]any
		if json.Unmarshal(r.Body, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// ---------- TTY / Sys / Clock / RNG ----------

type recordingTTY struct {
	mu              sync.Mutex
	lines           []string
	i               int
	echo            bool
	disableCount    int
	restoreCount    int
	disabledAtRead  bool
	readWhileEchoOn bool
	prompts         []string
	readErr         error
	disableErr      error
	restoreErr      error
}

func newTTY(lines ...string) *recordingTTY {
	return &recordingTTY{echo: true, lines: lines}
}

func (t *recordingTTY) DisableEcho() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.disableCount++
	if t.disableErr != nil {
		return t.disableErr
	}
	t.echo = false
	return nil
}

func (t *recordingTTY) RestoreEcho() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.restoreCount++
	if t.restoreErr != nil {
		return t.restoreErr
	}
	t.echo = true
	return nil
}

func (t *recordingTTY) EchoEnabled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.echo
}

func (t *recordingTTY) ReadLine() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.echo {
		t.readWhileEchoOn = true
	} else {
		t.disabledAtRead = true
	}
	if t.readErr != nil {
		return "", t.readErr
	}
	if t.i >= len(t.lines) {
		return "", io.EOF
	}
	s := t.lines[t.i]
	t.i++
	return s, nil
}

func (t *recordingTTY) WritePrompt(p []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prompts = append(t.prompts, string(p))
	return nil
}

func (t *recordingTTY) snapshot() (disable, restore int, disabledAtRead, readWhileEchoOn bool, prompts []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.disableCount, t.restoreCount, t.disabledAtRead, t.readWhileEchoOn, append([]string(nil), t.prompts...)
}

type fakeSys struct {
	euid  int
	stats map[string]FileStat
	files map[string][]byte
}

func newFakeSys(euid int) *fakeSys {
	return &fakeSys{euid: euid, stats: map[string]FileStat{}, files: map[string][]byte{}}
}

func (s *fakeSys) EffectiveUID() int { return s.euid }

func (s *fakeSys) Lstat(path string) (FileStat, error) {
	st, ok := s.stats[path]
	if !ok {
		return FileStat{}, os.ErrNotExist
	}
	return st, nil
}

func (s *fakeSys) ReadFile(path string) ([]byte, error) {
	b, ok := s.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), b...), nil
}

func (s *fakeSys) add(path, body string, mode os.FileMode, uid int, regular bool) {
	s.stats[path] = FileStat{Mode: mode, UID: uid, Regular: regular}
	s.files[path] = []byte(body)
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type constRNG struct{ v float64 }

func (r constRNG) Float64() float64 { return r.v }

type seqRNG struct {
	mu sync.Mutex
	s  uint64
}

func newSeqRNG(seed uint64) *seqRNG { return &seqRNG{s: seed} }

func (r *seqRNG) Float64() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.s = r.s*1664525 + 1013904223
	return float64(r.s%1000000) / 1000000.0
}

type countHooks struct {
	mu             sync.Mutex
	remoteInit     int
	stateLoad      int
	identities     int
	enroll         int
	handles        int
	lastHandle     string
	hookDispatch   int
	rendezvousStop int
	agentTouch     int
	lastPub        ed25519.PublicKey
	hookNames      []string
	order          []string
}

func (h *countHooks) hooks() Hooks {
	return Hooks{
		OnRemoteInit: func() {
			h.mu.Lock()
			h.remoteInit++
			h.order = append(h.order, "remote-init")
			h.mu.Unlock()
		},
		OnStateLoad: func() {
			h.mu.Lock()
			h.stateLoad++
			h.order = append(h.order, "state-load")
			h.mu.Unlock()
		},
		OnIdentityGenerated: func(pub ed25519.PublicKey) {
			h.mu.Lock()
			h.identities++
			h.lastPub = append(ed25519.PublicKey(nil), pub...)
			h.order = append(h.order, "identity")
			h.mu.Unlock()
		},
		OnEnrollAttempt: func() {
			h.mu.Lock()
			h.enroll++
			h.order = append(h.order, "enroll")
			h.mu.Unlock()
		},
		OnHandleReceived: func(handle string) {
			h.mu.Lock()
			h.handles++
			h.lastHandle = handle
			h.order = append(h.order, "handle")
			h.mu.Unlock()
		},
		OnHookDispatch: func(name string) {
			h.mu.Lock()
			h.hookDispatch++
			h.hookNames = append(h.hookNames, name)
			h.order = append(h.order, "hook:"+name)
			h.mu.Unlock()
		},
		OnRendezvousStop: func() {
			h.mu.Lock()
			h.rendezvousStop++
			h.mu.Unlock()
		},
		OnAgentTouch: func() {
			h.mu.Lock()
			h.agentTouch++
			h.mu.Unlock()
		},
	}
}

type fakeChannel struct {
	closed  atomic.Bool
	mu      sync.Mutex
	got     [][]byte
	sendErr error
}

func (c *fakeChannel) Close() error {
	c.closed.Store(true)
	return nil
}
func (c *fakeChannel) Closed() bool { return c.closed.Load() }
func (c *fakeChannel) Send(msg []byte) error {
	c.mu.Lock()
	inject := c.sendErr
	c.mu.Unlock()
	if inject != nil {
		return inject
	}
	if c.closed.Load() {
		return errors.New("channel closed")
	}
	c.Push(msg)
	return nil
}
func (c *fakeChannel) Push(msg []byte) {
	c.mu.Lock()
	c.got = append(c.got, append([]byte(nil), msg...))
	c.mu.Unlock()
}
func (c *fakeChannel) Got() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.got))
	copy(out, c.got)
	return out
}

type fakeWaiter struct {
	id        string
	cancelled atomic.Bool
}

func (w *fakeWaiter) ID() string { return w.id }
func (w *fakeWaiter) Cancel()    { w.cancelled.Store(true) }
func (w *fakeWaiter) Cancelled() bool {
	return w.cancelled.Load()
}

func newGate() *Gate {
	return &Gate{
		Arrived:   make(chan struct{}),
		Release:   make(chan struct{}),
		Done:      make(chan struct{}),
		Published: make(chan struct{}),
	}
}

func waitBoundary(t *testing.T, errc <-chan error, g *Gate, what string) {
	t.Helper()
	select {
	case err := <-errc:
		t.Fatalf("%s returned before the authorization boundary: %v", what, err)
	case <-g.Arrived:
	case <-time.After(3 * time.Second):
		t.Fatalf("diagnostic timeout: %s neither reached the authorization boundary nor returned", what)
	}
}

func waitResult(t *testing.T, errc <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("diagnostic timeout: %s did not finish", what)
		return nil
	}
}

func ctxTO(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 10*time.Second)
}
