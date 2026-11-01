package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/sys/unix"
)

func TestReleaseOwnerFactoryBindsVerifiedArtifactGuard(t *testing.T) {
	if root := os.Getenv("SUBYARD_TEST_NATIVE_GUARD_ROOT"); root != "" {
		// The authenticated session supplies this process-owned asset descriptor.
		unix.CloseOnExec(4)
		configRoot, err := os.Open(filepath.Join(root, "state"))
		if err != nil {
			t.Fatal(err)
		}
		defer configRoot.Close()
		components, err := newReleaseOwnerComponents(context.Background(), releaseowner.Context{
			RepositoryRoot: root, PhysicalRepositoryRoot: "/proc/self/fd/4",
			ConfigHome: filepath.Join(root, "state"), DataHome: filepath.Join(root, "data"),
		}, configRoot)
		if err != nil {
			t.Fatal(err)
		}
		defer components.Close()
		artifact, err := uninstall.CaptureArtifact(filepath.Join(root, "artifact"))
		if err != nil || len(artifact.Entries) != 1 {
			t.Fatalf("native factory artifact capture: entries=%d err=%v", len(artifact.Entries), err)
		}
		return
	}
	root, environment, _ := nativeFixture(t)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"state", "scripts/lib"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	guard, err := os.ReadFile(filepath.Join(repositoryRoot(t), "scripts", "lib", "teardown-plan.py"))
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "scripts", "lib", "teardown-plan.py"), guard, 0600)
	testkit.WriteFile(t, filepath.Join(root, "artifact"), []byte("owned fixture"), 0600)
	assetRoot, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer assetRoot.Close()
	engine, _ := testMemfdImage(t, "native-owner-guard-test")
	command := exec.Command("/proc/self/fd/3", "-test.run=^TestReleaseOwnerFactoryBindsVerifiedArtifactGuard$")
	command.ExtraFiles = []*os.File{engine, assetRoot}
	command.Env = append(append(os.Environ(), environment...), "SUBYARD_TEST_NATIVE_GUARD_ROOT="+root)
	command.Dir = testkit.TempDir(t)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native factory memfd subprocess: %v\n%s", err, output)
	}
}

func TestSealedDispatcherRemainsExecutableInProfileServiceChild(t *testing.T) {
	if root := os.Getenv("SUBYARD_TEST_DISPATCH_ROOT"); root != "" {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(self, "/memfd:") {
			t.Fatal("fixture is not running the memfd image")
		}
		var output bytes.Buffer
		cli, err := New(Options{RepositoryRoot: root, DispatcherPath: self, Environment: os.Environ(), Stdout: &output, Stderr: &output})
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := cli.resolveReleaseTransitionContext("default", cli.env["SUBYARD_CONFIG_HOME"])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(loaded.Context.Paths.DataHome, 0700); err != nil {
			t.Fatal(err)
		}
		_, release, err := uninstall.OwnerLock(context.Background(), loaded.Context.Paths.DataHome, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		observer := &maintenanceCommandObserver{}
		ctx := ports.WithNativeOwnerHome(ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(context.Background(), observer), "sealed-profile-callback"), loaded.Context.Paths.DataHome)
		for _, mode := range []string{"sealed", "disk", "foreign", "other-memfd"} {
			runtime := cli.initPlatform(loaded, nil).(reconcileruntime.Runtime)
			if mode != "sealed" {
				value := os.Getenv("SUBYARD_TEST_DISK_ENGINE")
				if mode == "foreign" {
					value = filepath.Join(root, "missing-engine")
				}
				if mode == "other-memfd" {
					value = "/memfd:foreign-engine (deleted)"
				}
				for i, entry := range runtime.Environment {
					if strings.HasPrefix(entry, "SUBYARD_DISPATCHER_PATH=") || strings.HasPrefix(entry, "SUBYARD_POWER_ENGINE_SOURCE=") {
						key, _, _ := strings.Cut(entry, "=")
						runtime.Environment[i] = key + "=" + value
					}
				}
			}
			before := strings.Join(runtime.Environment, "\x00")
			marker := filepath.Join(loaded.Context.Paths.DataHome, "callback-proof")
			_ = os.Remove(marker)
			output.Reset()
			err := runtime.ApplyStage(ctx, ports.ReconcileStageProfileServices)
			if strings.Join(runtime.Environment, "\x00") != before {
				t.Fatal("execution mutated bound runtime environment")
			}
			bound := []string{}
			for _, entry := range runtime.Environment {
				if !strings.HasPrefix(entry, "SUBYARD_OPERATION_ID=") && !strings.HasPrefix(entry, "SUBYARD_SUDO_PREAUTHORIZED=") {
					bound = append(bound, entry)
				}
			}
			bound = append(bound, "SUBYARD_NATIVE_OWNER_FD3=1")
			body, readErr := os.ReadFile(filepath.Join(root, "scripts/profile-services.sh"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			raw, _ := json.Marshal(struct {
				Payload                []byte
				Environment, Arguments []string
			}{body, bound, []string{"--yes"}})
			if observer.source != fmt.Sprintf("%x", sha256.Sum256(raw)) {
				t.Fatal("native command binding lost original symbolic engine roles")
			}
			if mode == "foreign" || mode == "other-memfd" {
				if err == nil || !strings.Contains(output.String(), "profile services: native engine is missing") {
					t.Fatal("foreign dispatcher replaced", mode, err, output.String())
				}
				continue
			}
			if err != nil {
				t.Fatal("current engine callback failed", mode, err, output.String())
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "callback-complete" {
				t.Fatal("actual profile callback did not finish", mode, err)
			}
		}
		return
	}
	root, environment, _ := nativeFixture(t)
	for _, name := range []string{"scripts/lib", "config/profiles/sample"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"profile-services.sh", "lib/engine-context.sh"} {
		data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, "scripts", name), data, 0700)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/profiles/sample/profile.json"), []byte(`{"schema_version":1,"owner_service":"owner.sh"}`), 0600)
	testkit.WriteFile(t, filepath.Join(root, "config/profiles/sample/owner.sh"), []byte(`#!/bin/sh
set -eu
[ -x "$SUBYARD_POWER_ENGINE_SOURCE" ]
cmp "$SUBYARD_POWER_ENGINE_SOURCE" "$SUBYARD_TEST_DISK_ENGINE"
printf callback-complete > "$SUBYARD_HOME/callback-proof"
`), 0700)
	engine, executable := testMemfdImage(t, "subyard-dispatch-test")
	if _, err := unix.FcntlInt(engine.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/proc/self/fd/3", "-test.run=^TestSealedDispatcherRemainsExecutableInProfileServiceChild$")
	command.ExtraFiles = []*os.File{engine}
	command.Env = append(append(os.Environ(), environment...), "SUBYARD_TEST_DISPATCH_ROOT="+root, "SUBYARD_TEST_DISK_ENGINE="+executable)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal("sealed real profile dispatcher", err, string(output))
	}
}

func TestMigrationOperatorInputDoesNotLeakToNativeHelpers(t *testing.T) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "fixture-terminal-master")
	defer master.Close()
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := unix.FcntlInt(input.Fd(), unix.F_SETFD, 0); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBYARD_MIGRATION_OPERATOR_INPUT_FD", strconv.Itoa(int(input.Fd())))
	var output bytes.Buffer
	program := &CLI{options: Options{Stderr: &output}}
	if code := program.runReleaseTransition(context.Background(), []string{"invalid"}); code != 2 {
		t.Fatal("invalid request unexpectedly ran", code)
	}
	flags, err := unix.FcntlInt(input.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("operator terminal remains inheritable before native helpers", flags, err)
	}
	child := exec.Command("/bin/sh", "-c", `test ! -e "/proc/self/fd/$1"`, "_", strconv.Itoa(int(input.Fd())))
	if err := child.Run(); err != nil {
		t.Fatal("ordinary helper inherited operator input", err)
	}
	child = exec.Command("/bin/sh", "-c", "test -t 3")
	child.ExtraFiles = []*os.File{input}
	if err := child.Run(); err != nil {
		t.Fatal("explicit verified handoff lost operator terminal", err)
	}
	if _, err := master.Write([]byte("yes\n")); err != nil {
		t.Fatal(err)
	}
	program.migrationOperatorInput = input
	if accepted, err := (migrationLatePrompt{cli: program}).Confirm(context.Background(), domain.ConfirmationRequest{Summary: "separate maintenance consent", Default: domain.ConfirmationDefaultNo}); err != nil || !accepted {
		t.Fatal("engine lost its late terminal input", accepted, err)
	}
}

// Return the current executable image unsealed; each caller owns its sealing policy.
func testMemfdImage(t *testing.T, label string) (*os.File, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	flags := unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING
	fd, err := unix.MemfdCreate(label, flags|unix.MFD_EXEC)
	if err == unix.EINVAL {
		fd, err = unix.MemfdCreate(label, flags)
	}
	if err != nil {
		t.Fatal(err)
	}
	engine := os.NewFile(uintptr(fd), label)
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := io.Copy(engine, source); err != nil {
		t.Fatal(err)
	}
	if err := engine.Chmod(0500); err != nil {
		t.Fatal(err)
	}
	return engine, executable
}
