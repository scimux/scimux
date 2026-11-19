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

func main() { exit(testowned.Command(args, writer, run)) }
