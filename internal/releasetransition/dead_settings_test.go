package releasetransition

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestDeadSettingsPreserveMigrationInheritanceAcrossCleanup(t *testing.T) {
	root, home := testkit.TempDir(t), testkit.TempDir(t)
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte(content), 0600)
	}
	write(filepath.Join(root, "config/agents/sample/integration.json"), `{"schema_version":2,"command":"sample","artifacts":[{"id":"config","kind":"config","source":"settings.json","destination":".sample/settings.json","format":"destination"}]}`)
	write(filepath.Join(root, "config/agents/sample/settings.json"), "{}")
	write(filepath.Join(home, "overrides/host/agents/sample/settings.json"), "{}")
	path := filepath.Join(home, "config.env")
	write(path, "AGENT_sample_CONFIG=\"$SUBYARD_CONFIG_DIR/../private/agents/sample/settings.json\"\nDEV_UID=1000\n")
	view := posixV2SettingsSnapshotView{configHome: home}
	before, err := view.ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := effectiveSettingsSnapshot(home, filepath.Join(root, "config"), path, before, view)
	if err != nil || string(effective.Content) != "DEV_UID=1000\n" {
		t.Fatalf("effective=%q err=%v", effective.Content, err)
	}
	capability := newTestVMSettingsV2Capability(home, nil)
	capability.candidateConfigDir = filepath.Join(root, "config")
	_, inheritance, blocker, err := capability.observeInheritedSettings()
	if err != nil || blocker != nil {
		t.Fatalf("inheritance=%v blocker=%+v err=%v", inheritance, blocker, err)
	}
	transition := &V2Transition{options: V2Options{ConfigHome: home, CandidateConfigDir: filepath.Join(root, "config")}}
	_, _, registeredBefore, err := transition.registeredSettingsSnapshot("default", nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := config.LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := catalog.PlanDeadAssignments(home, filepath.Join(root, "config"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(home); err != nil {
		t.Fatal(err)
	}
	after, err := view.ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(effective.Content, after.Content) {
		t.Fatal("effective input changed after cleanup")
	}
	_, inheritanceAfter, blocker, err := capability.observeInheritedSettings()
	if err != nil || blocker != nil || inheritanceAfter != inheritance {
		t.Fatalf("inheritance changed: %v -> %v blocker=%+v err=%v", inheritance, inheritanceAfter, blocker, err)
	}
	_, _, registeredAfter, err := transition.registeredSettingsSnapshot("default", nil)
	if err != nil || registeredAfter != registeredBefore {
		t.Fatalf("registered inheritance changed: %v -> %v err=%v", registeredBefore, registeredAfter, err)
	}
}

func TestSettingsInheritanceCollectsBothLiteralRefusals(t *testing.T) {
	home := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(home, "overrides/shared"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, "config.env"), []byte("DEV_UID=$UID\n"), 0600)
	testkit.WriteFile(t, filepath.Join(home, "overrides/shared/config.env"), []byte("GIT_USER_NAME=$NAME\n"), 0600)
	capability := newTestVMSettingsV2Capability(home, nil)
	_, _, blocker, err := capability.observeInheritedSettings()
	if err != nil || blocker == nil || !strings.Contains(blocker.Message, "DEV_UID") || !strings.Contains(blocker.Message, "GIT_USER_NAME") {
		t.Fatalf("blocker=%+v err=%v", blocker, err)
	}
}
