package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunClaudeNotifyHookWritesPermissionPrompt(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "notify"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claudeNotifyEvent{
		HookEventName:    "Notification",
		NotificationType: "permission_prompt",
		SessionID:        hookSIDOwn,
		Message:          "Claude needs your permission",
	})
	var out, errBuf bytes.Buffer
	if err := RunClaudeNotifyHook(dir, bytes.NewReader(payload), &out, &errBuf); err != nil {
		t.Fatalf("notify hook: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("notify hook must write no stdout, got %q", out.String())
	}
	ents, err := os.ReadDir(filepath.Join(dir, "notify"))
	if err != nil {
		t.Fatal(err)
	}
	var ready int
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			ready++
		}
	}
	if ready != 1 {
		t.Fatalf("ready notify files = %d, want 1", ready)
	}
}

func TestRunClaudeNotifyHookRejectsIdlePrompt(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "notify"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claudeNotifyEvent{
		HookEventName:    "Notification",
		NotificationType: "idle_prompt",
		SessionID:        hookSIDOwn,
	})
	if err := RunClaudeNotifyHook(dir, bytes.NewReader(payload), nil, nil); err == nil {
		t.Fatal("idle_prompt must be rejected")
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "notify"))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("idle_prompt wrote %s", e.Name())
		}
	}
}
