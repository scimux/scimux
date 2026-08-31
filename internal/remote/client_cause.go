package remote

// TransportCause reports the FR-24 cause for deviceID's live session.
//
// The app holds channels as the Channel interface, not *Session; this is
// the lookup that type-asserts. A missing channel, a Channel that is not
// a *Session, and a Session whose own TransportCause is ClassPeerAbsent
// are all "no cause" (D2) — never computer-offline, never an invented
// constant. A live session returns ("", nil).
func (c *Client) TransportCause(deviceID string) (TransportCause, error) {
	if c == nil {
		return "", classError(ClassPeerAbsent, "transport-cause", "no attached channel")
	}
	c.mu.Lock()
	ch := c.channels[deviceID]
	c.mu.Unlock()
	s, ok := ch.(*Session)
	if !ok || s == nil {
		return "", classError(ClassPeerAbsent, "transport-cause", "no attached channel")
	}
	return s.TransportCause()
}

// Guidance is the operator-facing sentence for this cause, or empty.
// Only ICE failure names a fallback (AT-FR-24-c).
func (c TransportCause) Guidance() string {
	if c == CauseICEFailed {
		return iceFallbackGuidance
	}
	return ""
}
