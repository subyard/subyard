package amnezia_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestNativePrepareAcceptsHashedIncusContext(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fixture := testkit.TempDir(t)
	// Pending native ownership exposes the down consequence and the conditional
	// guest shutdown without opening any guest or provider configuration.
	testkit.WriteFile(t, filepath.Join(fixture, "incus"), []byte(`#!/bin/sh
[ "$1" = query ] || exit 1
printf '%s\n' '{"type":"virtual-machine","status":"Running","devices":{},"config":{"user.subyard.resource.amnezia-vpn":"v1:pending:0000000000000000000000000000000000000000000000000000000000000001"}}'
`), 0755)
	command := exec.Command("python3", filepath.Join(root, "config/profiles/amnezia/resources/vpn/handler.py"), "down")
	command.Env = append(os.Environ(), "PATH="+fixture+":"+os.Getenv("PATH"),
		"SUBYARD_ENGINE_CONTEXT=1", "SUBYARD_RESOURCE_MODE=prepare", "YARD_NAME=vpn-fixture",
		"EXCLUSIVE_ENVIRONMENT_PROFILE=amnezia", "YARD_KIND=vm",
		"INCUS_PROJECT=subyard-amnezia-bootstrap-5b9fe2b7ea30", "YARD_INSTANCE_NAME=yard-amnezia-bootstrap-5b9fe2b7ea30")
	payload, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("native prepare fixture: %v\n%s", err, payload)
	}
	registry, err := resource.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := domain.NewActionRegistry(registry.ActionDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := registry.PrepareResult(actions, "vpn", "down", payload)
	if err != nil {
		t.Fatalf("CLI prepare parser rejected native assessment: %v", err)
	}
	if _, err := actions.Assess(plan.Assessment.Action, domain.ActionDelta{
		Changed: plan.Assessment.Changed, Consequences: domain.OperationStepConsequences(plan.Steps),
	}); err != nil {
		t.Fatalf("native step projection cannot form an action plan: %v", err)
	}
	if len(plan.Steps) != 2 || !plan.Assessment.Changed {
		t.Fatalf("native shutdown facts were lost: %#v", plan)
	}
	for _, step := range plan.Steps {
		if !strings.Contains(step.Target, "incus:subyard-amnezia-bootstrap-5b9fe2b7ea30/yard-amnezia-bootstrap-5b9fe2b7ea30") ||
			!strings.Contains(step.Consequence, "incus:subyard-amnezia-bootstrap-5b9fe2b7ea30/yard-amnezia-bootstrap-5b9fe2b7ea30") {
			t.Fatalf("native shutdown scope was lost: %#v", step)
		}
	}
}
