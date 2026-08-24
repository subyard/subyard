package testimpact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestRegistryContainsRequiredChecksAndFixedCommands(t *testing.T) {
	registry, err := BuiltInRegistry()
	if err != nil {
		t.Fatalf("BuiltInRegistry() error = %v", err)
	}

	goPackages := []string{
		"application", "audit", "cli", "command", "config", "configsync", "credential",
		"domain", "migration", "ownerinventory", "ports", "resource", "rpc", "shellquote",
		"sshidentity", "sshrelay", "state", "systemdunit", "testyardmigration",
		"adapters/credentialmeta", "adapters/credentialruntime", "adapters/hostruntime",
		"adapters/incusclient", "adapters/projectruntime", "adapters/reconcileruntime",
		"adapters/releaseruntime", "adapters/remotecontrol", "adapters/securityruntime",
		"adapters/shelladapter", "adapters/statusruntime", "adapters/testvmsruntime",
		"adapters/transport",
	}
	for _, packageName := range goPackages {
		assertRegistryCheck(t, registry, Check{
			ID:            "go:" + packageName,
			Tier:          "T1",
			Argv:          []string{"go", "test", "-race", "./internal/" + packageName},
			BudgetSeconds: 180,
			Rationale:     "race-test the owning Go package",
		})
	}

	exact := []Check{
		{ID: "host-free:core", Tier: "T2", Argv: []string{"./tests/run.sh"}, BudgetSeconds: 1800, Rationale: "required core host-free merge gate"},
		{ID: "go:testimpact", Tier: "T1", Argv: []string{"go", "test", "-race", "./internal/testimpact"}, BudgetSeconds: 180, Rationale: "selector package self-check"},
		{ID: "go:test-impact-cli", Tier: "T1", Argv: []string{"go", "test", "./cmd/test-impact"}, BudgetSeconds: 180, Rationale: "selector CLI self-check"},
		{ID: "shell:test-impact", Tier: "T1", Argv: []string{"bash", "tests/test-impact.sh"}, BudgetSeconds: 180, Rationale: "selector wrapper and integration contract"},
		{ID: "veranda:test", Tier: "T1", Argv: []string{"npm", "run", "test"}, WorkingDirectory: "veranda", BudgetSeconds: 180, Rationale: "Veranda unit tests"},
		{ID: "veranda:check", Tier: "T1", Argv: []string{"npm", "run", "check"}, WorkingDirectory: "veranda", BudgetSeconds: 180, Rationale: "Veranda static checks"},
		{ID: "veranda:build", Tier: "T1", Argv: []string{"npm", "run", "build"}, WorkingDirectory: "veranda", BudgetSeconds: 180, Rationale: "Veranda production build"},
		{ID: "veranda:rust-test", Tier: "T1", Argv: []string{"cargo", "test", "--manifest-path", "veranda/src-tauri/Cargo.toml", "--no-default-features"}, BudgetSeconds: 300, Rationale: "Veranda Rust tests without desktop dependencies"},
	}
	for _, want := range exact {
		assertRegistryCheck(t, registry, want)
	}

	all, ok := registry.Check("host-free:all")
	if !ok {
		t.Fatal("registry is missing host-free:all")
	}
	wantMembers := []string{"host-free:core", "veranda:build", "veranda:check", "veranda:rust-test", "veranda:test"}
	if all.Tier != "T2" || len(all.Argv) != 0 || !reflect.DeepEqual(all.Members, wantMembers) {
		t.Fatalf("host-free:all = %#v, want non-executing T2 composite with members %v", all, wantMembers)
	}
	expanded, err := registry.Expand([]string{"host-free:all"})
	if err != nil {
		t.Fatalf("Expand(host-free:all) error = %v", err)
	}
	wantExpanded := []string{"host-free:core", "veranda:build", "veranda:check", "veranda:rust-test", "veranda:test"}
	if got := checkIDs(expanded); !reflect.DeepEqual(got, wantExpanded) {
		t.Fatalf("Expand(host-free:all) IDs = %v, want %v", got, wantExpanded)
	}
}

func TestRegistryShellInventoryMatchesTopLevelTests(t *testing.T) {
	repo := repositoryRoot(t)
	registry, err := BuiltInRegistry()
	if err != nil {
		t.Fatalf("BuiltInRegistry() error = %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(repo, "tests"))
	if err != nil {
		t.Fatalf("ReadDir(tests) error = %v", err)
	}
	filenames := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			filenames = append(filenames, entry.Name())
		}
	}
	want := expectedShellIDs(filenames)

	var got []string
	for _, id := range registry.IDs() {
		if strings.HasPrefix(id, "shell:") {
			got = append(got, id)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registry shell IDs = %v, want filesystem inventory plus bootstrap exception %v", got, want)
	}
	for _, id := range got {
		check, _ := registry.Check(id)
		basename := strings.TrimPrefix(id, "shell:")
		if !reflect.DeepEqual(check.Argv, []string{"bash", "tests/" + basename + ".sh"}) {
			t.Errorf("%s argv = %v", id, check.Argv)
		}
	}
}

func TestRegistryShellInventoryBootstrapIDRemainsUniqueAfterFileCreation(t *testing.T) {
	want := []string{"shell:agent-e2e", "shell:test-impact"}
	if got := expectedShellIDs([]string{"agent-e2e.sh", "test-impact.sh"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("bootstrap plus discovered shell IDs = %v, want %v", got, want)
	}
}

func TestRegistryNamedE2EChecksUsePreparedRunnableEntrypoints(t *testing.T) {
	registry, err := BuiltInRegistry()
	if err != nil {
		t.Fatalf("BuiltInRegistry() error = %v", err)
	}
	want := map[string][]string{
		"e2e:adapter-contracts": {
			"dev/agent-e2e.sh", "--purpose", "adapter-contracts", "--vm", "1", "--",
			"bash", "tests/real-host/adapter-contracts.sh",
		},
		"e2e:credential-tools": {
			"dev/agent-e2e.sh", "--purpose", "credential-tools", "--vm", "1", "--",
			"bash", "tests/real-host/adapter-contracts.sh", "--check", "credential-tools",
		},
		"e2e:hermes-profile": {
			"dev/agent-e2e.sh", "--purpose", "hermes-profile", "--vm", "both", "--",
			"./dev/e2e/hermes-profile.sh",
		},
		"e2e:orca-resource": {
			"dev/agent-e2e.sh", "--purpose", "orca-resource", "--vm", "1", "--",
			"env", "SUBYARD_E2E_ORCA_RESOURCE=1", "./tests/real-host/orca-resource.sh",
		},
		"e2e:ssh-credential-peer": {
			"dev/agent-e2e.sh", "--purpose", "ssh-credential-peer", "--vm", "1", "--",
			"bash", "tests/real-host/adapter-contracts.sh", "--check", "ssh-credential-peer",
		},
		"e2e:ssh-rpc": {
			"dev/agent-e2e.sh", "--purpose", "ssh-rpc", "--vm", "1", "--",
			"bash", "tests/real-host/adapter-contracts.sh", "--check", "ssh-rpc",
		},
	}

	gotIDs := make([]string, 0, len(want))
	for _, id := range registry.IDs() {
		if strings.HasPrefix(id, "e2e:") {
			gotIDs = append(gotIDs, id)
		}
	}
	wantIDs := make([]string, 0, len(want))
	for id := range want {
		wantIDs = append(wantIDs, id)
	}
	sort.Strings(wantIDs)
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("named E2E IDs = %v, want only prepared runnable checks %v", gotIDs, wantIDs)
	}
	for id, argv := range want {
		check, exists := registry.Check(id)
		if !exists {
			t.Errorf("registry is missing %s", id)
			continue
		}
		if check.Tier != "T3" || !reflect.DeepEqual(check.Argv, argv) {
			t.Errorf("%s = %#v, want fixed runnable T3 argv %v", id, check, argv)
		}
	}
	if _, exists := registry.Check("e2e:incus-contract"); exists {
		t.Error("unprepared incus-contract must use p0:real-incus, not a direct named E2E entry")
	}
}

func TestRegistryRejectsInvalidDefinitions(t *testing.T) {
	leaf := Check{ID: "leaf", Tier: "T1", Argv: []string{"true"}, BudgetSeconds: 1, Rationale: "fixture"}
	tests := []struct {
		name   string
		checks []Check
	}{
		{"duplicate ID", []Check{leaf, leaf}},
		{"empty ID", []Check{{Tier: "T1", Argv: []string{"true"}, BudgetSeconds: 1, Rationale: "fixture"}}},
		{"unknown tier", []Check{{ID: "leaf", Tier: "T0", Argv: []string{"true"}, BudgetSeconds: 1, Rationale: "fixture"}}},
		{"missing argv", []Check{{ID: "leaf", Tier: "T1", BudgetSeconds: 1, Rationale: "fixture"}}},
		{"missing rationale", []Check{{ID: "leaf", Tier: "T1", Argv: []string{"true"}, BudgetSeconds: 1}}},
		{"invalid budget", []Check{{ID: "leaf", Tier: "T1", Argv: []string{"true"}, Rationale: "fixture"}}},
		{"invalid working directory", []Check{{ID: "leaf", Tier: "T1", Argv: []string{"true"}, WorkingDirectory: "../outside", BudgetSeconds: 1, Rationale: "fixture"}}},
		{"composite with argv", []Check{{ID: "all", Tier: "T2", Argv: []string{"true"}, Members: []string{"leaf"}, BudgetSeconds: 1, Rationale: "fixture"}, leaf}},
		{"unknown member", []Check{{ID: "all", Tier: "T2", Members: []string{"missing"}, BudgetSeconds: 1, Rationale: "fixture"}}},
		{"duplicate member", []Check{{ID: "all", Tier: "T2", Members: []string{"leaf", "leaf"}, BudgetSeconds: 1, Rationale: "fixture"}, leaf}},
		{"composite cycle", []Check{
			{ID: "first", Tier: "T2", Members: []string{"second"}, BudgetSeconds: 1, Rationale: "fixture"},
			{ID: "second", Tier: "T2", Members: []string{"first"}, BudgetSeconds: 1, Rationale: "fixture"},
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ValidateRegistry(test.checks); err == nil {
				t.Fatal("ValidateRegistry() accepted invalid definitions")
			}
		})
	}
}

func TestRegistryP0InventoryMatchesAcceptanceLanesInCleanEnvironment(t *testing.T) {
	repo := repositoryRoot(t)
	registry, err := BuiltInRegistry()
	if err != nil {
		t.Fatalf("BuiltInRegistry() error = %v", err)
	}

	command := exec.Command(
		"/usr/bin/env", "-i",
		"PATH=/usr/bin:/bin",
		"HOME="+t.TempDir(),
		filepath.Join(repo, "dev/e2e/p0-acceptance.sh"), "--list-lanes",
	)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("p0-acceptance.sh --list-lanes error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 14 {
		t.Fatalf("--list-lanes returned %d rows, want 14: %q", len(lines), output)
	}

	wantP0 := make([]string, 0, 13)
	for _, lane := range lines[:13] {
		if lane == "" || strings.ContainsAny(lane, "\t ") {
			t.Fatalf("invalid targeted lane row %q", lane)
		}
		wantP0 = append(wantP0, "p0:"+lane)
	}
	sort.Strings(wantP0)
	var gotP0 []string
	for _, id := range registry.IDs() {
		if strings.HasPrefix(id, "p0:") {
			gotP0 = append(gotP0, id)
		}
	}
	if !reflect.DeepEqual(gotP0, wantP0) {
		t.Fatalf("registry p0 IDs = %v, want --list-lanes targeted IDs %v", gotP0, wantP0)
	}
	if _, exists := registry.Check("p0:full"); exists {
		t.Fatal("special full inventory row must not be a p0:full T3 check")
	}

	wantFull := "full\tboundary transport nested-teardown release source-upgrade power-systemd peer cleanup"
	if lines[13] != wantFull {
		t.Fatalf("full T4 inventory row = %q, want %q", lines[13], wantFull)
	}
	for _, id := range gotP0 {
		check, _ := registry.Check(id)
		lane := strings.TrimPrefix(id, "p0:")
		if check.Tier != "T3" || !reflect.DeepEqual(check.Argv, []string{"dev/e2e/p0-acceptance.sh", "--lane", lane}) {
			t.Errorf("%s = %#v, want fixed targeted T3 argv", id, check)
		}
	}
}

func TestPolicyLoadsRepositoryMapWithoutExecutableFields(t *testing.T) {
	repo := repositoryRoot(t)
	registry, err := BuiltInRegistry()
	if err != nil {
		t.Fatalf("BuiltInRegistry() error = %v", err)
	}
	source, err := os.ReadFile(filepath.Join(repo, "tests/impact-map.json"))
	if err != nil {
		t.Fatalf("ReadFile(impact-map.json) error = %v", err)
	}
	policy, err := LoadPolicy(bytes.NewReader(source), registry)
	if err != nil {
		t.Fatalf("LoadPolicy(impact-map.json) error = %v", err)
	}
	if policy.schemaVersion != 1 || !policy.defaultMultiDomainFull {
		t.Fatalf("policy version/default = %d/%t, want 1/true", policy.schemaVersion, policy.defaultMultiDomainFull)
	}

	var document any
	if err := json.Unmarshal(source, &document); err != nil {
		t.Fatalf("json.Unmarshal(impact-map.json) error = %v", err)
	}
	assertNoExecutableJSONFields(t, document)

	standalone := []string{"allocation-fencing-lease-identity", "checkpoint-resume", "full-matrix-composition", "marker-owned-cleanup"}
	if !reflect.DeepEqual(policy.standaloneFullDomains, standalone) {
		t.Fatalf("standalone full domains = %v, want %v", policy.standaloneFullDomains, standalone)
	}
	wantHighRisk := [][]string{
		{"boot-lifecycle", "migration"},
		{"broker-admission", "ssh-remote-transport"},
		{"durable-layout", "release-runner"},
	}
	for _, combination := range wantHighRisk {
		if !slices.ContainsFunc(policy.highRiskCombinations, func(candidate []string) bool {
			return reflect.DeepEqual(candidate, combination)
		}) {
			t.Errorf("high-risk combinations do not contain %v: %v", combination, policy.highRiskCombinations)
		}
	}
	if len(policy.safeMultiDomainCombinations) == 0 {
		t.Fatal("repository policy has no exact safe multi-domain combinations")
	}
}

func TestPolicyRejectsInvalidDocuments(t *testing.T) {
	registry, err := BuiltInRegistry()
	if err != nil {
		t.Fatalf("BuiltInRegistry() error = %v", err)
	}

	tests := []struct {
		name string
		json string
	}{
		{"unsupported schema", policyDocument(`"schema_version":2`)},
		{"unknown root field", policyDocument(`"extra":true`)},
		{"duplicate root key", strings.Replace(policyDocument(""), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1)},
		{"duplicate rule key", strings.Replace(policyDocument(""), `"id":"root","priority":1`, `"id":"root","id":"again","priority":1`, 1)},
		{"unpaired Unicode surrogate", strings.Replace(policyDocument(""), `"id":"root"`, `"id":"\ud800"`, 1)},
		{"unknown check ID", policyDocument(`"check_sets":{"base":["missing:check"]}`)},
		{"duplicate check ID", policyDocument(`"check_sets":{"base":["host-free:core","host-free:core"]}`)},
		{"empty named check set", policyDocument(`"check_sets":{"base":[]}`)},
		{"unknown rule check set", policyDocument(`"rules":[{"id":"root","priority":1,"pattern":"^.*$","check_sets":["missing"],"risk_domains":[]}]`)},
		{"unanchored regex", policyDocument(`"rules":[{"id":"root","priority":1,"pattern":".*","check_sets":["base"],"risk_domains":[]}]`)},
		{"top-level alternative has unanchored branches", policyDocument(`"rules":[{"id":"root","priority":1,"pattern":"^foo|bar$","check_sets":["base"],"risk_domains":[]}]`)},
		{"top-level alternative hides overlap", policyDocument(`"rules":[{"id":"first","priority":7,"pattern":"^foo|bar$","check_sets":["base"],"risk_domains":[]},{"id":"second","priority":7,"pattern":"^xxbar$","check_sets":[],"risk_domains":["alpha"]}]`)},
		{"escaped dollar is not an end anchor", policyDocument(`"rules":[{"id":"root","priority":1,"pattern":"^literal\\$","check_sets":["base"],"risk_domains":[]}]`)},
		{"invalid regex", policyDocument(`"rules":[{"id":"root","priority":1,"pattern":"^[$","check_sets":["base"],"risk_domains":[]}]`)},
		{"rule ID contains whitespace", policyDocument(`"rules":[{"id":"bad id","priority":1,"pattern":"^.*$","check_sets":["base"],"risk_domains":[]}]`)},
		{"duplicate rule ID", policyDocument(`"rules":[{"id":"root","priority":1,"pattern":"^first$","check_sets":["base"],"risk_domains":[]},{"id":"root","priority":2,"pattern":"^second$","check_sets":["base"],"risk_domains":[]}]`)},
		{"unknown rule domain", policyDocument(`"rules":[{"id":"root","priority":1,"pattern":"^.*$","check_sets":["base"],"risk_domains":["missing"]}]`)},
		{"ambiguous equal-priority overlap", policyDocument(`"rules":[{"id":"broad","priority":7,"pattern":"^internal/.*$","check_sets":["base"],"risk_domains":[]},{"id":"narrow","priority":7,"pattern":"^internal/cli/.*$","check_sets":[],"risk_domains":["alpha"]}]`)},
		{"ambiguous grouped alternatives", policyDocument(`"rules":[{"id":"first","priority":7,"pattern":"^(foo|bar)$","check_sets":["base"],"risk_domains":[]},{"id":"second","priority":7,"pattern":"^(xxbar|bar)$","check_sets":[],"risk_domains":["alpha"]}]`)},
		{"missing risk suppressions", strings.Replace(policyDocument(""), `"risk_suppressions":[],`, "", 1)},
		{"risk suppression missing ID", policyDocument(`"risk_suppressions":[{"kind":"go-test-file"}]`)},
		{"risk suppression missing kind", policyDocument(`"risk_suppressions":[{"id":"go-tests"}]`)},
		{"risk suppression blank ID", policyDocument(`"risk_suppressions":[{"id":"","kind":"go-test-file"}]`)},
		{"risk suppression ID contains whitespace", policyDocument(`"risk_suppressions":[{"id":"go tests","kind":"go-test-file"}]`)},
		{"duplicate risk suppression ID", policyDocument(`"risk_suppressions":[{"id":"go-tests","kind":"go-test-file"},{"id":"go-tests","kind":"go-test-file"}]`)},
		{"unknown risk suppression kind", policyDocument(`"risk_suppressions":[{"id":"unknown","kind":"other"}]`)},
		{"overlapping risk suppressions", policyDocument(`"risk_suppressions":[{"id":"all-tests","kind":"go-test-file"},{"id":"cli-tests","kind":"go-test-file"}]`)},
		{"risk suppression cannot target production paths", policyDocument(`"risk_suppressions":[{"id":"production","kind":"production-path"}]`)},
		{"risk suppression adds classification fields", policyDocument(`"risk_suppressions":[{"id":"go-tests","kind":"go-test-file","risk_domains":[]}]`)},
		{"unknown standalone domain", policyDocument(`"standalone_full_domains":["missing"]`)},
		{"one-domain high-risk set", policyDocument(`"high_risk_combinations":[["alpha"]]`)},
		{"duplicate high-risk exact set", policyDocument(`"high_risk_combinations":[["alpha","beta"],["beta","alpha"]]`)},
		{"one-domain safe set", policyDocument(`"safe_multi_domain_combinations":[{"domains":["alpha"],"contract":"tests/example.sh","rationale":"fixture"}]`)},
		{"duplicate safe domain", policyDocument(`"safe_multi_domain_combinations":[{"domains":["alpha","alpha"],"contract":"tests/example.sh","rationale":"fixture"}]`)},
		{"unknown safe domain", policyDocument(`"safe_multi_domain_combinations":[{"domains":["alpha","missing"],"contract":"tests/example.sh","rationale":"fixture"}]`)},
		{"missing safe contract", policyDocument(`"safe_multi_domain_combinations":[{"domains":["alpha","beta"],"rationale":"fixture"}]`)},
		{"missing safe rationale", policyDocument(`"safe_multi_domain_combinations":[{"domains":["alpha","beta"],"contract":"tests/example.sh"}]`)},
		{"duplicate safe exact set", policyDocument(`"safe_multi_domain_combinations":[{"domains":["alpha","beta"],"contract":"one","rationale":"fixture"},{"domains":["beta","alpha"],"contract":"two","rationale":"fixture"}]`)},
		{"safe and high-risk exact set", policyDocument(`"high_risk_combinations":[["alpha","beta"]],"safe_multi_domain_combinations":[{"domains":["alpha","beta"],"contract":"tests/example.sh","rationale":"fixture"}]`)},
		{"safe set contains high-risk subset", policyDocument(`"risk_domains":["alpha","beta","gamma"],"high_risk_combinations":[["alpha","beta"]],"safe_multi_domain_combinations":[{"domains":["alpha","beta","gamma"],"contract":"tests/example.sh","rationale":"fixture"}]`)},
		{"safe set contains standalone domain", policyDocument(`"standalone_full_domains":["alpha"],"safe_multi_domain_combinations":[{"domains":["alpha","beta"],"contract":"tests/example.sh","rationale":"fixture"}]`)},
		{"default multi-domain full false", policyDocument(`"default_multi_domain_full":false`)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := LoadPolicy(strings.NewReader(test.json), registry); err == nil {
				t.Fatal("LoadPolicy() accepted invalid document")
			}
		})
	}
}

func TestPolicyAcceptsEscapedUnicodeSurrogatePair(t *testing.T) {
	registry, err := BuiltInRegistry()
	if err != nil {
		t.Fatalf("BuiltInRegistry() error = %v", err)
	}
	document := strings.Replace(
		policyDocument(""),
		`"pattern":"^.*$"`,
		`"pattern":"^docs/\ud83d\ude80$"`,
		1,
	)

	policy, err := LoadPolicy(strings.NewReader(document), registry)
	if err != nil {
		t.Fatalf("LoadPolicy() error = %v", err)
	}
	classification, err := policy.ClassifyPath("docs/🚀")
	if err != nil {
		t.Fatalf("ClassifyPath() error = %v", err)
	}
	if !reflect.DeepEqual(classification.RuleIDs, []string{"root"}) {
		t.Fatalf("RuleIDs = %v, want root", classification.RuleIDs)
	}
}

func TestPolicyRiskSuppressionRetainsOwningChecks(t *testing.T) {
	registry, err := BuiltInRegistry()
	if err != nil {
		t.Fatalf("BuiltInRegistry() error = %v", err)
	}
	policy, err := LoadPolicy(strings.NewReader(policyDocument(`
		"rules":[{"id":"owner","priority":1,"pattern":"^internal/cli/.*$","check_sets":["base"],"risk_domains":["alpha"]}],
		"risk_suppressions":[{"id":"go-test-files","kind":"go-test-file"}]
	`)), registry)
	if err != nil {
		t.Fatalf("LoadPolicy() error = %v", err)
	}

	classification, err := policy.ClassifyPath("internal/cli/cli_test.go")
	if err != nil {
		t.Fatalf("ClassifyPath() error = %v", err)
	}
	if !reflect.DeepEqual(classification.RuleIDs, []string{"owner"}) ||
		!reflect.DeepEqual(classification.CheckSets, []string{"host-free:core"}) ||
		len(classification.RiskDomains) != 0 {
		t.Fatalf("ClassifyPath() = %#v, want owning rule/check and no risk domains", classification)
	}
}

func TestPolicyCanonicalizesOrderAndAppliesPriority(t *testing.T) {
	registry, err := BuiltInRegistry()
	if err != nil {
		t.Fatalf("BuiltInRegistry() error = %v", err)
	}
	first := policyDocument(`
		"risk_domains":["beta","alpha"],
		"rules":[
			{"id":"broad","priority":1,"pattern":"^internal/.*$","check_sets":["base"],"risk_domains":["beta"]},
			{"id":"narrow","priority":2,"pattern":"^internal/cli/.*$","check_sets":[],"risk_domains":["alpha"]}
		],
		"safe_multi_domain_combinations":[{"domains":["beta","alpha"],"contract":"tests/example.sh","rationale":"fixture"}]
	`)
	second := policyDocument(`
		"risk_domains":["alpha","beta"],
		"rules":[
			{"id":"narrow","priority":2,"pattern":"^internal/cli/.*$","check_sets":[],"risk_domains":["alpha"]},
			{"id":"broad","priority":1,"pattern":"^internal/.*$","check_sets":["base"],"risk_domains":["beta"]}
		],
		"safe_multi_domain_combinations":[{"domains":["alpha","beta"],"contract":"tests/example.sh","rationale":"fixture"}]
	`)
	one, err := LoadPolicy(strings.NewReader(first), registry)
	if err != nil {
		t.Fatalf("LoadPolicy(first) error = %v", err)
	}
	two, err := LoadPolicy(strings.NewReader(second), registry)
	if err != nil {
		t.Fatalf("LoadPolicy(second) error = %v", err)
	}

	oneClass, err := one.ClassifyPath("internal/cli/cli.go")
	if err != nil {
		t.Fatalf("first ClassifyPath() error = %v", err)
	}
	twoClass, err := two.ClassifyPath("internal/cli/cli.go")
	if err != nil {
		t.Fatalf("second ClassifyPath() error = %v", err)
	}
	want := Classification{RuleIDs: []string{"narrow"}, RiskDomains: []string{"alpha"}}
	if !reflect.DeepEqual(oneClass, want) || !reflect.DeepEqual(twoClass, want) {
		t.Fatalf("priority classifications = %#v / %#v, want %#v", oneClass, twoClass, want)
	}
	if !reflect.DeepEqual(one.riskDomains, two.riskDomains) ||
		!reflect.DeepEqual(one.safeMultiDomainCombinations, two.safeMultiDomainCombinations) {
		t.Fatalf("canonical policies differ by JSON order:\n%#v\n%#v", one, two)
	}
}

func TestPolicySelectorSelfChecksCannotBeRemovedByMap(t *testing.T) {
	registry, err := BuiltInRegistry()
	if err != nil {
		t.Fatalf("BuiltInRegistry() error = %v", err)
	}
	policy, err := LoadPolicy(strings.NewReader(policyDocument("")), registry)
	if err != nil {
		t.Fatalf("LoadPolicy() error = %v", err)
	}

	for _, path := range []string{
		"internal/testimpact/select.go",
		"cmd/test-impact/main.go",
		"dev/test-impact.sh",
		"tests/impact-map.json",
		"tests/test-impact.sh",
	} {
		classification, err := policy.ClassifyPath(path)
		if err != nil {
			t.Fatalf("ClassifyPath(%q) error = %v", path, err)
		}
		for _, wantCheck := range []string{"go:test-impact-cli", "go:testimpact", "shell:test-impact"} {
			if !slices.Contains(classification.CheckSets, wantCheck) {
				t.Errorf("ClassifyPath(%q) checks = %v, missing hard-coded self check %q", path, classification.CheckSets, wantCheck)
			}
		}
	}
}

func TestPolicyRepresentativePathsSelectOwningEvidence(t *testing.T) {
	policy := loadRepositoryPolicy(t)
	tests := []struct {
		name             string
		path             string
		checks           []string
		domains          []string
		standaloneDomain string
		highRisk         []string
	}{
		{
			name: "P0 guest matrix implementation", path: "dev/e2e/p0-guest.sh",
			checks:  []string{"host-free:core", "p0:boundary"},
			domains: []string{"full-matrix-composition"}, standaloneDomain: "full-matrix-composition",
		},
		{
			name: "P0 matrix runner", path: "dev/e2e/p0-acceptance.sh",
			checks:  []string{"host-free:core", "p0:boundary"},
			domains: []string{"checkpoint-resume", "full-matrix-composition"}, standaloneDomain: "full-matrix-composition",
		},
		{
			name: "real Incus lane", path: "dev/e2e/p0-real-incus.sh",
			checks:  []string{"go:adapters/incusclient", "p0:real-incus"},
			domains: []string{"incus-runtime"},
		},
		{
			name: "source upgrade lane", path: "dev/e2e/p0-source-upgrade.sh",
			checks:  []string{"go:migration", "p0:source-upgrade"},
			domains: []string{"durable-layout", "release-runner"}, highRisk: []string{"durable-layout", "release-runner"},
		},
		{
			name: "release catch-up fixture", path: "dev/e2e/release-migration-catch-up.sh",
			checks:  []string{"go:migration", "p0:release", "p0:source-upgrade"},
			domains: []string{"durable-layout", "release-runner"}, highRisk: []string{"durable-layout", "release-runner"},
		},
		{
			name: "release consumer fixture", path: "dev/e2e/release-migration-consumer.sh",
			checks:  []string{"go:migration", "p0:release", "p0:source-upgrade"},
			domains: []string{"durable-layout", "release-runner"}, highRisk: []string{"durable-layout", "release-runner"},
		},
		{
			name: "release layout test fixture", path: "tests/fixtures/migrations/layout-6-production-prefix.json",
			checks:  []string{"go:migration", "p0:release", "p0:source-upgrade"},
			domains: []string{},
		},
		{
			name: "Hermes E2E profile", path: "dev/e2e/hermes-profile.sh",
			checks:  []string{"e2e:hermes-profile", "p0:profile-resource", "shell:hermes-e2e-contract"},
			domains: []string{"external-service-profile", "profile-resource"},
		},
		{
			name: "Hermes profile handler", path: "config/profiles/hermes/resources/dashboard/handler.sh",
			checks:  []string{"e2e:hermes-profile", "p0:profile-resource", "shell:hermes-dashboard-resource"},
			domains: []string{"external-service-profile", "profile-resource"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classification, err := policy.ClassifyPath(test.path)
			if err != nil {
				t.Fatalf("ClassifyPath(%q) error = %v", test.path, err)
			}
			assertContainsAll(t, classification.CheckSets, test.checks)
			if !slices.Equal(classification.RiskDomains, test.domains) {
				t.Errorf("ClassifyPath(%q) domains = %v, want %v", test.path, classification.RiskDomains, test.domains)
			}
			if test.standaloneDomain != "" && !slices.Contains(policy.standaloneFullDomains, test.standaloneDomain) {
				t.Errorf("%q is not configured as a standalone-full domain", test.standaloneDomain)
			}
			if len(test.highRisk) != 0 && !slices.ContainsFunc(policy.highRiskCombinations, func(candidate []string) bool {
				return reflect.DeepEqual(candidate, test.highRisk)
			}) {
				t.Errorf("%v is not configured as a high-risk combination", test.highRisk)
			}
		})
	}
}

func TestPolicyRealHostTestPathsSelectOwningChecksWithoutRiskDomains(t *testing.T) {
	policy := loadRepositoryPolicy(t)
	tests := []struct {
		path  string
		check string
	}{
		{"tests/real-host/adapter-contracts.sh", "e2e:adapter-contracts"},
		{"tests/real-host/credential-tools.sh", "e2e:credential-tools"},
		{"tests/real-host/incus-contract.sh", "p0:real-incus"},
		{"tests/real-host/orca-resource.sh", "e2e:orca-resource"},
		{"tests/real-host/ssh-credential-peer.sh", "e2e:ssh-credential-peer"},
		{"tests/real-host/ssh-rpc.sh", "e2e:ssh-rpc"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			classification, err := policy.ClassifyPath(test.path)
			if err != nil {
				t.Fatalf("ClassifyPath(%q) error = %v", test.path, err)
			}
			if !slices.Contains(classification.CheckSets, test.check) {
				t.Errorf("ClassifyPath(%q) checks = %v, missing %q", test.path, classification.CheckSets, test.check)
			}
			if len(classification.RiskDomains) != 0 {
				t.Errorf("ClassifyPath(%q) test-only risk domains = %v, want none", test.path, classification.RiskDomains)
			}
		})
	}
}

func TestPolicyTestOnlyCleanupContractsAddNoRiskDomains(t *testing.T) {
	policy := loadRepositoryPolicy(t)
	for _, test := range []struct {
		path  string
		check string
	}{
		{"tests/lifecycle-guard.sh", "shell:lifecycle-guard"},
		{"tests/teardown-runtime-preservation.sh", "shell:teardown-runtime-preservation"},
	} {
		classification, err := policy.ClassifyPath(test.path)
		if err != nil {
			t.Fatalf("ClassifyPath(%q) error = %v", test.path, err)
		}
		if !slices.Contains(classification.CheckSets, test.check) {
			t.Errorf("ClassifyPath(%q) checks = %v, missing %q", test.path, classification.CheckSets, test.check)
		}
		if len(classification.RiskDomains) != 0 {
			t.Errorf("ClassifyPath(%q) test-only risk domains = %v, want none", test.path, classification.RiskDomains)
		}
	}
}

func TestPolicyCoLocatedGoTestsRetainOwningChecksWithoutRiskDomains(t *testing.T) {
	policy := loadRepositoryPolicy(t)
	for _, test := range []struct {
		path  string
		check string
	}{
		{path: "internal/cli/cli_test.go", check: "go:cli"},
		{path: "internal/configsync/configsync_test.go", check: "go:configsync"},
		{path: "internal/adapters/transport/process_test.go", check: "go:adapters/transport"},
		{path: "internal/migration/power_reconciler_test.go", check: "p0:power-systemd"},
	} {
		t.Run(test.path, func(t *testing.T) {
			classification, err := policy.ClassifyPath(test.path)
			if err != nil {
				t.Fatalf("ClassifyPath(%q) error = %v", test.path, err)
			}
			if !slices.Contains(classification.CheckSets, test.check) {
				t.Errorf("ClassifyPath(%q) checks = %v, missing owning check %q", test.path, classification.CheckSets, test.check)
			}
			if len(classification.RiskDomains) != 0 {
				t.Errorf("ClassifyPath(%q) test-only risk domains = %v, want none", test.path, classification.RiskDomains)
			}
		})
	}
}

func TestPathCoverageClassifiesEveryPublicRepositoryPath(t *testing.T) {
	repo := repositoryRoot(t)
	registry, err := BuiltInRegistry()
	if err != nil {
		t.Fatalf("BuiltInRegistry() error = %v", err)
	}
	mapFile, err := os.Open(filepath.Join(repo, "tests/impact-map.json"))
	if err != nil {
		t.Fatalf("Open(impact-map.json) error = %v", err)
	}
	t.Cleanup(func() { _ = mapFile.Close() })
	policy, err := LoadPolicy(mapFile, registry)
	if err != nil {
		t.Fatalf("LoadPolicy(impact-map.json) error = %v", err)
	}

	command := exec.Command("git", "-c", "safe.directory="+repo, "-C", repo,
		"ls-files", "--cached", "--others", "--exclude-standard", "-z")
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_OPTIONAL_LOCKS=0",
	}
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git ls-files error = %v", err)
	}
	paths := bytes.Split(bytes.TrimSuffix(output, []byte{0}), []byte{0})
	if len(paths) == 0 {
		t.Fatal("git ls-files returned no public paths")
	}
	for _, rawPath := range paths {
		path := string(rawPath)
		classification, err := policy.ClassifyPath(path)
		if err != nil {
			t.Errorf("ClassifyPath(%q) error = %v", path, err)
			continue
		}
		if len(classification.RuleIDs) == 0 {
			t.Errorf("public path %q has no explicit impact-map rule", path)
		}
	}
}

func assertRegistryCheck(t *testing.T, registry Registry, want Check) {
	t.Helper()
	got, ok := registry.Check(want.ID)
	if !ok {
		t.Errorf("registry is missing %s", want.ID)
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("registry check %s = %#v, want %#v", want.ID, got, want)
	}
}

func loadRepositoryPolicy(t *testing.T) Policy {
	t.Helper()
	registry, err := BuiltInRegistry()
	if err != nil {
		t.Fatalf("BuiltInRegistry() error = %v", err)
	}
	mapFile, err := os.Open(filepath.Join(repositoryRoot(t), "tests/impact-map.json"))
	if err != nil {
		t.Fatalf("Open(impact-map.json) error = %v", err)
	}
	t.Cleanup(func() { _ = mapFile.Close() })
	policy, err := LoadPolicy(mapFile, registry)
	if err != nil {
		t.Fatalf("LoadPolicy(impact-map.json) error = %v", err)
	}
	return policy
}

func assertContainsAll(t *testing.T, got, want []string) {
	t.Helper()
	for _, value := range want {
		if !slices.Contains(got, value) {
			t.Errorf("values = %v, missing %q", got, value)
		}
	}
}

func checkIDs(checks []Check) []string {
	ids := make([]string, 0, len(checks))
	for _, check := range checks {
		ids = append(ids, check.ID)
	}
	sort.Strings(ids)
	return ids
}

func expectedShellIDs(filenames []string) []string {
	ids := map[string]struct{}{"shell:test-impact": {}}
	for _, filename := range filenames {
		if strings.HasSuffix(filename, ".sh") && filename != "run.sh" {
			ids["shell:"+strings.TrimSuffix(filename, ".sh")] = struct{}{}
		}
	}
	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(current), "../.."))
}

func assertNoExecutableJSONFields(t *testing.T, value any) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			switch strings.ToLower(key) {
			case "argv", "args", "command", "commands", "executable", "working_directory":
				t.Errorf("impact-map.json contains executable field %q", key)
			}
			assertNoExecutableJSONFields(t, nested)
		}
	case []any:
		for _, nested := range typed {
			assertNoExecutableJSONFields(t, nested)
		}
	}
}

func policyDocument(overrides string) string {
	fields := map[string]string{
		"schema_version":                 `1`,
		"check_sets":                     `{"base":["host-free:core"]}`,
		"risk_domains":                   `["alpha","beta"]`,
		"rules":                          `[{"id":"root","priority":1,"pattern":"^.*$","check_sets":["base"],"risk_domains":[]}]`,
		"risk_suppressions":              `[]`,
		"standalone_full_domains":        `[]`,
		"high_risk_combinations":         `[]`,
		"default_multi_domain_full":      `true`,
		"safe_multi_domain_combinations": `[]`,
	}
	if strings.TrimSpace(overrides) != "" {
		var replacement map[string]json.RawMessage
		if err := json.Unmarshal([]byte("{"+overrides+"}"), &replacement); err != nil {
			panic(fmt.Sprintf("invalid policy test override: %v", err))
		}
		for key, value := range replacement {
			fields[key] = string(value)
		}
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	members := make([]string, 0, len(keys))
	for _, key := range keys {
		members = append(members, fmt.Sprintf("%q:%s", key, fields[key]))
	}
	return "{" + strings.Join(members, ",") + "}"
}
