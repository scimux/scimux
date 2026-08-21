package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

const (
	authPrefix     = "scimux-rv/auth/v1"
	inviteInputMax = 256
	httpBodyMax    = 2048
	verifyBodyMax  = 64
	waitBodyMax    = 4096
)

func buildAuthMessage(origin string, version int, route, handle string, challenge []byte) []byte {
	var b []byte
	b = append(b, []byte(authPrefix)...)
	b = append(b, 0)
	b = append(b, []byte(origin)...)
	b = append(b, 0)
	b = append(b, []byte(strconv.Itoa(version))...)
	b = append(b, 0)
	b = append(b, []byte(route)...)
	b = append(b, 0)
	b = append(b, []byte(handle)...)
	b = append(b, 0)
	b = append(b, challenge...)
	return b
}

func (c *Client) authenticate(ctx context.Context) error {
	if c.st.Handle == "" {
		return nil
	}
	chal, err := c.postChallenge(ctx)
	if err != nil {
		return err
	}
	return c.postVerify(ctx, chal)
}

func (c *Client) postChallenge(ctx context.Context) ([]byte, error) {
	body, _ := json.Marshal(map[string]any{"v": c.requestV(), "handle": c.st.Handle})
	resp, err := c.postJSON(ctx, "/v1/challenge", body)
	if err != nil {
		return nil, classErrorf(ClassUnavailable, "challenge", "rendezvous is unavailable", err)
	}
	defer resp.Body.Close()
	raw, err := readBounded(resp.Body, httpBodyMax)
	if err != nil {
		return nil, classErrorf(ClassUnavailable, "challenge", "challenge response is malformed", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, classError(ClassRevoked, "challenge", "this installation has been revoked")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classError(ClassUnavailable, "challenge", "unexpected challenge status")
	}
	ct := resp.Header.Get("Content-Type")
	if !acceptMediaType(ct, "application/json") {
		return nil, classError(ClassUnavailable, "challenge", "unexpected challenge content type")
	}
	var cr struct {
		Challenge string      `json:"challenge"`
		V         json.Number `json:"v"`
	}
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, classError(ClassUnavailable, "challenge", "malformed challenge response")
	}
	if v, err := cr.V.Int64(); err != nil || int(v) != ProtocolVersion {
		return nil, classError(ClassUnavailable, "challenge", "unexpected challenge protocol version")
	}
	chal, err := hex.DecodeString(cr.Challenge)
	if err != nil || len(chal) != 32 {
		return nil, classError(ClassUnavailable, "challenge", "invalid challenge")
	}
	return chal, nil
}

func (c *Client) postVerify(ctx context.Context, chal []byte) error {
	msg := buildAuthMessage(c.origin(), ProtocolVersion, "/v1/verify", c.st.Handle, chal)
	sig := ed25519.Sign(c.priv, msg)
	body, _ := json.Marshal(map[string]any{
		"v":         c.requestV(),
		"handle":    c.st.Handle,
		"challenge": hex.EncodeToString(chal),
		"sig":       hex.EncodeToString(sig),
	})
	resp, err := c.postJSON(ctx, "/v1/verify", body)
	if err != nil {
		return classErrorf(ClassUnavailable, "verify", "rendezvous is unavailable", err)
	}
	defer resp.Body.Close()
	raw, boundErr := readBounded(resp.Body, verifyBodyMax)
	if resp.StatusCode == http.StatusNotFound {
		return classError(ClassRevoked, "verify", "this installation has been revoked")
	}
	if resp.StatusCode != http.StatusNoContent {
		return classError(ClassUnavailable, "verify", "unexpected verify status")
	}
	if boundErr != nil {
		return classErrorf(ClassUnavailable, "verify", "verify response is oversized", boundErr)
	}
	if len(raw) != 0 || resp.ContentLength > 0 {
		return classError(ClassUnavailable, "verify", "verify response must be empty")
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" && cl != "0" {
		return classError(ClassUnavailable, "verify", "verify response must be empty")
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.rvBase(), "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	return c.httpClient().Do(req)
}

func readBounded(r io.Reader, max int) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > max {
		return nil, fmt.Errorf("remote: response exceeds %d bytes", max)
	}
	return raw, nil
}

func acceptMediaType(ct, want string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == want
}
