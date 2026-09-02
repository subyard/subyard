package testimpact

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
)

// Check is one immutable check definition. Leaf checks have Argv; composites
// have Members. Declarative policy refers only to ID.
type Check struct {
	ID               string
	Tier             string
	Argv             []string
	WorkingDirectory string
	Members          []string
	BudgetSeconds    int
	Rationale        string
}

// Registry is a validated immutable collection of executable check metadata.
type Registry struct {
	checks map[string]Check
}

// BuiltInRegistry returns Subyard's compiled check registry.
func BuiltInRegistry() (Registry, error) {
	checks := []Check{
		{
			ID:            "host-free:core",
			Tier:          "T2",
			Argv:          []string{"./tests/run.sh"},
			BudgetSeconds: 1800,
			Rationale:     "required core host-free merge gate",
		},
		{
			ID:            "host-free:all",
			Tier:          "T2",
			Members:       []string{"host-free:core", "veranda:build", "veranda:check", "veranda:rust-test", "veranda:test"},
			BudgetSeconds: 2700,
			Rationale:     "universal host-free fallback including Veranda",
		},
	}

	goPackages := []string{
		"application", "audit", "cli", "command", "config", "configsync", "credential",
		"domain", "migration", "ownerinventory", "ports", "releasetransition", "resource", "rpc", "shellquote",
		"sshidentity", "sshrelay", "state", "systemdunit", "testyardmigration",
		"adapters/credentialmeta", "adapters/credentialruntime", "adapters/hostruntime",
		"adapters/incusclient", "adapters/projectruntime", "adapters/reconcileruntime",
		"adapters/releaseruntime", "adapters/remotecontrol", "adapters/securityruntime",
		"adapters/shelladapter", "adapters/statusruntime", "adapters/testvmsruntime",
		"adapters/transport",
		"architecture", "contracttest", "testkit",
	}
	for _, packageName := range goPackages {
		checks = append(checks, Check{
			ID:            "go:" + packageName,
			Tier:          "T1",
			Argv:          []string{"go", "test", "-race", "./internal/" + packageName},
			BudgetSeconds: 180,
			Rationale:     "race-test the owning Go package",
		})
	}
	checks = append(checks,
		Check{ID: "go:testimpact", Tier: "T1", Argv: []string{"go", "test", "-race", "./internal/testimpact"}, BudgetSeconds: 180, Rationale: "selector package self-check"},
		Check{ID: "go:test-impact-cli", Tier: "T1", Argv: []string{"go", "test", "./cmd/test-impact"}, BudgetSeconds: 180, Rationale: "selector CLI self-check"},
	)

	shellTests := []string{
		"agent-e2e", "agent-selection", "android-provision-check", "build-engine",
		"ccusage-provision", "cli-contract", "codex-agent-defaults", "codex-agent-provision",
		"command-registry", "create-subyard-docker-apparmor", "docker-forwarding-convergence",
		"emulator-process-control",
		"emulator-process-identity", "emulator-resource-protocol", "engine-release",
		"hermes-dashboard-resource", "hermes-e2e-contract", "hermes-provision",
		"init-extras-convergence", "init-network-convergence", "init-project-convergence",
		"install-incus-data-home", "install-runtime-release-rollback", "key-tools-install", "lib-power-network",
		"lifecycle-guard", "openclaw-provision-check", "openclaw-resource-protocol",
		"opencode-agent-defaults", "opencode-agent-provision", "orca-profile-resource",
		"p0-capacity", "paseo-agent-contract", "paseo-project-sync",
		"power-reconciler-systemd-255-launch", "power-reconciler-systemd",
		"profile-resource-lifecycle", "project-registry-convergence",
		"prompt-contract", "provision-profile-check", "remote-projects",
		"runtime-privilege-reexec", "ssh-config", "ssh-transport-identity",
		"subyard-dev-provision", "teardown-runtime-preservation", "test-vms",
		"vscode-remote-maintenance", "workflow-real-adapter-gate", "yard-extras-convergence",
		"yard-keys", "yard-remote", "yard-shell", "yard-usage", "zabbly-download",
		"test-impact",
	}
	for _, name := range shellTests {
		checks = append(checks, Check{
			ID:            "shell:" + name,
			Tier:          "T1",
			Argv:          []string{"bash", "tests/" + name + ".sh"},
			BudgetSeconds: 180,
			Rationale:     "host-free shell contract",
		})
	}
	for index := range checks {
		switch checks[index].ID {
		case "shell:test-impact":
			checks[index].Rationale = "selector wrapper and integration contract"
		}
	}

	checks = append(checks,
		Check{ID: "veranda:test", Tier: "T1", Argv: []string{"npm", "run", "test"}, WorkingDirectory: "veranda", BudgetSeconds: 180, Rationale: "Veranda unit tests"},
		Check{ID: "veranda:check", Tier: "T1", Argv: []string{"npm", "run", "check"}, WorkingDirectory: "veranda", BudgetSeconds: 180, Rationale: "Veranda static checks"},
		Check{ID: "veranda:build", Tier: "T1", Argv: []string{"npm", "run", "build"}, WorkingDirectory: "veranda", BudgetSeconds: 180, Rationale: "Veranda production build"},
		Check{ID: "veranda:rust-test", Tier: "T1", Argv: []string{"cargo", "test", "--manifest-path", "veranda/src-tauri/Cargo.toml", "--no-default-features"}, BudgetSeconds: 300, Rationale: "Veranda Rust tests without desktop dependencies"},
	)

	p0Lanes := []string{
		"boundary", "nested-teardown", "transport", "dependencies", "real-incus",
		"profile-resource", "release", "source-upgrade", "power-systemd", "reboot-verify",
		"peer", "peer-cleanup", "cleanup",
	}
	for _, lane := range p0Lanes {
		checks = append(checks, Check{
			ID:            "p0:" + lane,
			Tier:          "T3",
			Argv:          []string{"dev/e2e/p0-acceptance.sh", "--lane", lane},
			BudgetSeconds: 1800,
			Rationale:     "targeted real-host acceptance lane",
		})
	}

	preparedAdapterChecks := []string{
		"adapter-contracts", "credential-tools", "ssh-credential-peer", "ssh-rpc",
	}
	for _, name := range preparedAdapterChecks {
		argv := []string{
			"dev/agent-e2e.sh", "--purpose", name, "--vm", "1", "--",
			"bash", "tests/real-host/adapter-contracts.sh",
		}
		rationale := "prepared real crypto and OpenSSH adapter contract"
		if name != "adapter-contracts" {
			argv = append(argv, "--check", name)
			rationale = "prepared targeted real-host adapter contract"
		}
		checks = append(checks, Check{
			ID:            "e2e:" + name,
			Tier:          "T3",
			Argv:          argv,
			BudgetSeconds: 900,
			Rationale:     rationale,
		})
	}
	checks = append(checks,
		Check{
			ID:   "e2e:hermes-profile",
			Tier: "T3",
			Argv: []string{
				"dev/agent-e2e.sh", "--purpose", "hermes-profile", "--vm", "both", "--",
				"./dev/e2e/hermes-profile.sh",
			},
			BudgetSeconds: 1800,
			Rationale:     "documented two-VM Hermes substrate and persistence acceptance",
		},
		Check{
			ID:   "e2e:orca-resource",
			Tier: "T3",
			Argv: []string{
				"dev/agent-e2e.sh", "--purpose", "orca-resource", "--vm", "1", "--",
				"env", "SUBYARD_E2E_ORCA_RESOURCE=1", "./tests/real-host/orca-resource.sh",
			},
			BudgetSeconds: 1800,
			Rationale:     "opt-in Orca resource acceptance on a disposable leased VM",
		},
	)

	return ValidateRegistry(checks)
}

// ValidateRegistry validates definitions and returns an immutable registry.
func ValidateRegistry(checks []Check) (Registry, error) {
	if len(checks) == 0 {
		return Registry{}, errors.New("registry is empty")
	}
	validated := Registry{checks: make(map[string]Check, len(checks))}
	for index, source := range checks {
		check := cloneCheck(source)
		if check.ID == "" || strings.TrimSpace(check.ID) != check.ID || strings.ContainsAny(check.ID, "\x00\r\n\t ") {
			return Registry{}, fmt.Errorf("check[%d] has invalid ID", index)
		}
		if _, exists := validated.checks[check.ID]; exists {
			return Registry{}, fmt.Errorf("duplicate check ID %q", check.ID)
		}
		switch check.Tier {
		case "T1", "T2", "T3":
		default:
			return Registry{}, fmt.Errorf("check %q has invalid tier %q", check.ID, check.Tier)
		}
		if check.BudgetSeconds <= 0 {
			return Registry{}, fmt.Errorf("check %q must have a positive budget_seconds", check.ID)
		}
		if strings.TrimSpace(check.Rationale) == "" {
			return Registry{}, fmt.Errorf("check %q must have a rationale", check.ID)
		}
		if check.WorkingDirectory != "" {
			if path.IsAbs(check.WorkingDirectory) || path.Clean(check.WorkingDirectory) != check.WorkingDirectory || strings.HasPrefix(check.WorkingDirectory, "../") {
				return Registry{}, fmt.Errorf("check %q has invalid working directory", check.ID)
			}
		}
		if len(check.Members) == 0 {
			if len(check.Argv) == 0 {
				return Registry{}, fmt.Errorf("leaf check %q has no argv", check.ID)
			}
			for _, argument := range check.Argv {
				if argument == "" || strings.IndexByte(argument, 0) >= 0 {
					return Registry{}, fmt.Errorf("check %q has invalid argv", check.ID)
				}
			}
		} else {
			if len(check.Argv) != 0 || check.WorkingDirectory != "" {
				return Registry{}, fmt.Errorf("composite check %q cannot be executable", check.ID)
			}
			if !uniqueStrings(check.Members) {
				return Registry{}, fmt.Errorf("composite check %q has duplicate members", check.ID)
			}
			sort.Strings(check.Members)
		}
		validated.checks[check.ID] = check
	}

	for id, check := range validated.checks {
		for _, member := range check.Members {
			if _, exists := validated.checks[member]; !exists {
				return Registry{}, fmt.Errorf("composite check %q has unknown member %q", id, member)
			}
		}
	}
	for id := range validated.checks {
		if err := validated.detectCycle(id, make(map[string]bool)); err != nil {
			return Registry{}, err
		}
	}
	return validated, nil
}

// IDs returns all registered IDs in stable order.
func (registry Registry) IDs() []string {
	ids := make([]string, 0, len(registry.checks))
	for id := range registry.checks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Check returns a defensive copy of a registered check.
func (registry Registry) Check(id string) (Check, bool) {
	check, exists := registry.checks[id]
	return cloneCheck(check), exists
}

// Expand recursively expands IDs into a sorted, de-duplicated leaf plan.
func (registry Registry) Expand(ids []string) ([]Check, error) {
	leaves := make(map[string]Check)
	var visit func(string) error
	visit = func(id string) error {
		check, exists := registry.checks[id]
		if !exists {
			return fmt.Errorf("unknown check ID %q", id)
		}
		if len(check.Members) == 0 {
			leaves[id] = check
			return nil
		}
		for _, member := range check.Members {
			if err := visit(member); err != nil {
				return err
			}
		}
		return nil
	}
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	result := make([]Check, 0, len(leaves))
	for _, check := range leaves {
		result = append(result, cloneCheck(check))
	}
	slices.SortFunc(result, func(left, right Check) int { return strings.Compare(left.ID, right.ID) })
	return result, nil
}

func (registry Registry) detectCycle(id string, visiting map[string]bool) error {
	if visiting[id] {
		return fmt.Errorf("registry composite cycle through %q", id)
	}
	check := registry.checks[id]
	if len(check.Members) == 0 {
		return nil
	}
	visiting[id] = true
	for _, member := range check.Members {
		if err := registry.detectCycle(member, visiting); err != nil {
			return err
		}
	}
	delete(visiting, id)
	return nil
}

func cloneCheck(check Check) Check {
	check.Argv = slices.Clone(check.Argv)
	check.Members = slices.Clone(check.Members)
	return check
}

func uniqueStrings(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}
