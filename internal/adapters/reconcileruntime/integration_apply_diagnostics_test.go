package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

// applyDiagnosticExecutor fails exactly one apply step with private native output.
type applyDiagnosticExecutor struct {
	retryableIntegrationExecutor
	cut             string
	applyStarted    bool
	persistApplied  bool
	verifiedReads   int
	captureAttempts int
}

func (fixture *applyDiagnosticExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	args := request.Command
	point := ""
	if len(args) >= 5 && args[0] == "python3" {
		switch args[3] {
		case IntegrationInventoryReader():
			point = "inventory " + args[4]
			if args[4] == "capture-settings" {
				fixture.captureAttempts++
			}
			if args[4] == "settings" {
				return ports.InstanceExecResult{Stdout: []byte(`{}`)}, nil
			}
			if args[4] == "observe" && fixture.commits != 0 {
				point = "verify"
				fixture.verifiedReads++
			}
			if fixture.cut == "retire structured config" && args[4] == "observe" {
				payload, _ := json.Marshal(integrationObservation{Fingerprint: strings.Repeat("a", 64), Changed: true,
					Retired: []integrationArtifact{{ID: "sample", Kind: "structured", Format: "json", Path: "/home/dev/settings.json"}}})
				return ports.InstanceExecResult{Stdout: payload}, nil
			}
		case integrationPersistProgram:
			if args[4] == "observe" {
				observed := persistObservation{Fingerprint: strings.Repeat("b", 64)}
				if !fixture.persistApplied || fixture.cut == "persist verify drift" || fixture.cut == "persist capture drift" && fixture.verifiedReads != 0 {
					observed.Entries = []persistEntry{{Integration: "sample", Source: "/home/dev/.memory", Ops: []string{"finish"}}}
				}
				payload, _ := json.Marshal(observed)
				return ports.InstanceExecResult{Stdout: payload}, nil
			}
			point = "persist"
			fixture.persistApplied = true
			if fixture.cut != point {
				return ports.InstanceExecResult{Stdout: []byte("{\"results\":[]}")}, nil
			}
		default:
			if args[4] == configmaterial.ModeObserve {
				if fixture.applyStarted && fixture.cut == "structured observe" {
					return ports.InstanceExecResult{Stdout: []byte("private-native-stdout"), Stderr: []byte("private-native-stderr")}, errors.New("private-native-error")
				}
				return ports.InstanceExecResult{Stdout: []byte(`{"converged":true,"fingerprint":"` + strings.Repeat("c", 64) + `"}`)}, nil
			}
			if args[4] == configmaterial.ModeApply && fixture.cut == "structured apply" {
				return ports.InstanceExecResult{ExitCode: 2, Stderr: []byte("config materialization: unowned-invalid-destination-v1 JSON; private-native-document")}, nil
			}
			if args[4] == configmaterial.ModeAssessRetire {
				return ports.InstanceExecResult{Stdout: []byte(`{"converged":false,"fingerprint":"` + strings.Repeat("c", 64) + `"}`)}, nil
			}
			if args[4] == configmaterial.ModeRetire {
				point = "retire structured config"
			}
		}
	}
	if len(args) == 4 && args[3] == "--observe" {
		point = "hooks run"
	}
	if len(args) > 3 && strings.Contains(args[3], "temporary=$(mktemp") {
		point = "configs"
	}
	if fixture.commits != 0 {
		switch {
		case len(args) == 5 && args[4] == "private-native-command":
			point = "verify command"
		case len(args) == 1 && args[0] == "private-native-check":
			point = "verify check"
			if fixture.verifiedReads == 2 && fixture.cut == "capture check exit" {
				point = "capture check"
			}
		case len(args) > 3 && strings.Contains(args[3], "dispatcher=$3"):
			point = "verify project hooks"
		case len(args) > 5 && args[5] == "observe" && strings.Contains(args[3], "mode=$1"):
			point = "verify services"
		}
	}
	if point != "" && fixture.cut == point+" exit" {
		result := ports.InstanceExecResult{ExitCode: 1, Stdout: []byte("private-native-stdout"), Stderr: []byte("private-native-stderr")}
		return result, &ports.GuestExitError{Code: 1}
	}
	if fixture.cut == "verify independent refusals" {
		switch point {
		case "verify", "verify check", "verify project hooks":
			return ports.InstanceExecResult{}, errors.New("private-native-error")
		case "verify services":
			return ports.InstanceExecResult{ExitCode: 1, Stderr: []byte("subyard-integration-conflict:ccusage\nprivate-native-stderr")}, &ports.GuestExitError{Code: 1}
		}
	}
	if point == fixture.cut {
		return ports.InstanceExecResult{Stdout: []byte("private-native-stdout"), Stderr: []byte("private-native-stderr")}, errors.New("private-native-error")
	}
	return fixture.retryableIntegrationExecutor.Exec(ctx, project, instance, request)
}

func TestIntegrationApplyStepDiagnostics(t *testing.T) {
	const yardRetry, sampleRetry, secondRetry, codexRetry = "yard -Y sample integration status --json", "yard -Y sample integration status sample --json",
		"yard -Y sample integration status second --json", "yard -Y sample integration status codex --json"
	for _, test := range []struct{ cut, message, retry string }{
		{"persist", "integration sample apply persist failed", sampleRetry},
		{"inventory apply", "integration apply inventory update failed", yardRetry},
		{"inventory commit", "integration apply inventory commit failed", yardRetry},
		{"inventory capture-settings", "integration apply settings capture failed", yardRetry},
		{"configs", "integration codex apply configs failed", codexRetry},
		{"script", "integration apply script failed", yardRetry},
		{"hooks run", "integration apply hooks failed", yardRetry},
		{"hooks capture", "integration sample apply hook capture failed", sampleRetry},
		{"retire structured config", "integration sample apply retire failed", sampleRetry},
		{"verify", "integration apply verify failed", yardRetry},
		{"verify command", "integration second apply verify failed", secondRetry},
		{"verify check", "integration second apply verify failed", secondRetry},
		{"verify command exit", "integration second verify in yard sample: native lifecycle did not converge", secondRetry},
		{"verify check exit", "integration second verify in yard sample: native lifecycle did not converge", secondRetry},
		{"capture check exit", "integration apply settings capture failed", yardRetry},
		{"structured observe", "integration codex apply observe failed", codexRetry},
		{"structured apply", `integration codex apply configs in yard sample: unowned invalid configuration at "/home/dev/.codex/settings.json" preserved; after the next command, rerun the pending release activation`, ""},
		{"persist approval", "integration sample apply persist approval failed", sampleRetry},
		{"persist verify drift", "integration sample verify in yard sample: native lifecycle did not converge", sampleRetry},
		{"persist capture drift", "integration apply settings capture failed", yardRetry},
	} {
		t.Run(test.cut, func(t *testing.T) {
			runtime, executor, output := applyDiagnosticFixture(t, test.cut)
			ctx := context.Background()
			plan, err := runtime.IntegrationPlan(ctx)
			if err != nil || !plan.Changed {
				t.Fatalf("fixture plan: changed=%t, %v", plan.Changed, err)
			}
			if test.cut == "persist approval" {
				runtime.PersistFinishOnly = false
				oldFingerprint := strings.Repeat("a", 64)
				runtime.ApprovedPersist = &oldFingerprint
			}
			executor.applyStarted = true
			err = runtime.ApplyIntegrations(ctx, plan)
			var public interface{ ActivationDiagnostic() (string, string) }
			if !errors.As(err, &public) {
				t.Fatalf("missing public diagnostic: %v", err)
			}
			message, retry := public.ActivationDiagnostic()
			wantMessage, wantRetry := test.message+" in yard sample", test.retry
			if strings.Contains(test.message, " in yard ") || test.cut == "structured apply" {
				wantMessage = test.message
			}
			if test.cut == "structured apply" {
				wantRetry = runtime.preserveInvalidConfigCommand("/home/dev/.codex/settings.json")
			}
			if message != wantMessage || retry != wantRetry || strings.Contains(message+retry+output.String(), "private-native") {
				t.Fatalf("diagnostic = %q / %q; want %q / %q (output %q)", message, retry, wantMessage, wantRetry, output.String())
			}
			wantStale := test.cut == "persist capture drift" || test.cut == "capture check exit" || test.cut == "persist approval" || test.cut == "hooks capture"
			if errors.Is(err, domain.ErrPlanStale) != wantStale {
				t.Fatalf("stale classification = %t, want %t: %v", errors.Is(err, domain.ErrPlanStale), wantStale, err)
			}
			if test.cut == "persist approval" && (executor.persistApplied || !strings.Contains(err.Error(), "changed after the init plan was confirmed; nothing was transferred") ||
				!strings.Contains(err.Error(), "'yard' '-Y' 'sample' 'init'")) {
				t.Fatalf("init lost the stale approval refusal or executed the transfer: %v", err)
			}
			if !strings.HasPrefix(test.cut, "inventory capture") && !strings.HasPrefix(test.cut, "capture") && test.cut != "persist capture drift" && executor.captureAttempts != 0 {
				t.Fatal("failed apply reached settings capture")
			}
		})
	}
}

func TestIntegrationVerifyReportsEveryIndependentRefusal(t *testing.T) {
	runtime, executor, output := applyDiagnosticFixture(t, "verify independent refusals")
	ctx := context.Background()
	plan, err := runtime.IntegrationPlan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	executor.applyStarted = true
	err = runtime.ApplyIntegrations(ctx, plan)
	var findings *domain.Refusals
	if !errors.As(err, &findings) || len(findings.Unwrap()) != 4 {
		t.Fatalf("expected four independent verification refusals: %v", err)
	}
	var messages []string
	for _, finding := range findings.Unwrap() {
		var public interface{ ActivationDiagnostic() (string, string) }
		if !errors.As(finding, &public) {
			messages = append(messages, "unattributed: "+finding.Error())
			continue
		}
		message, _ := public.ActivationDiagnostic()
		messages = append(messages, message)
	}
	slices.Sort(messages)
	want := []string{"integration apply verify failed in yard sample", "integration apply verify failed in yard sample",
		"integration second apply verify failed in yard sample",
		"this yard has ALLOWS_CODING_TOOLS=false, but /usr/local/bin/ccusage remains and ownership cannot be verified using /var/lib/subyard/ccusage-ownership; utility preserved. Inspect its installation before explicitly retiring the unwanted utility"}
	if !slices.Equal(messages, want) || strings.Contains(strings.Join(messages, "")+output.String(), "private-native") {
		t.Fatalf("verification refusals = %q", messages)
	}
}

func applyDiagnosticFixture(t *testing.T, cut string) (Runtime, *applyDiagnosticExecutor, *bytes.Buffer) {
	t.Helper()
	root := testkit.TempDir(t)
	for _, dir := range []string{"config", "scripts/lib"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0755)
	testkit.WriteFile(t, filepath.Join(root, "integration.json"), []byte("{}"), 0600)
	script := []byte("#!/bin/sh\nexit 0\n")
	if cut == "script" {
		script = []byte("#!/bin/sh\necho private-native-stdout\necho private-native-stderr >&2\nexit 1\n")
	}
	testkit.WriteFile(t, filepath.Join(root, "scripts/reconcile-integrations.sh"), script, 0755)
	executor := &applyDiagnosticExecutor{cut: cut}
	executor.serviceChanged = true
	output := &bytes.Buffer{}
	runtime := Runtime{RepositoryRoot: root, Executor: executor, PersistFinishOnly: true, Stdout: output,
		Incus:        &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}},
		Yard:         domain.Context{YardName: "sample", DevUser: "dev", DevUID: 1000},
		Integrations: []profile.IntegrationDefinition{{Name: "sample", Root: root}}}
	provision := filepath.Join(root, "provision.sh")
	testkit.WriteFile(t, provision, []byte("#!/bin/sh\nexit 0\n"), 0700)
	switch cut {
	case "persist", "persist approval", "persist verify drift", "persist capture drift":
		runtime.Environment = []string{"AGENT_sample_PERSIST=.memory:/mnt/host/agent-sessions/sample/memory"}
	case "verify command", "verify check", "verify command exit", "verify check exit", "verify independent refusals", "capture check exit":
		secondRoot := filepath.Join(root, "second")
		if err := os.Mkdir(secondRoot, 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(secondRoot, "integration.json"), []byte("{}"), 0600)
		runtime.Integrations = append(runtime.Integrations, profile.IntegrationDefinition{Name: "second", Root: secondRoot})
		runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=sample second",
			"AGENT_sample_PROVISION=" + provision, "AGENT_sample_COMMAND=first-command", "AGENT_sample_CHECK=first-check",
			"AGENT_second_PROVISION=" + provision, "AGENT_second_COMMAND=private-native-command", "AGENT_second_CHECK=private-native-check"}
	case "structured observe", "structured apply":
		runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=codex",
			"AGENT_codex_CONFIG=" + filepath.Join(root, "config/agents/codex/config"), "AGENT_codex_CONFIG_DEST=.codex/settings.json"}
		bindInstructionTestPackage(t, &runtime, root, "codex")
	case "configs":
		path := filepath.Join(root, "instructions.md")
		testkit.WriteFile(t, path, []byte("instructions\n"), 0600)
		runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=codex", "HOST_CODEX_AGENTS_MD=" + path}
		bindInstructionTestPackage(t, &runtime, root, "codex")
	case "hooks capture", "hooks run":
		if cut == "hooks capture" {
			runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=sample", "AGENT_sample_PROVISION=" + provision, "AGENT_sample_COMMAND=true",
				"AGENT_sample_PROJECTS_CHANGED=fixture-hook"}
			executor.observeHooks = func() hookObservation {
				return hookObservation{Projects: strings.Repeat("a", 64), Wiring: strings.Repeat("b", 64)}
			}
		}
		scope, _, err := runtime.IntegrationScope()
		if err != nil {
			t.Fatal(err)
		}
		runtime.HookPlan = &ProjectHookPlan{integrationScope: scope}
	}
	return runtime, executor, output
}

func TestOwnershipRecoveryPreservesEveryConflictBesideIt(t *testing.T) {
	root := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(root, "incus"), []byte("#!/bin/sh\nshift 5\nexec \"$@\"\n"), 0700)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	// A short name keeps its basename; a name at the limit falls back to the digest.
	for name, pattern := range map[string]string{"settings.json": "settings.json.subyard-preserved-*", strings.Repeat("x", 249) + "&copy": ".subyard-preserved-*"} {
		t.Run(pattern, func(t *testing.T) { preserveSiblingFixture(t, name, pattern) })
	}
}

func preserveSiblingFixture(t *testing.T, name, pattern string) {
	directory := testkit.TempDir(t)
	path := filepath.Join(directory, name)
	runtime := Runtime{Yard: domain.Context{YardInstanceName: "yard", IncusProject: "subyard"}}
	command := runtime.preserveSiblingCommand(path, "preserved")
	for _, content := range []string{"first conflict", "second conflict", "third conflict"} {
		testkit.WriteFile(t, path, []byte(content), 0o640)
		if output, err := exec.Command("sh", "-c", command).CombinedOutput(); err != nil {
			t.Fatalf("preserve artifact: %v\n%s", err, output)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("conflicting artifact was left in place: %v", err)
		}
	}
	preserved, err := filepath.Glob(filepath.Join(directory, pattern))
	if err != nil || len(preserved) != 3 {
		t.Fatalf("preserved siblings: %v / %v", preserved, err)
	}
	var saved []string
	for _, sibling := range preserved {
		info, err := os.Stat(sibling)
		if err != nil || info.Mode().Perm() != 0o640 || len(filepath.Base(sibling)) > 255 {
			t.Fatalf("preserved sibling lost its mode or exceeds the name limit: %v %v", info, err)
		}
		payload, err := os.ReadFile(sibling)
		if err != nil {
			t.Fatal(err)
		}
		saved = append(saved, string(payload))
	}
	slices.Sort(saved)
	if !slices.Equal(saved, []string{"first conflict", "second conflict", "third conflict"}) {
		t.Fatalf("preserved data: %q", saved)
	}
}

func TestInventoryConflictsRequireCompletedGuestExit(t *testing.T) {
	report := []byte(`{"conflicts":[{"reason":"owned artifact drift","path":"/home/dev/.codex/AGENTS.md"}]}`)
	for _, test := range []struct {
		name      string
		err       error
		recovered bool
	}{
		{"completed", &ports.GuestExitError{Code: 1}, true},
		{"cancelled", context.Canceled, false},
		{"transport", errors.New("transport failed"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			incus := runningIncus(1)
			incus.ExecSteps[0].Result = ports.InstanceExecResult{ExitCode: 1, Stderr: report}
			incus.ExecSteps[0].Err = test.err
			runtime := Runtime{RepositoryRoot: testkit.TempDir(t), Integrations: []profile.IntegrationDefinition{}, Executor: incus,
				Yard: domain.Context{YardName: "sample", YardInstanceName: "yard", IncusProject: "subyard", DevUser: "dev", DevUID: 1000}}
			entries := []integrationArtifact{{ID: "codex", Kind: "file", Path: "/home/dev/.codex/AGENTS.md"}}
			_, err := runtime.integrationInventory(context.Background(), "apply", entries, "")
			var public interface{ ActivationDiagnostic() (string, string) }
			recovered := errors.As(err, &public)
			if recovered != test.recovered || strings.Contains(err.Error(), "'mv'") != test.recovered {
				t.Fatalf("recovery offered = %t, want %t: %v", recovered, test.recovered, err)
			}
			if test.recovered && IntegrationOwnershipConflict(err) != true || !test.recovered && IntegrationOwnershipConflict(err) {
				t.Fatalf("ownership classification: %v", err)
			}
		})
	}
}

func TestPersistRefusalRendersUnknownGuestTextAsHostText(t *testing.T) {
	runtime := Runtime{Yard: domain.Context{YardName: "default", YardInstanceName: "yard-default", IncusProject: "subyard-default"},
		Environment: []string{"CODING_TOOL_INTEGRATIONS=claude"}, Integrations: []profile.IntegrationDefinition{}}
	mappings := []PersistMapping{{Integration: "claude", Source: "/home/dev/.claude/projects", Target: "/mnt/host/agent-sessions/claude/projects", Selected: true}}
	stderr, _ := json.Marshal(map[string]any{"conflicts": []map[string]string{
		{"reason": "private future reason", "path": "/home/dev/.claude/projects/x", "integration": "claude"},
		{"reason": "persist filesystem error", "path": "/home/dev/.claude/projects/p", "detail": "private detail: relative/name", "integration": "claude"},
		{"reason": "persist filesystem error", "path": "/home/dev/.claude/projects", "detail": "Permission denied", "integration": "claude"},
		{"reason": "atomic exchange unsupported", "path": "", "detail": "Invalid argument", "integration": "private-undeclared"},
		{"reason": "persist paths overlap", "path": "/home/dev/.claude/projects", "detail": "/home/dev/.claude/private", "integration": "private-undeclared"},
		{"reason": "persist space unavailable", "path": "/mnt/host/agent-sessions/claude/projects", "detail": "needs 10 bytes plus a 1 GiB reserve; 5 available", "integration": "claude"},
		{"reason": "persist target entry refused", "path": "/mnt/host/agent-sessions/claude/projects/y", "detail": "Unknown error 999", "integration": "claude"},
		{"reason": "persist paths overlap", "path": "/home/dev/.claude/projects", "detail": "private relative detail", "integration": "claude"},
		{"reason": "unsafe persist data", "path": "private relative path", "integration": "claude"},
		{"reason": "persist mount boundary", "path": "/srv/private-elsewhere", "integration": "claude"},
	}, "entries": []map[string]any{{"source": "/home/dev/private-undeclared", "persist": "deferred"}}})
	err := runtime.persistRefusal(stderr, mappings)
	text := err.Error()
	for _, want := range []string{"refused (unrecognized reason) at \"/home/dev/.claude/projects\" (an entry below it, sha256:", "(unrecognized detail)", "(Permission denied)",
		"atomic exchange unsupported (Invalid argument)", "for \"/home/dev/.claude/projects\" and \"/home/dev/.claude\" (an entry below it, sha256:",
		"(the source size plus a 1 GiB reserve is not available)", "'df' '-h' '--' '/mnt/host/agent-sessions/claude/'", "(unknown operating system error)",
		"for \"/home/dev/.claude/projects\" and an unrecognized path;", "unsafe persist data at an unrecognized path; data was preserved",
		"persist transfer in yard default: an unrecognized path is a mount point"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal omitted %q: %s", want, text)
		}
	}
	if strings.Contains(text, "private") || strings.Contains(text, "'disable'") || strings.Contains(text, "AGENT_private") || strings.Contains(text, `"unrecognized detail"`) ||
		strings.Contains(text, "999") || strings.Contains(text, "10 bytes") {
		t.Fatalf("refusal rendered undeclared guest text: %s", text)
	}
	var refused persistRefused
	if errors.As(err, &refused) {
		t.Fatal("an undeclared entry was carried to the inventory projection")
	}
}

func TestIntegrationPackageSourcesReportEveryUnreadableProvision(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	runtime := Runtime{RepositoryRoot: root, Yard: domain.Context{YardName: "sample", DevUser: "dev", DevUID: 1000},
		Environment: []string{"CODING_TOOL_INTEGRATIONS=codex first second", "HOST_CODEX_AGENTS_MD=" + filepath.Join(root, "codex.md"),
			"AGENT_first_PROVISION=" + filepath.Join(root, "first.sh"), "AGENT_second_PROVISION=" + filepath.Join(root, "second.sh")}}
	bindInstructionTestPackage(t, &runtime, root, "codex")
	reads := 0
	_, err := runtime.integrationArtifactsWithSourceReader("apply", func(asset config.MaterializedAsset) ([]byte, error) {
		if base := filepath.Base(asset.Source); base == "first.sh" || base == "second.sh" || base == "codex.md" {
			reads++
			return nil, errors.New("private unreadable " + base)
		}
		return asset.ReadSource()
	})
	var findings *domain.Refusals
	failure := runtime.stepFailure(stepArtifacts, "", err)
	if reads != 3 || !errors.As(failure, &findings) || len(findings.Unwrap()) != 3 {
		t.Fatalf("reads=%d, refusals: %v", reads, failure)
	}
	var messages []string
	for _, finding := range findings.Unwrap() {
		message, _ := finding.(*stepFailure).ActivationDiagnostic()
		messages = append(messages, message)
	}
	slices.Sort(messages)
	if !slices.Equal(messages, []string{"integration codex apply artifacts failed in yard sample", "integration first apply artifacts failed in yard sample",
		"integration second apply artifacts failed in yard sample"}) {
		t.Fatalf("package refusals = %q", messages)
	}
}

func TestProvisionDeclarationRefusalNamesItsIntegration(t *testing.T) {
	runtime := Runtime{Yard: domain.Context{YardName: "sample"}, Integrations: []profile.IntegrationDefinition{},
		Environment: []string{"CODING_TOOL_INTEGRATIONS=sample", "AGENT_sample_CHECK=bad check"}}
	_, err := runtime.provisionAgentChecks()
	failure := runtime.stepFailure(stepVerify, "", err)
	message, retry := failure.(*stepFailure).ActivationDiagnostic()
	if message != "integration sample apply verify failed in yard sample" || retry != "yard -Y sample integration status sample --json" {
		t.Fatalf("diagnostic = %q / %q", message, retry)
	}
}

func TestInventoryKeepsEachUnrecognizedConflict(t *testing.T) {
	incus := runningIncus(1)
	incus.ExecSteps[0].Result = ports.InstanceExecResult{ExitCode: 1,
		Stderr: []byte(`{"conflicts":[{"reason":"private first","path":"/x"},{"reason":"private second","path":"/y"}]}`)}
	incus.ExecSteps[0].Err = &ports.GuestExitError{Code: 1}
	runtime := Runtime{RepositoryRoot: testkit.TempDir(t), Integrations: []profile.IntegrationDefinition{}, Executor: incus,
		Yard: domain.Context{YardName: "sample", YardInstanceName: "yard", IncusProject: "subyard", DevUser: "dev", DevUID: 1000}}
	_, err := runtime.integrationInventory(context.Background(), "apply", nil, "")
	var findings *domain.Refusals
	if !errors.As(err, &findings) || len(findings.Unwrap()) != 2 || strings.Contains(err.Error(), "private") || !IntegrationOwnershipConflict(err) {
		t.Fatalf("unrecognized conflicts: %v", err)
	}
}

func TestIntegrationArtifactsReportEveryUnreadableSource(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	runtime := Runtime{RepositoryRoot: root, Yard: domain.Context{YardName: "sample", DevUser: "dev", DevUID: 1000},
		Environment: []string{"CODING_TOOL_INTEGRATIONS=claude codex",
			"HOST_CLAUDE_MD=" + filepath.Join(root, "claude.md"), "HOST_CODEX_AGENTS_MD=" + filepath.Join(root, "codex.md")}}
	bindInstructionTestPackage(t, &runtime, root, "claude")
	bindInstructionTestPackage(t, &runtime, root, "codex")
	reads := 0
	_, err := runtime.integrationArtifactsWithSourceReader("apply", func(asset config.MaterializedAsset) ([]byte, error) {
		if strings.HasSuffix(asset.Source, ".md") {
			reads++
			return nil, errors.New("private unreadable " + filepath.Base(asset.Source))
		}
		return asset.ReadSource()
	})
	failure := runtime.stepFailure(stepArtifacts, "", err)
	var findings *domain.Refusals
	if reads != 2 || !errors.As(failure, &findings) || len(findings.Unwrap()) != 2 {
		t.Fatalf("reads=%d, refusals: %v", reads, failure)
	}
	var messages []string
	for _, finding := range findings.Unwrap() {
		message, _ := finding.(*stepFailure).ActivationDiagnostic()
		messages = append(messages, message)
	}
	slices.Sort(messages)
	if !slices.Equal(messages, []string{"integration claude apply artifacts failed in yard sample", "integration codex apply artifacts failed in yard sample"}) {
		t.Fatalf("artifact refusals = %q", messages)
	}
}

func TestProvisionedHookCaptureNamesEveryMissingHook(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	provision := filepath.Join(root, "provision.sh")
	testkit.WriteFile(t, provision, []byte("#!/bin/sh\nexit 0\n"), 0o700)
	executor := &retryableIntegrationExecutor{observeHooks: func() hookObservation {
		return hookObservation{Projects: strings.Repeat("a", 64), Wiring: strings.Repeat("b", 64)}
	}}
	var definitions []profile.IntegrationDefinition
	for _, name := range []string{"first", "second"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, name, "integration.json"), []byte("{}"), 0o600)
		definitions = append(definitions, profile.IntegrationDefinition{Name: name, Root: filepath.Join(root, name)})
	}
	runtime := Runtime{RepositoryRoot: root, Executor: executor, Integrations: definitions,
		Incus: &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}},
		Yard:  domain.Context{YardName: "sample", DevUser: "dev", DevUID: 1000},
		Environment: []string{"CODING_TOOL_INTEGRATIONS=first second", "AGENT_first_PROVISION=" + provision, "AGENT_first_PROJECTS_CHANGED=first-hook",
			"AGENT_second_PROVISION=" + provision, "AGENT_second_PROJECTS_CHANGED=second-hook"}}
	scope, _, err := runtime.IntegrationScope()
	if err != nil {
		t.Fatal(err)
	}
	runtime.HookPlan = &ProjectHookPlan{integrationScope: scope}
	failure := runtime.stepFailure(stepHookCapture, "", runtime.captureProvisionedProjectHooks(context.Background()))
	var findings *domain.Refusals
	if !errors.As(failure, &findings) || len(findings.Unwrap()) != 2 || !errors.Is(failure, domain.ErrPlanStale) {
		t.Fatalf("hook capture refusals: %v", failure)
	}
	for index, owner := range []string{"first", "second"} {
		message, retry := findings.Unwrap()[index].(*stepFailure).ActivationDiagnostic()
		if message != "integration "+owner+" apply hook capture failed in yard sample" || retry != "yard -Y sample integration status "+owner+" --json" {
			t.Fatalf("hook capture diagnostic = %q / %q", message, retry)
		}
	}
}

func TestStepFailureKeepsSourceErrorsForTheBoundary(t *testing.T) {
	source := config.Catalog{}.ValidateIntegrationInitSettings(map[string]string{"UNSUPPORTED_FIELD": "x"})
	var typed *config.SourceError
	if !errors.As(source, &typed) {
		t.Fatalf("fixture is not a source error: %v", source)
	}
	runtime := Runtime{Yard: domain.Context{YardName: "default"}, Integrations: []profile.IntegrationDefinition{}}
	// Unchanged, the boundary renders it with its own source diagnostic and repair retry.
	if failure := runtime.stepFailure(stepSettings, "", source); failure != source {
		t.Fatalf("settings capture source error was wrapped: %v", failure)
	}
}

func TestIntegrationLinkRefusalKeepsFileSourceRefusals(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	runtime := Runtime{RepositoryRoot: root, Yard: domain.Context{YardName: "sample", DevUser: "dev", DevUID: 1000},
		Environment: []string{"CODING_TOOL_INTEGRATIONS=claude codex",
			"HOST_CLAUDE_MD=" + filepath.Join(root, "claude.md"), "HOST_CODEX_AGENTS_MD=" + filepath.Join(root, "codex.md"),
			"INTEGRATION_HOST_LINKS=.foo:/srv/foo"}}
	bindInstructionTestPackage(t, &runtime, root, "claude")
	bindInstructionTestPackage(t, &runtime, root, "codex")
	_, err := runtime.integrationArtifactsWithSourceReader("apply", func(asset config.MaterializedAsset) ([]byte, error) {
		if strings.HasSuffix(asset.Source, ".md") {
			return nil, errors.New("private unreadable " + filepath.Base(asset.Source))
		}
		return asset.ReadSource()
	})
	var findings *domain.Refusals
	failure := runtime.stepFailure(stepArtifacts, "", err)
	if !errors.As(failure, &findings) || len(findings.Unwrap()) != 3 {
		t.Fatalf("link refusal hid file-source refusals: %v", failure)
	}
	var messages []string
	for _, finding := range findings.Unwrap() {
		message, _ := finding.(*stepFailure).ActivationDiagnostic()
		messages = append(messages, message)
	}
	slices.Sort(messages)
	if !slices.Equal(messages, []string{"integration apply artifacts failed in yard sample", "integration claude apply artifacts failed in yard sample",
		"integration codex apply artifacts failed in yard sample"}) {
		t.Fatalf("artifact refusals = %q", messages)
	}
}

func TestInventoryNumbersOnlyUnrecognizedConflicts(t *testing.T) {
	incus := runningIncus(1)
	incus.ExecSteps[0].Result = ports.InstanceExecResult{ExitCode: 1, Stderr: []byte(`{"conflicts":[` +
		`{"reason":"owned artifact drift","path":"/home/dev/.codex/AGENTS.md"},{"reason":"private first","path":"/x"},{"reason":"private second","path":"/y"}]}`)}
	incus.ExecSteps[0].Err = &ports.GuestExitError{Code: 1}
	runtime := Runtime{RepositoryRoot: testkit.TempDir(t), Integrations: []profile.IntegrationDefinition{}, Executor: incus,
		Yard: domain.Context{YardName: "sample", YardInstanceName: "yard", IncusProject: "subyard", DevUser: "dev", DevUID: 1000}}
	entries := []integrationArtifact{{ID: "codex", Kind: "file", Path: "/home/dev/.codex/AGENTS.md"}}
	_, err := runtime.integrationInventory(context.Background(), "apply", entries, "")
	var findings *domain.Refusals
	if !errors.As(err, &findings) || len(findings.Unwrap()) != 3 || strings.Contains(err.Error(), "private") ||
		!strings.Contains(err.Error(), "(finding 1 of 2)") || !strings.Contains(err.Error(), "(finding 2 of 2)") {
		t.Fatalf("conflicts: %v", err)
	}
}

func TestPersistRefusalWithoutPathInspectsStatus(t *testing.T) {
	runtime := Runtime{Yard: domain.Context{YardName: "default", YardInstanceName: "yard-default", IncusProject: "subyard-default"},
		Environment: []string{"CODING_TOOL_INTEGRATIONS=claude"}, Integrations: []profile.IntegrationDefinition{}}
	stderr, _ := json.Marshal(map[string]any{"conflicts": []map[string]string{
		{"reason": "persist filesystem error", "path": "", "detail": "Permission denied", "integration": "claude"},
	}})
	var public interface{ ActivationDiagnostic() (string, string) }
	if err := runtime.persistRefusal(stderr, nil); !errors.As(err, &public) {
		t.Fatalf("not public: %v", err)
	}
	message, retry := public.ActivationDiagnostic()
	if !strings.Contains(message, "filesystem error at an unrecognized path (Permission denied)") || retry != "'yard' '-Y' 'default' 'integration' 'status'" {
		t.Fatalf("diagnostic = %q / %q", message, retry)
	}
}
