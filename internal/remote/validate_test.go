package remote

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
)

func TestValidatePersistedSemantics(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(pub)
	privHex := hex.EncodeToString(priv)
	handle := "ih_041061050R3GG28A"
	rid1 := strings.Repeat("ab", 32)
	rid2 := strings.Repeat("cd", 32)
	if len(rid1) != RendezvousIDHexLen || len(rid2) != RendezvousIDHexLen {
		t.Fatalf("fixture RID length: got %d/%d want %d", len(rid1), len(rid2), RendezvousIDHexLen)
	}

	enrolled := PersistedState{
		V: 1, Status: StateEnrolled, Handle: handle,
		PublicKey: pubHex, PrivateKey: privHex, Origin: DefaultOrigin,
	}
	validDevice := PersistedDevice{ID: "phone", RID: rid1, PubKey: pubHex}

	type row struct {
		name string
		st   PersistedState
		want Class
	}
	rows := []row{
		// Valid accepted states.
		{"valid enrolled", enrolled, ""},
		{"valid disabled", PersistedState{V: 1, Status: StateDisabled, Origin: DefaultOrigin}, ""},
		{"valid revoked", PersistedState{V: 1, Status: StateRevoked, Origin: DefaultOrigin}, ""},
		{"valid ambiguous", PersistedState{Status: StateAmbiguous}, ""},
		{"valid partial", PersistedState{Status: StatePartial}, ""},
		{"valid key-missing", PersistedState{Status: StateKeyMissing}, ""},
		{"valid corrupt", PersistedState{Status: StateCorrupt}, ""},
		{"valid enrolled with device", func() PersistedState {
			st := enrolled
			st.Devices = []PersistedDevice{validDevice}
			return st
		}(), ""},
		{"valid ambiguous matching pending", PersistedState{
			Status: StateAmbiguous, PublicKey: pubHex,
			Pending: &PendingEnrollment{PublicKey: pubHex},
		}, ""},

		// Invalid: status / version / origin.
		{"empty status", PersistedState{}, ClassCorruptIdentity},
		{"unknown status", PersistedState{Status: "weird"}, ClassCorruptIdentity},
		{"unsupported version", func() PersistedState {
			st := enrolled
			st.V = 2
			return st
		}(), ClassCorruptIdentity},
		{"enrolled without version", func() PersistedState {
			st := enrolled
			st.V = 0
			return st
		}(), ClassCorruptIdentity},
		{"disabled without version", PersistedState{Status: StateDisabled, Origin: DefaultOrigin}, ClassCorruptIdentity},
		{"revoked without version", PersistedState{Status: StateRevoked, Origin: DefaultOrigin}, ClassCorruptIdentity},
		{"enrolled without origin", func() PersistedState {
			st := enrolled
			st.Origin = ""
			return st
		}(), ClassCorruptIdentity},
		{"disabled without origin", PersistedState{V: 1, Status: StateDisabled}, ClassCorruptIdentity},
		{"revoked without origin", PersistedState{V: 1, Status: StateRevoked}, ClassCorruptIdentity},

		// Keys.
		{"malformed public key", func() PersistedState {
			st := enrolled
			st.PublicKey = "zz"
			return st
		}(), ClassCorruptIdentity},
		{"wrong-length public key", func() PersistedState {
			st := enrolled
			st.PublicKey = "ab"
			return st
		}(), ClassCorruptIdentity},
		{"malformed private key", func() PersistedState {
			st := enrolled
			st.PrivateKey = "zz"
			return st
		}(), ClassCorruptIdentity},
		{"wrong-length private key", func() PersistedState {
			st := enrolled
			st.PrivateKey = "abcd"
			return st
		}(), ClassCorruptIdentity},
		{"non-corresponding keys", func() PersistedState {
			st := enrolled
			pub2, _, err := ed25519.GenerateKey(nil)
			if err != nil {
				t.Fatal(err)
			}
			st.PublicKey = hex.EncodeToString(pub2)
			return st
		}(), ClassCorruptIdentity},

		// Enrolled-specific.
		{"enrolled missing handle", func() PersistedState {
			st := enrolled
			st.Handle = ""
			return st
		}(), ClassCorruptIdentity},
		{"enrolled invalid handle", func() PersistedState {
			st := enrolled
			st.Handle = "not-a-handle"
			return st
		}(), ClassCorruptIdentity},
		{"enrolled missing public key", func() PersistedState {
			st := enrolled
			st.PublicKey = ""
			return st
		}(), ClassKeyMissing},
		{"enrolled missing private key", func() PersistedState {
			st := enrolled
			st.PrivateKey = ""
			return st
		}(), ClassKeyMissing},
		{"enrolled with pending", func() PersistedState {
			st := enrolled
			st.Pending = &PendingEnrollment{PublicKey: pubHex}
			return st
		}(), ClassCorruptIdentity},
		{"ambiguous pending public key disagrees", PersistedState{
			Status: StateAmbiguous, PublicKey: pubHex,
			Pending: &PendingEnrollment{PublicKey: hex.EncodeToString(make([]byte, ed25519.PublicKeySize))},
		}, ClassCorruptIdentity},

		// Devices.
		{"device empty id", func() PersistedState {
			st := enrolled
			st.Devices = []PersistedDevice{{ID: "", RID: rid1, PubKey: pubHex}}
			return st
		}(), ClassCorruptIdentity},
		{"device duplicate id", func() PersistedState {
			st := enrolled
			st.Devices = []PersistedDevice{
				{ID: "phone", RID: rid1, PubKey: pubHex},
				{ID: "phone", RID: rid2, PubKey: pubHex},
			}
			return st
		}(), ClassCorruptIdentity},
		{"invalid rendezvous id", func() PersistedState {
			st := enrolled
			st.Devices = []PersistedDevice{{ID: "phone", RID: "not-a-rid", PubKey: pubHex}}
			return st
		}(), ClassCorruptIdentity},
		{"uppercase rendezvous id", func() PersistedState {
			st := enrolled
			st.Devices = []PersistedDevice{{ID: "phone", RID: strings.ToUpper(rid1), PubKey: pubHex}}
			return st
		}(), ClassCorruptIdentity},
		{"duplicate rendezvous id", func() PersistedState {
			st := enrolled
			st.Devices = []PersistedDevice{
				{ID: "phone", RID: rid1, PubKey: pubHex},
				{ID: "tablet", RID: rid1, PubKey: pubHex},
			}
			return st
		}(), ClassCorruptIdentity},
		{"malformed device public key", func() PersistedState {
			st := enrolled
			st.Devices = []PersistedDevice{{ID: "phone", RID: rid1, PubKey: "zz"}}
			return st
		}(), ClassCorruptIdentity},
		{"wrong-length device public key", func() PersistedState {
			st := enrolled
			st.Devices = []PersistedDevice{{ID: "phone", RID: rid1, PubKey: "ab"}}
			return st
		}(), ClassCorruptIdentity},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			got := validatePersistedSemantics(tc.st)
			if got != tc.want {
				t.Fatalf("validatePersistedSemantics() = %q, want %q", got, tc.want)
			}
		})
	}
}
