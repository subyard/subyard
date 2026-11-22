package releasetransition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/blang/semver/v4"
)

type rollbackGateBaseline struct {
	Version            string                         `json:"version"`
	BundleSHA256       string                         `json:"bundleSHA256"`
	AgentSourcesDigest Fingerprint                    `json:"agentSourcesDigest"`
	Profiles           map[string]profile.RuntimeHook `json:"profiles"`
	RegistryFeatures   []string                       `json:"registryFeatures"`
}

type rollbackGateTree struct {
	registry    RegistryV2
	declaration RollbackDeclaration
	profiles    map[string]profile.RuntimeHook
	agents      Fingerprint
}

// This is a repository contract, deliberately conservative about agent source
// changes: a source diff needs reviewed cleanup even when files remain readable.
// No network, Git history or packaging-time capability negotiation is needed.
func rollbackGate(current, previous rollbackGateTree, version string) error {
	var refusals []error
	declared := func(component string, digest Fingerprint) bool {
		return slices.ContainsFunc(current.declaration.Boundaries, func(b RollbackBoundary) bool {
			return b.Component == component && b.ThroughVersion == version && (digest == "" || b.SourceDigest == digest)
		})
	}
	path, err := PlanDownPath(current.registry, previous.registry, previous.declaration.RegistryFeatures...)
	if err != nil {
		refusals = append(refusals, err)
	} else if path.Kind == HistoryRefuse ||
		path.Kind == HistoryEquivalent && current.declaration.PredecessorRegistryDigest != path.Target {
		if !declared("registry", "") {
			refusals = append(refusals, fmt.Errorf("registry changed without a down path to %s or a rollback boundary", version))
		}
	}
	owners := slices.Sorted(maps.Keys(previous.profiles))
	for owner := range current.profiles {
		if !slices.Contains(owners, owner) {
			owners = append(owners, owner)
		}
	}
	slices.Sort(owners)
	for _, owner := range owners {
		before, beforeOK := previous.profiles[owner]
		after, afterOK := current.profiles[owner]
		if beforeOK && afterOK && (before.EffectiveFormat() == after.EffectiveFormat() || after.PreparesRollbackTo == before.EffectiveFormat()) {
			continue
		}
		if !declared("profile:"+owner, "") {
			refusals = append(refusals, fmt.Errorf("profile runtime %s changed without rollback preparation to %s or a rollback boundary", owner, version))
		}
	}
	if current.agents != previous.agents && !declared("agents", current.agents) {
		refusals = append(refusals, fmt.Errorf("agent integration sources changed without reviewed surviving files and manual cleanup; declare an agents boundary through %s with sourceDigest %s", version, current.agents))
	}
	return domain.CollectRefusals(refusals...)
}

func TestPublishedRollbackGate(t *testing.T) {
	root := filepath.Join("..", "..")
	var baseline rollbackGateBaseline
	readJSONTest(t, "testdata/published-rollback-baseline.json", &baseline)
	// The published-upgrade list, not a second release-version constant, selects N-1.
	data, err := os.ReadFile(filepath.Join(root, "dev/e2e/published-upgrade-baselines.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	latest, checksum := semver.Version{}, ""
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 {
			t.Fatalf("invalid published baseline row %q", line)
		}
		version, err := semver.Parse(fields[0])
		if err != nil {
			t.Fatal(err)
		}
		if version.GT(latest) {
			latest, checksum = version, fields[2]
		}
	}
	if baseline.Version != latest.String() || baseline.BundleSHA256 != checksum {
		t.Fatal("rollback snapshot is stale; run python3 dev/update-rollback-baseline.py from the repository root")
	}
	previous := rollbackGateTree{profiles: baseline.Profiles, agents: baseline.AgentSourcesDigest,
		declaration: RollbackDeclaration{RegistryFeatures: baseline.RegistryFeatures}}
	previous.registry = readGateRegistry(t, "testdata/v"+baseline.Version+"-release-transition.json")
	current := rollbackGateTree{registry: readGateRegistry(t, filepath.Join(root, "config/release-transition.json")), profiles: map[string]profile.RuntimeHook{}}
	declaration, err := os.ReadFile(filepath.Join(root, "config/release-rollback.json"))
	if err != nil {
		t.Fatal(err)
	}
	current.declaration, err = ParseRollbackDeclaration(declaration)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "config/profiles/*/profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		var definition struct {
			Runtime *profile.RuntimeHook `json:"runtime"`
		}
		readJSONTest(t, path, &definition)
		if definition.Runtime != nil {
			current.profiles[definition.Runtime.ActivationID] = *definition.Runtime
		}
	}
	current.agents = agentSourcesDigest(t, filepath.Join(root, "config/agents"))
	if err := rollbackGate(current, previous, baseline.Version); err != nil {
		t.Fatal(err)
	}
}

func readGateRegistry(t *testing.T, path string) RegistryV2 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	registry, _, err := ParseRegistryV2(data, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func readJSONTest(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

func agentSourcesDigest(t *testing.T, root string) Fingerprint {
	t.Helper()
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "tests" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("agent source must be a regular file: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		// Git stores one executable flag, based on owner execute, rather than
		// group/other permissions. Managed worktrees may use 0744 instead of 0755.
		mode := 0
		if info.Mode().Perm()&0o100 != 0 {
			mode = 0o111
		}
		fmt.Fprintf(hash, "%s %03o %x\n", filepath.ToSlash(relative), mode, sha256.Sum256(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return Fingerprint(hex.EncodeToString(hash.Sum(nil)))
}

func TestRollbackGateRequiresCoverageForEveryChange(t *testing.T) {
	registry := singleDomainRegistry()
	digest, _ := registry.CanonicalDigest()
	old := rollbackGateTree{registry: registry, profiles: map[string]profile.RuntimeHook{"sample": {}}, agents: Fingerprint(strings.Repeat("a", 64))}
	fresh := func() rollbackGateTree {
		return rollbackGateTree{registry: registry, profiles: maps.Clone(old.profiles), agents: old.agents,
			declaration: RollbackDeclaration{PredecessorRegistryDigest: digest}}
	}
	for _, area := range []string{"registry", "profile:sample", "agents"} {
		t.Run(area, func(t *testing.T) {
			current := fresh()
			switch area {
			case "registry":
				current.registry = olderHistoryRegistry(registry)
			case "profile:sample":
				current.profiles["sample"] = profile.RuntimeHook{Format: 2}
			case "agents":
				current.agents = Fingerprint(strings.Repeat("b", 64))
			}
			if err := rollbackGate(current, old, "0.30.0"); err == nil {
				t.Fatal("undeclared change passed")
			}
			boundary := RollbackBoundary{Component: area, ThroughVersion: "0.30.0", Reason: "incompatible installed state", Recovery: "keep current release active and restore the saved state"}
			if area == "agents" {
				boundary.SourceDigest = current.agents
				boundary.Files = []string{"/etc/sample/drop-in.json"}
			}
			current.declaration.Boundaries = []RollbackBoundary{boundary}
			if err := rollbackGate(current, old, "0.30.0"); err != nil {
				t.Fatal(err)
			}
			current.declaration.Boundaries[0].ThroughVersion = "0.29.0"
			if err := rollbackGate(current, old, "0.30.0"); err == nil {
				t.Fatal("stale boundary passed")
			}
		})
	}
	current := fresh()
	current.profiles["sample"] = profile.RuntimeHook{Format: 2, PreparesRollbackTo: 1}
	if err := rollbackGate(current, old, "0.30.0"); err != nil {
		t.Fatalf("profile hook refused: %v", err)
	}
	current = fresh()
	current.registry = retire(t, registry, map[string]int{"settings": 2})
	if err := rollbackGate(current, old, "0.30.0"); err != nil {
		t.Fatalf("declared equivalent down path refused: %v", err)
	}
	current = fresh()
	current.agents = Fingerprint(strings.Repeat("b", 64))
	current.declaration.Boundaries = []RollbackBoundary{{Component: "agents", ThroughVersion: "0.30.0", SourceDigest: old.agents}}
	if err := rollbackGate(current, old, "0.30.0"); err == nil {
		t.Fatal("stale agent source review passed")
	}
}

func TestAgentSourcesDigestUsesGitExecutableFlag(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "provision.sh")
	source := []byte("#!/bin/sh\nexit 0\n")
	testkit.WriteFile(t, path, source, 0o744)
	ownerOnly := agentSourcesDigest(t, root)
	testkit.WriteFile(t, path, source, 0o755)
	if got := agentSourcesDigest(t, root); got != ownerOnly {
		t.Fatalf("Git-equivalent executable modes changed digest: 0744=%s 0755=%s", ownerOnly, got)
	}
	testkit.WriteFile(t, path, source, 0o644)
	if got := agentSourcesDigest(t, root); got == ownerOnly {
		t.Fatal("removing the executable flag did not change the source digest")
	}
}
