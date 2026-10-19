package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type afterOwnerIntegrationExecutor struct {
	*retryableIntegrationExecutor
	observed string
}

func (e *afterOwnerIntegrationExecutor) Exec(ctx context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	result, err := e.retryableIntegrationExecutor.Exec(ctx, project, name, request)
	if err != nil {
		return result, err
	}
	if len(request.Command) >= 5 && request.Command[0] == "python3" && strings.Contains(request.Command[3], "Version 1 records observed ownership only") && request.Command[4] == "observe" {
		var input struct {
			Entries []integrationArtifact `json:"entries"`
		}
		if json.Unmarshal(request.Stdin, &input) != nil {
			return result, err
		}
		var observation integrationObservation
		if json.Unmarshal(result.Stdout, &observation) != nil {
			return result, err
		}
		for _, entry := range input.Entries {
			path := entry.Path
			if path == "" {
				path = entry.ID
			}
			observation.Facts = append(observation.Facts, integrationFact{Key: entry.Kind + ":" + path, Desired: strings.Repeat("a", 64), Observed: e.observed})
		}
		observation.Fingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte(e.observed)))
		result.Stdout, _ = json.Marshal(observation)
	}
	return result, nil
}

func TestAfterOwnerIntegrationRetainsOriginalPhysicalFacts(t *testing.T) {
	ctx := context.Background()
	root := os.Getenv("SUBYARD_AFTER_OWNER_INTEGRATION_CHILD")
	child := root != ""
	if !child {
		root = testkit.TempDir(t)
	}
	runtime, base := materializedIntegrationFixture(t, root, !child)
	executor := &afterOwnerIntegrationExecutor{base, strings.Repeat("b", 64)}
	runtime.Executor = executor
	incus := runtime.Incus.(*testkit.Incus)
	if child {
		raw, err := os.ReadFile(filepath.Join(root, "original.recipe"))
		if err != nil {
			t.Fatal(err)
		}
		physical, err := os.ReadFile(filepath.Join(root, "physical.record"))
		if err != nil {
			t.Fatal(err)
		}
		restored, err := runtime.BindAfterOwnerIntegrations(ctx, raw, physical)
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		executor.observed = strings.Repeat("c", 64)
		if _, _, err := runtime.ObserveAfterOwnerIntegrations(ctx, restored); err == nil {
			t.Fatal("fresh process adopted changed pending fact")
		}
		return
	}
	incus.Reconcile.InstanceFound = false
	source, err := runtime.PrepareAfterOwnerIntegrationSources(ctx)
	if err != nil {
		t.Fatal("original sources", err)
	}
	defer source.Close()
	if len(executor.requests) != 0 {
		t.Fatal("source-only capture inspected guest")
	}
	bound, err := source.AfterOwnerIntegrationPhysicalBound()
	if err != nil {
		t.Fatal(err)
	}
	template, _ := json.Marshal(struct {
		Version         int    `json:"version"`
		Phase           string `json:"phase"`
		MetadataBytes   int    `json:"metadataBytes"`
		DescriptorBytes int    `json:"descriptorBytes"`
	}{1, "template", bound*2 + 2048, bound*2 + 2048})
	original, err := source.ReserveAfterOwnerIntegrationMetadata(template)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	incus.Reconcile.InstanceFound = true
	incus.Instances = map[string]ports.InstanceInfo{"test/yard": incus.Reconcile.Instance}
	resolved, physical, err := runtime.ResolveAfterOwnerIntegrations(ctx, original.Recipe())
	if err != nil {
		t.Fatal("physical resolve", err)
	}
	defer resolved.Close()
	if len(physical) > bound {
		t.Fatal("physical record exceeded original scope bound")
	}
	exportAfterOwnerAdapterPair(t, "materialized-integrations", original, physical, "original-instance", resolved.PreparedIntegrations().Binding())
	if path := os.Getenv("SUBYARD_AFTER_OWNER_MATERIALIZED-INTEGRATIONS_CORPUS"); path != "" {
		d, _, err := decodeMaterializedMetadata(original)
		if err != nil {
			t.Fatal(err)
		}
		classes := map[string]int{}
		for _, fact := range d.Integrations.Plan.Facts {
			prefix, _, _ := strings.Cut(fact.Key, ":")
			classes[prefix]++
		}
		raw, _ := json.Marshal(map[string]any{"fixture": "owning integration source constructor; not global maximum census", "originalFacts": classes, "originalSiblingRoutes": len(d.Integrations.Plan.ProfileRoutes), "originalAllowedHooks": len(d.Integrations.Hooks.AllowedHooks), "originalSourceBlobs": len(original.files), "physicalBytes": len(physical), "declaredPhysicalBound": bound})
		testkit.WriteFile(t, path+".census.json", raw, 0600)
	}

	testkit.WriteFile(t, filepath.Join(root, "original.recipe"), original.Recipe(), 0600)
	testkit.WriteFile(t, filepath.Join(root, "physical.record"), physical, 0600)
	process := exec.Command(os.Args[0], "-test.run=^TestAfterOwnerIntegrationRetainsOriginalPhysicalFacts$")
	process.Env = append(os.Environ(), "SUBYARD_AFTER_OWNER_INTEGRATION_CHILD="+root)
	if output, err := process.CombinedOutput(); err != nil {
		t.Fatalf("fresh process: %v %s", err, output)
	}
	baseline := resolved.PreparedIntegrations().Binding()
	// A new restore must not re-plan the changed pending observation. The fresh
	// native plan below differs; the original fact guard must reject it.
	executor.observed = strings.Repeat("c", 64)
	replayed, err := runtime.BindAfterOwnerIntegrations(ctx, original.Recipe(), physical)
	if err != nil {
		t.Fatal("replay original physical record", err)
	}
	defer replayed.Close()
	if replayed.PreparedIntegrations().Binding() != baseline {
		t.Fatal("replay replaced original physical plan")
	}
	if _, _, err = runtime.ObserveAfterOwnerIntegrations(ctx, replayed); err == nil {
		t.Fatal("changed pending native fact adopted")
	}
	incus.Reconcile.Instance.Status = "Stopped"
	incus.Instances["test/yard"] = incus.Reconcile.Instance
	requests := len(executor.requests)
	stopped, stoppedRecord, err := runtime.ResolveAfterOwnerIntegrations(ctx, original.Recipe())
	if err != nil {
		t.Fatal("stopped resolve", err)
	}
	defer stopped.Close()
	if len(executor.requests) != requests {
		t.Fatal("stopped resolution woke or inspected guest")
	}
	if _, deferred, err := runtime.ObserveAfterOwnerIntegrations(ctx, stopped); err != nil || !deferred || len(stoppedRecord) == 0 {
		t.Fatal("stopped selected view", err)
	}
	incus.Reconcile.Instance.Config["volatile.uuid"] = "foreign-instance"
	incus.Instances["test/yard"] = incus.Reconcile.Instance
	if _, _, err := runtime.ObserveAfterOwnerIntegrations(ctx, replayed); err == nil {
		t.Fatal("foreign UUID admitted")
	}
	incus.Reconcile.Instance.Config["volatile.uuid"] = "original-instance"
	incus.Reconcile.Instance.Status = "Running"
	incus.Instances["test/yard"] = incus.Reconcile.Instance
	testkit.WriteFile(t, filepath.Join(runtime.RepositoryRoot, "config/agents/sample/runtime.sh"), []byte("#!/bin/sh\nexit 91\n"), 0755)
	if p, err := runtime.BindAfterOwnerIntegrations(ctx, original.Recipe(), physical); err == nil {
		p.Close()
		t.Fatal("new handler source B admitted")
	}
}
