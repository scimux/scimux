package app

import "testing"

// TestEligibleAutoAllowMatrix is the pure policy table for structured
// auto-approval. One function is shared by ACP and Codex — never duplicate.
//
// Semantic policy (approved P3 amendment): exactly one option with kind
// "allow" (one-time / request-scoped) is eligible, regardless of displayed
// key or position. Zero or multiple "allow" options stay manual. Never
// automates allow_always, reject, reject_always, missing, or unknown kinds.
func TestEligibleAutoAllowMatrix(t *testing.T) {
	allow1 := PermOption{Key: "1", Name: "Allow once", Kind: "allow"}
	reject2 := PermOption{Key: "2", Name: "Reject", Kind: "reject"}
	allowAlways1 := PermOption{Key: "1", Name: "Always", Kind: "allow_always"}
	reject1 := PermOption{Key: "1", Name: "Reject", Kind: "reject"}
	rejectAlways1 := PermOption{Key: "1", Name: "Never", Kind: "reject_always"}
	unknown1 := PermOption{Key: "1", Name: "Maybe", Kind: ""}
	// Grok-style: option 1 is allow_always, sole one-time allow is key "2".
	grokStyle := []PermOption{
		{Key: "1", Name: "Always allow", Kind: "allow_always"},
		{Key: "2", Name: "Allow once", Kind: "allow"},
		{Key: "3", Name: "Reject", Kind: "reject"},
	}
	// Sole allow at a later non-"1" key (e.g. reject first, allow second).
	allowOnly2 := []PermOption{
		{Key: "1", Name: "Reject", Kind: "reject"},
		{Key: "2", Name: "Allow once", Kind: "allow"},
	}
	// Sole allow at key "3" (two non-allows first).
	allowOnly3 := []PermOption{
		{Key: "1", Name: "Always", Kind: "allow_always"},
		{Key: "2", Name: "Reject", Kind: "reject"},
		{Key: "3", Name: "Allow once", Kind: "allow"},
	}
	// Two one-time allows → ambiguous, stay manual.
	twoAllows := []PermOption{
		{Key: "1", Name: "Allow once", Kind: "allow"},
		{Key: "2", Name: "Allow for this tool", Kind: "allow"},
		{Key: "3", Name: "Reject", Kind: "reject"},
	}

	cases := []struct {
		name            string
		armed           bool
		afterEnable     bool // request was created after the enable cutoff
		opts            []PermOption
		wantOK          bool
		wantSelectedKey string
	}{
		{
			name:  "sole allow at key 1 is eligible",
			armed: true, afterEnable: true,
			opts:   []PermOption{allow1, reject2},
			wantOK: true, wantSelectedKey: "1",
		},
		{
			name:  "grok style allow_always then allow selects key 2",
			armed: true, afterEnable: true,
			opts:   grokStyle,
			wantOK: true, wantSelectedKey: "2",
		},
		{
			name:  "sole allow at later key 2 is eligible",
			armed: true, afterEnable: true,
			opts:   allowOnly2,
			wantOK: true, wantSelectedKey: "2",
		},
		{
			name:  "sole allow at later key 3 is eligible",
			armed: true, afterEnable: true,
			opts:   allowOnly3,
			wantOK: true, wantSelectedKey: "3",
		},
		{
			name:  "allow_always alone is ineligible",
			armed: true, afterEnable: true,
			opts:   []PermOption{allowAlways1},
			wantOK: false,
		},
		{
			name:  "allow_always first with only reject is ineligible",
			armed: true, afterEnable: true,
			opts:   []PermOption{allowAlways1, reject2},
			wantOK: false,
		},
		{
			name:  "multiple one-time allow options are ambiguous",
			armed: true, afterEnable: true,
			opts:   twoAllows,
			wantOK: false,
		},
		{
			name:  "reject first is not eligible when no allow",
			armed: true, afterEnable: true,
			opts:   []PermOption{reject1},
			wantOK: false,
		},
		{
			name:  "reject_always alone is not eligible",
			armed: true, afterEnable: true,
			opts:   []PermOption{rejectAlways1},
			wantOK: false,
		},
		{
			name:  "missing kind is not eligible",
			armed: true, afterEnable: true,
			opts:   []PermOption{unknown1},
			wantOK: false,
		},
		{
			name:  "missing options is not eligible",
			armed: true, afterEnable: true,
			opts:   nil,
			wantOK: false,
		},
		{
			name:  "empty options is not eligible",
			armed: true, afterEnable: true,
			opts:   []PermOption{},
			wantOK: false,
		},
		{
			name:  "request already pending at enable is not eligible",
			armed: true, afterEnable: false,
			opts:   []PermOption{allow1, reject2},
			wantOK: false,
		},
		{
			name:  "request at enable even with sole allow at key 2 is not eligible",
			armed: true, afterEnable: false,
			opts:   grokStyle,
			wantOK: false,
		},
		{
			name:  "not armed is not eligible",
			armed: false, afterEnable: true,
			opts:   []PermOption{allow1},
			wantOK: false,
		},
		{
			name:  "sole allow with non-digit key is eligible by kind",
			armed: true, afterEnable: true,
			opts:   []PermOption{{Key: "y", Name: "Allow", Kind: "allow"}},
			wantOK: true, wantSelectedKey: "y",
		},
		{
			name:  "unknown kind string is not eligible",
			armed: true, afterEnable: true,
			opts:   []PermOption{{Key: "1", Name: "Future", Kind: "maybe"}},
			wantOK: false,
		},
		{
			name:  "allow_always is never selected even when sole non-reject",
			armed: true, afterEnable: true,
			opts: []PermOption{
				{Key: "1", Name: "Always", Kind: "allow_always"},
				{Key: "2", Name: "Reject", Kind: "reject"},
			},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel, ok := eligibleAutoAllow(tc.armed, tc.afterEnable, tc.opts)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (selected=%+v)", ok, tc.wantOK, sel)
			}
			if tc.wantOK {
				if sel.Key != tc.wantSelectedKey || sel.Kind != "allow" {
					t.Fatalf("selected = %+v, want key=%s kind=allow", sel, tc.wantSelectedKey)
				}
			} else if sel != (PermOption{}) {
				t.Fatalf("ineligible must return zero option, got %+v", sel)
			}
		})
	}
}

// Questions, neutral diagnostics, and non-permission states never pass through
// eligibleAutoAllow because the poller only invokes it on structured Pending
// with options. Document that empty/non-permission inputs are fail-closed.
func TestEligibleAutoAllowNonPermissionInputs(t *testing.T) {
	// No options ≈ question/inspect/dialog with nothing structured to pick.
	if _, ok := eligibleAutoAllow(true, false, nil); ok {
		t.Fatal("nil options must not be eligible")
	}
	// Disarmed lease even with a perfect option.
	if _, ok := eligibleAutoAllow(false, false, []PermOption{{Key: "1", Name: "A", Kind: "allow"}}); ok {
		t.Fatal("disarmed lease must not be eligible")
	}
}
