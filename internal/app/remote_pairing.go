package app

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"codeberg.org/chrberger/scimux/internal/remote"
)

// hostedPairingClient is the pairing API the HTTP routes call. *remote.Client
// implements it. hostedRemote stays HostedStatus-only and cannot reach pairing.
type hostedPairingClient interface {
	MintPairingCode(context.Context) (remote.PairingCode, error)
	PairingSession(string) (remote.PairingStatus, error)
	CompletePairing(context.Context, string, bool, bool) (remote.PairedDevice, error)
	CancelPairing(context.Context, string) error
	PairedDevices() ([]remote.PairedDevice, error)
	RevokePairedDevice(context.Context, string) error
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

func (a *app) handleRemotePairingMint(w http.ResponseWriter, r *http.Request) {
	p := a.pairingClient()
	if p == nil {
		http.Error(w, "remote pairing is not enabled", http.StatusNotFound)
		return
	}
	// Body is ignored: a mint request cannot confirm. FR-12 is a separate
	// explicit confirm call; confirm flags here must not complete pairing.
	code, err := p.MintPairingCode(r.Context())
	if err != nil {
		writeRemotePairingError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"code":       code.Code,
		"rid":        code.RID,
		"expires_at": code.ExpiresAt.UTC().Format(time.RFC3339Nano),
		"state":      remote.PairStatePending,
	})
}

func (a *app) handleRemotePairingState(w http.ResponseWriter, r *http.Request) {
	p := a.pairingClient()
	if p == nil {
		http.Error(w, "remote pairing is not enabled", http.StatusNotFound)
		return
	}
	st, err := p.PairingSession(r.PathValue("code"))
	if err != nil {
		writeRemotePairingError(w, err)
		return
	}
	writeJSON(w, pairingStatusJSON(st))
}

func (a *app) handleRemotePairingConfirm(w http.ResponseWriter, r *http.Request) {
	p := a.pairingClient()
	if p == nil {
		http.Error(w, "remote pairing is not enabled", http.StatusNotFound)
		return
	}
	var body struct {
		LaptopConfirm *bool `json:"laptop_confirm"`
		DeviceConfirm *bool `json:"device_confirm"`
	}
	if r.Header.Get("Content-Type") != "" || r.ContentLength > 0 {
		if err := decodeJSON(w, r, &body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
	}
	laptop := body.LaptopConfirm != nil && *body.LaptopConfirm
	device := body.DeviceConfirm != nil && *body.DeviceConfirm
	dev, err := p.CompletePairing(r.Context(), r.PathValue("code"), laptop, device)
	if err != nil {
		writeRemotePairingError(w, err)
		return
	}
	writeJSON(w, pairedDeviceJSON(dev))
}

func (a *app) handleRemotePairingCancel(w http.ResponseWriter, r *http.Request) {
	p := a.pairingClient()
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

func (a *app) handleRemoteDeviceList(w http.ResponseWriter, r *http.Request) {
	p := a.pairingClient()
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

func (a *app) handleRemoteDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	p := a.pairingClient()
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
	if len(st.LaptopPub) > 0 {
		out["laptop_pub"] = hex.EncodeToString(st.LaptopPub)
	}
	if len(st.ReplyNonce) > 0 {
		out["reply_nonce"] = hex.EncodeToString(st.ReplyNonce)
	}
	return out
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
	if len(d.PubKey) > 0 {
		out["public_key"] = hex.EncodeToString(d.PubKey)
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
