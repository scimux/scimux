//go:build !linux || testowned_unsupported

package testowned

import "testing"

func TestUnsupportedRootFailsClosed(t *testing.T) {
	if RootSupported() {
		t.Fatal("unsupported root support claimed")
	}
	r, err := BindRoot("synthetic", "synthetic")
	if err == nil || r != nil {
		t.Fatal("unsupported root accepted")
	}
	if err := r.VerifyHandle(nil); err == nil {
		t.Fatal("unsupported handle accepted")
	}
	if err := r.RemoveAfter(nil, Result{}); err == nil {
		t.Fatal("unsupported removal accepted")
	}
	if err := r.RemoveUnstarted(); err == nil {
		t.Fatal("unsupported unstarted removal accepted")
	}
	p, err := BindParent("synthetic")
	if err == nil || p != nil {
		t.Fatal("unsupported parent accepted")
	}
	if err := p.RegisterChild("synthetic"); err == nil {
		t.Fatal("unsupported child accepted")
	}
	if err := p.RemoveParent(); err == nil {
		t.Fatal("unsupported parent removal accepted")
	}
	if err := p.RemoveUnstarted(); err == nil {
		t.Fatal("unsupported unstarted parent removal accepted")
	}
}
