package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// envProbeScript records which perimeter markers are set. It writes names
// only, so a failure log cannot contain a credential value.
func envProbeScript(report string) string {
	return "#!/bin/sh\n" +
		"{\n" +
		"if [ -n \"$SCIMUX_UNRELATED_SECRET\" ]; then echo SENTINEL; fi\n" +
		"if [ -n \"$VIBE_NOT_A_REAL_SETTING\" ]; then echo VIBE_PREFIX; fi\n" +
		"if [ -n \"$MISTRAL_NOT_A_KEY\" ]; then echo MISTRAL_PREFIX; fi\n" +
		"if [ -n \"$VIBE_HOME\" ]; then echo HAS_VIBE_HOME; fi\n" +
		"if [ -n \"$MISTRAL_API_KEY\" ]; then echo HAS_MISTRAL_API_KEY; fi\n" +
		"if [ -n \"$SSL_CERT_FILE\" ]; then echo HAS_SSL_CERT_FILE; fi\n" +
		"if [ -n \"$PATH\" ]; then echo HAS_PATH; fi\n" +
		"if [ -n \"$LANG\" ]; then echo HAS_LANG; fi\n" +
		"} > " + quoteSh(report) + "\n" +
		"exit 0\n"
}

func quoteSh(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func writeEnvProbe(t *testing.T, dir, name, report string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(envProbeScript(report)), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func setVibeEnvPerimeter(t *testing.T) {
	t.Helper()
	t.Setenv("SCIMUX_UNRELATED_SECRET", "synthetic-sentinel")
	t.Setenv("VIBE_NOT_A_REAL_SETTING", "1")
	t.Setenv("MISTRAL_NOT_A_KEY", "1")
	t.Setenv("VIBE_HOME", t.TempDir())
	t.Setenv("MISTRAL_API_KEY", "synthetic-not-a-credential")
	t.Setenv("SSL_CERT_FILE", filepath.Join(t.TempDir(), "ca.pem"))
	t.Setenv("LANG", "C")
}

func envMarkers(t *testing.T, report string) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("environment report missing: %v", err)
	}
	got := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, "=") {
			t.Fatal("environment report included a value")
		}
		got[line] = true
	}
	return got
}

func requireVibePerimeter(t *testing.T, got map[string]bool) {
	t.Helper()
	for _, forbidden := range []string{"SENTINEL", "VIBE_PREFIX", "MISTRAL_PREFIX"} {
		if got[forbidden] {
			t.Fatalf("vibe process received %s", forbidden)
		}
	}
	for _, required := range []string{"HAS_VIBE_HOME", "HAS_MISTRAL_API_KEY", "HAS_SSL_CERT_FILE", "HAS_PATH", "HAS_LANG"} {
		if !got[required] {
			t.Fatalf("vibe process missing %s", required)
		}
	}
}

func TestVibeCatalogProbeDropsUnrelatedSecrets(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report")
	bin := writeEnvProbe(t, dir, "vibe-acp", report)
	setVibeEnvPerimeter(t)
	t.Setenv("PATH", dir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = ProbeVibeCatalog(ctx, bin)
	requireVibePerimeter(t, envMarkers(t, report))
}

func TestVibeLiveChildInheritsUserCommandEnvironment(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report")
	writeEnvProbe(t, dir, "vibe-acp", report)
	setVibeEnvPerimeter(t)
	t.Setenv("PATH", dir)

	proc, err := execRunner("n-vibe-env", "vibe", dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = proc.Wait()
	got := envMarkers(t, report)
	for _, key := range []string{"SENTINEL", "VIBE_PREFIX", "MISTRAL_PREFIX", "HAS_VIBE_HOME", "HAS_MISTRAL_API_KEY", "HAS_SSL_CERT_FILE", "HAS_PATH", "HAS_LANG"} {
		if !got[key] {
			t.Fatalf("live Vibe child lost %s", key)
		}
	}
}

func TestVibeChildEnvKeepsOnlyTheFirstAllowedValue(t *testing.T) {
	got := vibeChildEnv([]string{
		"PATH=/bin",
		"PATH=/usr/bin",
		"NOT_A_PAIR",
		"SCIMUX_UNRELATED_SECRET=synthetic-sentinel",
		"VIBE_HOME=/tmp/vibe-home",
		"MISTRAL_API_KEY=synthetic-not-a-credential",
	})
	want := []string{
		"MISTRAL_API_KEY=synthetic-not-a-credential",
		"PATH=/bin",
		"VIBE_HOME=/tmp/vibe-home",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatal("vibe environment allowlist mismatch")
	}
	if vibeChildEnv(nil) == nil {
		t.Fatal("an empty allowlist must still replace inheritance")
	}
}

func TestOtherHarnessStillInheritsTheProcessEnvironment(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report")
	writeEnvProbe(t, dir, "pi-acp", report)
	setVibeEnvPerimeter(t)
	t.Setenv("PATH", dir)

	proc, err := execRunner("n-pi-env", "pi", dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = proc.Wait()
	got := envMarkers(t, report)
	if !got["SENTINEL"] {
		t.Fatal("pi child lost the surrounding process environment")
	}
	if !got["HAS_MISTRAL_API_KEY"] {
		t.Fatal("pi child lost a variable it previously inherited")
	}
}
