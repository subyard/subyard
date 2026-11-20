package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

const (
	equivalentSuccessor   = "0.31.0-bbbbbbbbbbbb"
	equivalentPredecessor = "0.30.0-aaaaaaaaaaaa"
)

// equivalentRollbackFixture is a v0.30.0 installation updated to a successor
// whose registry is successorRegistry: current names the successor and
// previous the v0.30.0 release. The protected history is the one the v0.30.0
// tag engine wrote for a completed install; a successor that only retires the
// definition-free project-state prefix leaves it unchanged.
type equivalentRollbackFixture struct {
	runtimeRoot, configHome string
	request                 releasetransition.ProcessRequest
	v030                    []byte
	v030Canonical           releasetransition.Fingerprint
}

func newEquivalentRollbackFixture(t *testing.T, successorRegistry []byte, predecessor releasetransition.Fingerprint) equivalentRollbackFixture {
	t.Helper()
	root := t.TempDir()
	fixture := equivalentRollbackFixture{runtimeRoot: filepath.Join(root, "runtime"), configHome: filepath.Join(root, "config")}
	var err error
	if fixture.v030, err = os.ReadFile(filepath.Join("..", "releasetransition", "testdata", "v0.30.0-release-transition.json")); err != nil {
		t.Fatal(err)
	}
	fixture.v030Canonical = v030CanonicalDigest(t)
	successorFiles := map[string][]byte{"config/release-transition.json": successorRegistry}
	if predecessor != "" {
		successorFiles["config/release-rollback.json"] = []byte(fmt.Sprintf(
			`{"schemaVersion":1,"predecessorRegistryDigest":%q,"migrations":[],"profileFormats":[]}`+"\n", predecessor))
	}
	writeSealedEquivalentRelease(t, fixture.runtimeRoot, equivalentSuccessor, "0.31.0", successorFiles)
	digest := writeSealedEquivalentRelease(t, fixture.runtimeRoot, equivalentPredecessor, "0.30.0",
		map[string][]byte{"config/release-transition.json": fixture.v030})
	for link, release := range map[string]string{"current": equivalentSuccessor, "previous": equivalentPredecessor} {
		if err := os.Symlink("releases/"+release, filepath.Join(fixture.runtimeRoot, link)); err != nil {
			t.Fatal(err)
		}
	}
	history, err := os.ReadFile(filepath.Join("..", "releasetransition", "testdata", "v0.30.0-completed-install-checkpointed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var files map[string]string
	if err := json.Unmarshal(history, &files); err != nil {
		t.Fatal(err)
	}
	for path, body := range files {
		writeReleaseTransitionTestFile(t, filepath.Join(fixture.configHome, path), []byte(body), 0o600)
	}
	fixture.request = releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1, Mode: releasetransition.ProcessInspect,
		RuntimeRoot: fixture.runtimeRoot, ConfigHome: fixture.configHome,
		Target: equivalentPredecessor, Direction: releasetransition.DirectionActivatePrevious, ArtifactDigest: digest,
	}
	return fixture
}

func v030CanonicalDigest(t *testing.T) releasetransition.Fingerprint {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("..", "releasetransition", "testdata", "v0.30.0-release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry, _, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := registry.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func writeSealedEquivalentRelease(t *testing.T, runtimeRoot, release, version string, files map[string][]byte) releasetransition.Fingerprint {
	t.Helper()
	root := filepath.Join(runtimeRoot, "releases", release)
	files["bin/yard-engine"] = []byte("#!/bin/sh\nprintf 'yard-engine " + version + "\\n'\n")
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	var manifest strings.Builder
	for _, path := range paths {
		mode := os.FileMode(0o600)
		if strings.HasPrefix(path, "bin/") {
			mode = 0o700
		}
		writeReleaseTransitionTestFile(t, filepath.Join(root, path), files[path], mode)
		fmt.Fprintf(&manifest, "%x  ./%s\n", sha256.Sum256(files[path]), path)
	}
	writeReleaseTransitionTestFile(t, filepath.Join(root, "runtime-files.sha256"), []byte(manifest.String()), 0o600)
	return releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256([]byte(manifest.String()))))
}

func projectStateRetiredV030(t *testing.T) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("..", "releasetransition", "testdata", "v0.30.0-release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry, _, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	retired, err := registry.RetirePrefixes(map[string]int{"project-state": 1})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(retired)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func inspectEquivalentRollback(t *testing.T, repositoryRoot string, request releasetransition.ProcessRequest) releasetransition.ProcessResponse {
	t.Helper()
	response, err := executeReleaseTransitionRequest(context.Background(), repositoryRoot, request,
		func(releasetransition.PlanToken, releasetransition.Authorization) bool { return true },
		nil, releaseTransitionOwnerFixture{}, nil)
	if err != nil {
		t.Fatalf("rollback inspection: %v", err)
	}
	return response
}

func rollbackIncompatible(response releasetransition.ProcessResponse) bool {
	if response.Outcome != nil && response.Outcome.Code == releasetransition.CodeRollbackIncompatible {
		return true
	}
	return response.Inspection != nil && slices.ContainsFunc(response.Inspection.Blockers, func(blocker releasetransition.Blocker) bool {
		return blocker.Code == releasetransition.CodeRollbackIncompatible
	})
}

// The v0.30.0 engine verb validates the active registry with CheckSourcePolicy
// and then inspects the rollback it owns. At this commit the registry parser,
// CheckSourcePolicy's registry rule and this engine's registry are those of
// v0.30.0 (internal/migrationhistory is unchanged since the tag, and
// config/release-transition.json has the tag bytes), so the same entry point
// reproduces it. A successor that retires only project-state passes it;
// a successor whose surviving definition depends on retired history does not,
// and the active release refuses to select that rollback.
func TestV030RollbackEntryPointAdmitsOnlyParseableRetirement(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	shipped, err := os.ReadFile(filepath.Join(repositoryRoot, "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := os.ReadFile(filepath.Join("..", "releasetransition", "testdata", "retiring-candidate-registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		registry []byte
		admitted bool
	}{
		"project-state retired":         {registry: projectStateRetiredV030(t), admitted: true},
		"dependency on retired history": {registry: candidate},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newEquivalentRollbackFixture(t, test.registry, v030CanonicalDigest(t))
			if string(shipped) != string(fixture.v030) {
				t.Fatal("this engine no longer stands in for the v0.30.0 registry")
			}
			for _, mode := range []releasetransition.ProcessMode{releasetransition.ProcessInspect, releasetransition.ProcessConverge} {
				request := fixture.request
				request.Mode = mode
				err := releaseruntime.CheckSourcePolicy(context.Background(), request)
				if test.admitted && err != nil || !test.admitted && (err == nil || !strings.Contains(err.Error(), "invalid or foreign V2 registry provenance")) {
					t.Fatalf("%s source policy = %v", mode, err)
				}
			}
			report, err := releaseruntime.InspectHistoryCompatibility(fixture.runtimeRoot, fixture.configHome, equivalentPredecessor)
			if test.admitted != (err == nil && report.Admitted) {
				t.Fatalf("active release selection = %#v, %v", report, err)
			}
			if !test.admitted {
				return
			}
			if report.Kind != releasetransition.HistoryEquivalent || !report.DeclaredPredecessor {
				t.Fatalf("active release selection = %#v", report)
			}
			response := inspectEquivalentRollback(t, repositoryRoot, fixture.request)
			if response.Outcome != nil || response.Inspection == nil || len(response.Inspection.Blockers) != 0 {
				t.Fatalf("v0.30.0 registry engine refused the rollback: %#v", response)
			}
		})
	}
}

// A current engine whose registry retires prefixes owns the rollback to its
// declared predecessor only through the rollback target's parsed registry and
// its own declaration.
func TestEngineRollbackAdmitsDeclaredEquivalentPredecessor(t *testing.T) {
	retired := projectStateRetiredV030(t)
	for name, declared := range map[string]bool{"declared": true, "undeclared": false} {
		t.Run(name, func(t *testing.T) {
			fixture := newEquivalentRollbackFixture(t, retired, "")
			predecessor := releasetransition.Fingerprint(strings.Repeat("f", 64))
			if declared {
				predecessor = fixture.v030Canonical
			}
			repositoryRoot := t.TempDir()
			writeReleaseTransitionTestFile(t, filepath.Join(repositoryRoot, "config", "release-transition.json"), retired, 0o600)
			writeReleaseTransitionTestFile(t, filepath.Join(repositoryRoot, "config", "release-rollback.json"), []byte(fmt.Sprintf(
				`{"schemaVersion":1,"predecessorRegistryDigest":%q,"migrations":[],"profileFormats":[]}`+"\n", predecessor)), 0o600)
			if refused := rollbackIncompatible(inspectEquivalentRollback(t, repositoryRoot, fixture.request)); refused == declared {
				t.Fatalf("rollback-incompatible = %v", refused)
			}
		})
	}
}
