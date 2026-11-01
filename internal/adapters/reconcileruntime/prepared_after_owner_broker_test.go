package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestAfterOwnerBrokerRetainsSourceAndMeasuredNative(t *testing.T) {
	runtime := brokerRecipeFixture(t)
	runtime.Yard.NestedE2EVMs = true
	incus := runtime.Incus.(*testkit.Incus)
	incus.Err = ports.ErrInstanceNotFound
	ctx := context.Background()
	path := filepath.Join(strings.Split(os.Getenv("PATH"), ":")[0], "incus")
	originalCommand, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte("#!/bin/sh\nexit 91\n"), 0700)
	p, err := runtime.PrepareAfterOwnerBroker(ctx, []byte(`{"version":1,"phase":"template","metadataBytes":4096,"descriptorBytes":4096}`))
	if err != nil {
		t.Fatal("source capture queried physical broker", err)
	}
	defer p.Close()
	if _, err := runtime.PrepareTestVMBrokerRuntime(ctx); !errors.Is(err, ports.ErrInstanceNotFound) {
		t.Fatal("ordinary preparation stopped checking the actual instance", err)
	}
	raw := p.Recipe()
	source, err := runtime.BindAfterOwnerBroker(ctx, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if source.PreparedBroker().power != application.InitialPower(runtime.Yard) {
		t.Fatal("source power differs from approved canonical init")
	}
	source.Close()
	testkit.WriteFile(t, path, originalCommand, 0700)
	incus.Err = nil
	resolved, physical, err := runtime.ResolveAfterOwnerBroker(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer resolved.Close()
	instance := incus.Instances[runtime.Yard.IncusProject+"/"+runtime.Yard.YardInstanceName]
	instance.Config["user.subyard.desired_power"] = "stopped"
	if err := runtime.ApplyPreparedTestVMBrokerRuntime(ctx, resolved.PreparedBroker()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("changed actual power intent admitted", err)
	}
	instance.Config["user.subyard.desired_power"] = "running"
	exportAfterOwnerAdapterPair(t, "broker", p, physical, "original-created-uuid", "")
	var value afterOwnerBrokerPhysical
	if json.Unmarshal(physical, &value) != nil || value.Native == strings.Repeat("0", 64) || len(physical) > AfterOwnerBrokerPhysicalBound() {
		t.Fatal("missing bounded physical record")
	}
	if !bytes.Equal(raw, resolved.Recipe()) {
		t.Fatal("resolution replaced original recipe source")
	}
	// A new restore consumes the recorded native observation, rather than
	// adopting the replacement that a new PrepareApply would measure.
	testkit.WriteFile(t, path, bytes.ReplaceAll(originalCommand, []byte("original-uuid"), []byte("foreign-uuid")), 0700)
	restored, err := runtime.BindAfterOwnerBroker(ctx, raw, physical)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.PreparedBroker().Binding() != value.Binding {
		t.Fatal("physical binding changed")
	}
	if err = restored.broker.backend.ApplyPrepared(ctx, restored.broker.apply); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("replacement native state admitted", err)
	}
	testkit.WriteFile(t, filepath.Join(runtime.RepositoryRoot, "scripts/e2e-lab/provision.sh"), []byte("foreign source"), 0700)
	if changed, err := runtime.BindAfterOwnerBroker(ctx, raw, physical); err == nil {
		changed.Close()
		t.Fatal("source drift admitted")
	}
}
