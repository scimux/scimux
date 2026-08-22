package remote

// S6 — the FR-24 transport probes.
//
// AT-FR-24-a insists that "ICE failed" and "connected then lost" are
// distinct states, and the defect being guarded is conflating them into one
// "unreachable". Producing those two errors from a constant would satisfy
// the letter of that row and none of its point, so both are provoked out of
// the real stack: one pair that genuinely cannot find a path, and one pair
// that genuinely connects and is then dropped.

import (
	"context"
	"net"
	"time"

	"github.com/pion/webrtc/v4"
)

// probeKind selects which failure to provoke.
type probeKind int

const (
	// transportProbeNoCandidates filters every local interface away, so
	// neither peer can gather a candidate and ICE has nothing to check.
	transportProbeNoCandidates probeKind = iota
	// transportProbeDropAfterOpen negotiates normally, waits for the data
	// channel to open, and then closes the far peer.
	transportProbeDropAfterOpen
)

// probeResult is what the probe observed. It is deliberately not an error:
// the caller decides the class, and a probe that reached the wrong state
// must not be dressed up as the state it was asked for.
type probeResult int

const (
	probeInconclusive probeResult = iota
	probeICEFailed
	probeLostAfterOpen
)

// probeICEFailedAfter is how long the real agent is given before it must
// declare failure. ICE's production timeouts are tens of seconds; the probe
// is asking a question whose answer is already determined (there are no
// candidates), so it shortens the clock rather than the mechanism.
const (
	probeICEFailedAfter       = 2 * time.Second
	probeICEDisconnectedAfter = 1 * time.Second
	probeICEKeepalive         = 500 * time.Millisecond
)

// unroutableProbeCandidate is TEST-NET-1 (RFC 5737), reserved for
// documentation and never routed. Giving the agent one remote candidate it
// can never reach is what moves it from "gathering" into "checking", so the
// failure it reports is a real connectivity failure rather than an agent
// that never started.
const unroutableProbeCandidate = "candidate:1 1 udp 2130706431 192.0.2.1 9 typ host"

func probeTransport(ctx context.Context, kind probeKind) (probeResult, error) {
	switch kind {
	case transportProbeNoCandidates:
		return probeICEFailure(ctx)
	case transportProbeDropAfterOpen:
		return probeChannelLoss(ctx)
	default:
		return probeInconclusive, classError(ClassHandshake, "transport", "unknown transport probe")
	}
}

// probeAPI builds an API whose agent cannot see any local interface. The
// filter is the mechanism: gathering runs for real and finds nothing.
func probeAPI(blind bool) *webrtc.API {
	var se webrtc.SettingEngine
	se.SetICETimeouts(probeICEDisconnectedAfter, probeICEFailedAfter, probeICEKeepalive)
	if blind {
		se.SetInterfaceFilter(func(string) bool { return false })
		se.SetIPFilter(func(net.IP) bool { return false })
	}
	return webrtc.NewAPI(webrtc.WithSettingEngine(se))
}

func probeICEFailure(ctx context.Context) (probeResult, error) {
	api := probeAPI(true)
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "could not create the probe peer", err)
	}
	defer func() { _ = pc.Close() }()

	failed := make(chan struct{})
	var closeOnce onceCloser
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		if s == webrtc.ICEConnectionStateFailed {
			closeOnce.close(failed)
		}
	})

	if _, err := pc.CreateDataChannel(tunnelChannelLabel, nil); err != nil {
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "could not create the probe channel", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "could not create the probe offer", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "could not apply the probe offer", err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "the probe never finished gathering", ctx.Err())
	}

	// An answer built by the same blind API: it too has no candidates, so
	// the only thing either side can check is the unroutable candidate fed
	// in below.
	answerPC, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "could not create the probe answerer", err)
	}
	defer func() { _ = answerPC.Close() }()
	if err := answerPC.SetRemoteDescription(*pc.LocalDescription()); err != nil {
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "the probe offer was refused", err)
	}
	answer, err := answerPC.CreateAnswer(nil)
	if err != nil {
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "could not create the probe answer", err)
	}
	answerGathered := webrtc.GatheringCompletePromise(answerPC)
	if err := answerPC.SetLocalDescription(answer); err != nil {
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "could not apply the probe answer", err)
	}
	select {
	case <-answerGathered:
	case <-ctx.Done():
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "the probe answer never finished gathering", ctx.Err())
	}
	if err := pc.SetRemoteDescription(*answerPC.LocalDescription()); err != nil {
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "the probe answer was refused", err)
	}
	if err := pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: unroutableProbeCandidate}); err != nil {
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "could not seed the probe candidate", err)
	}

	select {
	case <-failed:
		return probeICEFailed, nil
	case <-ctx.Done():
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "ICE neither connected nor failed", ctx.Err())
	}
}

func probeChannelLoss(ctx context.Context) (probeResult, error) {
	pair, err := negotiatePair(ctx, probeAPI(false), nil)
	if err != nil {
		return probeInconclusive, err
	}
	local, remote, dc := pair.client, pair.laptop, pair.clientDC
	defer func() {
		_ = local.Close()
		_ = remote.Close()
	}()
	if dc.ReadyState() != webrtc.DataChannelStateOpen {
		return probeInconclusive, classError(ClassHandshake, "transport", "the probe channel never opened")
	}

	lost := make(chan struct{})
	var closeOnce onceCloser
	dc.OnClose(func() { closeOnce.close(lost) })
	local.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			closeOnce.close(lost)
		}
	})

	// Drop the far peer only after the channel is proven open. This is the
	// "connected" half of connected-then-lost; without it the two FR-24
	// states would be indistinguishable at the source.
	if err := remote.Close(); err != nil {
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "could not drop the far probe peer", err)
	}
	select {
	case <-lost:
		return probeLostAfterOpen, nil
	case <-ctx.Done():
		return probeInconclusive, classErrorf(ClassHandshake, "transport", "the dropped peer was never noticed", ctx.Err())
	}
}
