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

func TestVerifiedRuntimeTransitionUsesDelegateEngineAndOriginalAssets(t *testing.T) {
	root := testkit.TempDir(t)
	capture := filepath.Join(root, "repository-root")
	registry := []byte("{}\n")
	assets := writeDelegateRuntimeFixture(t, root, "1.2.3-assets", "original-assets", registry, `#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  *) exit 91 ;;
esac
`)
	engine := writeDelegateRuntimeFixture(t, root, "1.2.4-engine", "delegate-assets", registry, fmt.Sprintf(`#!/bin/sh
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
	runtime := New(Config{Stderr: &bytes.Buffer{}})
	defer runtime.Close()
	for _, mode := range []releasetransition.ProcessMode{
		releasetransition.ProcessInspect, releasetransition.ProcessConverge,
	} {
		t.Run(string(mode), func(t *testing.T) {
			verifiedAssets, err := runtime.verifyPublishedCandidate(context.Background(), assets, root, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer verifiedAssets.Close()
			verifiedEngine, err := runtime.verifyPublishedCandidate(context.Background(), engine, root, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer verifiedEngine.Close()
			if verifiedAssets.version != "1.2.3" || verifiedEngine.version != "1.2.4" {
				t.Fatalf("runtime versions: assets=%q engine=%q", verifiedAssets.version, verifiedEngine.version)
			}
			request := candidateProtocolRequest(verifiedAssets, root)
			request.Mode = mode
			if mode == releasetransition.ProcessConverge {
				request.Execution = &releasetransition.Execution{
					Plan: releasetransition.PlanToken("plan-v1-" + strings.Repeat("a", 64)),
				}
			}
			if _, err := runtime.invokeVerifiedRuntimeTransition(
				context.Background(), verifiedAssets, verifiedEngine, request, "",
			); err != nil {
				t.Fatalf("%s delegated transition: %v", mode, err)
			}
		})
	}
	if _, err := os.Stat(capture); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedRuntimeTransitionRejectsUnavailableOrIncompatibleDelegate(t *testing.T) {
	root := testkit.TempDir(t)
	capture := filepath.Join(root, "transition-executed")
	payload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition) : > %q; exit 97 ;;
  *) exit 98 ;;
esac
`, capture)
	assets := writeDelegateRuntimeFixture(t, root, "1.2.3-assets", "assets", []byte("{}\n"), payload)
	incompatible := writeDelegateRuntimeFixture(t, root, "1.2.3-incompatible", "engine", []byte("{\"different\":true}\n"), payload)
	runtime := New(Config{Stderr: &bytes.Buffer{}})
	defer runtime.Close()
	verifiedAssets, err := runtime.verifyPublishedCandidate(context.Background(), assets, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer verifiedAssets.Close()
	verifiedEngine, err := runtime.verifyPublishedCandidate(context.Background(), incompatible, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer verifiedEngine.Close()
	for _, test := range []struct {
		name           string
		assets, engine *verifiedPublishedCandidate
	}{
		{name: "missing assets", engine: verifiedAssets},
		{name: "missing engine", assets: verifiedAssets},
		{name: "incompatible registry", assets: verifiedAssets, engine: verifiedEngine},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := runtime.invokeVerifiedRuntimeTransition(
				context.Background(), test.assets, test.engine,
				candidateProtocolRequest(verifiedAssets, root), "",
			); err == nil {
				t.Fatal("invalid delegated transition was accepted")
			}
			if _, err := os.Lstat(capture); !os.IsNotExist(err) {
				t.Fatalf("invalid delegated transition executed an engine: %v", err)
			}
		})
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
