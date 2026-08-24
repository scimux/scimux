// claude_elicitation.go — Elicitation / ElicitationResult hook transport.
//
// MCP elicitation is passive UX evidence: the helper records that Claude's
// native dialog is asking for input, and removes that record when the user
// (or Claude) finishes. scimux never answers the request, never returns a
// JSON decision object, never holds the hook process open, and never retains
// requested_schema or result content.
package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// claudeElicitationHookCmd is the hidden helper argv token for both
// Elicitation and ElicitationResult. cmd/scimux only dispatches.
const claudeElicitationHookCmd = "__claude-elicitation-hook"

// claudeElicitationStdinLimit bounds the whole stdin payload, including
// requested_schema / content which are skipped and never stored.
const claudeElicitationStdinLimit = 4 << 20

// claudeElicitationTTL is the leak backstop for an active request that is
// never cleared (ElicitationResult missed, scimux down).
const claudeElicitationTTL = 4 * time.Hour

const (
	claudeElicitationRecordMax  = 16 << 10
	claudeElicitationListMax    = 8
	claudeElicitationMessageMax = 2000
	claudeElicitationServerMax  = 200
	claudeElicitationURLMax     = 2048
)

type claudeElicitationEvent struct {
	HookEventName string
	SessionID     string
	Server        string
	Message       string
	Mode          string
	URL           string
	ElicitationID string
	Action        string
	ObservedAt    time.Time
}

type claudeElicitationRecord struct {
	Nonce             string `json:"nonce"`
	SessionID         string `json:"session_id"`
	ElicitationIDHash string `json:"elicitation_id_hash,omitempty"`
	Server            string `json:"mcp_server_name"`
	ServerHash        string `json:"mcp_server_name_hash"`
	Message           string `json:"message"`
	Mode              string `json:"mode,omitempty"`
	URL               string `json:"url,omitempty"`
	Turn              string `json:"turn,omitempty"`
	TurnGen           int    `json:"turn_gen,omitempty"`
	At                string `json:"at"`
}

// claudeElicitationView is the public chat-API projection of one request.
// Nonces, schemas, result content, bundle paths, and raw payloads stay out.
type claudeElicitationView struct {
	Server  string `json:"server"`
	Message string `json:"message"`
	Mode    string `json:"mode,omitempty"`
	URL     string `json:"url,omitempty"`
}

type claudeElicitationFile struct {
	path string
	rec  claudeElicitationRecord
	at   time.Time
}

func claudeElicitationHookCommand(execPath, hookDir string) (string, error) {
	if execPath == "" || hookDir == "" {
		return "", errClaudeHookRejected
	}
	return shellQuote(execPath) + " " + claudeElicitationHookCmd + " --dir " + shellQuote(hookDir), nil
}

func claudeElicitationDir(bundle string) (string, error) {
	if bundle == "" {
		return "", errClaudeHookRejected
	}
	dir := filepath.Join(bundle, "elicitation")
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", errClaudeHookRejected
	}
	return dir, nil
}

func claudeElicitationActiveDir(bundle string) (string, error) {
	if _, err := claudeElicitationDir(bundle); err != nil {
		return "", err
	}
	dir := filepath.Join(bundle, "elicitation", "active")
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", errClaudeHookRejected
	}
	return dir, nil
}

func bundleSupportsElicitation(bundle string) bool {
	caps, ok := readClaudeHookCapabilities(bundle)
	if !ok || caps.Elicitation < 1 {
		return false
	}
	if !claudeBundleExecUsable(caps) {
		return false
	}
	_, err := claudeElicitationActiveDir(bundle)
	return err == nil
}

func allowedClaudeElicitationMode(mode string) bool {
	return mode == "" || mode == "form" || mode == "url"
}

func allowedClaudeElicitationAction(action string) bool {
	switch action {
	case "accept", "decline", "cancel":
		return true
	}
	return false
}

func validatedElicitationURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || !u.IsAbs() {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	if u.Host == "" || u.User != nil {
		return ""
	}
	out := u.String()
	if len(out) > claudeElicitationURLMax {
		return ""
	}
	return out
}

func boundElicitationString(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func digestElicitationValue(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validElicitationDigest(hash string) bool {
	if hash == "" {
		return true
	}
	if len(hash) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

func decodeClaudeElicitationEvent(r io.Reader) (claudeElicitationEvent, error) {
	var empty claudeElicitationEvent
	limited := &io.LimitedReader{R: r, N: claudeElicitationStdinLimit + 1}
	dec := json.NewDecoder(limited)
	tok, err := dec.Token()
	if err != nil {
		return empty, errClaudeHookRejected
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '{' {
		return empty, errClaudeHookRejected
	}
	var ev claudeElicitationEvent
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return empty, errClaudeHookRejected
		}
		key, ok := keyTok.(string)
		if !ok {
			return empty, errClaudeHookRejected
		}
		switch key {
		case "hook_event_name":
			ev.HookEventName, err = decodeJSONStringToken(dec)
		case "session_id":
			ev.SessionID, err = decodeJSONStringToken(dec)
		case "mcp_server_name":
			ev.Server, err = decodeJSONStringToken(dec)
		case "message":
			ev.Message, err = decodeJSONStringToken(dec)
		case "mode":
			ev.Mode, err = decodeJSONStringToken(dec)
		case "url":
			ev.URL, err = decodeJSONStringToken(dec)
		case "elicitation_id":
			ev.ElicitationID, err = decodeJSONStringToken(dec)
		case "action":
			ev.Action, err = decodeJSONStringToken(dec)
		default:
			// requested_schema / content and any other extra members are
			// skipped and never stored.
			err = skipJSONValue(dec)
		}
		if err != nil {
			return empty, errClaudeHookRejected
		}
	}
	if _, err := dec.Token(); err != nil {
		return empty, errClaudeHookRejected
	}
	if _, err := dec.Token(); err != io.EOF {
		return empty, errClaudeHookRejected
	}
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return empty, errClaudeHookRejected
	}
	if limited.N <= 0 {
		return empty, errClaudeHookRejected
	}
	return ev, nil
}

// RunClaudeElicitationHook is the hidden helper body. It never writes stdout
// or stderr: a non-zero Elicitation exit denies the request, and a non-zero
// ElicitationResult exit turns the user's action into decline.
// Invalid input fails closed without mutating existing records. The
// command-main wrapper always exits zero even when this returns an error.
func RunClaudeElicitationHook(dir string, r io.Reader, stdout, stderr io.Writer) error {
	observedAt := time.Now().UTC()
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if dir == "" || !filepath.IsAbs(dir) || dir != filepath.Clean(dir) || !safePathComponent(filepath.Base(dir)) {
		return errClaudeHookRejected
	}
	if _, err := claudeElicitationActiveDir(dir); err != nil {
		return errClaudeHookRejected
	}
	ev, err := decodeClaudeElicitationEvent(r)
	if err != nil {
		return errClaudeHookRejected
	}
	ev.ObservedAt = observedAt
	switch ev.HookEventName {
	case "Elicitation":
		return publishClaudeElicitation(dir, ev)
	case "ElicitationResult":
		return resolveClaudeElicitation(dir, ev)
	}
	return errClaudeHookRejected
}

func publishClaudeElicitation(bundle string, ev claudeElicitationEvent) error {
	if ev.SessionID == "" || ev.Server == "" || ev.Message == "" {
		return errClaudeHookRejected
	}
	if !allowedClaudeElicitationMode(ev.Mode) {
		return errClaudeHookRejected
	}
	observedAt := ev.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	rec := claudeElicitationRecord{
		SessionID:         ev.SessionID,
		ElicitationIDHash: digestElicitationValue(ev.ElicitationID),
		Server:            boundElicitationString(ev.Server, claudeElicitationServerMax),
		ServerHash:        digestElicitationValue(ev.Server),
		Message:           boundElicitationString(ev.Message, claudeElicitationMessageMax),
		Mode:              ev.Mode,
		At:                observedAt.Format(time.RFC3339Nano),
	}
	if ev.Mode == "url" {
		rec.URL = validatedElicitationURL(ev.URL)
	}
	if turn, ok := readClaudeAcceptedTurn(filepath.Join(bundle, "perm")); ok && turn.Session == ev.SessionID {
		// The marker is published before Enter. Do not relabel a helper that
		// started before a later turn marker appeared as belonging to that turn.
		turnAt, err := time.Parse(time.RFC3339Nano, turn.At)
		if err != nil || !observedAt.Before(turnAt) {
			rec.Turn, rec.TurnGen = turn.Turn, turn.Gen
		}
	}
	if rec.Server == "" || rec.Message == "" {
		return errClaudeHookRejected
	}
	active, err := claudeElicitationActiveDir(bundle)
	if err != nil {
		return errClaudeHookRejected
	}
	for i := 0; i < 8; i++ {
		nonce, err := newHookID()
		if err != nil || !safePathComponent(nonce) {
			return errClaudeHookRejected
		}
		rec.Nonce = nonce
		path := filepath.Join(active, nonce+".json")
		if _, err := os.Lstat(path); err == nil {
			continue
		}
		if err := writeClaudePermFile(path, rec); err != nil {
			return errClaudeHookRejected
		}
		return nil
	}
	return errClaudeHookRejected
}

func resolveClaudeElicitation(bundle string, ev claudeElicitationEvent) error {
	if ev.SessionID == "" || ev.Server == "" || !allowedClaudeElicitationAction(ev.Action) {
		return errClaudeHookRejected
	}
	if !allowedClaudeElicitationMode(ev.Mode) {
		return errClaudeHookRejected
	}
	files := readClaudeElicitationFiles(bundle, time.Now())
	var matches []claudeElicitationFile
	if ev.ElicitationID != "" {
		idHash := digestElicitationValue(ev.ElicitationID)
		serverHash := digestElicitationValue(ev.Server)
		for _, f := range files {
			if f.rec.SessionID == ev.SessionID && f.rec.ServerHash == serverHash &&
				f.rec.ElicitationIDHash == idHash && (ev.Mode == "" || f.rec.Mode == ev.Mode) {
				matches = append(matches, f)
			}
		}
		for _, m := range matches {
			if err := removeClaudeElicitationFile(m.path); err != nil {
				return err
			}
		}
		return nil
	}
	serverHash := digestElicitationValue(ev.Server)
	for _, f := range files {
		if f.rec.SessionID != ev.SessionID || f.rec.ServerHash != serverHash {
			continue
		}
		if ev.Mode != "" && f.rec.Mode != ev.Mode {
			continue
		}
		matches = append(matches, f)
	}
	if len(matches) != 1 {
		return nil
	}
	return removeClaudeElicitationFile(matches[0].path)
}

func removeClaudeElicitationFile(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errClaudeHookRejected
	}
	_ = sessionlog.SyncParentDir(path)
	return nil
}

func readClaudeElicitationFiles(bundle string, now time.Time) []claudeElicitationFile {
	active, err := claudeElicitationActiveDir(bundle)
	if err != nil {
		return nil
	}
	ents, err := os.ReadDir(active)
	if err != nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	var out []claudeElicitationFile
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if !safePathComponent(name) {
			continue
		}
		p := filepath.Join(active, e.Name())
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > claudeElicitationRecordMax {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil || len(b) == 0 || len(b) > claudeElicitationRecordMax {
			continue
		}
		var rec claudeElicitationRecord
		if json.Unmarshal(b, &rec) != nil {
			continue
		}
		if rec.Nonce == "" {
			rec.Nonce = name
		}
		if rec.Nonce != name || rec.SessionID == "" || rec.Server == "" || rec.Message == "" ||
			!validElicitationDigest(rec.ElicitationIDHash) || !validElicitationDigest(rec.ServerHash) ||
			rec.ServerHash == "" || rec.TurnGen < 0 ||
			(rec.Turn != "" && !safePathComponent(rec.Turn)) {
			continue
		}
		if !allowedClaudeElicitationMode(rec.Mode) {
			continue
		}
		if rec.URL != "" && validatedElicitationURL(rec.URL) == "" {
			rec.URL = ""
		}
		at, err := time.Parse(time.RFC3339Nano, rec.At)
		if err != nil {
			at, err = time.Parse(time.RFC3339, rec.At)
			if err != nil {
				continue
			}
		}
		if now.Sub(at) > claudeElicitationTTL || at.After(now.Add(time.Minute)) {
			_ = os.Remove(p)
			continue
		}
		out = append(out, claudeElicitationFile{path: p, rec: rec, at: at})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].at.Equal(out[j].at) {
			return out[i].at.Before(out[j].at)
		}
		return out[i].rec.Nonce < out[j].rec.Nonce
	})
	return out
}

func liveClaudeElicitations(bundle, session string, now time.Time) []claudeElicitationFile {
	if session == "" {
		return nil
	}
	var out []claudeElicitationFile
	for _, f := range readClaudeElicitationFiles(bundle, now) {
		if f.rec.SessionID != session {
			continue
		}
		if !claudeElicitationBelongsToOpenTurn(bundle, f) {
			_ = removeClaudeElicitationFile(f.path)
			continue
		}
		out = append(out, f)
	}
	return out
}

func claudeElicitationBelongsToOpenTurn(bundle string, f claudeElicitationFile) bool {
	perm := filepath.Join(bundle, "perm")
	current, hasCurrent := readClaudeAcceptedTurn(perm)
	closed, hasClosed := readClaudeTurnClosed(perm)
	if f.rec.Turn != "" {
		if !hasCurrent || current.Turn != f.rec.Turn || current.Session != f.rec.SessionID {
			return false
		}
		return f.rec.TurnGen == 0 || current.Gen == 0 || f.rec.TurnGen == current.Gen
	}
	// A helper spawned by an app-accepted turn must observe turn.json, which
	// is durable before Enter. Untagged records while such a turn is current
	// cannot be proven to belong to it.
	if hasCurrent {
		return false
	}
	if !hasClosed || (closed.Session != "" && closed.Session != f.rec.SessionID) || closed.At == "" {
		return true
	}
	closedAt, cerr := time.Parse(time.RFC3339Nano, closed.At)
	if cerr != nil {
		return false
	}
	// An untagged terminal-entered turn after the boundary is supported. A
	// helper that started before cleanup but published after it is rejected.
	return f.at.After(closedAt)
}

func clearClaudeElicitationsForSession(bundle, session string) {
	if bundle == "" {
		return
	}
	for _, f := range readClaudeElicitationFiles(bundle, time.Now()) {
		if session != "" && f.rec.SessionID != "" && f.rec.SessionID != session {
			continue
		}
		_ = removeClaudeElicitationFile(f.path)
	}
}

func (a *app) claudeElicitationWaiting(n *Node) bool {
	waiting, _, _ := a.claudeElicitationChatState(n)
	return waiting
}

// claudeElicitationChatState is the chat-server projection of
// elicitation/active/*.json. Current session, non-expired records only.
func (a *app) claudeElicitationChatState(n *Node) (bool, int, []claudeElicitationView) {
	if a == nil || n == nil || n.Agent != "claude" || n.transport() != "tmux" {
		return false, 0, nil
	}
	a.mu.Lock()
	ended := n.EndedAt != ""
	live := a.live[n.ID]
	sup := a.claudeSupervisionOf(n)
	hookID := a.claudeHookIDLocked(n.ID)
	sid := n.SessionID
	a.mu.Unlock()
	if ended || live == "exited" || sup != claudeSupStrict {
		return false, 0, nil
	}
	bundle := a.claudeHookBundlePath(hookID)
	if !bundleSupportsElicitation(bundle) {
		return false, 0, nil
	}
	files := liveClaudeElicitations(bundle, sid, time.Now())
	if len(files) == 0 {
		return false, 0, nil
	}
	out := make([]claudeElicitationView, 0, len(files))
	for _, f := range files {
		if len(out) >= claudeElicitationListMax {
			break
		}
		item := claudeElicitationView{
			Server:  boundElicitationString(f.rec.Server, claudeElicitationServerMax),
			Message: boundElicitationString(f.rec.Message, claudeElicitationMessageMax),
			Mode:    f.rec.Mode,
		}
		if u := validatedElicitationURL(f.rec.URL); u != "" {
			item.URL = u
		}
		out = append(out, item)
	}
	return true, len(files), out
}

func runClaudeElicitationHookMain(args []string) int {
	dir := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			dir = args[i+1]
			i++
		}
	}
	_ = RunClaudeElicitationHook(dir, os.Stdin, io.Discard, io.Discard)
	return 0
}
