package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardWalkersExcludeOnlyRootAttic(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"attic", "published", "nested/attic"} {
		path := filepath.Join(root, dir, "synthetic.go")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		// Assemble the synthetic forbidden strings so the module-wide guard
		// does not mistake the test's own construction for a product read.
		body := "package synthetic\nimport _ \"github.com/" + "pion/webrtc/v4\"\n" +
			"var A = \".cred" + "entials\"\n" +
			"var B = \"OPENAI_" + "API_KEY\"\n"
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	imports := scanGoImportsAt(t, root)
	literals := scanGoStringLiteralsAt(t, root)
	pion, err := scanPionLeaks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		dir  string
		want bool
	}{{"attic", false}, {"published", true}, {"nested/attic", true}} {
		t.Run(tc.dir, func(t *testing.T) {
			prefix := tc.dir + "/"
			foundImport := false
			for _, imp := range imports {
				if strings.HasPrefix(imp.rel, prefix) && strings.HasPrefix(imp.path, "github.com/"+"pion/") {
					foundImport = true
				}
			}
			var scoped []goLiteral
			for _, lit := range literals {
				if strings.HasPrefix(lit.rel, prefix) {
					scoped = append(scoped, lit)
				}
			}
			foundPion := false
			for _, leak := range pion {
				if strings.HasPrefix(leak, prefix) {
					foundPion = true
				}
			}
			if foundImport != tc.want || (len(credentialViolations(scoped, bannedCredentialPaths)) > 0) != tc.want || (len(credentialViolations(scoped, providerEnvRules())) > 0) != tc.want || foundPion != tc.want {
				t.Errorf("unexpected guard findings for %s: import=%v credential=%v provider=%v pion=%v", tc.dir, foundImport, len(credentialViolations(scoped, bannedCredentialPaths)) > 0, len(credentialViolations(scoped, providerEnvRules())) > 0, foundPion)
			}
		})
	}
}
