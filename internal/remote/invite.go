package remote

import (
	"errors"
	"strings"
	"unicode"
)

const inviteAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func parseInvite(s string) (string, error) {
	var b strings.Builder
	b.Grow(24)
	for _, r := range s {
		switch r {
		case '-', ' ':
			continue
		case 'o', 'O':
			r = '0'
		case 'i', 'I', 'l', 'L':
			r = '1'
		default:
			r = unicode.ToUpper(r)
		}
		if strings.IndexRune(inviteAlphabet, r) < 0 {
			return "", errors.New("invalid invite code")
		}
		b.WriteRune(r)
	}
	if b.Len() != 24 {
		return "", errors.New("invalid invite code")
	}
	return b.String(), nil
}
