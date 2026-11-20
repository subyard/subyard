package releaseruntime

import (
	"bytes"
	"context"
	"encoding/json"
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
)

func TestMigrationKindRollbackRefusesBeforeTargetInspectionAndCollectsLayout(t *testing.T) {
	runtime, parsed, candidate, request, called := configLayoutRollbackFixture(t, "--scope")
	writeMigrationRollbackSource(t, candidate, request, []byte(migrationRollbackSettingsRegistry))
	writeConfigLayoutManifest(t, request.ConfigHome, 2, true)
	manifest := filepath.Join(request.ConfigHome, ".sync", "manifest.json")
	before := mustReadFile(t, manifest)
	ledger := filepath.Join(request.ConfigHome, "release-transition/v2/ledger.json")
	beforeLedger := mustReadFile(t, ledger)
	_, err := runtime.prepareRetainedTransition(context.Background(), parsed, request.ConfigHome, request.Yard, nil)
	failure := assertMigrationRollbackRefused(t, request, err, "migration registry differs", "Git configuration cache")
	if failure.Active != "release-a" || failure.Previous == nil || *failure.Previous != "release-b" {
		t.Fatalf("refusal reported unexpected runtime links: %+v", failure)
	}
	assertMigrationRollbackUnchanged(t, request, called)
	if !bytes.Equal(before, mustReadFile(t, manifest)) {
		t.Fatal("refusal changed the configuration manifest")
	}
	if !bytes.Equal(beforeLedger, mustReadFile(t, ledger)) {
		t.Fatal("refusal changed the migration ledger")
	}
}

func assertMigrationRollbackRefused(t *testing.T, request releasetransition.ProcessRequest, err error, messages ...string) releasetransition.Outcome {
	t.Helper()
	outcome, ok := TransitionOutcome(err)
	if !ok || outcome.Code != releasetransition.CodeRollbackIncompatible {
		t.Fatalf("refusal = %v", err)
	}
	assertMigrationRollbackOutcome(t, request, outcome, messages...)
	return outcome
}

func assertMigrationRollbackOutcome(t *testing.T, request releasetransition.ProcessRequest, outcome releasetransition.Outcome, messages ...string) {
	t.Helper()
	if outcome.Code != releasetransition.CodeRollbackIncompatible {
		t.Fatalf("outcome = %+v", outcome)
	}
	if err := releasetransition.ValidateProcessOutcome(releasetransition.Goal{Target: request.Target, Direction: request.Direction}, outcome); err != nil {
		t.Fatalf("refusal violates the frozen outcome contract: %v", err)
	}
	for _, message := range messages {
		if !strings.Contains(outcome.Message, message) {
			t.Fatalf("refusal message %q lacks %q", outcome.Message, message)
		}
	}
	if want := "keep the current release active and resolve all reported release compatibility requirements before retrying"; outcome.Retry != want {
		t.Fatalf("next action = %q, want %q", outcome.Retry, want)
	}
}

func assertMigrationRollbackUnchanged(t *testing.T, request releasetransition.ProcessRequest, called string) {
	t.Helper()
	if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target inspected before refusal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(request.ConfigHome, "release-transition/v2/journal.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection created a journal: %v", err)
	}
	for link, want := range map[string]string{"current": "releases/release-a", "previous": "releases/" + string(request.Target)} {
		got, err := os.Readlink(filepath.Join(request.RuntimeRoot, link))
		if err != nil || got != want {
			t.Fatalf("%s link = %q, %v, want %q", link, got, err, want)
		}
	}
}

func TestMigrationKindRollbackRefusesMissingActiveRegistry(t *testing.T) {
	runtime, parsed, candidate, request, called := configLayoutRollbackFixture(t, "--scope")
	writeMigrationRollbackSource(t, candidate, request, []byte(migrationRollbackSettingsRegistry))
	active := filepath.Join(request.RuntimeRoot, "releases", "release-a")
	registryPath := filepath.Join(active, "config", "release-transition.json")
	manifest := filepath.Join(active, "runtime-files.sha256")
	before := mustReadFile(t, manifest)
	if err := os.Remove(registryPath); err != nil {
		t.Fatal(err)
	}
	_, err := runtime.prepareRetainedTransition(context.Background(), parsed, request.ConfigHome, request.Yard, nil)
	assertMigrationRollbackRefused(t, request, err, "active release's sealed migration registry cannot be verified")
	assertMigrationRollbackUnchanged(t, request, called)
	if !bytes.Equal(before, mustReadFile(t, manifest)) {
		t.Fatal("refusal modified the active manifest")
	}
}

func TestMigrationKindRollbackRefusesMissingAuthoritativeHistory(t *testing.T) {
	runtime, parsed, _, request, called := configLayoutRollbackFixture(t, "--scope")
	if err := os.Remove(filepath.Join(request.ConfigHome, "release-transition/v2/ledger.json")); err != nil {
		t.Fatal(err)
	}
	_, err := runtime.prepareRetainedTransition(context.Background(), parsed, request.ConfigHome, request.Yard, nil)
	assertMigrationRollbackRefused(t, request, err, "authoritative active migration history is missing")
	assertMigrationRollbackUnchanged(t, request, called)
}

func TestUpdateResumesFirstPublicationWithoutMigrationHistory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		retained bool
		previous bool
	}{
		{name: "first publication"},
		{name: "retained release without previous link", retained: true},
		{name: "retained release with previous link", retained: true, previous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime, candidate, request, called, args := retainedVersionFixture(t)
			request.Direction = releasetransition.DirectionActivateTarget
			current := filepath.Join(request.RuntimeRoot, "current")
			if err := os.Remove(current); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("releases/"+string(candidate.release), current); err != nil {
				t.Fatal(err)
			}
			previousLink := filepath.Join(request.RuntimeRoot, "previous")
			if err := os.Remove(previousLink); err != nil {
				t.Fatal(err)
			}
			if tc.previous {
				if err := os.Symlink("releases/release-a", previousLink); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.retained {
				retained := filepath.Join(request.RuntimeRoot, "releases", "release-a")
				if err := os.Rename(retained, filepath.Join(filepath.Dir(request.RuntimeRoot), "unpublished-fixture")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(filepath.Join(request.ConfigHome, "release-transition/v2/ledger.json")); err != nil {
				t.Fatal(err)
			}
			prepared, err := runtime.PrepareTransition(context.Background(), args, request.ConfigHome, request.Yard)
			if tc.retained {
				assertMigrationRollbackRefused(t, request, err, "authoritative active migration history is missing")
				if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("engine was invoked without migration history: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Close != nil {
				t.Cleanup(func() { _ = prepared.Close() })
			}
			if !prepared.Changed || prepared.Request.Target != request.Target {
				t.Fatalf("first publication did not produce a transition plan: %+v", prepared.Request)
			}
			if _, err := os.Stat(called); err != nil {
				t.Fatalf("first publication did not reach engine inspection: %v", err)
			}
		})
	}
}

func TestMigrationKindRollbackRefusesExplicitVersionSelectingPrevious(t *testing.T) {
	runtime, _, request, called, args := retainedVersionFixture(t)
	request.Direction = releasetransition.DirectionActivateTarget
	_, err := runtime.PrepareTransition(context.Background(), args, request.ConfigHome, request.Yard)
	assertMigrationRollbackRefused(t, request, err, "migration registry differs")
	prepared, err := runtime.PrepareTransition(context.Background(), append(args, "--check", "--json"), request.ConfigHome, request.Yard)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	var report struct{ Outcome releasetransition.Outcome }
	if err := json.Unmarshal(runtime.config.Stdout.(*bytes.Buffer).Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	assertMigrationRollbackOutcome(t, request, report.Outcome, "migration registry differs")
	assertMigrationRollbackUnchanged(t, request, called)
}

func retainedVersionFixture(t *testing.T) (*Runtime, publishedCandidate, releasetransition.ProcessRequest, string, []string) {
	t.Helper()
	return retainedFixtureAtVersion(t, "0.29.0")
}

func retainedFixtureAtVersion(t *testing.T, version string) (*Runtime, publishedCandidate, releasetransition.ProcessRequest, string, []string) {
	t.Helper()
	runtime, _, candidate, request, called := configLayoutRollbackFixture(t, "--scope")
	bundle := []byte("retained release fixture")
	root := filepath.Dir(request.RuntimeRoot)
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(filepath.Join(cache, version), 0o700); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("subyard-%s-linux-%s.tar.gz", version, goruntime.GOARCH)
	for _, suffix := range []string{"", ".sha256", ".manifest.json", ".provenance.json"} {
		testkit.WriteFile(t, filepath.Join(cache, version, name+suffix), bundle, 0o600)
	}
	target, _, err := releaseBundleIdentity(version, filepath.Join(cache, version, name))
	if err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(candidate.root, "bin", "yard-engine")
	candidate.release = releasetransition.ReleaseID(filepath.Base(target))
	payload := strings.NewReplacer("1.2.3", version, "release-b", string(candidate.release)).Replace(string(mustReadFile(t, engine)))
	testkit.WriteFile(t, engine, []byte(payload), 0o700)
	rewriteReleaseOwnerManifest(t, candidate)
	destination := filepath.Join(request.RuntimeRoot, target)
	if err := os.Rename(candidate.root, destination); err != nil {
		t.Fatal(err)
	}
	candidate.root = destination
	previous := filepath.Join(request.RuntimeRoot, "previous")
	if err := os.Remove(previous); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, previous); err != nil {
		t.Fatal(err)
	}
	writeMigrationRollbackSource(t, candidate, request, []byte(migrationRollbackSettingsRegistry))
	request.Target = candidate.release
	installer := filepath.Join(root, "installer")
	testkit.WriteFile(t, installer, []byte(fmt.Sprintf("#!/bin/sh\n# --publish-only\nset -eu\nmkdir -p \"$2/releases\"\ncp -a %q \"$2/%s\"\nprintf '%%s\\n' %q\n", destination, target, target)), 0o700)
	stdout := &bytes.Buffer{}
	runtime.config.Installer = installer
	runtime.config.Stdout = stdout
	runtime.config.Inputs = fixtureCommandInputs(map[string]string{"HOME": root, "YARD_RELEASE_CACHE": cache})
	t.Cleanup(func() { _ = runtime.Close() })

	args := []string{"--offline", "--version", version, "--runtime-root", request.RuntimeRoot}
	return runtime, candidate, request, called, args
}

func TestRetainedMigrationRegistryIdentity(t *testing.T) {
	const live = `{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":3},"migrations":[{"id":"first","kind":"test-vms-settings-v1-to-v2","domain":"settings","fromEpoch":1,"toEpoch":2},{"id":"second","kind":"local-ssh-port-settings-v2-to-v3","domain":"settings","fromEpoch":2,"toEpoch":3}]}`
	for _, name := range []string{"identical", "both registryless", "one registryless", "different ID", "swapped kinds", "retired target with unreadable projection", "identical retired registries with unreadable projection", "NetworkManager epoch crossing", "declared equivalent predecessor", "undeclared equivalent predecessor", "equivalent predecessor with unreadable declaration", "identical with unreadable declaration", "declared equivalent successor", "undeclared equivalent successor"} {
		t.Run(name, func(t *testing.T) {
			runtime, candidate, request, called, updateArgs := retainedVersionFixture(t)
			active := publishedCandidate{release: "release-a", root: filepath.Join(request.RuntimeRoot, "releases", "release-a")}
			source, retained := []byte(migrationRollbackSettingsRegistry), []byte(migrationRollbackSettingsRegistry)
			want := "migration registry differs"
			switch name {
			case "identical":
				retained = append([]byte("  "), retained...)
				want = ""
			case "both registryless", "one registryless":
				if err := os.Remove(filepath.Join(candidate.root, "config", "release-transition.json")); err != nil {
					t.Fatal(err)
				}
				retained = nil
				if name == "both registryless" {
					if err := os.Remove(filepath.Join(active.root, "config", "release-transition.json")); err != nil {
						t.Fatal(err)
					}
					want = ""
				}
			case "different ID":
				retained = bytes.ReplaceAll(retained, []byte("canonicalize"), []byte("different-id"))
			case "swapped kinds":
				source = []byte(live)
				retained = []byte(strings.NewReplacer("test-vms-settings-v1-to-v2", "local-ssh-port-settings-v2-to-v3", "local-ssh-port-settings-v2-to-v3", "test-vms-settings-v1-to-v2").Replace(live))
				writeAppliedSettingsCheckpoint(t, request, source)
			case "retired target with unreadable projection", "identical retired registries with unreadable projection":
				retained = writeAppliedSettingsCheckpoint(t, request, source)
				if name == "identical retired registries with unreadable projection" {
					source = retained
					want = "cannot read the legacy migration ledger projection"
				}
			case "declared equivalent predecessor", "undeclared equivalent predecessor", "equivalent predecessor with unreadable declaration":
				// The active release retires the prefix its predecessor still defines.
				source = writeAppliedSettingsCheckpoint(t, request, retained)
				switch name {
				case "declared equivalent predecessor":
					want = ""
				case "equivalent predecessor with unreadable declaration":
					want = "rollback declaration of the release pair cannot be verified"
				}
			case "declared equivalent successor", "undeclared equivalent successor":
				// The retained release retires the prefix the active one still defines.
				retained = writeAppliedSettingsCheckpoint(t, request, source)
				if name == "declared equivalent successor" {
					// The registry relation is admitted, so the history is read.
					want = "cannot read the legacy migration ledger projection"
				}
			case "identical with unreadable declaration":
				// Only an equivalent relation depends on the declaration.
				want = ""
			case "NetworkManager epoch crossing":
				source = []byte(`{"schemaVersion":2,"minimumEpochs":{"power-metadata":3},"currentEpochs":{"power-metadata":4},"migrations":[{"id":"introduce-nm-device-guard-v4","kind":"nm-device-guard-v1","domain":"power-metadata","fromEpoch":3,"toEpoch":4}]}`)
				retained = []byte(`{"schemaVersion":2,"minimumEpochs":{"power-metadata":3},"currentEpochs":{"power-metadata":3},"migrations":[]}`)
			}
			if name == "both registryless" {
				rewriteReleaseOwnerManifest(t, active)
			} else {
				writeMigrationRollbackSource(t, candidate, request, source)
			}
			switch name {
			case "declared equivalent predecessor":
				declareRollbackPredecessor(t, active, registryCanonicalDigest(t, retained))
			case "equivalent predecessor with unreadable declaration", "identical with unreadable declaration":
				declareRollbackPredecessor(t, active, "AAAA")
			case "declared equivalent successor":
				declareRollbackPredecessor(t, candidate, registryCanonicalDigest(t, source))
			}
			if retained != nil {
				bindRollbackFixtureRegistry(t, candidate, retained)
			}
			rewriteReleaseOwnerManifest(t, candidate)
			if name == "both registryless" {
				ctx := context.Background()
				target, err := runtime.verifyPublishedCandidate(ctx, candidate, request.RuntimeRoot, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer target.Close()
				links, err := runtime.inspectRuntimeLinks(request.RuntimeRoot)
				if err != nil {
					t.Fatal(err)
				}
				selection, err := runtime.captureReleaseSelection(ctx, target, request, links)
				if err != nil || selection.refusal != "" {
					t.Fatalf("verified registryless pair did not match: %+v, %v", selection, err)
				}
				return
			}
			for _, rollback := range []bool{true, false} {
				args := updateArgs
				request.Direction = releasetransition.DirectionActivateTarget
				if rollback {
					args = []string{"--rollback", "--runtime-root", request.RuntimeRoot}
					request.Direction = releasetransition.DirectionActivatePrevious
				}
				prepared, err := runtime.PrepareTransition(context.Background(), args, request.ConfigHome, request.Yard)
				if want != "" {
					assertMigrationRollbackRefused(t, request, err, want)
					assertMigrationRollbackUnchanged(t, request, called)
					continue
				}
				if err != nil || !prepared.Changed || prepared.Request.Direction != request.Direction {
					t.Fatalf("compatible retained release did not produce a plan: %v", err)
				}
				if prepared.Close != nil {
					if err := prepared.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func registryCanonicalDigest(t *testing.T, registry []byte) string {
	t.Helper()
	parsed, _, err := releasetransition.ParseRegistryV2(registry, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := parsed.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	return string(digest)
}

// declareRollbackPredecessor seals a rollback declaration naming digest into
// the release.
func declareRollbackPredecessor(t *testing.T, release publishedCandidate, digest string) {
	t.Helper()
	testkit.WriteFile(t, filepath.Join(release.root, "config", "release-rollback.json"),
		[]byte(fmt.Sprintf(`{"schemaVersion":1,"predecessorRegistryDigest":%q,"migrations":[],"profileFormats":[]}`+"\n", digest)), 0o600)
	rewriteReleaseOwnerManifest(t, release)
}

// Create a checkpoint at epoch 2 whose legacy projection lists the live ID.
// That projection is unreadable by the returned registry with a retired floor.
func writeAppliedSettingsCheckpoint(t *testing.T, request releasetransition.ProcessRequest, payload []byte) []byte {
	t.Helper()
	registry, digest, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadMigrationLedger(registry, digest)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := state.Ledger.Advance(registry, registry.Migrations[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapMigrationLedger(state, ledger, registry, digest); err != nil {
		t.Fatal(err)
	}
	state, err = store.ReadMigrationLedger(registry, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConvertMigrationLedger(state, registry, digest, registry, digest); err != nil {
		t.Fatal(err)
	}
	prefix, err := registry.MigrationPrefixDigest("settings", 2)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(fmt.Sprintf(`{"schemaVersion":2,"minimumEpochs":{"settings":2},"currentEpochs":{"settings":2},"migrations":[],"retiredPrefixes":{"settings":{"throughEpoch":2,"digest":%q}}}`, prefix))
}

func TestRetainedSelectionRefusesStalePrevious(t *testing.T) {
	for _, name := range []string{"rollback before inspection", "explicit update before inspection", "rollback before execution", "explicit update before execution", "rollback artifact replaced", "explicit update artifact replaced"} {
		t.Run(name, func(t *testing.T) {
			runtime, candidate, request, called, args := retainedVersionFixture(t)
			previous := filepath.Join(request.RuntimeRoot, "previous")
			rollback := strings.HasPrefix(name, "rollback")
			if rollback {
				args = []string{"--rollback", "--runtime-root", request.RuntimeRoot}
			}
			linkRemoval := fmt.Sprintf("rm -- %q", previous)
			if strings.HasSuffix(name, "before inspection") {
				if rollback {
					engine := filepath.Join(candidate.root, "bin", "yard-engine")
					payload := strings.Replace(string(mustReadFile(t, engine)), "--version)", "--version) "+linkRemoval+";", 1)
					testkit.WriteFile(t, engine, []byte(payload), 0o700)
					rewriteReleaseOwnerManifest(t, candidate)
				} else {
					installer := runtime.config.Installer
					payload := append(mustReadFile(t, installer), []byte(linkRemoval+"\n")...)
					testkit.WriteFile(t, installer, payload, 0o700)
				}
				_, err := runtime.PrepareTransition(context.Background(), args, request.ConfigHome, request.Yard)
				if !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("lost previous at inspection: %v", err)
				}
				if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale selection invoked retained transition: %v", err)
				}
				return
			}
			bindRollbackFixtureRegistry(t, candidate, []byte(migrationRollbackSettingsRegistry))
			rewriteReleaseOwnerManifest(t, candidate)
			prepared, err := runtime.PrepareTransition(context.Background(), args, request.ConfigHome, request.Yard)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Close != nil {
				t.Cleanup(func() { _ = prepared.Close() })
			}
			before := mustReadFile(t, called)
			if strings.HasSuffix(name, "artifact replaced") {
				testkit.WriteFile(t, filepath.Join(candidate.root, "config", "changed-fixture"), []byte("different artifact"), 0o600)
				rewriteReleaseOwnerManifest(t, candidate)
			} else if err := os.Remove(previous); err != nil {
				t.Fatal(err)
			}
			if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("stale previous at execution: %v", err)
			}
			if !bytes.Equal(before, mustReadFile(t, called)) {
				t.Fatal("stale selection invoked retained transition")
			}
		})
	}
}

const migrationRollbackSettingsRegistry = `{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":2},"migrations":[{"id":"canonicalize","kind":"test-vms-settings-v1-to-v2","domain":"settings","fromEpoch":1,"toEpoch":2}]}`

func writeMigrationRollbackSource(t *testing.T, candidate publishedCandidate, request releasetransition.ProcessRequest, registry []byte) {
	t.Helper()
	source := publishedCandidate{release: "release-a", root: filepath.Join(request.RuntimeRoot, "releases", "release-a")}
	targetEngine, err := os.ReadFile(filepath.Join(candidate.root, "bin", "yard-engine"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source.root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(source.root, "bin", "yard-engine"), targetEngine, 0o700)
	if err := os.MkdirAll(filepath.Join(source.root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if registry != nil {
		testkit.WriteFile(t, filepath.Join(source.root, "config", "release-transition.json"), registry, 0o600)
	}
	rewriteReleaseOwnerManifest(t, source)
}

func TestMigrationKindRollbackRechecksBeforeExecution(t *testing.T) {
	runtime, parsed, candidate, request, called := configLayoutRollbackFixture(t, "--scope")
	prepared, err := prepareCandidateTransitionForTest(runtime, context.Background(), parsed, candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	before := mustReadFile(t, called)
	writeMigrationRollbackSource(t, candidate, request, []byte(migrationRollbackSettingsRegistry))
	err = prepared.Execute(context.Background())
	assertReleaseSelectionStale(t, err)
	if string(mustReadFile(t, called)) != string(before) {
		t.Fatal("refused execution invoked the target")
	}
}

func assertReleaseSelectionStale(t *testing.T, err error) {
	t.Helper()
	outcome, ok := TransitionOutcome(err)
	if !errors.Is(err, domain.ErrPlanStale) || !ok || outcome.Code != releasetransition.CodePlanStale ||
		outcome.Message != "release state changed; retry" {
		t.Fatalf("selection change did not produce a stale refusal: %v", err)
	}
}

func TestReleaseSelectionRevalidatesAfterVMInspection(t *testing.T) {
	for _, entry := range []string{"rollback", "explicit update"} {
		for _, scenario := range []string{"current switch during preparation", "checkpoint advance during preparation", "current switch during execution", "checkpoint advance during execution", "previous appears"} {
			if entry == "rollback" && scenario == "previous appears" {
				continue
			}
			t.Run(entry+"/"+scenario, func(t *testing.T) {
				runtime, candidate, request, called, args := retainedVersionFixture(t)
				if entry == "rollback" {
					args = []string{"--rollback", "--runtime-root", request.RuntimeRoot}
				}
				bindRollbackFixtureRegistry(t, candidate, []byte(migrationRollbackSettingsRegistry))
				rewriteReleaseOwnerManifest(t, candidate)
				_, profileRoot, _, _, _ := retainedRuntimeFixture(t)
				runtime.config.RepositoryRoot = profileRoot
				writeVMRollbackSettings(t, request.ConfigHome, "default", "1")
				current := filepath.Join(request.RuntimeRoot, "current")
				previous := filepath.Join(request.RuntimeRoot, "previous")
				registry, digest, err := releasetransition.ParseRegistryV2([]byte(migrationRollbackSettingsRegistry), releasetransition.BuiltinCapabilityCatalog())
				if err != nil {
					t.Fatal(err)
				}
				store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
				if err != nil {
					t.Fatal(err)
				}
				state, err := store.ReadMigrationLedger(registry, digest)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.ConvertMigrationLedger(state, registry, digest, registry, digest); err != nil {
					t.Fatal(err)
				}
				state, err = store.ReadMigrationLedger(registry, digest)
				if err != nil {
					t.Fatal(err)
				}
				next, err := state.Ledger.Advance(registry, registry.Migrations[0])
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "previous appears" {
					if err := os.Remove(previous); err != nil {
						t.Fatal(err)
					}
				}
				execution := strings.HasSuffix(scenario, "during execution")
				changed := false
				runtime.config.YardInstance = func(context.Context, config.Loaded) (ports.InstanceInfo, error) {
					return ports.InstanceInfo{}, ports.ErrInstanceNotFound
				}
				var prepared Prepared
				if execution {
					prepared, err = runtime.PrepareTransition(context.Background(), args, request.ConfigHome, request.Yard)
					if err != nil {
						t.Fatal(err)
					}
					if prepared.Close != nil {
						t.Cleanup(func() { _ = prepared.Close() })
					}
				}
				runtime.config.YardInstance = func(context.Context, config.Loaded) (ports.InstanceInfo, error) {
					if changed {
						t.Fatal("VM probe repeated after the selection changed")
					}
					changed = true
					switch {
					case strings.HasPrefix(scenario, "current switch"):
						other := filepath.Join(request.RuntimeRoot, "releases", "release-c")
						if err := os.Mkdir(other, 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.Remove(current); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink("releases/release-c", current); err != nil {
							t.Fatal(err)
						}
					case strings.HasPrefix(scenario, "checkpoint advance"):
						if err := store.CompareAndSwapMigrationLedger(state, next, registry, digest); err != nil {
							t.Fatal(err)
						}
					default:
						if err := os.Symlink("releases/"+string(request.Target), previous); err != nil {
							t.Fatal(err)
						}
					}
					return ports.InstanceInfo{}, ports.ErrInstanceNotFound
				}
				if execution {
					before := mustReadFile(t, called)
					assertReleaseSelectionStale(t, prepared.Execute(context.Background()))
					if !bytes.Equal(before, mustReadFile(t, called)) {
						t.Fatal("execution delegated after the selection changed")
					}
				} else {
					_, err := runtime.PrepareTransition(context.Background(), args, request.ConfigHome, request.Yard)
					assertReleaseSelectionStale(t, err)
					if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("inspection delegated after the selection changed: %v", err)
					}
				}
				if !changed {
					t.Fatal("test did not reach the VM check")
				}
			})
		}
	}
}

func TestExplicitVersionRequiresRegistryIdentityWithoutPrevious(t *testing.T) {
	runtime, _, request, called, args := retainedVersionFixture(t)
	if err := os.Remove(filepath.Join(request.RuntimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}
	request.Direction = releasetransition.DirectionActivateTarget
	_, err := runtime.PrepareTransition(context.Background(), args, request.ConfigHome, request.Yard)
	assertMigrationRollbackRefused(t, request, err, "migration registry differs")
	if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incompatible explicit target received transition inspection: %v", err)
	}
}

type releaseProgressFunc func(string)

func (write releaseProgressFunc) Write(payload []byte) (int, error) {
	write(string(payload))
	return len(payload), nil
}

func TestEnvironmentPinnedInstalledReleaseRefusesNewPreviousBeforeInspection(t *testing.T) {
	runtime, candidate, request, called, _ := retainedVersionFixture(t)
	runtime.config.Inputs.values["YARD_RELEASE_VERSION"] = "0.29.0"
	bindRollbackFixtureRegistry(t, candidate, []byte(migrationRollbackSettingsRegistry))
	rewriteReleaseOwnerManifest(t, candidate)
	previous := filepath.Join(request.RuntimeRoot, "previous")
	if err := os.Remove(previous); err != nil {
		t.Fatal(err)
	}
	changed := false
	runtime.config.Progress = releaseProgressFunc(func(message string) {
		if message == "Checking update requirements...\n" {
			if err := os.Symlink("releases/"+string(request.Target), previous); err != nil {
				t.Fatal(err)
			}
			changed = true
		}
	})
	_, err := runtime.PrepareTransition(context.Background(), []string{"--offline", "--runtime-root", request.RuntimeRoot}, request.ConfigHome, request.Yard)
	assertReleaseSelectionStale(t, err)
	if !changed {
		t.Fatal("test did not install the previous link after selection")
	}
	if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale selection invoked transition inspection: %v", err)
	}
}

func TestEnvironmentPinnedUpdateRefusesPublicationCollisionWithDifferentArtifact(t *testing.T) {
	runtime, candidate, request, called, _ := retainedVersionFixture(t)
	runtime.config.Inputs.values["YARD_RELEASE_VERSION"] = "0.29.0"
	writeMigrationRollbackSource(t, candidate, request, []byte(sourceAdmissionRegistry))
	if err := os.Remove(filepath.Join(request.RuntimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(filepath.Dir(request.RuntimeRoot), "publication-template")
	if err := os.Rename(candidate.root, template); err != nil {
		t.Fatal(err)
	}
	installer := mustReadFile(t, runtime.config.Installer)
	testkit.WriteFile(t, runtime.config.Installer, bytes.ReplaceAll(installer, []byte(candidate.root), []byte(template)), 0o700)
	ctx := context.Background()
	args := []string{"--offline", "--runtime-root", request.RuntimeRoot}
	prepared, err := runtime.PrepareTransition(ctx, args, request.ConfigHome, request.Yard)
	if err != nil || !prepared.Changed {
		t.Fatalf("prepare fresh update: changed=%t, %v", prepared.Changed, err)
	}
	t.Cleanup(func() { _ = prepared.Close() })

	if err := os.Rename(template, candidate.root); err != nil {
		t.Fatal(err)
	}
	bindRollbackFixtureRegistry(t, candidate, []byte(migrationRollbackSettingsRegistry))
	rewriteReleaseOwnerManifest(t, candidate)
	testkit.WriteFile(t, runtime.config.Installer, installer, 0o700)
	assertReleaseSelectionStale(t, prepared.Execute(ctx))
	if got := string(mustReadFile(t, called)); got != "called\n" {
		t.Fatalf("expected only the initial draft inspection, got %q", got)
	}
	if current, err := os.Readlink(filepath.Join(request.RuntimeRoot, "current")); err != nil || current != "releases/release-a" {
		t.Fatalf("collision changed the active release: %q, %v", current, err)
	}

	request.Direction = releasetransition.DirectionActivateTarget
	_, err = runtime.PrepareTransition(ctx, args, request.ConfigHome, request.Yard)
	assertMigrationRollbackRefused(t, request, err, "migration registry differs")
	if got := string(mustReadFile(t, called)); got != "called\n" {
		t.Fatalf("fresh selection delegated to the incompatible installed release: %q", got)
	}
}

func TestForwardUpdateUsesFreshDraftAcrossRegistryChange(t *testing.T) {
	t.Run("approved draft", func(t *testing.T) { checkForwardDraftPublication(t, false) })
	t.Run("content changed during final inspection", func(t *testing.T) { checkForwardDraftPublication(t, true) })
}

func checkForwardDraftPublication(t *testing.T, changeContent bool) {
	t.Helper()
	runtime, candidate, request, called, args := retainedVersionFixture(t)
	if err := os.Remove(filepath.Join(request.RuntimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}
	// The fixture normally returns an inspection. Give the convergence call
	// its own response so the test exercises the same draft after publication.
	engine := filepath.Join(candidate.root, "bin", "yard-engine")
	ready := fmt.Sprintf(`{"schemaVersion":1,"outcome":{"status":"ready","reachedGoal":true,"active":%q,"previous":"release-a","target":%q,"code":"ready","message":"verified","transaction":"tx-0123456789abcdef"}}`,
		candidate.release, candidate.release)
	converge := fmt.Sprintf(`if grep -q '"mode":"converge"'; then printf 'called\n' >> %q; printf '%%s\n' '%s'; exit 0; fi;`, called, ready)
	if changeContent {
		converge += fmt.Sprintf(`if [ -f %q ]; then printf 'changed' > "$SUBYARD_REPOSITORY_ROOT/config/release-transition.json"; fi;`, called)
	}
	enginePayload := bytes.Replace(mustReadFile(t, engine), []byte("cat >/dev/null;"), []byte(converge), 1)
	testkit.WriteFile(t, engine, enginePayload, 0o700)
	rewriteReleaseOwnerManifest(t, candidate)
	template := filepath.Join(filepath.Dir(request.RuntimeRoot), "forward-template")
	if err := os.Rename(candidate.root, template); err != nil {
		t.Fatal(err)
	}
	installer := mustReadFile(t, runtime.config.Installer)
	testkit.WriteFile(t, runtime.config.Installer, bytes.ReplaceAll(installer, []byte(candidate.root), []byte(template)), 0o700)
	prepared, err := runtime.PrepareTransition(context.Background(), args, request.ConfigHome, request.Yard)
	if err != nil || !prepared.Changed {
		t.Fatalf("the fresh draft could not plan its migration: changed=%t, %v", prepared.Changed, err)
	}
	t.Cleanup(func() { _ = prepared.Close() })
	if got := string(mustReadFile(t, called)); got != "called\n" {
		t.Fatalf("expected the draft inspection, got %q", got)
	}
	if _, err := os.Lstat(candidate.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection published the draft: %v", err)
	}
	err = prepared.Execute(context.Background())
	if changeContent {
		if !errors.Is(err, domain.ErrPlanStale) || !strings.Contains(err.Error(), "reverify approved release draft") {
			t.Fatalf("content change did not fail before publication: %v", err)
		}
		if _, err := os.Lstat(candidate.root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("content change left a canonical release: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("the published draft could not converge: %v", err)
	}
	if got := string(mustReadFile(t, called)); got != "called\ncalled\ncalled\n" {
		t.Fatalf("expected inspection, reinspection and convergence, got %q", got)
	}
}

func TestPreStagedUpdatePreservesVerifiedSourceHopGuidance(t *testing.T) {
	runtime, candidate, request, called, args := retainedVersionFixture(t)
	if err := os.Remove(filepath.Join(request.RuntimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(request.RuntimeRoot, "releases", "0.11.0-aaaaaaaaaaaa")
	old := filepath.Join(request.RuntimeRoot, "releases", "release-a")
	engine := filepath.Join(old, "bin", "yard-engine")
	testkit.WriteFile(t, engine, bytes.ReplaceAll(mustReadFile(t, engine), []byte("yard-engine 0.29.0"), []byte("yard-engine 0.11.0")), 0o700)
	rewriteReleaseOwnerManifest(t, publishedCandidate{root: old})
	if err := os.Rename(old, active); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(request.RuntimeRoot, "current")
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/"+filepath.Base(active), current); err != nil {
		t.Fatal(err)
	}
	checkGuidance := func(t *testing.T) {
		t.Helper()
		_, err := runtime.PrepareTransition(context.Background(), args, request.ConfigHome, request.Yard)
		if err == nil || !strings.Contains(err.Error(), "source-anchor-required: installed v0.11.0") ||
			!strings.Contains(err.Error(), "next: yard update --version "+UpdateHopVersion+" && yard update") {
			t.Fatalf("pre-staged %s lost source hop guidance: %v", candidate.release, err)
		}
		if _, err := os.Lstat(called); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a candidate transition ran before the source hop refusal: %v", err)
		}
	}
	t.Run("different registries", checkGuidance)
	t.Run("matching registries without history", func(t *testing.T) {
		registryPath := "config/release-transition.json"
		testkit.WriteFile(t, filepath.Join(active, registryPath), mustReadFile(t, filepath.Join(candidate.root, registryPath)), 0o600)
		rewriteReleaseOwnerManifest(t, publishedCandidate{root: active})
		if err := os.Remove(filepath.Join(request.ConfigHome, "release-transition/v2/ledger.json")); err != nil {
			t.Fatal(err)
		}
		checkGuidance(t)
	})
}

func TestUnfinishedJournalCannotDelegateToIncompatiblePrevious(t *testing.T) {
	for _, standalone := range []bool{false, true} {
		t.Run(fmt.Sprintf("standalone=%t", standalone), func(t *testing.T) {
			runtime, candidate, request, called, _ := retainedFixtureAtVersion(t, "2.0.0")
			unfinished := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
			owner := filepath.Join("releases", unfinished.target)
			if err := os.Rename(filepath.Join(unfinished.runtimeRoot, owner), filepath.Join(request.RuntimeRoot, owner)); err != nil {
				t.Fatal(err)
			}
			journal := mustReadFile(t, unfinished.journalPath)
			journalPath := filepath.Join(request.ConfigHome, "release-transition", "v2", "journal.json")
			testkit.WriteFile(t, journalPath, journal, 0o600)
			ctx := context.Background()
			protected, err := runtime.inspectProtectedTransition(ctx, request.RuntimeRoot, request.ConfigHome, "default", nil)
			if err != nil || protected == nil || protected.inspection.Resume == nil {
				t.Fatalf("exact journal owner could not inspect its unfinished transaction: %v", err)
			}

			args := []string{"--version", "2.0.0", "--runtime-root", request.RuntimeRoot}
			if standalone {
				runtime.config.RepositoryRoot = candidate.root
				args = append(args, "--offline")
				testkit.WriteFile(t, runtime.config.Installer, []byte("#!/bin/sh\nexit 97\n"), 0o700)
			} else {
				cache := runtime.config.Inputs.values["YARD_RELEASE_CACHE"]
				runtime.config.Inputs.values["YARD_RELEASE_BASE_URL"] = "file://" + filepath.Join(cache, "2.0.0")
			}
			request.Direction = releasetransition.DirectionActivateTarget
			_, err = runtime.PrepareTransition(ctx, args, request.ConfigHome, request.Yard)
			assertMigrationRollbackRefused(t, request, err, "migration registry differs")
			if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("incompatible previous release inspected the unfinished transaction: %v", err)
			}
			if !bytes.Equal(journal, mustReadFile(t, journalPath)) {
				t.Fatal("refusal changed the unfinished journal")
			}
		})
	}
}

func TestTransitionLaunchRefusesNonActiveEngineWithoutSelection(t *testing.T) {
	runtime, candidate, request, called, _ := retainedVersionFixture(t)
	writeMigrationRollbackSource(t, candidate, request, []byte(sourceAdmissionRegistry))
	ctx := context.Background()
	verified, err := runtime.verifyPublishedCandidate(ctx, candidate, request.RuntimeRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	request.ArtifactDigest = verified.manifestDigest
	_, err = runtime.invokeVerifiedCandidateTransition(ctx, verified, request, "")
	assertReleaseSelectionStale(t, err)

	active := publishedCandidate{release: "release-a", root: filepath.Join(request.RuntimeRoot, "releases", "release-a")}
	assets, err := runtime.verifyPublishedCandidate(ctx, active, request.RuntimeRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer assets.Close()
	_, err = runtime.invokeVerifiedRuntimeTransition(ctx, assets, verified, request, "")
	assertReleaseSelectionStale(t, err)
	assertMigrationRollbackUnchanged(t, request, called)
}
