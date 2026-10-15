package uninstall

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestInheritedOwnerDescriptorHelper(t *testing.T) {
	role := os.Getenv("SUBYARD_TEST_OWNER_ROLE")
	if role == "" {
		return
	}
	root := os.Getenv("SUBYARD_TEST_OWNER_ROOT")
	_, release, err := OwnerLock(context.Background(), filepath.Join(root, "data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if role == "native-parent" {
		UseArtifactGuard(root)
		if err := nativeArtifact("barrier-fixture", nil, nil); err != nil {
			t.Fatal(err)
		}
		return
	}
	if role == "parent" {
		borrowed, err := BorrowOwnerLock(filepath.Join(root, "data"))
		if err != nil || borrowed == nil {
			t.Fatal(err)
		}
		defer borrowed.Close()
		child := exec.Command(os.Args[0], "-test.run=^TestInheritedOwnerDescriptorHelper$")
		child.Env = append(os.Environ(), "SUBYARD_TEST_OWNER_ROLE=child", "SUBYARD_OWNER_LOCK_FD=3")
		child.ExtraFiles = []*os.File{borrowed}
		if err = child.Start(); err != nil {
			t.Fatal(err)
		}
		if err = child.Wait(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err = os.WriteFile(filepath.Join(root, "ready"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(root, "finish")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("inherited descriptor fixture timed out")
}

func TestInheritedOwnerBarrierSurvivesParentInterruption(t *testing.T) {
	for _, role := range []string{"parent", "native-parent"} {
		t.Run(role, func(t *testing.T) {
			root := testkit.TempDir(t)
			if role == "native-parent" {
				directory := filepath.Join(root, "scripts", "lib")
				if err := os.MkdirAll(directory, 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(directory, "teardown-plan.py"), []byte(`import os,pathlib,time
root=pathlib.Path(__file__).resolve().parents[2]
(root/"ready").write_text(str(os.getpid()))
deadline=time.monotonic()+10
while not (root/"finish").exists():
 if time.monotonic()>deadline:raise SystemExit(1)
 time.sleep(0.01)
`), 0700)
			}
			parent := exec.Command(os.Args[0], "-test.run=^TestInheritedOwnerDescriptorHelper$")
			parent.Env = append(os.Environ(), "SUBYARD_TEST_OWNER_ROLE="+role, "SUBYARD_TEST_OWNER_ROOT="+root)
			if err := parent.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = parent.Process.Kill(); _ = parent.Wait() }()
			deadline := time.Now().Add(3 * time.Second)
			var childPID int
			for time.Now().Before(deadline) {
				body, err := os.ReadFile(filepath.Join(root, "ready"))
				if err == nil {
					if pid, parseErr := strconv.Atoi(string(body)); parseErr == nil && pid > 0 {
						childPID = pid
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			if childPID == 0 {
				t.Fatal("inherited child never acquired barrier")
			}
			defer syscall.Kill(childPID, syscall.SIGKILL)
			if err := parent.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = parent.Wait()
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			_, release, err := OwnerLock(ctx, filepath.Join(root, "data"), nil)
			cancel()
			if err == nil {
				release()
				t.Fatal("parent interruption released inherited child barrier")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "finish"), nil, 0600)
			ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, release, err = OwnerLock(ctx, filepath.Join(root, "data"), nil)
			if err != nil {
				t.Fatal("child did not release inherited barrier", err)
			}
			release()
		})
	}

}

func TestBorrowedReleasePreservesParentAndRejectsForeignScope(t *testing.T) {
	root := testkit.TempDir(t)
	home := filepath.Join(root, "data")
	_, release, err := OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	borrowed, err := BorrowOwnerLock(home)
	if err != nil || borrowed == nil {
		t.Fatal(err)
	}
	defer borrowed.Close()
	t.Setenv("SUBYARD_OWNER_LOCK_FD", strconv.Itoa(int(borrowed.Fd())))
	_, childRelease, err := OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	childRelease()
	retained, err := BorrowOwnerLock(home)
	if err != nil || retained == nil {
		t.Fatal("borrowed release lost parent delegation", err)
	}
	retained.Close()
	if _, foreignRelease, err := OwnerLock(context.Background(), home+"-foreign", nil); err == nil {
		foreignRelease()
		t.Fatal("inherited barrier admitted foreign installation")
	}
	t.Setenv("SUBYARD_OWNER_LOCK_FD", "")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, otherRelease, err := OwnerLock(ctx, home, nil)
	if err == nil {
		otherRelease()
		t.Fatal("borrowed release unlocked parent")
	}
}

func TestQueuedDirectInstallerRejectsCompletedUninstall(t *testing.T) {
	root := testkit.TempDir(t)
	home := filepath.Join(root, "data")
	runtime := filepath.Join(root, "runtime")
	if err := os.Mkdir(runtime, 0700); err != nil {
		t.Fatal(err)
	}
	_, release, err := OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	installer, err := filepath.Abs("../../scripts/install-runtime-release.sh")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", installer, "--runtime-root", runtime)
	command.Env = append(os.Environ(), "SUBYARD_HOME="+home, "SUBYARD_INSTALL_OWNER_FD=", "SUBYARD_INSTALL_OWNER_EPOCH=")
	var output strings.Builder
	command.Stdout = &output
	command.Stderr = &output
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill() }()
	// The native installer captures epoch/target generation before opening this
	// descriptor. Observe that actual descriptor rather than racing a fixed sleep.
	deadline := time.Now().Add(3 * time.Second)
	waiting := false
	var owns func(int) bool
	owns = func(pid int) bool {
		names, _ := filepath.Glob(filepath.Join("/proc", strconv.Itoa(pid), "fd", "*"))
		for _, name := range names {
			target, _ := os.Readlink(name)
			if target == OwnerLockPath(home) {
				return true
			}
		}
		children, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", strconv.Itoa(pid), "children"))
		for _, child := range strings.Fields(string(children)) {
			childPID, _ := strconv.Atoi(child)
			if owns(childPID) {
				return true
			}
		}
		return false
	}
	for time.Now().Before(deadline) {
		if owns(command.Process.Pid) {
			waiting = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("native installer never waited at owner barrier")
	}
	if err = AdvanceOwnerEpoch(home, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(runtime); err != nil {
		t.Fatal(err)
	}
	release()
	err = command.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 75 {
		t.Fatalf("queued installer did not reject removed generation: %v %s", err, &output)
	}
	if _, err = os.Stat(runtime); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("queued installer recreated runtime", err)
	}
}
