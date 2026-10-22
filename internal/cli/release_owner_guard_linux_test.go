package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	fd, err := unix.MemfdCreate("native-owner-guard-test", unix.MFD_CLOEXEC|unix.MFD_EXEC)
	if err == unix.EINVAL {
		fd, err = unix.MemfdCreate("native-owner-guard-test", unix.MFD_CLOEXEC)
	}
	if err != nil {
		t.Fatal(err)
	}
	engine := os.NewFile(uintptr(fd), "native-owner-guard-test")
	defer engine.Close()
	if _, err := io.Copy(engine, source); err != nil {
		t.Fatal(err)
	}
	if err := engine.Chmod(0500); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/proc/self/fd/3", "-test.run=^TestReleaseOwnerFactoryBindsVerifiedArtifactGuard$")
	command.ExtraFiles = []*os.File{engine, assetRoot}
	command.Env = append(append(os.Environ(), environment...), "SUBYARD_TEST_NATIVE_GUARD_ROOT="+root)
	command.Dir = testkit.TempDir(t)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native factory memfd subprocess: %v\n%s", err, output)
	}
}
