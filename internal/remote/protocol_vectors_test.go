package remote

// S1 (remote refactor) — helpers for AT-NFR-14-a / AT-NFR-14-b.
//
// Gate B: execute the vendored protocol spec and its vectors. No product
// behaviour lives here. Constructions use the Go standard library against
// the specification; HTTP/rejection rows are structurally validated
// rather than replayed through an rv handler. The message catalog is
// parsed from the specification itself — it is not a hand-maintained list.
//
// Version negotiation is the spec's Protocol-Version and request `v`
// rules. There is no parallel spec_version field on the vectors: one
// digest binds both sides.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

const (
	specRelPath   = "docs/protocol/rendezvous-v1.md"
	vectorRelDir  = "internal/remote/testdata/vectors"
	digestRelPath = "internal/remote/testdata/vectors.sha256"
	syncRelPath   = "docs/rendezvous-protocol-sync.md"

	specVersionKey = "Protocol-Version:"
	specStatusKey  = "Spec-Status:"

	specAuthPrefix        = "scimux-rv/auth/v1"
	specRejectBody        = "not found\n"
	specCrockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

	specSTUNHeaderLen      = 20
	specSTUNBindingRequest = 0x0001
	specSTUNBindingSuccess = 0x0101
	specSTUNMagicCookie    = 0x2112A442
	specSTUNXORMapped      = 0x0020
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %q has no go.mod: %v", root, err)
	}
	return root
}

func mustReadRel(t *testing.T, rel, at string) []byte {
	t.Helper()
	path := filepath.Join(repoRoot(t), rel)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %s is absent: %v", at, rel, err)
	}
	return body
}

type specMeta struct {
	ProtocolVersion int
	SpecStatus      int
	MinRequestV     int
}

func loadSpec(t *testing.T, at string) ([]byte, specMeta) {
	t.Helper()
	body := mustReadRel(t, specRelPath, at)
	meta, err := parseSpecMeta(body)
	if err != nil {
		t.Fatalf("%s: %s is not versioned: %v", at, specRelPath, err)
	}
	return body, meta
}

func parseSpecMeta(body []byte) (specMeta, error) {
	var meta specMeta
	ver, ok := specHeaderInt(body, specVersionKey)
	if !ok {
		return meta, fmt.Errorf("no integer %s", specVersionKey)
	}
	status, ok := specHeaderInt(body, specStatusKey)
	if !ok {
		return meta, fmt.Errorf("no integer %s", specStatusKey)
	}
	minV, ok := specMinimumRequestV(body)
	if !ok {
		return meta, fmt.Errorf("no integer Minimum accepted request v")
	}
	if minV < 1 {
		return meta, fmt.Errorf("Minimum accepted request v is %d, want >= 1", minV)
	}
	if ver < minV {
		return meta, fmt.Errorf("Protocol-Version %d is below minimum request v %d", ver, minV)
	}
	meta.ProtocolVersion = ver
	meta.SpecStatus = status
	meta.MinRequestV = minV
	return meta, nil
}

func specHeaderInt(body []byte, key string) (int, bool) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "**")
		if !strings.HasPrefix(line, key) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, key))
		rest = strings.Trim(rest, "*")
		rest = strings.TrimSpace(rest)
		n, err := strconv.Atoi(rest)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func specMinimumRequestV(body []byte) (int, bool) {
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.Trim(trimmed, "*")
		trimmed = strings.TrimSpace(trimmed)
		lower := strings.ToLower(trimmed)
		if !strings.HasPrefix(lower, "minimum accepted request") {
			continue
		}
		i := strings.LastIndexAny(trimmed, "0123456789")
		if i < 0 {
			return 0, false
		}
		start := i
		for start > 0 && unicode.IsDigit(rune(trimmed[start-1])) {
			start--
		}
		n, err := strconv.Atoi(trimmed[start : i+1])
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

type catalogEntry struct {
	ID    string
	Class string
	Route string
	Order int
}

func parseMessageCatalog(t *testing.T, body []byte) map[string]catalogEntry {
	t.Helper()
	lines := strings.Split(string(body), "\n")
	start := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") && strings.Contains(strings.ToLower(trimmed), "message catalog") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatal("spec has no 'Message catalog' heading")
	}

	i := start
	for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "|") {
		i++
	}
	if i >= len(lines) {
		t.Fatal("spec message catalog has no table")
	}

	header := splitTableRow(lines[i])
	if len(header) == 0 || !strings.EqualFold(strings.Trim(header[0], "`"), "id") {
		t.Fatalf("spec message catalog table must start with an id column, got %q", header)
	}
	routeCol := -1
	classCol := -1
	for c, h := range header {
		name := strings.ToLower(strings.Trim(h, "`"))
		switch name {
		case "route":
			routeCol = c
		case "class":
			classCol = c
		}
	}
	if routeCol < 0 {
		t.Fatal("spec message catalog table has no route column")
	}
	i++
	if i >= len(lines) || !isTableSep(lines[i]) {
		t.Fatal("spec message catalog table is missing the separator row")
	}
	i++

	catalog := make(map[string]catalogEntry)
	order := 0
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || !strings.HasPrefix(line, "|") {
			break
		}
		cells := splitTableRow(lines[i])
		if len(cells) == 0 {
			continue
		}
		id := strings.TrimSpace(strings.Trim(cells[0], "`"))
		if id == "" {
			continue
		}
		if !isCatalogID(id) {
			t.Fatalf("spec catalog id %q is not [a-z][a-z0-9-]*", id)
		}
		if _, dup := catalog[id]; dup {
			t.Fatalf("spec catalog id %q is duplicated", id)
		}
		class := ""
		if classCol >= 0 && classCol < len(cells) {
			class = strings.TrimSpace(strings.Trim(cells[classCol], "`"))
		} else if len(cells) > 1 {
			class = strings.TrimSpace(strings.Trim(cells[1], "`"))
		}
		route := ""
		if routeCol < len(cells) {
			route = strings.TrimSpace(strings.Trim(cells[routeCol], "`"))
		}
		catalog[id] = catalogEntry{
			ID:    id,
			Class: class,
			Route: route,
			Order: order,
		}
		order++
	}
	if len(catalog) == 0 {
		t.Fatal("spec message catalog is empty")
	}
	return catalog
}

func splitTableRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = strings.TrimSpace(p)
	}
	return out
}

func isTableSep(line string) bool {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") {
		return false
	}
	for _, cell := range splitTableRow(line) {
		cell = strings.TrimSpace(cell)
		if cell == "" {
			continue
		}
		for _, r := range cell {
			if r != '-' && r != ':' {
				return false
			}
		}
	}
	return true
}

func isCatalogID(id string) bool {
	if id == "" || id[0] < 'a' || id[0] > 'z' {
		return false
	}
	for _, r := range id[1:] {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}

type vector struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	Comment string `json:"comment,omitempty"`
	Kind    string `json:"kind"`
	source  string

	Origin       string `json:"origin,omitempty"`
	Version      int    `json:"version,omitempty"`
	Route        string `json:"route,omitempty"`
	Handle       string `json:"handle,omitempty"`
	ChallengeHex string `json:"challenge_hex,omitempty"`
	MessageHex   string `json:"message_hex,omitempty"`
	SeedHex      string `json:"seed_hex,omitempty"`
	PublicKeyHex string `json:"public_key_hex,omitempty"`
	SignatureHex string `json:"signature_hex,omitempty"`
	EntropyHex   string `json:"entropy_hex,omitempty"`
	RID          string `json:"rid,omitempty"`

	Request  *httpVec `json:"request,omitempty"`
	Response *httpVec `json:"response,omitempty"`

	Taken   []string `json:"taken,omitempty"`
	Retries *int     `json:"retries,omitempty"`

	RecipientPubHex  string `json:"recipient_pub_hex,omitempty"`
	RecipientPrivHex string `json:"recipient_priv_hex,omitempty"`
	EphemeralPrivHex string `json:"ephemeral_priv_hex,omitempty"`
	EphemeralPubHex  string `json:"ephemeral_pub_hex,omitempty"`
	SenderPrivHex    string `json:"sender_priv_hex,omitempty"`
	SenderPubHex     string `json:"sender_pub_hex,omitempty"`
	NonceHex         string `json:"nonce_hex,omitempty"`
	PlaintextHex     string `json:"plaintext_hex,omitempty"`
	ADHex            string `json:"ad_hex,omitempty"`
	SealedHex        string `json:"sealed_hex,omitempty"`
	TranscriptHex    string `json:"transcript_hex,omitempty"`
	SAS              string `json:"sas,omitempty"`
	Code             string `json:"code,omitempty"`

	StunHex     string `json:"stun_hex,omitempty"`
	MappedIP    string `json:"mapped_ip,omitempty"`
	MappedPort  int    `json:"mapped_port,omitempty"`
	TIDHex      string `json:"transaction_id_hex,omitempty"`
	ResponseHex string `json:"response_hex,omitempty"`
}

type httpVec struct {
	Method        string            `json:"method,omitempty"`
	Path          string            `json:"path,omitempty"`
	Status        int               `json:"status,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	BodyUTF8      string            `json:"body_utf8,omitempty"`
	BodyHex       string            `json:"body_hex,omitempty"`
	ContentLength *int64            `json:"content_length,omitempty"`
}

func loadVectors(t *testing.T, at string) []vector {
	t.Helper()
	dir := filepath.Join(repoRoot(t), vectorRelDir)
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("%s: vector directory %s is absent: %v", at, vectorRelDir, err)
	}
	if !st.IsDir() {
		t.Fatalf("%s: %s is not a directory", at, vectorRelDir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("%s: read %s: %v", at, vectorRelDir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("%s: extra subdirectory %s/%s", at, vectorRelDir, e.Name())
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			t.Errorf("%s: extra unlisted file %s/%s", at, vectorRelDir, name)
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("%s: %s has no *.json files", at, vectorRelDir)
	}

	seenID := make(map[string]string)
	var all []vector
	for _, name := range names {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: read %s: %v", at, name, err)
		}
		vs, err := parseVectorFile(raw)
		if err != nil {
			t.Fatalf("%s: malformed vector file %s: %v", at, name, err)
		}
		if len(vs) == 0 {
			t.Fatalf("%s: vector file %s contains no objects", at, name)
		}
		for i, v := range vs {
			if v.ID == "" {
				t.Fatalf("%s: %s vector %d is missing id", at, name, i)
			}
			if v.Message == "" {
				t.Fatalf("%s: vector %q is missing message", at, v.ID)
			}
			if v.Kind == "" {
				t.Fatalf("%s: vector %q is missing kind", at, v.ID)
			}
			if prev, dup := seenID[v.ID]; dup {
				t.Fatalf("%s: duplicate vector id %q in %s and %s", at, v.ID, prev, name)
			}
			seenID[v.ID] = name
			v.source = name
			all = append(all, v)
		}
	}
	if len(all) == 0 {
		t.Fatalf("%s: vector set is empty; nothing to execute", at)
	}
	return all
}

func parseVectorFile(raw []byte) ([]vector, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty file")
	}
	if raw[0] != '[' {
		return nil, fmt.Errorf("vector file must be a JSON array")
	}
	var vs []vector
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&vs); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing JSON after the vector array")
	}
	return vs, nil
}

type digestEntry struct {
	Hex  string
	Name string
}

func assertVectorDigest(t *testing.T, at string) {
	t.Helper()
	want := mustReadRel(t, digestRelPath, at)
	dir := filepath.Join(repoRoot(t), vectorRelDir)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("%s: vector directory %s is absent: %v", at, vectorRelDir, err)
	}

	got := recomputeVectorDigest(t, dir)
	if !bytes.Equal(want, []byte(got)) {
		t.Fatalf("%s: %s does not match %s\n got:\n%s\nwant:\n%s", at, digestRelPath, vectorRelDir, got, want)
	}

	entries := parseDigestFile(t, at, want)
	files := listedJSONFiles(t, at, dir)
	fromDigest := make(map[string]int, len(entries))
	for _, e := range entries {
		fromDigest[e.Name]++
		if fromDigest[e.Name] > 1 {
			t.Errorf("%s: duplicate digest entry for %s", at, e.Name)
		}
		if _, ok := files[e.Name]; !ok {
			t.Errorf("%s: digest lists %s, which is not a vector JSON file", at, e.Name)
		}
	}
	for name := range files {
		if fromDigest[name] == 0 {
			t.Errorf("%s: vector file %s is unlisted in %s", at, name, digestRelPath)
		}
	}
}

func parseDigestFile(t *testing.T, at string, raw []byte) []digestEntry {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("%s: digest %s is empty", at, digestRelPath)
	}
	if raw[len(raw)-1] != '\n' {
		t.Fatalf("%s: digest %s must end with a newline", at, digestRelPath)
	}
	var out []digestEntry
	for i, line := range strings.Split(string(raw[:len(raw)-1]), "\n") {
		if line == "" {
			t.Fatalf("%s: digest %s has a blank line at %d", at, digestRelPath, i+1)
		}
		if strings.ContainsRune(line, '\r') {
			t.Fatalf("%s: digest %s line %d contains CR", at, digestRelPath, i+1)
		}
		hexPart, name, ok := strings.Cut(line, "  ")
		if !ok || name == "" || strings.Contains(name, "  ") {
			t.Fatalf("%s: digest %s line %d is malformed (want `hex  filename`)", at, digestRelPath, i+1)
		}
		if strings.ContainsAny(name, "/\\ \t") {
			t.Fatalf("%s: digest %s line %d filename must be a basename: %q", at, digestRelPath, i+1, name)
		}
		if !strings.HasSuffix(name, ".json") {
			t.Fatalf("%s: digest %s line %d does not name a .json file: %q", at, digestRelPath, i+1, name)
		}
		if len(hexPart) != 64 || !isLowerHex(hexPart) {
			t.Fatalf("%s: digest %s line %d has a malformed SHA-256: %q", at, digestRelPath, i+1, hexPart)
		}
		out = append(out, digestEntry{Hex: hexPart, Name: name})
	}
	if len(out) == 0 {
		t.Fatalf("%s: digest %s has no entries", at, digestRelPath)
	}
	return out
}

func isLowerHex(s string) bool {
	if len(s) == 0 || len(s)%2 != 0 {
		return false
	}
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return false
	}
	return true
}

func listedJSONFiles(t *testing.T, at, dir string) map[string]struct{} {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("%s: read %s: %v", at, dir, err)
	}
	out := make(map[string]struct{})
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if _, dup := out[e.Name()]; dup {
			t.Fatalf("%s: duplicate vector filename %q", at, e.Name())
		}
		out[e.Name()] = struct{}{}
	}
	return out
}

func recomputeVectorDigest(t *testing.T, dir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(matches, func(i, j int) bool {
		return filepath.Base(matches[i]) < filepath.Base(matches[j])
	})
	var b strings.Builder
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		fmt.Fprintf(&b, "%x  %s\n", sum, filepath.Base(path))
	}
	return b.String()
}

func executeVector(t *testing.T, v vector, meta specMeta) {
	t.Helper()
	switch v.Kind {
	case "auth-message":
		executeAuthMessage(t, v, meta)
	case "ed25519":
		executeEd25519(t, v)
	case "handle":
		executeHandle(t, v)
	case "rid":
		executeRID(t, v)
	case "rid-collision-retry":
		executeRIDCollision(t, v)
	case "envelope-seal-pairing":
		executeEnvelopeSealPairing(t, v)
	case "envelope-seal-session":
		executeEnvelopeSealSession(t, v)
	case "pairing-sas", "pairing-transcript":
		executePairingSAS(t, v)
	case "pairing-code":
		executePairingCode(t, v)
	case "envelope-inner":
		executeEnvelopeInner(t, v)
	case "http", "rejection":
		executeHTTPDeclarative(t, v, meta)
	case "stun":
		executeSTUN(t, v)
	case "stun-drop":
		executeSTUNDrop(t, v)
	default:
		t.Fatalf("unknown vector kind %q", v.Kind)
	}
}

func executeAuthMessage(t *testing.T, v vector, meta specMeta) {
	t.Helper()
	if v.Version != 0 && v.Version != meta.ProtocolVersion {
		t.Fatalf("auth-message version %d is not spec Protocol-Version %d; signing the request v when it differs fails", v.Version, meta.ProtocolVersion)
	}
	chal, err := decodeFixedHex(v.ChallengeHex, 32)
	if err != nil {
		t.Fatalf("challenge_hex: %v", err)
	}
	want, err := hex.DecodeString(v.MessageHex)
	if err != nil {
		t.Fatalf("message_hex: %v", err)
	}
	got := concatAuthMessage(v.Origin, meta.ProtocolVersion, v.Route, v.Handle, chal)
	if !bytes.Equal(got, want) {
		t.Fatalf("auth-message mismatch\n got %x\nwant %x", got, want)
	}
}

func concatAuthMessage(origin string, version int, route, handle string, challenge []byte) []byte {
	var b []byte
	b = append(b, []byte(specAuthPrefix)...)
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

func executeEd25519(t *testing.T, v vector) {
	t.Helper()
	seed, err := decodeFixedHex(v.SeedHex, ed25519.SeedSize)
	if err != nil {
		t.Fatalf("seed_hex: %v", err)
	}
	msg, err := hex.DecodeString(v.MessageHex)
	if err != nil {
		t.Fatalf("message_hex: %v", err)
	}
	wantSig, err := decodeFixedHex(v.SignatureHex, ed25519.SignatureSize)
	if err != nil {
		t.Fatalf("signature_hex: %v", err)
	}
	wantPub, err := decodeFixedHex(v.PublicKeyHex, ed25519.PublicKeySize)
	if err != nil {
		t.Fatalf("public_key_hex: %v", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	if !bytes.Equal(pub, wantPub) {
		t.Fatalf("public key\n got %x\nwant %x", pub, wantPub)
	}
	got := ed25519.Sign(priv, msg)
	if !bytes.Equal(got, wantSig) {
		t.Fatalf("signature\n got %x\nwant %x", got, wantSig)
	}
	if !ed25519.Verify(pub, msg, wantSig) {
		t.Fatal("ed25519.Verify rejected the vector signature")
	}
}

func executeHandle(t *testing.T, v vector) {
	t.Helper()
	ent, err := decodeFixedHex(v.EntropyHex, 10)
	if err != nil {
		t.Fatalf("entropy_hex: %v", err)
	}
	got := "ih_" + specCrockford(ent)
	if got != v.Handle {
		t.Fatalf("handle = %q, want %q", got, v.Handle)
	}
}

func executeRID(t *testing.T, v vector) {
	t.Helper()
	ent, err := hex.DecodeString(v.EntropyHex)
	if err != nil {
		t.Fatalf("entropy_hex: %v", err)
	}
	if len(ent) != 32 {
		t.Fatalf("rid entropy %d bytes, want 32 (256 bits; NFR-17 floor 128, prefer 256)", len(ent))
	}
	got := hex.EncodeToString(ent)
	if got != v.RID {
		t.Fatalf("rid = %q, want lowercase %q", v.RID, got)
	}
}

func executeRIDCollision(t *testing.T, v vector) {
	t.Helper()
	// Test-only: a seeded KDF so the retry count is reproducible.
	// Clients MUST mint with crypto/rand (spec §8). This is not a minting API.
	seed, err := hex.DecodeString(v.SeedHex)
	if err != nil || len(seed) == 0 {
		t.Fatalf("seed_hex: %v (need a non-empty seed)", err)
	}
	taken := make(map[string]bool, len(v.Taken))
	for _, id := range v.Taken {
		taken[id] = true
	}
	var got string
	retries := 0
	for i := 0; i < 8; i++ {
		id := hex.EncodeToString(testOnlyDeriveRID(seed, i))
		if taken[id] {
			retries++
			continue
		}
		got = id
		break
	}
	if got == "" {
		t.Fatal("seeded generator exhausted collision retries")
	}
	if got != v.RID {
		t.Fatalf("rid after retry = %q, want %q", got, v.RID)
	}
	if v.Retries != nil && *v.Retries != retries {
		t.Fatalf("collision retries = %d, want %d", retries, *v.Retries)
	}
}

func testOnlyDeriveRID(seed []byte, attempt int) []byte {
	h := sha256.New()
	h.Write([]byte("TEST-ONLY/not-a-minting-rule/rid"))
	h.Write([]byte{0})
	h.Write(seed)
	h.Write([]byte{0, byte(attempt)})
	return h.Sum(nil)
}

// executeEnvelopeSealSession runs both directions and the negative. The negative
// is the finding: under §12.2.1 every static opened the blob, so a row that
// only checked the positive would have passed against the defect.
func executeEnvelopeSealSession(t *testing.T, v vector) {
	t.Helper()
	sealed, err := specSealSession(v)
	if err != nil {
		t.Fatal(err)
	}
	want, err := hex.DecodeString(v.SealedHex)
	if err != nil {
		t.Fatalf("sealed_hex: %v", err)
	}
	if !bytes.Equal(sealed, want) {
		t.Fatalf("sealed\n got %x\nwant %x", sealed, want)
	}
	plain, err := specOpenSession(v, v.SenderPubHex)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wantPlain, err := hex.DecodeString(v.PlaintextHex)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, wantPlain) {
		t.Fatalf("opened plaintext\n got %x\nwant %x", plain, wantPlain)
	}
	if _, err := specOpenSession(v, v.EphemeralPubHex); err == nil {
		t.Fatal("the envelope opened under a static that did not seal it")
	}
}

func executeEnvelopeSealPairing(t *testing.T, v vector) {
	t.Helper()
	sealed, err := specSeal(v)
	if err != nil {
		t.Fatal(err)
	}
	want, err := hex.DecodeString(v.SealedHex)
	if err != nil {
		t.Fatalf("sealed_hex: %v", err)
	}
	if !bytes.Equal(sealed, want) {
		t.Fatalf("sealed\n got %x\nwant %x", sealed, want)
	}
	plain, err := specOpen(v)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wantPlain, err := hex.DecodeString(v.PlaintextHex)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, wantPlain) {
		t.Fatalf("opened plaintext\n got %x\nwant %x", plain, wantPlain)
	}
}

func specSeal(v vector) ([]byte, error) {
	curve := ecdh.P256()
	privBytes, err := hex.DecodeString(v.EphemeralPrivHex)
	if err != nil {
		return nil, fmt.Errorf("ephemeral_priv_hex: %w", err)
	}
	priv, err := curve.NewPrivateKey(privBytes)
	if err != nil {
		return nil, err
	}
	pubBytes, err := hex.DecodeString(v.RecipientPubHex)
	if err != nil {
		return nil, fmt.Errorf("recipient_pub_hex: %w", err)
	}
	pub, err := curve.NewPublicKey(pubBytes)
	if err != nil {
		return nil, err
	}
	shared, err := priv.ECDH(pub)
	if err != nil {
		return nil, err
	}
	nonce, err := hex.DecodeString(v.NonceHex)
	if err != nil || len(nonce) != 12 {
		return nil, fmt.Errorf("nonce_hex: need 12 bytes")
	}
	plain, err := hex.DecodeString(v.PlaintextHex)
	if err != nil {
		return nil, err
	}
	ad, err := hex.DecodeString(v.ADHex)
	if err != nil {
		return nil, err
	}
	ePub := priv.PublicKey().Bytes()
	key, err := hkdf.Key(sha256.New, shared, []byte("scimux-rv/envelope/pairing"), pairingSpecSealInfo(ePub, pubBytes), 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plain, ad)
	out := append(append(append([]byte{}, ePub...), nonce...), ct...)
	return out, nil
}

func pairingSpecSealInfo(ePub, recipientStatic []byte) string {
	var b []byte
	b = append(b, []byte("seal-pairing")...)
	b = append(b, 0)
	b = append(b, ePub...)
	b = append(b, 0)
	b = append(b, recipientStatic...)
	return string(b)
}

// sessionSpecSealInfo is §12.2.2's info, written from the spec's literals
// like pairingSpecSealInfo above it. A helper that called the product's sessionSealInfo
// would make this file a round trip rather than an oracle.
func sessionSpecSealInfo(ePub, recipientStatic, senderStatic []byte) string {
	var b []byte
	b = append(b, []byte("seal-session")...)
	b = append(b, 0)
	b = append(b, ePub...)
	b = append(b, 0)
	b = append(b, recipientStatic...)
	b = append(b, 0)
	b = append(b, senderStatic...)
	return string(b)
}

func specGCMSession(ePub, recipientStatic, senderStatic, sharedE, sharedS []byte) (cipher.AEAD, error) {
	ikm := append(append([]byte{}, sharedE...), sharedS...)
	key, err := hkdf.Key(sha256.New, ikm, []byte("scimux-rv/envelope/session"),
		sessionSpecSealInfo(ePub, recipientStatic, senderStatic), 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func specSealSession(v vector) ([]byte, error) {
	curve := ecdh.P256()
	ephBytes, err := hex.DecodeString(v.EphemeralPrivHex)
	if err != nil {
		return nil, fmt.Errorf("ephemeral_priv_hex: %w", err)
	}
	eph, err := curve.NewPrivateKey(ephBytes)
	if err != nil {
		return nil, err
	}
	senderBytes, err := hex.DecodeString(v.SenderPrivHex)
	if err != nil {
		return nil, fmt.Errorf("sender_priv_hex: %w", err)
	}
	sender, err := curve.NewPrivateKey(senderBytes)
	if err != nil {
		return nil, err
	}
	if v.SenderPubHex != "" && hex.EncodeToString(sender.PublicKey().Bytes()) != v.SenderPubHex {
		return nil, fmt.Errorf("sender_pub_hex is not sender_priv_hex's point")
	}
	pubBytes, err := hex.DecodeString(v.RecipientPubHex)
	if err != nil {
		return nil, fmt.Errorf("recipient_pub_hex: %w", err)
	}
	pub, err := curve.NewPublicKey(pubBytes)
	if err != nil {
		return nil, err
	}
	sharedE, err := eph.ECDH(pub)
	if err != nil {
		return nil, err
	}
	sharedS, err := sender.ECDH(pub)
	if err != nil {
		return nil, err
	}
	nonce, err := hex.DecodeString(v.NonceHex)
	if err != nil || len(nonce) != 12 {
		return nil, fmt.Errorf("nonce_hex: need 12 bytes")
	}
	plain, err := hex.DecodeString(v.PlaintextHex)
	if err != nil {
		return nil, err
	}
	ad, err := hex.DecodeString(v.ADHex)
	if err != nil {
		return nil, err
	}
	ePub := eph.PublicKey().Bytes()
	gcm, err := specGCMSession(ePub, pubBytes, sender.PublicKey().Bytes(), sharedE, sharedS)
	if err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plain, ad)
	return append(append(append([]byte{}, ePub...), nonce...), ct...), nil
}

// specOpenSession takes the expected sender separately from the vector so that
// naming the wrong one is expressible. That is the whole point of §12.2.2,
// and a helper that read sender_pub_hex internally could not express it.
func specOpenSession(v vector, senderPubHex string) ([]byte, error) {
	curve := ecdh.P256()
	privBytes, err := hex.DecodeString(v.RecipientPrivHex)
	if err != nil {
		return nil, fmt.Errorf("recipient_priv_hex: %w", err)
	}
	priv, err := curve.NewPrivateKey(privBytes)
	if err != nil {
		return nil, err
	}
	senderBytes, err := hex.DecodeString(senderPubHex)
	if err != nil {
		return nil, fmt.Errorf("sender pub: %w", err)
	}
	sender, err := curve.NewPublicKey(senderBytes)
	if err != nil {
		return nil, err
	}
	sealed, err := hex.DecodeString(v.SealedHex)
	if err != nil {
		return nil, err
	}
	if len(sealed) < 65+12+16 {
		return nil, fmt.Errorf("sealed blob too short")
	}
	if v.EphemeralPubHex != "" && hex.EncodeToString(sealed[:65]) != v.EphemeralPubHex {
		return nil, fmt.Errorf("sealed prefix is not ephemeral_pub")
	}
	ePub, err := curve.NewPublicKey(sealed[:65])
	if err != nil {
		return nil, err
	}
	sharedE, err := priv.ECDH(ePub)
	if err != nil {
		return nil, err
	}
	sharedS, err := priv.ECDH(sender)
	if err != nil {
		return nil, err
	}
	ad, err := hex.DecodeString(v.ADHex)
	if err != nil {
		return nil, err
	}
	recipPub, err := hex.DecodeString(v.RecipientPubHex)
	if err != nil {
		return nil, err
	}
	gcm, err := specGCMSession(sealed[:65], recipPub, senderBytes, sharedE, sharedS)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, sealed[65:65+12], sealed[65+12:], ad)
}

func specOpen(v vector) ([]byte, error) {
	curve := ecdh.P256()
	privBytes, err := hex.DecodeString(v.RecipientPrivHex)
	if err != nil {
		return nil, fmt.Errorf("recipient_priv_hex: %w", err)
	}
	priv, err := curve.NewPrivateKey(privBytes)
	if err != nil {
		return nil, err
	}
	sealed, err := hex.DecodeString(v.SealedHex)
	if err != nil {
		return nil, err
	}
	if len(sealed) < 65+12+16 {
		return nil, fmt.Errorf("sealed blob too short")
	}
	if v.EphemeralPubHex != "" && hex.EncodeToString(sealed[:65]) != v.EphemeralPubHex {
		return nil, fmt.Errorf("sealed prefix is not ephemeral_pub")
	}
	ePub, err := curve.NewPublicKey(sealed[:65])
	if err != nil {
		return nil, err
	}
	shared, err := priv.ECDH(ePub)
	if err != nil {
		return nil, err
	}
	ad, err := hex.DecodeString(v.ADHex)
	if err != nil {
		return nil, err
	}
	recipPub, err := hex.DecodeString(v.RecipientPubHex)
	if err != nil {
		return nil, err
	}
	key, err := hkdf.Key(sha256.New, shared, []byte("scimux-rv/envelope/pairing"), pairingSpecSealInfo(sealed[:65], recipPub), 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, sealed[65:65+12], sealed[65+12:], ad)
}

func executePairingSAS(t *testing.T, v vector) {
	t.Helper()
	tr, err := hex.DecodeString(v.TranscriptHex)
	if err != nil || len(tr) == 0 {
		t.Fatalf("transcript_hex: %v", err)
	}
	shared, err := specECDH(v.SenderPrivHex, v.RecipientPubHex)
	if err != nil {
		t.Fatal(err)
	}
	out, err := hkdf.Key(sha256.New, shared, []byte("scimux-rv/sas/v1"), string(tr), 4)
	if err != nil {
		t.Fatal(err)
	}
	n := binary.BigEndian.Uint32(out)
	got := fmt.Sprintf("%06d", n%1000000)
	if v.Kind == "pairing-sas" && got != v.SAS {
		t.Fatalf("sas = %q, want %q", got, v.SAS)
	}
}

func specECDH(privHex, pubHex string) ([]byte, error) {
	curve := ecdh.P256()
	privBytes, err := hex.DecodeString(privHex)
	if err != nil {
		return nil, fmt.Errorf("private: %w", err)
	}
	priv, err := curve.NewPrivateKey(privBytes)
	if err != nil {
		return nil, err
	}
	pubBytes, err := hex.DecodeString(pubHex)
	if err != nil {
		return nil, fmt.Errorf("public: %w", err)
	}
	pub, err := curve.NewPublicKey(pubBytes)
	if err != nil {
		return nil, err
	}
	return priv.ECDH(pub)
}

func executePairingCode(t *testing.T, v vector) {
	t.Helper()
	ent, err := hex.DecodeString(v.EntropyHex)
	if err != nil {
		t.Fatalf("entropy_hex: %v", err)
	}
	if len(ent) != 5 {
		t.Fatalf("pairing-code entropy %d bytes, want 5 (40 bits)", len(ent))
	}
	got := specCrockford(ent)
	if len(got) != 8 {
		t.Fatalf("pairing-code length %d, want 8", len(got))
	}
	if got != v.Code {
		t.Fatalf("pairing-code = %q, want %q", got, v.Code)
	}
}

func executeEnvelopeInner(t *testing.T, v vector) {
	t.Helper()
	body, err := hex.DecodeString(v.PlaintextHex)
	if err != nil || len(body) == 0 {
		t.Fatalf("plaintext_hex: %v", err)
	}
	if !json.Valid(body) {
		t.Fatalf("envelope-inner is not valid JSON: %q", body)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"v", "type", "sdp", "fingerprint"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("envelope-inner missing %q", k)
		}
	}
}

func executeHTTPDeclarative(t *testing.T, v vector, meta specMeta) {
	t.Helper()
	if v.Request == nil {
		t.Fatal("http/rejection vector needs a request")
	}
	if v.Response == nil {
		t.Fatal("http/rejection vector needs a response")
	}
	if v.Request.Method == "" {
		t.Fatal("http/rejection request needs a method")
	}
	if v.Request.Path == "" {
		t.Fatal("http/rejection request needs a path")
	}
	if v.Response.Status == 0 {
		t.Fatal("http/rejection response needs a status")
	}
	checkHTTPSide(t, "request", v.Request)
	checkHTTPSide(t, "response", v.Response)
	pinVersionNegotiationHTTP(t, v, meta)

	if isRateLimitMessage(v.Message) {
		if v.Response.Status != 429 {
			t.Fatalf("rate-limit status %d, want 429", v.Response.Status)
		}
		if _, ok := headerLookup(v.Response.Headers, "Retry-After"); !ok {
			t.Fatal("rate-limit response needs Retry-After")
		}
		body, err := httpBody(v.Response)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) != 0 {
			t.Fatalf("rate-limit body %q, want empty", body)
		}
		return
	}
	if isRejectionKind(v) {
		assertDocumentedRejection(t, v.Response)
	}
}

func pinVersionNegotiationHTTP(t *testing.T, v vector, meta specMeta) {
	t.Helper()
	if v.Request != nil {
		if reqV, numeric, present := jsonNumberV(mustHTTPBody(t, v.Request)); present {
			if !numeric {
				if v.Message != "rejection-version" {
					t.Fatalf("non-numeric request v must be message rejection-version, got %q", v.Message)
				}
			} else if reqV < float64(meta.MinRequestV) {
				if v.Message != "rejection-version" {
					t.Fatalf("request v %v < minimum %d must be message rejection-version, got %q", reqV, meta.MinRequestV, v.Message)
				}
			}
		}
	}
	if v.Response != nil {
		if respV, numeric, present := jsonNumberV(mustHTTPBody(t, v.Response)); present {
			if !numeric {
				t.Fatalf("response v is not a JSON number")
			}
			if respV != float64(meta.ProtocolVersion) {
				t.Fatalf("response v %v, want spec Protocol-Version %d (server constant, never the request v)", respV, meta.ProtocolVersion)
			}
		}
	}
}

func mustHTTPBody(t *testing.T, h *httpVec) []byte {
	t.Helper()
	body, err := httpBody(h)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func jsonNumberV(body []byte) (float64, bool, bool) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return 0, false, false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return 0, false, false
	}
	raw, ok := obj["v"]
	if !ok {
		return 0, false, false
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false, true
	}
	f, err := n.Float64()
	if err != nil {
		return 0, false, true
	}
	return f, true, true
}

func isRateLimitMessage(message string) bool {
	return message == "envelope-rate-limit" || message == "pair-rate-limit"
}

func isRejectionKind(v vector) bool {
	return v.Kind == "rejection" || v.Message == "constant-rejection" || v.Message == "p-rejection" || strings.HasPrefix(v.Message, "rejection-")
}

func assertDocumentedRejection(t *testing.T, h *httpVec) {
	t.Helper()
	if h.Status != 404 {
		t.Fatalf("rejection status %d, want 404 (a 405 is an existence oracle)", h.Status)
	}
	body, err := httpBody(h)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != specRejectBody {
		t.Fatalf("rejection body %q, want %q", body, specRejectBody)
	}
	ct, ok := headerLookup(h.Headers, "Content-Type")
	if !ok || ct != "text/plain; charset=utf-8" {
		t.Fatalf("rejection Content-Type %q, want text/plain; charset=utf-8", ct)
	}
	nosniff, ok := headerLookup(h.Headers, "X-Content-Type-Options")
	if !ok || !strings.EqualFold(nosniff, "nosniff") {
		t.Fatalf("rejection X-Content-Type-Options %q, want nosniff", nosniff)
	}
}

func executeSTUN(t *testing.T, v vector) {
	t.Helper()
	switch v.Message {
	case "stun-binding-request":
		raw, err := hex.DecodeString(v.StunHex)
		if err != nil {
			t.Fatalf("stun_hex: %v", err)
		}
		if len(raw) != specSTUNHeaderLen {
			t.Fatalf("binding request length %d, want %d", len(raw), specSTUNHeaderLen)
		}
		if binary.BigEndian.Uint16(raw[0:2]) != specSTUNBindingRequest {
			t.Fatalf("type %04x, want Binding Request %04x", binary.BigEndian.Uint16(raw[0:2]), specSTUNBindingRequest)
		}
		if binary.BigEndian.Uint16(raw[2:4]) != 0 {
			t.Fatalf("length %d, want 0 (empty Binding)", binary.BigEndian.Uint16(raw[2:4]))
		}
		if binary.BigEndian.Uint32(raw[4:8]) != specSTUNMagicCookie {
			t.Fatalf("cookie %08x, want %08x", binary.BigEndian.Uint32(raw[4:8]), specSTUNMagicCookie)
		}
		if hex.EncodeToString(raw[8:20]) != v.TIDHex {
			t.Fatalf("tid %x, want %s", raw[8:20], v.TIDHex)
		}
	case "stun-binding-success":
		tid, err := hex.DecodeString(v.TIDHex)
		if err != nil || len(tid) != 12 {
			t.Fatalf("transaction_id_hex: %v len %d", err, len(tid))
		}
		ip := net.ParseIP(v.MappedIP)
		if ip == nil {
			t.Fatalf("mapped_ip %q", v.MappedIP)
		}
		got := specXORMapped(tid, ip, v.MappedPort)
		want, err := hex.DecodeString(v.ResponseHex)
		if err != nil {
			t.Fatalf("response_hex: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("XOR-MAPPED-ADDRESS\n got %x\nwant %x", got, want)
		}
	default:
		t.Fatalf("stun vector %s: unhandled message %q", v.ID, v.Message)
	}
}

func specXORMapped(tid []byte, ip net.IP, port int) []byte {
	cookie := []byte{0x21, 0x12, 0xa4, 0x42}
	xport := uint16(port) ^ 0x2112
	var family byte
	var xaddr []byte
	if v4 := ip.To4(); v4 != nil {
		family = 0x01
		xaddr = make([]byte, 4)
		for i := 0; i < 4; i++ {
			xaddr[i] = v4[i] ^ cookie[i]
		}
	} else {
		family = 0x02
		v6 := ip.To16()
		mask := append(append([]byte{}, cookie...), tid...)
		xaddr = make([]byte, 16)
		for i := 0; i < 16; i++ {
			xaddr[i] = v6[i] ^ mask[i]
		}
	}
	attrLen := 4 + len(xaddr)
	msgLen := 4 + attrLen
	out := make([]byte, 20+msgLen)
	binary.BigEndian.PutUint16(out[0:2], specSTUNBindingSuccess)
	binary.BigEndian.PutUint16(out[2:4], uint16(msgLen))
	binary.BigEndian.PutUint32(out[4:8], specSTUNMagicCookie)
	copy(out[8:20], tid)
	binary.BigEndian.PutUint16(out[20:22], specSTUNXORMapped)
	binary.BigEndian.PutUint16(out[22:24], uint16(attrLen))
	out[24] = 0
	out[25] = family
	binary.BigEndian.PutUint16(out[26:28], xport)
	copy(out[28:], xaddr)
	return out
}

func executeSTUNDrop(t *testing.T, v vector) {
	t.Helper()
	if v.StunHex == "" {
		t.Fatal("stun-drop vector needs stun_hex")
	}
	if _, err := hex.DecodeString(v.StunHex); err != nil {
		t.Fatalf("stun_hex: %v", err)
	}
	if v.ResponseHex != "" {
		t.Fatal("stun-drop must not carry a response")
	}
}

func checkHTTPSide(t *testing.T, side string, h *httpVec) {
	t.Helper()
	body, err := httpBody(h)
	if err != nil {
		t.Fatalf("%s body: %v", side, err)
	}
	if h.BodyHex != "" && h.BodyUTF8 != "" {
		t.Fatalf("%s has both body_hex and body_utf8", side)
	}
	if cl, ok := headerLookup(h.Headers, "Content-Length"); ok {
		n, err := strconv.Atoi(cl)
		if err != nil {
			t.Fatalf("%s Content-Length %q is not a number", side, cl)
		}
		if n != len(body) {
			t.Fatalf("%s Content-Length %d, body is %d bytes", side, n, len(body))
		}
	}
	if h.ContentLength != nil && *h.ContentLength >= 0 && int(*h.ContentLength) != len(body) {
		t.Fatalf("%s content_length %d, body is %d bytes", side, *h.ContentLength, len(body))
	}
	if side == "response" {
		if ct, ok := headerLookup(h.Headers, "Content-Type"); ok && strings.Contains(ct, "application/json") && len(body) > 0 {
			if !json.Valid(body) {
				t.Fatalf("%s Content-Type is JSON but body is not valid JSON: %q", side, body)
			}
		}
	}
}

func httpBody(h *httpVec) ([]byte, error) {
	if h == nil {
		return nil, nil
	}
	switch {
	case h.BodyHex != "":
		return hex.DecodeString(h.BodyHex)
	default:
		return []byte(h.BodyUTF8), nil
	}
}

func headerLookup(h map[string]string, key string) (string, bool) {
	if h == nil {
		return "", false
	}
	if v, ok := h[key]; ok {
		return v, true
	}
	for k, v := range h {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

func decodeFixedHex(s string, n int) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != n {
		return nil, fmt.Errorf("got %d bytes, want %d", len(b), n)
	}
	return b, nil
}

func specCrockford(src []byte) string {
	var acc uint64
	var bits uint
	out := make([]byte, 0, (len(src)*8+4)/5)
	for _, b := range src {
		acc = (acc << 8) | uint64(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, specCrockfordAlphabet[(acc>>bits)&31])
		}
	}
	if bits > 0 {
		out = append(out, specCrockfordAlphabet[(acc<<(5-bits))&31])
	}
	return string(out)
}
