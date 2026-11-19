//go:build !linux || testowned_unsupported

package testowned

import "testing"

func TestUnsupportedFailsBeforeLaunch(t *testing.T) {
	result := Run([]string{"synthetic"})
	if result.Clean || result.Category != "prerequisite" {
		t.Fatalf("unsupported result %+v", result)
	}
}
