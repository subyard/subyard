package testimpact

import (
	"reflect"
	"slices"
	"testing"
)

func TestSelectEmptyDiffReturnsAnEmptySelectedPlan(t *testing.T) {
	policy, registry := selectorFixture(t)

	got := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: []Change{}})

	want := Result{
		SchemaVersion:  1,
		Status:         "selected",
		Changes:        []Change{},
		CheckSets:      []string{},
		RiskDomains:    []string{},
		HostFreeChecks: []CheckRecommendation{},
		E2EChecks:      []CheckRecommendation{},
		FullP0:         FullP0Selection{Reasons: []FullP0Reason{}},
		Reasons:        []SelectionReason{},
		Errors:         []ResultError{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Select(empty) = %#v, want %#v", got, want)
	}
}

func TestSelectClassifiesDocumentationTestsLeafPackagesAndSpecialProfiles(t *testing.T) {
	policy, registry := selectorFixture(t)
	tests := []struct {
		name         string
		changes      []Change
		checkSets    []string
		riskDomains  []string
		hostFreeIDs  []string
		e2eIDs       []string
		fullRequired bool
	}{
		{
			name:        "documentation has no checks or production domain",
			changes:     []Change{addedChange("docs/selector.md")},
			checkSets:   []string{},
			riskDomains: []string{},
			hostFreeIDs: []string{},
			e2eIDs:      []string{},
		},
		{
			name:        "test-only path can select evidence without a production domain",
			changes:     []Change{modifiedChange("tests/new-contract.sh")},
			checkSets:   []string{"host-free:core"},
			riskDomains: []string{},
			hostFreeIDs: []string{"host-free:core"},
			e2eIDs:      []string{},
		},
		{
			name:         "leaf package selects its owner",
			changes:      []Change{modifiedChange("internal/configsync/sync.go")},
			checkSets:    []string{"go:configsync", "host-free:core"},
			riskDomains:  []string{"config-state"},
			hostFreeIDs:  []string{"go:configsync", "host-free:core"},
			e2eIDs:       []string{},
			fullRequired: false,
		},
		{
			name: "special profile expands named host-free and targeted evidence",
			changes: []Change{
				modifiedChange("config/profiles/orca/provision.sh"),
			},
			checkSets: []string{
				"e2e:orca-resource", "go:resource", "p0:profile-resource",
				"shell:orca-profile-resource", "shell:profile-resource-lifecycle",
				"shell:provision-profile-check",
			},
			riskDomains: []string{"orca-gpu-profile", "profile-resource"},
			hostFreeIDs: []string{
				"go:resource", "shell:orca-profile-resource",
				"shell:profile-resource-lifecycle", "shell:provision-profile-check",
			},
			e2eIDs:       []string{"e2e:orca-resource", "p0:profile-resource"},
			fullRequired: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: test.changes})
			if !reflect.DeepEqual(got.CheckSets, test.checkSets) {
				t.Errorf("CheckSets = %v, want %v", got.CheckSets, test.checkSets)
			}
			if !reflect.DeepEqual(got.RiskDomains, test.riskDomains) {
				t.Errorf("RiskDomains = %v, want %v", got.RiskDomains, test.riskDomains)
			}
			if gotIDs := recommendationIDs(got.HostFreeChecks); !reflect.DeepEqual(gotIDs, test.hostFreeIDs) {
				t.Errorf("host-free IDs = %v, want %v", gotIDs, test.hostFreeIDs)
			}
			if gotIDs := recommendationIDs(got.E2EChecks); !reflect.DeepEqual(gotIDs, test.e2eIDs) {
				t.Errorf("E2E IDs = %v, want %v", gotIDs, test.e2eIDs)
			}
			if got.FullP0.Required != test.fullRequired {
				t.Errorf("FullP0.Required = %t, want %t", got.FullP0.Required, test.fullRequired)
			}
			if len(got.Errors) != 0 {
				t.Errorf("Errors = %v, want none", got.Errors)
			}
		})
	}
}

func TestSelectClassifiesRenameCopyDeleteAndModeSides(t *testing.T) {
	policy, registry := selectorFixture(t)
	tests := []struct {
		name    string
		change  Change
		reasons []SelectionReason
	}{
		{
			name: "rename classifies old and new paths",
			change: renamedChange(
				"internal/configsync/old.go",
				"internal/rpc/new.go",
			),
			reasons: []SelectionReason{
				{
					Path: "internal/configsync/old.go", Side: "old", RuleID: "internal-configsync",
					CheckSets: []string{"go:configsync", "host-free:core"}, RiskDomains: []string{"config-state"},
				},
				{
					Path: "internal/rpc/new.go", Side: "new", RuleID: "internal-rpc",
					CheckSets:   []string{"e2e:ssh-rpc", "go:adapters/remotecontrol", "go:adapters/transport", "go:application", "go:audit", "go:cli", "go:command", "go:domain", "go:rpc", "go:sshidentity", "go:sshrelay", "p0:peer", "p0:transport", "shell:cli-contract", "shell:command-registry", "shell:prompt-contract", "shell:remote-projects", "shell:ssh-config", "shell:ssh-transport-identity", "shell:yard-remote", "shell:yard-usage"},
					RiskDomains: []string{"ssh-remote-transport"},
				},
			},
		},
		{
			name:   "copy classifies old and new paths",
			change: copiedChange("docs/source.md", "internal/configsync/copied.go"),
			reasons: []SelectionReason{
				{Path: "docs/source.md", Side: "old", RuleID: "documentation-and-metadata", CheckSets: []string{}, RiskDomains: []string{}},
				{Path: "internal/configsync/copied.go", Side: "new", RuleID: "internal-configsync", CheckSets: []string{"go:configsync", "host-free:core"}, RiskDomains: []string{"config-state"}},
			},
		},
		{
			name:   "delete classifies the old path",
			change: deletedChange("internal/configsync/removed.go"),
			reasons: []SelectionReason{
				{Path: "internal/configsync/removed.go", Side: "old", RuleID: "internal-configsync", CheckSets: []string{"go:configsync", "host-free:core"}, RiskDomains: []string{"config-state"}},
			},
		},
		{
			name:   "mode change classifies its single identity once",
			change: typeChangedChange("internal/configsync/link.go"),
			reasons: []SelectionReason{
				{Path: "internal/configsync/link.go", Side: "new", RuleID: "internal-configsync", CheckSets: []string{"go:configsync", "host-free:core"}, RiskDomains: []string{"config-state"}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: []Change{test.change}})
			if !reflect.DeepEqual(got.Reasons, test.reasons) {
				t.Fatalf("Reasons = %#v, want %#v", got.Reasons, test.reasons)
			}
		})
	}
}

func TestSelectAppliesFullP0Precedence(t *testing.T) {
	policy, registry := selectorFixture(t)
	tests := []struct {
		name    string
		changes []Change
		want    FullP0Selection
	}{
		{
			name:    "standalone full domain",
			changes: []Change{modifiedChange("dev/agent-e2e.sh")},
			want: FullP0Selection{Required: true, Reasons: []FullP0Reason{
				{Code: "standalone_full_domain", RiskDomains: []string{"allocation-fencing-lease-identity"}},
			}},
		},
		{
			name: "standalone full domain takes precedence over high-risk combination",
			changes: []Change{
				modifiedChange("dev/agent-e2e.sh"),
				modifiedChange("internal/adapters/transport/client.go"),
			},
			want: FullP0Selection{Required: true, Reasons: []FullP0Reason{
				{Code: "standalone_full_domain", RiskDomains: []string{"allocation-fencing-lease-identity"}},
			}},
		},
		{
			name: "explicit high-risk combination",
			changes: []Change{
				modifiedChange("internal/migration/engine.go"),
				modifiedChange("config/systemd/subyard.service.in"),
			},
			want: FullP0Selection{Required: true, Reasons: []FullP0Reason{
				{Code: "high_risk_combination", RiskDomains: []string{"boot-lifecycle", "migration"}},
			}},
		},
		{
			name: "high-risk combination matches a domain superset",
			changes: []Change{
				modifiedChange("internal/migration/engine.go"),
				modifiedChange("config/systemd/subyard.service.in"),
				modifiedChange("internal/configsync/sync.go"),
			},
			want: FullP0Selection{Required: true, Reasons: []FullP0Reason{
				{Code: "high_risk_combination", RiskDomains: []string{"boot-lifecycle", "migration"}},
			}},
		},
		{
			name:    "exact safe combination suppresses default only",
			changes: []Change{modifiedChange("config/profiles/orca/provision.sh")},
			want:    FullP0Selection{Reasons: []FullP0Reason{}},
		},
		{
			name: "safe combination does not exempt a domain superset",
			changes: []Change{
				modifiedChange("config/profiles/orca/provision.sh"),
				modifiedChange("internal/configsync/sync.go"),
			},
			want: FullP0Selection{Required: true, Reasons: []FullP0Reason{
				{Code: "default_multi_domain", RiskDomains: []string{"config-state", "orca-gpu-profile", "profile-resource"}},
			}},
		},
		{
			name: "other multi-domain combination uses default",
			changes: []Change{
				modifiedChange("internal/application/app.go"),
				modifiedChange("internal/configsync/sync.go"),
			},
			want: FullP0Selection{Required: true, Reasons: []FullP0Reason{
				{Code: "default_multi_domain", RiskDomains: []string{"command-control", "config-state"}},
			}},
		},
		{
			name:    "one production domain does not require full",
			changes: []Change{modifiedChange("internal/configsync/sync.go")},
			want:    FullP0Selection{Reasons: []FullP0Reason{}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: test.changes})
			if !reflect.DeepEqual(got.FullP0, test.want) {
				t.Fatalf("FullP0 = %#v, want %#v (domains %v)", got.FullP0, test.want, got.RiskDomains)
			}
		})
	}
}

func TestSelectRequiresFullP0ForCombinedBootMigrationImplementation(t *testing.T) {
	policy, registry := selectorFixture(t)

	got := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: []Change{
		modifiedChange("internal/migration/power_reconciler.go"),
	}})

	want := FullP0Selection{Required: true, Reasons: []FullP0Reason{{
		Code: "high_risk_combination", RiskDomains: []string{"boot-lifecycle", "migration"},
	}}}
	if !reflect.DeepEqual(got.RiskDomains, []string{"boot-lifecycle", "migration"}) {
		t.Fatalf("RiskDomains = %v, want boot lifecycle plus migration", got.RiskDomains)
	}
	if !reflect.DeepEqual(got.FullP0, want) {
		t.Fatalf("FullP0 = %#v, want %#v", got.FullP0, want)
	}

	for _, path := range []string{
		"config/systemd/subyard-power-reconcile.service.in",
		"scripts/install-power-reconciler.sh",
		"dev/e2e/power-reconciler-systemd-255.sh",
		"dev/e2e/power-reconciler-systemd.sh",
	} {
		t.Run(path+" remains boot-only", func(t *testing.T) {
			bootOnly := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: []Change{modifiedChange(path)}})
			if !reflect.DeepEqual(bootOnly.RiskDomains, []string{"boot-lifecycle"}) || bootOnly.FullP0.Required {
				t.Fatalf("ordinary boot path gained migration risk: domains=%v full=%#v", bootOnly.RiskDomains, bootOnly.FullP0)
			}
			if !slices.Contains(recommendationIDs(bootOnly.HostFreeChecks), "shell:power-reconciler-systemd-255-launch") {
				t.Fatalf("ordinary boot path omitted the systemd-255 launch contract: checks=%v", recommendationIDs(bootOnly.HostFreeChecks))
			}
		})
	}
}

func TestSelectRequiresFullP0ForDurableReleaseScripts(t *testing.T) {
	policy, registry := selectorFixture(t)
	for _, path := range []string{
		"scripts/install-runtime-release.sh",
		"scripts/migrate-source-install.sh",
		"scripts/restore-source-install.sh",
	} {
		t.Run(path, func(t *testing.T) {
			got := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: []Change{modifiedChange(path)}})

			if !reflect.DeepEqual(got.RiskDomains, []string{"durable-layout", "release-runner"}) {
				t.Fatalf("RiskDomains = %v, want durable layout plus release runner", got.RiskDomains)
			}
			want := FullP0Selection{Required: true, Reasons: []FullP0Reason{{
				Code: "high_risk_combination", RiskDomains: []string{"durable-layout", "release-runner"},
			}}}
			if !reflect.DeepEqual(got.FullP0, want) {
				t.Fatalf("FullP0 = %#v, want %#v", got.FullP0, want)
			}
		})
	}

}

func TestSelectRequiresFullP0ForDependencyAndReleaseRunnerSubpaths(t *testing.T) {
	policy, registry := selectorFixture(t)
	wantDomains := []string{"dependency-compatibility", "release-runner"}
	wantFull := FullP0Selection{Required: true, Reasons: []FullP0Reason{{
		Code: "default_multi_domain", RiskDomains: wantDomains,
	}}}

	for _, changedPath := range []string{
		"go.mod",
		"go.sum",
		"dev/package-engine.sh",
		"dev/release-assets.sh",
		".github/workflows/release.yml",
	} {
		t.Run(changedPath, func(t *testing.T) {
			got := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: []Change{
				modifiedChange(changedPath),
			}})
			if !reflect.DeepEqual(got.RiskDomains, wantDomains) {
				t.Fatalf("RiskDomains = %v, want %v", got.RiskDomains, wantDomains)
			}
			if !reflect.DeepEqual(got.FullP0, wantFull) {
				t.Fatalf("FullP0 = %#v, want %#v", got.FullP0, wantFull)
			}
		})
	}
}

func TestSelectGoTestsRetainOwningChecksWithoutProductionRisk(t *testing.T) {
	policy, registry := selectorFixture(t)

	testOnly := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: []Change{
		modifiedChange("internal/cli/cli_test.go"),
	}})
	if !slices.Contains(testOnly.CheckSets, "go:cli") {
		t.Fatalf("test-only checks = %v, missing owning go:cli check", testOnly.CheckSets)
	}
	if len(testOnly.RiskDomains) != 0 || testOnly.FullP0.Required {
		t.Fatalf("test-only change added production risk: domains=%v full=%#v", testOnly.RiskDomains, testOnly.FullP0)
	}
	if len(testOnly.Reasons) != 1 || testOnly.Reasons[0].RuleID != "internal-cli" || len(testOnly.Reasons[0].RiskDomains) != 0 {
		t.Fatalf("test-only reason lost owning rule or retained risk: %#v", testOnly.Reasons)
	}

	crossPackage := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: []Change{
		modifiedChange("internal/configsync/sync.go"),
		modifiedChange("internal/cli/cli_test.go"),
	}})
	if !slices.Contains(crossPackage.CheckSets, "go:configsync") || !slices.Contains(crossPackage.CheckSets, "go:cli") {
		t.Fatalf("cross-package checks = %v, want both owning checks", crossPackage.CheckSets)
	}
	if !reflect.DeepEqual(crossPackage.RiskDomains, []string{"config-state"}) {
		t.Fatalf("cross-package domains = %v, want only production config-state", crossPackage.RiskDomains)
	}
	if crossPackage.FullP0.Required {
		t.Fatalf("co-located test spuriously required full P0: %#v", crossPackage.FullP0)
	}
}

func TestSelectFullP0DependsOnDomainsNotFileCountOrNonProductionPaths(t *testing.T) {
	policy, registry := selectorFixture(t)
	one := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: []Change{
		modifiedChange("internal/configsync/one.go"),
	}})
	many := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: []Change{
		modifiedChange("internal/configsync/one.go"),
		modifiedChange("internal/configsync/two.go"),
		modifiedChange("tests/new-contract.sh"),
		modifiedChange("docs/selector.md"),
	}})

	if !reflect.DeepEqual(one.RiskDomains, []string{"config-state"}) || !reflect.DeepEqual(many.RiskDomains, one.RiskDomains) {
		t.Fatalf("risk domains changed with file count/non-production paths: one=%v many=%v", one.RiskDomains, many.RiskDomains)
	}
	if !reflect.DeepEqual(many.FullP0, one.FullP0) || many.FullP0.Required {
		t.Fatalf("full-P0 decision changed with file count/non-production paths: one=%#v many=%#v", one.FullP0, many.FullP0)
	}
	if slices.Contains(many.CheckSets, "host-free:all") {
		t.Fatalf("ordinary affected leaves synthesized host-free:all: %v", many.CheckSets)
	}
}

func TestSelectReportsUnknownPathsWithoutSynthesizingFallback(t *testing.T) {
	policy, registry := selectorFixture(t)
	change := addedChange("product/new-boundary.go")

	got := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: []Change{change}})

	if got.Status != "selected" {
		t.Errorf("Status = %q, want selected for the outer CLI to handle", got.Status)
	}
	if !reflect.DeepEqual(got.Errors, []ResultError{{
		Code: "UNMATCHED_PATH", Message: "repository path is not covered by impact policy: product/new-boundary.go",
	}}) {
		t.Errorf("Errors = %#v", got.Errors)
	}
	if len(got.CheckSets) != 0 || len(got.HostFreeChecks) != 0 || len(got.E2EChecks) != 0 || got.FullP0.Required {
		t.Fatalf("selector synthesized fallback for unknown path: %#v", got)
	}
}

func TestSelectReturnsStableSortedDeduplicatedMetadata(t *testing.T) {
	policy, registry := selectorFixture(t)
	changes := []Change{
		modifiedChange("internal/rpc/z.go"),
		modifiedChange("internal/configsync/a.go"),
		modifiedChange("internal/rpc/a.go"),
	}

	got := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: changes})
	again := Select(policy, registry, ChangeSet{SchemaVersion: 1, Changes: slices.Clone(changes)})

	if !reflect.DeepEqual(got, again) {
		t.Fatalf("repeated selection differs:\n%#v\n%#v", got, again)
	}
	if !slices.IsSorted(got.CheckSets) || !slices.IsSorted(got.RiskDomains) ||
		!strictlySorted(recommendationIDs(got.HostFreeChecks)) || !strictlySorted(recommendationIDs(got.E2EChecks)) {
		t.Fatalf("result sets are not stable sorted: checks=%v domains=%v host-free=%v e2e=%v",
			got.CheckSets, got.RiskDomains, recommendationIDs(got.HostFreeChecks), recommendationIDs(got.E2EChecks))
	}
	if len(got.RiskDomains) != 2 || len(got.Reasons) != 3 {
		t.Fatalf("deduplication lost domains or distinct path reasons: domains=%v reasons=%#v", got.RiskDomains, got.Reasons)
	}
	if got.Reasons[0].Path != "internal/configsync/a.go" || got.Reasons[1].Path != "internal/rpc/a.go" || got.Reasons[2].Path != "internal/rpc/z.go" {
		t.Fatalf("reasons are not path sorted: %#v", got.Reasons)
	}
	if got.Changes[0].NewPath == nil || *got.Changes[0].NewPath != "internal/configsync/a.go" {
		t.Fatalf("changes were not canonicalized: %#v", got.Changes)
	}
	for _, recommendation := range append(slices.Clone(got.HostFreeChecks), got.E2EChecks...) {
		registered, ok := registry.Check(recommendation.ID)
		if !ok {
			t.Fatalf("recommendation %q is not registered", recommendation.ID)
		}
		want := CheckRecommendation{
			ID: registered.ID, Tier: registered.Tier,
			BudgetSeconds: registered.BudgetSeconds, Rationale: registered.Rationale,
		}
		if recommendation != want {
			t.Errorf("recommendation = %#v, want fixed registry metadata %#v", recommendation, want)
		}
	}
}

func selectorFixture(t *testing.T) (Policy, Registry) {
	t.Helper()
	registry, err := BuiltInRegistry()
	if err != nil {
		t.Fatalf("BuiltInRegistry() error = %v", err)
	}
	return loadRepositoryPolicy(t), registry
}

func recommendationIDs(recommendations []CheckRecommendation) []string {
	ids := make([]string, 0, len(recommendations))
	for _, recommendation := range recommendations {
		ids = append(ids, recommendation.ID)
	}
	return ids
}

func strictlySorted(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] >= values[index] {
			return false
		}
	}
	return true
}

func addedChange(path string) Change {
	return Change{Status: "A", NewPath: stringPointer(path), OldMode: "000000", NewMode: "100644"}
}

func modifiedChange(path string) Change {
	return Change{Status: "M", OldPath: stringPointer(path), NewPath: stringPointer(path), OldMode: "100644", NewMode: "100644"}
}

func typeChangedChange(path string) Change {
	return Change{Status: "T", OldPath: stringPointer(path), NewPath: stringPointer(path), OldMode: "100644", NewMode: "120000"}
}

func deletedChange(path string) Change {
	return Change{Status: "D", OldPath: stringPointer(path), OldMode: "100644", NewMode: "000000"}
}

func renamedChange(oldPath, newPath string) Change {
	similarity := 100
	return Change{Status: "R", Similarity: &similarity, OldPath: stringPointer(oldPath), NewPath: stringPointer(newPath), OldMode: "100644", NewMode: "100644"}
}

func copiedChange(oldPath, newPath string) Change {
	change := renamedChange(oldPath, newPath)
	change.Status = "C"
	return change
}
