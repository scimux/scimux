package codec

import (
	"net/http"
	"strings"
	"unicode"
)

func containsCRLF(s string) bool {
	return strings.ContainsAny(s, "\r\n")
}

func isAbsoluteForm(path string) bool {
	colon := strings.Index(path, "://")
	if colon <= 0 {
		return false
	}
	for i := 0; i < colon; i++ {
		c := rune(path[i])
		if i == 0 {
			if !unicode.IsLetter(c) {
				return false
			}
			continue
		}
		if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '+' || c == '-' || c == '.' {
			continue
		}
		return false
	}
	return true
}

func headerHas(h http.Header, name string) bool {
	if h == nil {
		return false
	}
	canon := http.CanonicalHeaderKey(name)
	if vs, ok := h[canon]; ok && len(vs) > 0 {
		return true
	}
	for k, vs := range h {
		if http.CanonicalHeaderKey(k) == canon && len(vs) > 0 {
			return true
		}
	}
	return false
}

func filterHeaders(h http.Header, allow map[string]struct{}) (http.Header, error) {
	if h == nil {
		return make(http.Header), nil
	}
	for name, vals := range h {
		if containsCRLF(name) {
			return nil, reject(ClassCRLF, "header-name")
		}
		for _, v := range vals {
			if containsCRLF(v) {
				return nil, reject(ClassCRLF, "header-value")
			}
		}
	}
	if headerHas(h, "Content-Length") && headerHas(h, "Transfer-Encoding") {
		return nil, reject(ClassLengthConflict, "header-name")
	}
	for name := range h {
		switch http.CanonicalHeaderKey(name) {
		case "Host":
			return nil, reject(ClassAuthority, "header-name")
		case "Upgrade":
			return nil, reject(ClassUpgrade, "header-name")
		case "Trailer":
			return nil, reject(ClassTrailer, "header-name")
		case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
			"Proxy-Connection", "Te", "Transfer-Encoding":
			return nil, reject(ClassHopByHop, "header-name")
		}
	}
	out := make(http.Header)
	for name, vals := range h {
		canon := http.CanonicalHeaderKey(name)
		if _, ok := allow[canon]; !ok {
			continue
		}
		cp := make([]string, len(vals))
		copy(cp, vals)
		out[canon] = cp
	}
	return out, nil
}

func validateRequest(req *Request) (http.Header, error) {
	if req == nil {
		return nil, reject(ClassMalformed, "")
	}
	if _, ok := allowedMethods[req.Method]; !ok {
		return nil, reject(ClassMethod, "")
	}
	if containsCRLF(req.Path) {
		return nil, reject(ClassCRLF, "path")
	}
	if containsCRLF(req.Query) {
		return nil, reject(ClassCRLF, "query")
	}
	if isAbsoluteForm(req.Path) {
		return nil, reject(ClassAbsoluteURI, "path")
	}
	if strings.HasPrefix(req.Path, "//") {
		return nil, reject(ClassAuthority, "path")
	}
	return filterHeaders(req.Headers, allowedRequestHeaders)
}

func validateResponseHeaders(h http.Header) (http.Header, error) {
	return filterHeaders(h, allowedResponseHeaders)
}

// maxRejectClassLen bounds a class this build has never heard of. Every
// class in tunnel-v2 §5 is well under it; the bound exists because the
// string is a peer's, not because a longer name is meaningful.
const maxRejectClassLen = 40

// plausibleRejectClass reports whether an unrecognised class string is
// shaped like one a future minor version could legitimately define:
// a short lowercase hyphenated token. It is a grammar check on untrusted
// text, not a guess at meaning — see acceptReject.
func plausibleRejectClass(c Class) bool {
	if len(c) == 0 || len(c) > maxRejectClassLen {
		return false
	}
	if c[0] == '-' || c[len(c)-1] == '-' {
		return false
	}
	for i := 0; i < len(c); i++ {
		ch := c[i]
		switch {
		case ch >= 'a' && ch <= 'z':
		case ch >= '0' && ch <= '9':
		case ch == '-':
		default:
			return false
		}
	}
	return true
}

func knownRejectClass(c Class) bool {
	switch c {
	case ClassAbsoluteURI, ClassAuthority, ClassHopByHop, ClassUpgrade,
		ClassTrailer, ClassLengthConflict, ClassCRLF, ClassMethod,
		ClassBodyTooLarge, ClassMalformed, ClassTruncated:
		return true
	default:
		return false
	}
}
