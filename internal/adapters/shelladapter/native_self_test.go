package shelladapter

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

type fixedSelfTestCapability struct {
	file   *os.File
	digest string
}

func (c fixedSelfTestCapability) Borrow() (*os.File, string, error) {
	fd, err := syscall.Dup(int(c.file.Fd()))
	if err != nil {
		return nil, "", err
	}
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "fixture retained self"), c.digest, nil
}

type fixedSelfObserver struct{ adapterNativeObserver }

func (o *fixedSelfObserver) CommandStarted(kind, source string) (uint64, error) {
	if kind != "vm-cpu-self" {
		return 0, fmt.Errorf("wrong fixed leaf %q", kind)
	}
	return o.adapterNativeObserver.CommandStarted("adapter-script", source)
}
func fixedSelfContext(t *testing.T, root, body string) (context.Context, *fixedSelfObserver) {
	t.Helper()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(bin, "sudo"), []byte("#!/bin/sh\nset -eu\nshift 2\nexec 3>&-\nexec \"$@\"\n"), 0700)
	path := filepath.Join(root, "engine")
	testkit.WriteFile(t, path, []byte(body), 0700)
	image, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { image.Close() })
	sum := sha256.Sum256([]byte(body))
	// The pathname changes after the capability retains the actual image.
	replacement := path + ".replacement"
	testkit.WriteFile(t, replacement, []byte("#!/bin/sh\nexit 77\n"), 0700)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	observer := &fixedSelfObserver{adapterNativeObserver: adapterNativeObserver{root: root}}
	ctx := ports.WithNativeOwnerHome(ports.WithNativeOperationObserver(context.Background(), observer), root)
	return ports.WithNativeSelfExecutable(ctx, fixedSelfTestCapability{image, fmt.Sprintf("%x", sum)}), observer
}
func TestNativeFixedSelfUsesRetainedImageAndLeavesScriptBound(t *testing.T) {
	root := testkit.TempDir(t)
	_, release, err := uninstall.OwnerLock(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// Over32MiB is valid only through the trusted self capability; source scripts
	// still fail their original32MiB guard before dispatch.
	body := "#!/bin/sh\nset -eu\n[ \"$1\" = _vm-cpu ]\n[ \"$2\" = native-apply ]\n[ \"$6\" = original ]\n[ -f /proc/self/fd/0 ]\nprintf applied > \"$8/effect\"\nexit 0\n#" + string(make([]byte, 32<<20))
	ctx, observer := fixedSelfContext(t, root, body)
	request := NativeVMCPU{Project: "fixture", Instance: "fixture", UUID: "original"}
	if err := runNativeVMCPU(ctx, request, io.Discard, filepath.Join(root, "bin/sudo")); err != nil {
		t.Fatal(err)
	}
	if !observer.terminal || observer.uncertain {
		t.Fatal("retained fixed leaf lost completion")
	}
	if raw, err := os.ReadFile(filepath.Join(root, "effect")); err != nil || string(raw) != "applied" {
		t.Fatal("replacement executable ran", err)
	}
	cap := ports.NativeSelfExecutableFrom(ctx)
	file, _, err := cap.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	command := exec.Command(fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), file.Fd()))
	if err := RunNativeCommand(ctx, command, nil, nil, nil); err == nil {
		t.Fatal("large source script escaped original bound")
	}
}

func TestNativeFixedSelfLostHelperAndSurvivingDescendantKeepOriginalOFD(t *testing.T) {
	if root := os.Getenv("SUBYARD_SELF_CUSTODY_ROOT"); root != "" {
		_, release, err := uninstall.OwnerLock(context.Background(), root, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		mode := os.Getenv("SUBYARD_SELF_CUSTODY_MODE")
		body := "#!/bin/sh\nset -eu\nroot=$8\nprintf 'once\\n' >>\"$root/effects\"\n"
		if mode == "descendant" {
			body += "exec 9<&0\n( echo $$ >\"$root/pid\"; while [ ! -f \"$root/release\" ]; do sleep .01; done ) <&9 >/dev/null 2>&1 &\nexit 0\n"
		} else {
			body += "echo $$ >\"$root/pid\"\nwhile [ ! -f \"$root/release\" ]; do sleep .01; done\n"
		}
		ctx, observer := fixedSelfContext(t, root, body)
		err = runNativeVMCPU(ctx, NativeVMCPU{Project: "fixture", Instance: "fixture", UUID: "original"}, io.Discard, filepath.Join(root, "bin/sudo"))
		if mode == "descendant" {
			if err == nil || observer.terminal || !observer.uncertain {
				t.Fatal("surviving mutator became complete")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, mode := range []string{"loss", "descendant"} {
		t.Run(mode, func(t *testing.T) {
			root := testkit.TempDir(t)
			child := exec.Command(os.Args[0], "-test.run=^TestNativeFixedSelfLostHelperAndSurvivingDescendantKeepOriginalOFD$")
			child.Env = append(os.Environ(), "SUBYARD_SELF_CUSTODY_ROOT="+root, "SUBYARD_SELF_CUSTODY_MODE="+mode)
			child.Stdout, child.Stderr = io.Discard, io.Discard
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { os.WriteFile(filepath.Join(root, "release"), nil, 0600); child.Process.Kill(); child.Wait() }()
			deadline := time.Now().Add(8 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(root, "pid")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("fixed child did not dispatch")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if mode == "loss" {
				if err := child.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err := child.Wait(); err == nil {
					t.Fatal("lost helper succeeded")
				}
			} else {
				if err := child.Wait(); err != nil {
					t.Fatal("descendant refusal failed", err)
				}
			}
			lock, err := os.OpenFile(uninstall.OwnerLockPath(root), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
				t.Fatal("survivor lost original OFD")
			}
			if _, err := os.Stat(filepath.Join(root, "intent")); err != nil {
				t.Fatal("effect preceded intent", err)
			}
			if _, err := os.Stat(filepath.Join(root, "terminal")); !os.IsNotExist(err) {
				t.Fatal("survivor gained terminal")
			}
			if raw, err := os.ReadFile(filepath.Join(root, "effects")); err != nil || string(raw) != "once\n" {
				t.Fatal("duplicate apply", err)
			}
			testkit.WriteFile(t, filepath.Join(root, "release"), nil, 0600)
			for time.Now().Before(deadline) {
				if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			raw, _ := os.ReadFile(filepath.Join(root, "pid"))
			pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
			if pid > 1 {
				syscall.Kill(-pid, syscall.SIGKILL)
			}
			t.Fatal("fixed descendant did not drain")
		})
	}
}

func TestNativeFixedSelfRefusesMissingCapabilityOrOriginalOwnerBeforeDispatch(t *testing.T) {
	root := testkit.TempDir(t)
	ctx, observer := fixedSelfContext(t, root, "#!/bin/sh\nexit 0\n")
	request := NativeVMCPU{Project: "fixture", Instance: "fixture", UUID: "original"}
	if err := RunNativeVMCPU(ctx, request, io.Discard); err == nil {
		t.Fatal("missing original owner admitted")
	}
	ctx = ports.WithNativeOwnerHome(ports.WithNativeOperationObserver(context.Background(), observer), root)
	if err := RunNativeVMCPU(ctx, request, io.Discard); err == nil {
		t.Fatal("missing self capability admitted")
	}
	if _, err := os.Stat(filepath.Join(root, "intent")); !os.IsNotExist(err) {
		t.Fatal("unadmitted fixed leaf reserved effect")
	}
}
