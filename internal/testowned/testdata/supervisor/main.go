package main

import (
	"github.com/scimux/scimux/internal/testowned"
	"io"
	"os"
)

var args = os.Args[1:]
var writer io.Writer = os.Stdout
var exit = os.Exit
var run = testowned.Run

func main() {
	if os.Getenv("TESTOWNED_FORWARD_STREAMS") == "1" {
		run = func(args []string) testowned.Result {
			handles := testowned.Handles{
				Stdin:  os.Stdin,
				Stdout: os.NewFile(3, "runner-stdout"),
				Stderr: os.NewFile(4, "runner-stderr"),
			}
			if os.Getenv("TESTOWNED_BROWSER_TEARDOWN") == "1" {
				return testowned.RunBrowserWithHandles(args, handles)
			}
			return testowned.RunWithHandles(args, handles)
		}
	}
	exit(testowned.Command(args, writer, run))
}
