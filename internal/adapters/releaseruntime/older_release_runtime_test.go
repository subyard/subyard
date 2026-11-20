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

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/blang/semver/v4"
)

// olderReleaseFixture publishes an active release and a target release beside it.
// The target advertises its compiled activation owners and declares targetID.
func olderReleaseFixture(t *testing.T, targetVersion, targetID string) (*Runtime, string, string, string, *verifiedPublishedCandidate) {
	t.Helper()
	aboveAnchor := semver.MustParse(UpdateAnchorVersion)
	aboveAnchor.Patch++
	activeVersion := aboveAnchor.String()
	runtime, active, target, home, installed := retainedRuntimeFixture(t)
	t.Cleanup(func() { _ = runtime.Close() })
	runtimeRoot := filepath.Join(filepath.Dir(active), "runtime")
	if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases"), 0o700); err != nil {
		t.Fatal(err)
	}
	capabilities, err := releasetransition.MarshalRecoveryProcessResponse(releasetransition.RecoveryProcessResponse{
		SchemaVersion: releasetransition.ProcessRecoverySchemaV2, ActivationOwners: []string{targetID},
	})
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(target, "config/profiles/sample/profile.json"), []byte(`{"schema_version":1,"default_yards":["default"],"runtime":{"activation_id":"`+targetID+`","handler":"runtime.sh"},"managed_paths":[{"root":"data","path":"service/{yard}"}]}`), 0o600)
	activeID, targetRelease := activeVersion+"-active", targetVersion+"-target"
	for _, candidate := range []struct{ path, id, version string }{{active, activeID, activeVersion}, {target, targetRelease, targetVersion}} {
		for _, directory := range []string{"bin", "config/preview"} {
			if err := os.MkdirAll(filepath.Join(candidate.path, directory), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		engine := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in\n --version) printf 'yard-engine %s\\n' ;;\n _release-transition) request=$(cat); case \"$request\" in\n *runtime-activation-capabilities*) printf '%%s\\n' %q ;;\n *) exit 1 ;;\n esac ;;\n *) exit 64 ;;\nesac\n", candidate.version, capabilities)
		testkit.WriteFile(t, filepath.Join(candidate.path, "bin/yard-engine"), []byte(engine), 0o700)
		testkit.WriteFile(t, filepath.Join(candidate.path, "config/release-transition.json"), []byte(sourceAdmissionRegistry), 0o600)
		testkit.WriteFile(t, filepath.Join(candidate.path, "config/preview/subyard-preview"), []byte("identical public core helper"), 0o700)
		var manifest strings.Builder
		if err := filepath.WalkDir(candidate.path, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			payload, err := os.ReadFile(path)
			if err == nil {
				relative, _ := filepath.Rel(candidate.path, path)
				fmt.Fprintf(&manifest, "%x  ./%s\n", sha256.Sum256(payload), relative)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(candidate.path, "runtime-files.sha256"), []byte(manifest.String()), 0o600)
		if err := os.Rename(candidate.path, filepath.Join(runtimeRoot, "releases", candidate.id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/"+activeID, filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	runtime.config.InstalledRuntimePresence = func(context.Context, string, config.Loaded, string) (ports.RuntimePresence, error) {
		return ports.RuntimePresence{Actual: "installed", Preview: true}, nil
	}
	writeRollbackFixtureBaselineLedger(t, home)
	// The sample profile runtime is installed in the default yard.
	testkit.WriteFile(t, installed, []byte("installed profile runtime"), 0o700)
	verified, err := runtime.verifyPublishedCandidate(context.Background(),
		publishedCandidate{release: releasetransition.ReleaseID(targetRelease), root: filepath.Join(runtimeRoot, "releases", targetRelease)}, runtimeRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = verified.Close() })
	return runtime, runtimeRoot, home, installed, verified
}

func TestExplicitOlderReleaseKeepsTheRetainedRuntimeContract(t *testing.T) {
	for name, test := range map[string]struct {
		version, owner string
		older, refused bool
	}{
		"older release with a different activation contract": {version: UpdateAnchorVersion, owner: "sample-runtime-v0", older: true, refused: true},
		"older release with the same activation contract":    {version: UpdateAnchorVersion, owner: "sample-runtime", older: true},
		"newer release": {version: "99.0.0", owner: "sample-runtime-v0"},
	} {
		t.Run(name, func(t *testing.T) {
			runtime, runtimeRoot, home, _, verified := olderReleaseFixture(t, test.version, test.owner)
			links, err := runtime.inspectRuntimeLinks(runtimeRoot)
			if err != nil {
				t.Fatal(err)
			}
			// The update path captures the release selection before this admission.
			request := releasetransition.ProcessRequest{
				SchemaVersion: releasetransition.ProcessProtocolSchemaV1, Mode: releasetransition.ProcessInspect,
				RuntimeRoot: runtimeRoot, ConfigHome: home, Yard: "default", Target: verified.candidate.release,
				Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: verified.manifestDigest,
			}
			selection, err := runtime.captureTransitionSelection(context.Background(), verified, request, links, false)
			if err != nil {
				t.Fatal(err)
			}
			ctx := selectionContext(context.Background(), selection)
			older, err := runtime.admitOlderRelease(ctx, runtimeRoot, links, verified, home)
			if test.refused {
				var refusal publicReleaseInspectionError
				if older != nil || !errors.As(err, &refusal) || refusal.outcome.Code != releasetransition.CodeRollbackIncompatible ||
					refusal.outcome.Message != olderRuntimeRefusal || refusal.outcome.Target != verified.candidate.release {
					t.Fatalf("older release losing an installed runtime owner was admitted: %#v, %v", older, err)
				}
				// --check reports the same outcome; an applying update returns it as its error.
				var stdout bytes.Buffer
				runtime.config.Stdout = &stdout
				prepared, checkErr := runtime.inspectionFailure(options{check: true, root: runtimeRoot}, err)
				if checkErr != nil || prepared.run == nil {
					t.Fatalf("--check did not report the refusal: %v", checkErr)
				}
				_ = prepared.Execute(context.Background())
				if !strings.Contains(stdout.String(), olderRuntimeRefusal) {
					t.Fatalf("--check report omits the refusal: %q", stdout.String())
				}
				if _, applyErr := runtime.inspectionFailure(options{root: runtimeRoot}, err); applyErr == nil ||
					!strings.Contains(applyErr.Error(), "code=rollback-incompatible") || !strings.Contains(applyErr.Error(), olderRuntimeRefusal) {
					t.Fatalf("applying update lost the refusal: %v", applyErr)
				}
				return
			}
			if err != nil || (older != nil) != test.older || older != nil && older.binding == "" {
				t.Fatalf("admission = %#v, %v", older, err)
			}
		})
	}
}
