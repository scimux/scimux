package codex

import (
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
func decodeTokenUsage(params json.RawMessage) *UsageEvent {
	var t struct {
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
	}
}

// item is the common envelope of item/started and item/completed notifications.
type itemEnvelope struct {
	Item struct {
		Type    string          `json:"type"`
		ID      string          `json:"id"`
		Text    string          `json:"text"`
		Content []contentBlock  `json:"content"`
		Command string          `json:"command"`
		Status  string          `json:"status"`
		Raw     json.RawMessage `json:"-"`
	} `json:"item"`
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
	it := e.Item
	switch it.Type {
	case "userMessage":
		txt := it.Text
		if txt == "" {
			txt = joinContent(it.Content)
		}
		if strings.TrimSpace(txt) == "" {
			return nil
		}
		return &Event{T: "user", Text: txt}
	case "agentMessage":
		txt := it.Text
		if txt == "" {
			txt = joinContent(it.Content)
		}
		if strings.TrimSpace(txt) == "" {
			return nil
		}
		return &Event{T: "assistant", Text: txt}
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
