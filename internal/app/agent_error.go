package app

import (
	"encoding/json"
	"strings"
)

// userFacingAgentError turns transport envelopes into concise recovery copy.
// It is presentation-only: classification never feeds liveness, attention,
// turn completion, or retry behavior. Unknown errors pass through unchanged.
func userFacingAgentError(agent, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	message := embeddedErrorMessage(raw)
	lower := strings.ToLower(message)

	if agent == "grok" && strings.Contains(lower, "grok build usage balance exhausted") {
		return "Grok Build usage balance exhausted. Add credits or wait for the balance to reset, then try again."
	}
	if strings.Contains(lower, "usage limit") ||
		strings.Contains(lower, "session limit") ||
		strings.Contains(lower, "monthly spend limit") ||
		strings.Contains(lower, "quota exceeded") ||
		strings.Contains(lower, "credit balance") {
		if !strings.HasSuffix(message, ".") {
			message += "."
		}
		if !strings.Contains(lower, "try again") &&
			!strings.Contains(lower, "reset") &&
			!strings.Contains(lower, "raise it") &&
			!strings.Contains(lower, "add credit") {
			message += " Try again after the account limit resets."
		}
		return message
	}

	return message
}

// embeddedErrorMessage extracts the deepest useful JSON "message" from the
// envelopes emitted by ACP and codex app-server. It deliberately ignores
// unknown fields and falls back to the original text.
func embeddedErrorMessage(raw string) string {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return raw
	}
	var v any
	if json.Unmarshal([]byte(raw[start:]), &v) != nil {
		return raw
	}
	if message := findErrorMessage(v); message != "" {
		return message
	}
	return raw
}

func findErrorMessage(v any) string {
	switch x := v.(type) {
	case map[string]any:
		if nested, ok := x["error"]; ok {
			if message := findErrorMessage(nested); message != "" {
				return message
			}
		}
		// JSON-RPC deliberately keeps its generic "Internal error" in the
		// envelope's message and the provider's actionable explanation in data.
		// Prefer that structured detail before falling back to the envelope.
		if nested, ok := x["data"]; ok {
			if message := findErrorMessage(nested); message != "" {
				return message
			}
		}
		if message, ok := x["message"].(string); ok && strings.TrimSpace(message) != "" {
			return strings.TrimSpace(message)
		}
		for key, nested := range x {
			if key == "error" || key == "data" || key == "message" {
				continue
			}
			if message := findErrorMessage(nested); message != "" {
				return message
			}
		}
	case []any:
		for _, nested := range x {
			if message := findErrorMessage(nested); message != "" {
				return message
			}
		}
	}
	return ""
}
