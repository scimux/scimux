package muse

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Observed against Muse 1.1.1-R2514.1 / MSP schema 1. A fingerprint mismatch
// is observable on the client and must not fail initialization.
const (
	ObservedMuseVersion = "1.1.1-R2514.1"
	ObservedMSPSchema   = 1
	PinnedFingerprint   = "sha256:c669a30c2ee17d63192b227865b424d1d78b5d6c04d9f1c9e9b77b9cf03e6a4f"
)

type SchemaInfo struct {
	Name        string `json:"name,omitempty"`
	Version     int    `json:"version,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type ServerInfo struct {
	Name        string `json:"name,omitempty"`
	Version     string `json:"version,omitempty"`
	Title       string `json:"title,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"` // compatibility alias; schema.fingerprint is authoritative
}

type InitializeResult struct {
	ProtocolVersion     string          `json:"protocolVersion,omitempty"`
	ServerInfo          ServerInfo      `json:"serverInfo"`
	Schema              SchemaInfo      `json:"schema"`
	Capabilities        json.RawMessage `json:"capabilities"`
	GrantedCapabilities []string        `json:"grantedCapabilities,omitempty"`
	SessionDurability   string          `json:"sessionDurability,omitempty"`
	UserInputDialogs    *bool           `json:"userInputDialogs,omitempty"`
	// Fingerprint is a compatibility alias. The documented location is
	// schema.fingerprint; this field is consulted only when schema is empty.
	Fingerprint string `json:"fingerprint,omitempty"`
}

func (r InitializeResult) FingerprintValue() string {
	if r.Schema.Fingerprint != "" {
		return r.Schema.Fingerprint
	}
	if r.Fingerprint != "" {
		return r.Fingerprint
	}
	return r.ServerInfo.Fingerprint
}

type Session struct {
	SessionID  string `json:"sessionId"`
	ViewCursor string `json:"viewCursor,omitempty"`
}

type StartParams struct {
	Cwd          string
	Model        string
	ApprovalMode string
	SessionID    string
}

type TurnResult struct {
	CommandID      string `json:"commandId"`
	TurnID         string `json:"turnId"`
	Status         string `json:"status"`
	Disposition    string `json:"disposition"`
	StartedNewTurn bool   `json:"startedNewTurn"`
	ViewCursor     string `json:"viewCursor,omitempty"`
}

// Model is a catalog row. Null/absent limits stay unknown; no tier is inferred.
type Model struct {
	ID           string  `json:"id"`
	Name         string  `json:"name,omitempty"`
	Label        string  `json:"label,omitempty"`
	Source       string  `json:"source,omitempty"`
	ProfileID    string  `json:"profileId,omitempty"`
	ContextLimit *int    `json:"contextLimit"`
	OutputLimit  *int    `json:"outputLimit"`
	ReleaseDate  *string `json:"releaseDate"`
	IsDefault    bool    `json:"isDefault"`
}

type Spend struct {
	InputTokens         int
	OutputTokens        int
	CachedReadTokens    int
	CacheCreationTokens int
	ReasoningTokens     int
	TotalTokens         int
	Model               string
	TurnID              string
}

type Occupancy struct {
	Used     int
	Size     int
	Pressure string
}

type capabilities struct {
	UserInputCapable bool
	Durable          bool
}

// decodeCapabilities treats omitted booleans as unset, not false: absent
// userInputDialogs means capable; absent sessionDurability means durable.
func decodeCapabilities(raw json.RawMessage) capabilities {
	out := capabilities{UserInputCapable: true, Durable: true}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return out
	}
	var p struct {
		UserInputDialogs  *bool   `json:"userInputDialogs"`
		SessionDurability *string `json:"sessionDurability"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return out
	}
	if p.UserInputDialogs != nil {
		out.UserInputCapable = *p.UserInputDialogs
	}
	if p.SessionDurability != nil && *p.SessionDurability == "ephemeral" {
		out.Durable = false
	}
	return out
}

type item struct {
	ItemID       string `json:"itemId"`
	Kind         string `json:"kind"`
	Revision     int    `json:"revision"`
	Status       string `json:"status"`
	Text         string `json:"text"`
	Tool         string `json:"tool"`
	Args         string `json:"args"`
	CallID       string `json:"callId"`
	ApprovalID   string `json:"approvalId"`
	FallbackText string `json:"fallbackText"`
	Retracted    bool   `json:"retracted"`
	VisibleOut   string `json:"visibleOutput"`
	ChildSession string `json:"childSessionId"`
	Objective    string `json:"objective"`
	CommandText  string `json:"commandText"`
}

// Item IDs remain opaque strings; they are never validated as UUIDv7.
// Any status other than inProgress is terminal for tool-row purposes.
func itemTerminal(status string) bool { return status != "inProgress" }

func toolStatusTerminal(status string) bool { return itemTerminal(status) }

func blank(s string) bool { return strings.TrimSpace(s) == "" }

type fold struct {
	rev        map[string]int
	stream     map[string]string
	streamIDs  []string
	streamProv map[string][]provEntry
	kinds      map[string]string
	hidden     map[string]string
}

func newFold() *fold {
	return &fold{
		rev:        map[string]int{},
		stream:     map[string]string{},
		streamProv: map[string][]provEntry{},
		kinds:      map[string]string{},
		hidden:     map[string]string{},
	}
}

func (f *fold) streaming() string {
	if f == nil {
		return ""
	}
	var b strings.Builder
	for _, id := range f.streamIDs {
		b.WriteString(f.stream[id])
	}
	return b.String()
}

func (f *fold) reset() { f.clearStreams(); f.rev = map[string]int{} }

func (f *fold) clearStreams() {
	f.stream = map[string]string{}
	f.streamIDs = nil
	f.streamProv = map[string][]provEntry{}
	f.kinds = map[string]string{}
	f.hidden = map[string]string{}
}

func (f *fold) apply(method string, params json.RawMessage) []*Event {
	switch method {
	case "item/started", "item/updated", "item/completed":
		return f.applyItem(method, params)
	case "item/delta":
		f.applyDelta(params)
		return nil
	case "view/gap":
		f.clearStreams()
		after, next := decodeViewGap(params)
		return []*Event{{T: "error", Error: "view gap after " + after + " next " + next}}
	case "session/started":
		// Observed on Muse 1.1.1-R2514.1 despite not appearing in schema v1.
		// Tolerate it; do not invent session semantics from it.
		return nil
	case "session/tokenUsage":
		if u := decodeTokenUsage(params); u != nil {
			return []*Event{{T: "usage", Usage: u}}
		}
	case "session/contextUsage":
		if u := decodeContextUsage(params); u != nil {
			return []*Event{{T: "usage", Usage: u}}
		}
	case "turn/completed":
		f.clearStreams()
		if ev := decodeTurnCompleted(params); ev != nil {
			return []*Event{ev}
		}
	}
	return nil
}

func (f *fold) accept(id string, rev int) bool {
	if id == "" || rev < 1 {
		return false
	}
	if cur, ok := f.rev[id]; ok && rev <= cur {
		return false
	}
	f.rev[id] = rev
	return true
}

func (f *fold) applyItem(method string, params json.RawMessage) []*Event {
	var env struct {
		Item json.RawMessage `json:"item"`
	}
	if json.Unmarshal(params, &env) != nil {
		return nil
	}
	var it item
	if json.Unmarshal(env.Item, &it) != nil {
		return nil
	}
	if !f.accept(it.ItemID, it.Revision) {
		return nil
	}
	notifProv := namedProv(params, locNotification)
	itemProv := namedProv(env.Item, locItem)
	f.identify(it.ItemID, it.Kind)
	if method != "item/completed" {
		f.addStreamProv(it.ItemID, notifProv, itemProv)
		// Started/updated may emit tool rows only. userMessage and
		// agentMessage wait for item/completed, even if status looks terminal.
		ev := decodeItem(params)
		if ev == nil || ev.T == "user" || ev.T == "assistant" {
			return nil
		}
		ev.Prov = packProv(notifProv, itemProv)
		return []*Event{ev}
	}
	prior := f.takeStreamProv(it.ItemID)
	f.clearItemStream(it.ItemID)
	ev := decodeItem(params)
	if ev == nil {
		return nil
	}
	ev.Prov = packProv(prior, notifProv, itemProv)
	return []*Event{ev}
}

func (f *fold) applyDelta(params json.RawMessage) {
	id, field, delta := decodeItemDelta(params)
	if id == "" {
		return
	}
	// Deltas never increment or synthesize item revisions.
	if field == "text" && delta != "" && !strings.Contains(field, ".") {
		kind, known := f.kinds[id]
		switch {
		case !known || kind == "":
			f.hidden[id] += delta
		case kind == "agentMessage":
			f.ensureStream(id)
			f.stream[id] += delta
		}
	}
	f.addStreamProv(id, namedProv(params, locNotification))
}

func (f *fold) identify(id, kind string) {
	if id == "" || kind == "" {
		return
	}
	f.kinds[id] = kind
	hid := f.hidden[id]
	delete(f.hidden, id)
	if kind == "agentMessage" {
		f.ensureStream(id)
		if hid != "" {
			f.stream[id] = hid + f.stream[id]
		}
	}
}

func (f *fold) ensureStream(id string) {
	if _, ok := f.stream[id]; ok {
		return
	}
	f.stream[id] = ""
	f.streamIDs = append(f.streamIDs, id)
}

func (f *fold) addStreamProv(id string, groups ...[]provEntry) {
	if id == "" {
		return
	}
	f.streamProv[id] = appendProv(f.streamProv[id], groups...)
}

func (f *fold) takeStreamProv(id string) []provEntry {
	p := f.streamProv[id]
	delete(f.streamProv, id)
	return p
}

func (f *fold) clearItemStream(id string) {
	delete(f.stream, id)
	delete(f.streamProv, id)
	delete(f.kinds, id)
	delete(f.hidden, id)
	out := f.streamIDs[:0]
	for _, s := range f.streamIDs {
		if s != id {
			out = append(out, s)
		}
	}
	f.streamIDs = out
}

func decodeItem(params json.RawMessage) *Event {
	var env struct {
		Item json.RawMessage `json:"item"`
	}
	if json.Unmarshal(params, &env) != nil {
		return nil
	}
	var it item
	if json.Unmarshal(env.Item, &it) != nil || it.ItemID == "" {
		return nil
	}
	switch it.Kind {
	case "userMessage":
		if it.Retracted || blank(it.Text) {
			return nil
		}
		return &Event{T: "user", Text: it.Text}
	case "agentMessage":
		if blank(it.Text) {
			return nil
		}
		return &Event{T: "assistant", Text: it.Text}
	case "reasoning":
		return nil
	}
	return &Event{T: "tool", Tool: &ToolEvent{
		ID:       it.ItemID,
		Title:    itemTitle(it),
		Kind:     it.Kind, // unknown kinds stay visible with an honest label
		Status:   it.Status,
		RawInput: rawArgs(it),
	}}
}

func itemTitle(it item) string {
	switch it.Kind {
	case "toolCall":
		if it.Tool != "" {
			return it.Tool
		}
	case "userShell":
		if it.CommandText != "" {
			return it.CommandText
		}
	case "subagent":
		if it.Objective != "" {
			return it.Objective
		}
	}
	if it.FallbackText != "" {
		return it.FallbackText
	}
	if it.Tool != "" {
		return it.Tool
	}
	if it.Kind != "" {
		return it.Kind
	}
	return "item"
}

func rawArgs(it item) any {
	src := it.Args
	if src == "" {
		src = it.VisibleOut
	}
	if src == "" {
		return nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(src), &parsed); err == nil {
		return parsed
	}
	return src
}

func decodeItemDelta(params json.RawMessage) (id, field, delta string) {
	var p struct {
		ItemID string `json:"itemId"`
		Field  string `json:"field"`
		Delta  string `json:"delta"`
	}
	if json.Unmarshal(params, &p) != nil {
		return "", "", ""
	}
	field = p.Field
	if field == "" {
		field = "text"
	}
	return p.ItemID, field, p.Delta
}

// decodeTokenUsage folds session/tokenUsage into spend. InputTokens comes from
// counted-once promptTokens, never raw usage.inputTokens. Occupancy Used/Size
// are never populated here.
func decodeTokenUsage(params json.RawMessage) *UsageEvent {
	var p struct {
		TurnID       string  `json:"turnId"`
		ModelID      *string `json:"modelId"`
		PromptTokens int     `json:"promptTokens"`
		TotalTokens  int     `json:"totalTokens"`
		Usage        struct {
			OutputTokens     int `json:"outputTokens"`
			CachedTokens     int `json:"cachedTokens"`
			CacheReadTokens  int `json:"cacheReadTokens"`
			CacheWriteTokens int `json:"cacheWriteTokens"`
			ReasoningTokens  int `json:"reasoningTokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	u := &UsageEvent{
		InputTokens:         p.PromptTokens,
		OutputTokens:        p.Usage.OutputTokens,
		CachedReadTokens:    p.Usage.CacheReadTokens,
		CacheCreationTokens: p.Usage.CacheWriteTokens,
		ReasoningTokens:     p.Usage.ReasoningTokens,
		TotalTokens:         p.TotalTokens,
		TurnID:              p.TurnID,
	}
	if u.CachedReadTokens == 0 {
		u.CachedReadTokens = p.Usage.CachedTokens
	}
	if p.ModelID != nil {
		u.Model = *p.ModelID
	}
	return u
}

// decodeContextUsage folds session/contextUsage into occupancy. A null or
// absent window is Size 0. Spend fields are never populated from this plane.
func decodeContextUsage(params json.RawMessage) *UsageEvent {
	var p struct {
		UsedTokens   int    `json:"usedTokens"`
		WindowTokens *int   `json:"windowTokens"`
		Pressure     string `json:"pressure"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	u := &UsageEvent{Used: p.UsedTokens}
	if p.WindowTokens != nil {
		u.Size = *p.WindowTokens
	}
	return u
}

func accumulateSpend(cur, next Spend) Spend {
	if next.TotalTokens < cur.TotalTokens {
		return cur
	}
	if next.TotalTokens == cur.TotalTokens && cur.TotalTokens != 0 {
		return cur
	}
	return next
}

func usageToSpend(u *UsageEvent) Spend {
	if u == nil {
		return Spend{}
	}
	return Spend{
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		CachedReadTokens:    u.CachedReadTokens,
		CacheCreationTokens: u.CacheCreationTokens,
		ReasoningTokens:     u.ReasoningTokens,
		TotalTokens:         u.TotalTokens,
		Model:               u.Model,
		TurnID:              u.TurnID,
	}
}

func decodeTurnCompleted(params json.RawMessage) *Event {
	var p struct {
		TurnID   string `json:"turnId"`
		Terminal string `json:"terminal"`
		Reason   string `json:"reason"`
		Error    *struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	term := p.Terminal
	if term == "" {
		term = "completed"
	}
	msg := p.Reason
	if p.Error != nil {
		msg = strings.TrimSpace(p.Error.Kind + ": " + p.Error.Message)
	}
	if msg == "" {
		msg = term
	}
	switch term {
	case "completed", "succeeded":
		return &Event{T: "stop", StopReason: msg}
	case "cancelled", "interrupted":
		return &Event{T: "stop", StopReason: msg}
	default:
		// Unknown terminals stay visible rather than ending the turn silently.
		return &Event{T: "error", Error: msg}
	}
}

func decodeViewGap(params json.RawMessage) (after, next string) {
	var p struct {
		After string `json:"after"`
		Next  string `json:"next"`
	}
	_ = json.Unmarshal(params, &p)
	return p.After, p.Next
}

func cursorOf(params json.RawMessage) string {
	var p struct {
		ViewCursor string `json:"viewCursor"`
	}
	_ = json.Unmarshal(params, &p)
	return p.ViewCursor
}

const (
	locNotification = "notification"
	locItem         = "item"
	keyMeta         = "_meta"
	keyProv         = "provenance"
)

type provEntry struct {
	Loc string          `json:"loc"`
	Key string          `json:"key"`
	V   json.RawMessage `json:"v"`
}

func namedProv(raw json.RawMessage, loc string) []provEntry {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	var out []provEntry
	for _, key := range []string{keyMeta, keyProv} {
		if v, ok := obj[key]; ok {
			if vv := validJSONValue(v); vv != nil {
				out = append(out, provEntry{Loc: loc, Key: key, V: vv})
			}
		}
	}
	return out
}

func validJSONValue(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !json.Valid(raw) {
		return nil
	}
	return copyRaw(raw)
}

func packProv(groups ...[]provEntry) json.RawMessage {
	all := appendProv(nil, groups...)
	if len(all) == 0 {
		return nil
	}
	b, err := json.Marshal(all)
	if err != nil {
		return nil
	}
	return copyRaw(b)
}

func appendProv(dst []provEntry, groups ...[]provEntry) []provEntry {
	seen := map[string]bool{}
	for _, e := range dst {
		seen[e.Loc+"\x00"+e.Key+"\x00"+string(e.V)] = true
	}
	for _, g := range groups {
		for _, e := range g {
			k := e.Loc + "\x00" + e.Key + "\x00" + string(e.V)
			if seen[k] {
				continue
			}
			seen[k] = true
			dst = append(dst, provEntry{Loc: e.Loc, Key: e.Key, V: copyRaw(e.V)})
		}
	}
	return dst
}
