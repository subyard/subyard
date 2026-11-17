package releaseruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRollbackTargetAssetsAcceptStructuralSourceWithoutExecutingItsKinds(t *testing.T) {
	runtimeRoot := testkit.TempDir(t)
	sourcePayload := []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":2},"migrations":[{"id":"future-v2","domain":"settings","fromEpoch":1,"toEpoch":2,"kind":"future-source-v1"}]}`)
	source := writeDelegateRuntimeFixture(t, runtimeRoot, "1.3.0-source", "source", sourcePayload, "#!/bin/sh\nprintf 'yard-engine 1.3.0\\n'\n")
	targetPayload := []byte(sourceAdmissionRegistry)
	target := writeDelegateRuntimeFixture(t, runtimeRoot, "1.2.3-target", "target", targetPayload, "#!/bin/sh\nprintf 'yard-engine 1.2.3\\n'\n")
	runtime := New(Config{})
	defer runtime.Close()
	verifiedSource, err := runtime.verifyPublishedCandidate(context.Background(), source, runtimeRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer verifiedSource.Close()
	verifiedTarget, err := runtime.verifyPublishedCandidate(context.Background(), target, runtimeRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer verifiedTarget.Close()
	registry, _, err := releasetransition.ParseRegistryV2(targetPayload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	native := releasetransition.BaselineLedgerV2(registry)
	configHome := testkit.TempDir(t)
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	request := releasetransition.RollbackTargetRequest{SchemaVersion: 3, Mode: "activate-under-v2", RuntimeRoot: runtimeRoot, ConfigHome: configHome, Releases: releasetransition.ReleasePair{From: source.release, Target: target.release}, SourceArtifact: verifiedSource.manifestDigest, SourceRegistry: verifiedSource.registryDigest, TargetArtifact: verifiedTarget.manifestDigest, TargetRegistry: verifiedTarget.registryDigest, Projection: projection.Fingerprint, NativeHistory: &native}
	payload, version, err := VerifyRollbackTargetAssets(context.Background(), target.root, request)
	if err != nil || string(payload) != string(targetPayload) || version != "1.2.3" {
		t.Fatalf("target admission = %q, %s, %v", payload, version, err)
	}
	if _, _, err := releasetransition.ParseRegistryV2(sourcePayload, releasetransition.BuiltinCapabilityCatalog()); err == nil {
		t.Fatal("structural source admission granted execution support")
	}
	changed := request
	changed.SourceRegistry = releasetransition.Fingerprint(strings.Repeat("a", 64))
	changed.Projection = releasetransition.Fingerprint(strings.Repeat("b", 64))
	_, _, err = VerifyRollbackTargetAssets(context.Background(), source.root, changed)
	if err == nil || !strings.Contains(err.Error(), "source registry binding changed") || !strings.Contains(err.Error(), "sealed target assets") || !strings.Contains(err.Error(), "projection changed") {
		t.Fatalf("independent asset refusals lost: %v", err)
	}
}

func TestRollbackTargetAssetsRefuseSharedPrefixDependencyDrift(t *testing.T) {
	ctx := context.Background()
	root, home := testkit.TempDir(t), testkit.TempDir(t)
	targetPayload := []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1,"owner-registration":1},"currentEpochs":{"settings":2,"owner-registration":2},"migrations":[{"id":"settings-v2","domain":"settings","fromEpoch":1,"toEpoch":2,"kind":"test-vms-settings-v1-to-v2"},{"id":"owner-v2","domain":"owner-registration","fromEpoch":1,"toEpoch":2,"kind":"test-yard-owner-v1-to-v2"}]}`)
	registry, _, err := releasetransition.ParseRegistryV2(targetPayload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	registry.Migrations[1].DependsOn = []string{"settings-v2"}
	sourcePayload, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	source := writeDelegateRuntimeFixture(t, root, "1.3.0-source", "source", sourcePayload, "#!/bin/sh\nprintf 'yard-engine 1.3.0\\n'\n")
	target := writeDelegateRuntimeFixture(t, root, "1.2.3-target", "target", targetPayload, "#!/bin/sh\nprintf 'yard-engine 1.2.3\\n'\n")
	runtime := New(Config{})
	defer runtime.Close()
	sealedS, err := runtime.verifyPublishedCandidate(ctx, source, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sealedS.Close()
	sealedR, err := runtime.verifyPublishedCandidate(ctx, target, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sealedR.Close()
	store, err := releasetransition.NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	native := releasetransition.LedgerV2{SchemaVersion: 2, Domains: map[string]releasetransition.DomainLedgerV2{
		"settings": {Epoch: 2, Applied: []string{"settings-v2"}}, "owner-registration": {Epoch: 2, Applied: []string{"owner-v2"}},
	}}
	request := releasetransition.RollbackTargetRequest{SchemaVersion: 3, Mode: "activate-under-v2", RuntimeRoot: root, ConfigHome: home, Releases: releasetransition.ReleasePair{From: source.release, Target: target.release}, SourceArtifact: sealedS.manifestDigest, SourceRegistry: sealedS.registryDigest, TargetArtifact: sealedR.manifestDigest, TargetRegistry: sealedR.registryDigest, Projection: projection.Fingerprint, NativeHistory: &native}
	if _, _, err := VerifyRollbackTargetAssets(ctx, target.root, request); err == nil || !strings.Contains(err.Error(), `different migration prefix for domain "owner-registration"`) {
		t.Fatalf("shared prefix drift admission = %v", err)
	}
}
