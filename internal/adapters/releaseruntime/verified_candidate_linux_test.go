//go:build linux

package releaseruntime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"golang.org/x/sys/unix"
)

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
