package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/rpc"
)

type integrationGuestExec func(ports.InstanceExecRequest) (ports.InstanceExecResult, error)

func (execute integrationGuestExec) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	return execute(request)
}

func TestIntegrationGuestTextStaysPrivate(t *testing.T) {
	const marker = "guest-text-sentinel-b7419c8e"
	const declared = "/home/dev/.codex/AGENTS.md"
	const source = "/home/dev/.memory"
	const target = "/mnt/host/agent-sessions/codex/memory"
	ctx := context.Background()
	program, incus, _, _, output := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=codex\n")
	root := program.options.RepositoryRoot
	instructions := filepath.Join(root, "instructions.md")
	writeCLIFile(t, instructions, "Host instructions\n", 0600)
	writeCLIFile(t, filepath.Join(root, "config/projects-changed.sh"), "#!/bin/sh\nexit 0\n", 0644)
	writeCLIFile(t, filepath.Join(root, "config/host.env"), "HOST_CODEX_AGENTS_MD="+instructions+"\n", 0600)
	declareCLIInstructionFixture(t, root, "codex", "HOST_CODEX_AGENTS_MD", ".codex/AGENTS.md", "Codex instructions")
	catalog, err := config.LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	program.catalog = catalog
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Environment["AGENT_codex_PERSIST"] = ".memory:" + target
	loaded.Environment["AGENT_codex_PROJECTS_CHANGED"] = "fixture-hook"
	loaded.Environment["AGENT_codex_HEALTH"] = "fixture-health"
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
	oldDocument := "/home/dev/" + marker + ".json"
	inventory := map[string]any{
		"fingerprint": digest, "initial": true, "installed": true, "changed": true,
		"adopted":  []any{artifact(marker, marker, "/home/dev/"+marker, marker), artifact("codex", "file", declared, marker)},
		"retired":  []any{artifact("codex", "structured", oldDocument, "json"), artifact(marker, marker, marker, marker)},
		"facts":    []any{map[string]any{"key": "structured:" + oldDocument, "desired": marker, "observed": marker}},
		"selected": []string{marker}, "applied_settings": map[string]string{marker: marker},
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
	var progressSeen bool
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
				if mode == "refusals" || args[4] == "apply" {
					return refuse(map[string]any{"reason": marker, "path": marker, "detail": marker, "initial": true,
						"conflicts": []any{conflict(marker, marker, marker), conflict("unowned selected artifact", declared, marker)}})
				}
				return ok(inventory)
			case strings.Contains(args[3], "class Refusals") && args[4] == "retained":
				return ok(map[string]any{"retained": []any{map[string]string{"integration": marker, "kind": marker, "path": marker}}})
			case strings.Contains(args[3], "class Refusals"):
				entries := []any{persistEntry, persistUnmatched}
				if mode == "refusals" {
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
					progressSeen = true
					report["results"] = []any{map[string]any{"source": source, "not_transferred": []string{marker}, "retained": []string{marker}},
						map[string]any{"source": marker}}
				}
				return ok(report)
			case args[4] == "assess-retire":
				report := map[string]any{"converged": false, "fingerprint": digest, "release": "changed",
					"released": []string{"/" + marker}, "released_count": 1}
				if mode == "malformed structured config" {
					report["fingerprint"], report["release"] = marker, marker
				}
				return ok(report)
			}
		}
		if len(args) > 2 && strings.Contains(args[2], "mode=$1") {
			if mode == "refusals" {
				return ports.InstanceExecResult{ExitCode: 1, Stderr: []byte("Bubblewrap AppArmor: " + marker)}, nil
			}
			return ports.InstanceExecResult{Stdout: []byte("changed=0\n" + marker), Stderr: []byte(marker)}, nil
		}
		if args[0] == "bash" && args[len(args)-1] == "--observe" {
			report := map[string]any{"projects": digest, "wiring": digest, "roots": []string{marker}, "hooks": []string{marker},
				"facts": map[string]string{marker: marker}, "resolved": map[string]string{marker: marker, "fixture-hook": "/opt/" + marker + "/fixture-hook"}}
			if mode == "malformed hooks" {
				report["projects"], report["wiring"] = marker, marker
			}
			return ok(report)
		}
		if len(args) > 2 && strings.Contains(args[2], "exec timeout") {
			return ports.InstanceExecResult{ExitCode: 1, Stdout: []byte("subyard-project-hook-failed:/opt/" + marker + "/fixture-hook"), Stderr: []byte(marker)}, nil
		}
		if args[0] == "fixture-health" {
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
	checkPublic("release consequences", integrationAdoptionConsequences("default", plan))
	checkPublic("operation steps", plan.OperationSteps("subyard/yard"))
	checkPublic("warnings", plan.Warnings)

	runtime.AdoptLegacyIntegrations = true
	runtime.LegacyIntegrationFingerprint = plan.AdoptionFingerprint
	if err := runtime.ApplyIntegrations(ctx, plan); err == nil || !progressSeen {
		t.Fatalf("fixture did not reach persist progress and then refuse inventory apply: %v", err)
	} else {
		checkPublic("apply error", err.Error())
	}
	hooks := runtime
	hooks.PersistFinishOnly = true
	if err := hooks.RunProjectHooks(ctx); err == nil {
		t.Fatal("fixture did not exercise the hook failure")
	} else {
		checkPublic("hook failure", err.Error())
		finding, _ := domain.PublicFinding(err)
		checkPublic("hook diagnostic", finding.Message)
	}

	handler := &rpcHandler{cli: program, loaded: loaded}
	for _, scenario := range []string{"", "refusals"} {
		mode = scenario
		status, err := handler.Handle(ctx, rpc.Call{Method: rpc.MethodIntegrationStatus, Params: json.RawMessage(`{}`)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var wire bytes.Buffer
		if err := rpc.NewCodec(nil, &wire).Write(rpc.Response{Result: status}); err != nil || bytes.Contains(wire.Bytes(), []byte(marker)) {
			t.Fatalf("RPC status encoding exposed guest text or failed: %v", err)
		}
	}
	_, _, err = runtime.InspectPersistTransfers(ctx)
	if err == nil || !strings.Contains(err.Error(), "refused for entry 1") || !strings.Contains(err.Error(), "refused for entry 2") {
		t.Fatal("fixture did not expose both init persist refusals")
	}
	checkPublic("init guidance", err.Error())
	_, cause := runtime.IntegrationPlan(ctx)
	if cause == nil {
		t.Fatal("fixture did not refuse activation observation")
	}
	boundary := materializedConfigActivationError{cause: cause, yard: "default", phase: "integration plan", action: "inspect", command: "integration status"}
	for _, refusal := range boundary.appendTo(nil) {
		finding, _ := domain.PublicFinding(refusal)
		checkPublic("activation finding and recovery", []string{finding.Message, finding.Recovery})
	}
	checkPublic("activation summary", boundary.collected().Error())
	checkPublic("stdout and stderr", output.String())

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
