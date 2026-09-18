package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/scimux/scimux/internal/remote"
)

// hostedPairingClient is the pairing API the HTTP routes call. *remote.Client
// implements it. hostedRemote stays HostedStatus-only and cannot reach pairing.
type hostedPairingClient interface {
	MintPairingCode(context.Context) (remote.PairingCode, error)
	PairingLink(remote.PairingCode) (string, error)
	PairingSession(string) (remote.PairingStatus, error)
	CompletePairing(context.Context, string, bool, bool) (remote.PairedDevice, error)
	CancelPairing(context.Context, string) error
	PairedDevices() ([]remote.PairedDevice, error)
	RevokePairedDevice(context.Context, string) error
	RenamePairedDevice(context.Context, string, string) (remote.PairedDevice, error)
	Unenroll(context.Context) (bool, error)
	HostedStatus() string
	TransportCause(string) (remote.TransportCause, error)
}

var _ hostedPairingClient = (*remote.Client)(nil)

// remoteHandlers is web-generation state. Keeping only a client accessor here
// lets the replaceable web backend own rendezvous/WebRTC without acquiring an
// app pointer (and therefore without acquiring harness or store authority).
type remoteHandlers struct {
	client func() hostedPairingClient
}

func (h remoteHandlers) pairingClient() hostedPairingClient {
	if h.client == nil {
		return nil
	}
	return h.client()
}

func (a *app) remoteHandlers() remoteHandlers {
	return remoteHandlers{client: a.pairingClient}
}

func (a *app) pairingClient() hostedPairingClient {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	p := a.hostedPairing
	a.mu.Unlock()
	return p
}

func (h remoteHandlers) serveRemotePairingMint(w http.ResponseWriter, r *http.Request) {
	p := h.pairingClient()
	if p == nil {
		http.Error(w, "remote pairing is not enabled", http.StatusNotFound)
		return
	}
	if hosted := p.HostedStatus(); !pairingAvailable(hosted) {
		writeRemoteJSON(w, http.StatusConflict, map[string]any{
			"hosted": hosted,
			"error":  hostedPairingRefusal(hosted),
		})
		return
	}
	// Body is ignored: a mint request cannot confirm. FR-12 is a separate
	// explicit confirm call; confirm flags here must not complete pairing.
	code, err := p.MintPairingCode(r.Context())
	if err != nil {
		writeRemotePairingError(w, err)
		return
	}
	// The link is what makes the minted session joinable (rendezvous-v1
	// §11.1). V1 has no typed path, so a code announced without X and the
	// rid is a pairing no browser can enter — the device could not derive
	// the SAS even if the human typed every character correctly.
	//
	// A link that cannot be built therefore fails the request rather than
	// returning the code alone. The session is left to expire: that costs
	// a rate-limit slot, where the alternative sends the user to a page
	// whose only possible message is that the invite is unusable.
	link, err := p.PairingLink(code)
	if err != nil {
		writeRemotePairingError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"code":       code.Code,
		"rid":        code.RID,
		"expires_at": code.ExpiresAt.UTC().Format(time.RFC3339Nano),
		"state":      remote.PairStatePending,
		"link":       link,
	})
}

func (h remoteHandlers) serveRemotePairingState(w http.ResponseWriter, r *http.Request) {
	p := h.pairingClient()
	if p == nil {
		http.Error(w, "remote pairing is not enabled", http.StatusNotFound)
		return
	}
	st, err := p.PairingSession(r.PathValue("code"))
	if err != nil {
		writeRemotePairingError(w, err)
		return
	}
	writeJSON(w, projectPairingStatus(st, p.HostedStatus()))
}

func (h remoteHandlers) serveRemotePairingConfirm(w http.ResponseWriter, r *http.Request) {
	p := h.pairingClient()
	if p == nil {
		http.Error(w, "remote pairing is not enabled", http.StatusNotFound)
		return
	}
	var body struct {
		ComputerConfirm *bool `json:"computer_confirm"`
		DeviceConfirm   *bool `json:"device_confirm"`
	}
	if r.Header.Get("Content-Type") != "" || r.ContentLength > 0 {
		if err := decodeJSON(w, r, &body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
	}
	computer := body.ComputerConfirm != nil && *body.ComputerConfirm
	device := body.DeviceConfirm != nil && *body.DeviceConfirm
	dev, err := p.CompletePairing(r.Context(), r.PathValue("code"), computer, device)
	if err != nil {
		writeRemotePairingError(w, err)
		return
	}
	writeJSON(w, pairedDeviceJSON(dev))
}

func (h remoteHandlers) serveRemotePairingCancel(w http.ResponseWriter, r *http.Request) {
	p := h.pairingClient()
	if p == nil {
		http.Error(w, "remote pairing is not enabled", http.StatusNotFound)
		return
	}
	if err := p.CancelPairing(r.Context(), r.PathValue("code")); err != nil {
		writeRemotePairingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h remoteHandlers) serveRemoteDeviceList(w http.ResponseWriter, r *http.Request) {
	p := h.pairingClient()
	if p == nil {
		http.Error(w, "remote pairing is not enabled", http.StatusNotFound)
		return
	}
	list, err := p.PairedDevices()
	if err != nil {
		writeRemotePairingError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, d := range list {
		out = append(out, pairedDeviceJSON(d))
	}
	writeJSON(w, map[string]any{"devices": out})
}

func (h remoteHandlers) serveRemoteDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	p := h.pairingClient()
	if p == nil {
		http.Error(w, "remote pairing is not enabled", http.StatusNotFound)
		return
	}
	if err := p.RevokePairedDevice(r.Context(), r.PathValue("id")); err != nil {
		writeRemotePairingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveRemoteDeviceRename gives a row the operator's own name for it.
//
// The name on a row arrives from the device in its pair-offer, which makes
// it a claim: nothing stops a phone from calling itself after the laptop
// next to it. Renaming is how the operator replaces that claim with
// something they asserted, and it is safe to allow free-form only because
// the row keeps showing the device's identity underneath. The label is
// bounded and stripped in internal/remote, at the point where it is
// stored, so both suppliers are held to the same rule.
func (h remoteHandlers) serveRemoteDeviceRename(w http.ResponseWriter, r *http.Request) {
	p := h.pairingClient()
	if p == nil {
		http.Error(w, "remote pairing is not enabled", http.StatusNotFound)
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	dev, err := p.RenamePairedDevice(r.Context(), r.PathValue("id"), body.Label)
	if err != nil {
		writeRemotePairingError(w, err)
		return
	}
	writeJSON(w, pairedDeviceJSON(dev))
}

// serveRemoteUnenroll is the way out of an enrollment (rendezvous-v1
// §4.4). The two halves are not equal partners underneath — the local
// unlink always happens, the remote release is best-effort — so this
// route answers 200 with released:false rather than an error when the
// rendezvous could not be reached. That is the honest report: the computer
// is unlinked, and an installation the rendezvous still holds is one only
// its operator can strike off.
//
// A failure of the *local* half is a real error. An identity still on
// disk is still an enrollment, and a UI told otherwise would leave the
// user believing they had unlinked.
func (h remoteHandlers) serveRemoteUnenroll(w http.ResponseWriter, r *http.Request) {
	p := h.pairingClient()
	if p == nil {
		http.Error(w, "remote pairing is not enabled", http.StatusNotFound)
		return
	}
	released, err := p.Unenroll(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The status is read after the unlink so the burger can retire the
	// remote section without a second round trip.
	writeJSON(w, map[string]any{
		"released": released,
		"hosted":   p.HostedStatus(),
	})
}

func (h remoteHandlers) serveRemoteStatus(w http.ResponseWriter, r *http.Request) {
	p := h.pairingClient()
	if p == nil {
		http.Error(w, "remote pairing is not enabled", http.StatusNotFound)
		return
	}
	list, err := p.PairedDevices()
	if err != nil {
		writeRemotePairingError(w, err)
		return
	}
	devices := make([]map[string]any, 0, len(list))
	for _, d := range list {
		devices = append(devices, projectRemoteDeviceCause(p, d.ID))
	}
	// can_pair is the menu's question, answered here so there is one copy
	// of the policy: a browser deciding it from `hosted` would be a second
	// authority, free to drift from the one the mint enforces. The refusal
	// sentence rides along so the menu has something to put where the
	// button was — a control that vanishes without a word reads as a bug.
	hosted := p.HostedStatus()
	out := map[string]any{
		"hosted":   hosted,
		"devices":  devices,
		"can_pair": pairingAvailable(hosted),
	}
	if !pairingAvailable(hosted) {
		out["pair_refusal"] = hostedPairingRefusal(hosted)
	}
	writeJSON(w, out)
}

// handleRemotePairingMint preserves the established monolithic route table as
// a characterization oracle. Production split serving registers the
// corresponding remoteHandlers method in the web child instead.
func (a *app) handleRemotePairingMint(w http.ResponseWriter, r *http.Request) {
	a.remoteHandlers().serveRemotePairingMint(w, r)
}

func (a *app) handleRemotePairingState(w http.ResponseWriter, r *http.Request) {
	a.remoteHandlers().serveRemotePairingState(w, r)
}

func (a *app) handleRemotePairingConfirm(w http.ResponseWriter, r *http.Request) {
	a.remoteHandlers().serveRemotePairingConfirm(w, r)
}

func (a *app) handleRemotePairingCancel(w http.ResponseWriter, r *http.Request) {
	a.remoteHandlers().serveRemotePairingCancel(w, r)
}

func (a *app) handleRemoteDeviceList(w http.ResponseWriter, r *http.Request) {
	a.remoteHandlers().serveRemoteDeviceList(w, r)
}

func (a *app) handleRemoteDeviceRename(w http.ResponseWriter, r *http.Request) {
	a.remoteHandlers().serveRemoteDeviceRename(w, r)
}

func (a *app) handleRemoteDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	a.remoteHandlers().serveRemoteDeviceRevoke(w, r)
}

func (a *app) handleRemoteUnenroll(w http.ResponseWriter, r *http.Request) {
	a.remoteHandlers().serveRemoteUnenroll(w, r)
}

func (a *app) handleRemoteStatus(w http.ResponseWriter, r *http.Request) {
	a.remoteHandlers().serveRemoteStatus(w, r)
}

// projectRemoteDeviceCause is the FR-24 HTTP view of one paired device.
// connected:true with no cause is a live tunnel. connected:false with the
// cause key absent is D2: no attached Session, or a ClassPeerAbsent error,
// is not a cause. guidance is AT-FR-24-c: only ice-failed names a fallback.
func projectRemoteDeviceCause(p hostedPairingClient, id string) map[string]any {
	out := map[string]any{"id": id, "connected": false}
	cause, err := p.TransportCause(id)
	if err != nil {
		return out
	}
	if cause == "" {
		out["connected"] = true
		return out
	}
	out["cause"] = string(cause)
	if g := cause.Guidance(); g != "" {
		out["guidance"] = g
	}
	return out
}

func pairingStatusJSON(st remote.PairingStatus) map[string]any {
	out := map[string]any{
		"state":      st.State,
		"code":       st.Code,
		"rid":        st.RID,
		"expires_at": st.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
	if st.SAS != "" {
		out["sas"] = st.SAS
	}
	if len(st.ComputerPub) > 0 {
		out["computer_pub"] = hex.EncodeToString(st.ComputerPub)
	}
	if len(st.ReplyNonce) > 0 {
		out["reply_nonce"] = hex.EncodeToString(st.ReplyNonce)
	}
	return out
}

// projectPairingStatus maps a live session onto FR-38 for the HTTP
// response. pairingStatusJSON stays a pure struct→JSON projection;
// rewriting a pending/warning session under a dead installation is a
// caller concern.
func projectPairingStatus(st remote.PairingStatus, hosted string) map[string]any {
	out := pairingStatusJSON(st)
	reason := pairingRefusalReason(hosted)
	if reason == "" {
		return out
	}
	switch st.State {
	case remote.PairStatePending, remote.PairStateWarning:
		out["state"] = remote.PairStateFailed
		out["reason"] = reason
	}
	return out
}

// pairingAvailable reports whether this installation is in a state where a
// pairing code could actually be completed. It is an allowlist, and that is
// the point: minting is entirely local — a code, a locally minted RID and a
// link built from the configured origin, with nothing asked of the
// rendezvous — so any state this function does not recognise would otherwise
// hand the user a credential-shaped string, a QR for it, and a wait that can
// only end in an expiry. A state added later must therefore have to argue
// its way in here rather than inherit permission by not being listed.
//
// "enrolled" is the ordinary case. "unavailable" is in for a specific
// reason: it is what Start() records on a *transient* authenticate failure
// (internal/remote/client.go:264), and it returns nil, so scimux comes up
// normally in that state. The only thing that clears it is noteWaitResult,
// which runs solely inside waitLoopRID, which exists solely for a RID in
// liveRIDs() — paired devices plus pairing sessions. A fresh installation
// with no devices paired has neither until a code is minted, and minting is
// what registers the pairing waiter (pairing.go:255).
//
// So for that one state the mint is the recovery path. Refusing it would be
// self-sustaining: one network blip at startup and that user could never
// pair until scimux restarted. The same reasoning keeps a live session out
// of FR-38's terminal "failed" while the outage is merely transient.
//
// Everything else is refused. "revoked" and "disabled" are durable facts
// about authorization. "" is what a computer that has just unlinked reports
// (internal/remote/unenroll.go:116 clears the status along with the identity
// on disk), and the FR-30 broken states — absent, key-missing, partial,
// corrupt, ambiguous — describe an identity the rendezvous would not admit.
// None of them can complete a pairing, so none of them may start one.
func pairingAvailable(hosted string) bool {
	switch hosted {
	case "enrolled", "unavailable":
		return true
	}
	return false
}

// pairingRefusalReason is the machine-readable half of a refusal, and empty
// when there is nothing to refuse. The hosted status is its own reason
// wherever it names something ("revoked", "disabled", "corrupt"); the empty
// status is the one that does not, so it is given a name here rather than
// reaching a UI as a blank string nobody can render.
func pairingRefusalReason(hosted string) string {
	switch {
	case pairingAvailable(hosted):
		return ""
	case hosted == "":
		return "not-enrolled"
	default:
		return hosted
	}
}

func hostedPairingRefusal(hosted string) string {
	switch hosted {
	case "revoked":
		return "This installation has been revoked and can no longer pair devices."
	case "disabled":
		return "Remote access is disabled and pairing is not available."
	case "key-missing", "partial", "corrupt", "ambiguous":
		// The enrollment exists on disk and is not usable. Saying "not
		// enrolled" would send the operator to enroll again, which is not
		// the repair; the state name is what the FR-30 guidance is indexed
		// by, so it goes in the sentence.
		return "This installation's enrollment is not usable (" + hosted +
			") and pairing is not available until it is repaired."
	default:
		return "This installation is not enrolled with a rendezvous, so there is " +
			"nowhere for a device to meet it."
	}
}

// writeRemoteJSON is a status-taking sibling of writeJSON. writeJSON
// has callers outside this file and does not take a code.
func writeRemoteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func pairedDeviceJSON(d remote.PairedDevice) map[string]any {
	out := map[string]any{
		"id":    d.ID,
		"rid":   d.RID,
		"label": d.Label,
	}
	if !d.PairedAt.IsZero() {
		out["paired_at"] = d.PairedAt.UTC().Format(time.RFC3339Nano)
	}
	// PairedDevice.PubKey is the device's static P-256 ECDH key (the offer's
	// DevicePub), not its ed25519 signing identity — pairing.go puts SignPub
	// on DeviceRecord.PubKey instead, and says the two cannot share a field.
	// `public_key` means ed25519 everywhere else here, including the on-disk
	// PersistedDevice, so this material ships under the name that record
	// already gives it.
	if len(d.PubKey) > 0 {
		out["ecdh_public_key"] = hex.EncodeToString(d.PubKey)
	}
	return out
}

func writeRemotePairingError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	var re *remote.Error
	if errors.As(err, &re) {
		switch re.Class {
		case remote.ClassNotFound:
			http.Error(w, re.Error(), http.StatusNotFound)
			return
		case remote.ClassPairExpired:
			http.Error(w, re.Error(), http.StatusGone)
			return
		case remote.ClassPairUnconfirmed, remote.ClassPairConsumed:
			http.Error(w, re.Error(), http.StatusConflict)
			return
		}
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}
