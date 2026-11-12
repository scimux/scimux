package team

import "errors"

// ErrInvalidLink means the join link is not a usable http(s) URL of the
// form BASE/join#CODE. The error text never includes the input.
var ErrInvalidLink = errors.New("team: invalid join link")

// ParseJoinLink splits a join link into a base URL and a join code.
// The stub accepts nothing; the tests name the contract.
func ParseJoinLink(s string) (baseURL, code string, err error) {
	return "", "", nil
}
