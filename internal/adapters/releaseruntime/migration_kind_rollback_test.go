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

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestMigrationKindRollbackPublishedTarget(t *testing.T) {
	source := releasetransition.RegistryV2{Migrations: []releasetransition.MigrationDefinitionV2{
		{ID: "z-future", Kind: "future-v1", Domain: "settings"},
		{ID: "a-guard", Kind: "nm-device-guard-v1", Domain: "power-metadata"},
		{ID: "canonical", Kind: "test-vms-settings-v1-to-v2", Domain: "settings"},
	}}
	target := &verifiedPublishedCandidate{version: "0.29.0", manifestDigest: "4591220831f78d5893a50bdc2ff59e4f21f09a2b5a4f99a15390cb1107eb6cdb", registryDigest: "bound"}
	findings := migrationKindRollbackFindings(target, source, releasetransition.RegistryV2{}, nil)
	if len(findings) != 2 || !strings.Contains(findings[0], "release 0.29.0 does not know migration a-guard (kind nm-device-guard-v1)") || !strings.Contains(findings[1], "z-future") {
		t.Fatalf("findings = %q", findings)
	}
	for _, change := range []string{"manifest", "version"} {
		changed := *target
		if change == "manifest" {
			changed.manifestDigest = releasetransition.Fingerprint(strings.Repeat("a", 64))
		} else {
			changed.version = "0.30.0"
		}
		unknown := migrationKindRollbackFindings(&changed, source, releasetransition.RegistryV2{}, nil)
		if len(unknown) != 3 || !strings.Contains(unknown[0], "cannot verify support") {
			t.Fatalf("%s inferred capabilities: %q", change, unknown)
		}
	}
}

func TestMigrationKindRollbackRegisteredKindsAreOnlyLowerBound(t *testing.T) {
	migration := releasetransition.MigrationDefinitionV2{ID: "source", Kind: "test-vms-settings-v1-to-v2", Domain: "settings"}
	source := releasetransition.RegistryV2{Migrations: []releasetransition.MigrationDefinitionV2{migration}}
	retained := source
	retained.Migrations = []releasetransition.MigrationDefinitionV2{{ID: "different-id", Kind: migration.Kind, Domain: migration.Domain}}
	target := &verifiedPublishedCandidate{version: "custom", registryDigest: "bound"}
	if findings := migrationKindRollbackFindings(target, source, retained, nil); len(findings) != 0 {
		t.Fatalf("registered support refused: %q", findings)
	}
	retained.Migrations[0].Domain = "power-metadata"
	if findings := migrationKindRollbackFindings(target, source, retained, nil); len(findings) != 1 {
		t.Fatalf("wrong domain accepted: %q", findings)
	}
}

func TestMigrationKindRollbackRefusesBeforeTargetInspectionAndCollectsLayout(t *testing.T) {
	runtime, parsed, candidate, request, called := configLayoutRollbackFixture(t, "--scope")
	writeMigrationRollbackSource(t, candidate, request, []byte(migrationRollbackSettingsRegistry))
	writeConfigLayoutManifest(t, request.ConfigHome, 2, true)
	manifest := filepath.Join(request.ConfigHome, ".sync", "manifest.json")
	before := mustReadFile(t, manifest)
	_, err := runtime.prepareRetainedTransition(context.Background(), parsed, request.ConfigHome, request.Yard, nil)
	failure := assertMigrationRollbackRefused(t, request, err, "migration canonicalize", "Git configuration cache")
	if failure.Active != "release-a" || failure.Previous == nil || *failure.Previous != "release-b" {
		t.Fatalf("refusal reported unexpected runtime links: %+v", failure)
	}
	assertMigrationRollbackUnchanged(t, request, called)
	if !bytes.Equal(before, mustReadFile(t, manifest)) {
		t.Fatal("refusal changed the configuration manifest")
	}
	if _, err := os.Stat(filepath.Join(request.ConfigHome, "release-transition")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection created migration history: %v", err)
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
	assertMigrationRollbackRefused(t, request, err, "active release's sealed migration inventory cannot be verified", "restore the exact active release files")
	assertMigrationRollbackUnchanged(t, request, called)
	if !bytes.Equal(before, mustReadFile(t, manifest)) {
		t.Fatal("refusal modified the active manifest")
	}
}

func TestMigrationKindRollbackRefusesExplicitVersionSelectingPrevious(t *testing.T) {
	runtime, _, request, called, args := retainedVersionFixture(t)
	request.Direction = releasetransition.DirectionActivateTarget
	_, err := runtime.PrepareTransition(context.Background(), args, request.ConfigHome, request.Yard)
	assertMigrationRollbackRefused(t, request, err, "migration canonicalize")
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
	assertMigrationRollbackOutcome(t, request, report.Outcome, "migration canonicalize")
	assertMigrationRollbackUnchanged(t, request, called)
}

func retainedVersionFixture(t *testing.T) (*Runtime, publishedCandidate, releasetransition.ProcessRequest, string, []string) {
	t.Helper()
	runtime, _, candidate, request, called := configLayoutRollbackFixture(t, "--scope")
	bundle := []byte("retained release fixture")
	version := "0.29.0"
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

func TestMigrationKindRollbackAcceptsRetainedTargetWithMatchingRetiredPrefix(t *testing.T) {
	runtime, candidate, request, called, updateArgs := retainedVersionFixture(t)
	catalog := releasetransition.BuiltinCapabilityCatalog()
	source, sourceDigest, err := releasetransition.ParseRegistryV2([]byte(migrationRollbackSettingsRegistry), catalog)
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := source.MigrationPrefixDigest("settings", 2)
	if err != nil {
		t.Fatal(err)
	}
	checkpointMarker := mustReadFile(t, filepath.Join("..", "..", "..", "config", "release-checkpoint.json"))
	testkit.WriteFile(t, filepath.Join(candidate.root, "config", "release-checkpoint.json"), checkpointMarker, 0o600)
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadMigrationLedger(source, sourceDigest)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := state.Ledger.Advance(source, source.Migrations[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapMigrationLedger(state, ledger, source, sourceDigest); err != nil {
		t.Fatal(err)
	}
	state, err = store.ReadMigrationLedger(source, sourceDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConvertMigrationLedger(state, source, sourceDigest, source, sourceDigest); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name      string
		args      []string
		direction releasetransition.Direction
	}{
		{"rollback", []string{"--rollback", "--runtime-root", request.RuntimeRoot}, releasetransition.DirectionActivatePrevious},
		{"explicit update", updateArgs, releasetransition.DirectionActivateTarget},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			for _, digest := range []releasetransition.Fingerprint{releasetransition.Fingerprint(strings.Repeat("f", 64)), prefix} {
				registry := []byte(fmt.Sprintf(`{"schemaVersion":2,"minimumEpochs":{"settings":2},"currentEpochs":{"settings":2},"migrations":[],"retiredPrefixes":{"settings":{"throughEpoch":2,"digest":%q}}}`, digest))
				retained, registryDigest, err := releasetransition.ParseRegistryV2(registry, catalog)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.ReadMigrationLedger(retained, registryDigest); (err == nil) != (digest == prefix) {
					t.Fatalf("checkpoint reader disagreed with the fixture's prefix match: %v", err)
				}
				bindRollbackFixtureRegistry(t, candidate, registry)
				rewriteReleaseOwnerManifest(t, candidate)
				request.Direction = scenario.direction
				prepared, err := runtime.PrepareTransition(context.Background(), scenario.args, request.ConfigHome, request.Yard)
				if digest != prefix {
					assertMigrationRollbackRefused(t, request, err, "migration canonicalize")
					continue
				}
				if err != nil || !prepared.Changed || prepared.Request.Direction != scenario.direction {
					t.Fatalf("compatible retained release did not produce a plan: %+v, %v", prepared, err)
				}
				if prepared.Close != nil {
					if err := prepared.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
	if got := string(mustReadFile(t, called)); got != "called\ncalled\n" {
		t.Fatalf("retained transition inspections = %q, want two", got)
	}
}

func TestMigrationKindRollbackRefusesNetworkManagerEpochBeforeCheckpointGate(t *testing.T) {
	const priorRegistry = `{"schemaVersion":2,"minimumEpochs":{"power-metadata":3},"currentEpochs":{"power-metadata":3},"migrations":[]}`
	const currentRegistry = `{"schemaVersion":2,"minimumEpochs":{"power-metadata":3},"currentEpochs":{"power-metadata":4},"migrations":[{"id":"introduce-nm-device-guard-v4","kind":"nm-device-guard-v1","domain":"power-metadata","fromEpoch":3,"toEpoch":4}]}`
	runtime, parsed, candidate, request, called := configLayoutRollbackFixture(t, "--scope")
	bindRollbackFixtureRegistry(t, candidate, []byte(priorRegistry))
	rewriteReleaseOwnerManifest(t, candidate)
	writeMigrationRollbackSource(t, candidate, request, []byte(currentRegistry))
	registry, digest, err := releasetransition.ParseRegistryV2([]byte(currentRegistry), releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := releasetransition.BaselineLedgerV2(registry).Advance(registry, registry.Migrations[0])
	if err != nil {
		t.Fatal(err)
	}
	projection, _, err := releasetransition.MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(raw, projection); err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadMigrationLedger(registry, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConvertMigrationLedger(state, registry, digest, registry, digest); err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadMigrationCheckpoint()
	if err != nil || !before.Exists {
		t.Fatalf("checkpoint setup: %v", err)
	}

	prefix, err := registry.MigrationPrefixDigest("power-metadata", 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range []string{"live migration", "retired prefix"} {
		t.Run(shape, func(t *testing.T) {
			if shape == "retired prefix" {
				registry.MinimumEpochs["power-metadata"] = 4
				registry.Migrations = nil
				registry.RetiredPrefixes = map[string]releasetransition.RetiredMigrationPrefixV2{"power-metadata": {ThroughEpoch: 4, Digest: prefix}}
				payload, err := json.Marshal(registry)
				if err != nil {
					t.Fatal(err)
				}
				writeMigrationRollbackSource(t, candidate, request, payload)
			}
			_, err := runtime.prepareRetainedTransition(context.Background(), parsed, request.ConfigHome, request.Yard, nil)
			assertMigrationRollbackRefused(t, request, err, "cannot read power-metadata epoch 4", "older runtimes can replace the guard during init")
			assertMigrationRollbackUnchanged(t, request, called)
			after, err := store.ReadMigrationCheckpoint()
			if err != nil || !after.Exists || !bytes.Equal(before.Payload, after.Payload) {
				t.Fatalf("refusal changed checkpoint: %v", err)
			}
			raw, err := store.ReadLedger()
			if err != nil || !raw.Exists || !bytes.Equal(raw.Payload, projection) {
				t.Fatalf("refusal changed ledger: %v", err)
			}
		})
	}
}

func TestMigrationKindRollbackReportsAppliedState(t *testing.T) {
	target := &verifiedPublishedCandidate{version: "0.29.0", manifestDigest: "4591220831f78d5893a50bdc2ff59e4f21f09a2b5a4f99a15390cb1107eb6cdb"}
	source := releasetransition.RegistryV2{Migrations: []releasetransition.MigrationDefinitionV2{{ID: "future", Kind: "future-v1", Domain: "settings", FromEpoch: 3, ToEpoch: 4}}}
	for _, epoch := range []int{3, 4} {
		ledger := releasetransition.LedgerV2{Domains: map[string]releasetransition.DomainLedgerV2{"settings": {Epoch: epoch}}}
		findings := migrationKindRollbackFindings(target, source, releasetransition.RegistryV2{}, &ledger)
		want := "the registration has not been applied"
		if epoch == 4 {
			want = "the migration is applied"
		}
		if len(findings) != 1 || !strings.Contains(findings[0], want) {
			t.Fatalf("epoch %d: %q", epoch, findings)
		}
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
	var failure publicReleaseInspectionError
	if !errors.As(err, &failure) || failure.outcome.Code != releasetransition.CodeRollbackIncompatible || !strings.Contains(failure.outcome.Message, "migration canonicalize") {
		t.Fatalf("refusal = %v", err)
	}
	if string(mustReadFile(t, called)) != string(before) {
		t.Fatal("refused execution invoked the target")
	}
}
