package architecture

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/command"
)

func TestProductionShellIsReachableAndLeafOnly(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	for _, retired := range []string{
		"dev/install-cli.sh",
		"scripts/lib/cache.sh", "scripts/state/transport.sh", "scripts/yard-boot-reconcile.sh",
		"scripts/sy-stage.sh", "scripts/build-engine.sh", "scripts/package-engine.sh",
		"scripts/bootstrap-runtime.sh", "scripts/install-cli.sh",
		"scripts/08-git-identity.sh", "scripts/agent-configs.sh",
		"scripts/status-probe.sh", "scripts/lib/env.sh",
		"tests/legacy-source-install.sh", "tests/real-host/installed-cli.sh",
	} {
		if _, err := os.Lstat(filepath.Join(root, retired)); err == nil {
			t.Errorf("retired shell path returned: %s", retired)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	manifestFile, err := os.Open(filepath.Join(root, "config", "commands.registry"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, parseErr := command.Parse(manifestFile)
	closeErr := manifestFile.Close()
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	definitions := manifest.Commands()
	allowedHandlers := map[string]bool{}
	handlers := make(map[string]bool)
	for _, definition := range definitions {
		if strings.HasPrefix(definition.Handler, "@") {
			for _, candidate := range []string{
				definition.Name + ".sh", "yard-" + definition.Name + ".sh",
				strings.TrimPrefix(definition.Handler, "@") + ".sh",
				"yard-" + strings.TrimPrefix(definition.Handler, "@") + ".sh",
			} {
				if _, err := os.Lstat(filepath.Join(root, "scripts", candidate)); err == nil {
					t.Errorf("native command keeps a replaced shell path: scripts/%s", candidate)
				}
			}
			continue
		}
		handlers[definition.Handler] = true
		path := filepath.Join(root, "scripts", definition.Handler)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			t.Errorf("manifest handler is unavailable: scripts/%s", definition.Handler)
		}
		if !allowedHandlers[definition.Handler] {
			t.Errorf("core registry uses a non-leaf shell handler: scripts/%s", definition.Handler)
		}
	}

	contracts := productionShellContracts()
	leafContracts := productionLeafContracts()
	actual := append(shellFiles(t, filepath.Join(root, "scripts")),
		shellFiles(t, filepath.Join(root, "config", "profiles"))...)
	actual = append(actual, shellFiles(t, filepath.Join(root, "config", "agents"))...)
	for _, script := range actual {
		path, err := filepath.Rel(root, script)
		if err != nil {
			t.Fatal(err)
		}
		path = filepath.ToSlash(path)
		contract, ok := contracts[path]
		if !ok {
			t.Errorf("production shell has no explicit owner contract: %s", path)
			continue
		}
		delete(contracts, path)
		if contract.kind != "leaf" && contract.kind != "library" &&
			contract.kind != "embedded" && contract.kind != "profile" &&
			contract.kind != "bootstrap" {
			t.Errorf("%s has invalid shell contract %q", path, contract.kind)
		}
		owner, err := os.ReadFile(filepath.Join(root, contract.owner))
		if err != nil {
			t.Errorf("%s owner %s is unavailable: %v", path, contract.owner, err)
			continue
		}
		if !strings.Contains(string(owner), contract.reference) {
			t.Errorf("%s is no longer called by %s through %q", path, contract.owner, contract.reference)
		}
		if contract.kind == "leaf" {
			leaf, ok := leafContracts[path]
			if !ok {
				t.Errorf("physical leaf has no semantic owner contract: %s", path)
				continue
			}
			delete(leafContracts, path)
			if !strings.HasSuffix(contract.owner, ".go") {
				t.Errorf("physical leaf %s has no Go owner: %s", path, contract.owner)
			}
			if leaf.kind == "reconcile" {
				for _, marker := range []string{
					"func (runtime Runtime) CheckStage",
					"func (runtime Runtime) ApplyStage",
					"func (runtime Runtime) VerifyStage",
					leaf.name,
				} {
					if !strings.Contains(string(owner), marker) {
						t.Errorf("%s lacks host-free check/apply/verify ownership in %s through %q",
							path, contract.owner, marker)
					}
				}
			}
		}
	}
	for path := range contracts {
		t.Errorf("shell owner contract outlived its file: %s", path)
	}
	for path := range leafContracts {
		t.Errorf("semantic leaf owner contract outlived its file: %s", path)
	}
}

func TestCriticalShellCallerGraphIsExact(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	expected := map[string][]string{
		"install-power-reconciler.sh": {
			"internal/adapters/reconcileruntime/runtime.go",
			"scripts/teardown-physical.sh",
		},
		"lifecycle-guard.sh": {
			"internal/adapters/reconcileruntime/runtime.go",
			"internal/cli/cli.go",
			"scripts/03-create-subyard.sh",
		},
		"teardown-physical.sh": {
			"internal/adapters/reconcileruntime/runtime.go",
			"internal/cli/cli.go",
		},
	}

	sources := sourceFiles(t, []string{
		filepath.Join(root, "cmd"),
		filepath.Join(root, "config"),
		filepath.Join(root, "internal"),
		filepath.Join(root, "scripts"),
	}, func(path string) bool {
		return !strings.HasSuffix(path, "_test.go") &&
			(strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".sh") ||
				strings.HasSuffix(path, ".env") || strings.HasSuffix(path, ".registry") ||
				strings.HasSuffix(path, ".in"))
	})

	for script, want := range expected {
		var got []string
		for _, source := range sources {
			relative, err := filepath.Rel(root, source)
			if err != nil {
				t.Fatal(err)
			}
			relative = filepath.ToSlash(relative)
			if strings.HasSuffix(relative, "/"+script) || relative == script {
				continue
			}
			contents, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(contents), "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || strings.HasPrefix(trimmed, "#") ||
					strings.HasPrefix(trimmed, "//") {
					continue
				}
				if strings.Contains(line, script) {
					got = append(got, relative)
					break
				}
			}
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s caller graph changed:\n got: %q\nwant: %q", script, got, want)
		}
	}
}

func TestShellTestsStayOutsideProductionTrees(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	for _, directory := range []string{"scripts", "config", "dev"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.HasPrefix(entry.Name(), "test-") &&
				strings.HasSuffix(entry.Name(), ".sh") {
				t.Errorf("shell test must live under tests/: %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPhysicalShellConsumesOnlyPreparedControlPlaneState(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	allowedSources := map[string]bool{
		"runtime.sh": true, "engine-context.sh": true, "ui.sh": true,
		"lib-power.sh": true, "host.sh": true, "ssh-config.sh": true,
	}
	forbidden := []string{
		"SUBYARD_CONFIG_LOADED", "SUBYARD_PROFILES_DIR", "YARD_TEMPLATE", "OWNER_ENDPOINT",
		"/config/yards", "resolve_project", "route_sync_target", "state_engine",
		"stage_registry", "source_control_plane",
	}
	sourcePattern := regexp.MustCompile(`(?m)^[[:space:]]*\.[[:space:]]+["']?([^"';[:space:]]+)`)
	for path, contract := range productionShellContracts() {
		if contract.kind != "leaf" {
			continue
		}
		if !strings.HasSuffix(contract.owner, ".go") {
			t.Errorf("physical leaf %s has no Go owner: %s", path, contract.owner)
		}
		payload, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		text := uncommentShell(string(payload))
		if !strings.Contains(text, "subyard_require_engine_context") {
			t.Errorf("physical leaf does not fail closed without typed engine context: %s", path)
		}
		for _, token := range forbidden {
			if strings.Contains(text, token) {
				t.Errorf("physical leaf %s regained control-plane semantic %q", path, token)
			}
		}
		for _, match := range sourcePattern.FindAllStringSubmatch(text, -1) {
			target := filepath.Base(match[1])
			if !allowedSources[target] {
				t.Errorf("physical leaf %s sources non-boundary runtime %q", path, match[1])
			}
		}
	}
}

func uncommentShell(value string) string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func TestProjectStateAndRoutingStayGoOwned(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	for _, retired := range []string{
		"scripts/state/store.sh",
		"scripts/state/resolver.sh",
		"scripts/state/transport.sh",
		"scripts/project-clone.sh",
		"scripts/project-remove.sh",
		"scripts/project-sync.sh",
		"scripts/project-code.sh",
		"scripts/project-export.sh",
		"scripts/lib/project-snapshot.sh",
		"scripts/state/metadata.sh",
	} {
		if _, err := os.Lstat(filepath.Join(root, retired)); err == nil {
			t.Errorf("retired project state or routing shim returned: %s", retired)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	for _, relative := range []string{"scripts/09-yard-extras.sh"} {
		payload, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			"state_engine", "state_get", "state_write", "state_set", "state_remove",
			"state_exists", "state_ids", "state_validate", "resolve_project",
			"route_sync_target", "maybe_reconcile", "_project-state",
		} {
			if strings.Contains(string(payload), forbidden) {
				t.Errorf("Go-owned project state or routing returned to %s through %q",
					relative, forbidden)
			}
		}
	}
}

type shellContract struct {
	kind      string
	owner     string
	reference string
}

func productionShellContracts() map[string]shellContract {
	goReconcile := "internal/adapters/reconcileruntime/runtime.go"
	goCLI := "internal/cli/cli.go"
	return map[string]shellContract{
		"config/agents/ccusage/provision.sh":  {"profile", "config/agents.env", `agents/ccusage/provision.sh`},
		"config/agents/codex/provision.sh":    {"profile", "config/agents.env", `agents/codex/provision.sh`},
		"config/agents/opencode/provision.sh": {"profile", "config/agents.env", `agents/opencode/provision.sh`},
		"config/agents/paseo/provision.sh":    {"profile", "config/agents.env", `agents/paseo/provision.sh`},
		"scripts/01-install-incus.sh":         {"leaf", goReconcile, `"01-install-incus.sh"`},
		"scripts/02-create-project.sh":        {"leaf", goReconcile, `"02-create-project.sh"`},
		"scripts/03-create-subyard.sh":        {"leaf", goReconcile, `"03-create-subyard.sh"`},
		"scripts/04-provision-subyard.sh":     {"leaf", goReconcile, `"04-provision-subyard.sh"`},
		"scripts/05-mount-host-paths.sh":      {"leaf", goReconcile, `"05-mount-host-paths.sh"`},
		"scripts/06-network.sh":               {"leaf", goReconcile, `"06-network.sh"`},
		"scripts/07-ssh-access.sh":            {"leaf", goReconcile, `"07-ssh-access.sh"`},
		"scripts/09-yard-extras.sh":           {"leaf", goReconcile, `"09-yard-extras.sh"`},
		"scripts/e2e-lab/invoke.sh":           {"leaf", goCLI, `e2e-lab/invoke.sh`},
		"scripts/e2e-lab/provision.sh":        {"embedded", "internal/adapters/testvmsruntime/backend.go", `"provision.sh"`},
		"scripts/install-key-tools.sh":        {"leaf", goReconcile, `"install-key-tools.sh"`},
		"scripts/install-keys-auto-sync.sh":   {"leaf", goReconcile, `"install-keys-auto-sync.sh"`},
		"scripts/install-power-reconciler.sh": {"leaf", goReconcile, `"install-power-reconciler.sh"`},
		"scripts/install-ssh-relay.sh":        {"embedded", "scripts/07-ssh-access.sh", `install-ssh-relay.sh`},
		"scripts/install-test-vms-host-sink.sh": {
			"leaf", goReconcile, `"install-test-vms-host-sink.sh"`,
		},
		"scripts/install-runtime-release.sh":   {"bootstrap", "internal/cli/update.go", `"install-runtime-release.sh"`},
		"scripts/migrate-source-install.sh":    {"bootstrap", "dev/bootstrap-runtime.sh", `migrate-source-install.sh`},
		"scripts/restore-source-install.sh":    {"embedded", "scripts/migrate-source-install.sh", `restore-source-install.sh`},
		"scripts/lib-power.sh":                 {"library", "scripts/lifecycle-guard.sh", `lib-power.sh`},
		"scripts/lib-service.sh":               {"library", "config/profiles/android/resources/emulator/handler.sh", `lib-service.sh`},
		"scripts/lib/engine-context.sh":        {"library", "scripts/01-install-incus.sh", `lib/engine-context.sh`},
		"scripts/lib/download.sh":              {"library", "scripts/lib/host.sh", `lib/download.sh`},
		"scripts/lib/host.sh":                  {"library", "scripts/01-install-incus.sh", `lib/host.sh`},
		"scripts/lib/runtime.sh":               {"library", "scripts/01-install-incus.sh", `lib/runtime.sh`},
		"scripts/lib/ssh-config.sh":            {"library", "scripts/07-ssh-access.sh", `lib/ssh-config.sh`},
		"scripts/lib/ui.sh":                    {"library", "scripts/01-install-incus.sh", `lib/ui.sh`},
		"scripts/lifecycle-guard.sh":           {"leaf", goCLI, `"lifecycle-guard.sh"`},
		"scripts/provision-profile.sh":         {"profile", goCLI, `"scripts/provision-profile.sh"`},
		"scripts/teardown-physical.sh":         {"leaf", goCLI, `"scripts/teardown-physical.sh"`},
		"scripts/vscode-remote-maintenance.sh": {"embedded", "scripts/lifecycle-guard.sh", `vscode-remote-maintenance.sh`},

		"config/profiles/android/emulator-control.sh":                    {"profile", "config/profiles/android/resources/emulator/handler.sh", `emulator-control.sh`},
		"config/profiles/android/emulator-run.sh":                        {"profile", "config/profiles/android/resources/emulator/handler.sh", `emulator-run.sh`},
		"config/profiles/android/provision.sh":                           {"profile", "internal/cli/provision.go", `"provision.sh"`},
		"config/profiles/android/resources/emulator/handler.sh":          {"profile", "config/profiles/android/resources/emulator.res", `HANDLER=resources/emulator/handler.sh`},
		"config/profiles/android/resources/emulator/process-identity.sh": {"library", "config/profiles/android/resources/emulator/handler.sh", `process-identity.sh`},
		"config/profiles/hermes/provision.sh":                            {"profile", "internal/cli/provision.go", `"provision.sh"`},
		"config/profiles/hermes/resources/dashboard/handler.sh":          {"profile", "config/profiles/hermes/resources/dashboard.res", `HANDLER=resources/dashboard/handler.sh`},
		"config/profiles/openclaw/provision.sh":                          {"profile", "internal/cli/provision.go", `"provision.sh"`},
		"config/profiles/openclaw/resources/qa-bot-broker/handler.sh":    {"profile", "config/profiles/openclaw/resources/qa-bot-broker.res", `HANDLER=resources/qa-bot-broker/handler.sh`},
		"config/profiles/openclaw/resources/staging-gateway/handler.sh":  {"profile", "config/profiles/openclaw/resources/staging-gateway.res", `HANDLER=resources/staging-gateway/handler.sh`},
		"config/profiles/openclaw/resources/staging-gateway/sy-stage.sh": {"embedded", "config/profiles/openclaw/resources/staging-gateway/handler.sh", `sy-stage.sh`},
		"config/profiles/orca/resources/orca/handler.sh":                 {"profile", "config/profiles/orca/resources/orca.res", `HANDLER=resources/orca/handler.sh`},
		"config/profiles/subyard-dev/provision.sh":                       {"profile", "internal/cli/provision.go", `"provision.sh"`},
	}
}

type leafContract struct {
	kind string
	name string
}

func productionLeafContracts() map[string]leafContract {
	return map[string]leafContract{
		"scripts/01-install-incus.sh":         {"reconcile", "ports.ReconcileStageIncus"},
		"scripts/02-create-project.sh":        {"reconcile", "ports.ReconcileStageProject"},
		"scripts/03-create-subyard.sh":        {"reconcile", "ports.ReconcileStageInstance"},
		"scripts/04-provision-subyard.sh":     {"reconcile", "ports.ReconcileStageProvision"},
		"scripts/05-mount-host-paths.sh":      {"reconcile", "ports.ReconcileStageMounts"},
		"scripts/06-network.sh":               {"reconcile", "ports.ReconcileStageNetwork"},
		"scripts/07-ssh-access.sh":            {"reconcile", "ports.ReconcileStageSSH"},
		"scripts/09-yard-extras.sh":           {"reconcile", "ports.ReconcileStageExtras"},
		"scripts/install-key-tools.sh":        {"reconcile", "ports.ReconcileStageKeys"},
		"scripts/install-keys-auto-sync.sh":   {"reconcile", "ports.ReconcileStageKeys"},
		"scripts/install-power-reconciler.sh": {"reconcile", "ports.ReconcileStagePower"},
		"scripts/install-test-vms-host-sink.sh": {
			"reconcile", "ports.ReconcileStageTestVMs",
		},
		"scripts/e2e-lab/invoke.sh":    {"command", "test-vms"},
		"scripts/lifecycle-guard.sh":   {"command", "lifecycle"},
		"scripts/teardown-physical.sh": {"command", "teardown"},
	}
}

func shellFiles(t *testing.T, root string) []string {
	t.Helper()
	return sourceFiles(t, []string{root}, func(path string) bool { return strings.HasSuffix(path, ".sh") })
}

func sourceFiles(t *testing.T, roots []string, include func(string) bool) []string {
	t.Helper()
	var result []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !entry.IsDir() && entry.Type().IsRegular() && include(path) {
				result = append(result, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}
