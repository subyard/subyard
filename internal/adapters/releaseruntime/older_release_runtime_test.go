package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/blang/semver/v4"
)

// olderReleaseFixture publishes an active release and a target release beside it.
// The target advertises its compiled activation owners and declares targetID.
func olderReleaseFixture(t *testing.T, targetVersion, targetID string) (*Runtime, string, string, string, *verifiedPublishedCandidate) {
	t.Helper()
	fixture := newOlderReleaseFixture(t, targetVersion, targetID, targetVersion+"-target", sourceAdmissionRegistry)
	return fixture.runtime, fixture.runtimeRoot, fixture.home, fixture.installed, fixture.verified
}

type olderRelease struct {
	runtime                                *Runtime
	runtimeRoot, home, installed, launches string
	activeID, targetRelease, inspection    string
	verified                               *verifiedPublishedCandidate
}

func newOlderReleaseFixture(t *testing.T, targetVersion, targetID, targetRelease, targetRegistry string) olderRelease {
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
	activeID := activeVersion + "-active"
	launches := filepath.Join(filepath.Dir(active), "transition-launches")
	inspectionPath := filepath.Join(filepath.Dir(active), "inspection.json")
	inspection := fmt.Sprintf(`{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["activate the older release"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"%s","target":"%s","code":"transition-required","message":"confirmation required","retry":"confirm the inspected update"}}}`, activeID, targetRelease)
	testkit.WriteFile(t, inspectionPath, []byte(inspection+"\n"), 0o600)
	for _, candidate := range []struct{ path, id, version, registry string }{{active, activeID, activeVersion, sourceAdmissionRegistry}, {target, targetRelease, targetVersion, targetRegistry}} {
		for _, directory := range []string{"bin", "config/preview"} {
			if err := os.MkdirAll(filepath.Join(candidate.path, directory), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		engine := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in\n --version) printf 'yard-engine %s\\n' ;;\n _release-transition) request=$(cat); case \"$request\" in\n *runtime-activation-capabilities*) printf '%%s\\n' %q ;;\n *) printf 'launch\\n' >> %q; cat %q ;;\n esac ;;\n *) exit 64 ;;\nesac\n", candidate.version, capabilities, launches, inspectionPath)
		testkit.WriteFile(t, filepath.Join(candidate.path, "bin/yard-engine"), []byte(engine), 0o700)
		testkit.WriteFile(t, filepath.Join(candidate.path, "config/release-transition.json"), []byte(candidate.registry), 0o600)
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
	return olderRelease{runtime: runtime, runtimeRoot: runtimeRoot, home: home, installed: installed, launches: launches,
		activeID: activeID, targetRelease: targetRelease, inspection: inspectionPath, verified: verified}
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

// olderUpdateFixture serves the target as the verified offline bundle for
// yard update --version, beside the active release that installed its runtime.
func olderUpdateFixture(t *testing.T, targetID, targetRegistry string) (olderRelease, []string) {
	t.Helper()
	version := UpdateAnchorVersion
	cache := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(cache, version), 0o700); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("subyard-%s-linux-%s.tar.gz", version, goruntime.GOARCH)
	for _, suffix := range []string{"", ".sha256", ".manifest.json", ".provenance.json"} {
		testkit.WriteFile(t, filepath.Join(cache, version, name+suffix), []byte("older release fixture"), 0o600)
	}
	target, _, err := releaseBundleIdentity(version, filepath.Join(cache, version, name))
	if err != nil {
		t.Fatal(err)
	}
	fixture := newOlderReleaseFixture(t, version, targetID, filepath.Base(target), targetRegistry)
	published := filepath.Join(fixture.runtimeRoot, target)
	installer := filepath.Join(cache, "installer")
	testkit.WriteFile(t, installer, []byte(fmt.Sprintf("#!/bin/sh\n# --publish-only\nset -eu\nmkdir -p \"$2/releases\"\ncp -a %q \"$2/%s\"\nprintf '%%s\\n' %q\n", published, target, target)), 0o700)
	fixture.runtime.config.Installer = installer
	fixture.runtime.config.Inputs.values["YARD_RELEASE_CACHE"] = cache
	fixture.runtime.config.Stdout, fixture.runtime.config.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	return fixture, []string{"--offline", "--version", version, "--runtime-root", fixture.runtimeRoot}
}

func (fixture olderRelease) assertUnchanged(t *testing.T, launches []byte) {
	t.Helper()
	if current, err := os.Readlink(filepath.Join(fixture.runtimeRoot, "current")); err != nil || current != "releases/"+fixture.activeID {
		t.Fatalf("active runtime changed: %q, %v", current, err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.runtimeRoot, "previous")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusal created a rollback horizon: %v", err)
	}
	if got, _ := os.ReadFile(fixture.launches); !bytes.Equal(got, launches) {
		t.Fatalf("a refused update launched the transition: %q", got)
	}
}

func TestUpdateToAnExplicitOlderReleaseRefusesALostRuntimeOwner(t *testing.T) {
	for _, check := range []bool{true, false} {
		t.Run(fmt.Sprintf("check=%t", check), func(t *testing.T) {
			fixture, arguments := olderUpdateFixture(t, "sample-runtime-v0", sourceAdmissionRegistry)
			if check {
				arguments = append(arguments, "--check")
			}
			prepared, err := fixture.runtime.PrepareTransition(context.Background(), arguments, fixture.home, "default")
			if prepared.Close != nil {
				defer prepared.Close()
			}
			outcome, ok := TransitionOutcome(err)
			if check {
				var stdout bytes.Buffer
				fixture.runtime.config.Stdout = &stdout
				if err != nil || prepared.run == nil {
					t.Fatalf("--check did not report the refusal: %v", err)
				}
				_ = prepared.Execute(context.Background())
				if !strings.Contains(stdout.String(), `"code": "rollback-incompatible"`) || !strings.Contains(stdout.String(), olderRuntimeRefusal) {
					t.Fatalf("--check report omits the refusal: %q", stdout.String())
				}
			} else if !ok || outcome.Code != releasetransition.CodeRollbackIncompatible || outcome.Message != olderRuntimeRefusal ||
				outcome.Target != releasetransition.ReleaseID(fixture.targetRelease) {
				t.Fatalf("older release losing an installed runtime owner was not refused: %v", err)
			}
			fixture.assertUnchanged(t, nil)
		})
	}
}

func TestUpdateToAnExplicitOlderReleaseRechecksRuntimeOwnersBeforeEffects(t *testing.T) {
	fixture, arguments := olderUpdateFixture(t, "sample-runtime", sourceAdmissionRegistry)
	prepared, err := fixture.runtime.PrepareTransition(context.Background(), append(arguments, "--yes"), fixture.home, "default")
	if err != nil || !prepared.Changed {
		t.Fatalf("older release keeping every runtime owner was not planned: %v", err)
	}
	defer prepared.Close()
	launches := mustReadFile(t, fixture.launches)
	testkit.WriteFile(t, fixture.installed, []byte("changed installed profile runtime"), 0o700)
	if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) || !strings.Contains(err.Error(), "installed runtime inputs changed") {
		t.Fatalf("execution did not recheck the installed runtime owners: %v", err)
	}
	fixture.assertUnchanged(t, launches)
}

// The owner query of a selection with a differing migration registry is a
// refused read: the update reports the registry refusal, not a lost owner.
func TestUpdateToAnExplicitOlderReleaseReportsItsRegistryRefusal(t *testing.T) {
	for _, check := range []bool{true, false} {
		t.Run(fmt.Sprintf("check=%t", check), func(t *testing.T) {
			fixture, arguments := olderUpdateFixture(t, "sample-runtime-v0", migrationRollbackSettingsRegistry)
			if check {
				arguments = append(arguments, "--check")
			}
			var stdout bytes.Buffer
			prepared, err := fixture.runtime.PrepareTransition(context.Background(), arguments, fixture.home, "default")
			if prepared.Close != nil {
				defer prepared.Close()
			}
			report := ""
			if check {
				if err != nil || prepared.run == nil {
					t.Fatalf("--check did not report the refusal: %v", err)
				}
				fixture.runtime.config.Stdout = &stdout
				_ = prepared.Execute(context.Background())
				report = stdout.String()
			} else if err != nil {
				report = err.Error()
			}
			if !strings.Contains(report, "rollback-incompatible") || !strings.Contains(report, "migration registry differs") || strings.Contains(report, olderRuntimeRefusal) {
				t.Fatalf("registry refusal was not reported: %q (%v)", report, err)
			}
			fixture.assertUnchanged(t, nil)
		})
	}
}

// declareDisableableProfile gives both releases a sample profile like telemetry:
// an owner service, so `profile disable` exists, and no managed host paths.
func (fixture olderRelease) declareDisableableProfile(t *testing.T, targetID string) {
	t.Helper()
	for release, id := range map[string]string{fixture.activeID: "sample-runtime", fixture.targetRelease: targetID} {
		root := filepath.Join(fixture.runtimeRoot, "releases", release)
		testkit.WriteFile(t, filepath.Join(root, "config/profiles/sample/profile.json"), []byte(`{"schema_version":1,"default_yards":["default"],"owner_service":"runtime.sh","runtime":{"activation_id":"`+id+`","handler":"runtime.sh"}}`), 0o600)
		rewriteReleaseOwnerManifest(t, publishedCandidate{root: root})
	}
}

func TestOlderReleaseRefusalNamesTheEnabledProfileToDisable(t *testing.T) {
	const (
		message = "the retained release cannot activate the installed runtime of enabled profile sample in yard default"
		retry   = "disable profile sample in yard default (yard -Y default profile disable sample), then run yard update --check"
	)
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback=%t", rollback), func(t *testing.T) {
			fixture, arguments := olderUpdateFixture(t, "sample-runtime-v0", sourceAdmissionRegistry)
			fixture.declareDisableableProfile(t, "sample-runtime-v0")
			if rollback {
				if err := os.Symlink("releases/"+fixture.targetRelease, filepath.Join(fixture.runtimeRoot, "previous")); err != nil {
					t.Fatal(err)
				}
				arguments = []string{"--rollback", "--runtime-root", fixture.runtimeRoot}
				inspection := strings.Replace(string(mustReadFile(t, fixture.inspection)), `"target":`, `"previous":"`+fixture.targetRelease+`","target":`, 1)
				testkit.WriteFile(t, fixture.inspection, []byte(inspection), 0o600)
			}
			observation := ports.RuntimeObservation{State: ports.RuntimeStateCurrent, Actual: strings.Repeat("a", 64), Desired: strings.Repeat("a", 64)}
			fixture.runtime.config.ProfileRuntimeObservation = func(context.Context, string, config.Loaded, string) (ports.RuntimeObservation, error) {
				return observation, nil
			}
			prepare := func() (Prepared, error) {
				prepared, err := fixture.runtime.PrepareTransition(context.Background(), arguments, fixture.home, "default")
				if prepared.Close != nil {
					t.Cleanup(func() { _ = prepared.Close() })
				}
				return prepared, err
			}
			// Enabled: the refusal names the profile, its yard and the command that deactivates it.
			_, err := prepare()
			if outcome, ok := TransitionOutcome(err); !ok || outcome.Code != releasetransition.CodeRollbackIncompatible ||
				outcome.Message != message || outcome.Retry != retry {
				t.Fatalf("enabled profile refusal = %v", err)
			}
			// Disabled but not proven deactivated: the general refusal remains.
			testkit.WriteFile(t, filepath.Join(fixture.home, "config.env"), []byte("ENVIRONMENT_PROFILES=\n"), 0o600)
			_, err = prepare()
			if outcome, ok := TransitionOutcome(err); !ok || outcome.Message != olderRuntimeRefusal {
				t.Fatalf("disabled runtime without a deactivation proof = %v", err)
			}
			// Disabled and deactivated: the older release is admitted.
			observation.Deactivated = true
			if prepared, err := prepare(); err != nil || !prepared.Changed {
				t.Fatalf("deactivated runtime was not admitted: %v", err)
			}
		})
	}
}

func TestOlderRuntimeOutcomeTextNamesEveryDisableableProfile(t *testing.T) {
	const fallback = "keep the current release active"
	enabled := func(yard, name string) error {
		return retainedProfileRuntimeRefusal{yard: yard, profile: name, owner: name + "-runtime", disable: true}
	}
	for name, test := range map[string]struct {
		refusal        error
		message, retry string
	}{
		"two yards": {
			refusal: domain.CollectOrderedRefusals(enabled("default", "sample"), enabled("work", "other")),
			message: "the retained release cannot activate the installed runtime of enabled profile sample in yard default, profile other in yard work",
			retry:   "disable profile sample in yard default (yard -Y default profile disable sample) and profile other in yard work (yard -Y work profile disable other), then run yard update --check",
		},
		"one profile without disable": {
			refusal: domain.CollectOrderedRefusals(enabled("default", "sample"), retainedProfileRuntimeRefusal{yard: "work", profile: "other", owner: "other-runtime"}),
			message: olderRuntimeRefusal, retry: fallback,
		},
		"core helper": {
			refusal: domain.CollectOrderedRefusals(enabled("default", "sample"), errors.New("retained release cannot activate installed core helper contract")),
			message: olderRuntimeRefusal, retry: fallback,
		},
		"unrelated failure": {refusal: errors.New("cannot capture retained runtime selection"), message: olderRuntimeRefusal, retry: fallback},
	} {
		t.Run(name, func(t *testing.T) {
			message, retry := olderRuntimeOutcomeText(test.refusal, fallback)
			if message != test.message || retry != test.retry {
				t.Fatalf("outcome text = %q / %q", message, retry)
			}
		})
	}
}
