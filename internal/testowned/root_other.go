//go:build !linux || testowned_unsupported

package testowned

import (
	"errors"
	"os"
)

type Root struct{}

func RootSupported() bool { return false }

// BindRoot fails closed where the Linux test ownership proof is unavailable.
func BindRoot(string, string) (*Root, error)  { return nil, errors.New("test root unsupported") }
func (*Root) VerifyHandle(*os.File) error     { return errors.New("test root unsupported") }
func (*Root) RemoveAfter(error, Result) error { return errors.New("test root unsupported") }
func (*Root) RemoveUnstarted() error          { return errors.New("test root unsupported") }

type Parent struct{}

func BindParent(string) (*Parent, error)   { return nil, errors.New("test parent unsupported") }
func (*Parent) RegisterChild(string) error { return errors.New("test parent unsupported") }
func (*Parent) RemoveParent() error        { return errors.New("test parent unsupported") }
func (*Parent) RemoveUnstarted() error     { return errors.New("test parent unsupported") }
