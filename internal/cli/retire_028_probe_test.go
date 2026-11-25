package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// A 0.33.0 release shipping this repository's registry and rollback
// declaration rolls back to a release with the published 0.32.0 declaration and
// refuses one with the published 0.31.0 declaration. Both published releases
// ship the same registry bytes as v0.30.0.
func TestShippedRetirementRegistryAdmitsOnlyThe032Reader(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	read := func(path string) []byte {
		t.Helper()
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	shippedRegistry := read(filepath.Join(repositoryRoot, "config", "release-transition.json"))
	shippedDeclaration := read(filepath.Join(repositoryRoot, "config", "release-rollback.json"))
	publishedRegistry := read(filepath.Join("..", "releasetransition", "testdata", "v0.30.0-release-transition.json"))
	for _, test := range []struct {
		version  string
		admitted bool
	}{
		{version: "0.31.0"},
		{version: "0.32.0", admitted: true},
	} {
		t.Run(test.version, func(t *testing.T) {
			root := t.TempDir()
			runtimeRoot, configHome := filepath.Join(root, "runtime"), filepath.Join(root, "config")
			active, target := "0.33.0-cccccccccccc", test.version+"-aaaaaaaaaaaa"
			writeSealedEquivalentRelease(t, runtimeRoot, active, "0.33.0", map[string][]byte{
				"config/release-transition.json": shippedRegistry, "config/release-rollback.json": shippedDeclaration})
			digest := writeSealedEquivalentRelease(t, runtimeRoot, target, test.version, map[string][]byte{
				"config/release-transition.json": publishedRegistry,
				"config/release-rollback.json":   read(filepath.Join("testdata", "release-rollback-v"+test.version+".json"))})
			for link, release := range map[string]string{"current": active, "previous": target} {
				if err := os.Symlink("releases/"+release, filepath.Join(runtimeRoot, link)); err != nil {
					t.Fatal(err)
				}
			}
			var files map[string]string
			if err := json.Unmarshal(read(filepath.Join("..", "releasetransition", "testdata", "v0.30.0-completed-install-checkpointed.json")), &files); err != nil {
				t.Fatal(err)
			}
			for path, body := range files {
				writeReleaseTransitionTestFile(t, filepath.Join(configHome, path), []byte(body), 0o600)
			}
			report, err := releaseruntime.InspectHistoryCompatibility(runtimeRoot, configHome, releasetransition.ReleaseID(target))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("outer selection to %s: kind=%s admitted=%v declared=%v refusals=%+v", test.version, report.Kind, report.Admitted, report.DeclaredPredecessor, report.Refusals)
			if report.Admitted != test.admitted {
				t.Fatalf("outer selection = %#v", report)
			}
			if test.admitted && (report.Kind != releasetransition.HistoryEquivalent || !report.DeclaredPredecessor) {
				t.Fatalf("outer selection = %#v", report)
			}
			if !test.admitted && (len(report.Refusals) != 1 || report.Refusals[0].Reason != releasetransition.HistoryRegistryUnreadable) {
				t.Fatalf("outer selection refusals = %#v", report.Refusals)
			}
			request := releasetransition.ProcessRequest{
				SchemaVersion: releasetransition.ProcessProtocolSchemaV1, Mode: releasetransition.ProcessInspect,
				RuntimeRoot: runtimeRoot, ConfigHome: configHome,
				Target: releasetransition.ReleaseID(target), Direction: releasetransition.DirectionActivatePrevious, ArtifactDigest: digest,
			}
			for _, mode := range []releasetransition.ProcessMode{releasetransition.ProcessInspect, releasetransition.ProcessConverge} {
				request.Mode = mode
				if err := releaseruntime.CheckSourcePolicy(context.Background(), request); err != nil {
					t.Fatalf("%s source policy = %v", mode, err)
				}
			}
			request.Mode = releasetransition.ProcessInspect
			response := inspectEquivalentRollback(t, repositoryRoot, request)
			refused := rollbackIncompatible(response)
			t.Logf("engine rollback to %s: refused=%v outcome=%+v inspection=%+v", test.version, refused, response.Outcome, response.Inspection)
			if refused == test.admitted {
				t.Fatalf("engine rollback refused = %v", refused)
			}
			if test.admitted && (response.Outcome != nil || response.Inspection == nil || len(response.Inspection.Blockers) != 0) {
				t.Fatalf("engine rollback to 0.32.0 = %#v", response)
			}
		})
	}
}
