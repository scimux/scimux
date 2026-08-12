package codex

import "testing"

// An unrecognized decision key must not present as a semantic "allow".
//
// Kind started as a presentation hint (it picks the button's role class), but
// the auto-approver promoted it to an authorization predicate: a request whose
// options contain exactly one kind=="allow" is eligible for automatic
// approval. A fail-open default therefore hands scimux's own authority to
// whatever token upstream invents next — the exact hazard the repository's
// defensive-parsing rule exists to survive. Unknown means unknown ("" — the
// same answer mapOptionKind already gives on the ACP side, and the same answer
// applyNetworkPolicyAmendment already gives for a rule it cannot read).
func TestUnknownDecisionKeyIsNotAllow(t *testing.T) {
	for _, key := range []string{
		"futureDecision",
		"acceptOnceV2",        // a renamed once-allow
		"acceptForWorkspace",  // an unrecognized, possibly persistent grant
		"approveButDifferent", // "approve"-shaped but not the known token
	} {
		name, kind := decisionPresentation(Decision{Key: key})
		if name != key {
			t.Errorf("decisionPresentation(%q) name = %q, want the raw key", key, name)
		}
		if kind != "" {
			t.Errorf("decisionPresentation(%q) kind = %q, want \"\" (unknown is not allow)", key, kind)
		}
	}
}

// The kind an unknown key carries decides whether the auto-approver may act,
// so assert the property the way eligibleAutoAllow reads it: the number of
// options presenting as semantic "allow". Both menus below must offer none.
func TestUnknownSoleOptionOffersNoSemanticAllow(t *testing.T) {
	cases := []struct {
		what      string
		decisions []Decision
	}{
		{
			// Upstream renames the once-allow key: the unknown token would
			// otherwise become the sole "allow" and be auto-approved.
			what:      "renamed once-allow",
			decisions: []Decision{{Key: "acceptOnceV2"}, {Key: "acceptForSession"}, {Key: "decline"}},
		},
		{
			// The only non-reject option is unrecognized and may be a
			// persistent grant.
			what:      "unrecognized grant",
			decisions: []Decision{{Key: "acceptForWorkspace"}, {Key: "decline"}},
		},
	}
	for _, tc := range cases {
		s := &Session{pending: []*pendingPermission{{
			seq:      1,
			approval: Approval{Command: "rm -rf /", AvailableDecisions: tc.decisions},
			ch:       make(chan chosen, 1),
		}}}
		p, ok := s.pendingInfo()
		if !ok {
			t.Fatalf("%s: expected a pending permission", tc.what)
		}
		allows := 0
		for _, o := range p.Options {
			if o.Kind == "allow" {
				allows++
			}
		}
		if allows != 0 {
			t.Errorf("%s: %d option(s) present as semantic allow, want 0 — %+v",
				tc.what, allows, p.Options)
		}
	}
}

// Known keys keep their mappings: the fix must narrow only the default branch,
// never make a real approval menu unanswerable or unstyled.
func TestKnownDecisionKindsUnchanged(t *testing.T) {
	want := map[string]string{
		"accept":                        "allow",
		"approved":                      "allow",
		"allowForTurn":                  "allow",
		"acceptForSession":              "allow_always",
		"approved_for_session":          "allow_always",
		"acceptWithExecpolicyAmendment": "allow_always",
		"allowForSession":               "allow_always",
		"decline":                       "reject",
		"denied":                        "reject",
		"cancel":                        "reject",
		"abort":                         "reject",
		"declinePermissions":            "reject",
	}
	for key, kind := range want {
		if _, got := decisionPresentation(Decision{Key: key}); got != kind {
			t.Errorf("decisionPresentation(%q) kind = %q, want %q", key, got, kind)
		}
	}
}
