package muse

import (
	"os"
	"strings"
	"testing"
)

// The Vibe review seams are unrelated to Muse. Production must keep the
// previous callback and publication checks.
func TestMuseProductionHasNoReviewSeams(t *testing.T) {
	body, err := os.ReadFile("manager.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	for _, name := range []string{"musePublishCheck", "museStopOverride", "callbackStopped"} {
		if strings.Contains(src, name) {
			t.Fatalf("muse production still defines %s", name)
		}
	}
}
