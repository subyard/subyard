package releasetransition

import (
	"errors"
	"github.com/Subyard/Subyard/internal/config"
	"strings"
	"testing"
)

func TestSettingsV2LogicalSnapshotsPreserveFingerprintAndMutationContracts(t *testing.T) {
	plan, err := planSyntheticSettingsV2(map[string]string{"yards/hermes/config.env": "YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\n"}, nil, nil)
	if err != nil || len(plan.Files) != 1 {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}
	file := plan.Files[0]
	if file.Inheritance != "4fd17b3d60a7d93eaa14720d236a3f7b34127b4feeb647d355d67d8d9d3e1e18" || file.ExpectedFingerprint != "f2848dc24f51159f44c302ae583a14bdee2333039c86306530b4f1e0e58bd7cb" || file.DesiredFingerprint != "4d78391fbf80f2149a664e609ea89fa5da15f42bd48e4a6b058a21df61e9169b" {
		t.Fatalf("fingerprints changed: %#v", file)
	}
	input := settingsV2YardSnapshot{Yard: "hermes", Nested: config.PersistentFileSnapshot{Exists: true, Content: []byte("YARD_TEMPLATE=e2e-vms\n")}}
	output, _, blocker := planSettingsV2Yard(input, nil, file.Inheritance)
	if blocker != nil || output == nil {
		t.Fatalf("output=%#v blocker=%#v", output, blocker)
	}
	output.Expected.Content[0] = 'X'
	output.Desired[0] = 'Z'
	if string(input.Nested.Content) != "YARD_TEMPLATE=e2e-vms\n" {
		t.Fatal("planner output aliases input")
	}
	// A value change in inherited ownership stays the same identity; removal is
	// late ownership drift and must invalidate a previously bound resource.
	shared := settingsV2InheritedSnapshot{Role: "shared", Label: "overrides/shared/config.env", Snapshot: config.PersistentFileSnapshot{Exists: true, Content: []byte("NESTED_E2E_VMS=0\n")}}
	_, before, _, _ := classifySettingsV2Inheritance([]settingsV2InheritedSnapshot{shared}, nil)
	shared.Snapshot.Content = []byte("NESTED_E2E_VMS=1\n")
	_, same, _, _ := classifySettingsV2Inheritance([]settingsV2InheritedSnapshot{shared}, nil)
	shared.Snapshot.Content = nil
	_, after, _, _ := classifySettingsV2Inheritance([]settingsV2InheritedSnapshot{shared}, nil)
	if before != same || before == after {
		t.Fatal("inherited ownership fingerprint contract changed")
	}
}

func TestSettingsV2AwaitImportUsesOnlyLogicalLayoutFacts(t *testing.T) {
	for _, test := range []struct {
		name                                         string
		expected, nestedLayout, nested, legacy, want bool
	}{
		{"prospective nested absent", true, true, false, false, true},
		{"expected absent", false, true, false, false, false},
		{"legacy expected", true, false, false, false, false},
		{"nested imported", true, true, true, false, false},
		{"legacy present", true, true, false, true, false},
		{"duplicate", true, true, true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed := settingsV2YardSnapshot{Nested: config.PersistentFileSnapshot{Exists: test.nested}, Legacy: config.PersistentFileSnapshot{Exists: test.legacy}}
			if got := settingsV2AwaitSourceImport(config.PersistentFileSnapshot{Exists: test.expected}, test.nestedLayout, observed); got != test.want {
				t.Fatalf("await=%v", got)
			}
		})
	}
}

type failingSettingsV2View struct{}

func (failingSettingsV2View) ListYards() ([]string, error) { return nil, nil }
func (failingSettingsV2View) ReadSnapshot(string) (config.PersistentFileSnapshot, error) {
	return config.PersistentFileSnapshot{}, errors.New("synthetic read failure")
}
func TestSettingsV2InjectedObservationDoesNotReadPhysicalRoot(t *testing.T) {
	plan, err := newTestVMSettingsV2Capability("logical-config-home", nil, failingSettingsV2View{}).Inspect()
	if err != nil || len(plan.Blockers) != 1 || plan.Blockers[0].Resource != "settings.inherited" {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}
}

type orderedSettingsV2View struct{ hostRead bool }

func (*orderedSettingsV2View) ListYards() ([]string, error) { return nil, nil }
func (view *orderedSettingsV2View) ReadSnapshot(label string) (config.PersistentFileSnapshot, error) {
	if strings.Contains(label, "overrides/shared/") {
		return config.PersistentFileSnapshot{Exists: true, Content: []byte("NESTED_E2E_VMS='unterminated\n")}, nil
	}
	view.hostRead = true
	return config.PersistentFileSnapshot{}, errors.New("later unsafe source")
}
func TestSettingsV2PreservesInheritedFailureOrder(t *testing.T) {
	view := &orderedSettingsV2View{}
	plan, err := newTestVMSettingsV2Capability("logical-config-home", nil, view).Inspect()
	if err != nil || len(plan.Blockers) != 1 || view.hostRead || !strings.Contains(plan.Blockers[0].Message, "overrides/shared/config.env:1") {
		t.Fatalf("source precedence changed: plan=%#v err=%v hostRead=%v", plan, err, view.hostRead)
	}
}
