package configsync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

func TestLegacySSHSyncPreservesPersistentPortsAndUnrelatedLocalRecords(t *testing.T) {
	f := newSyncFixture(t, "owner-a")
	f.writeSource("hosts/owner-a/config.env", "# source host\nSSH_PORT=2500\nDEV_SUDO=0\n")
	f.writeSource("hosts/owner-a/yards/demo/config.env", "SSH_PORT=2501\n")
	f.writeSource("hosts/owner-b/config.env", "SSH_PORT=2600\n")
	f.commit("legacy source")
	writeSyncTestFile(t, filepath.Join(f.configHome, gitSettingsPath("config.env")), "SSH_PORT=2200\n", 0o600)
	writeSyncTestFile(t, filepath.Join(f.configHome, gitSettingsPath("yards/demo/config.env")), "SSH_PORT=2201\n", 0o600)
	local := filepath.Join(f.configHome, "yards/demo/config.env")
	writeSyncTestFile(t, local, "# local choice\nDEV_SUDO=0\n", 0o640)
	options := f.options(true)
	options.Environment["SSH_PORT"] = "2999" // Temporary inputs never become pins.
	plan, err := BuildPlan(options)
	if err != nil {
		t.Fatal(err)
	}
	contexts, err := plan.CandidateConfigs()
	if err != nil || len(contexts) != 2 || contexts[0].Context.SSHPort != 2200 || contexts[1].Context.SSHPort != 2201 {
		t.Fatalf("projected persistent ports changed: %v", err)
	}
	assertLocalPinSteps := func(fresh Plan, decision domain.StepDecision) {
		t.Helper()
		steps := fresh.OperationSteps(plan)
		for path, file := range plan.localDesired {
			found := false
			for _, step := range steps {
				if step.Target == "managed setting "+path {
					found = true
					if step.Desired != "fingerprint:"+file.Digest+fmt.Sprintf("; mode:%o", file.Mode) || step.Decision != decision {
						t.Fatalf("prepared pin differs from published candidate: %#v", step)
					}
				}
			}
			if !found {
				t.Fatalf("prepared pin omitted: %s", path)
			}
		}
	}
	if len(plan.localDesired) != 2 {
		t.Fatal("fixture did not exercise both local pin targets")
	}
	assertLocalPinSteps(plan, domain.StepApply)
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	assertSyncTestFile(t, local, "# local choice\nDEV_SUDO=0\nSSH_PORT='2201'\n", 0o640)
	assertSyncTestFile(t, filepath.Join(f.configHome, gitSettingsPath("config.env")), "# source host\nDEV_SUDO=0\n", 0o600)
	assertSyncTestFile(t, filepath.Join(f.configHome, gitSettingsPath("yards/demo/config.env")), "", 0o600)
	// Source publication is a separate explicit prepared Git operation.
	assertSyncTestFile(t, filepath.Join(f.source, "hosts/owner-a/config.env"), "# source host\nSSH_PORT=2500\nDEV_SUDO=0\n", 0o600)
	assertSyncTestFile(t, filepath.Join(f.source, "hosts/owner-b/config.env"), "SSH_PORT=2600\n", 0o600)
	manifest, err := readManifest(f.configHome)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range manifest.Files {
		if !strings.HasPrefix(file.Path, config.GitSettingsRelativePath+"/") {
			t.Fatal("local pin became Git-owned")
		}
	}
	fresh, err := BuildPlan(options)
	if err != nil || fresh.NeedsApply() {
		t.Fatalf("migration did not converge: %v", err)
	}
	assertLocalPinSteps(fresh, domain.StepSkip)
}

func TestLegacySSHSyncBindsLocalPinAndPreservesWinningOverride(t *testing.T) {
	f := newSyncFixture(t, "owner-a")
	f.writeSource("hosts/owner-a/config.env", "SSH_PORT=2500\n")
	f.commit("legacy source")
	local := filepath.Join(f.configHome, "config.env")
	writeSyncTestFile(t, local, "# winning local value\nSSH_PORT=2200\n", 0o640)
	plan, err := BuildPlan(f.options(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.localDesired) != 0 {
		t.Fatal("winning local assignment was rewritten")
	}
	writeSyncTestFile(t, local, "SSH_PORT=2201\n", 0o640)
	if err := Apply(plan); !errors.Is(err, ErrPlanStale) {
		t.Fatalf("stale override accepted: %v", err)
	}
}

func TestLegacySSHSyncRejectsDynamicSourceAndPortCollision(t *testing.T) {
	for _, value := range []string{"${PRIVATE_PORT:-2500}", "$(printf 2500)"} {
		f := newSyncFixture(t, "owner-a")
		f.writeSource("hosts/owner-a/config.env", "SSH_PORT="+value+"\n")
		f.commit("dynamic legacy source")
		if _, err := BuildPlan(f.options(false)); err == nil || strings.Contains(err.Error(), "PRIVATE_PORT") {
			t.Fatal("dynamic assignment admitted or disclosed")
		}
	}
	f := newSyncFixture(t, "owner-a")
	f.writeSource("hosts/owner-a/config.env", "SSH_PORT=2500\n")
	f.writeSource("hosts/owner-a/yards/demo/config.env", "SSH_PORT=2501\n")
	f.commit("legacy source")
	writeSyncTestFile(t, filepath.Join(f.configHome, "config.env"), "SSH_PORT=2200\n", 0o600)
	writeSyncTestFile(t, filepath.Join(f.configHome, "yards/demo/config.env"), "SSH_PORT=2200\n", 0o600)
	if _, err := BuildPlan(f.options(false)); err == nil || !strings.Contains(err.Error(), "SSH port") {
		t.Fatalf("persistent collision admitted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.configHome, gitSettingsPath("config.env"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed planning changed cache")
	}
}
