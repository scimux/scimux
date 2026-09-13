package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// phase5MuseProductionFiles is the explicit scan list for Phase 5 Muse
// application plumbing. A walk that forgot a file would otherwise pass by
// omitting the code that could violate the boundary.
var phase5MuseProductionFiles = []string{
	"app.go",
	"node_lifecycle.go",
	"node_api.go",
	"conversation_api.go",
	"poller.go",
	"state_api.go",
	"auto_approve.go",
	"agents.go",
	"harness_version.go",
	"settings.go",
	"main.go",
	"update.go",
	"worker_harness.go",
	"worker_manager.go",
	"../sessionworker/protocol.go",
}

func TestMuseBoundaryIncludesSessionWorkerSurfaces(t *testing.T) {
	listed := map[string]bool{}
	for _, name := range phase5MuseProductionFiles {
		listed[name] = true
	}
	for _, name := range []string{"worker_harness.go", "worker_manager.go", "../sessionworker/protocol.go"} {
		if !listed[name] {
			t.Errorf("Muse session-worker surface %s is outside the boundary scan", name)
		}
	}
}

func appDirFromTest(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(thisFile)
}

func TestMuseBoundaryEnumeratesPhase5ProductionFiles(t *testing.T) {
	dir := appDirFromTest(t)
	seen := map[string]bool{}
	for _, name := range phase5MuseProductionFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected Phase 5 production file %s: %v", name, err)
		}
		seen[name] = true
	}
	if len(seen) != 15 {
		t.Fatalf("want 15 Phase 5 production files, listed %d", len(seen))
	}
}

func TestMuseBoundaryAntiVacuity(t *testing.T) {
	files := readPhase5Production(t)
	joined := ""
	for _, src := range files {
		joined += src
	}
	needles := []string{
		`"muse"`,
		"muse_approval_judge_consent",
		"museStableChannel",
		"museClassify",
		"museManager",
		"case \"muse\"",
	}
	for _, n := range needles {
		if !strings.Contains(joined, n) {
			t.Fatalf("anti-vacuity: scanned production did not contain %q", n)
		}
	}
	if !strings.Contains(files["auto_approve.go"], `case "acp", "codex", "muse":`) {
		t.Fatal("auto_approve.go must route Muse through structured auto-approval")
	}
	if !strings.Contains(files["harness_version.go"], museStableChannel) {
		t.Fatal("stable-channel source missing from harness_version.go")
	}
	var classifierOwner string
	for name, src := range files {
		if !assignsMuseClassifier(t, name, src) {
			continue
		}
		if classifierOwner != "" {
			t.Fatalf("multiple production Muse tier classifier owners: %s and %s", classifierOwner, name)
		}
		classifierOwner = name
	}
	if classifierOwner != "main.go" {
		t.Fatalf("production Muse tier classifier owner = %q, want main.go", classifierOwner)
	}
	if !strings.Contains(files["main.go"], "a.museClassify = classifyMuseStandard") {
		t.Fatal("main.go must install exactly the maintainer-approved all-Standard classifier")
	}
	if strings.Contains(joined, "this planted token must not appear in production") {
		t.Fatal("scan included test files")
	}
}

func TestMuseBoundaryForbiddenLiteralsAbsentFromProduction(t *testing.T) {
	files := readPhase5Production(t)
	for name, src := range files {
		for _, hit := range museAppForbiddenHits(name, src) {
			t.Errorf("%s: %s", name, hit)
		}
	}
}

func TestMuseBoundaryImportsAreStdlibOrInternal(t *testing.T) {
	dir := appDirFromTest(t)
	fset := token.NewFileSet()
	for _, name := range phase5MuseProductionFiles {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			if !museAppAllowedImport(p) {
				t.Errorf("%s imports %s (stdlib or scimux internal only)", name, p)
			}
			if strings.Contains(p, "acp-go-sdk") {
				t.Errorf("%s imports ACP SDK %s", name, p)
			}
		}
	}
}

// phase6MuseBrowserFiles owns Muse policy presentation in the browser.
// Anti-vacuity: a walk that forgot one of these would otherwise pass by
// omitting the code that must consume the server-owned field names.
var phase6MuseBrowserFiles = []string{
	"web/js/sheets.js",
	"web/js/chat.js",
	"web/js/harness.js",
	"web/js/app.js",
	"web/js/usage.js",
	"web/index.html",
}

func TestMuseBoundaryPhase6BrowserFiles(t *testing.T) {
	root := repoRootFromTest(t)
	for _, rel := range phase6MuseBrowserFiles {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("expected Phase 6 browser file %s: %v", rel, err)
		}
	}

	sheets, err := os.ReadFile(filepath.Join(root, "web/js/sheets.js"))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := os.ReadFile(filepath.Join(root, "web/js/chat.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sheets), "muse_models") {
		t.Error("web/js/sheets.js must consume the literal field name muse_models")
	}
	if !strings.Contains(string(chat), "muse_schema_warning") {
		t.Error("web/js/chat.js must consume the literal field name muse_schema_warning")
	}
	if museFieldIsConcealed(string(sheets), "muse_models") {
		t.Error("web/js/sheets.js conceals muse_models instead of using the literal field name")
	}
	if museFieldIsConcealed(string(chat), "muse_schema_warning") {
		t.Error("web/js/chat.js conceals muse_schema_warning instead of using the literal field name")
	}

	web := filepath.Join(root, "web")
	err = filepath.WalkDir(web, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "test" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(d.Name()) {
		case ".js", ".html", ".css":
		default:
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(b)
		rel, _ := filepath.Rel(root, path)
		for _, hit := range museBrowserForbiddenHits(src) {
			t.Errorf("%s: %s", rel, hit)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

var museConcealSpace = regexp.MustCompile(`\s+`)

func museFieldIsConcealed(src, field string) bool {
	rest, ok := strings.CutPrefix(field, "muse")
	if !ok || rest == "" {
		return false
	}
	compact := museConcealSpace.ReplaceAllString(src, "")
	for _, n := range []string{
		`"muse"+"` + rest + `"`,
		`'muse'+'` + rest + `'`,
		`"muse"+'` + rest + `'`,
		`'muse'+"` + rest + `"`,
		`["muse","` + rest + `"].join("")`,
		`["muse","` + rest + `"].join('')`,
		`['muse','` + rest + `'].join("")`,
		`['muse','` + rest + `'].join('')`,
	} {
		if strings.Contains(compact, n) {
			return true
		}
	}
	return false
}

func museBrowserForbiddenHits(src string) []string {
	cred := "credentials" + ".json"
	auth := "auth" + ".json"
	var hits []string
	if strings.Contains(src, "/.muse") || strings.Contains(src, "~/.muse") {
		hits = append(hits, "muse config dir")
	}
	if strings.Contains(src, "muse-token") ||
		strings.Contains(src, cred) ||
		strings.Contains(src, auth) {
		hits = append(hits, "credential filename")
	}
	if strings.Contains(src, "--base-url") {
		hits = append(hits, "base-url flag")
	}
	if strings.Contains(src, "--provider") {
		hits = append(hits, "provider flag")
	}
	if strings.Contains(src, "--approval-judge") {
		hits = append(hits, "approval-judge flag")
	}
	if strings.Contains(src, "api.meta.ai") {
		hits = append(hits, "meta operational endpoint")
	}
	if strings.Contains(src, "install-muse") {
		hits = append(hits, "launcher fetch")
	}
	if strings.Contains(src, "museTiers") ||
		strings.Contains(src, "tierByModel") ||
		strings.Contains(src, "modelToTier") ||
		strings.Contains(src, "MUSE_TIER_BY_ID") {
		hits = append(hits, "hardcoded muse tier map")
	}
	return hits
}

func TestMuseBrowserBoundaryMatchersCatchPlantedViolations(t *testing.T) {
	cred := "credentials" + ".json"
	auth := "auth" + ".json"
	plants := []struct {
		name string
		src  string
		hit  string
	}{
		{"muse config dir", `path = "/home/user/.muse/config"`, "muse config dir"},
		{"token filename", `n = "muse-token"`, "credential filename"},
		{"credential filename", `n = "` + cred + `"`, "credential filename"},
		{"auth filename", `n = "` + auth + `"`, "credential filename"},
		{"base-url flag", `flag = "--base-url"`, "base-url flag"},
		{"provider flag", `flag = "--provider"`, "provider flag"},
		{"approval-judge flag", `flag = "--approval-judge"`, "approval-judge flag"},
		{"meta endpoint", `u = "https://api.meta.ai/v1"`, "meta operational endpoint"},
		{"launcher", `u = "https://example.invalid/install-muse.sh"`, "launcher fetch"},
		{"museTiers", `const museTiers = {}`, "hardcoded muse tier map"},
		{"tierByModel", `const tierByModel = {}`, "hardcoded muse tier map"},
		{"modelToTier", `const modelToTier = {}`, "hardcoded muse tier map"},
		{"MUSE_TIER_BY_ID", `const MUSE_TIER_BY_ID = {}`, "hardcoded muse tier map"},
	}
	for _, p := range plants {
		hits := museBrowserForbiddenHits(p.src)
		found := false
		for _, h := range hits {
			if h == p.hit {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: want hit %q, got %v", p.name, p.hit, hits)
		}
	}

	safe := []string{
		`info.muse_models`,
		`n.muse_schema_warning`,
		`row.tier === "standard"`,
		`row.tier === "discounted"`,
		`row.tier === "unknown"`,
		`const providerName = info.providerName`,
		`const providerId = m.providerId`,
		`Muse may use part of your plan's usage limit when it checks whether tool actions are safe.`,
		`Actions that still need approval stay paused until they are allowed or rejected.`,
	}
	for _, s := range safe {
		if hits := museBrowserForbiddenHits(s); len(hits) > 0 {
			t.Errorf("false positive on %q: %v", s, hits)
		}
	}

	conceal := []struct {
		field, src string
	}{
		{"muse_models", `"muse" + "_models"`},
		{"muse_models", `"muse"+"_models"`},
		{"muse_models", `'muse' + '_models'`},
		{"muse_models", `'muse'+'_models'`},
		{"muse_models", `"muse" + '_models'`},
		{"muse_models", "\"muse\" +\n \"_models\""},
		{"muse_models", `["muse", "_models"].join("")`},
		{"muse_models", `['muse', '_models'].join('')`},
		{"muse_models", `["muse","_models"].join("")`},
		{"muse_schema_warning", `"muse" + "_schema_warning"`},
		{"muse_schema_warning", `"muse"+"_schema_warning"`},
		{"muse_schema_warning", `'muse' + '_schema_warning'`},
		{"muse_schema_warning", `'muse'+'_schema_warning'`},
		{"muse_schema_warning", `"muse" + '_schema_warning'`},
		{"muse_schema_warning", "\"muse\" +\n \"_schema_warning\""},
		{"muse_schema_warning", `["muse", "_schema_warning"].join("")`},
		{"muse_schema_warning", `['muse', '_schema_warning'].join('')`},
	}
	for _, c := range conceal {
		if !museFieldIsConcealed(c.src, c.field) {
			t.Errorf("concealment missed %s in %q", c.field, c.src)
		}
	}

	notConceal := []string{
		`info.muse_models`,
		`n.muse_schema_warning`,
		`const muse = "muse"`,
		`const models = info.models`,
		`const schema = "schema"`,
		`const warning = "warning"`,
	}
	for _, s := range notConceal {
		if museFieldIsConcealed(s, "muse_models") || museFieldIsConcealed(s, "muse_schema_warning") {
			t.Errorf("false concealment on %q", s)
		}
	}
}

func TestMuseBoundaryTestsNeverExecRealAgentCLI(t *testing.T) {
	dir := appDirFromTest(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var scanned int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for _, hit := range museRealAgentExecHits(string(b)) {
			t.Errorf("%s: %s", e.Name(), hit)
		}
	}
	if scanned < 10 {
		t.Fatalf("scanned %d app test files, walk looks vacuous", scanned)
	}
}

func TestMuseRealAgentMatcherCatchesPlantedViolations(t *testing.T) {
	museBin := "mu" + "se"
	plants := []struct {
		name string
		src  string
	}{
		{"exec muse", `exec.Command("` + museBin + `", "serve")`},
		{"command-context muse", `exec.CommandContext(context.Background(), "` + museBin + `", "serve")`},
		{"lookpath muse", `exec.LookPath("` + museBin + `")`},
		{"exec claude", `exec.Command("` + "clau" + "de" + `")`},
		{"exec codex", `exec.Command("` + "cod" + "ex" + `")`},
		{"exec grok", `exec.Command("` + "gr" + "ok" + `")`},
		{"exec pi", `exec.Command("` + "p" + "i" + `")`},
		{"exec opencode", `exec.Command("` + "open" + "code" + `")`},
	}
	for _, p := range plants {
		if hits := museRealAgentExecHits(p.src); len(hits) == 0 {
			t.Errorf("%s: matcher missed %q", p.name, p.src)
		}
	}
	safe := []string{
		`exec.Command(os.Args[0])`,
		`exec.LookPath("tmux")`,
		`exec.LookPath("git")`,
		`exec.LookPath("node")`,
		`writeScript(t, binDir, "muse", "exit 0")`,
		`if h.bin != "muse"`,
		`Title: "approval-judge unavailable"`,
		`const providerName = info.providerName`,
	}
	for _, s := range safe {
		if hits := museRealAgentExecHits(s); len(hits) > 0 {
			t.Errorf("false positive on %q: %v", s, hits)
		}
	}
}

func TestMuseBoundaryMatchersCatchPlantedViolations(t *testing.T) {
	cred := "credentials" + ".json"
	auth := "auth" + ".json"
	plants := map[string]string{
		"other meta endpoint":  `u := "https://api.meta.ai/muse-code/v1/models"`,
		"stable url elsewhere": `u := "https://api.meta.ai/muse-code/channels/muse-stable"`,
		"muse config dir":      `path := "/home/user/.muse/tokens"`,
		"base-url flag":        `args := []string{"serve", "--base-url", "http://127.0.0.1"}`,
		"provider flag":        `args := []string{"serve", "--provider", "other"}`,
		"approval-judge":       `args := []string{"serve", "--approval-judge", "off"}`,
		"cred filename":        "os.ReadFile(\"" + cred + "\")",
		"auth filename":        "os.ReadFile(\"" + auth + "\")",
		"launcher curl":        `exec.Command("curl", "https://example.invalid/install-muse.sh")`,
		"schema artifact":      `os.ReadFile("schema/msp.schema.json")`,
		"golden testdata":      `os.ReadFile("testdata/golden-approval.ndjson")`,
		"acp sdk import":       `import "github.com/coder/acp-go-sdk"`,
		"hardcoded tier map":   `var museTiers = map[string]string{"spark-code": "standard"}`,
		"auth header":          `req.Header.Set("Authorization", "Bearer x")`,
	}
	for label, plant := range plants {
		file := "agents.go"
		if label == "stable url elsewhere" {
			file = "agents.go"
		}
		if label == "auth header" {
			file = "harness_version.go"
		}
		hits := museAppForbiddenHits(file, plant)
		if strings.Contains(plant, "acp-go-sdk") {
			if museAppAllowedImport("github.com/coder/acp-go-sdk") {
				t.Fatalf("%s: SDK import was allowed", label)
			}
			continue
		}
		if len(hits) == 0 {
			t.Fatalf("matcher missed planted violation %s: %s", label, plant)
		}
	}
	if hits := museAppForbiddenHits("harness_version.go", `u := "https://api.meta.ai/muse-code/channels/muse-stable"`); len(hits) != 0 {
		t.Fatalf("stable URL in harness_version.go must be allowed, got %v", hits)
	}
	if museAppAllowedImport("corp/localpkg") {
		t.Fatal("dotless third-party import was classified as standard library")
	}
	if !assignsMuseClassifier(t, "plant.go", "package app\nfunc f(a *app) { a.museClassify = classify }") {
		t.Fatal("classifier assignment matcher missed a planted assignment")
	}
	safe := []string{
		`ProviderName string ` + "`json:\"providerName\"`",
		`Title: "approval-judge unavailable"`,
		`tier := museTierUnknown`,
	}
	for _, s := range safe {
		if hits := museAppForbiddenHits("agents.go", s); len(hits) > 0 {
			t.Fatalf("false positive on %q: %v", s, hits)
		}
	}
}

func museAppAllowedImport(p string) bool {
	if museAppAllowedStdlib[p] {
		return true
	}
	if strings.HasPrefix(p, "codeberg.org/chrberger/scimux/internal/") ||
		p == "codeberg.org/chrberger/scimux/legal" {
		return true
	}
	return false
}

var museAppAllowedStdlib = map[string]bool{
	"bytes": true, "context": true, "crypto/rand": true, "crypto/sha256": true,
	"crypto/subtle": true,
	"encoding/hex":  true, "encoding/json": true, "errors": true,
	"flag": true, "fmt": true, "hash/fnv": true, "io": true, "net": true,
	"net/http": true, "net/url": true, "os": true, "os/exec": true,
	"os/signal": true, "path/filepath": true, "regexp": true,
	"runtime": true, "strconv": true, "strings": true, "sync": true,
	"sync/atomic": true, "syscall": true, "time": true,
}

func assignsMuseClassifier(t *testing.T, name, src string) bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "museClassify" {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func readPhase5Production(t *testing.T) map[string]string {
	t.Helper()
	dir := appDirFromTest(t)
	out := map[string]string{}
	for _, name := range phase5MuseProductionFiles {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = string(b)
	}
	return out
}

func museAppForbiddenHits(file, src string) []string {
	var hits []string
	stable := "https://api.meta.ai/muse-code/channels/muse-stable"
	cred := "credentials" + ".json"
	auth := "auth" + ".json"
	checks := []struct {
		name string
		ok   func() bool
	}{
		{"meta api endpoint", func() bool {
			if !strings.Contains(src, "api.meta.ai") {
				return false
			}
			if file == "harness_version.go" && strings.Contains(src, stable) &&
				!strings.Contains(strings.ReplaceAll(src, stable, ""), "api.meta.ai") {
				return false
			}
			return true
		}},
		{"stable url outside harness_version.go", func() bool {
			return file != "harness_version.go" && strings.Contains(src, stable)
		}},
		{"muse config dir", func() bool {
			return strings.Contains(src, "/.muse") || strings.Contains(src, "~/.muse")
		}},
		{"base-url flag", func() bool { return strings.Contains(src, "--base-url") }},
		{"provider flag", func() bool { return strings.Contains(src, "--provider") }},
		{"approval-judge flag", func() bool { return strings.Contains(src, "--approval-judge") }},
		{"credential filename", func() bool {
			return strings.Contains(src, "muse-token") ||
				strings.Contains(src, cred) ||
				strings.Contains(src, auth)
		}},
		{"launcher fetch", func() bool {
			return strings.Contains(src, "install-muse") ||
				(file != "harness_version.go" && strings.Contains(src, "muse-stable"))
		}},
		{"schema artifact", func() bool {
			return strings.Contains(src, "msp.schema.json") || strings.Contains(src, "/schema/")
		}},
		{"vendored testdata", func() bool {
			return strings.Contains(src, "testdata/golden") || strings.Contains(src, "testdata/real-") ||
				strings.Contains(src, ".ndjson")
		}},
		{"hardcoded muse tier map", func() bool {
			return strings.Contains(src, "museTiers") ||
				(strings.Contains(src, `map[string]string{`) && strings.Contains(src, `"standard"`) &&
					strings.Contains(src, "spark"))
		}},
		{"muse update authorization", func() bool {
			if file != "harness_version.go" {
				return false
			}
			return strings.Contains(src, `Header.Set("Authorization"`) ||
				strings.Contains(src, `Header.Set("Cookie"`) ||
				strings.Contains(src, "SetCookie")
		}},
	}
	for _, c := range checks {
		if c.ok() {
			hits = append(hits, c.name)
		}
	}
	return hits
}

func museRealAgentExecHits(src string) []string {
	var hits []string
	for _, bin := range []string{"muse", "claude", "codex", "pi", "opencode", "grok"} {
		literal := `"(?:[^"\n]*/)?` + regexp.QuoteMeta(bin) + `"`
		if regexp.MustCompile(`exec\.Command\s*\(\s*` + literal).MatchString(src) {
			hits = append(hits, "exec.Command("+bin+")")
		}
		if regexp.MustCompile(`exec\.CommandContext\s*\([^,]+,\s*` + literal).MatchString(src) {
			hits = append(hits, "exec.CommandContext("+bin+")")
		}
		if regexp.MustCompile(`exec\.LookPath\s*\(\s*"` + regexp.QuoteMeta(bin) + `"`).MatchString(src) {
			hits = append(hits, "exec.LookPath("+bin+")")
		}
	}
	return hits
}
