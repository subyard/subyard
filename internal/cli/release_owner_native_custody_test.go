package cli

import (
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releaseowner"
)

func TestNativeCustodyRebindPreservesOriginalScopeAndBaseline(t *testing.T) {
	a, b, next := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	original := releaseowner.Target{Class: "profile-runtime", Owner: "synthetic-runtime", Yard: "default", Project: "project", Instance: "yard", InstanceUUID: "instance-uuid", State: "stale", Actual: a, Desired: b, Source: a, ReportedActual: a, ReportedDesired: b}
	original.Before = `{"version":1,"expected":"0","generation":"` + a + `"}`
	current := original
	current.Before = `{"version":1,"expected":"` + a + `","generation":"` + a + `"}`
	component := &releaseOwnerComponents{}
	if !component.rebindProfileCustody(current, original, "") {
		t.Fatal("original idempotent generation refused")
	}
	for _, field := range []string{"owner", "class", "yard", "project", "instance", "uuid", "source", "desired", "actual"} {
		changed := current
		switch field {
		case "owner":
			changed.Owner = "other-runtime"
		case "class":
			changed.Class = "other"
		case "yard":
			changed.Yard = "other"
		case "project":
			changed.Project = "other"
		case "instance":
			changed.Instance = "other"
		case "uuid":
			changed.InstanceUUID = "other"
		case "source":
			changed.Source = b
		case "desired":
			changed.Desired = a
		case "actual":
			changed.Actual = b
		}
		if component.rebindProfileCustody(changed, original, "") {
			t.Fatalf("changed %s recaptured", field)
		}
	}
	// Rust may persist a fresh C/G after a validated successor fence. It never
	// restores the old generation, and the component keeps the original scope.
	replacement := original
	replacement.Before = `{"version":1,"expected":"` + b + `","generation":"` + next + `"}`
	current.Before = `{"version":1,"expected":"` + b + `","generation":"` + a + `"}`
	if !component.rebindProfileCustody(current, replacement, "") {
		t.Fatal("bound fresh recovery generation refused")
	}
	current.Before = `{"version":1,"expected":"` + next + `","generation":"` + a + `"}`
	if !component.rebindProfileCustody(current, replacement, "") {
		t.Fatal("idempotent fresh recovery generation refused")
	}
}
