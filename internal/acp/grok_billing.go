package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"time"
)

// xAI-specific ACP extension, implemented by the open-source Grok Build CLI but not part of the portable ACP standard.
// GrokBilling maps its billing result onto agentUsage. Five-hour fields are
// intentionally absent: this surface only exposes a weekly credit window.
type GrokBilling struct {
	Plan               string
	CreditUsagePercent float64 // 0..100, clamped
	WeeklyReset        *time.Time
	Source             string
}

// GrokBillingQuery, when set on GrokBillingOptions, replaces the live
// short-lived `grok agent stdio` subprocess. It must return the JSON-RPC
// *result* object of `_x.ai/billing` (not the envelope). Tests inject fakes
// so no CLI, tokens, or network are required.
type GrokBillingQuery func(ctx context.Context) (json.RawMessage, error)

// GrokBillingOptions configures QueryGrokBilling. Production leaves Query
// unset so the live CLI path runs.
type GrokBillingOptions struct {
	Query GrokBillingQuery
}

// QueryGrokBilling asks Grok for subscription credit usage. The CLI must
// already be authenticated (same contract as launching a grok node); scimux
// never authenticates. Failures (missing binary, timeout, protocol error,
// malformed data, unofficial-surface drift) return a generic
// "usage unavailable" error.
func QueryGrokBilling(ctx context.Context, opts GrokBillingOptions) (GrokBilling, error) {
	shell := GrokBilling{Source: "grok-acp-billing"}
	var raw json.RawMessage
	var err error
	if opts.Query != nil {
		raw, err = opts.Query(ctx)
	} else {
		raw, err = grokBillingViaCLI(ctx)
	}
	if err != nil {
		// The provider or injected process may mention account state. Keep the
		// public usage surface deliberately generic.
		return shell, errors.New("usage unavailable")
	}
	return ParseGrokBilling(raw)
}

// grokBillingViaCLI spawns a short-lived ACP agent, runs initialize +
// `_x.ai/billing`, and returns the result object. Stderr is discarded so a
// chatty CLI cannot pollute the scimux log with auth noise on every refresh.
func grokBillingViaCLI(ctx context.Context) (json.RawMessage, error) {
	bin, err := exec.LookPath("grok")
	if err != nil {
		return nil, errors.New("usage unavailable: grok not installed")
	}
	cmd := exec.CommandContext(ctx, bin, "--no-auto-update", "agent", "stdio")
	cmd.Stderr = nil
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("usage unavailable")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, errors.New("usage unavailable")
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.New("usage unavailable")
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	type rpcResp struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	// waitForID drains notifications until the matching response id arrives.
	waitForID := func(want int) (json.RawMessage, error) {
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var r rpcResp
			if json.Unmarshal(line, &r) != nil {
				continue
			}
			// Notifications have a method and no usable id for us.
			var peek map[string]json.RawMessage
			if json.Unmarshal(line, &peek) == nil {
				if _, hasMethod := peek["method"]; hasMethod {
					if _, hasID := peek["id"]; !hasID {
						continue
					}
				}
			}
			if r.ID != want {
				continue
			}
			if r.Error != nil {
				// Never echo agent error bodies (may mention account state).
				return nil, errors.New("usage unavailable")
			}
			return r.Result, nil
		}
		if err := sc.Err(); err != nil {
			return nil, errors.New("usage unavailable")
		}
		return nil, errors.New("usage unavailable")
	}
	writeReq := func(id int, method string, params any) error {
		msg := map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  method,
			"params":  params,
		}
		b, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		_, err = stdin.Write(append(b, '\n'))
		return err
	}

	if err := writeReq(1, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	}); err != nil {
		return nil, errors.New("usage unavailable")
	}
	if _, err := waitForID(1); err != nil {
		return nil, err
	}
	// scimux never authenticates; the CLI uses its already-cached session.
	if err := writeReq(2, "_x.ai/billing", map[string]any{}); err != nil {
		return nil, errors.New("usage unavailable")
	}
	return waitForID(2)
}

// ParseGrokBilling maps a `_x.ai/billing` result onto GrokBilling. Weekly
// creditUsagePercent is "used"; remaining is derived by the caller as
// 100−used. The period end is the weekly reset. Defensive: unknown shapes
// yield "usage unavailable", never a panic.
func ParseGrokBilling(raw json.RawMessage) (GrokBilling, error) {
	shell := GrokBilling{Source: "grok-acp-billing"}
	if len(raw) == 0 {
		return shell, errors.New("usage unavailable")
	}
	var r struct {
		Config *struct {
			CreditUsagePercent *float64 `json:"creditUsagePercent"`
			CurrentPeriod      *struct {
				Type  string `json:"type"`
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"currentPeriod"`
			BillingPeriodEnd string `json:"billingPeriodEnd"`
		} `json:"config"`
		SubscriptionTier string `json:"subscription_tier"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Config == nil || r.Config.CreditUsagePercent == nil {
		return shell, errors.New("usage unavailable")
	}
	used := *r.Config.CreditUsagePercent
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	out := GrokBilling{
		Plan:               r.SubscriptionTier,
		CreditUsagePercent: used,
		Source:             "grok-acp-billing",
	}
	// Prefer currentPeriod.end (the usage window); fall back to billingPeriodEnd.
	end := ""
	if r.Config.CurrentPeriod != nil {
		end = r.Config.CurrentPeriod.End
	}
	if end == "" {
		end = r.Config.BillingPeriodEnd
	}
	if end != "" {
		if t, err := time.Parse(time.RFC3339, end); err == nil {
			out.WeeklyReset = &t
		}
	}
	return out, nil
}
