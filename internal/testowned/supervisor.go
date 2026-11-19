// Package testowned supervises processes created exclusively by test tooling.
package testowned

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

type Result struct {
	RunnerExit int
	Clean      bool
	Category   string
	Reaped     int
}

// Handles transfers ownership of explicitly supplied runner descriptors to
// the supervisor. They are closed after the launch attempt, including when
// preparation or launch fails. Nil fields preserve the ordinary Run defaults.
type Handles struct {
	Stdin, Stdout, Stderr *os.File
	ExtraFiles            []*os.File
}

func (h Handles) close() {
	files := append([]*os.File{h.Stdin, h.Stdout, h.Stderr}, h.ExtraFiles...)
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

type kernel interface {
	prepare() error
	start() error
	waitRunner() (int, error)
	children() ([]int, error)
	wait(int) (bool, error)
	signal(int, int) error
	pause(time.Duration)
}

func supervise(k kernel, grace, limit int) Result {
	result := Result{}
	if err := k.prepare(); err != nil {
		result.Category = "prerequisite"
		return result
	}
	if err := k.start(); err != nil {
		result.Category = "start"
		return result
	}
	exit, err := k.waitRunner()
	if err != nil {
		result.Category = "runner"
		return result
	}
	result.RunnerExit = exit
	ages := make(map[int]int)
	for tick := 0; tick < limit; tick++ {
		children, err := k.children()
		if err != nil {
			result.Category = "children"
			return result
		}
		if len(children) == 0 {
			result.Clean = true
			result.Category = "clean"
			return result
		}
		seen := make(map[int]bool)
		for _, pid := range children {
			if pid <= 0 || seen[pid] {
				result.Category = "children"
				return result
			}
			seen[pid] = true
			dead, err := k.wait(pid)
			if err != nil {
				result.Category = "wait"
				return result
			}
			if dead {
				delete(ages, pid)
				result.Reaped++
				continue
			}
			age, exists := ages[pid]
			if !exists || age == grace {
				sig := 15
				if exists {
					sig = 9
				}
				if err := k.signal(pid, sig); err != nil {
					result.Category = "signal"
					return result
				}
			}
			ages[pid] = age + 1
		}
		k.pause(10 * time.Millisecond)
	}
	result.Category = "timeout"
	return result
}

type runnerWait struct {
	code int
	err  error
}

func awaitRunner(done <-chan runnerWait, signals <-chan os.Signal, ticks <-chan time.Time, forward func(os.Signal) error, kill func() error, grace, limit int) (int, error) {
	interrupted := false
	age := 0
	for {
		select {
		case result := <-done:
			return result.code, result.err
		case sig := <-signals:
			if !interrupted {
				interrupted = true
				if err := forward(sig); err != nil {
					return 0, err
				}
			}
		case <-ticks:
			if interrupted {
				age++
				if age == grace {
					if err := kill(); err != nil {
						return 0, err
					}
				}
				if age >= limit {
					return 0, errors.New("runner timeout")
				}
			}
		}
	}
}

func Command(args []string, out io.Writer, run func([]string) Result) int {
	if len(args) < 2 || args[0] != "--" || args[1] == "" {
		_, _ = fmt.Fprintln(out, "status prerequisite")
		return 2
	}
	result := run(args[1:])
	category := result.Category
	switch category {
	case "clean", "prerequisite", "start", "runner", "children", "wait", "signal", "timeout":
	default:
		category = "failure"
	}
	if category == "clean" && !result.Clean {
		category = "failure"
	}
	if category != "clean" {
		result.Clean = false
	}
	if _, err := fmt.Fprintf(out, "status %s\nrunner_exit %d\nreaped %d\n", category, result.RunnerExit, result.Reaped); err != nil {
		return 1
	}
	if category == "prerequisite" {
		return 2
	}
	if !result.Clean {
		return 1
	}
	return 0
}

// CheckRunner evaluates the runner outcome independently of cleanup proof.
func CheckRunner(result Result, expectedExit int) error {
	if result.RunnerExit != expectedExit {
		return errors.New("unexpected test runner outcome")
	}
	return nil
}
