package uninstall

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestArtifactGuardInheritedAssetRoot(t *testing.T) {
	if mode := os.Getenv("SUBYARD_TEST_ARTIFACT_GUARD"); mode != "" {
		// Match the process entrypoint: assets are independent of the sealed engine.
		UseArtifactGuard("/proc/self/fd/4")
		// The Go process owns this root; native helpers must receive their own FD.
		unix.CloseOnExec(4)
		if mode == "retained" {
			UseArtifactGuard(os.Getenv("SUBYARD_TEST_RETAINED_ROOT"))
		}
		artifact, err := CaptureArtifact(os.Getenv("SUBYARD_TEST_ARTIFACT"))
		if mode == "missing" || mode == "unsafe" {
			if err == nil {
				t.Fatal("unsafe or missing guard input was accepted")
			}
			return
		}
		if err != nil || len(artifact.Entries) != 1 {
			t.Fatalf("native capture from verified asset root: entries=%d err=%v", len(artifact.Entries), err)
		}
		return
	}

	guard, err := os.ReadFile(filepath.Join("..", "..", "scripts", "lib", "teardown-plan.py"))
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	fd, err := unix.MemfdCreate("artifact-guard-test", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING|unix.MFD_EXEC)
	if err == unix.EINVAL {
		fd, err = unix.MemfdCreate("artifact-guard-test", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	}
	if err != nil {
		t.Fatal(err)
	}
	engine := os.NewFile(uintptr(fd), "artifact-guard-test")
	defer engine.Close()
	if _, err := io.Copy(engine, source); err != nil {
		t.Fatal(err)
	}
	if err := engine.Chmod(0500); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(engine.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"capture", "retained", "missing", "unsafe"} {
		t.Run(mode, func(t *testing.T) {
			root := testkit.TempDir(t)
			assets := filepath.Join(root, "scripts", "lib")
			if err := os.MkdirAll(assets, 0700); err != nil {
				t.Fatal(err)
			}
			if mode != "missing" && mode != "retained" {
				testkit.WriteFile(t, filepath.Join(assets, "teardown-plan.py"), guard, 0600)
			}
			retained := testkit.TempDir(t)
			if mode == "retained" {
				if err := os.MkdirAll(filepath.Join(retained, "scripts", "lib"), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(retained, "scripts", "lib", "teardown-plan.py"), guard, 0600)
			}
			target := filepath.Join(root, "artifact")
			testkit.WriteFile(t, target, []byte("owned fixture"), 0600)
			if mode == "unsafe" {
				if err := os.Symlink(root, filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(root, "link", "artifact")
			}
			assetRoot, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer assetRoot.Close()
			command := exec.Command("/proc/self/fd/3", "-test.run=^TestArtifactGuardInheritedAssetRoot$")
			command.ExtraFiles = []*os.File{engine, assetRoot}
			command.Env = append(os.Environ(), "SUBYARD_TEST_ARTIFACT_GUARD="+mode,
				"SUBYARD_TEST_ARTIFACT="+target, "SUBYARD_TEST_RETAINED_ROOT="+retained)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("sealed engine subprocess: %v\n%s", err, output)
			}
		})
	}
}
