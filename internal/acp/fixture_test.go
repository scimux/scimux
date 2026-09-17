package acp

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

// fixtureFrame is one agent→client line in a Grok ACP capture.
//
//	{"type":"response","result":{…}}                 // next request's result
//	{"type":"notification","method":"…","params":…}  // emitted before the next response
//
// Responses are paired with client requests in order (initialize, session/new,
// session/prompt, …). Notifications between responses are flushed when the
// following response is consumed. Fully synthetic fixtures live at
// testdata/synthetic-*.ndjson; real captures at testdata/real-*.ndjson are
// gitignored and supplied locally by maintainers.
type fixtureFrame struct {
	Type   string          `json:"type"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

func loadACPFixture(t *testing.T, name string) []fixtureFrame {
	t.Helper()
	path := filepath.Join("testdata", name)
	b, err := os.ReadFile(path)
	if err != nil {
		// real-* captures are gitignored; skip like codex golden tests.
		if strings.HasPrefix(name, "real-") && os.IsNotExist(err) {
			t.Skipf("golden fixture %s absent (optional local real capture)", name)
		}
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var frames []fixtureFrame
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var fr fixtureFrame
		if err := json.Unmarshal([]byte(line), &fr); err != nil {
			t.Fatalf("fixture %s line %d: %v", name, i+1, err)
		}
		if fr.Type != "response" && fr.Type != "notification" {
			t.Fatalf("fixture %s line %d: unknown type %q", name, i+1, fr.Type)
		}
		frames = append(frames, fr)
	}
	if len(frames) == 0 {
		t.Fatalf("fixture %s is empty", name)
	}
	return frames
}

// fixtureProcess is a Process that speaks agent-side ACP from a fixture.
// The SDK ClientSideConnection writes requests to Stdin; we reply on Stdout
// with the next fixture response (and any preceding notifications).
type fixtureProcess struct {
	stdin  *io.PipeWriter // client writes here
	stdout *io.PipeReader // client reads here
	inR    *io.PipeReader
	outW   *io.PipeWriter
	done   chan struct{}
	once   sync.Once
}

func startFixtureProcess(t *testing.T, frames []fixtureFrame) *fixtureProcess {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	fp := &fixtureProcess{
		stdin: inW, stdout: outR, inR: inR, outW: outW, done: make(chan struct{}),
	}
	go fp.serve(frames)
	return fp
}

func (p *fixtureProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *fixtureProcess) Stdout() io.Reader     { return p.stdout }
func (p *fixtureProcess) Kill() error {
	p.once.Do(func() {
		_ = p.stdin.Close()
		_ = p.outW.Close()
		_ = p.inR.Close()
		_ = p.stdout.Close()
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
		}
	})
	return nil
}
func (p *fixtureProcess) Wait() error {
	<-p.done
	return nil
}

func (p *fixtureProcess) serve(frames []fixtureFrame) {
	defer close(p.done)
	defer p.outW.Close()
	sc := bufio.NewScanner(p.inR)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	idx := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if json.Unmarshal(line, &req) != nil || len(req.ID) == 0 {
			// Notifications from client (e.g. session/cancel) — ignore.
			continue
		}
		// Flush notifications until the next response frame.
		for idx < len(frames) && frames[idx].Type == "notification" {
			fr := frames[idx]
			idx++
			msg := map[string]any{
				"jsonrpc": "2.0",
				"method":  fr.Method,
			}
			if len(fr.Params) > 0 {
				msg["params"] = json.RawMessage(fr.Params)
			}
			p.writeJSON(msg)
		}
		if idx >= len(frames) || frames[idx].Type != "response" {
			// No scripted answer: return empty result so the SDK unblocks.
			p.writeJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result":  map[string]any{},
			})
			continue
		}
		fr := frames[idx]
		idx++
		out := map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(req.ID),
		}
		if len(fr.Error) > 0 && string(fr.Error) != "null" {
			out["error"] = json.RawMessage(fr.Error)
		} else if len(fr.Result) > 0 {
			out["result"] = json.RawMessage(fr.Result)
		} else {
			out["result"] = map[string]any{}
		}
		p.writeJSON(out)
	}
}

func (p *fixtureProcess) writeJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = p.outW.Write(append(b, '\n'))
}

// fixtureRunner returns a Runner that always plays the given frames.
func fixtureRunner(t *testing.T, frames []fixtureFrame) Runner {
	t.Helper()
	return func(nodeID, agent, dir, model, effort string) (Process, error) {
		return startFixtureProcess(t, frames), nil
	}
}

// assertFixturePrivacy fails if a fixture still embeds environment markers.
// Applied to both synthetic (must stay clean) and real captures (scrub gate).
func assertFixturePrivacy(t *testing.T, frames []fixtureFrame) {
	t.Helper()
	blob := ""
	for _, fr := range frames {
		b, _ := json.Marshal(fr)
		blob += string(b) + "\n"
	}
	home, _ := os.UserHomeDir()
	if home != "" && home != "/" && strings.Contains(blob, home) {
		t.Fatalf("fixture still contains $HOME %q", home)
	}
	for _, needle := range []string{
		"/home/", "/Users/",
		"@gmail.", "@x.ai",
		"sk-", "Bearer ",
		"aiagent.local", // hostnames from this lab must not ship in synthetic
	} {
		if strings.Contains(blob, needle) {
			// synthetic uses /scrubbed only; real captures must scrub homes.
			if needle == "/home/" || needle == "/Users/" {
				t.Fatalf("fixture still contains absolute home path marker %q", needle)
			}
			if needle == "aiagent.local" && !strings.Contains(t.Name(), "Real") {
				t.Fatalf("synthetic fixture must not embed lab hostname %q", needle)
			}
			if strings.Contains(needle, "@") || needle == "sk-" || needle == "Bearer " {
				t.Fatalf("fixture still contains secret/email marker %q", needle)
			}
		}
	}
	// Skill inventories are the main privacy leak from Grok availableCommands.
	if strings.Contains(blob, "SKILL.md") || strings.Contains(blob, "availableCommands") {
		t.Fatal("fixture still contains skill inventory / availableCommands")
	}
	if strings.Contains(blob, "~/") || strings.Contains(blob, "~\\") {
		t.Fatal("fixture still contains tilde-home path")
	}
}

// TestSyntheticGrokReplay drives Manager against the committed synthetic
// fixture: initialize → session/new → prompt with Grok-style _meta usage.
// Asserts context occupancy Used/Size for the gauge and assistant text.
func TestSyntheticGrokReplay(t *testing.T) {
	frames := loadACPFixture(t, "synthetic-grok-turn.ndjson")
	assertFixturePrivacy(t, frames)

	m := NewManagerWithRunner(t.TempDir(), fixtureRunner(t, frames))
	sid, err := m.Launch("n1", "grok", t.TempDir(), "grok-4.5", "low")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if sid == "" {
		t.Fatal("empty session id")
	}
	// Window must come from initialize modelState (500000 in the fixture).
	if s := m.session("n1"); s == nil || s.ctxSize != 500_000 {
		t.Fatalf("ctxSize = %v, want 500000", s)
	}
	if err := m.Send("n1", "Reply with exactly: pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, "turn quiet", func() bool { return m.Live("n1") == "quiet" })

	used, window := m.Usage("n1")
	if used != 16330 || window != 500_000 {
		t.Fatalf("usage = %d/%d, want 16330/500000 (Grok _meta occupancy)", used, window)
	}
	// Assistant text from agent_message_chunk must land in the session log.
	peek := Peek(m, "n1")
	if !strings.Contains(peek, "pong") {
		t.Fatalf("synthetic assistant text missing from session log: %q", peek)
	}
}

// TestSyntheticDshReplay drives Manager against the committed synthetic dsh
// fixture. dsh's wire shape differs from Grok's in the two ways that matter
// to the gauge, and this pins both: initialize carries no modelState, so no
// context window is knowable at launch; and the prompt response is a bare
// stopReason with no usage object, so the only statement of spend all turn is
// the mid-turn usage_update. Read the second without the first and the gauge
// has a Used with no Size; wait for the turn-end record and it never comes.
func TestSyntheticDshReplay(t *testing.T) {
	frames := loadACPFixture(t, "synthetic-dsh-turn.ndjson")
	assertFixturePrivacy(t, frames)

	m := NewManagerWithRunner(t.TempDir(), fixtureRunner(t, frames))
	// No model/effort: dsh takes neither on argv, and the default path is the
	// one every launch uses until a supervisor picks something else.
	sid, err := m.Launch("n1", "dsh", t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if sid != "SESSION-SYN-DSH" {
		t.Fatalf("session id = %q, want the agent's own", sid)
	}
	// Nothing at initialize advertises a window. Inventing one here would
	// paint a gauge against a denominator no agent stated.
	if s := m.session("n1"); s == nil || s.ctxSize != 0 {
		t.Fatalf("ctxSize = %v, want 0 — dsh advertises no window at initialize", s)
	}
	if err := m.Send("n1", "Reply with exactly: pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, "turn quiet", func() bool { return m.Live("n1") == "quiet" })

	used, window := m.Usage("n1")
	if used != 7516 || window != 262144 {
		t.Fatalf("usage = %d/%d, want 7516/262144 from the mid-turn usage_update", used, window)
	}
	peek := Peek(m, "n1")
	if !strings.Contains(peek, "pong") {
		t.Fatalf("synthetic assistant text missing from session log: %q", peek)
	}
}

// TestRealGrokReplay is an optional maintainer-supplied local golden.
// Skips when the gitignored capture is absent (CI).
func TestRealGrokReplay(t *testing.T) {
	frames := loadACPFixture(t, "real-grok-turn.ndjson")
	assertFixturePrivacy(t, frames)

	// Must have at least initialize + session/new + prompt responses.
	nResp := 0
	for _, fr := range frames {
		if fr.Type == "response" {
			nResp++
		}
	}
	if nResp < 3 {
		t.Fatalf("real fixture has %d responses, want ≥3", nResp)
	}

	m := NewManagerWithRunner(t.TempDir(), fixtureRunner(t, frames))
	if _, err := m.Launch("n1", "grok", t.TempDir(), "grok-4.5", "low"); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := m.Send("n1", "Reply with exactly: pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, "turn quiet", func() bool { return m.Live("n1") == "quiet" })

	used, window := m.Usage("n1")
	if used <= 0 {
		t.Fatalf("expected positive used tokens from real capture, got %d", used)
	}
	if window <= 0 {
		t.Fatalf("expected positive context window from real capture, got %d", window)
	}
}

// TestRealDshReplay is the dsh half of the optional maintainer lane: a local
// capture of a real `dsh --profile acp` turn, replayed through Manager. It
// never runs dsh — the capture is taken by hand, out of band, and the file is
// gitignored, so CI and every other machine skip this.
//
// What it defends that the synthetic fixture cannot: that dsh still states
// spend the way we read it. dsh sends no usage object on the prompt response
// and no modelState on initialize, so `usage_update` is the only frame that
// names the occupancy — if a future dsh moved it, the gauge would read zero
// and nothing else in the suite would notice.
func TestRealDshReplay(t *testing.T) {
	frames := loadACPFixture(t, "real-dsh-turn.ndjson")
	assertFixturePrivacy(t, frames)

	nResp := 0
	for _, fr := range frames {
		if fr.Type == "response" {
			nResp++
		}
	}
	if nResp < 3 {
		t.Fatalf("real fixture has %d responses, want >=3 (initialize, session/new, prompt)", nResp)
	}

	m := NewManagerWithRunner(t.TempDir(), fixtureRunner(t, frames))
	// Empty model and effort: a real capture was recorded against whatever
	// the profile defaults to, and a SetSessionConfigOption call here would
	// consume a scripted response the capture never allocated.
	if _, err := m.Launch("n1", "dsh", t.TempDir(), "", ""); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := m.Send("n1", "Reply with exactly: pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, "turn quiet", func() bool { return m.Live("n1") == "quiet" })

	used, window := m.Usage("n1")
	if used <= 0 || window <= 0 {
		t.Fatalf("usage = %d/%d, want both positive from the real usage_update", used, window)
	}
}

// TestRealDshModelMenu reads the model select straight out of the same local
// capture. It is the only check that a real dsh still encodes a model option
// value as the JSON ["provider","model"] pair the launch path decodes; the
// synthetic fixture can only prove that we read the shape we wrote down.
// It parses the recorded session/new answer rather than opening a session of
// its own — a dsh session is durable and undeletable, so nothing outside a
// chat the user launched may create one.
func TestRealDshModelMenu(t *testing.T) {
	frames := loadACPFixture(t, "real-dsh-turn.ndjson")
	assertFixturePrivacy(t, frames)

	var ids []string
	for _, fr := range frames {
		if fr.Type != "response" || len(fr.Result) == 0 {
			continue
		}
		var resp sdk.NewSessionResponse
		if err := json.Unmarshal(fr.Result, &resp); err != nil {
			continue
		}
		if resp.SessionId == "" {
			continue
		}
		for _, opt := range resp.ConfigOptions {
			sel := opt.Select
			if sel == nil || !containsFold(string(sel.Id), "model") {
				continue
			}
			for _, o := range selectOptions(sel) {
				if id := dshModelID(string(o.Value)); id != "" {
					ids = append(ids, id)
				}
			}
		}
	}
	if len(ids) == 0 {
		t.Fatal("real capture yielded no models: dsh no longer advertises a model select we can decode")
	}
	for _, id := range ids {
		if !strings.Contains(id, "/") {
			t.Fatalf("model id %q is not provider/model: the option value stopped decoding as a pair", id)
		}
	}
}

// Peek is a tiny test helper: read the session log path the manager uses.
func Peek(m *Manager, nodeID string) string {
	return string(mustRead(m.logPath(nodeID)))
}

func mustRead(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}
