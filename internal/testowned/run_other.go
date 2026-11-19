//go:build !linux || testowned_unsupported

package testowned

// Run fails before launching a process on platforms without the Linux proof.
func Run([]string) Result { return Result{Category: "prerequisite"} }
