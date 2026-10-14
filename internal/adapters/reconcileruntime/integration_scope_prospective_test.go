package reconcileruntime

import (
	"context"
	"errors"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationScopePreparedBytesMatchNativeWithoutProjectingPackageAuthority(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config", "projects-changed.sh"), []byte("#!/bin/sh\n"), 0755)
	definition := profile.IntegrationDefinition{Name: "sample", Root: root}
	testkit.WriteFile(t, filepath.Join(root, "integration.json"), []byte(`{"schema_version":1}`), 0644)
	asset := filepath.Join(root, "settings.json")
	provision := filepath.Join(root, "operator-provision.sh")
	cleanup := filepath.Join(root, "operator-cleanup.sh")
	for _, path := range []string{asset, provision, cleanup} {
		testkit.WriteFile(t, path, []byte("old"), 0600)
	}
	runtime := Runtime{RepositoryRoot: root, Integrations: []profile.IntegrationDefinition{definition}, Environment: []string{"CODING_TOOL_INTEGRATIONS=sample", "AGENT_sample_CONFIG=" + asset, "AGENT_sample_CONFIG_DEST=.sample/settings", "AGENT_sample_PROVISION=" + provision, "AGENT_sample_CLEANUP=" + cleanup}}
	nativeBefore, _, err := runtime.IntegrationScope()
	if err != nil {
		t.Fatal(err)
	}
	bindingBefore, err := definition.SourceBinding()
	if err != nil {
		t.Fatal(err)
	}
	pending := map[string][]byte{asset: []byte("imported asset"), provision: []byte("imported provision"), cleanup: []byte("imported cleanup")}
	projected, _, err := runtime.IntegrationScopeWithSourceReader(func(source config.MaterializedAsset) ([]byte, error) {
		if bytes, ok := pending[source.Source]; ok {
			return bytes, nil
		}
		return source.ReadSource()
	})
	if err != nil {
		t.Fatal(err)
	}
	if projected == nativeBefore {
		t.Fatal("prospective content did not change desired integration scope")
	}
	for path, bytes := range pending {
		testkit.WriteFile(t, path, bytes, 0600)
	}
	nativeAfter, _, err := runtime.IntegrationScope()
	if err != nil || nativeAfter != projected {
		t.Fatalf("projected %s native %s err %v", projected, nativeAfter, err)
	}
	bindingAfter, err := definition.SourceBinding()
	if err != nil || bindingAfter != bindingBefore {
		t.Fatal("operator projection changed native package authority", err)
	}
}

func TestPreparedFullIntegrationPlanRetainsNativeOwnershipAndAdoption(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	instructions := filepath.Join(root, "instructions.md")
	asset := filepath.Join(root, "codex-config.json")
	testkit.WriteFile(t, filepath.Join(root, "config", "projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0644)
	testkit.WriteFile(t, instructions, []byte("old instructions"), 0600)
	testkit.WriteFile(t, asset, []byte(`{"selected":"old"}`), 0600)
	executor := &preparedAdoptionExecutor{fingerprint: strings.Repeat("a", 64), configFingerprint: strings.Repeat("c", 64), initial: true}
	instance := ports.InstanceInfo{Status: "Running", Config: map[string]string{}, Devices: map[string]map[string]string{}}
	runtime := Runtime{RepositoryRoot: root, Integrations: []profile.IntegrationDefinition{}, Incus: &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: instance}}, Executor: executor, Yard: domain.Context{IncusProject: "test", YardInstanceName: "yard", DevUser: "dev", DevUID: os.Getuid()}, Environment: []string{"CODING_TOOL_INTEGRATIONS=codex", "HOST_CODEX_AGENTS_MD=" + instructions, "AGENT_codex_CONFIG=" + asset, "AGENT_codex_CONFIG_DEST=.codex/config.json"}}
	oldRuntime, _, err := runtime.PrepareLegacyIntegrationAdoption(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	old, err := oldRuntime.IntegrationPlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pending := map[string][]byte{instructions: []byte("imported instructions"), asset: []byte(`{"selected":"imported"}`)}
	projected, err := runtime.PrepareSourceIntegrationPlan(context.Background(), func(source config.MaterializedAsset) ([]byte, error) {
		if bytes, ok := pending[source.Source]; ok {
			return bytes, nil
		}
		return source.ReadSource()
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Fingerprint == old.Fingerprint || projected.Scope == old.Scope || projected.AdoptionFingerprint == "" || len(projected.facts) == 0 {
		t.Fatal("projection lost complete native source/ownership/adoption facts")
	}
	for path, bytes := range pending {
		testkit.WriteFile(t, path, bytes, 0600)
	}
	native := runtime
	native.AdoptLegacyIntegrations = projected.AdoptionFingerprint != ""
	native.LegacyIntegrationFingerprint = projected.AdoptionFingerprint
	actual, err := native.IntegrationPlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if projected.Fingerprint != actual.Fingerprint || CheckIntegrationPlan(projected, actual) != nil {
		t.Fatal("projected full plan differs after source publication")
	}
	executor.configFingerprint = strings.Repeat("d", 64)
	if _, err := native.IntegrationPlan(context.Background()); err == nil {
		t.Fatal("post-source adoption ownership drift was accepted")
	}
	executor.configFingerprint = strings.Repeat("c", 64)
	instance.Config["foreign.owner"] = "changed"
	fresh, err := native.IntegrationPlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(CheckIntegrationPlan(projected, fresh), domain.ErrPlanStale) {
		t.Fatal("post-source instance ownership drift retained approval")
	}
	for _, mode := range executor.configModes {
		if mode == "apply" || mode == "retire" {
			t.Fatal("prospective full plan wrote integrations")
		}
	}
}
