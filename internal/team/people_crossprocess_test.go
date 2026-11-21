package team

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPeopleAdmissionAndDiscardAcrossProcesses(t *testing.T) {
	if dir := os.Getenv("SCIMUX_TEST_PEOPLE_SHARED_DIR"); dir != "" {
		lock, err := os.OpenFile(filepath.Join(dir, ".storage.lock"), os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		probe := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		state := "locked"
		if probe == nil {
			state = "free"
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		} else if !errors.Is(probe, syscall.EWOULDBLOCK) {
			t.Fatal(probe)
		}
		lock.Close()
		if err := os.WriteFile(filepath.Join(dir, "handshake"), []byte(state), 0600); err != nil {
			t.Fatal(err)
		}
		instance, err := ReadPeopleCopyInstance(dir, sampleCopyID())
		if err != nil {
			t.Fatal(err)
		}
		pending := syntheticPeoplePending(syntheticPeopleStage())
		if os.Getenv("SCIMUX_TEST_PEOPLE_COMPLETION") == "1" {
			pending = discardTestPending(t, syntheticPeopleStage())
		}
		seq, err := DiscardPeoplePending(dir, sampleCopyID(), instance, pending)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "outcome"), []byte(fmt.Sprint(seq)), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	if _, err := EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	var child *exec.Cmd
	finished := false
	t.Cleanup(func() {
		if child != nil && !finished {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	seenRetired, started := false, false
	peopleRecordIO.lstat = func(root *os.Root, name string) (os.FileInfo, error) {
		if name == pending.Key+".retired" {
			seenRetired = true
		}
		if seenRetired && !started && name == pending.Key+".json" {
			started = true
			child = exec.Command(os.Args[0], "-test.run=^TestPeopleAdmissionAndDiscardAcrossProcesses$", "-test.timeout=15s")
			child.Env = append(os.Environ(), "SCIMUX_TEST_PEOPLE_SHARED_DIR="+dir)
			if err := child.Start(); err != nil {
				return nil, err
			}
			var signal []byte
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
				signal, _ = os.ReadFile(filepath.Join(dir, "handshake"))
				if len(signal) != 0 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if len(signal) == 0 {
				return nil, errors.New("child handshake timeout")
			}
			if string(signal) == "free" {
				err := child.Wait()
				finished = true
				if err != nil {
					return nil, err
				}
			}
		}
		return original.lstat(root, name)
	}
	_ = SavePeoplePending(dir, id, pending, []PeopleStage{stage})
	peopleRecordIO = original
	if !started {
		t.Fatal("interleaving was not reached")
	}
	if !finished {
		err := child.Wait()
		finished = true
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadPeoplePending(dir, id, pending.Key); err == nil {
		t.Fatal("retired key resurrected")
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
		t.Fatal("retired key admitted")
	}
}

func TestPeopleConfirmationAndDiscardAcrossProcesses(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	if _, err := EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := discardTestPending(t, stage)
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	var child *exec.Cmd
	started, finished := false, false
	t.Cleanup(func() {
		if child != nil && !finished {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	peopleRecordIO.openDir = func(root *os.Root) (*os.File, error) {
		f, err := original.openDir(root)
		if err != nil || started || !strings.Contains(f.Name(), "people-pending") {
			return f, err
		}
		started = true
		child = exec.Command(os.Args[0], "-test.run=^TestPeopleAdmissionAndDiscardAcrossProcesses$", "-test.timeout=15s")
		child.Env = append(os.Environ(), "SCIMUX_TEST_PEOPLE_SHARED_DIR="+dir, "SCIMUX_TEST_PEOPLE_COMPLETION=1")
		if err := child.Start(); err != nil {
			f.Close()
			return nil, err
		}
		var signal []byte
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			signal, _ = os.ReadFile(filepath.Join(dir, "handshake"))
			if len(signal) != 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if len(signal) == 0 {
			f.Close()
			return nil, errors.New("child handshake timeout")
		}
		if string(signal) == "free" {
			err := child.Wait()
			finished = true
			if err != nil {
				f.Close()
				return nil, err
			}
		}
		return f, nil
	}
	confirmErr := ConfirmPeoplePending(dir, id, pending.Key, 1, false)
	peopleRecordIO = original
	if !started {
		t.Fatal("confirmation interleaving not reached")
	}
	if !finished {
		err := child.Wait()
		finished = true
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadPeoplePending(dir, id, pending.Key); err == nil {
		t.Fatal("retired key remains pending")
	}
	outcome, err := os.ReadFile(filepath.Join(dir, "outcome"))
	if err != nil {
		t.Fatal(err)
	}
	if string(outcome) == "0" && confirmErr == nil {
		t.Fatal("confirmation succeeded after discard")
	}
	if string(outcome) == "0" {
		if _, err := ReadPeopleCompletion(dir, id, pending.Key); err == nil {
			t.Fatal("late completion was created")
		}
	}
	if string(outcome) == "1" && confirmErr != nil {
		t.Fatalf("completed without confirmation: %v", confirmErr)
	}
	if string(outcome) != "0" && string(outcome) != "1" {
		t.Fatalf("invalid outcome %q", outcome)
	}
}

func TestPeopleDiscardAndCopyReplacementAcrossProcesses(t *testing.T) {
	if dir := os.Getenv("SCIMUX_TEST_PEOPLE_REPLACE_DIR"); dir != "" {
		lock, err := os.OpenFile(filepath.Join(dir, ".storage.lock"), os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		probe := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		state := "locked"
		if probe == nil {
			state = "free"
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		} else if !errors.Is(probe, syscall.EWOULDBLOCK) {
			t.Fatal(probe)
		}
		lock.Close()
		if err := os.WriteFile(filepath.Join(dir, "replacement-ready"), []byte(state), 0600); err != nil {
			t.Fatal(err)
		}
		id, stage := sampleCopyID(), syntheticPeopleStage()
		if err := RemoveCopy(dir, id); err != nil {
			t.Fatal(err)
		}
		if _, err := EnsurePeopleCopyInstance(dir, id); err != nil {
			t.Fatal(err)
		}
		if err := SavePeopleStage(dir, id, stage); err != nil {
			t.Fatal(err)
		}
		if err := SavePeoplePending(dir, id, discardTestPending(t, stage), []PeopleStage{stage}); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir, id, instance, _, pending := setupDiscard(t)
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	var child *exec.Cmd
	started, finished := false, false
	t.Cleanup(func() {
		if child != nil && !finished {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	peopleRecordIO.sync = func(f *os.File) error {
		if started || !strings.Contains(f.Name(), "people-identity") {
			return original.sync(f)
		}
		started = true
		child = exec.Command(os.Args[0], "-test.run=^TestPeopleDiscardAndCopyReplacementAcrossProcesses$", "-test.timeout=15s")
		child.Env = append(os.Environ(), "SCIMUX_TEST_PEOPLE_REPLACE_DIR="+dir)
		if err := child.Start(); err != nil {
			return err
		}
		var signal []byte
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			signal, _ = os.ReadFile(filepath.Join(dir, "replacement-ready"))
			if len(signal) != 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if len(signal) == 0 {
			return errors.New("replacement handshake timeout")
		}
		if string(signal) == "free" {
			err := child.Wait()
			finished = true
			if err != nil {
				return err
			}
		}
		return original.sync(f)
	}
	_, _ = DiscardPeoplePending(dir, id, instance, pending)
	peopleRecordIO = original
	if !started {
		t.Fatal("identity interleaving not reached")
	}
	if !finished {
		err := child.Wait()
		finished = true
		if err != nil {
			t.Fatal(err)
		}
	}
	newInstance, err := ReadPeopleCopyInstance(dir, id)
	if err != nil || newInstance == instance {
		t.Fatalf("replacement identity missing: %v", err)
	}
	if _, err := ReadPeoplePending(dir, id, pending.Key); err != nil {
		t.Fatalf("replacement pending changed: %v", err)
	}
}
