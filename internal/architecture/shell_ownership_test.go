package architecture

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/profile"
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

	contracts := productionShellContracts(t)
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
			"internal/cli/prepared_command.go",
			"scripts/03-create-subyard.sh",
		},
		"teardown-physical.sh": {
			"internal/adapters/reconcileruntime/runtime.go",
			"internal/cli/prepared_command.go",
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
	productionCommands := map[string]bool{
		filepath.Join(root, "dev", "test-impact.sh"):   true,
		filepath.Join(root, "dev", "test-profiles.sh"): true,
	}
	for _, directory := range []string{"scripts", "config", "dev"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && profileTestPath(path) {
				return filepath.SkipDir
			}
			if productionCommands[path] {
				return nil
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
		"ai-observer-proxy.sh":     true,
		"preview-proxy.sh":         true,
		"lib-vm-page-reporting.sh": true, "lib-vm-storage.sh": true,
	}
	forbidden := []string{
		"SUBYARD_CONFIG_LOADED", "SUBYARD_PROFILES_DIR", "YARD_TEMPLATE", "OWNER_ENDPOINT",
		"/config/yards", "resolve_project", "route_sync_target", "state_engine",
		"stage_registry", "source_control_plane",
	}
	sourcePattern := regexp.MustCompile(`(?m)^[[:space:]]*\.[[:space:]]+["']?([^"';[:space:]]+)`)
	for path, contract := range productionShellContracts(t) {
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

type profileShellOwnerManifest struct {
	SchemaVersion int                 `json:"schema_version"`
	Owners        []profileShellOwner `json:"owners"`
}

type profileShellOwner struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Owner     string `json:"owner"`
	Reference string `json:"reference"`
}

func productionShellContracts(t *testing.T) map[string]shellContract {
	goReconcile := "internal/adapters/reconcileruntime/runtime.go"
	goPrepared := "internal/cli/prepared_command.go"
	contracts := map[string]shellContract{
		"scripts/lib/ai-observer-proxy.sh":      {"library", "scripts/reconcile-integrations.sh", `lib/ai-observer-proxy.sh`},
		"scripts/lib/preview-proxy.sh":          {"library", "scripts/04-provision-subyard.sh", `lib/preview-proxy.sh`},
		"config/agents/aiobserver/provision.sh": {"profile", "config/agents.env", `agents/aiobserver/provision.sh`},
		"config/agents/ccusage/provision.sh":    {"profile", "config/agents.env", `agents/ccusage/provision.sh`},
		"config/agents/codex/provision.sh":      {"profile", "config/agents.env", `agents/codex/provision.sh`},
		"config/agents/opencode/provision.sh":   {"profile", "config/agents.env", `agents/opencode/provision.sh`},
		"config/agents/paseo/cleanup.sh":        {"profile", "config/agents.env", `agents/paseo/cleanup.sh`},
		"config/agents/paseo/provision.sh":      {"profile", "config/agents.env", `agents/paseo/provision.sh`},
		"scripts/01-install-incus.sh":           {"leaf", goReconcile, `"01-install-incus.sh"`},
		"scripts/02-create-project.sh":          {"leaf", goReconcile, `"02-create-project.sh"`},
		"scripts/03-create-subyard.sh":          {"leaf", goReconcile, `"03-create-subyard.sh"`},
		"scripts/04-provision-subyard.sh":       {"leaf", goReconcile, `"04-provision-subyard.sh"`},
		"scripts/reconcile-integrations.sh":     {"leaf", "internal/adapters/reconcileruntime/integrations.go", `"reconcile-integrations.sh"`},
		"scripts/05-mount-host-paths.sh":        {"leaf", goReconcile, `"05-mount-host-paths.sh"`},
		"scripts/06-network.sh":                 {"leaf", goReconcile, `"06-network.sh"`},
		"scripts/07-ssh-access.sh":              {"leaf", goReconcile, `"07-ssh-access.sh"`},
		"scripts/09-yard-extras.sh":             {"leaf", goReconcile, `"09-yard-extras.sh"`},
		"scripts/e2e-lab/invoke.sh":             {"leaf", goPrepared, `e2e-lab/invoke.sh`},
		"scripts/e2e-lab/base.sh":               {"embedded", "internal/adapters/testvmsruntime/images.go", `scripts/e2e-lab/base.sh`},
		"scripts/e2e-lab/provision.sh":          {"embedded", "internal/adapters/testvmsruntime/backend.go", `"provision.sh"`},
		"scripts/install-key-tools.sh":          {"leaf", goReconcile, `"install-key-tools.sh"`},
		"scripts/install-keys-auto-sync.sh":     {"leaf", goReconcile, `"install-keys-auto-sync.sh"`},
		"scripts/profile-services.sh":           {"leaf", goReconcile, `"profile-services.sh"`},
		"scripts/install-power-reconciler.sh":   {"leaf", goReconcile, `"install-power-reconciler.sh"`},
		"scripts/install-ssh-relay.sh":          {"embedded", "scripts/07-ssh-access.sh", `install-ssh-relay.sh`},
		"scripts/ssh-agent-environment.sh":      {"embedded", "internal/cli/ssh_agent.go", `"ssh-agent-environment.sh"`},
		"scripts/install-test-vms-host-sink.sh": {
			"leaf", goReconcile, `"install-test-vms-host-sink.sh"`,
		},
		"scripts/install-runtime-release.sh":   {"bootstrap", "internal/cli/update.go", `"install-runtime-release.sh"`},
		"scripts/migrate-source-install.sh":    {"bootstrap", "internal/migration/v2_source_ingress.go", `migrate-source-install.sh`},
		"scripts/restore-source-install.sh":    {"embedded", "scripts/migrate-source-install.sh", `restore-source-install.sh`},
		"scripts/lib-power.sh":                 {"library", "scripts/lifecycle-guard.sh", `lib-power.sh`},
		"scripts/lib-vm-page-reporting.sh":     {"library", "scripts/02-create-project.sh", `lib-vm-page-reporting.sh`},
		"scripts/lib-vm-storage.sh":            {"library", "scripts/03-create-subyard.sh", `lib-vm-storage.sh`},
		"scripts/lib/engine-context.sh":        {"library", "scripts/01-install-incus.sh", `lib/engine-context.sh`},
		"scripts/lib/download.sh":              {"library", "scripts/lib/host.sh", `lib/download.sh`},
		"scripts/lib/host.sh":                  {"library", "scripts/01-install-incus.sh", `lib/host.sh`},
		"scripts/lib/runtime.sh":               {"library", "scripts/01-install-incus.sh", `lib/runtime.sh`},
		"scripts/lib/ssh-config.sh":            {"library", "scripts/07-ssh-access.sh", `lib/ssh-config.sh`},
		"scripts/lib/ui.sh":                    {"library", "scripts/01-install-incus.sh", `lib/ui.sh`},
		"scripts/lifecycle-guard.sh":           {"leaf", goPrepared, `"lifecycle-guard.sh"`},
		"scripts/provision-profile.sh":         {"profile", goPrepared, `"scripts/provision-profile.sh"`},
		"scripts/teardown-physical.sh":         {"leaf", goPrepared, `"scripts/teardown-physical.sh"`},
		"scripts/vscode-remote-maintenance.sh": {"embedded", "scripts/lifecycle-guard.sh", `vscode-remote-maintenance.sh`},
	}
	root := filepath.Join("..", "..")
	definitions, err := profile.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		declaration := filepath.ToSlash(filepath.Join("config", "profiles", definition.Name, "profile.json"))
		if definition.Runtime != nil {
			handler := filepath.ToSlash(filepath.Join("config", "profiles", definition.Name, definition.Runtime.Handler))
			contracts[handler] = shellContract{"profile", declaration, definition.Runtime.Handler}
		}
		if definition.GuestEnvironment != nil {
			handler := filepath.ToSlash(filepath.Join("config", "profiles", definition.Name, definition.GuestEnvironment.Handler))
			contracts[handler] = shellContract{"profile", declaration, definition.GuestEnvironment.Handler}
		}
		for _, consumer := range definition.Consumers {
			if consumer.StopHandler != "" {
				handler := filepath.ToSlash(filepath.Join("config", "profiles", definition.Name, consumer.StopHandler))
				contracts[handler] = shellContract{"profile", declaration, consumer.StopHandler}
			}
		}
		if definition.OwnerService == "" {
			continue
		}
		directory := filepath.ToSlash(filepath.Join("config", "profiles", definition.Name))
		owner := directory + "/" + definition.OwnerService
		contracts[owner] = shellContract{"profile", directory + "/profile.json", definition.OwnerService}
		provision := directory + "/provision.sh"
		if _, err := os.Stat(filepath.Join(root, provision)); err == nil {
			contracts[provision] = shellContract{"profile", owner, "provision.sh"}
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	addProfileShellOwnerManifests(t, root, contracts)
	return contracts

}

func addProfileShellOwnerManifests(t *testing.T, root string, contracts map[string]shellContract) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "config", "profiles", "*", "tests", "shell-owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 64<<10 {
			t.Fatalf("invalid profile shell ownership manifest %s: %v", path, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var manifest profileShellOwnerManifest
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&manifest); err != nil || decoder.Decode(new(any)) != io.EOF || manifest.SchemaVersion != 1 || len(manifest.Owners) == 0 {
			t.Fatalf("invalid profile shell ownership manifest %s: %v", path, err)
		}
		profileName := filepath.Base(filepath.Dir(filepath.Dir(path)))
		profilePrefix := "config/profiles/" + profileName + "/"
		for _, owner := range manifest.Owners {
			ownedPath := strings.HasPrefix(owner.Path, profilePrefix) ||
				owner.Kind == "library" && strings.HasPrefix(owner.Path, "scripts/") &&
					strings.HasPrefix(owner.Owner, profilePrefix)
			if !safeShellOwnerPath(owner.Path) || !ownedPath ||
				!safeShellOwnerPath(owner.Owner) || owner.Reference == "" || len(owner.Reference) > 256 ||
				owner.Kind != "profile" && owner.Kind != "library" && owner.Kind != "embedded" {
				t.Fatalf("invalid profile shell owner entry in %s: %+v", path, owner)
			}
			if _, exists := contracts[owner.Path]; exists {
				t.Fatalf("duplicate shell ownership contract for %s", owner.Path)
			}
			contracts[owner.Path] = shellContract{owner.Kind, owner.Owner, owner.Reference}
		}
	}
}

func safeShellOwnerPath(value string) bool {
	return value != "" && !filepath.IsAbs(value) && filepath.ToSlash(filepath.Clean(value)) == value &&
		!strings.ContainsAny(value, "\\\r\n\t") && value != ".." && !strings.HasPrefix(value, "../")
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
		"scripts/profile-services.sh":         {"reconcile", "ports.ReconcileStageProfileServices"},
		"scripts/install-power-reconciler.sh": {"reconcile", "ports.ReconcileStagePower"},
		"scripts/install-test-vms-host-sink.sh": {
			"reconcile", "ports.ReconcileStageTestVMs",
		},
		"scripts/e2e-lab/invoke.sh":         {"command", "test-vms"},
		"scripts/reconcile-integrations.sh": {"command", "integration"},
		"scripts/lifecycle-guard.sh":        {"command", "lifecycle"},
		"scripts/teardown-physical.sh":      {"command", "teardown"},
	}
}

func shellFiles(t *testing.T, root string) []string {
	t.Helper()
	return sourceFiles(t, []string{root}, func(path string) bool { return strings.HasSuffix(path, ".sh") })
}

func profileTestPath(path string) bool {
	_, relative, found := strings.Cut(filepath.ToSlash(path), "/config/profiles/")
	if !found {
		return false
	}
	_, relative, found = strings.Cut(relative, "/")
	return found && (relative == "tests" || strings.HasPrefix(relative, "tests/"))
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
			if entry.IsDir() && profileTestPath(path) {
				return filepath.SkipDir
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
