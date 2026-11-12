package team

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
)

// ErrInvalidLink means the join link is not a usable http(s) URL of the
// form BASE/join#CODE. The error text never includes the input.
var ErrInvalidLink = errors.New("team: invalid join link")

const (
	maxJoinLink     = 512
	maxJoinCode     = 64
	joinCodeLetters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789- "
)

// ParseJoinLink splits a join link into a base URL and a join code.
// The base URL is scheme, host and port, with no trailing slash. The code
// is the raw fragment: letters, digits, hyphens and spaces, unchanged.
func ParseJoinLink(s string) (string, string, error) {
	if len(s) > maxJoinLink {
		return "", "", ErrInvalidLink
	}
	s = strings.TrimSpace(s)
	hash := strings.IndexByte(s, '#')
	if hash < 0 {
		return "", "", ErrInvalidLink
	}
	rawBase := s[:hash]
	code := s[hash+1:]
	if !validJoinCode(code) {
		return "", "", ErrInvalidLink
	}
	u, err := url.Parse(rawBase)
	if err != nil || strings.ContainsFunc(rawBase, unicode.IsSpace) {
		return "", "", ErrInvalidLink
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", ErrInvalidLink
	}
	if u.Host == "" || strings.HasSuffix(u.Host, ":") || u.User != nil {
		return "", "", ErrInvalidLink
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", "", ErrInvalidLink
	}
	if u.Path != "/join" {
		return "", "", ErrInvalidLink
	}
	return u.Scheme + "://" + u.Host, code, nil
}

func validJoinCode(code string) bool {
	if code == "" || len(code) > maxJoinCode {
		return false
	}
	for i := 0; i < len(code); i++ {
		if !strings.ContainsRune(joinCodeLetters, rune(code[i])) {
			return false
		}
	}
	return true
}
