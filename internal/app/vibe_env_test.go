package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionworker"
)

func TestVibeWorkerChildPreservesUserCommandEnvironment(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"{\n" +
		"if [ -n \"$SCIMUX_UNRELATED_SECRET\" ]; then echo SENTINEL; fi\n" +
		"if [ -n \"$VIBE_NOT_A_REAL_SETTING\" ]; then echo VIBE_PREFIX; fi\n" +
		"if [ -n \"$MISTRAL_NOT_A_KEY\" ]; then echo MISTRAL_PREFIX; fi\n" +
		"if [ -n \"$VIBE_HOME\" ]; then echo HAS_VIBE_HOME; fi\n" +
		"if [ -n \"$MISTRAL_API_KEY\" ]; then echo HAS_MISTRAL_API_KEY; fi\n" +
		"if [ -n \"$SSL_CERT_FILE\" ]; then echo HAS_SSL_CERT_FILE; fi\n" +
		"if [ -n \"$PATH\" ]; then echo HAS_PATH; fi\n" +
		"if [ -n \"$LANG\" ]; then echo HAS_LANG; fi\n" +
		"} > '" + strings.ReplaceAll(report, "'", "'\\''") + "'\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "vibe-acp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	workspace := filepath.Join(dir, "workspace")
	for _, path := range []string{data, workspace, filepath.Join(dir, "vibe-home")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("SCIMUX_UNRELATED_SECRET", "synthetic-sentinel")
	t.Setenv("VIBE_NOT_A_REAL_SETTING", "1")
	t.Setenv("MISTRAL_NOT_A_KEY", "1")
	t.Setenv("VIBE_HOME", filepath.Join(dir, "vibe-home"))
	t.Setenv("MISTRAL_API_KEY", "synthetic-not-a-credential")
	t.Setenv("SSL_CERT_FILE", filepath.Join(dir, "ca.pem"))
	t.Setenv("LANG", "C")

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := sessionWorkerConfig{
		DataDir: data,
		NodeID:  "vibe-env",
		Identity: sessionworker.Identity{
			WorkerID: "worker-vibe-env",
			Agent:    "vibe",
		},
	}
	process, err := startSessionWorker(context.Background(), exe, config, sessionWorkerStartOptions{
		args: []string{"-test.run=^TestProductionSessionWorkerHelperProcess$"},
		env:  []string{"SCIMUX_PRODUCTION_SESSION_WORKER_TEST=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if process.client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = process.client.Stop(ctx, true)
		}
		_ = process.waitForExit(3 * time.Second)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = process.client.Launch(ctx, sessionworker.LaunchRequest{
		NodeID: config.NodeID,
		Agent:  "vibe",
		Dir:    workspace,
	})

	body, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("vibe process did not record its environment: %v", err)
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
	for _, required := range []string{"SENTINEL", "VIBE_PREFIX", "MISTRAL_PREFIX", "HAS_VIBE_HOME", "HAS_MISTRAL_API_KEY", "HAS_SSL_CERT_FILE", "HAS_PATH", "HAS_LANG"} {
		if !got[required] {
			t.Fatalf("vibe process missing %s", required)
		}
	}
}
