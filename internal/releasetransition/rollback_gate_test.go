package releasetransition_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	rt "github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testkit/integrationassets"
	"github.com/blang/semver/v4"
)

type rollbackGateBaseline struct {
	Version                   string                         `json:"version"`
	BundleSHA256              string                         `json:"bundleSHA256"`
	AgentSourcesDigest        rt.Fingerprint                 `json:"agentSourcesDigest"`
	Profiles                  map[string]profile.RuntimeHook `json:"profiles"`
	RegistryFeatures          []string                       `json:"registryFeatures"`
	PredecessorRegistryDigest rt.Fingerprint                 `json:"predecessorRegistryDigest,omitempty"`
}

type rollbackGateTree struct {
	registry    rt.RegistryV2
	declaration rt.RollbackDeclaration
	profiles    map[string]profile.RuntimeHook
	agents      rt.Fingerprint
}

// This is a repository contract, deliberately conservative about agent source
// changes: a source diff needs reviewed cleanup even when files remain readable.
// No network, Git history or packaging-time capability negotiation is needed.
func rollbackGate(current, previous rollbackGateTree, version string) error {
	var refusals []error
	declared := func(component string, digest rt.Fingerprint) bool {
		return slices.ContainsFunc(current.declaration.Boundaries, func(b rt.RollbackBoundary) bool {
			return b.Component == component && b.ThroughVersion == version && (digest == "" || b.SourceDigest == digest)
		})
	}
	verdict := rt.HistoryCompatibility(rt.HistoryCompatibilityInput{Active: &current.registry, Target: &previous.registry,
		Predecessor: current.declaration.PredecessorRegistryDigest, TargetPredecessor: previous.declaration.PredecessorRegistryDigest,
		TargetRegistryFeatures: previous.declaration.RegistryFeatures})
	if !verdict.RegistryAdmitted() && !declared("registry", "") {
		refusals = append(refusals, fmt.Errorf("registry changed without a down path to %s or a rollback boundary", version))
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
		if !beforeOK || !afterOK || before.EffectiveFormat() == after.EffectiveFormat() || after.PreparesRollbackTo == before.EffectiveFormat() {
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
		declaration: rt.RollbackDeclaration{RegistryFeatures: baseline.RegistryFeatures, PredecessorRegistryDigest: baseline.PredecessorRegistryDigest}}
	previous.registry = readGateRegistry(t, "testdata/v"+baseline.Version+"-release-transition.json")
	current := rollbackGateTree{registry: readGateRegistry(t, filepath.Join(root, "config/release-transition.json")), profiles: map[string]profile.RuntimeHook{}}
	declaration, err := os.ReadFile(filepath.Join(root, "config/release-rollback.json"))
	if err != nil {
		t.Fatal(err)
	}
	current.declaration, err = rt.ParseRollbackDeclaration(declaration)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := integrationassets.TrackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range paths {
		if !strings.HasPrefix(relative, "config/profiles/") || !strings.HasSuffix(relative, "/profile.json") {
			continue
		}
		path := filepath.Join(root, relative)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		var definition struct {
			Runtime *profile.RuntimeHook `json:"runtime"`
		}
		readJSONTest(t, path, &definition)
		if definition.Runtime != nil {
			current.profiles[definition.Runtime.ActivationID] = *definition.Runtime
		}
	}
	current.agents = agentSourcesDigest(t, root)
	if err := rollbackGate(current, previous, baseline.Version); err != nil {
		t.Fatal(err)
	}
}

func readGateRegistry(t *testing.T, path string) rt.RegistryV2 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	registry, _, err := rt.ParseRegistryV2(data, rt.BuiltinCapabilityCatalog())
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

func agentSourcesDigest(t *testing.T, root string) rt.Fingerprint {
	t.Helper()
	files, err := integrationassets.TrackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	digest, _, err := integrationassets.Digest(root, files)
	if err != nil {
		t.Fatal(err)
	}
	return rt.Fingerprint(digest)
}

func TestRollbackGateRequiresCoverageForEveryChange(t *testing.T) {
	registry := gateRegistry()
	digest, _ := registry.CanonicalDigest()
	old := rollbackGateTree{registry: registry, profiles: map[string]profile.RuntimeHook{"sample": {}}, agents: rt.Fingerprint(strings.Repeat("a", 64))}
	fresh := func() rollbackGateTree {
		return rollbackGateTree{registry: registry, profiles: maps.Clone(old.profiles), agents: old.agents,
			declaration: rt.RollbackDeclaration{PredecessorRegistryDigest: digest}}
	}
	for _, area := range []string{"registry", "profile:sample", "agents"} {
		t.Run(area, func(t *testing.T) {
			current := fresh()
			switch area {
			case "registry":
				current.registry = olderGateRegistry(registry)
			case "profile:sample":
				current.profiles["sample"] = profile.RuntimeHook{Format: 2}
			case "agents":
				current.agents = rt.Fingerprint(strings.Repeat("b", 64))
			}
			if err := rollbackGate(current, old, "0.30.0"); err == nil {
				t.Fatal("undeclared change passed")
			}
			boundary := rt.RollbackBoundary{Component: area, ThroughVersion: "0.30.0", Reason: "incompatible installed state", Recovery: "keep current release active and restore the saved state"}
			if area == "agents" {
				boundary.SourceDigest = current.agents
				boundary.Files = []string{"/etc/sample/drop-in.json"}
			}
			current.declaration.Boundaries = []rt.RollbackBoundary{boundary}
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
	current.registry = retireGateRegistry(t, registry, map[string]int{"settings": 2})
	if err := rollbackGate(current, old, "0.30.0"); err != nil {
		t.Fatalf("declared equivalent down path refused: %v", err)
	}
	current = fresh()
	current.agents = rt.Fingerprint(strings.Repeat("b", 64))
	current.declaration.Boundaries = []rt.RollbackBoundary{{Component: "agents", ThroughVersion: "0.30.0", SourceDigest: old.agents}}
	if err := rollbackGate(current, old, "0.30.0"); err == nil {
		t.Fatal("stale agent source review passed")
	}
}

func TestAgentSourcesDigestUsesGitExecutableFlag(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "provision.sh")
	source := []byte("#!/bin/sh\nexit 0\n")
	testkit.WriteFile(t, path, source, 0o744)
	ownerOnly, err := integrationassets.DigestFiles(root, []string{"provision.sh"})
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, source, 0o755)
	if got, err := integrationassets.DigestFiles(root, []string{"provision.sh"}); err != nil || got != ownerOnly {
		t.Fatalf("Git-equivalent executable modes changed digest: 0744=%s 0755=%s", ownerOnly, got)
	}
	testkit.WriteFile(t, path, source, 0o644)
	if got, err := integrationassets.DigestFiles(root, []string{"provision.sh"}); err != nil || got == ownerOnly {
		t.Fatal("removing the executable flag did not change the source digest")
	}
}

func TestRollbackGateIgnoresIntroducedAndRemovedRuntimeOwners(t *testing.T) {
	registry := gateRegistry()
	previous := rollbackGateTree{registry: registry, profiles: map[string]profile.RuntimeHook{"removed": {Format: 2}, "shared": {Format: 1}}}
	current := rollbackGateTree{registry: registry, profiles: map[string]profile.RuntimeHook{"introduced": {Format: 3}, "shared": {Format: 1}}}
	if err := rollbackGate(current, previous, "0.30.0"); err != nil {
		t.Fatal(err)
	}
	current.profiles["shared"] = profile.RuntimeHook{Format: 2}
	if err := rollbackGate(current, previous, "0.30.0"); err == nil {
		t.Fatal("a shared owner's format change escaped")
	}
}

func TestRollbackGateUsesTheTargetDeclarationForRetiringSuccessors(t *testing.T) {
	registry := gateRegistry()
	retired := retireGateRegistry(t, registry, map[string]int{"settings": 2})
	activeDigest, _ := registry.CanonicalDigest()
	targetDigest, _ := retired.CanonicalDigest()
	current := rollbackGateTree{registry: registry, declaration: rt.RollbackDeclaration{PredecessorRegistryDigest: targetDigest}}
	previous := rollbackGateTree{registry: retired}
	if err := rollbackGate(current, previous, "0.30.0"); err == nil {
		t.Fatal("active predecessor incorrectly admitted a retiring target")
	}
	current.declaration.PredecessorRegistryDigest = ""
	previous.declaration.PredecessorRegistryDigest = activeDigest
	if err := rollbackGate(current, previous, "0.30.0"); err != nil {
		t.Fatal(err)
	}
	previous.declaration.PredecessorRegistryDigest = rt.Fingerprint(strings.Repeat("f", 64))
	if err := rollbackGate(current, previous, "0.30.0"); err == nil {
		t.Fatal("foreign target predecessor admitted")
	}
}

func gateRegistry() rt.RegistryV2 {
	return rt.RegistryV2{SchemaVersion: 2, MinimumEpochs: map[string]int{"settings": 1}, CurrentEpochs: map[string]int{"settings": 3}, Migrations: []rt.MigrationDefinitionV2{
		{ID: "settings-v2", Domain: "settings", FromEpoch: 1, ToEpoch: 2, Kind: "test-vms-settings-v1-to-v2"},
		{ID: "settings-v3", Domain: "settings", FromEpoch: 2, ToEpoch: 3, Kind: "local-ssh-port-settings-v2-to-v3"},
	}}
}
func olderGateRegistry(registry rt.RegistryV2) rt.RegistryV2 {
	registry.CurrentEpochs = maps.Clone(registry.CurrentEpochs)
	registry.CurrentEpochs["settings"] = 2
	registry.Migrations = slices.Clone(registry.Migrations[:1])
	return registry
}
func retireGateRegistry(t *testing.T, registry rt.RegistryV2, floors map[string]int) rt.RegistryV2 {
	t.Helper()
	retired, err := registry.RetirePrefixes(floors)
	if err != nil {
		t.Fatal(err)
	}
	return retired
}
