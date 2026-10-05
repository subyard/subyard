package configsync

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

func TestPreparedConfigSyncAllowsNativePerTargetConvergence(t *testing.T) {
	fixture := newSyncFixture(t, "owner-a")
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2290\n")
	fixture.writeSource("hosts/owner-a/yards/demo/config.env", "SSH_PORT=2291\n")
	fixture.commit("initial")
	initial, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(initial); err != nil {
		t.Fatal(err)
	}
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2390\n")
	fixture.writeSource("hosts/owner-a/yards/demo/config.env", "SSH_PORT=2391\n")
	fixture.commit("desired")
	approved, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	writeSyncTestFile(t, filepath.Join(fixture.configHome, config.GitSettingsRelativePath, "config.env"), "SSH_PORT=2390\n", 0o600)
	fresh, err := approved.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if err := domain.CheckOperationSteps(approved.OperationSteps(approved), fresh.OperationSteps(approved)); err != nil {
		t.Fatal(err)
	}
	steps := fresh.OperationSteps(approved)
	if steps[0].Decision != domain.StepSkip || steps[1].Decision != domain.StepApply {
		t.Fatalf("partial convergence not preserved: %#v", steps)
	}
	if err := Apply(fresh); err != nil {
		t.Fatal(err)
	}
	if err := fresh.VerifyPublished(); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedConfigSyncRejectsNoOpExpansionAndSourceDrift(t *testing.T) {
	for _, kind := range []string{"managed target", "source", "local input"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newSyncFixture(t, "owner-a")
			fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2290\n")
			fixture.commit("initial")
			initial, err := BuildPlan(fixture.options(false))
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(initial); err != nil {
				t.Fatal(err)
			}
			approved, err := BuildPlan(fixture.options(false))
			if err != nil {
				t.Fatal(err)
			}
			if approved.NeedsApply() {
				t.Fatal("expected native no-op")
			}
			switch kind {
			case "managed target":
				writeSyncTestFile(t, filepath.Join(fixture.configHome, config.GitSettingsRelativePath, "config.env"), "SSH_PORT=2390\n", 0o600)
			case "source":
				fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2390\n")
				fixture.commit("concurrent")
			case "local input":
				writeSyncTestFile(t, filepath.Join(fixture.configHome, "config.env"), "SSH_PORT=2390\n", 0o600)
			}
			if _, err := approved.Refresh(); !errors.Is(err, ErrPlanStale) {
				t.Fatalf("drift did not invalidate native no-op: %v", err)
			}
		})
	}
}

func TestPreparedConfigSyncCandidateContextsUseUnpublishedDesiredInputs(t *testing.T) {
	fixture := newSyncFixture(t, "owner-a")
	fixture.writeSource("hosts/owner-a/config.env", "SSH_PORT=2390\n")
	fixture.writeSource("hosts/owner-a/yards/demo/config.env", "SSH_PORT=2391\n")
	fixture.commit("candidate")
	plan, err := BuildPlan(fixture.options(false))
	if err != nil {
		t.Fatal(err)
	}
	contexts, err := plan.CandidateConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(contexts) != 2 || contexts[0].Environment["SSH_PORT"] != "2390" || contexts[1].Environment["SSH_PORT"] != "2391" {
		t.Fatalf("candidate contexts did not use source inputs")
	}
	if err := plan.VerifyPublished(); err == nil {
		t.Fatal("unpublished candidate incorrectly verified as converged")
	}
}
