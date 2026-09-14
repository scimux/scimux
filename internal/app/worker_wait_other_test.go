//go:build !linux

package app

import "testing"

func killUnwaitedWorkerAndAwaitZombie(t *testing.T, _ int) {
	t.Helper()
	t.Skip("uses Linux waitid to observe completed exit without reaping")
}
