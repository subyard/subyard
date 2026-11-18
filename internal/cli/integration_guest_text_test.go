package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/rpc"
)

type integrationGuestExec func(ports.InstanceExecRequest) (ports.InstanceExecResult, error)

func (execute integrationGuestExec) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	return execute(request)
}

type integrationGuestBlocker struct{ cause error }

func (integrationGuestBlocker) ID() string { return "materialized-config" }
func (blocker integrationGuestBlocker) Observe(context.Context, releasetransition.ReleasePair, releasetransition.ReleaseLinks) (releasetransition.V2ActivationObservation, error) {
	return releasetransition.V2ActivationObservation{}, blocker.cause
}
func (integrationGuestBlocker) Reconcile(context.Context, releasetransition.ReleaseLinks) error {
	return errors.New("a blocked integration must not reconcile")
}

func TestIntegrationGuestTextStaysPrivate(t *testing.T) {
	const marker = "guest-text-sentinel-b7419c8e"
	const declared = "/home/dev/.codex/AGENTS.md"
	const source = "/home/dev/.memory"
	const target = "/mnt/host/agent-sessions/codex/memory"
	const backup = source + ".subyard-backup-0123456789abcdef"
	const hookPath = "/usr/local/libexec/fixture-hook"
	ctx := context.Background()
	program, incus, _, _, output := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=codex\n")
	root := program.options.RepositoryRoot
	instructions := filepath.Join(root, "instructions.md")
	settingsFile := filepath.Join(root, "settings.json")
	provision := filepath.Join(root, "scripts", "fixture-provision.sh")
	script := filepath.Join(root, "scripts", "reconcile-integrations.sh")
	writeCLIFile(t, provision, "#!/bin/sh\nexit 0\n", 0700)
	writeCLIFile(t, instructions, "Host instructions\n", 0600)
	writeCLIFile(t, settingsFile, `{"retained": true}`, 0600)
	writeCLIFile(t, filepath.Join(root, "config/projects-changed.sh"), "#!/bin/sh\nexit 0\n", 0644)
	writeCLIFile(t, filepath.Join(root, "config/host.env"), "HOST_CODEX_AGENTS_MD="+instructions+"\n", 0600)
	writeCLIFile(t, filepath.Join(root, "config/agents.env"), "CODING_TOOL_INTEGRATIONS=\nAGENT_codex_COMMAND=codex\n"+
		"AGENT_codex_PERSIST=.memory:"+target+"\nAGENT_codex_PROJECTS_CHANGED=fixture-hook\nAGENT_codex_HEALTH=fixture-health\n"+
		"AGENT_codex_CONFIG="+settingsFile+"\nAGENT_codex_CONFIG_DEST=.codex/config.json\nAGENT_codex_PROVISION="+provision+"\n"+
		"AGENT_relay_COMMAND=relay\nAGENT_relay_CONFIG="+settingsFile+"\nAGENT_relay_CONFIG_DEST=.relay/config.json\n", 0600)
	declareCLIInstructionFixture(t, root, "codex", "HOST_CODEX_AGENTS_MD", ".codex/AGENTS.md", "Codex instructions")
	declareCLIInstructionFixture(t, root, "relay", "HOST_CODEX_AGENTS_MD", ".relay/AGENTS.md", "Relay instructions")
	catalog, err := config.LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	program.catalog = catalog
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: incus.Instances["subyard/yard"]}
	program.options.IntegrationRuntime = nil

	digest := strings.Repeat("a", 64)
	encode := func(value any) []byte {
		t.Helper()
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	artifact := func(owner, kind, path, format string) map[string]any {
		return map[string]any{"id": owner, "kind": kind, "path": path, "format": format,
			"digest": marker, "source_digest": marker, "target": marker, "content": marker}
	}
	oldDocument := "/home/dev/.relay/config.json"
	appliedSettings := catalog.IntegrationInitSettings(loaded.Environment)
	inventory := map[string]any{
		"fingerprint": digest, "initial": true, "installed": true, "changed": true,
		"adopted":  []any{artifact(marker, marker, "/home/dev/"+marker, marker), artifact("codex", "file", declared, marker)},
		"retired":  []any{artifact("relay", "structured", oldDocument, "json"), artifact(marker, marker, marker, marker)},
		"facts":    []any{map[string]any{"key": "structured:" + oldDocument, "desired": marker, "observed": marker}},
		"selected": []string{"codex"}, "applied_settings": appliedSettings,
		"diagnostics": map[string]any{"key": marker, "digests": map[string]string{marker: marker},
			"files": map[string]any{marker: map[string]string{"path": marker, "digest": marker}}},
	}
	persistEntry := map[string]any{"integration": marker, "source": source, "target": marker, "ops": []string{"finish", marker},
		"id": marker, "state": marker, "persist": marker, "identity": []string{marker}, "target_identity": []string{marker}, "existing": []string{marker}}
	persistUnmatched := map[string]any{"source": marker, "ops": []string{marker}, "persist": marker}
	conflict := func(reason, path, owner string) map[string]string {
		return map[string]string{"reason": reason, "path": path, "integration": owner, "detail": marker}
	}
	mode := ""
	seen := map[string]int{}
	committed := false
	program.options.Executor = integrationGuestExec(func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		args := request.Command
		ok := func(value any) (ports.InstanceExecResult, error) {
			return ports.InstanceExecResult{Stdout: encode(value), Stderr: []byte(marker)}, nil
		}
		refuse := func(value any) (ports.InstanceExecResult, error) {
			return ports.InstanceExecResult{ExitCode: 2, Stdout: []byte(marker), Stderr: encode(value)}, &ports.GuestExitError{Code: 2}
		}
		if len(args) > 4 && args[0] == "python3" {
			switch {
			case args[3] == reconcileruntime.IntegrationInventoryReader():
				seen["inventory "+args[4]]++
				if mode == "refusals" && strings.HasPrefix(args[4], "observe") || mode == "inventory "+args[4] {
					return refuse(map[string]any{"reason": marker, "path": marker, "detail": marker, "initial": true,
						"conflicts": []any{conflict("owned artifact drift", "/home/dev/"+marker+"-first", marker),
							conflict("owned artifact drift", "/home/dev/"+marker+"-second", marker),
							conflict("unowned selected artifact", declared, marker)}})
				}
				if args[4] == "commit" {
					committed = true
				}
				if committed && strings.HasPrefix(args[4], "observe") {
					return ok(map[string]any{"fingerprint": digest, "installed": true})
				}
				return ok(inventory)
			case strings.Contains(args[3], "class Refusals") && args[4] == "retained":
				seen["persist retained"]++
				return ok(map[string]any{"retained": []any{map[string]any{"integration": marker, "kind": "backup", "path": backup, "bytes": 5},
					map[string]any{"integration": marker, "kind": "backup", "path": source + ".subyard-backup-" + marker}}})
			case strings.Contains(args[3], "class Refusals"):
				seen["persist "+args[4]]++
				entries := []any{persistEntry, persistUnmatched}
				if mode == "refusals" || mode == "persist "+args[4] {
					return refuse(map[string]any{"error": marker, "entries": entries, "conflicts": []any{
						conflict("persist target entry refused", target+"/"+marker+"-a", "codex"),
						conflict("persist target entry refused", target+"/"+marker+"-b", "codex"),
						conflict(marker, marker, marker)}})
				}
				report := map[string]any{"fingerprint": digest, "entries": entries}
				if mode == "malformed persist" {
					report["fingerprint"] = marker
				}
				if args[4] == "apply" {
					report["results"] = []any{map[string]any{"source": source, "not_transferred": []string{marker}, "retained": []string{marker}},
						map[string]any{"source": marker}}
				}
				if committed {
					report["entries"] = []any{map[string]any{"source": source, "persist": "link"}}
				}
				return ok(report)
			case strings.Contains(args[3], "def retire("):
				seen["structured "+args[4]]++
				if mode == "structured "+args[4] {
					return ports.InstanceExecResult{ExitCode: 2, Stdout: []byte(marker), Stderr: []byte("config materialization: unowned-invalid-destination-v1 JSON; " + marker)}, &ports.GuestExitError{Code: 2}
				}
				report := map[string]any{"converged": committed || args[4] == "retire" || mode == "config status", "fingerprint": digest}
				if args[4] == "assess-retire" || args[4] == "retire" {
					report["release"], report["released"], report["released_count"] = "changed", []string{"/" + marker, "/retained"}, 2
				}
				if mode == "malformed structured config" {
					report["fingerprint"], report["release"] = marker, marker
				}
				return ok(report)
			}
		}
		if len(args) > 5 && strings.Contains(args[3], "mode=$1") {
			seen["services "+args[5]]++
			if mode == "refusals" || mode == "services "+args[5] {
				return ports.InstanceExecResult{ExitCode: 1, Stderr: []byte("Bubblewrap AppArmor: " + marker)}, nil
			}
			return ports.InstanceExecResult{Stdout: []byte("changed=0\n" + marker), Stderr: []byte(marker)}, nil
		}
		if args[0] == "bash" && args[len(args)-1] == "--observe" {
			seen["hook inputs"]++
			report := map[string]any{"projects": digest, "wiring": digest, "roots": []string{"/srv/" + marker}, "hooks": []string{"fixture-hook", marker},
				"facts": map[string]string{marker: digest, "fixture-hook": digest}, "resolved": map[string]string{marker: "/opt/" + marker, "fixture-hook": hookPath}}
			if mode == "malformed hooks" || mode == "hooks capture" && seen["inventory apply"] != 0 {
				report["projects"], report["wiring"] = marker, marker
			}
			return ok(report)
		}
		if len(args) > 2 && strings.Contains(args[2], "exec timeout") {
			seen["hook dispatch"]++
			result := ports.InstanceExecResult{Stdout: []byte("subyard-project-hook-failed:/opt/" + marker + "\nsubyard-project-hook-failed:" + hookPath + "\n"), Stderr: []byte(marker)}
			if mode == "hooks run" {
				result.ExitCode = 124
			}
			return result, nil
		}
		if args[0] == "fixture-health" {
			seen["health"]++
			return ok(map[string]string{"state": marker})
		}
		return ports.InstanceExecResult{Stdout: []byte(marker), Stderr: []byte(marker)}, nil
	})
	runtime := program.initPlatform(loaded, nil).(reconcileruntime.Runtime)
	checkPublic := func(label string, value any) {
		t.Helper()
		if bytes.Contains(encode(value), []byte(marker)) {
			t.Errorf("%s disclosed guest text", label)
		}
	}
	plan, err := runtime.PrepareSourceIntegrationPlan(ctx, func(asset config.MaterializedAsset) ([]byte, error) { return asset.ReadSource() }, true)
	if err != nil || len(plan.Adoption) != 1 || plan.Adoption[0] != declared || !plan.HasOwnershipReleases() {
		t.Fatalf("fixture must exercise adoption and retirement: %#v, %v", plan.Adoption, err)
	}
	checkPublic("prospective release plan", plan)
	steps := plan.OperationSteps("subyard/yard")
	if err := domain.ValidateOperationSteps(steps); err != nil {
		t.Fatal(err)
	}
	program.printOperationSteps(domain.OperationPlan{OperationID: "fixture", Steps: steps})
	program.printInitPlan(&initExecution{loaded: loaded, integrationAdoption: plan, integrationWarnings: plan.Warnings})
	if !strings.Contains(output.String(), "retiring relay releases ownership") || !strings.Contains(output.String(), "take matching legacy path under integration management") {
		t.Fatalf("plan renderers did not show the adoption and retirement: %s", output.String())
	}
	checkPublic("operation and init plans", output.String())

	registry, err := os.ReadFile(filepath.Join(repositoryRoot(t), "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseTransitionTestFile(t, filepath.Join(root, "config", "release-transition.json"), registry, 0600)
	runtimeRoot := t.TempDir()
	for _, release := range []string{"release-a", "release-b"} {
		if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", release), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/release-a", filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	request := releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1, Mode: releasetransition.ProcessInspect,
		RuntimeRoot: runtimeRoot, ConfigHome: loaded.Context.Paths.ConfigHome,
		Target: "release-b", Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: releasetransition.Fingerprint(digest),
	}
	checkBlocker := func(t *testing.T, label string, cause error) string {
		t.Helper()
		boundary := materializedConfigActivationError{cause: cause, yard: "default", phase: "integration plan", action: "inspect", command: "integration status"}
		var stderr bytes.Buffer
		response, err := executeReleaseTransitionRequest(releasetransition.WithMigrationDiagnosticWriter(ctx, &stderr), root, request,
			func(releasetransition.PlanToken, releasetransition.Authorization) bool { return false },
			[]releasetransition.V2ActivationReconciler{integrationGuestBlocker{boundary.collected()}}, releaseTransitionOwnerFixture{}, nil)
		if err != nil || response.Inspection == nil || len(response.Inspection.Blockers) != 1 || !strings.HasPrefix(response.Inspection.Blockers[0].Resource, "activation.materialized-config") {
			t.Fatalf("%s did not reach the release blocker: %#v / %v", label, response.Inspection, err)
		}
		checkPublic(label+" release JSON", response)
		output.Reset()
		program.printUpdateResult(&releaseExecution{prepared: releaseruntime.Prepared{Action: "update.activate"}, inspection: response.Inspection}, false)
		checkPublic(label+" release text", output.String())
		checkPublic(label+" release stderr", stderr.String())
		return stderr.String()
	}

	runtime.AdoptLegacyIntegrations = true
	runtime.LegacyIntegrationFingerprint = plan.AdoptionFingerprint
	initDefinition, _ := program.manifest.Lookup("init")
	admitPreparedFixture(program)
	for _, scenario := range []struct{ name, step, message string }{
		{"services apply", "services apply", "Bubblewrap AppArmor"},
		{"persist apply", "persist apply", "persist"},
		{"inventory apply", "inventory apply", "owned selected artifact"},
		{"inventory configs-prepare", "inventory configs-prepare", "apply inventory configs prepare"},
		{"structured apply", "structured apply", "invalid configuration preserved"},
		{"inventory configs-commit", "inventory configs-commit", "apply inventory configs commit"},
		{"script", "structured apply", "integration apply script"},
		{"hooks capture", "hook inputs", "apply hooks capture"},
		{"hooks run", "hook dispatch", "fixture-hook"},
		{"structured retire", "structured retire", "retire structured config"},
		{"inventory commit", "inventory commit", "inventory commit"},
		{"inventory capture-settings", "inventory capture-settings", "inventory capture settings"},
		{"success", "inventory capture-settings", ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			output.Reset()
			clear(seen)
			mode, committed = scenario.name, false
			body := "#!/bin/sh\nprintf 'codex\\n'\n"
			if mode == "script" {
				body = "#!/bin/sh\nprintf '%s\\n' '" + marker + "'\nexit 1\n"
			}
			writeCLIFile(t, script, body, 0700)
			execution := runtime
			hooks, err := execution.PrepareProjectHooks(ctx, false)
			if err != nil || hooks.Binding() == "" {
				t.Fatalf("prepare hook plan: %v", err)
			}
			execution.HookPlan = hooks
			prepared := &preparedCommand{CLI: program, Loaded: loaded, Definition: initDefinition,
				policy: commandPolicy(initDefinition, loaded.Context, nil, nil), steps: func() []domain.OperationStep { return steps },
				assess: func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
					return "yard.init.reconcile", domain.ActionDelta{Changed: true}, nil
				},
				execute: func(ctx context.Context, _ *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
					err = execution.ApplyIntegrations(ctx, plan)
					return domain.AdapterResult{Status: "ok"}, err
				},
			}
			if err := prepared.preparePlan(ctx); err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			code := program.runPreparedCommand(ctx, prepared, true)
			if seen[scenario.step] == 0 || (err == nil) != (scenario.message == "") || err != nil && !strings.Contains(err.Error(), scenario.message) {
				t.Fatalf("did not reach or report %s: calls %v, err %v", scenario.step, seen, err)
			}
			if (code == 0) != (err == nil) || err != nil && !strings.Contains(output.String(), scenario.message) {
				t.Fatalf("init did not render the apply result: %d / %s", code, output.String())
			}
			if err != nil {
				if mode == "hooks run" && !strings.Contains(err.Error(), "optional agent project hooks timed out") {
					t.Fatalf("dispatcher timeout lost beside attributed failure: %v", err)
				}
				if mode == "inventory apply" && len(domain.RefusalCauses(err)) != 3 {
					t.Fatalf("independent inventory conflicts collapsed: %v", err)
				}
				var wire bytes.Buffer
				if err := rpc.NewCodec(nil, &wire).Write(rpc.Response{Error: operationRPCError(rpc.CodeOperationFailed, err)}); err != nil {
					t.Fatal(err)
				}
				checkPublic("init RPC error", wire.String())
			}
			checkPublic("init output", output.String())
			if err != nil {
				checkBlocker(t, scenario.name, err)
			}
		})
	}

	committed = false
	handler := &rpcHandler{cli: program, loaded: loaded}
	for _, scenario := range []string{"", "refusals"} {
		mode = scenario
		output.Reset()
		statusText, err := program.queryIntegrationStatus(ctx, loaded, "codex")
		if err != nil || len(statusText.PersistRetained) != 1 || statusText.PersistRetained[0].Path != backup {
			t.Fatalf("retained backup missing from integration status: %#v / %v", statusText.PersistRetained, err)
		}
		program.printIntegrationStatus(statusText, false)
		if !strings.Contains(output.String(), "persist retained backup "+backup+" (5 bytes)") {
			t.Fatalf("retained backup missing from human status: %s", output.String())
		}
		checkPublic("integration status text", output.String())
		output.Reset()
		program.printIntegrationStatus(statusText, true)
		checkPublic("integration status JSON", output.String())
		status, err := handler.Handle(ctx, rpc.Call{Method: rpc.MethodIntegrationStatus, Params: json.RawMessage(`{}`)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var wire bytes.Buffer
		if err := rpc.NewCodec(nil, &wire).Write(rpc.Response{Result: status}); err != nil || bytes.Contains(wire.Bytes(), []byte(marker)) {
			t.Fatalf("RPC status encoding exposed guest text or failed: %v", err)
		}
	}
	mode = "config status"
	for _, scenario := range []struct {
		selected string
		metadata bool
		code     int
	}{{"codex", false, 0}, {marker, false, 1}, {marker, true, 1}} {
		inventory["selected"] = []string{scenario.selected}
		settings := maps.Clone(appliedSettings)
		if scenario.metadata {
			settings["AGENT_"+marker+"_COMMAND"] = "fixture-command"
		}
		inventory["applied_settings"] = settings
		output.Reset()
		before := seen["inventory selection"]
		code := program.runConfig(ctx, loaded, []string{"status"})
		if seen["inventory selection"] == before || code != scenario.code {
			t.Fatalf("config status did not assess the applied selection: %d / %s", code, output.String())
		}
		checkPublic("config status", output.String())
	}
	inventory["selected"] = []string{"codex"}
	inventory["applied_settings"] = appliedSettings
	mode = "refusals"
	_, _, err = runtime.InspectPersistTransfers(ctx)
	if err == nil || !strings.Contains(err.Error(), "refused for entry 1") || !strings.Contains(err.Error(), "refused for entry 2") {
		t.Fatal("fixture did not expose both init persist refusals")
	}
	output.Reset()
	program.reportPreparationError(initDefinition, err)
	checkPublic("init persist refusal", output.String())
	reads := seen["persist observe"]
	_, err = runtime.PrepareSourceIntegrationPlan(ctx, func(asset config.MaterializedAsset) ([]byte, error) {
		if asset.Source == instructions {
			return nil, errors.New("unreadable instruction source")
		}
		return asset.ReadSource()
	}, true)
	if err == nil || seen["persist observe"] <= reads || !strings.Contains(err.Error(), "refused for entry 2") {
		t.Fatalf("unreadable instruction source hid persist refusal: %v / %v", err, seen)
	}
	_, cause := runtime.IntegrationPlan(ctx)
	if cause == nil {
		t.Fatal("fixture did not refuse activation observation")
	}
	if stderr := checkBlocker(t, "observation", cause); stderr == "" || !strings.Contains(stderr, "retry:") {
		t.Fatal("fixture did not exercise release overflow diagnostics on stderr")
	}

	// Check malformed variants of fields that must be valid to reach a plan.
	for _, scenario := range []string{"malformed persist", "malformed structured config"} {
		mode = scenario
		if _, err := runtime.IntegrationPlan(ctx); err == nil {
			t.Fatalf("%s was accepted", scenario)
		} else {
			checkPublic(scenario, err.Error())
		}
	}
	mode = "malformed hooks"
	if _, err := runtime.PrepareProjectHooks(ctx, false); err == nil {
		t.Fatal("malformed hook observation was accepted")
	} else {
		checkPublic(mode, err.Error())
	}

	mode = ""
	inventory["applied_settings"] = map[string]string{marker: marker}
	if _, err := runtime.AppliedIntegrationConfiguration(ctx); err == nil {
		t.Fatal("malformed applied settings were accepted")
	} else {
		checkPublic("malformed applied settings", err.Error())
	}
	inventory["fingerprint"] = marker
	if _, err := runtime.IntegrationPlan(ctx); err == nil {
		t.Fatal("malformed inventory was accepted")
	} else {
		checkPublic("malformed inventory", err.Error())
	}
}
