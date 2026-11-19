package application

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestConformanceGoldenJSONAndText(t *testing.T) {
	report, err := InspectConformance([]InspectionSection{
		{ID: "config"},
		{ID: "release", Items: []domain.Inspected{{Key: "activation.sample.pending-configuration", State: domain.InspectedPending, Resource: "activation.sample.pending-configuration", Message: "sample settings wait for yard init", Next: "yard -Y sample init"}}},
		{ID: "host", Items: []domain.Inspected{{Key: "preflight.kvm", State: domain.InspectedNotice, Resource: "KVM device", Message: "/dev/kvm missing", Next: "yard check --host"}}},
		{ID: "state", Items: []domain.Inspected{{Key: "registrations.sample", State: domain.InspectedDrift, Resource: "sample", Installed: "flat and nested", Desired: "nested", Message: "flat and nested registrations coexist", Next: "yard config repair-registration sample"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile(filepath.Join("testdata", "conformance.golden.json"))
	if err != nil || string(encoded)+"\n" != string(expected) {
		t.Fatalf("JSON changed (%v):\n%s", err, encoded)
	}
	var output bytes.Buffer
	if err := report.WriteText(&output); err != nil {
		t.Fatal(err)
	}
	want := `config: ok
release: ok
  [pending] release.activation.sample.pending-configuration: sample settings wait for yard init
    next: yard -Y sample init
host: ok
  [notice] host.preflight.kvm: /dev/kvm missing
    next: yard check --host
state: drift
  [drift] state.registrations.sample: flat and nested registrations coexist
    next: yard config repair-registration sample
DRIFT; 1 drift, 0 blockers, 1 pending, 0 not inspected, 1 notices
` + ConformanceClosing + "\n"
	if report.ExitCode() != 1 || output.String() != want {
		t.Fatalf("code=%d text:\n%s", report.ExitCode(), output.String())
	}
}

func TestConformanceMechanicalStatesAndProgramErrors(t *testing.T) {
	for state, want := range map[domain.InspectionState]int{
		domain.InspectedDrift: 1, domain.InspectedUnknown: 1, domain.InspectedBlocked: 1,
		domain.InspectedPending: 0, domain.InspectedDeferred: 0, domain.InspectedNotice: 0,
	} {
		report, err := InspectConformance([]InspectionSection{{ID: "host", Items: []domain.Inspected{{Key: "resource.predicate", State: state, Message: "observed", Next: "yard init"}}}})
		if err != nil || report.ExitCode() != want || report.Sections[0].Findings[0].ID != "host.resource.predicate" {
			t.Fatalf("state=%s report=%v err=%v", state, report, err)
		}
	}
	for _, sections := range [][]InspectionSection{
		{{ID: "host", Items: []domain.Inspected{{State: domain.InspectedUnknown, Message: "missing key"}}}},
		{{ID: "host", Items: []domain.Inspected{{Key: "Upper", State: domain.InspectedUnknown, Message: "invalid key"}}}},
		{{ID: "host", Items: []domain.Inspected{{Key: "a b", State: domain.InspectedUnknown, Message: "invalid key"}}}},
		{{ID: "host", Items: []domain.Inspected{{Key: "drift", State: domain.InspectedDrift, Message: "missing next"}}}},
		{{ID: "host", Items: []domain.Inspected{{Key: "empty", State: domain.InspectedNotice}}}},
		{{ID: "host", Items: []domain.Inspected{{Key: "state", State: "warning", Message: "unknown state"}}}},
		{{ID: "host", Items: []domain.Inspected{{Key: "same", State: domain.InspectedNotice, Message: "one"}, {Key: "same", State: domain.InspectedNotice, Message: "two"}}}},
		{{ID: "host"}, {ID: "host"}},
	} {
		if _, err := InspectConformance(sections); err == nil {
			t.Fatalf("programming error accepted: %v", sections)
		}
	}
}

func TestConformanceKeepsEverySectionAndFindingOrder(t *testing.T) {
	report, err := InspectConformance([]InspectionSection{
		{ID: "config", Items: []domain.Inspected{{Key: "root.read", State: domain.InspectedUnknown, Message: "unavailable"}}},
		{ID: "release", Items: []domain.Inspected{{Key: "config", State: domain.InspectedDeferred, Message: "deferred by config.root.read"}}},
		{ID: "host", Items: []domain.Inspected{{Key: "b", State: domain.InspectedNotice, Message: "first"}, {Key: "a", State: domain.InspectedNotice, Message: "second"}}},
		{ID: "state"},
	})
	if err != nil || len(report.Sections) != 4 || report.Status != "blocked" || report.Sections[1].Status != "skipped" ||
		report.Sections[2].Findings[0].ID != "host.b" || report.Sections[2].Findings[1].ID != "host.a" {
		t.Fatalf("report=%v err=%v", report, err)
	}
}
