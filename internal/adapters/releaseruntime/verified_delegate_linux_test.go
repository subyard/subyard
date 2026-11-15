//go:build linux

package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestVerifiedCandidateTransitionPinsItsOwnAssetsAcrossProcesses(t *testing.T) {
	root := testkit.TempDir(t)
	capture := filepath.Join(root, "repository-root")
	registry := []byte("{}\n")
	candidate := writeDelegateRuntimeFixture(t, root, "1.2.4-candidate", "original-assets", registry, fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.4\n' ;;
  _release-transition)
    cat >/dev/null
    repository_root="$SUBYARD_REPOSITORY_ROOT"
    [ "$(cat "$repository_root/asset-marker")" = original-assets ] || exit 92
    case "$repository_root" in /proc/self/*) exit 93 ;; esac
    if [ -f %q ]; then
      [ "$repository_root" = "$(cat %q)" ] || exit 94
    else
      printf '%%s\n' "$repository_root" > %q
    fi
    sh -c 'test "$(cat "$1/asset-marker")" = original-assets' child "$repository_root" || exit 95
    printf '%%s\n' '%s'
    ;;
  *) exit 96 ;;
esac
`, capture, capture, capture, candidateProtocolFixtureResponse))
	linkActiveTransitionFixture(t, root, engine)
	runtime := New(Config{Stderr: &bytes.Buffer{}})
	defer runtime.Close()
	for _, mode := range []releasetransition.ProcessMode{
		releasetransition.ProcessInspect, releasetransition.ProcessConverge,
	} {
		t.Run(string(mode), func(t *testing.T) {
			verified, err := runtime.verifyPublishedCandidate(context.Background(), candidate, root, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer verified.Close()
			request := candidateProtocolRequest(verified, root)
			request.Mode = mode
			if mode == releasetransition.ProcessConverge {
				request.Execution = &releasetransition.Execution{
					Plan: releasetransition.PlanToken("plan-v1-" + strings.Repeat("a", 64)),
				}
			}
			if _, err := runtime.invokeVerifiedCandidateTransition(
				context.Background(), verified, request, "",
			); err != nil {
				t.Fatalf("%s candidate transition: %v", mode, err)
			}
		})
	}
	if _, err := os.Stat(capture); err != nil {
		t.Fatal(err)
	}
}

func writeDelegateRuntimeFixture(
	t *testing.T,
	runtimeRoot, release, marker string,
	registry []byte,
	payload string,
) publishedCandidate {
	t.Helper()
	root := filepath.Join(runtimeRoot, "releases", release)
	for _, directory := range []string{"bin", "config"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	markerPayload := []byte(marker + "\n")
	testkit.WriteFile(t, filepath.Join(root, "bin", "yard-engine"), []byte(payload), 0o700)
	testkit.WriteFile(t, filepath.Join(root, "config", "release-transition.json"), registry, 0o600)
	testkit.WriteFile(t, filepath.Join(root, "asset-marker"), markerPayload, 0o600)
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n%x  ./asset-marker\n",
		sha256.Sum256([]byte(payload)), sha256.Sum256(registry), sha256.Sum256(markerPayload))
	testkit.WriteFile(t, filepath.Join(root, "runtime-files.sha256"), []byte(manifest), 0o600)
	return publishedCandidate{release: releasetransition.ReleaseID(release), root: root}
}
