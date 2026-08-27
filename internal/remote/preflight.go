package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// preflight proves the rendezvous is reachable and speaks a version this
// build can use, before the invite is read off disk.
//
// The failure it exists to prevent: enrolling is how the client used to
// discover it could not reach the server, and an invite is single-use.
// A typo'd URL, an offline laptop and a blocked port all became
// StateAmbiguous — invite erased, stuck state persisted, recovery only an
// operator's. §4.0 is unauthenticated and stateless precisely so this
// question can be asked for free.
//
// Every failure here returns before the invite file is opened, so the
// user's only copy stays where they put it and a rerun of the same
// command is the whole recovery.
func (c *Client) preflight(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.rvBase(), "/")+"/v1/hello", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return classErrorf(ClassUnreachable, "preflight",
			"could not reach the rendezvous at "+c.origin()+". Nothing was sent and your invite is unchanged; "+
				"check the network or the --rendezvous-url and run the same command again", err)
	}
	raw, rerr := readBounded(resp.Body, httpBodyMax)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// A §7 constant rejection is positive evidence: this really is a
		// rendezvous, it is reachable, and it simply predates §4.0. Every
		// rv that has ever existed accepts v1, so an older deployment is
		// compatible by construction and enrolling against it is safe.
		// Anything else — a proxy error page, a captive portal, a
		// truncated body — proves nothing was reached, and the whole
		// point is not to spend the invite finding out.
		if rerr == nil && isConstantRejection(resp, raw) {
			return nil
		}
		return classError(ClassUnreachable, "preflight",
			"the rendezvous at "+c.origin()+" did not answer the version probe. Nothing was sent and your invite "+
				"is unchanged; check the --rendezvous-url and run the same command again")
	}
	if rerr != nil {
		return classErrorf(ClassUnreachable, "preflight",
			"the rendezvous at "+c.origin()+" answered the version probe with a body that could not be read. "+
				"Nothing was sent and your invite is unchanged", rerr)
	}

	var win struct {
		V   *int `json:"v"`
		Min *int `json:"min"`
	}
	if json.Unmarshal(raw, &win) != nil || win.V == nil || win.Min == nil {
		// Answered, but not as a rendezvous. Naming this a version
		// mismatch would send the user looking for an upgrade that does
		// not exist; what they actually have is the wrong address.
		return classError(ClassUnreachable, "preflight",
			"the address in --rendezvous-url answered, but not as a rendezvous. Nothing was sent and your "+
				"invite is unchanged; check the --rendezvous-url and run the same command again")
	}

	// Both bounds are checked, and the guidance names which side is
	// behind — that is the entire reason §4.0 reports `min` as well as
	// `v`. "Incompatible" alone would leave the user with no idea
	// whether to update scimux or to ask the operator.
	if *win.Min > ProtocolVersion {
		return classError(ClassVersionMismatch, "preflight",
			"the rendezvous has moved on and no longer accepts this version of scimux. Nothing was sent and "+
				"your invite is unchanged; update scimux and run the same command again")
	}
	if *win.V < MinRequestV {
		return classError(ClassVersionMismatch, "preflight",
			"the rendezvous speaks an older protocol than this version of scimux requires. Nothing was sent "+
				"and your invite is unchanged; ask whoever runs it to update, then run the same command again")
	}
	return nil
}
