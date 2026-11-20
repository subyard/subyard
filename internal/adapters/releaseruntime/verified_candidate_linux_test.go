//go:build linux

package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestVerifiedCandidateRequiresExactManifestInventory(t *testing.T) {
	for _, scenario := range []string{"exact", "changed-content", "extra-file", "omitted-entry", "unlisted-nested-manifest", "symlink", "fifo"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			capture := filepath.Join(root, "engine-invoked")
			candidate := writeRuntimeCandidatePayload(t, root, fmt.Sprintf(
				"#!/bin/sh\n: > %q\nprintf 'yard-engine 1.2.3\\n'\n", capture,
			))
			manifestPath := filepath.Join(candidate.root, "runtime-files.sha256")
			manifest, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(candidate.root, "scripts", "nested.sh")
			if err := os.Mkdir(filepath.Dir(script), 0o700); err != nil {
				t.Fatal(err)
			}
			payload := []byte("#!/bin/sh\nexit 0\n")
			testkit.WriteFile(t, script, payload, 0o700)
			scriptEntry := fmt.Sprintf("%x  ./scripts/nested.sh\n", sha256.Sum256(payload))
			nestedEntry := fmt.Sprintf("%x  ./config/runtime-files.sha256\n", sha256.Sum256(payload))
			testkit.WriteFile(t, filepath.Join(candidate.root, "config", "runtime-files.sha256"), payload, 0o600)
			manifest = append(manifest, []byte(scriptEntry+nestedEntry)...)
			switch scenario {
			case "changed-content":
				testkit.WriteFile(t, script, []byte("#!/bin/sh\nexit 7\n"), 0o700)
			case "extra-file":
				testkit.WriteFile(t, filepath.Join(candidate.root, "config", "extra.conf"), payload, 0o600)
			case "omitted-entry":
				manifest = []byte(strings.Replace(string(manifest), scriptEntry, "", 1))
			case "unlisted-nested-manifest":
				manifest = []byte(strings.Replace(string(manifest), nestedEntry, "", 1))
			case "symlink", "fifo":
				if err := os.Remove(script); err != nil {
					t.Fatal(err)
				}
				if scenario == "symlink" {
					err = os.Symlink(filepath.Join(candidate.root, "bin", "yard-engine"), script)
				} else {
					err = unix.Mkfifo(script, 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, manifestPath, manifest, 0o600)
			runtime := New(Config{Stderr: &bytes.Buffer{}})
			defer runtime.Close()
			verified, err := runtime.verifyPublishedCandidate(context.Background(), candidate, root, nil)
			if scenario == "exact" {
				if err != nil || verified == nil || verified.version != "1.2.3" {
					t.Fatalf("exact candidate = %#v, %v", verified, err)
				}
				defer verified.Close()
			} else {
				if err == nil || verified != nil {
					t.Fatalf("unsafe inventory accepted: %#v, %v", verified, err)
				}
				if _, err := os.Lstat(capture); !os.IsNotExist(err) {
					t.Fatalf("unsafe inventory executed the engine: %v", err)
				}
			}
		})
	}
}

func TestExecutableMemfdFallsBackForPreMFDExecKernels(t *testing.T) {
	var flags []int
	fd, err := createExecutableMemfd(func(_ string, current int) (int, error) {
		flags = append(flags, current)
		if len(flags) == 1 {
			return -1, unix.EINVAL
		}
		return 42, nil
	})
	if err != nil || fd != 42 {
		t.Fatalf("memfd fallback fd=%d err=%v", fd, err)
	}
	if len(flags) != 2 || flags[0]&unix.MFD_EXEC == 0 || flags[1]&unix.MFD_EXEC != 0 ||
		flags[1] != unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING {
		t.Fatalf("memfd fallback flags=%#v", flags)
	}
}

func TestSealVerifiedEngineRetriesOnlyBusy(t *testing.T) {
	payload := []byte("#!/bin/sh\nexit 0\n")
	path := filepath.Join(testkit.TempDir(t), "yard-engine")
	testkit.WriteFile(t, path, payload, 0o700)
	for _, scenario := range []struct {
		name      string
		wantCalls int
		wantErr   error
	}{
		{name: "busy-then-success", wantCalls: 3},
		{name: "always-busy", wantCalls: 5, wantErr: unix.EBUSY},
		{name: "permission-denied", wantCalls: 1, wantErr: unix.EPERM},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var callTimes []time.Time
			var sealed *os.File
			synctest.Test(t, func(*testing.T) {
				sealed, err = sealVerifiedEngineWith(file, sha256.Sum256(payload), func(fd uintptr, cmd, seals int) (int, error) {
					callTimes = append(callTimes, time.Now())
					if scenario.wantErr != nil {
						return -1, scenario.wantErr
					}
					if len(callTimes) <= 2 {
						return -1, unix.EBUSY
					}
					return unix.FcntlInt(fd, cmd, seals)
				})
			})
			if sealed != nil {
				defer sealed.Close()
			}
			if len(callTimes) != scenario.wantCalls {
				t.Errorf("seal calls = %d, want %d", len(callTimes), scenario.wantCalls)
			}
			for i := 1; i < len(callTimes); i++ {
				if delay := callTimes[i].Sub(callTimes[i-1]); delay != 75*time.Millisecond {
					t.Errorf("delay before seal call %d = %v, want 75ms", i+1, delay)
				}
			}
			if scenario.wantErr != nil {
				if sealed != nil || err == scenario.wantErr || !errors.Is(err, scenario.wantErr) {
					t.Fatalf("sealed file = %v, error = %v, want nil file and wrapped %v", sealed, err, scenario.wantErr)
				}
				return
			}
			if sealed == nil || err != nil {
				t.Fatalf("seal verified engine: file = %v, error = %v", sealed, err)
			}
			const wantSeals = unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
			seals, err := unix.FcntlInt(sealed.Fd(), unix.F_GET_SEALS, 0)
			if err != nil || seals&wantSeals != wantSeals {
				t.Fatalf("seals = %#x, error = %v, want bits %#x", seals, err, wantSeals)
			}
		})
	}
}

func TestCandidateResponseBufferIsBoundedWhileWriting(t *testing.T) {
	var output boundedResponseBuffer
	payload := bytes.Repeat([]byte("x"), 2*maximumCandidateResponseBytes)
	written, err := output.Write(payload)
	if err != nil || written != len(payload) || output.Len() != maximumCandidateResponseBytes+1 {
		t.Fatalf("bounded response write=%d len=%d err=%v", written, output.Len(), err)
	}
}

func TestVerifiedCandidateTransitionKeepsRepositoryRootAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	capture := filepath.Join(t.TempDir(), "repository-root")
	payload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition)
    request="$(cat)"
    repository_root="$SUBYARD_REPOSITORY_ROOT"
    case "$repository_root" in
      /proc/self/fd/*) repository_root="/proc/$$/fd/${repository_root##*/}" ;;
    esac
    case "$request" in
      *'"mode":"inspect"'*) printf '%%s\n' "$repository_root" > %q ;;
      *'"mode":"converge"'*)
        [ "$repository_root" = "$(cat %q)" ] || exit 73
        sh -c 'test -f "$1/config/release-transition.json"' child "$repository_root" || exit 74
        ;;
    esac
	    printf '%%s\n' '%s'
    ;;
  *) exit 64 ;;
esac
`, capture, capture, candidateProtocolFixtureResponse)
	candidate := writeRuntimeCandidatePayload(t, root, payload)
	linkActiveTransitionFixture(t, root, candidate)
	runtime := New(Config{})
	for _, mode := range []releasetransition.ProcessMode{
		releasetransition.ProcessInspect,
		releasetransition.ProcessConverge,
	} {
		verified, err := runtime.verifyPublishedCandidate(
			context.Background(), candidate, root, nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		request := candidateProtocolRequest(verified, root)
		request.Mode = mode
		if mode == releasetransition.ProcessConverge {
			request.Execution = &releasetransition.Execution{
				Plan: releasetransition.PlanToken("plan-v1-" + strings.Repeat("a", 64)),
			}
		}
		_, invokeErr := runtime.invokeVerifiedCandidateTransition(
			context.Background(), verified, request, "",
		)
		closeErr := verified.Close()
		if invokeErr != nil || closeErr != nil {
			t.Fatalf("%s candidate invocation: invoke=%v close=%v", mode, invokeErr, closeErr)
		}
	}
	if _, err := os.Stat(capture); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCloseReleasesPinnedCandidateRoot(t *testing.T) {
	root := t.TempDir()
	candidate := writeRuntimeCandidatePayload(t, root, `#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  *) exit 64 ;;
esac
`)
	runtime := New(Config{})
	verified, err := runtime.verifyPublishedCandidate(
		context.Background(), candidate, root, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	if err := runtime.pinCandidateRoot(verified); err != nil {
		t.Fatal(err)
	}
	pinned := runtime.pinnedCandidateRoot
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if runtime.pinnedCandidateRoot != nil || runtime.pinnedCandidateRelease != "" {
		t.Fatal("runtime retained the pinned candidate after close")
	}
	if _, err := pinned.Stat(); err == nil {
		t.Fatal("runtime close left the pinned candidate descriptor open")
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("second runtime close: %v", err)
	}
}
