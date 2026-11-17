package releaseruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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
	var failure publicReleaseInspectionError
	if !errors.As(err, &failure) || failure.outcome.Code != releasetransition.CodeRollbackIncompatible {
		t.Fatalf("refusal = %v", err)
	}
	if err := releasetransition.ValidateProcessOutcome(releasetransition.Goal{Target: request.Target, Direction: request.Direction}, failure.outcome); err != nil {
		t.Fatalf("refusal violates the frozen outcome contract: %v", err)
	}
	for _, message := range messages {
		if !strings.Contains(failure.outcome.Message, message) {
			t.Fatalf("refusal message %q lacks %q", failure.outcome.Message, message)
		}
	}
	if want := "keep the current release active and select a verified retained release supporting all registered migrations before retrying yard update --rollback"; failure.outcome.Retry != want {
		t.Fatalf("next action = %q, want %q", failure.outcome.Retry, want)
	}
	return failure.outcome
}

func assertMigrationRollbackUnchanged(t *testing.T, request releasetransition.ProcessRequest, called string) {
	t.Helper()
	if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target inspected before refusal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(request.ConfigHome, "release-transition/v2/journal.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection created a journal: %v", err)
	}
	for link, want := range map[string]string{"current": "releases/release-a", "previous": "releases/release-b"} {
		got, err := os.Readlink(filepath.Join(request.RuntimeRoot, link))
		if err != nil || got != want {
			t.Fatalf("%s link = %q, %v, want %q", link, got, err, want)
		}
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

	_, err = runtime.prepareRetainedTransition(context.Background(), parsed, request.ConfigHome, request.Yard, nil)
	assertMigrationRollbackRefused(t, request, err, "introduce-nm-device-guard-v4", "the migration is applied", "older runtimes can replace the NetworkManager device guard during init")
	assertMigrationRollbackUnchanged(t, request, called)
	after, err := store.ReadMigrationCheckpoint()
	if err != nil || !after.Exists || !bytes.Equal(before.Payload, after.Payload) {
		t.Fatalf("refusal changed checkpoint: %v", err)
	}
	raw, err = store.ReadLedger()
	if err != nil || !raw.Exists || !bytes.Equal(raw.Payload, projection) {
		t.Fatalf("refusal changed ledger: %v", err)
	}
}

func TestMigrationKindRollbackReportsAppliedState(t *testing.T) {
	target := &verifiedPublishedCandidate{version: "0.29.0", manifestDigest: "4591220831f78d5893a50bdc2ff59e4f21f09a2b5a4f99a15390cb1107eb6cdb"}
	source := releasetransition.RegistryV2{Migrations: []releasetransition.MigrationDefinitionV2{{ID: "guard", Kind: "nm-device-guard-v1", Domain: "power-metadata", FromEpoch: 3, ToEpoch: 4}}}
	for _, epoch := range []int{3, 4} {
		ledger := releasetransition.LedgerV2{Domains: map[string]releasetransition.DomainLedgerV2{"power-metadata": {Epoch: epoch}}}
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
	bindRollbackFixtureRegistry(t, source, registry)
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
