package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRefreshConfigsUsesTypedAtomicGuestWrites(t *testing.T) {
	root := t.TempDir()
	source := func(name, payload string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	environment := []string{
		"CODING_TOOL_INTEGRATIONS=opencode",
		"AGENT_opencode_CONFIG=" + source("opencode.jsonc", "{}\n"),
		"AGENT_opencode_CONFIG_DEST=.config/opencode/opencode.jsonc",
		"AGENT_opencode_RULES=" + source("repo.rules", "rule\n"),
		"AGENT_opencode_RULES_DEST=.config/opencode/repo.rules",
	}
	incus := runningIncus(10)
	wantDestinations := []string{
		"/home/dev/.config/opencode/opencode.jsonc",
		"/home/dev/.config/opencode/repo.rules",
	}
	incus.ExecSteps[0].Result.Stdout = []byte(`{}`)
	incus.ExecSteps[5].Result.Stdout = []byte(`{}`)
	var output bytes.Buffer
	runtime := Runtime{
		Integrations: []profile.IntegrationDefinition{{Name: "opencode", Artifacts: []profile.IntegrationArtifact{{ID: "config", Kind: "config", Destination: ".config/opencode/opencode.jsonc", Format: "raw"}, {ID: "rules", Kind: "config", Destination: ".config/opencode/repo.rules", Format: "raw"}}}},
		Environment:  environment, Incus: incus, Executor: incus, Stdout: &output,
		Yard: domain.Context{
			IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1001,
		},
	}
	bindArtifactTestPackages(t, &runtime, root)
	if err := runtime.RefreshConfigs(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RefreshConfigs(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(incus.ExecCalls) != 10 || len(incus.TokenObservations) != 2 {
		t.Fatalf("config refresh calls = %d, want 4 writes and 6 inventory operations after 2 observations", len(incus.ExecCalls))
	}
	for index, mode := range map[int]string{0: "settings", 1: "configs-prepare", 4: "configs-commit", 5: "settings", 6: "configs-prepare", 9: "configs-commit"} {
		if command := incus.ExecCalls[index].Request.Command; len(command) != 5 || command[4] != mode {
			t.Fatalf("inventory operation %d: %v", index, command)
		}
	}
	writes := append(incus.ExecCalls[2:4:4], incus.ExecCalls[7:9]...)
	for index, call := range writes {
		if call.Project != "subyard" || call.Name != "yard" ||
			len(call.Request.Command) != 10 ||
			call.Request.Command[3] != configmaterial.CompareWriteProgram() || call.Request.Command[4] != "write" ||
			call.Request.Command[7] != "1001" || call.Request.Command[8] != configmaterial.TokenAbsent {
			t.Fatalf("config write %d is not the compare-and-write contract: %#v", index, call)
		}
		destination := call.Request.Command[6]
		if !strings.HasPrefix(destination, "/home/dev/") ||
			strings.Contains(destination, "auth.json") {
			t.Fatalf("unsafe config destination %q", destination)
		}
		if !slices.Contains(wantDestinations, destination) {
			t.Fatalf("unselected agent config destination %q", destination)
		}
		if index >= 2 {
			first := writes[index-2].Request
			if !slices.Equal(first.Command, call.Request.Command) ||
				!bytes.Equal(first.Stdin, call.Request.Stdin) {
				t.Fatalf("config refresh %d is not idempotent", index-2)
			}
		}
	}
	if !strings.Contains(output.String(), "Agent configs refreshed") {
		t.Fatalf("refresh output changed: %q", output.String())
	}
}

// staleGuest refuses, as the guest does, every write over the stale token.
type staleGuest struct {
	*testkit.Incus
	stale string
}

func (guest staleGuest) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	result, err := guest.Incus.Exec(ctx, project, instance, request)
	if command := request.Command; len(command) == 10 && command[4] == "write" && command[8] == guest.stale {
		return ports.InstanceExecResult{ExitCode: 4}, &ports.GuestExitError{Code: 4}
	}
	return result, err
}

// A confirmed refresh writes over its own tokens: another instance is a stale
// plan; a file the confirmation did not cover is written over the writer's
// own observation; a file changed since its token is refused while the others
// are written, and the inventory stays uncommitted.
func TestRefreshConfigsWritesOverConsentOrItsOwnObservation(t *testing.T) {
	root := t.TempDir()
	source := func(name, payload string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	environment := []string{
		"CODING_TOOL_INTEGRATIONS=opencode",
		"AGENT_opencode_CONFIG=" + source("opencode.jsonc", "{}\n"),
		"AGENT_opencode_CONFIG_DEST=.config/opencode/opencode.jsonc",
		"AGENT_opencode_RULES=" + source("repo.rules", "rule\n"),
		"AGENT_opencode_RULES_DEST=.config/opencode/repo.rules",
	}
	stale := "sha256:" + strings.Repeat("a", 64)
	newRuntime := func(incus *testkit.Incus) Runtime {
		incus.Reconcile.Instance.Config = map[string]string{"volatile.uuid": "instance-a"}
		runtime := Runtime{
			Integrations: []profile.IntegrationDefinition{{Name: "opencode", Artifacts: []profile.IntegrationArtifact{{ID: "config", Kind: "config", Destination: ".config/opencode/opencode.jsonc", Format: "raw"}, {ID: "rules", Kind: "config", Destination: ".config/opencode/repo.rules", Format: "raw"}}}},
			Environment:  environment, Incus: incus, Executor: staleGuest{incus, stale}, Stdout: io.Discard,
			Yard: domain.Context{YardName: "named", IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1001},
		}
		bindArtifactTestPackages(t, &runtime, root)
		return runtime
	}
	other := runningIncus(8)
	other.ExecSteps[0].Result.Stdout = []byte(`{}`)
	err := newRuntime(other).RefreshConfigs(context.Background(), &ports.ConfigConsent{InstanceUUID: "instance-b"})
	if !errors.Is(err, domain.ErrPlanStale) || slices.ContainsFunc(other.ExecCalls, func(call testkit.IncusExecCall) bool { return slices.Contains(call.Request.Command, "write") }) {
		t.Fatalf("another instance: %v", err)
	}
	incus := runningIncus(8)
	incus.ExecSteps[0].Result.Stdout = []byte(`{}`)
	err = newRuntime(incus).RefreshConfigs(context.Background(), &ports.ConfigConsent{InstanceUUID: "instance-a", Files: []ports.ConfigFileConsent{
		{Destination: "/home/dev/.config/opencode/opencode.jsonc", Token: stale},
	}})
	var refusal configmaterial.FileRefusal
	if !errors.As(err, &refusal) || refusal.File.State() != "changed" || refusal.File.Destination != "/home/dev/.config/opencode/opencode.jsonc" ||
		!strings.HasSuffix(err.Error(), "; next: run yard -Y named config apply") {
		t.Fatalf("changed file: %v", err)
	}
	written := map[string]string{}
	for _, call := range incus.ExecCalls {
		if command := call.Request.Command; len(command) == 5 && command[4] == "configs-commit" {
			t.Fatal("inventory committed after a refused file")
		} else if len(command) == 10 && command[4] == "write" {
			written[command[6]] = command[8]
		}
	}
	if written["/home/dev/.config/opencode/repo.rules"] != configmaterial.TokenAbsent || written["/home/dev/.config/opencode/opencode.jsonc"] != stale ||
		len(incus.TokenObservations) != 1 || !slices.Equal(incus.TokenObservations[0].Request.Command[6:], []string{"/home/dev/.config/opencode/repo.rules"}) {
		t.Fatalf("writes %v, observations %v", written, incus.TokenObservations)
	}
	// Every refused file is reported with its own step.
	incus = runningIncus(8)
	incus.ExecSteps[0].Result.Stdout = []byte(`{}`)
	incus.GuestTokens["/home/dev/.config/opencode/repo.rules"] = configmaterial.TokenUnreadable
	err = newRuntime(incus).RefreshConfigs(context.Background(), &ports.ConfigConsent{InstanceUUID: "instance-a", Files: []ports.ConfigFileConsent{
		{Destination: "/home/dev/.config/opencode/opencode.jsonc", Token: stale},
	}})
	if err == nil || !strings.Contains(err.Error(), "/home/dev/.config/opencode/repo.rules is not a readable regular file; it was kept; next: replace it with a regular file or remove it, then run yard -Y named config apply") ||
		!strings.Contains(err.Error(), "opencode.jsonc changed after its refresh was planned; the guest's file was kept; next: run yard -Y named config apply") {
		t.Fatalf("refusal steps per file class: %v", err)
	}
}

// The compatibility fixture allows only this exact command, including its program.
func TestGuestConfigWriteMatchesCompatibilityFixture(t *testing.T) {
	incus := runningIncus(1)
	runtime := Runtime{Executor: incus, Yard: domain.Context{DevUser: "dev", DevUID: 1000}}
	payload := []byte("desired\n")
	if err := runtime.writeGuestFile(context.Background(), "config", "/home/dev/config", payload, configmaterial.TokenAbsent); err != nil {
		t.Fatal(err)
	}
	request := incus.ExecCalls[0].Request
	want := []string{"python3", "-B", "-c", configmaterial.CompareWriteProgram(), "write", "/home/dev", "/home/dev/config", "1000", configmaterial.TokenAbsent, fmt.Sprintf("%x", sha256.Sum256(payload))}
	if !slices.Equal(request.Command, want) || !bytes.Equal(request.Stdin, payload) {
		t.Fatalf("guest writer differs from the compatibility fixture: %#v", request)
	}
}

// Execute the actual guest writer with a prematurely closed transport. Account
// changes are stubbed because this fixture owns only ordinary /tmp files.
func TestGuestConfigWriteRefusesIncompleteTransport(t *testing.T) {
	for _, test := range []struct {
		desired, received string
		valid             bool
	}{
		{"approved complete instructions\n", "", false},
		{"approved complete instructions\n", "approved", false},
		{"approved complete instructions\n", "approved complete instructions\n", true},
		{"", "", true}, // Explicitly intended empty bytes are different from lost input.
	} {
		t.Run(fmt.Sprint(len(test.desired), "-", len(test.received)), func(t *testing.T) {
			root := testkit.TempDir(t)
			destination := filepath.Join(root, "instructions")
			original := []byte("previous complete instructions\n")
			testkit.WriteFile(t, destination, original, 0644)
			// SIGKILL may leave unpublished temps. Exact destination writes must
			// ignore them and must never follow a similarly named symlink.
			orphan := filepath.Join(root, ".subyard-config.interrupted")
			testkit.WriteFile(t, orphan, []byte("unpublished"), 0600)
			orphanLink := filepath.Join(root, ".subyard-config.foreign-link")
			if err := os.Symlink(orphan, orphanLink); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(destination)
			if err != nil {
				t.Fatal(err)
			}
			incus := runningIncus(1)
			runtime := Runtime{Executor: incus, Yard: domain.Context{DevUser: "next", DevUID: os.Getuid()}}
			if err := runtime.writeGuestFile(context.Background(), "instructions", "/home/next/instructions", []byte(test.desired), configmaterial.PayloadToken(original)); err != nil {
				t.Fatal(err)
			}
			request := incus.ExecCalls[0].Request
			request.Command[5], request.Command[6] = filepath.Dir(root), destination
			command := exec.Command(request.Command[0], request.Command[1:]...)
			command.Stdin = strings.NewReader(test.received)
			output, err := command.CombinedOutput()
			if (err == nil) != test.valid {
				t.Errorf("transport valid=%t: err=%v output=%s", test.valid, err, output)
			}
			after, statErr := os.Stat(destination)
			actual, readErr := os.ReadFile(destination)
			if target, err := os.Readlink(orphanLink); err != nil || target != orphan {
				t.Fatal("orphan symlink changed", err)
			}
			if payload, err := os.ReadFile(orphan); err != nil || string(payload) != "unpublished" {
				t.Fatal("orphan followed or changed", err)
			}
			if test.valid {
				if statErr != nil || readErr != nil || string(actual) != test.desired || after.Mode().Perm() != 0644 {
					t.Fatalf("approved write failed: %q %v %v", actual, statErr, readErr)
				}
				return
			}
			if statErr != nil || readErr != nil || !os.SameFile(before, after) || !bytes.Equal(actual, original) {
				t.Fatalf("incomplete transport replaced original: bytes=%q stat=%v read=%v", actual, statErr, readErr)
			}
		})
	}
}

func TestUnownedInvalidConfigRefusalNamesNativePreservationStep(t *testing.T) {
	fake := runningIncus(1)
	fake.ExecSteps = []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: 2, Stderr: []byte("config materialization: unowned-invalid-destination-v1 JSON; private document details")}}}
	runtime := Runtime{Executor: fake, Yard: domain.Context{YardName: "default", IncusProject: "sample", YardInstanceName: "sample-yard", DevUser: "dev", DevUID: 1000}}
	err := runtime.applyGuestConfig(context.Background(), guestConfigFile{destination: "/home/dev/settings.json", ownedFormat: "json"}, []byte(`{"managed":true}`), configmaterial.TokenAbsent)
	if err == nil || !strings.Contains(err.Error(), "'incus' 'exec' 'sample-yard' '--project' 'sample'") || !strings.Contains(err.Error(), "--backup=numbered") || !strings.Contains(err.Error(), "then run yard -Y default config apply") || strings.Contains(err.Error(), "private document") || strings.Contains(err.Error(), "yard shell") {
		t.Fatal("unreachable or unsafe preservation hint", err)
	}
}

func TestConfigApplyReadFailureDoesNotClaimUnownedPreservation(t *testing.T) {
	fake := runningIncus(1)
	fake.ExecSteps = []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: 2, Stderr: []byte("config materialization: invalid current JSON")}}}
	runtime := Runtime{Executor: fake, Yard: domain.Context{DevUser: "dev", DevUID: 1000}}
	err := runtime.applyGuestConfig(context.Background(), guestConfigFile{destination: "/home/dev/settings.json", ownedFormat: "json"}, []byte(`{"managed":true}`), configmaterial.TokenAbsent)
	if err == nil || strings.Contains(err.Error(), "unowned") || strings.Contains(err.Error(), "preserved") || strings.Contains(err.Error(), "mv") {
		t.Fatal("read failure misclassified", err)
	}
}

func TestPrepareConfigsReportsEveryInvalidSourceBeforeMutation(t *testing.T) {
	root := testkit.TempDir(t)
	incus := runningIncus(0)
	runtime := Runtime{Incus: incus, Executor: incus, Yard: domain.Context{DevUser: "dev"},
		Environment: []string{"CODING_TOOL_INTEGRATIONS=sample", "AGENT_sample_CONFIG=" + filepath.Join(root, "config-directory"), "AGENT_sample_CONFIG_DEST=config", "AGENT_sample_RULES=" + filepath.Join(root, "rules-directory"), "AGENT_sample_RULES_DEST=rules"}}
	for _, name := range []string{"config-directory", "rules-directory"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	_, err := runtime.PrepareConfigs(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sample config") || !strings.Contains(err.Error(), "sample rules") {
		t.Fatalf("independent source refusals missing: %v", err)
	}
	if len(incus.ExecCalls) != 0 {
		t.Fatal("invalid sources made guest calls")
	}
}

// The runtime's convergence is the token observation that config status and
// release readiness use: an unreadable file is not converged.
func TestConfigsConvergedComparesSourceAndGuestTokensReadOnly(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "repo.rules")
	payload := []byte("current rules\n")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	effective, err := (guestConfigFile{source: source}).readSource()
	if err != nil {
		t.Fatal(err)
	}
	observation := func(token string) string {
		payload, _ := json.Marshal(map[string]any{"files": []map[string]string{{"destination": "/home/dev/.codex/rules/repo.rules", "token": token}}})
		return string(payload)
	}
	for _, test := range []struct {
		name   string
		stdout string
		want   bool
	}{
		{name: "converged", stdout: observation(configmaterial.PayloadToken(effective)), want: true},
		{name: "drifted", stdout: observation("sha256:" + strings.Repeat("0", 64))},
		{name: "unreadable", stdout: observation(configmaterial.TokenUnreadable)},
	} {
		t.Run(test.name, func(t *testing.T) {
			incus := runningIncus(1)
			incus.GuestTokens = nil
			incus.ExecSteps[0].Result.Stdout = []byte(test.stdout)
			runtime := Runtime{
				Environment: []string{"CODING_TOOL_INTEGRATIONS=codex", "AGENT_codex_RULES=" + source, "AGENT_codex_RULES_DEST=.codex/rules/repo.rules"},
				Incus:       incus, Executor: incus,
				Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev"},
			}
			bindAgentTestPackage(t, &runtime, root, "codex")
			got, err := runtime.ConfigsConverged(context.Background())
			if err != nil || got != test.want {
				t.Fatalf("converged=%t err=%v", got, err)
			}
			if command := incus.ExecCalls[0].Request.Command; len(incus.ExecCalls) != 1 || !testkit.IsGuestTokenObservation(command) || !slices.Equal(command[6:], []string{"/home/dev/.codex/rules/repo.rules"}) {
				t.Fatalf("token probe=%#v", incus.ExecCalls)
			}
		})
	}
}

func TestRefreshConfigsRejectsPathsOutsideDeveloperHome(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	if err := os.WriteFile(config, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	incus := runningIncus(0)
	runtime := Runtime{
		Environment: []string{
			"CODING_TOOL_INTEGRATIONS=opencode",
			"AGENT_opencode_CONFIG=" + config,
			"AGENT_opencode_CONFIG_DEST=../../root/.config",
		},
		Incus: incus, Executor: incus,
		Yard: domain.Context{
			IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev",
		},
	}
	if err := runtime.RefreshConfigs(context.Background(), nil); err == nil ||
		!strings.Contains(err.Error(), "leaves the developer home") {
		t.Fatalf("unsafe config destination error = %v", err)
	}
	if len(incus.ExecCalls) != 0 {
		t.Fatalf("unsafe config caused guest execution: %#v", incus.ExecCalls)
	}
}

func TestRefreshConfigsRejectsSymlinkSourceBeforeGuestMutation(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "config.toml")
	if err := os.WriteFile(target, []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "linked.toml")
	if err := os.Symlink(target, source); err != nil {
		t.Fatal(err)
	}
	incus := runningIncus(0)
	runtime := Runtime{
		Environment: []string{
			"CODING_TOOL_INTEGRATIONS=codex",
			"AGENT_codex_CONFIG=" + source,
			"AGENT_codex_CONFIG_DEST=.codex/config.toml",
		},
		Incus: incus, Executor: incus,
		Yard: domain.Context{
			IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev",
		},
	}
	bindAgentTestPackage(t, &runtime, root, "codex")
	if err := runtime.RefreshConfigs(context.Background(), nil); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink source error = %v", err)
	}
	if len(incus.ExecCalls) != 0 {
		t.Fatalf("symlink source caused guest execution: %#v", incus.ExecCalls)
	}
}

func TestApplyGitIdentityUsesTypedDeveloperCommands(t *testing.T) {
	incus := runningIncus(3)
	runtime := Runtime{
		Environment: []string{"GIT_USER_NAME=Developer", "GIT_USER_EMAIL=dev@example.test"},
		Incus:       incus, Executor: incus,
		Yard: domain.Context{
			IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1001,
			Paths: domain.RuntimePaths{DataHome: t.TempDir()},
		},
	}
	if err := runtime.applyGitIdentity(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"git", "config", "--global", "--replace-all", "user.name", "Developer"},
		{"git", "config", "--global", "--replace-all", "user.email", "dev@example.test"},
		{"git", "config", "--global", "--replace-all", "safe.directory", "*"},
	}
	if len(incus.ExecCalls) != len(want) {
		t.Fatalf("git identity calls = %#v", incus.ExecCalls)
	}
	for index, call := range incus.ExecCalls {
		if !slices.Equal(call.Request.Command, want[index]) ||
			call.Request.User != 1001 || call.Request.Group != 1001 ||
			call.Request.Environment["HOME"] != "/home/dev" {
			t.Fatalf("git identity call %d = %#v", index, call)
		}
	}
}

func TestApplyGitIdentityKeepsHostFallbackAndIgnoresRetiredDropin(t *testing.T) {
	dataHome, hostHome := testkit.TempDir(t), testkit.TempDir(t)
	payload := []byte("[user]\n\tname = Retired\n[core]\n\teditor = retired-editor\n")
	testkit.WriteFile(t, filepath.Join(dataHome, "gitconfig"), payload, 0600)
	testkit.WriteFile(t, filepath.Join(hostHome, ".gitconfig"), []byte("[user]\n\tname = Host Operator\n\temail = host@example.test\n"), 0600)
	incus := runningIncus(3)
	runtime := Runtime{
		Incus: incus, Executor: incus,
		Environment: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + hostHome, "GIT_CONFIG_NOSYSTEM=1"},
		Yard: domain.Context{
			IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev",
			Paths: domain.RuntimePaths{DataHome: dataHome},
		},
	}
	if err := runtime.applyGitIdentity(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"git", "config", "--global", "--replace-all", "user.name", "Host Operator"},
		{"git", "config", "--global", "--replace-all", "user.email", "host@example.test"},
		{"git", "config", "--global", "--replace-all", "safe.directory", "*"},
	}
	if len(incus.ExecCalls) != len(want) {
		t.Fatalf("fallback calls = %#v", incus.ExecCalls)
	}
	for i, call := range incus.ExecCalls {
		if !slices.Equal(call.Request.Command, want[i]) || len(call.Request.Stdin) != 0 {
			t.Fatalf("fallback call = %#v", call)
		}
	}
	after, err := os.ReadFile(filepath.Join(dataHome, "gitconfig"))
	if err != nil || !bytes.Equal(payload, after) {
		t.Fatalf("retired drop-in changed: %q %v", after, err)
	}
}

func runningIncus(execCount int) *testkit.Incus {
	steps := make([]testkit.IncusExecStep, execCount)
	for index := range steps {
		steps[index].Result = ports.InstanceExecResult{}
	}
	return &testkit.Incus{
		Reconcile: ports.ReconcileState{
			InstanceFound: true,
			Instance:      ports.InstanceInfo{Status: "Running"},
		},
		ExecSteps:   steps,
		GuestTokens: map[string]string{},
	}
}

func TestConfigsConvergedUsesOwnedTOMLObservationForImportedSource(t *testing.T) {
	source := filepath.Join(t.TempDir(), "imported-config")
	if err := os.WriteFile(source, []byte("model = \"gpt-5\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	incus := runningIncus(1)
	incus.ExecSteps[0].Result.Stdout = []byte(`{"converged":true,"fingerprint":"` + strings.Repeat("a", 64) + `","raw":"absent"}`)
	runtime := Runtime{
		Environment: []string{"CODING_TOOL_INTEGRATIONS=codex", "AGENT_codex_CONFIG=" + source, "AGENT_codex_CONFIG_DEST=.codex/config.toml"},
		Incus:       incus, Executor: incus,
		Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev"},
	}
	bindAgentTestPackage(t, &runtime, filepath.Dir(source), "codex")
	converged, err := runtime.ConfigsConverged(context.Background())
	if err != nil || !converged {
		t.Fatalf("owned TOML observation rejected: converged=%v err=%v", converged, err)
	}
	request := incus.ExecCalls[0].Request
	if len(request.Command) == 0 || request.Command[0] != "python3" || len(request.Stdin) == 0 {
		t.Fatal("TOML observation did not use the guest ownership adapter")
	}
}

// Bind semantic fixture declarations to real package files before testing effects.
func bindArtifactTestPackages(t *testing.T, runtime *Runtime, root string) {
	t.Helper()
	for index := range runtime.Integrations {
		definition := &runtime.Integrations[index]
		if len(definition.Artifacts) == 0 {
			continue
		}
		definition.SchemaVersion, definition.Root = 2, root
		for index := range definition.Artifacts {
			artifact := &definition.Artifacts[index]
			if artifact.Kind == "config" && artifact.Source == "" {
				artifact.Source = filepath.Base(runtime.environmentValue("AGENT_" + definition.Name + "_" + strings.ToUpper(artifact.ID)))
			}
		}
		if _, err := definition.Metadata(); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(definition)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, "integration.json"), data, 0600)
	}
}

// Agent fixtures are real immutable packages, including across child
// processes. Reopening a fixture must not rewrite its source identities.
func bindAgentTestPackage(t *testing.T, runtime *Runtime, root, agent string) {
	t.Helper()
	directory := filepath.Join(root, "config/agents", agent)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	writeOnce := func(path string, data []byte) {
		t.Helper()
		old, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			testkit.WriteFile(t, path, data, 0600)
			return
		}
		if err != nil || !bytes.Equal(old, data) {
			t.Fatal("agent fixture package changed", path, err)
		}
	}
	definition := profile.IntegrationDefinition{SchemaVersion: 2, Name: agent}
	for _, kind := range []string{"CONFIG", "RULES"} {
		key := "AGENT_" + agent + "_" + kind
		if runtime.environmentValue(key) == "" || runtime.environmentValue(key+"_DEST") == "" {
			continue
		}
		name := strings.ToLower(kind)
		format := "raw"
		if kind == "CONFIG" {
			format = "destination"
		}
		definition.Artifacts = append(definition.Artifacts, profile.IntegrationArtifact{ID: name, Kind: "config", Source: name, Destination: runtime.environmentValue(key + "_DEST"), Format: format})
		writeOnce(filepath.Join(directory, name), []byte("{}\n"))
	}
	data, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	writeOnce(filepath.Join(directory, "integration.json"), data)
	loaded, err := profile.LoadIntegrations(root)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.RepositoryRoot == "" {
		runtime.RepositoryRoot = root
	}
	runtime.Integrations = loaded
}

type localGitIdentityExecutor struct{ home string }

func (executor localGitIdentityExecutor) Exec(ctx context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if request.Environment["HOME"] != "/home/developer" || request.User != 1001 || request.Group != 1001 {
		return ports.InstanceExecResult{}, errors.New("Git identity lost developer context")
	}
	command := exec.CommandContext(ctx, request.Command[0], request.Command[1:]...)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + executor.home, "XDG_CONFIG_HOME=" + executor.home + "/.config", "GIT_CONFIG_NOSYSTEM=1"}
	output, err := command.CombinedOutput()
	result := ports.InstanceExecResult{Stdout: output}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
	}
	return result, err
}

func TestGitIdentityApplyAndProbeUseNativeGlobalConfigSelection(t *testing.T) {
	for _, location := range []string{".gitconfig", ".config/git/config"} {
		t.Run(location, func(t *testing.T) {
			home := testkit.TempDir(t)
			path := filepath.Join(home, location)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, path, []byte("[user]\n\tname = retained\n[core]\n\teditor = retained-editor\n"), 0600)
			runtime := Runtime{Incus: runningIncus(0), Executor: localGitIdentityExecutor{home},
				Yard:        domain.Context{DevUser: "developer", DevUID: 1001, Paths: domain.RuntimePaths{DataHome: testkit.TempDir(t)}},
				Environment: []string{"GIT_USER_NAME=Developer", "GIT_USER_EMAIL=dev@example.test"}}
			if ready, err := runtime.CheckStage(context.Background(), ports.ReconcileStageGitIdentity); err != nil || ready {
				t.Fatalf("missing applied trust was accepted: %t %v", ready, err)
			}
			if err := runtime.ApplyStage(context.Background(), ports.ReconcileStageGitIdentity); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if ready, err := runtime.CheckStage(context.Background(), ports.ReconcileStageGitIdentity); err != nil || !ready {
				t.Fatalf("native Git applied state not recognized: %t %v", ready, err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			afterInfo, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || !os.SameFile(info, afterInfo) || info.Mode() != afterInfo.Mode() || info.ModTime() != afterInfo.ModTime() {
				t.Fatal("Git observation changed configuration")
			}
			for _, key := range []string{"user.name", "user.email"} {
				if err := runtime.runGuestAsDev(context.Background(), []string{"git", "config", "--global", "--add", key, "manual drift"}); err != nil {
					t.Fatal(err)
				}
				if ready, err := runtime.CheckStage(context.Background(), ports.ReconcileStageGitIdentity); err != nil || ready {
					t.Fatalf("owned %s drift was not observed: %t %v", key, ready, err)
				}
				if err := runtime.ApplyStage(context.Background(), ports.ReconcileStageGitIdentity); err != nil {
					t.Fatal(err)
				}
				if ready, err := runtime.CheckStage(context.Background(), ports.ReconcileStageGitIdentity); err != nil || !ready {
					t.Fatalf("git-identity init stage did not repair %s: %t %v", key, ready, err)
				}
			}
			if err := runtime.runGuestAsDev(context.Background(), []string{"git", "config", "--global", "--replace-all", "user.email", "unowned@example.test"}); err != nil {
				t.Fatal(err)
			}
			runtime.Environment = []string{"GIT_USER_NAME=Developer"}
			if ready, err := runtime.CheckStage(context.Background(), ports.ReconcileStageGitIdentity); err != nil || !ready {
				t.Fatalf("unset email reported drift: %t %v", ready, err)
			}
			runtime.Environment = nil
			if ready, err := runtime.CheckStage(context.Background(), ports.ReconcileStageGitIdentity); err != nil || !ready {
				t.Fatalf("unowned identity reported drift: %t %v", ready, err)
			}
			preserved, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(preserved, []byte("editor = retained-editor")) {
				t.Fatalf("unrelated Git settings lost: %q %v", preserved, err)
			}
			if location != ".gitconfig" {
				if _, err := os.Stat(filepath.Join(home, ".gitconfig")); !os.IsNotExist(err) {
					t.Fatal("XDG writer unexpectedly created legacy Git config")
				}
			}
		})
	}
}
