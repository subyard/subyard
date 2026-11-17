package releaseruntime

import (
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
	target := &verifiedPublishedCandidate{version: "0.29.0", manifestDigest: "4591220831f78d5893a50bdc2ff59e4f21f09a2b5a4f99a15390cb1107eb6cdb"}
	findings := migrationKindRollbackFindings(target, source, releasetransition.RegistryV2{}, nil)
	if len(findings) != 2 || findings[0] != "release 0.29.0 does not know migration a-guard (kind nm-device-guard-v1); applied state could not be established; rollback is incompatible" || !strings.Contains(findings[1], "z-future") {
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
		if len(unknown) != 3 || !strings.Contains(unknown[0], "cannot be established") {
			t.Fatalf("%s inferred capabilities: %q", change, unknown)
		}
	}
}

func TestMigrationKindRollbackRegisteredKindsAreOnlyLowerBound(t *testing.T) {
	migration := releasetransition.MigrationDefinitionV2{ID: "source", Kind: "test-vms-settings-v1-to-v2", Domain: "settings"}
	source := releasetransition.RegistryV2{Migrations: []releasetransition.MigrationDefinitionV2{migration}}
	retained := source
	retained.Migrations = []releasetransition.MigrationDefinitionV2{{ID: "different-id", Kind: migration.Kind, Domain: migration.Domain}}
	target := &verifiedPublishedCandidate{version: "custom"}
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
	writeMigrationRollbackSource(t, candidate, request)
	writeConfigLayoutManifest(t, request.ConfigHome, 2, true)
	_, err := prepareCandidateTransitionForTest(runtime, context.Background(), parsed, candidate, request)
	var failure publicReleaseInspectionError
	if !errors.As(err, &failure) || failure.outcome.Code != releasetransition.CodeRollbackIncompatible {
		t.Fatalf("refusal = %v", err)
	}
	if !strings.Contains(failure.outcome.Message, "migration canonicalize") || !strings.Contains(failure.outcome.Message, "Git configuration cache") {
		t.Fatalf("independent findings lost: %s", failure.outcome.Message)
	}
	if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target inspected before refusal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(request.ConfigHome, "release-transition")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection mutated history: %v", err)
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

func writeMigrationRollbackSource(t *testing.T, candidate publishedCandidate, request releasetransition.ProcessRequest) {
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
	bindRollbackFixtureRegistry(t, source, []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":2},"migrations":[{"id":"canonicalize","kind":"test-vms-settings-v1-to-v2","domain":"settings","fromEpoch":1,"toEpoch":2}]}`))
	rewriteReleaseOwnerManifest(t, source)
}

func TestMigrationKindRollbackRechecksBeforeExecution(t *testing.T) {
	runtime, parsed, candidate, request, called := configLayoutRollbackFixture(t, "--scope")
	prepared, err := prepareCandidateTransitionForTest(runtime, context.Background(), parsed, candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	before := mustReadFile(t, called)
	writeMigrationRollbackSource(t, candidate, request)
	err = prepared.Execute(context.Background())
	var failure publicReleaseInspectionError
	if !errors.As(err, &failure) || failure.outcome.Code != releasetransition.CodeRollbackIncompatible || !strings.Contains(failure.outcome.Message, "migration canonicalize") {
		t.Fatalf("refusal = %v", err)
	}
	if string(mustReadFile(t, called)) != string(before) {
		t.Fatal("refused execution invoked the target")
	}
}
