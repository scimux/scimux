package remote

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"strings"
)

// validatePersistedSemantics checks identity-file field invariants after a
// successful JSON decode. It is the semantic half of load-time validation;
// tmpIncomplete only detects an unreadable or malformed temporary file.
//
// Accepted status classes: enrolled, disabled, revoked, ambiguous, partial,
// key-missing, corrupt. Enrolled/disabled/revoked additionally require
// schema version 1 and a non-empty origin. Enrolled further requires a
// valid handle plus both public and private keys (and forbids a Pending
// record). Missing enrolled keys are ClassKeyMissing rather than generic
// corruption so startup can surface a distinct recovery path instead of
// treating the file as unparsable wreckage. Pending enrollment is legal
// only on ambiguous state, and then only when Pending.PublicKey matches
// the outer PublicKey. Device rows must carry unique IDs/RIDs and valid
// keys. Schema or state additions must update this classifier and its
// table test in validate_test.go — silent acceptance is not an extension
// mechanism.
func validatePersistedSemantics(st PersistedState) Class {
	if st.Status == "" {
		return ClassCorruptIdentity
	}
	switch st.Status {
	case StateEnrolled, StateDisabled, StateRevoked, StateAmbiguous, StatePartial, StateKeyMissing, StateCorrupt:
	default:
		return ClassCorruptIdentity
	}
	if st.V != 0 && st.V != 1 {
		return ClassCorruptIdentity
	}
	if st.Status == StateEnrolled || st.Status == StateDisabled || st.Status == StateRevoked {
		if st.V != 1 {
			return ClassCorruptIdentity
		}
		if st.Origin == "" {
			return ClassCorruptIdentity
		}
	}
	if st.PublicKey != "" {
		raw, err := hex.DecodeString(st.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return ClassCorruptIdentity
		}
	}
	if st.PrivateKey != "" {
		raw, err := hex.DecodeString(st.PrivateKey)
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			return ClassCorruptIdentity
		}
	}
	if st.PublicKey != "" && st.PrivateKey != "" && !ed25519KeysMatch(st.PublicKey, st.PrivateKey) {
		return ClassCorruptIdentity
	}
	if st.Status == StateEnrolled {
		if st.Handle == "" || !validHandle(st.Handle) {
			return ClassCorruptIdentity
		}
		if st.PublicKey == "" || st.PrivateKey == "" {
			return ClassKeyMissing
		}
		if st.Pending != nil {
			return ClassCorruptIdentity
		}
	}
	if st.Status == StateAmbiguous && st.Pending != nil {
		if st.Pending.PublicKey != st.PublicKey {
			return ClassCorruptIdentity
		}
	}
	seenID := map[string]struct{}{}
	seenRID := map[string]struct{}{}
	for _, d := range st.Devices {
		if d.ID == "" {
			return ClassCorruptIdentity
		}
		if _, ok := seenID[d.ID]; ok {
			return ClassCorruptIdentity
		}
		seenID[d.ID] = struct{}{}
		if !validRID(d.RID) {
			return ClassCorruptIdentity
		}
		if _, ok := seenRID[d.RID]; ok {
			return ClassCorruptIdentity
		}
		seenRID[d.RID] = struct{}{}
		raw, err := hex.DecodeString(d.PubKey)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return ClassCorruptIdentity
		}
	}
	return ""
}

func ed25519KeysMatch(pubHex, privHex string) bool {
	pub, err1 := hex.DecodeString(pubHex)
	priv, err2 := hex.DecodeString(privHex)
	if err1 != nil || err2 != nil {
		return false
	}
	if len(pub) != ed25519.PublicKeySize || len(priv) != ed25519.PrivateKeySize {
		return false
	}
	derived := ed25519.PrivateKey(priv).Public().(ed25519.PublicKey)
	return bytes.Equal([]byte(derived), pub)
}

func validHandle(h string) bool {
	if !strings.HasPrefix(h, "ih_") || len(h) != 3+16 {
		return false
	}
	for _, r := range h[3:] {
		if strings.IndexRune(inviteAlphabet, r) < 0 {
			return false
		}
	}
	return true
}

func validRID(rid string) bool {
	if len(rid) != RendezvousIDHexLen || rid != strings.ToLower(rid) {
		return false
	}
	_, err := hex.DecodeString(rid)
	return err == nil
}
