package codex

import (
	"bytes"
	"encoding/json"
	"strings"
)

// The Event/ToolEvent/UsageEvent record types are the unified sessionlog
// schema, aliased in log.go — the codex transport writes the same records as
// every other transport, which is what makes the store readable uniformly.

// --- notification decoders (defensive: unknown shapes yield nothing) ---

// decodeAgentDelta pulls the streamed assistant text fragment and its item id
// from an item/agentMessage/delta notification. Both fields are set by the
// server; if either is absent the delta is ignored by the accumulator.
func decodeAgentDelta(params json.RawMessage) (itemID, delta string) {
	var d struct {
		ItemID string `json:"itemId"`
		Delta  string `json:"delta"`
		// Some server shapes embed the item object instead of a flat itemId.
		Item struct {
			ID string `json:"id"`
		} `json:"item"`
	}
	_ = json.Unmarshal(params, &d)
	id := d.ItemID
	if id == "" {
		id = d.Item.ID
	}
	return id, d.Delta
}

// decodeItemID extracts the item id from an item/completed notification so the
// delta accumulator can flush buffered text for the right item.
func decodeItemID(params json.RawMessage) string {
	var e struct {
		Item struct {
			ID string `json:"id"`
		} `json:"item"`
	}
	_ = json.Unmarshal(params, &e)
	return e.Item.ID
}

// decodeStatus returns the thread status type ("active" | "idle" | ...) from a
// thread/status/changed notification. The status is an object, not a string.
func decodeStatus(params json.RawMessage) string {
	var s struct {
		Status struct {
			Type string `json:"type"`
		} `json:"status"`
	}
	_ = json.Unmarshal(params, &s)
	return s.Status.Type
}

// decodeTokenUsage folds a thread/tokenUsage/updated notification into a
// UsageEvent. The "last" block is the just-finished turn (the leg boundary fare
// telemetry needs); modelContextWindow becomes Size.
//
// sessionModel is the session's effective model (log meta.model / thread/start).
// O4 (Phase 6): the codex app-server tokenUsage notification has no per-turn
// model field — fall back to sessionModel. TurnID comes from params.turnId when
// present; absent → empty so ReadFare uses the source-seam watermark (D3).
// Identity fields are additive; occupancy and token breakdown are unchanged.
func decodeTokenUsage(params json.RawMessage, sessionModel string) *UsageEvent {
	var t struct {
		TurnID     string `json:"turnId"`
		TokenUsage struct {
			Last struct {
				TotalTokens       int `json:"totalTokens"`
				InputTokens       int `json:"inputTokens"`
				CachedInputTokens int `json:"cachedInputTokens"`
				OutputTokens      int `json:"outputTokens"`
			} `json:"last"`
			ModelContextWindow int `json:"modelContextWindow"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(params, &t) != nil {
		return nil
	}
	last := t.TokenUsage.Last
	return &UsageEvent{
		Used:             last.TotalTokens,
		Size:             t.TokenUsage.ModelContextWindow,
		InputTokens:      last.InputTokens,
		OutputTokens:     last.OutputTokens,
		CachedReadTokens: last.CachedInputTokens,
		TotalTokens:      last.TotalTokens,
		// O4: no per-turn model on this notification — session meta.model fallback.
		Model:  sessionModel,
		TurnID: t.TurnID,
	}
}

// item is the common envelope of item/started and item/completed notifications.
type itemEnvelope struct {
	Item json.RawMessage `json:"item"`
}

type itemBody struct {
	Type    string         `json:"type"`
	ID      string         `json:"id"`
	Text    string         `json:"text"`
	Content []contentBlock `json:"content"`
	Command string         `json:"command"`
	Status  string         `json:"status"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// decodeItem maps a completed item notification to a log Event. userMessage and
// agentMessage become chat turns; commandExecution and other tool items become
// ToolEvents. Unknown item types yield nil (silently ignored).
func decodeItem(params json.RawMessage, completed bool) *Event {
	var e itemEnvelope
	if json.Unmarshal(params, &e) != nil {
		return nil
	}
	var it itemBody
	if json.Unmarshal(e.Item, &it) != nil {
		return nil
	}
	prov := packProv(namedProv(params, locNotification), namedProv(e.Item, locItem))
	switch it.Type {
	case "userMessage":
		txt := it.Text
		if txt == "" {
			txt = joinContent(it.Content)
		}
		if strings.TrimSpace(txt) == "" {
			return nil
		}
		return &Event{T: "user", Text: txt, Prov: prov}
	case "agentMessage":
		txt := it.Text
		if txt == "" {
			txt = joinContent(it.Content)
		}
		if strings.TrimSpace(txt) == "" && len(prov) == 0 {
			return nil
		}
		return &Event{T: "assistant", Text: txt, Prov: prov}
	case "reasoning":
		// Reasoning is not surfaced as chat; ignore.
		return nil
	default:
		// Treat any other item as a tool with whatever detail we have.
		status := it.Status
		if status == "" && completed {
			status = "completed"
		} else if status == "" {
			status = "started"
		}
		title := it.Command
		if title == "" {
			title = it.Type
		}
		return &Event{T: "tool", Tool: &ToolEvent{
			ID: it.ID, Title: title, Kind: it.Type, Status: status,
			RawInput: nonEmpty(it.Command),
		}}
	}
}

func joinContent(blocks []contentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func nonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
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
	if v, ok := obj[keyMeta]; ok {
		if vv := validJSONValue(v); vv != nil {
			out = append(out, provEntry{Loc: loc, Key: keyMeta, V: vv})
		}
	}
	if v, ok := obj[keyProv]; ok {
		if vv := validJSONValue(v); vv != nil {
			out = append(out, provEntry{Loc: loc, Key: keyProv, V: vv})
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
	var all []provEntry
	for _, g := range groups {
		all = append(all, g...)
	}
	if len(all) == 0 {
		return nil
	}
	b, err := json.Marshal(all)
	if err != nil {
		return nil
	}
	return copyRaw(b)
}

func copyRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	out := make(json.RawMessage, len(raw))
	copy(out, raw)
	return out
}
