package remote

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

const atNFR14a = "AT-NFR-14-a"
const atNFR14b = "AT-NFR-14-b"

// TestAT_NFR_14_a_SpecExistsAndIsVersioned — the checked-in protocol
// specification exists and carries integer Protocol-Version / Spec-Status
// / minimum request v. Absence of the spec fails CI. Version identity is
// the spec header itself; there is no parallel spec_version binding.
func TestAT_NFR_14_a_SpecExistsAndIsVersioned(t *testing.T) {
	_, meta := loadSpec(t, atNFR14a)
	if meta.ProtocolVersion < 1 {
		t.Fatalf("%s: Protocol-Version %d, want >= 1", atNFR14a, meta.ProtocolVersion)
	}
	if meta.SpecStatus < 1 {
		t.Fatalf("%s: Spec-Status %d, want >= 1", atNFR14a, meta.SpecStatus)
	}
	if meta.MinRequestV != 1 {
		t.Fatalf("%s: Minimum accepted request v is %d, want 1", atNFR14a, meta.MinRequestV)
	}
}

// TestAT_NFR_14_a_VectorDigestMatches — SHA-256 over the exact bytes of
// every JSON file, sorted by base name, in `hex  filename\n` form, must
// equal vectors.sha256 byte-for-byte. Missing, extra, duplicate,
// malformed, or unlisted files and digest entries fail.
func TestAT_NFR_14_a_VectorDigestMatches(t *testing.T) {
	assertVectorDigest(t, atNFR14a)
}

// TestAT_NFR_14_a_VectorsExecute — every vector is loaded and executed.
// Construction kinds are recomputed with the Go standard library from
// the specification. HTTP/rejection rows are parsed and structurally
// validated; they cannot disappear through a default switch arm.
// Unknown kinds fail.
func TestAT_NFR_14_a_VectorsExecute(t *testing.T) {
	_, meta := loadSpec(t, atNFR14a)
	assertVectorDigest(t, atNFR14a)
	vectors := loadVectors(t, atNFR14a)
	accounted := make(map[string]string, len(vectors))
	for i, v := range vectors {
		name := v.ID
		if name == "" {
			t.Fatalf("%s: vector %d has no id", atNFR14a, i)
		}
		t.Run(name, func(t *testing.T) {
			executeVector(t, v, meta)
		})
		accounted[v.ID] = v.Kind
	}
	if len(accounted) != len(vectors) {
		t.Fatalf("%s: accounted %d vector ids, loaded %d", atNFR14a, len(accounted), len(vectors))
	}
	if len(accounted) == 0 {
		t.Fatalf("%s: vector set is empty; nothing to execute", atNFR14a)
	}
}

// TestAT_NFR_14_a_VersionNegotiation pins FR-37 as the spec states it:
// request `v` is a JSON number; below-minimum `v` is rejection-version;
// the signed-message version segment and any response `v` are the spec
// Protocol-Version server constant. This is not a spec_version equality
// check on the vector files.
func TestAT_NFR_14_a_VersionNegotiation(t *testing.T) {
	body, meta := loadSpec(t, atNFR14a)
	section := specSection(body, "version negotiation")
	if section == "" {
		t.Fatalf("%s: spec has no Version negotiation section", atNFR14a)
	}
	lower := strings.ToLower(section)
	if !strings.Contains(lower, "rejection-version") {
		t.Fatalf("%s: version negotiation section does not name rejection-version", atNFR14a)
	}
	if !strings.Contains(lower, "json number") {
		t.Fatalf("%s: version negotiation section does not state that request v is a JSON number", atNFR14a)
	}
	if !strings.Contains(lower, "server constant") {
		t.Fatalf("%s: version negotiation section does not pin the response/signed v to the server constant", atNFR14a)
	}

	vectors := loadVectors(t, atNFR14a)
	var versionRejects int
	var authMessages int
	for _, v := range vectors {
		t.Run(v.ID, func(t *testing.T) {
			if v.Kind == "auth-message" {
				authMessages++
				executeAuthMessage(t, v, meta)
			}
			if v.Kind == "http" || v.Kind == "rejection" {
				pinVersionNegotiationHTTP(t, v, meta)
			}
			if v.Message == "rejection-version" {
				versionRejects++
				if v.Request == nil {
					t.Fatal("rejection-version needs a request")
				}
				reqV, numeric, present := jsonNumberV(mustHTTPBody(t, v.Request))
				if !present {
					t.Fatal("rejection-version request has no v field")
				}
				if numeric && reqV >= float64(meta.MinRequestV) {
					t.Fatalf("rejection-version request v %v is not below minimum %d", reqV, meta.MinRequestV)
				}
			}
		})
	}
	if authMessages == 0 {
		t.Fatalf("%s: no auth-message vector pins the signed Protocol-Version", atNFR14a)
	}
	if versionRejects == 0 {
		t.Fatalf("%s: no rejection-version vector pins below-minimum request v", atNFR14a)
	}
}

// TestAT_NFR_14_a_SyncProcedureDocument — an rv vector regeneration
// requires byte-copying the specification, digest, and all vector JSON
// from a named committed rv revision identified by a concrete Git
// OID, then running both Go and WebCrypto verification. Reformatting
// or reserialising JSON is forbidden.
func TestAT_NFR_14_a_SyncProcedureDocument(t *testing.T) {
	raw := mustReadRel(t, syncRelPath, atNFR14a)
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		t.Fatalf("%s: %s is empty", atNFR14a, syncRelPath)
	}
	lower := strings.ToLower(text)
	compact := strings.NewReplacer("-", " ", "_", " ", "\n", " ", "\t", " ").Replace(lower)
	for strings.Contains(compact, "  ") {
		compact = strings.ReplaceAll(compact, "  ", " ")
	}

	required := []struct {
		name string
		ok   bool
	}{
		{"rendezvous-v1.md", strings.Contains(lower, "rendezvous-v1.md")},
		{"vectors.sha256", strings.Contains(lower, "vectors.sha256")},
		{"vector JSON", strings.Contains(lower, ".json")},
		{"byte copy", strings.Contains(compact, "byte copy") || strings.Contains(compact, "bytecopy")},
		{"named committed rv revision", hasRvSourceOID(text)},
		{"Go verification", regexp.MustCompile(`\bgo\b`).MatchString(compact) && strings.Contains(compact, "verif")},
		{"WebCrypto verification", strings.Contains(compact, "webcrypto")},
		{"reformatting/reserialising forbidden", (strings.Contains(compact, "reformat") || strings.Contains(compact, "reserial")) && (strings.Contains(compact, "forbid") || strings.Contains(compact, "must not") || strings.Contains(compact, "never"))},
	}
	for _, row := range required {
		if !row.ok {
			t.Errorf("%s: %s must state %s", atNFR14a, syncRelPath, row.name)
		}
	}
}

// TestAT_NFR_14_b_EveryMessageHasAVector — every catalog id parsed from
// the specification has at least one vector, and every vector's
// `message` names a catalog id. The catalog is not duplicated as a
// hand-maintained Go list.
func TestAT_NFR_14_b_EveryMessageHasAVector(t *testing.T) {
	body, _ := loadSpec(t, atNFR14b)
	catalog := parseMessageCatalog(t, body)
	if len(catalog) == 0 {
		t.Fatalf("%s: spec message catalog is empty", atNFR14b)
	}
	vectors := loadVectors(t, atNFR14b)
	covered := make(map[string]int)
	for _, v := range vectors {
		if v.Message == "" {
			t.Errorf("vector %q has no message field", v.ID)
			continue
		}
		if _, ok := catalog[v.Message]; !ok {
			t.Errorf("vector %q names message %q, which is not in the spec catalog", v.ID, v.Message)
			continue
		}
		covered[v.Message]++
	}
	var missing []string
	for id := range catalog {
		if covered[id] == 0 {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%s: %d catalog message(s) have no vector: %s", atNFR14b, len(missing), strings.Join(missing, ", "))
	}
}

// gitOIDRe matches a Git object id abbreviation or full SHA-1
// (7–40 hex). SHA-256 object ids are 64 hex and are not accepted:
// this document names an rv revision, not a vector digest.
var gitOIDRe = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)

// hasRvSourceOID reports whether text names a concrete Git OID as
// the rv source revision. The OID is discovered, not compared to a
// fixed hash, so a later synchronized revision can replace the
// current one without changing this test.
func hasRvSourceOID(text string) bool {
	lower := strings.ToLower(text)
	locs := gitOIDRe.FindAllStringIndex(lower, -1)
	if len(locs) == 0 {
		return false
	}
	rvWord := regexp.MustCompile(`\brv\b`)
	for _, loc := range locs {
		start := loc[0] - 160
		if start < 0 {
			start = 0
		}
		end := loc[1] + 160
		if end > len(lower) {
			end = len(lower)
		}
		window := lower[start:end]
		if !rvWord.MatchString(window) {
			continue
		}
		if strings.Contains(window, "commit") || strings.Contains(window, "revision") {
			return true
		}
	}
	return false
}

func specSection(body []byte, headingNeedle string) string {
	lines := strings.Split(string(body), "\n")
	needle := strings.ToLower(headingNeedle)
	start := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") && strings.Contains(strings.ToLower(trimmed), needle) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}
