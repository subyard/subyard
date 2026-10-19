package shelladapter

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

type adapterNativeObserver struct {
	root                string
	reject              bool
	terminal, uncertain bool
}

func (*adapterNativeObserver) Before(ports.NativeOperation) error {
	return errors.New("unexpected SDK effect")
}
func (*adapterNativeObserver) Accepted(ports.NativeOperation) error {
	return errors.New("unexpected SDK effect")
}
func (*adapterNativeObserver) Terminal(ports.NativeOperation) error {
	return errors.New("unexpected SDK effect")
}
func (o *adapterNativeObserver) Uncertain() { o.uncertain = true }
func (o *adapterNativeObserver) CommandStarted(kind, source string) (uint64, error) {
	if o.reject {
		return 0, errors.New("durable intent refused")
	}
	if kind != "adapter-script" || len(source) != 64 {
		return 0, errors.New("invalid command binding")
	}
	return 1, os.WriteFile(filepath.Join(o.root, "intent"), []byte(source), 0600)
}
func (o *adapterNativeObserver) CommandTerminal(n uint64) error {
	if n != 1 {
		return errors.New("wrong ordinal")
	}
	o.terminal = true
	return os.WriteFile(filepath.Join(o.root, "terminal"), []byte("drained"), 0600)
}

func nativeAdapterFixture(root, home string) (Runner, domain.AdapterRequest) {
	return Runner{RepositoryRoot: root, Actions: map[string]map[string]Action{"teardown": {"apply": {Path: filepath.Join(root, "adapter.sh"), Direct: true}}}, ContextKeys: map[string]struct{}{"SUBYARD_HOME": {}, "FIXTURE": {}}, Timeout: 5 * time.Second}, domain.AdapterRequest{Schema: 1, OperationID: "original-owner", Adapter: "teardown", Action: "apply", Context: map[string]string{"SUBYARD_HOME": home, "FIXTURE": root}}
}

func TestNativeAdapterIntentAndFixedDescriptors(t *testing.T) {
	for _, mode := range []string{"success", "intent-refusal", "exit-error"} {
		t.Run(mode, func(t *testing.T) {
			root, home := testkit.TempDir(t), testkit.TempDir(t)
			_, closeOwner, err := uninstall.OwnerLock(context.Background(), home, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeOwner()
			body := "#!/bin/sh\nset -eu\n[ -f \"$FIXTURE/intent\" ]\n[ \"$SUBYARD_METADATA_FD\" = 4 ]\n[ \"$SUBYARD_NATIVE_OWNER_FD3\" = 1 ]\n[ -f /proc/self/fd/3 ]\ncat <&4 >\"$FIXTURE/metadata\"\ntouch \"$FIXTURE/effect\"\n"
			if mode == "exit-error" {
				body += "exit 7\n"
			}
			testkit.WriteFile(t, filepath.Join(root, "adapter.sh"), []byte(body), 0700)
			runner, request := nativeAdapterFixture(root, home)
			observer := &adapterNativeObserver{root: root, reject: mode == "intent-refusal"}
			ctx := ports.WithNativeOwnerHome(ports.WithNativeOperationObserver(context.Background(), observer), home)
			_, _, err = runner.Run(ctx, request, nil)
			_, effectErr := os.Stat(filepath.Join(root, "effect"))
			if mode == "success" {
				if err != nil || effectErr != nil || !observer.terminal || observer.uncertain {
					t.Fatal("normal drain lost", err)
				}
			} else if mode == "intent-refusal" {
				if err == nil || !os.IsNotExist(effectErr) || observer.terminal {
					t.Fatal("effect preceded intent", err)
				}
			} else if err == nil || !observer.uncertain || observer.terminal {
				t.Fatal("failed command gained closure", err)
			}
		})
	}
}

func TestNativeAdapterHelperProcess(t *testing.T) {
	if os.Getenv("SUBYARD_NATIVE_ADAPTER_CHILD") != "1" {
		return
	}
	root, home := os.Getenv("SUBYARD_NATIVE_ADAPTER_ROOT"), os.Getenv("SUBYARD_NATIVE_ADAPTER_HOME")
	owner := os.NewFile(3, "original-owner")
	release, err := uninstall.RegisterBorrowedOwnerLock(home, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer owner.Close()
	runner, request := nativeAdapterFixture(root, home)
	_, _, err = runner.Run(ports.WithNativeOwnerHome(ports.WithNativeOperationObserver(context.Background(), &adapterNativeObserver{root: root}), home), request, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestNativeAdapterHelperDeathRetainsOriginalOFDAndUnknownIntent(t *testing.T) {
	root, home := testkit.TempDir(t), testkit.TempDir(t)
	_, closeOwner, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOwner()
	borrowed, err := uninstall.BorrowOwnerLock(home)
	if err != nil || borrowed == nil {
		t.Fatal(err)
	}
	body := `#!/bin/sh
set -eu
[ -f "$FIXTURE/intent" ]
[ "$SUBYARD_METADATA_FD" = 4 ]
cat <&4 >"$FIXTURE/metadata"
printf '%s' "$$" >"$FIXTURE/child"
while [ ! -f "$FIXTURE/release" ]; do sleep .01; done
`
	testkit.WriteFile(t, filepath.Join(root, "adapter.sh"), []byte(body), 0700)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestNativeAdapterHelperProcess$")
	cmd.Env = append(os.Environ(), "SUBYARD_NATIVE_ADAPTER_CHILD=1", "SUBYARD_NATIVE_ADAPTER_ROOT="+root, "SUBYARD_NATIVE_ADAPTER_HOME="+home)
	cmd.ExtraFiles = []*os.File{borrowed}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	borrowed.Close()
	closeOwner()
	defer func() { _ = cmd.Process.Kill() }()
	deadline := time.Now().Add(4 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(filepath.Join(root, "child"))
		pid, _ = strconv.Atoi(string(raw))
		if pid > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid == 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("native child not accepted")
	}
	defer syscall.Kill(-pid, syscall.SIGKILL)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	contender, err := os.OpenFile(uninstall.OwnerLockPath(home), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()
	if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("child lost original owner OFD")
	}
	if _, err := os.Stat(filepath.Join(root, "terminal")); !os.IsNotExist(err) {
		t.Fatal("helper death invented closure")
	}
	testkit.WriteFile(t, filepath.Join(root, "release"), nil, 0600)
	for time.Now().Before(deadline) {
		if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("foreground did not release original OFD")
}
