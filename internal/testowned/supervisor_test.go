package testowned

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"syscall"
	"testing"
	"time"
)

type fakeKernel struct {
	prepareErr, startErr, runnerErr, childrenErr error
	emptyErr                                     error
	exit                                         int
	childrenLists                                [][]int
	waits                                        map[int][]bool
	waitErr                                      map[int]error
	signalErr                                    map[int]error
	events                                       []string
}

func (f *fakeKernel) prepare() error { f.events = append(f.events, "prepare"); return f.prepareErr }
func (f *fakeKernel) start() error   { f.events = append(f.events, "start"); return f.startErr }
func (f *fakeKernel) waitRunner() (int, error) {
	f.events = append(f.events, "runner")
	return f.exit, f.runnerErr
}
func (f *fakeKernel) children() ([]int, error) {
	f.events = append(f.events, "children")
	if f.childrenErr != nil {
		return nil, f.childrenErr
	}
	if len(f.childrenLists) == 0 {
		return nil, f.emptyErr
	}
	x := f.childrenLists[0]
	f.childrenLists = f.childrenLists[1:]
	return x, nil
}
func (f *fakeKernel) wait(pid int) (bool, error) {
	f.events = append(f.events, "wait")
	if e := f.waitErr[pid]; e != nil {
		return false, e
	}
	a := f.waits[pid]
	if len(a) == 0 {
		return false, nil
	}
	f.waits[pid] = a[1:]
	return a[0], nil
}
func (f *fakeKernel) signal(pid, sig int) error {
	f.events = append(f.events, fmt.Sprintf("signal:%d:%d", pid, sig))
	return f.signalErr[pid]
}
func (f *fakeKernel) pause(time.Duration) {}

func TestSupervisorBehavior(t *testing.T) {
	t.Run("normal runner and no descendants", func(t *testing.T) {
		f := &fakeKernel{exit: 7}
		got := supervise(f, 1, 3)
		if !got.Clean || got.RunnerExit != 7 || got.Category != "clean" {
			t.Fatalf("runner outcome not preserved: %+v", got)
		}
		if !reflect.DeepEqual(f.events, []string{"prepare", "start", "runner", "children"}) {
			t.Fatalf("order %v", f.events)
		}
	})
	t.Run("preparation failure prevents launch", func(t *testing.T) {
		f := &fakeKernel{prepareErr: errors.New("synthetic")}
		got := supervise(f, 1, 3)
		if got.Clean || got.Category != "prerequisite" || !reflect.DeepEqual(f.events, []string{"prepare"}) {
			t.Fatalf("unexpected result %+v events %v", got, f.events)
		}
	})
	t.Run("owned live orphan waited before signaling and reaped", func(t *testing.T) {
		f := &fakeKernel{childrenLists: [][]int{{42}, {42}, {}}, waits: map[int][]bool{42: {false, true}}}
		got := supervise(f, 1, 3)
		if !got.Clean || got.Reaped != 1 || !reflect.DeepEqual(f.events, []string{"prepare", "start", "runner", "children", "wait", "signal:42:15", "children", "wait", "children"}) {
			t.Fatalf("result %+v events %v", got, f.events)
		}
	})
}

func TestSupervisorFailures(t *testing.T) {
	synthetic := errors.New("synthetic")
	for _, tc := range []struct {
		name     string
		f        *fakeKernel
		category string
		absent   string
	}{
		{"start", &fakeKernel{startErr: synthetic}, "start", "runner"},
		{"runner wait", &fakeKernel{runnerErr: synthetic}, "runner", "children"},
		{"child enumeration", &fakeKernel{childrenErr: synthetic}, "children", "signal:42:15"},
		{"not a child", &fakeKernel{childrenLists: [][]int{{42}}, waitErr: map[int]error{42: synthetic}}, "wait", "signal:42:15"},
		{"invalid pid", &fakeKernel{childrenLists: [][]int{{0}}}, "children", "signal:42:15"},
		{"term failure", &fakeKernel{childrenLists: [][]int{{42}}, signalErr: map[int]error{42: synthetic}}, "signal", "children"},
		{"timeout", &fakeKernel{childrenLists: [][]int{{42}, {42}, {42}, {42}}}, "timeout", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := supervise(tc.f, 1, 3)
			if got.Clean || got.Category != tc.category {
				t.Fatalf("result %+v", got)
			}
			if tc.absent != "" {
				n := 0
				for _, e := range tc.f.events {
					if e == tc.absent {
						n++
					}
				}
				if (tc.name == "term failure" && n != 1) || (tc.name != "term failure" && n != 0) {
					t.Fatalf("events %v", tc.f.events)
				}
			}
		})
	}
}

func TestSupervisorEscalationAndAdoption(t *testing.T) {
	f := &fakeKernel{childrenLists: [][]int{{3}, {3}, {3}, {4}, {}}, waits: map[int][]bool{3: {false, false, true}, 4: {true}}}
	got := supervise(f, 1, 8)
	if !got.Clean || got.Reaped != 2 {
		t.Fatalf("result %+v", got)
	}
	want := []string{"prepare", "start", "runner", "children", "wait", "signal:3:15", "children", "wait", "signal:3:9", "children", "wait", "children", "wait", "children"}
	if !reflect.DeepEqual(f.events, want) {
		t.Fatalf("events %v", f.events)
	}
}

func TestEmptyEnumerationRequiresKernelNoChildrenProof(t *testing.T) {
	f := &fakeKernel{childrenErr: errors.New("synthetic kernel ownership unknown")}
	got := supervise(f, 1, 3)
	if got.Clean {
		t.Fatal("uncertain kernel child state was reported clean")
	}
}

func TestEmptyListingWithoutECHILDIsNotClean(t *testing.T) {
	f := &fakeKernel{emptyErr: errors.New("children state uncertain")}
	got := supervise(f, 1, 3)
	if got.Clean || got.Category != "children" {
		t.Fatalf("empty listing without ECHILD accepted: %+v", got)
	}
}

func TestAwaitRunner(t *testing.T) {
	t.Run("exact exit", func(t *testing.T) {
		done := make(chan runnerWait, 1)
		done <- runnerWait{code: 17}
		code, err := awaitRunner(done, nil, nil, nil, nil, 1, 3)
		if err != nil || code != 17 {
			t.Fatalf("code %d err %v", code, err)
		}
	})
	t.Run("wait failure", func(t *testing.T) {
		done := make(chan runnerWait, 1)
		done <- runnerWait{err: errors.New("synthetic")}
		_, err := awaitRunner(done, nil, nil, nil, nil, 1, 3)
		if err == nil {
			t.Fatal("wait error lost")
		}
	})
	t.Run("first signal only then kill", func(t *testing.T) {
		done := make(chan runnerWait, 1)
		signals := make(chan os.Signal, 2)
		ticks := make(chan time.Time, 2)
		signals <- syscall.SIGINT
		signals <- syscall.SIGTERM
		var sent []int
		f := func(sig os.Signal) error { sent = append(sent, int(sig.(syscall.Signal))); return nil }
		k := func() error { sent = append(sent, 9); done <- runnerWait{code: 137}; return nil }
		go func() { time.Sleep(10 * time.Millisecond); ticks <- time.Time{}; ticks <- time.Time{} }()
		code, err := awaitRunner(done, signals, ticks, f, k, 1, 3)
		if err != nil || code != 137 || !reflect.DeepEqual(sent, []int{2, 9}) {
			t.Fatalf("code %d signals %v err %v", code, sent, err)
		}
	})
	t.Run("forward failure", func(t *testing.T) {
		signals := make(chan os.Signal, 1)
		signals <- syscall.SIGTERM
		_, err := awaitRunner(make(chan runnerWait), signals, nil, func(os.Signal) error { return errors.New("synthetic") }, nil, 1, 3)
		if err == nil {
			t.Fatal("forward failure hidden")
		}
	})
	t.Run("kill failure", func(t *testing.T) {
		signals := make(chan os.Signal, 1)
		signals <- syscall.SIGINT
		ticks := make(chan time.Time, 1)
		go func() { time.Sleep(10 * time.Millisecond); ticks <- time.Time{} }()
		_, err := awaitRunner(make(chan runnerWait), signals, ticks, func(os.Signal) error { return nil }, func() error { return errors.New("synthetic") }, 1, 3)
		if err == nil {
			t.Fatal("kill failure hidden")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		signals := make(chan os.Signal, 1)
		signals <- syscall.SIGINT
		ticks := make(chan time.Time, 3)
		go func() {
			time.Sleep(10 * time.Millisecond)
			for i := 0; i < 3; i++ {
				ticks <- time.Time{}
			}
		}()
		_, err := awaitRunner(make(chan runnerWait), signals, ticks, func(os.Signal) error { return nil }, func() error { return nil }, 1, 3)
		if err == nil {
			t.Fatal("timeout hidden")
		}
	})
}

func TestCommandResults(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		result Result
		called bool
		exit   int
		want   string
	}{
		{nil, Result{}, false, 2, "status prerequisite\n"},
		{[]string{"--"}, Result{}, false, 2, "status prerequisite\n"},
		{[]string{"invalid", "x"}, Result{}, false, 2, "status prerequisite\n"},
		{[]string{"--", "synthetic"}, Result{RunnerExit: 7, Clean: true, Category: "clean", Reaped: 3}, true, 0, "status clean\nrunner_exit 7\nreaped 3\n"},
		{[]string{"--", "synthetic"}, Result{Category: "wait"}, true, 1, "status wait\nrunner_exit 0\nreaped 0\n"},
		{[]string{"--", "synthetic"}, Result{Category: "unsafe private text"}, true, 1, "status failure\nrunner_exit 0\nreaped 0\n"},
	} {
		var out bytes.Buffer
		called := false
		code := Command(tc.args, &out, func(got []string) Result {
			called = true
			if !reflect.DeepEqual(got, []string{"synthetic"}) {
				t.Fatalf("arguments %v", got)
			}
			return tc.result
		})
		if called != tc.called || code != tc.exit || out.String() != tc.want {
			t.Fatalf("got called %v exit %d output %q", called, code, out.String())
		}
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("synthetic") }
func TestCommandErrorsAndCategories(t *testing.T) {
	if got := Command([]string{"--", ""}, io.Discard, nil); got != 2 {
		t.Fatalf("missing runner %d", got)
	}
	if got := Command([]string{"--", "x"}, failWriter{}, func([]string) Result { return Result{Category: "clean", Clean: true} }); got != 1 {
		t.Fatalf("write failure %d", got)
	}
	for _, category := range []string{"prerequisite", "start", "runner", "children", "wait", "signal", "timeout", "clean"} {
		var out bytes.Buffer
		got := Command([]string{"--", "x"}, &out, func([]string) Result { return Result{Category: category} })
		want := 1
		if category == "prerequisite" {
			want = 2
		}
		if got != want {
			t.Fatalf("category %s exited %d", category, got)
		}
	}
}

func TestReapedPIDCanBeReusedOnlyAsNewKernelChild(t *testing.T) {
	f := &fakeKernel{childrenLists: [][]int{{33}, {33}, {33}, {}}, waits: map[int][]bool{33: {true, false, true}}}
	got := supervise(f, 1, 8)
	if !got.Clean || got.Reaped != 2 {
		t.Fatalf("result %+v", got)
	}
	want := []string{"prepare", "start", "runner", "children", "wait", "children", "wait", "signal:33:15", "children", "wait", "children"}
	if !reflect.DeepEqual(f.events, want) {
		t.Fatalf("events %v", f.events)
	}
}
func TestDuplicatePIDAndKillFailure(t *testing.T) {
	duplicate := &fakeKernel{childrenLists: [][]int{{7, 7}}}
	got := supervise(duplicate, 1, 3)
	if got.Clean || got.Category != "children" {
		t.Fatalf("duplicate accepted %+v", got)
	}
	fail := &fakeKernel{childrenLists: [][]int{{9}, {9}}, waits: map[int][]bool{9: {false, false}}}
	fail.signalErr = map[int]error{}
	// Inject failure at escalation only, while recording exact signals.
	k := &killFailureKernel{fakeKernel: fail}
	got = supervise(k, 1, 4)
	if got.Clean || got.Category != "signal" || !reflect.DeepEqual(k.signals, []string{"9:15", "9:9"}) {
		t.Fatalf("result %+v signals %v", got, k.signals)
	}
}

type killFailureKernel struct {
	*fakeKernel
	signals []string
}

func (k *killFailureKernel) signal(pid, sig int) error {
	k.signals = append(k.signals, fmt.Sprintf("%d:%d", pid, sig))
	if sig == 9 {
		return errors.New("synthetic")
	}
	return nil
}

func TestRunnerTickBeforeSignalDoesNotKill(t *testing.T) {
	done := make(chan runnerWait, 1)
	ticks := make(chan time.Time, 1)
	ticks <- time.Time{}
	called := false
	go func() { time.Sleep(10 * time.Millisecond); done <- runnerWait{code: 5} }()
	code, err := awaitRunner(done, nil, ticks, nil, func() error { called = true; return nil }, 1, 2)
	if err != nil || code != 5 || called {
		t.Fatalf("code=%d kill=%v error=%v", code, called, err)
	}
}

func TestCommandPrerequisiteExitIsConsistent(t *testing.T) {
	var invalid, unsupported bytes.Buffer
	bad := Command(nil, &invalid, func([]string) Result { t.Fatal("invalid input launched runner"); return Result{} })
	missing := Command([]string{"--", "synthetic"}, &unsupported, func([]string) Result { return Result{Category: "prerequisite"} })
	if bad != 2 || missing != 2 {
		t.Fatalf("prerequisite exit codes: invalid=%d unsupported=%d", bad, missing)
	}
}
