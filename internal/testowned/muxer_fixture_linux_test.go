//go:build linux && !testowned_unsupported

package testowned

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMuxerProbeSupervisor(t *testing.T) {
	if os.Getenv("TESTOWNED_MUXER_PROBE") != "1" {
		return
	}
	output := os.NewFile(4, "runner-output")
	testName := "-test.run=^TestMuxerExecParentHelperProcess$"
	if os.Getenv("TESTOWNED_MUXER_NORMAL") == "1" {
		testName = "-test.run=^TestMuxerExecPreservesWorkerAndHasNoOwnershipGap$"
	}
	handles := Handles{
		Stdin: os.Stdin, Stdout: output, Stderr: output, ExtraFiles: []*os.File{os.NewFile(3, "readiness")},
	}
	if os.Getenv("TESTOWNED_MUXER_NORMAL") != "1" {
		handles.ExtraFiles = append(handles.ExtraFiles, os.NewFile(5, "owned runtime"))
	}
	result := RunWithHandles([]string{os.Getenv("TESTOWNED_APP_TEST"), testName}, handles)
	_ = json.NewEncoder(os.Stdout).Encode(result)
	os.Exit(0)
}

func TestMuxerNormalHandoffAssertions(t *testing.T) {
	testMuxerOuterFixture(t, "")
}

func TestMuxerOuterReadinessFailureCleanup(t *testing.T) {
	testMuxerOuterFixture(t, "before-readiness")
}

func TestMuxerOuterInputFailureCleanup(t *testing.T) {
	testMuxerOuterFixture(t, "after-readiness")
}

func TestMuxerOuterLateFailureCleanup(t *testing.T)     { testMuxerOuterObservation(t, "late-after-stop") }
func TestMuxerInitAfterParentCleanup(t *testing.T)      { testMuxerOuterObservation(t, "init-parent") }
func TestMuxerInitAfterDataCleanup(t *testing.T)        { testMuxerOuterObservation(t, "init-data") }
func TestMuxerInitAfterBindCleanup(t *testing.T)        { testMuxerOuterObservation(t, "init-bound") }
func TestMuxerInitChangedIdentityRetained(t *testing.T) { testMuxerOuterObservation(t, "init-changed") }

type muxerObserved struct {
	Stage  string  `json:"stage"`
	Path   string  `json:"path"`
	Dev    uint64  `json:"dev"`
	Ino    uint64  `json:"ino"`
	UID    uint32  `json:"uid"`
	Result *Result `json:"result"`
}

func observedDirectory(event muxerObserved) (bool, error) {
	info, err := os.Lstat(event.Path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || uint64(stat.Dev) != event.Dev || stat.Ino != event.Ino || stat.Uid != event.UID || int(stat.Uid) != os.Getuid() {
		return false, errors.New("observed identity changed")
	}
	return true, nil
}

func testMuxerOuterObservation(t *testing.T, failure string) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires tmux and process exec")
	}
	requireDescriptorSeal(t)
	root, err := os.MkdirTemp("", "testowned-")
	if err != nil {
		t.Fatal(err)
	}
	parentInfo, err := os.Lstat(root)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode().Perm() != 0700 {
		t.Fatal("private parent identity unavailable")
	}
	parentStat, ok := parentInfo.Sys().(*syscall.Stat_t)
	if !ok || int(parentStat.Uid) != os.Getuid() {
		t.Fatal("private parent ownership unavailable")
	}
	retain := true
	var guard *fixtureReaper
	t.Cleanup(func() {
		if retain || guard == nil || !guard.clean {
			t.Logf("private fixture parent retained: %s", root)
			return
		}
		current, err := os.Lstat(root)
		if err != nil || !current.IsDir() || !os.SameFile(parentInfo, current) {
			t.Errorf("private parent changed; retained: %s", root)
			return
		}
		stat, ok := current.Sys().(*syscall.Stat_t)
		if !ok || current.Mode().Perm() != 0700 || stat.Uid != parentStat.Uid || stat.Dev != parentStat.Dev {
			t.Errorf("private parent owner changed; retained: %s", root)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("private parent removal: %v", err)
		}
	})
	guard = installFixtureReaper(t)
	neighborRoot := filepath.Join(root, "neighbor")
	if err := os.Mkdir(neighborRoot, 0700); err != nil {
		t.Fatal(err)
	}
	neighborInfo, err := os.Lstat(neighborRoot)
	if err != nil {
		t.Fatal(err)
	}
	neighbor, input, output := startNeighbor(t)
	defer finishNeighbor(t, guard, neighbor, input, output)
	if err := neighborResponds(neighbor, input, output); err != nil {
		t.Fatal("neighbor unavailable")
	}
	binary := filepath.Join(root, "app-test")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "test", "-c", "-o", binary, "../app")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build synthetic fixture: %v %s", err, output)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	defer readyW.Close()
	logPath, observationPath := filepath.Join(root, "runner.log"), filepath.Join(root, "observation.jsonl")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	runCtx, runCancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer runCancel()
	cmd := exec.CommandContext(runCtx, os.Args[0], "-test.run=^TestMuxerProbeSupervisor$")
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), "TESTOWNED_MUXER_PROBE=1", "TESTOWNED_MUXER_NORMAL=1", "TESTOWNED_APP_TEST="+binary,
		"SCIMUX_MUXER_EXEC_TEST_FAILURE="+failure, "SCIMUX_MUXER_EXEC_TEST_OBSERVATION="+observationPath, "TMPDIR="+root, "TMUX_TMPDIR="+root)
	cmd.ExtraFiles = []*os.File{readyW, logFile}
	var proof bytes.Buffer
	cmd.Stdout = &proof
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = readyW.Close()
	_ = logFile.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("supervisor wait unconfirmed: %v", err)
	}
	var result Result
	if err := json.Unmarshal(proof.Bytes(), &result); err != nil || !result.Clean || result.Category != "clean" || result.RunnerExit != 1 {
		t.Fatalf("outer failure proof invalid: %+v error=%v", result, err)
	}
	logFile, err = os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logBytes, err := io.ReadAll(io.LimitReader(logFile, 65537))
	_ = logFile.Close()
	if err != nil || len(logBytes) > 65536 {
		t.Fatal("synthetic failure output unavailable")
	}
	want := "controlled initialization failure"
	if failure == "late-after-stop" {
		want = "controlled late outer failure"
	}
	if !bytes.Contains(logBytes, []byte(want)) {
		t.Fatal("controlled failure did not run")
	}
	f, err := os.Open(observationPath)
	if err != nil {
		t.Fatal("synthetic observation unavailable")
	}
	limited := &io.LimitedReader{R: f, N: 65537}
	decoder := json.NewDecoder(limited)
	events := make(map[string]muxerObserved)
	for {
		var event muxerObserved
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || events[event.Stage].Stage != "" {
			_ = f.Close()
			t.Fatal("synthetic observation invalid")
		}
		events[event.Stage] = event
	}
	_ = f.Close()
	if limited.N == 0 {
		t.Fatal("synthetic observation too large")
	}
	parent := events["parent"]
	if parent.Stage == "" || filepath.Dir(parent.Path) != root {
		t.Fatal("observed parent not bound to private root")
	}
	data := events["data"]
	if failure != "init-parent" && (data.Stage == "" || filepath.Dir(data.Path) != parent.Path) {
		t.Fatal("observed runtime not bound to parent")
	}
	if failure == "late-after-stop" {
		inner := events["supervisor-result"].Result
		if events["wait-ready"].Stage == "" || inner == nil || !inner.Clean || inner.Category != "clean" || inner.RunnerExit != 0 {
			t.Fatal("inner completed supervisor proof unavailable")
		}
	}
	parentExists, err := observedDirectory(parent)
	if err != nil {
		t.Fatal("observed parent identity changed")
	}
	if failure == "init-changed" {
		original, replacement := events["original"], events["replacement"]
		if !parentExists || original.Stage == "" || replacement.Stage == "" || filepath.Dir(original.Path) != parent.Path || replacement.Path != data.Path || original.Ino != data.Ino || replacement.Ino == original.Ino {
			t.Fatal("changed identities not preserved")
		}
		for _, event := range []muxerObserved{original, replacement} {
			exists, err := observedDirectory(event)
			if err != nil || !exists {
				t.Fatal("changed identity was not retained")
			}
		}
		for _, event := range []muxerObserved{original, replacement} {
			if err := os.Remove(event.Path); err != nil {
				t.Fatal("known synthetic identity not empty")
			}
		}
	} else {
		if parentExists {
			t.Error("owned runtime parent remained after proven cleanup")
		}
		if parentExists && data.Stage != "" {
			dataExists, err := observedDirectory(data)
			if err != nil {
				t.Fatal("observed runtime identity changed")
			}
			if dataExists {
				owned, err := BindRoot(parent.Path, data.Path)
				if err != nil {
					t.Fatal("observed runtime could not be bound")
				}
				if err := owned.RemoveAfter(nil, result); err != nil {
					t.Fatal("observed test runtime retained")
				}
			}
		}
	}
	if parentExists {
		if err := os.Remove(parent.Path); err != nil {
			t.Fatal("observed parent not empty; retained")
		}
	}
	if err := neighborResponds(neighbor, input, output); err != nil {
		t.Fatal("neighbor process did not survive")
	}
	currentNeighbor, err := os.Lstat(neighborRoot)
	if err != nil || !os.SameFile(neighborInfo, currentNeighbor) {
		t.Fatal("neighbor directory did not survive")
	}
	retain = false
}

func testMuxerOuterFixture(t *testing.T, failure string) {
	if testing.Short() {
		t.Skip("requires tmux and process exec")
	}
	requireDescriptorSeal(t)
	root, err := os.MkdirTemp("", "testowned-")
	if err != nil {
		t.Fatal(err)
	}
	parentInfo, err := os.Lstat(root)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode().Perm() != 0700 {
		t.Fatal("private parent identity unavailable")
	}
	parentStat, ok := parentInfo.Sys().(*syscall.Stat_t)
	if !ok || int(parentStat.Uid) != os.Getuid() {
		t.Fatal("private parent ownership unavailable")
	}
	retain := true
	var guard *fixtureReaper
	t.Cleanup(func() {
		if retain || guard == nil || !guard.clean {
			t.Logf("private fixture parent retained: %s", root)
			return
		}
		current, err := os.Lstat(root)
		if err != nil || !current.IsDir() || !os.SameFile(parentInfo, current) {
			t.Errorf("private parent changed; retained: %s", root)
			return
		}
		stat, ok := current.Sys().(*syscall.Stat_t)
		if !ok || current.Mode().Perm() != 0700 || stat.Uid != parentStat.Uid || stat.Dev != parentStat.Dev {
			t.Errorf("private parent owner changed; retained: %s", root)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("private parent removal: %v", err)
		}
	})
	guard = installFixtureReaper(t)
	binary := filepath.Join(root, "app-test")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "test", "-c", "-o", binary, "../app")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build synthetic fixture: %v %s", err, output)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	defer readyW.Close()
	logPath := filepath.Join(root, "runner.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	runCtx, runCancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer runCancel()
	cmd := exec.CommandContext(runCtx, os.Args[0], "-test.run=^TestMuxerProbeSupervisor$")
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), "TESTOWNED_MUXER_PROBE=1", "TESTOWNED_MUXER_NORMAL=1", "TESTOWNED_APP_TEST="+binary, "SCIMUX_MUXER_EXEC_TEST_FAILURE="+failure, "TMPDIR="+root, "TMUX_TMPDIR="+root)
	cmd.ExtraFiles = []*os.File{readyW, logFile}
	var proof bytes.Buffer
	cmd.Stdout = &proof
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = readyW.Close()
	_ = logFile.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("supervisor wait: %v", err)
	}
	var result Result
	expected := 0
	if failure != "" {
		expected = 1
	}
	if err := json.Unmarshal(proof.Bytes(), &result); err != nil || !result.Clean || result.Category != "clean" || result.RunnerExit != expected {
		output, _ := os.ReadFile(logPath)
		t.Fatalf("normal handoff proof: %+v error=%v; output: %s", result, err, output)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal("private fixture parent unavailable")
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "mx-") {
			t.Fatal("outer runtime parent was not removed after proof")
		}
	}
	retain = false
}

// This runs the unchanged handoff helper inside a dedicated supervisor. The
// supervisor is paused while the helper fails its input gate, making the
// interval before descendant cleanup observable without a product hook.
func TestMuxerRootSurvivesEarlyInputFailure(t *testing.T) {
	testMuxerRootBeforeProof(t, "input")
}

func TestMuxerRootSurvivesReadinessFailure(t *testing.T) {
	testMuxerRootBeforeProof(t, "readiness")
}

func TestMuxerRootSurvivesPostExecFailure(t *testing.T) {
	testMuxerRootBeforeProof(t, "exec")
}

func testMuxerRootBeforeProof(t *testing.T, mode string) {
	if testing.Short() {
		t.Skip("requires tmux and process exec")
	}
	requireDescriptorSeal(t)
	root, err := os.MkdirTemp("", "testowned-")
	if err != nil {
		t.Fatal(err)
	}
	parentInfo, err := os.Lstat(root)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode().Perm() != 0700 {
		t.Fatal("private parent identity unavailable")
	}
	parentStat, ok := parentInfo.Sys().(*syscall.Stat_t)
	if !ok || int(parentStat.Uid) != os.Getuid() {
		t.Fatal("private parent ownership unavailable")
	}
	var guard *fixtureReaper
	retain := false
	t.Cleanup(func() {
		if retain || guard == nil || !guard.clean {
			t.Logf("private fixture parent retained: %s", root)
			return
		}
		current, err := os.Lstat(root)
		if err != nil || !current.IsDir() || !os.SameFile(parentInfo, current) {
			t.Errorf("private fixture parent identity changed; retained: %s", root)
			return
		}
		currentStat, ok := current.Sys().(*syscall.Stat_t)
		if !ok || current.Mode().Perm() != 0700 || currentStat.Uid != parentStat.Uid || currentStat.Dev != parentStat.Dev {
			t.Errorf("private fixture parent ownership changed; retained: %s", root)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("private fixture parent removal: %v", err)
		}
	})
	guard = installFixtureReaper(t)
	neighborRoot := filepath.Join(root, "scimux-muxer-exec-neighbor")
	if err := os.Mkdir(neighborRoot, 0700); err != nil {
		t.Fatal(err)
	}
	neighbor, in, out := startNeighbor(t)
	defer finishNeighbor(t, guard, neighbor, in, out)
	dataRoot, err := os.MkdirTemp(root, "r")
	if err != nil {
		t.Fatal(err)
	}
	owned, err := BindRoot(root, dataRoot)
	if err != nil {
		retain = true
		t.Fatal("fixture root registration failed")
	}
	rootHandle, err := os.Open(dataRoot)
	if err != nil {
		retain = true
		t.Fatal("fixture root handle unavailable")
	}
	defer rootHandle.Close()
	if err := owned.VerifyHandle(rootHandle); err != nil {
		retain = true
		t.Fatal("fixture root binding failed")
	}
	rootInfo, err := os.Lstat(dataRoot)
	if err != nil {
		retain = true
		t.Fatal("fixture root identity unavailable")
	}
	appBinary := filepath.Join(root, "app-test")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "test", "-c", "-o", appBinary, "../app")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build synthetic fixture: %v %s", err, output)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	logPath := filepath.Join(root, "runner.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMuxerProbeSupervisor$")
	cmd.Env = append(os.Environ(), "TESTOWNED_MUXER_PROBE=1", "TESTOWNED_APP_TEST="+appBinary,
		"SCIMUX_MUXER_EXEC_PARENT_TEST=1", "SCIMUX_MUXER_EXEC_READY_FD=3", "SCIMUX_MUXER_EXEC_ROOT="+dataRoot, "SCIMUX_MUXER_EXEC_ROOT_FD=4", "TMPDIR="+dataRoot, "TMUX_TMPDIR="+dataRoot)
	cmd.ExtraFiles = []*os.File{writer, logFile, rootHandle}
	gate, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	var proof bytes.Buffer
	cmd.Stdout = &proof
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	_ = logFile.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finished := false
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGCONT)
		_ = gate.Close()
		if finished {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				guard.blocked = true
			}
		}
	}()
	var ready struct {
		DataDir    string         `json:"data_dir"`
		WorkerPIDs map[string]int `json:"worker_pids"`
		MuxerPID   int            `json:"muxer_pid"`
		Addr       string         `json:"addr"`
	}
	if mode == "readiness" {
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if !entry.IsDir() || entry.Name() != filepath.Base(dataRoot) {
					continue
				}
				candidate := filepath.Join(root, entry.Name())
				paths, err := filepath.Glob(filepath.Join(candidate, "control", "workers", "*.json"))
				if err != nil {
					t.Fatal(err)
				}
				for _, path := range paths {
					data, err := os.ReadFile(path)
					if err != nil {
						continue
					}
					var locator struct {
						PID int `json:"pid"`
					}
					if json.Unmarshal(data, &locator) == nil && locator.PID > 0 {
						ready.DataDir, ready.WorkerPIDs = candidate, map[string]int{"worker": locator.PID}
						break
					}
				}
			}
			if ready.DataDir != "" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if ready.DataDir == "" {
			t.Fatal("fixture worker was not published before readiness")
		}
	} else {
		if err := reader.SetReadDeadline(time.Now().Add(45 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(reader).Decode(&ready); err != nil {
			output, _ := os.ReadFile(logPath)
			t.Fatalf("fixture readiness: %v; output: %s", err, output)
		}
	}
	if ready.DataDir != dataRoot || filepath.Dir(ready.DataDir) != root || !strings.HasPrefix(filepath.Base(ready.DataDir), "r") || len(ready.WorkerPIDs) == 0 {
		t.Fatal("fixture root or workers not registered in private parent")
	}
	if err := owned.VerifyHandle(rootHandle); err != nil {
		retain = true
		t.Fatal("fixture root identity unavailable")
	}
	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok || int(rootStat.Uid) != os.Getuid() || rootStat.Dev != parentStat.Dev {
		retain = true
		t.Fatal("fixture root ownership unavailable")
	}
	if mode == "exec" {
		if _, err := gate.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		_ = gate.Close()
		client := &http.Client{Timeout: time.Second}
		deadline := time.Now().Add(20 * time.Second)
		observed := false
		for time.Now().Before(deadline) {
			response, err := client.Get("http://" + ready.Addr + "/state")
			if err == nil {
				var state struct {
					MuxerPID int    `json:"muxer_pid"`
					Build    string `json:"build"`
				}
				err = json.NewDecoder(response.Body).Decode(&state)
				_ = response.Body.Close()
				if err == nil && state.MuxerPID == ready.MuxerPID && state.Build == "muxer-v2" {
					observed = true
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !observed {
			t.Fatal("replacement muxer did not become ready")
		}
	}
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "stat"))
		if err != nil {
			t.Fatal(err)
		}
		end := strings.LastIndex(string(data), ") ")
		if end >= 0 && strings.HasPrefix(string(data[end+2:]), "T ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor did not pause")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mode == "readiness" {
		_ = reader.Close()
	} else if mode == "input" {
		_ = gate.Close()
	} else {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(ready.MuxerPID), "stat"))
		if err != nil {
			t.Fatal(err)
		}
		end := strings.LastIndex(string(data), ") ")
		if end < 0 {
			t.Fatal("runner parent unknown")
		}
		fields := strings.Fields(string(data[end+2:]))
		if len(fields) < 2 || fields[1] != strconv.Itoa(cmd.Process.Pid) {
			t.Fatal("runner is not the supervisor's direct child")
		}
		if err := syscall.Kill(ready.MuxerPID, syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(ready.MuxerPID), "stat"))
			if err != nil {
				t.Fatal(err)
			}
			end := strings.LastIndex(string(data), ") ")
			if end >= 0 && strings.HasPrefix(string(data[end+2:]), "Z ") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("runner termination was not observed")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	deadline = time.Now().Add(10 * time.Second)
	removed := false
	for time.Now().Before(deadline) {
		_, err := os.Stat(ready.DataDir)
		if errors.Is(err, os.ErrNotExist) {
			removed = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if mode == "exec" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	liveWorker := false
	for _, pid := range ready.WorkerPIDs {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			continue
		}
		end := strings.LastIndex(string(data), ") ")
		if end >= 0 && !strings.HasPrefix(string(data[end+2:]), "Z ") {
			liveWorker = true
			break
		}
	}
	if !liveWorker {
		t.Error("no live worker observed while supervisor cleanup was paused")
	}
	if err := neighborResponds(neighbor, in, out); err != nil {
		t.Fatalf("neighbor process: %v", err)
	}
	if _, err := os.Stat(neighborRoot); err != nil {
		t.Fatalf("neighbor root: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatalf("supervisor wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor completion timeout")
	}
	var result Result
	if err := json.Unmarshal(proof.Bytes(), &result); err != nil || !result.Clean || result.RunnerExit == 0 {
		t.Fatalf("failure cleanup proof: %+v error=%v", result, err)
	}
	if current, err := os.Lstat(ready.DataDir); err == nil {
		if !current.IsDir() || !os.SameFile(rootInfo, current) {
			retain = true
			t.Errorf("fixture root changed after observation; private parent retained")
		} else {
			expectedExit := 1
			if mode == "exec" {
				expectedExit = 143
			}
			if err := CheckRunner(result, expectedExit); err != nil {
				t.Error("unexpected fixture runner outcome")
			}
			if err := owned.RemoveAfter(nil, result); err != nil {
				retain = true
				t.Errorf("remove proven fixture root: %v", err)
			}
			if _, err := os.Lstat(ready.DataDir); !errors.Is(err, os.ErrNotExist) {
				retain = true
				t.Error("proven fixture root remains")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		retain = true
		t.Fatalf("fixture root inspection: %v", err)
	}
	if err := neighborResponds(neighbor, in, out); err != nil {
		t.Fatalf("neighbor after cleanup: %v", err)
	}
	if _, err := os.Stat(neighborRoot); err != nil {
		t.Fatalf("neighbor root after cleanup: %v", err)
	}
	if removed && liveWorker {
		t.Error("runtime root removed while a worker was live and supervisor cleanup was paused")
	}
	if removed && !liveWorker {
		t.Error("runtime root removed before supervisor cleanup proof")
	}
}
