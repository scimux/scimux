package app

// Gate C, Go half — an acceptance test must assert over code a product entry
// point can actually reach.
//
// This arc has turned up the same defect five times, in five disguises:
//
//  1. FR-16 proven on an in-process harness path rather than the live one.
//  2. The pairing SAS computed by the test and by nothing else.
//  3. remote.Config.TunnelHandler, read by internal/remote and set by no
//     product code anywhere.
//  4. AcceptPairingOffer and newBoundaries, with zero product callers.
//  5. A whole pairing UI living in web/test/, shipping in no build.
//
// Every one passed review as green tests over working code. What none of them
// had was a path from `scimux` the binary to the thing asserted. The web half
// of this guard is web/test/reachability.test.js; this is the Go half.
//
// Two mechanical rules, because the failures come in two shapes:
//
//   - A symbol nothing calls. TestRemoteATsAssertOverReachableCode closes the
//     transitive call graph of internal/remote over the *non-test* sources,
//     seeded from what internal/app and cmd/scimux actually reference, and
//     requires every internal/remote symbol an AT names to be inside it.
//     Catches (4).
//
//   - A seam only tests configure. TestRemoteConfigSeamsAreSetByProductCode
//     requires every remote.Config field an AT assigns to be assigned by
//     non-test code too, or classified below. Catches (3) exactly.
//
// Neither rule can catch (1) or (2): there the symbol *is* reachable and the
// test simply drove a different path to it. No import- or call-graph check
// sees that — it needs a reader, which is why Gate C keeps a human story
// alongside these. The claim here is narrower and worth making anyway: of the
// five, two could not have shipped with this file present, and the waiver
// tables below turned three more unreachable paths into written-down debt
// instead of green tests nobody doubted.
//
// Two deliberate simplifications, both of which cost precision in the safe
// direction:
//
//   - Resolution is by *name*, not by type. A method call cannot be
//     attributed to a receiver without a full type-check, so `x.Close()`
//     credits every Close in the package. Collisions can only make a symbol
//     look more reachable, never less, so the guard under-reports and never
//     cries wolf. An offender it names is a real one.
//
//   - internal/remote/codec is folded into the same flat namespace as its
//     parent. It is reached only through internal/remote, so a codec symbol
//     with no caller anywhere up that chain is unreachable by the same
//     argument, and keeping one namespace keeps the graph honest about the
//     hop between them.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// reachTarget is the package whose acceptance tests this guard polices.
const reachTarget = "internal/remote"

// reachRoots are the non-test source trees that count as a product entry
// point. A symbol is reachable if it is referenced from one of these, or from
// something that is.
var reachRoots = []string{"cmd", "internal/app"}

// reachInterfaceMethods are method names the runtime and the standard library
// call for us, so no source file names them. Without these the guard would
// report Unwrap and ServeHTTP as dead code on the strength of nobody having
// written the call out longhand.
var reachInterfaceMethods = map[string]bool{
	"Error": true, "Unwrap": true, "Is": true, "As": true, "String": true,
	"ServeHTTP": true, "Read": true, "Write": true, "Close": true,
	"MarshalJSON": true, "UnmarshalJSON": true, "Len": true, "Less": true,
	"Swap": true, "Done": true, "Deadline": true, "Value": true, "Err": true,
	"init": true, "main": true,
}

// reachTestSupport is internal/remote API that exists for tests *by design*,
// so "no product caller" is the correct state and not a finding. Three shapes
// qualify, and the reason string says which:
//
//   - the device's half. In production the remote peer is a browser, so the Go
//     implementation of offer/handshake/round-trip exists only so a test can
//     play that part. There will never be a product caller.
//   - fault injection. Network and peer failures a real deployment suffers and
//     no code requests.
//   - inspection. Accessors that read internal state so an assertion can see
//     it, including the Gate event barriers remote.go documents as such.
//
// Adding a name here is a claim that wants justifying in review. It is the
// escape hatch, not the pattern.
var reachTestSupport = map[string]string{
	"CreateSessionOffer": "device half: in production the browser creates the offer",
	"HandshakeSession":   "device half: joins two in-process peers for a test",
	"RoundTrip":          "device half: the browser issues tunnelled requests, the laptop serves them",

	"SimulateICEFailure":  "fault injection: AT-FR-24 ICE failure",
	"SimulateChannelLoss": "fault injection: AT-FR-24 data-channel loss",
	"DisconnectPeer":      "fault injection: AT-S6 peer drop",
	"RestartRendezvous":   "fault injection: AT-S6 signalling restart",

	"Waiters":                 "inspection: FR-32 waiter set",
	"AddWaiter":               "inspection: installs a waiter the production loop would install itself",
	"SignallingWaiters":       "inspection: live signalling waiter count",
	"OfferCount":              "inspection: offers seen by a session",
	"PairingConsumed":         "inspection: §11 single-use flag",
	"PairingWaiterRegistered": "inspection: whether the wait loop is polling a pairing RID",
	"PairingState":            "inspection: pairing UI state; routes use PairingSession",
	"Empty":                   "inspection: PendingWork predicate",
	"AllWriteSteps":           "inspection: the FR-30 injected-failure table",
	"Attempt":                 "inspection: backoff attempt counter",
	"MethodAllowlist":         "inspection: exposes the codec's closed method set to assert over",
	"RequestHeaderAllowlist":  "inspection: exposes the codec's closed request-header set",
	"ResponseHeaderAllowlist": "inspection: exposes the codec's closed response-header set",
	"PublishChannel":          "inspection: Gate event barrier, remote.go:275 (AT-FR-29-a, AT-FR-32-b)",
	"BeginRequest":            "inspection: Gate event barrier, remote.go:275 (AT-FR-29-a, AT-FR-32-b)",
}

// reachOpenFindings is the other kind of waiver: a symbol that *should* have a
// product caller and does not. These are debt, written down rather than
// discovered a sixth time. Removing an entry — by wiring the join — is the fix;
// the guard then holds the ground.
var reachOpenFindings = map[string]string{
	"RegisterDevice": "no product caller: pairing adopts a device through " +
		"CompletePairing -> adoptPairedDevice, which also publishes to the FR-38 " +
		"list. Eight AT files build their device through this route instead, so " +
		"their devices never appear in the FR-38 list that production devices do.",

	"SetPending": "no product caller: Client.pending is written only by tests. " +
		"Production code only ever deletes from it (client.go:759, :1065), so the " +
		"FR-04/13/29 discard-on-revoke rows are proven against pending work that " +
		"cannot occur.",
	"DevicePending": "no product caller: the read half of the same map as SetPending.",
}

// reachConfigDefaults are remote.Config fields internal/remote defaults when
// they are nil or zero (`if c.cfg.X != nil { use it }`). Production leaving
// them unset is the intended configuration, so no product assignment is
// expected. Each was checked against its defaulting site.
var reachConfigDefaults = map[string]bool{
	"Clock":      true, // client.go:1175 via NewBackoff
	"RNG":        true, // client.go:1175 via NewBackoff
	"Rand":       true, // client.go:101
	"Sys":        true, // invite_file.go:14
	"Hooks":      true, // client.go:97
	"Scheduler":  true, // client.go:1172
	"HTTPClient": true, // client.go:108
	"Version":    true, // client.go:121
	"MinVersion": true, // client.go:128
}

// reachConfigSeams are remote.Config fields that exist for tests: fault
// injection and event barriers. Production leaves them nil and takes the real
// path, so "no product code sets this" is correct rather than a finding.
var reachConfigSeams = map[string]bool{
	"FailWrite":               true,
	"BeforeInviteUnlink":      true,
	"BeforeInviteUnlinkFinal": true,
	"BeforeInviteRestore":     true,
	"BeforeEnrollRequest":     true,
	"BeforePersist":           true,
	"OnLockHeld":              true,
	"Unlinkat":                true,
	"Renameat":                true,
	"LockHeldFile":            true,
	"LockReleaseFile":         true,
	"InviteString":            true, // remote.go:307 says so in as many words

	// RendezvousURL was filed as a default until 2026-08-23, on the strength of
	// persist.go's rvBase() fallback. That reading hid a real finding: no
	// product code set it *or* Origin, so every build could only ever reach
	// remote.DefaultOrigin while the acceptance files talked to httptest
	// servers and looked healthy. The operator path is --rendezvous-url, and it
	// sets Origin, because origin is bound into the pairing transcript
	// (rendezvous-v1 §11) and rvBase() falls back to it — one flag, so the
	// address and the transcript identity cannot drift apart. What is left here
	// is the genuine seam: a test pointing HTTP at an httptest base while
	// keeping the real origin the transcript vectors were computed against.
	"RendezvousURL": true,
}

// A name-resolution caveat worth stating where the next reader will look. The
// reachability guard resolves symbols by *name*, not by type, so a field is
// counted as set by product code if any same-named field is assigned anywhere
// in it. Config.Origin passed clean for the whole of S5-S7 on the strength of
// PersistedState.Origin being assigned at client.go:405 — a different field of
// a different struct. The guard under-reports rather than crying wolf, which
// is the documented trade, but it means a Config field sharing a name with a
// PersistedState field gets no coverage from this file. Config.Origin is now
// covered directly instead, by TestRendezvousURLFlagReachesRemoteConfig.

// reachConfigOpenFindings is the Config half of reachOpenFindings: a field the
// struct documents as an operator action, that no operator can reach.
var reachConfigOpenFindings = map[string]string{
	"Rotate": "remote.go:313 calls this \"the explicit identity rotation action\", " +
		"but remote_command.go registers no flag for it and nothing sets it. " +
		"Identity rotation is unreachable from the binary.",
	"TunnelHandler": "deprecated by join 3 in favour of TunnelHandlerFor, which " +
		"remote_command.go does set. The join-2 rows keep the old field exercised " +
		"until it is removed; this entry goes with it.",
}

// -- source inventory ------------------------------------------------------

// reachFile is one parsed source file with the repo-relative path it came
// from, so a finding can name the file that caused it.
type reachFile struct {
	rel  string
	file *ast.File
}

// parseReachTree parses every .go file under dir. tests selects whether
// _test.go files are included or excluded; both halves are needed, and
// confusing them is the bug this guard exists to find, so the caller always
// says which it wants.
func parseReachTree(t *testing.T, root, dir string, tests bool) []reachFile {
	t.Helper()
	fset := token.NewFileSet()
	var out []reachFile
	err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") != tests {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		out = append(out, reachFile{rel: filepath.ToSlash(rel), file: f})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// referencedNames collects every identifier n mentions, whether written bare
// or as the selector half of a qualified reference. `remote.NewClient` and a
// method call `c.Start(ctx)` both reduce to the name, which is what lets the
// graph cross the package boundary without a type checker.
func referencedNames(n ast.Node) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(n, func(node ast.Node) bool {
		switch v := node.(type) {
		case *ast.SelectorExpr:
			out[v.Sel.Name] = true
		case *ast.Ident:
			out[v.Name] = true
		}
		return true
	})
	return out
}

// declaredNames returns the graph names a top-level declaration introduces.
// Methods collapse onto their bare method name, for the reason in the header.
func declaredNames(d ast.Decl) []string {
	switch v := d.(type) {
	case *ast.FuncDecl:
		return []string{v.Name.Name}
	case *ast.GenDecl:
		var out []string
		for _, spec := range v.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				out = append(out, s.Name.Name)
			case *ast.ValueSpec:
				for _, id := range s.Names {
					out = append(out, id.Name)
				}
			}
		}
		return out
	}
	return nil
}

// -- the graph -------------------------------------------------------------

// reachGraph is the non-test call graph of the target package.
type reachGraph struct {
	decls map[string]bool            // every symbol the package declares
	edges map[string]map[string]bool // symbol -> declared symbols it references
	roots map[string]bool            // symbols named from a product entry point
}

func buildReachGraph(t *testing.T, root string) *reachGraph {
	t.Helper()
	g := &reachGraph{
		decls: map[string]bool{},
		edges: map[string]map[string]bool{},
		roots: map[string]bool{},
	}

	target := parseReachTree(t, root, reachTarget, false)

	// Pass one: what the package declares. The edge pass needs the whole set
	// up front, because a file may call something declared three files later.
	for _, rf := range target {
		for _, d := range rf.file.Decls {
			for _, name := range declaredNames(d) {
				g.decls[name] = true
			}
		}
	}

	// Pass two: edges, restricted to names the package itself declares.
	for _, rf := range target {
		for _, d := range rf.file.Decls {
			refs := referencedNames(d)
			for _, from := range declaredNames(d) {
				if g.edges[from] == nil {
					g.edges[from] = map[string]bool{}
				}
				for name := range refs {
					if name != from && g.decls[name] {
						g.edges[from][name] = true
					}
				}
			}
		}
	}

	// Roots: anything a product entry point names, plus the methods the
	// runtime and standard library call on our behalf.
	for _, dir := range reachRoots {
		for _, rf := range parseReachTree(t, root, dir, false) {
			for name := range referencedNames(rf.file) {
				if g.decls[name] {
					g.roots[name] = true
				}
			}
		}
	}
	for name := range reachInterfaceMethods {
		if g.decls[name] {
			g.roots[name] = true
		}
	}
	return g
}

// reachable closes the graph over its roots.
func (g *reachGraph) reachable() map[string]bool {
	seen := map[string]bool{}
	queue := make([]string, 0, len(g.roots))
	for name := range g.roots {
		queue = append(queue, name)
	}
	for len(queue) > 0 {
		name := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[name] {
			continue
		}
		seen[name] = true
		for next := range g.edges[name] {
			if !seen[next] {
				queue = append(queue, next)
			}
		}
	}
	return seen
}

// -- anti-vacuity ----------------------------------------------------------

// TestReachabilityGuardBuildsACallGraph is what stops this file from passing
// by scanning nothing. The boundary guard next door learned that the hard way:
// a check that is vacuously true is a check nobody ever sees fail.
func TestReachabilityGuardBuildsACallGraph(t *testing.T) {
	root := repoRootFromTest(t)
	g := buildReachGraph(t, root)

	if len(g.decls) < 100 {
		t.Fatalf("%s declares only %d symbols; the walk is not reaching the "+
			"package, so every rule below would pass vacuously", reachTarget, len(g.decls))
	}
	if len(g.roots) < 10 {
		t.Fatalf("only %d root symbols found from %v; the seed is not reaching "+
			"the product entry points, so nothing would be reachable and the "+
			"guard would report the whole package", len(g.roots), reachRoots)
	}
	edges := 0
	for _, to := range g.edges {
		edges += len(to)
	}
	if edges < 100 {
		t.Fatalf("only %d intra-package edges; the graph is not being built, so "+
			"reachability would stop at the root set", edges)
	}

	// Known-live symbols must come out reachable, or the closure is broken in
	// the direction that reports everything. All three are named outright by
	// internal/app, and CompletePairing pulls a deep subtree behind it.
	live := g.reachable()
	for _, name := range []string{"NewClient", "MintPairingCode", "CompletePairing"} {
		if !g.decls[name] {
			t.Fatalf("%s no longer declares %s; this guard's fixtures are stale", reachTarget, name)
		}
		if !live[name] {
			t.Fatalf("%s is not reachable from %v, which cannot be true — the "+
				"closure is broken", name, reachRoots)
		}
	}

	// And a name that does not exist must not come out reachable, or the
	// closure admits anything and the guard reports nothing.
	if live["zzNoSuchSymbolShouldNeverBeReachable"] {
		t.Fatal("an undeclared name came out reachable; the closure admits anything")
	}

	// Every waiver must still name a real symbol. A waiver that has outlived
	// its symbol is a licence nobody revoked.
	var stale []string
	for _, table := range []map[string]string{reachTestSupport, reachOpenFindings} {
		for name := range table {
			if !g.decls[name] {
				stale = append(stale, name)
			}
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("these reachability waivers name symbols %s no longer declares:\n\t%s\n\n"+
			"Delete the waiver — it is now silently excusing nothing, and would "+
			"excuse a future symbol that happened to reuse the name.",
			reachTarget, strings.Join(stale, "\n\t"))
	}
}

// TestReachabilityWaiversAreLoadBearing is the anti-vacuity assertion for the
// waiver tables themselves, and the standing proof that the rule below bites.
//
// Every waived name must be genuinely unreachable. Two things follow. A table
// that had quietly stopped excusing anything — because the closure broke and
// now reports nothing — fails here rather than passing rule one in silence.
// And the moment someone wires one of these up, the guard says so, so a fixed
// finding cannot leave behind a licence that would excuse the next regression.
func TestReachabilityWaiversAreLoadBearing(t *testing.T) {
	root := repoRootFromTest(t)
	g := buildReachGraph(t, root)
	live := g.reachable()

	tables := []struct {
		name  string
		entry map[string]string
		fixed string
	}{
		{"reachTestSupport", reachTestSupport,
			"it now has a product caller, so it is not test-only support after all"},
		{"reachOpenFindings", reachOpenFindings,
			"the join has been wired, so the finding is closed"},
	}

	var wrong []string
	for _, tab := range tables {
		for name := range tab.entry {
			if live[name] {
				wrong = append(wrong, name+" is waived in "+tab.name+", but "+tab.fixed)
			}
		}
	}
	sort.Strings(wrong)
	if len(wrong) > 0 {
		t.Fatalf("these reachability waivers no longer excuse anything:\n\t%s\n\n"+
			"Delete the entry. A waiver kept past its cause is permission nobody "+
			"reviewed, waiting for the next symbol that needs it.",
			strings.Join(wrong, "\n\t"))
	}
}

// -- rule one: ATs assert over reachable code ------------------------------

// atFiles are the acceptance-test files of the target package. They are the
// interesting surface because they make a claim about a requirement: an
// ordinary unit test may legitimately poke at a helper, but an AT asserting
// over unreachable code is a requirement nobody has actually met.
func atFiles(t *testing.T, root string) []reachFile {
	t.Helper()
	var out []reachFile
	for _, rf := range parseReachTree(t, root, reachTarget, true) {
		if strings.HasPrefix(filepath.Base(rf.rel), "at_") {
			out = append(out, rf)
		}
	}
	return out
}

func TestRemoteATsAssertOverReachableCode(t *testing.T) {
	root := repoRootFromTest(t)
	g := buildReachGraph(t, root)
	live := g.reachable()

	ats := atFiles(t, root)
	if len(ats) < 5 {
		t.Fatalf("found only %d at_*_test.go files under %s; the AT scan is not "+
			"working, so this rule would check nothing", len(ats), reachTarget)
	}

	// name -> the AT files that assert over it, so a finding says who believes
	// the requirement is met.
	blamed := map[string]map[string]bool{}
	for _, rf := range ats {
		for name := range referencedNames(rf.file) {
			switch {
			case !g.decls[name], live[name]:
				continue
			case reachTestSupport[name] != "", reachOpenFindings[name] != "":
				continue
			}
			if blamed[name] == nil {
				blamed[name] = map[string]bool{}
			}
			blamed[name][filepath.Base(rf.rel)] = true
		}
	}
	if len(blamed) == 0 {
		return
	}

	names := make([]string, 0, len(blamed))
	for name := range blamed {
		names = append(names, name)
	}
	sort.Strings(names)

	var report []string
	for _, name := range names {
		files := make([]string, 0, len(blamed[name]))
		for f := range blamed[name] {
			files = append(files, f)
		}
		sort.Strings(files)
		report = append(report, name+" (asserted by "+strings.Join(files, ", ")+")")
	}
	t.Fatalf("these %s symbols are asserted over by acceptance tests, and no "+
		"product entry point reaches them — so the tests cannot go red when the "+
		"product is broken:\n\t%s\n\n"+
		"Wire the symbol into the path that should be calling it; that join is "+
		"the requirement, and the primitive on its own never was. If it is "+
		"unreachable by design, classify it in reachTestSupport. If it should be "+
		"reachable and is not yet, record it in reachOpenFindings with the reason.",
		reachTarget, strings.Join(report, "\n\t"))
}

// -- rule two: configured seams are configured by production ---------------

// configFields returns the field names of the named struct in the target
// package.
func configFields(t *testing.T, root, typeName string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, rf := range parseReachTree(t, root, reachTarget, false) {
		for _, d := range rf.file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != typeName {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, f := range st.Fields.List {
					for _, id := range f.Names {
						out[id.Name] = true
					}
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no fields found on %s.%s; the struct scan is not working", reachTarget, typeName)
	}
	return out
}

// assignedFields reports which of fields are the target of a struct-literal key
// or an assignment `<something>.Field = ...` anywhere in files.
//
// A *read* deliberately does not count, and that distinction is the whole
// point: TunnelHandler was a field internal/remote read faithfully on every
// session and no product code ever wrote.
func assignedFields(files []reachFile, fields map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, rf := range files {
		ast.Inspect(rf.file, func(node ast.Node) bool {
			switch v := node.(type) {
			case *ast.KeyValueExpr:
				if id, ok := v.Key.(*ast.Ident); ok && fields[id.Name] {
					out[id.Name] = true
				}
			case *ast.AssignStmt:
				for _, lhs := range v.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && fields[sel.Sel.Name] {
						out[sel.Sel.Name] = true
					}
				}
			}
			return true
		})
	}
	return out
}

// TestRemoteConfigSeamsAreSetByProductCode is the field-shaped half of the same
// rule. remote.Config is how the laptop half is told what to be, so a field
// only ever populated by a test describes an installation that has never
// existed.
func TestRemoteConfigSeamsAreSetByProductCode(t *testing.T) {
	root := repoRootFromTest(t)
	fields := configFields(t, root, "Config")

	var product []reachFile
	for _, dir := range reachRoots {
		product = append(product, parseReachTree(t, root, dir, false)...)
	}
	// internal/remote's own non-test code may populate a Config too — a
	// default, a derived client. That is still production.
	product = append(product, parseReachTree(t, root, reachTarget, false)...)
	setByProduct := assignedFields(product, fields)

	if len(setByProduct) < 5 {
		t.Fatalf("product code appears to set only %d of %d Config fields; the "+
			"assignment scan is not working", len(setByProduct), len(fields))
	}

	// Waivers must name live fields, for the same reason the symbol waivers do.
	var stale []string
	for name := range reachConfigSeams {
		if !fields[name] {
			stale = append(stale, name)
		}
	}
	for name := range reachConfigDefaults {
		if !fields[name] {
			stale = append(stale, name)
		}
	}
	for name := range reachConfigOpenFindings {
		if !fields[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("these Config waivers name fields that no longer exist:\n\t%s\n\n"+
			"Delete them; a waiver outliving its field excuses nothing today and "+
			"the wrong thing tomorrow.", strings.Join(stale, "\n\t"))
	}

	// The load-bearing check, as above: a field recorded as unreachable debt
	// that production now sets is a closed finding, and its entry has to go
	// with it rather than stand as unreviewed permission.
	var closed []string
	for name := range reachConfigOpenFindings {
		if setByProduct[name] {
			closed = append(closed, name)
		}
	}
	sort.Strings(closed)
	if len(closed) > 0 {
		t.Fatalf("these Config fields are recorded in reachConfigOpenFindings and "+
			"product code now sets them:\n\t%s\n\nThe finding is closed; delete the "+
			"entry.", strings.Join(closed, "\n\t"))
	}

	var offenders []string
	for name := range assignedFields(atFiles(t, root), fields) {
		switch {
		case setByProduct[name], reachConfigSeams[name],
			reachConfigDefaults[name], reachConfigOpenFindings[name] != "":
			continue
		}
		offenders = append(offenders, name)
	}
	sort.Strings(offenders)

	if len(offenders) > 0 {
		t.Fatalf("acceptance tests configure these remote.Config fields and no "+
			"product code ever sets them:\n\t%s\n\n"+
			"The tests therefore describe an installation that cannot occur. Set "+
			"the field on the real startup path; or classify it — reachConfigSeams "+
			"for a fault-injection seam or event barrier, reachConfigDefaults if "+
			"internal/remote supplies a default when it is unset, "+
			"reachConfigOpenFindings if it ought to be reachable and is not yet.",
			strings.Join(offenders, "\n\t"))
	}
}
