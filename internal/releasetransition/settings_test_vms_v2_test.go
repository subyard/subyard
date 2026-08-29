package releasetransition

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
)

func TestTestVMSettingsV2PlansAggregateCanonicalizeAndReset(t *testing.T) {
	configHome := settingsV2Fixture(t, map[string]string{
		"yards/hermes/config.env": "# retained\nYARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\nSSH_PORT=2224\n",
	})
	plan, err := newTestVMSettingsV2Capability(configHome, nil).Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) != 0 || len(plan.Files) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	file := plan.Files[0]
	if file.Yard != "hermes" || file.Decision != DecisionReset ||
		!file.Expected.Exists || file.ExpectedFingerprint == file.DesiredFingerprint {
		t.Fatalf("file plan = %#v", file)
	}
	desired := string(file.Desired)
	if !strings.Contains(desired, "# retained\n") ||
		!strings.Contains(desired, "YARD_TEMPLATE='test-vms'\n") ||
		!strings.Contains(desired, "SSH_PORT=2224\n") ||
		strings.Contains(desired, "NESTED_E2E_VMS") {
		t.Fatalf("desired settings = %q", desired)
	}
	assertSettingsV2Decision(t, plan.Decisions, "hermes", "YARD_TEMPLATE", DecisionCanonicalize, "test-vms")
	assertSettingsV2Decision(t, plan.Decisions, "hermes", "NESTED_E2E_VMS", DecisionReset, "unset")
	actual, err := os.ReadFile(file.Path)
	if err != nil || !bytes.Equal(actual, file.Expected.Content) {
		t.Fatalf("Inspect changed source settings: %q, err=%v", actual, err)
	}
}

func TestTestVMSettingsV2PreservesCurrentAndUnrelatedSettings(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "current explicit", content: "YARD_TEMPLATE=test-vms\nNESTED_E2E_VMS=1\n"},
		{name: "current inherited profile default", content: "YARD_TEMPLATE=test-vms\n"},
		{name: "unrelated yard", content: "SSH_PORT=2224\nNESTED_E2E_VMS=0\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configHome := settingsV2Fixture(t, map[string]string{
				"yards/hermes/config.env": test.content,
			})
			plan, err := newTestVMSettingsV2Capability(configHome, nil).Inspect()
			if err != nil || len(plan.Blockers) != 0 || len(plan.Files) != 0 {
				t.Fatalf("plan = %#v, err=%v", plan, err)
			}
		})
	}
}

func TestTestVMSettingsV2ResetsCurrentProfileOverride(t *testing.T) {
	configHome := settingsV2Fixture(t, map[string]string{
		"yards/hermes.env": "YARD_TEMPLATE=test-vms\nNESTED_E2E_VMS=0\n",
	})
	plan, err := newTestVMSettingsV2Capability(configHome, nil).Inspect()
	if err != nil || len(plan.Blockers) != 0 || len(plan.Files) != 1 {
		t.Fatalf("plan = %#v, err=%v", plan, err)
	}
	if plan.Files[0].Decision != DecisionReset ||
		strings.Contains(string(plan.Files[0].Desired), "NESTED_E2E_VMS") {
		t.Fatalf("reset file = %#v", plan.Files[0])
	}
}

func TestTestVMSettingsV2BlocksUnknownDuplicateAndDynamicValues(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "unknown template", content: "YARD_TEMPLATE=private-should-not-leak\n"},
		{name: "duplicate template", content: "YARD_TEMPLATE=e2e-vms\nYARD_TEMPLATE=test-vms\n"},
		{name: "dynamic template", content: "PROFILE=e2e-vms\nYARD_TEMPLATE=$PROFILE\n"},
		{name: "unknown nested value", content: "YARD_TEMPLATE=test-vms\nNESTED_E2E_VMS=private-should-not-leak\n"},
		{name: "implicit nested value", content: "YARD_TEMPLATE=test-vms\n: ${NESTED_E2E_VMS:=0}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configHome := settingsV2Fixture(t, map[string]string{
				"yards/hermes/config.env": test.content,
			})
			plan, err := newTestVMSettingsV2Capability(configHome, nil).Inspect()
			if err != nil || len(plan.Blockers) != 1 || len(plan.Files) != 0 {
				t.Fatalf("plan = %#v, err=%v", plan, err)
			}
			public, err := json.Marshal(struct {
				Decisions []RedactedDecision `json:"decisions"`
				Blockers  []Blocker          `json:"blockers"`
			}{plan.Decisions, plan.Blockers})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(public), "private-should-not-leak") ||
				strings.Contains(string(public), "$PROFILE") {
				t.Fatalf("public plan leaked a raw value: %s", public)
			}
		})
	}
}

func TestTestVMSettingsV2BlocksAmbiguousOwnership(t *testing.T) {
	t.Run("nested and legacy registration", func(t *testing.T) {
		configHome := settingsV2Fixture(t, map[string]string{
			"yards/hermes/config.env": "YARD_TEMPLATE=e2e-vms\n",
			"yards/hermes.env":        "YARD_TEMPLATE=e2e-vms\n",
		})
		plan, err := newTestVMSettingsV2Capability(configHome, nil).Inspect()
		if err != nil || len(plan.Blockers) != 1 || len(plan.Files) != 0 {
			t.Fatalf("plan = %#v, err=%v", plan, err)
		}
	})

	for _, test := range []struct {
		name      string
		global    map[string]string
		inherited []string
	}{
		{name: "shared", global: map[string]string{
			"overrides/shared/config.env": "NESTED_E2E_VMS=0\n",
		}},
		{name: "host", global: map[string]string{"config.env": "NESTED_E2E_VMS=0\n"}},
		{name: "command", inherited: []string{"NESTED_E2E_VMS"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := map[string]string{
				"yards/hermes/config.env": "YARD_TEMPLATE=test-vms\nNESTED_E2E_VMS=0\n",
			}
			for path, content := range test.global {
				files[path] = content
			}
			configHome := settingsV2Fixture(t, files)
			plan, err := newTestVMSettingsV2Capability(configHome, test.inherited).Inspect()
			if err != nil || len(plan.Blockers) != 1 || len(plan.Files) != 0 {
				t.Fatalf("plan = %#v, err=%v", plan, err)
			}
		})
	}
}

func TestTestVMSettingsV2BlocksInheritedTemplateWithoutDirectAssignment(t *testing.T) {
	configHome := settingsV2Fixture(t, map[string]string{
		"yards/hermes/config.env": "NESTED_E2E_VMS=0\n",
	})
	plan, err := newTestVMSettingsV2Capability(
		configHome, []string{"YARD_TEMPLATE"},
	).Inspect()
	if err != nil || len(plan.Blockers) != 1 || len(plan.Files) != 0 {
		t.Fatalf("inherited template plan = %#v, err=%v", plan, err)
	}
}

func TestTestVMSettingsV2BlocksCatalogDriftBeforeReadingValues(t *testing.T) {
	configHome := settingsV2Fixture(t, map[string]string{
		"yards/hermes/config.env": "YARD_TEMPLATE=e2e-vms\n",
	})
	capability := newTestVMSettingsV2Capability(configHome, nil)
	capability.lookupSetting = func(name string) (config.SettingDefinition, bool) {
		definition, ok := config.LookupSetting(name)
		if name == "NESTED_E2E_VMS" {
			definition.Sensitive = true
		}
		return definition, ok
	}
	plan, err := capability.Inspect()
	if err != nil || len(plan.Blockers) != 1 || len(plan.Decisions) != 0 || len(plan.Files) != 0 {
		t.Fatalf("catalog-drift plan = %#v, err=%v", plan, err)
	}
	if plan.Blockers[0].Code != CodeUnsupportedKind {
		t.Fatalf("catalog-drift blocker = %#v", plan.Blockers[0])
	}
}

func TestTestVMSettingsV2BlocksUnsafePersistentResources(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			configHome := settingsV2Fixture(t, nil)
			yards := filepath.Join(configHome, "yards")
			foreign := filepath.Join(configHome, "foreign.env")
			if err := os.WriteFile(foreign, []byte("YARD_TEMPLATE=e2e-vms\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(yards, "hermes.env")
			var err error
			if kind == "symlink" {
				err = os.Symlink(foreign, target)
			} else {
				err = os.Link(foreign, target)
			}
			if err != nil {
				t.Fatal(err)
			}
			plan, err := newTestVMSettingsV2Capability(configHome, nil).Inspect()
			if err != nil || len(plan.Blockers) != 1 || len(plan.Files) != 0 {
				t.Fatalf("unsafe plan = %#v, err=%v", plan, err)
			}
		})
	}
}

func TestTestVMSettingsV2BlocksUnsafeInheritedResource(t *testing.T) {
	configHome := settingsV2Fixture(t, map[string]string{
		"yards/hermes/config.env": "YARD_TEMPLATE=test-vms\nNESTED_E2E_VMS=0\n",
		"foreign.env":             "NESTED_E2E_VMS=0\n",
	})
	if err := os.Symlink(
		filepath.Join(configHome, "foreign.env"), filepath.Join(configHome, "config.env"),
	); err != nil {
		t.Fatal(err)
	}
	plan, err := newTestVMSettingsV2Capability(configHome, nil).Inspect()
	if err != nil || len(plan.Blockers) != 1 || len(plan.Files) != 0 {
		t.Fatalf("unsafe inherited plan = %#v, err=%v", plan, err)
	}
	if validationErr := plan.Blockers[0].Validate(); validationErr != nil {
		t.Fatalf("unsafe inherited blocker is not public-safe: %v", validationErr)
	}
}

func TestTestVMSettingsV2InspectCreatesNothing(t *testing.T) {
	configHome := t.TempDir()
	if err := os.Chmod(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(configHome)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := newTestVMSettingsV2Capability(configHome, nil).Inspect()
	if err != nil || len(plan.Files) != 0 || len(plan.Blockers) != 0 {
		t.Fatalf("empty plan = %#v, err=%v", plan, err)
	}
	after, err := os.Lstat(configHome)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatal("Inspect changed config-home identity or mode")
	}
	entries, err := os.ReadDir(configHome)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Inspect created metadata: entries=%#v err=%v", entries, err)
	}
}

func TestTestVMSettingsV2ResourceBoundFitsPublicPlanBound(t *testing.T) {
	files := make(map[string]string, maximumSettingsV2Yards)
	for index := 0; index < maximumSettingsV2Yards; index++ {
		files[fmt.Sprintf("yards/yard-%03d/config.env", index)] =
			"YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\n"
	}
	plan, err := newTestVMSettingsV2Capability(settingsV2Fixture(t, files), nil).Inspect()
	if err != nil || len(plan.Files) != maximumSettingsV2Yards ||
		len(plan.Decisions) != MaxPlanItems {
		t.Fatalf("bounded plan: files=%d decisions=%d blockers=%d err=%v",
			len(plan.Files), len(plan.Decisions), len(plan.Blockers), err)
	}
	for _, decision := range plan.Decisions {
		if err := decision.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func settingsV2Fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	configHome := t.TempDir()
	if err := os.Chmod(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(configHome, "yards"), 0o700); err != nil {
		t.Fatal(err)
	}
	for relative, content := range files {
		path := filepath.Join(configHome, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return configHome
}

func assertSettingsV2Decision(
	t *testing.T,
	decisions []RedactedDecision,
	yard string,
	setting string,
	decision Decision,
	result string,
) {
	t.Helper()
	for _, actual := range decisions {
		if actual.Scope == "yard."+yard && actual.Resource == "setting."+setting {
			if actual.Decision != decision || actual.Result != result {
				t.Fatalf("decision = %#v", actual)
			}
			return
		}
	}
	t.Fatalf("missing decision for %s/%s: %#v", yard, setting, decisions)
}

func TestTestVMSettingsV2RejectsMalformedFileWithoutRawDiagnostic(t *testing.T) {
	configHome := settingsV2Fixture(t, map[string]string{
		"yards/hermes/config.env": "YARD_TEMPLATE='private-should-not-leak\n",
	})
	plan, err := newTestVMSettingsV2Capability(configHome, nil).Inspect()
	if err != nil || len(plan.Blockers) != 1 {
		t.Fatalf("malformed plan = %#v, err=%v", plan, err)
	}
	payload, marshalErr := json.Marshal(plan.Blockers)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(payload), "private-should-not-leak") {
		t.Fatalf("malformed blocker leaked value: %s", payload)
	}
}

func TestTestVMSettingsV2InspectRejectsMissingConfigHome(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	_, err := newTestVMSettingsV2Capability(missing, nil).Inspect()
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing config home error = %v", err)
	}
}
