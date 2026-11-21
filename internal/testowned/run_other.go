//go:build !linux || testowned_unsupported

package testowned

import "errors"

// Run fails before launching a process on platforms without the Linux proof.
func Run([]string) Result { return Result{Category: "prerequisite"} }

// RunWithHandles refuses to launch and closes transferred handles.
func RunWithHandles(_ []string, handles Handles) Result {
	handles.close()
	return Result{Category: "prerequisite"}
}

// RunBrowserWithHandles also refuses unsupported platforms.
func RunBrowserWithHandles(args []string, handles Handles) Result {
	return RunWithHandles(args, handles)
}

// DescriptorSealAvailable reports that the explicit-handle seal is unavailable.
func DescriptorSealAvailable() error { return errors.New("unsupported descriptor isolation") }
