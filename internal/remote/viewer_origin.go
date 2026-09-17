package remote

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// DefaultViewerOrigin is the trusted browser page that receives pairing
// invitations. It is deliberately separate from DefaultOrigin: the latter is
// the rendezvous service identity bound into signatures and pairing transcripts.
const DefaultViewerOrigin = "https://my.scimux.com"

// NormalizeViewerOrigin validates an origin used to deliver the trusted viewer.
// A single trailing slash is accepted for pasted browser URLs and removed.
// Other spellings that a browser would serialize differently are rejected so
// local configuration and the viewer's trusted release configuration compare
// exactly.
func NormalizeViewerOrigin(raw string) (string, error) {
	if raw == "" {
		return DefaultViewerOrigin, nil
	}
	if strings.ContainsAny(raw, "?#") {
		return "", fmt.Errorf("viewer origin %q must not contain a query or fragment", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("viewer origin %q is not a URL: %w", raw, err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("viewer origin %q must use https", raw)
	}
	if u.Host == "" || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return "", fmt.Errorf("viewer origin %q must be an absolute origin without user information", raw)
	}
	if (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return "", fmt.Errorf("viewer origin %q must not contain a path, query, or fragment", raw)
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("viewer origin %q has an invalid port", raw)
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("viewer origin %q has an invalid port", raw)
		}
		if port != strconv.Itoa(n) {
			return "", fmt.Errorf("viewer origin %q has a non-canonical port", raw)
		}
		if n == 443 {
			return "", fmt.Errorf("viewer origin %q must omit the default https port", raw)
		}
	}
	hostname := u.Hostname()
	if strings.Contains(hostname, "%") {
		return "", fmt.Errorf("viewer origin %q must not use an IPv6 zone", raw)
	}
	for _, b := range []byte(hostname) {
		if b > 0x7f {
			return "", fmt.Errorf("viewer origin %q must use an ASCII hostname", raw)
		}
	}
	canonicalHost := strings.ToLower(hostname)
	if ip := net.ParseIP(hostname); ip != nil {
		canonicalHost = ip.String()
		if strings.Contains(canonicalHost, ":") {
			canonicalHost = "[" + canonicalHost + "]"
		}
	} else if browserNumericHostShorthand(hostname) {
		return "", fmt.Errorf("viewer origin %q uses a non-canonical numeric host", raw)
	}
	if port != "" {
		canonicalHost += ":" + port
	}
	canonical := "https://" + canonicalHost
	if raw != canonical && raw != canonical+"/" {
		return "", fmt.Errorf("viewer origin %q is not in canonical form %q", raw, canonical)
	}
	return canonical, nil
}

func browserNumericHostShorthand(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	last := host
	if i := strings.LastIndexByte(host, '.'); i >= 0 {
		last = host[i+1:]
	}
	if last == "" || strings.HasPrefix(last, "0x") {
		return true
	}
	for _, r := range last {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (c *Client) validatedViewerOrigin() (string, error) {
	if c == nil {
		return "", fmt.Errorf("viewer origin: nil client")
	}
	origin, err := NormalizeViewerOrigin(c.cfg.ViewerOrigin)
	if err != nil {
		return "", err
	}
	return origin, nil
}
