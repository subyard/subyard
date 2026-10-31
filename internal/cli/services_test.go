package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/ownerservice"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func exactOwnerSessionFixture(t *testing.T, handler *rpcHandler) *ownerRPCSession {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() {
		err := (rpc.Session{Handler: handler, Capabilities: []string{exactPlanCapability, operationStepsCapability}}).Serve(context.Background(), server, server)
		handler.closePlans()
		done <- err
	}()
	t.Cleanup(func() {
		_ = client.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("owner session leaked")
		}
	})
	session := &ownerRPCSession{codec: rpc.NewCodec(client, client), close: client.Close}
	if err := session.negotiate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return session
}

func infrastructureFixture(t *testing.T, cli *CLI, incus *testkit.Incus, id string) *serviceSelection {
	return infrastructureYardFixture(t, cli, incus, id, "default")
}

func infrastructureYardFixture(t *testing.T, cli *CLI, incus *testkit.Incus, id, yard string) *serviceSelection {
	t.Helper()
	descriptor := `{"schema_version":1,"infrastructure_service":{"id":"` + id + `","default_yard":"fixture-runtime","required_settings":{"YARD_KIND":"container"},"route_root":"fixture-routes"}}`
	root := filepath.Join(cli.options.RepositoryRoot, "config/profiles/synthetic-service")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "profile.json"), []byte(descriptor), 0o644)
	loaded, err := cli.loadServiceYard(yard)
	if err != nil {
		t.Fatal(err)
	}
	home, _, owner, err := cli.servicePaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "fixture-routes"), 0o755); err != nil {
		t.Fatal(err)
	}
	instance := incus.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName]
	if instance.Config == nil {
		instance.Config = map[string]string{}
	}
	instance.Config["volatile.uuid"] = "fixture-uuid"
	instance.Config["user.subyard.managed"] = "true"
	instance.Config["user.subyard.name"] = loaded.Context.YardName
	incus.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName] = instance
	incus.Reconcile = ports.ReconcileState{ProjectFound: true, InstanceFound: true, Instance: instance, ProjectConfig: map[string]string{
		"user.subyard.installation": "1", "user.subyard.operator_uid": strconv.Itoa(os.Getuid()), "user.subyard.data_home": loaded.Context.Paths.DataHome, "user.subyard.config_home": loaded.Context.Paths.ConfigHome, "user.subyard.yard_name": loaded.Context.YardName, "user.subyard.instance_name": loaded.Context.YardInstanceName,
	}}
	definition, err := cli.serviceDefinition(id)
	if err != nil {
		t.Fatal(err)
	}
	uuid, ownership, err := cli.serviceRuntime(context.Background(), loaded, definition)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := ownerservice.NewGeneration()
	if err != nil {
		t.Fatal(err)
	}
	selection := &serviceSelection{definition: definition, registry: ownerservice.Registry{Root: filepath.Join(home, "services")}, publication: filepath.Join(home, "fixture-routes/services"), record: ownerservice.Registration{Public: ownerservice.Public{SchemaVersion: 1, Owner: owner, Service: id, Pool: "default", Yard: loaded.Context.YardName, Project: loaded.Context.IncusProject, Instance: loaded.Context.YardInstanceName, UUID: uuid, Generation: generation}, Ownership: ownership}}
	if err := selection.registry.Save(selection.record); err != nil {
		t.Fatal(err)
	}
	if err := selection.registry.Publish(selection.record, selection.publication); err != nil {
		t.Fatal(err)
	}
	return selection
}

func TestInfrastructureServiceRegistrationHasNoLifecycleEffects(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	incus := lifecycleIncus()
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus})
	if err != nil {
		t.Fatal(err)
	}
	selection := infrastructureFixture(t, program, incus, "fixture-service")
	if err := selection.registry.Remove(selection.record, selection.publication); err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	definition := command.Definition{Name: "service", Handler: "@service", Remote: command.RemoteForward, Effect: command.EffectMutate, Confirmation: command.ConfirmationDynamic, Visibility: command.VisibilityPublic}
	prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: []string{"register", "fixture-service", "--yard", "default"}, ExplicitYard: true})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	orchestrator := program.operationOrchestrator(prepared.Plan.OperationID, prepared.Loaded, nil, &definition)
	if _, err := prepared.Confirm(context.Background(), orchestrator, true); err != nil {
		t.Fatal(err)
	}
	result, err := prepared.Execute(context.Background(), orchestrator, io.Discard)
	if err != nil || result.Status != "ok" {
		t.Fatalf("registration: %v %#v", err, result)
	}
	registered, err := selection.registry.Resolve(selection.record.Owner, "fixture-service", "")
	if err != nil {
		t.Fatal(err)
	}
	if registered.UUID != selection.record.UUID || len(incus.PowerUpdates) != 0 || len(incus.ConfigUpdates) != 0 {
		t.Fatal("registration changed native runtime")
	}
}

func TestInfrastructureServiceOwnerRenameRequiresExplicitVerifiedRepair(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	incus := lifecycleIncus()
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus})
	if err != nil {
		t.Fatal(err)
	}
	selection := infrastructureFixture(t, program, incus, "fixture-service")
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(loaded.Context.Paths.ConfigHome, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(loaded.Context.Paths.ConfigHome, "host-id"), []byte("renamed-owner\n"), 0o600)
	_, err = program.resolveServiceCommand("fixture-service", nil)
	var failure *ownerservice.ResolutionError
	if !errors.As(err, &failure) || failure.Code != "service_owner_changed" || !strings.Contains(failure.Error(), "service register") {
		t.Fatalf("rename did not refuse with supported repair: %v", err)
	}
	if err := program.checkServiceSelection(context.Background(), loaded, selection); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("old owner plan remained valid: %v", err)
	}
	definition := command.Definition{Name: "service", Handler: "@service", Remote: command.RemoteForward, Effect: command.EffectMutate, Confirmation: command.ConfirmationDynamic, Visibility: command.VisibilityPublic}
	prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: []string{"register", "fixture-service", "--yard", "default"}})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	orchestrator := program.operationOrchestrator(prepared.Plan.OperationID, prepared.Loaded, nil, &definition)
	if _, err := prepared.Confirm(context.Background(), orchestrator, true); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Execute(context.Background(), orchestrator, io.Discard); err != nil {
		t.Fatal(err)
	}
	repaired, err := program.resolveServiceCommand("fixture-service", nil)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.record.Owner != "renamed-owner" || repaired.record.Generation == selection.record.Generation || repaired.record.UUID != selection.record.UUID || repaired.record.Ownership != selection.record.Ownership || len(incus.PowerUpdates) != 0 || len(incus.ConfigUpdates) != 0 {
		t.Fatal("owner metadata repair changed native runtime or retained old generation")
	}
}

func TestInfrastructureServiceLifecycleExecutesOnlyParentApproval(t *testing.T) {
	for _, mode := range []string{"stop", "start", "update", "update-converged", "remove"} {
		action := strings.TrimSuffix(mode, "-converged")
		for _, outcome := range []string{"success", "stale", "unauthorized", "forged-authority"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				root, environment, _ := nativeFixture(t)
				incus := lifecycleIncus()
				instance := incus.Instances["subyard/yard"]
				if action != "start" {
					instance.Status = "Running"
					instance.Config["user.subyard.desired_power"] = "running"
				}
				incus.Instances["subyard/yard"] = instance
				platform := newInitPlatformFixture()
				if mode == "update-converged" {
					platform.converged[ports.ReconcileStageProject] = true
				}
				prompt := &testkit.Prompt{Answers: []bool{true}}
				runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{Schema: 1, Status: "ok"}, Apply: func(request domain.AdapterRequest) {
					if request.Adapter == "lifecycle" {
						if err := incus.SetInstancePower(context.Background(), "subyard", "yard", action, false); err != nil {
							t.Fatal(err)
						}
					}
					if request.Adapter == "teardown" {
						incus.Reconcile = ports.ReconcileState{}
						delete(incus.Instances, "subyard/yard")
					}
				}}}}
				program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus, InitPlatform: platform, AdapterRunner: runner, NetworkPolicy: allowTestNetworkPolicy(), Prompt: prompt})
				if err != nil {
					t.Fatal(err)
				}
				selection := infrastructureFixture(t, program, incus, "fixture-service")
				if action == "remove" {
					helper, err := os.ReadFile("../../scripts/lib/teardown-plan.py")
					if err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(filepath.Join(root, "scripts/lib"), 0o700); err != nil {
						t.Fatal(err)
					}
					testkit.WriteFile(t, filepath.Join(root, "scripts/lib/teardown-plan.py"), helper, 0o644)
				}
				loaded, err := program.loadContext("default")
				if err != nil {
					t.Fatal(err)
				}
				definition := command.Definition{Name: "service", Handler: "@service", Remote: command.RemoteForward, Effect: command.EffectMutate, Confirmation: command.ConfirmationDynamic, Visibility: command.VisibilityPublic}
				prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: []string{action, "fixture-service"}})
				if err != nil {
					t.Fatal(err)
				}
				defer prepared.Close()
				if action == "start" && (prepared.Plan.Assessment == nil || prepared.Plan.Assessment.Action != "yard.start" || !prepared.stepsComplete || len(prepared.Plan.Steps) < 2 || !strings.Contains(prepared.Plan.Steps[0].Target, selection.record.Generation)) {
					t.Fatalf("service start lost prepared lifecycle: %#v", prepared.Plan)
				}
				if len(incus.PowerUpdates) != 0 || len(incus.ConfigUpdates) != 0 {
					t.Fatal("preparation changed runtime")
				}
				approvedSteps := domain.CloneOperationSteps(prepared.Plan.Steps)
				orchestrator := program.operationOrchestrator(prepared.Plan.OperationID, prepared.Loaded, nil, &definition)
				if outcome != "unauthorized" {
					if _, err := prepared.Confirm(context.Background(), orchestrator, false); err != nil {
						t.Fatal(err)
					}
				}
				if outcome == "forged-authority" {
					// Copying a confirmed plan into a new orchestrator does not
					// transfer the private authority recorded by the one prompt.
					orchestrator = program.operationOrchestrator(prepared.Plan.OperationID, prepared.Loaded, nil, &definition)
				}
				if outcome == "stale" {
					replacement := selection.record
					replacement.Generation = strings.Repeat("b", 64)
					payload, _ := json.Marshal(replacement)
					testkit.WriteFile(t, filepath.Join(selection.registry.Root, "fixture-service/default.json"), payload, 0o600)
				}
				result, err := prepared.Execute(context.Background(), orchestrator, io.Discard)
				if !domain.EqualOperationSteps(approvedSteps, prepared.Plan.Steps) {
					t.Fatal("delegation changed approved steps")
				}
				if outcome != "success" {
					if outcome == "stale" && !errors.Is(err, domain.ErrPlanStale) || outcome == "unauthorized" && !errors.Is(err, domain.ErrConfirmationRequired) || outcome == "forged-authority" && !errors.Is(err, domain.ErrActionPolicyInvalid) {
						t.Fatalf("%s: %v", outcome, err)
					}
					if len(runner.Requests) != 0 || len(platform.applied) != 0 || len(incus.PowerUpdates) != 0 {
						t.Fatal("unapproved child produced effects")
					}
					return
				}
				if err != nil || result.Status != "ok" {
					t.Fatalf("confirmed %s: %#v %v", action, result, err)
				}
				if len(prompt.Requests) != 1 {
					t.Fatalf("parent consent count=%d", len(prompt.Requests))
				}
				if action == "update" {
					if mode != "update-converged" && len(platform.applied) == 0 || len(runner.Requests) != 0 || platform.projectHooks != 1 {
						t.Fatal("service update did not use native initialization")
					}
				} else {
					if len(runner.Requests) != 1 {
						t.Fatalf("child requests=%d", len(runner.Requests))
					}
					request := runner.Requests[0]
					if action == "remove" && (request.Adapter != "teardown" || request.Action != "apply") || action != "remove" && (request.Adapter != "lifecycle" || request.Action != action) {
						t.Fatalf("wrong child action: %#v", request)
					}
				}
			})
		}
	}
}

func TestInfrastructureServiceExactOwnerRPCRejectsStaleTargets(t *testing.T) {
	for _, kind := range []string{"success", "registration", "owner", "uuid", "ownership", "expected-owner"} {
		t.Run(kind, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			environment = append(environment, "NESTED_E2E_VMS=1")
			incus := lifecycleIncus()
			instance := incus.Instances["subyard/yard"]
			instance.Status = "Running"
			incus.Instances["subyard/yard"] = instance
			probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":2,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":7,"lease_epoch":3,"state":"held"}]}}`)}
			runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{Schema: 1, OperationID: "service-rpc", Status: "ok"}, Apply: func(domain.AdapterRequest) {
				probe.output = bytes.Replace(probe.output, []byte(`"state":"held"`), []byte(`"state":"available"`), 1)
			}}}}
			program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus, ProjectData: probe, AdapterRunner: runner})
			if err != nil {
				t.Fatal(err)
			}
			selection := infrastructureFixture(t, program, incus, "test-vms")
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			handler := &rpcHandler{cli: program, loaded: loaded}
			session := exactOwnerSessionFixture(t, handler)
			var plan exactOperationPlan
			arguments := []string{"revoke", "--slot", "1"}
			if kind == "expected-owner" {
				arguments = append(arguments, "--expect-owner", "another-owner")
			}
			planErr := session.call(context.Background(), "operation.plan", "service-rpc", map[string]any{"command": "test-vms", "arguments": arguments, "exact": true, "stepSchema": 1}, &plan)
			if kind == "expected-owner" {
				var fault *rpc.Error
				if !errors.As(planErr, &fault) || fault.Code != domain.PlanStaleCode || len(runner.Requests) != 0 {
					t.Fatalf("wrong owner was planned: %v", planErr)
				}
				return
			}
			if err := planErr; err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "registration", "owner":
				replacement := selection.record
				if kind == "registration" {
					replacement.Generation, _ = ownerservice.NewGeneration()
				} else {
					replacement.Owner = "another-owner"
				}
				payload, _ := json.Marshal(replacement)
				testkit.WriteFile(t, filepath.Join(selection.registry.Root, replacement.Service, replacement.Pool+".json"), payload, 0o600)
			case "uuid":
				incus.Reconcile.Instance.Config["volatile.uuid"] = "replacement-uuid"
			case "ownership":
				incus.Reconcile.ProjectConfig["user.subyard.operator_uid"] = "-1"
			}
			if kind != "success" && program.checkServiceSelection(context.Background(), loaded, selection) == nil {
				t.Fatal("changed target accepted by service guard")
			}
			_, err = session.execute(context.Background(), plan)
			if kind == "success" {
				if err != nil || len(runner.Requests) != 1 {
					t.Fatalf("owner execute: %v calls=%d", err, len(runner.Requests))
				}
			} else {
				var fault *rpc.Error
				code := domain.PlanStaleCode
				if kind == "ownership" {
					code = "internal"
				}
				if !errors.As(err, &fault) || fault.Code != code || len(runner.Requests) != 0 {
					t.Fatalf("stale owner target applied: %v calls=%d", err, len(runner.Requests))
				}
			}
		})
	}
}

func TestInfrastructureServiceSelectionIgnoresWorkspaceAndRefusesAmbiguity(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	incus := lifecycleIncus()
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus})
	if err != nil {
		t.Fatal(err)
	}
	selection := infrastructureFixture(t, program, incus, "fixture-service")
	program.env["SUBYARD_YARD"] = "missing-workspace"
	definition := command.Definition{Name: "fixture-service", Handler: "@test-vms"}
	yard, _, err := program.selectServiceInvocation(context.Background(), definition, []string{"status"})
	if err != nil || yard != selection.record.Yard {
		t.Fatalf("workspace changed service: %q %v", yard, err)
	}
	second := selection.record
	second.Pool = "another"
	second.UUID = "other-uuid"
	if err := selection.registry.Save(second); err != nil {
		t.Fatal(err)
	}
	if err := selection.registry.Publish(second, selection.publication); err != nil {
		t.Fatal(err)
	}
	_, _, err = program.selectServiceInvocation(context.Background(), definition, []string{"status"})
	var failure *ownerservice.ResolutionError
	if !errors.As(err, &failure) || failure.Code != "service_ambiguous" {
		t.Fatalf("ambiguous service selected: %v", err)
	}
	yard, _, err = program.selectServiceInvocation(context.Background(), definition, []string{"status", "--pool", "default"})
	if err != nil || yard != selection.record.Yard {
		t.Fatalf("exact pool failed: %q %v", yard, err)
	}
}

func TestInfrastructureServiceErrorsAreStructured(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	var output bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Stdout: &output})
	if err != nil {
		t.Fatal(err)
	}
	failure := &ownerservice.ResolutionError{Code: "service_absent", Owner: "fixture-owner", Service: "fixture-service"}
	if code := program.reportServiceError([]string{"status", "--json"}, failure); code != 1 {
		t.Fatal(code)
	}
	var response map[string]any
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["code"] != "service_absent" || response["owner"] != "fixture-owner" {
		t.Fatalf("unsafe or incomplete error: %s", output.String())
	}
}

func sameJSON(left, right []byte) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && reflect.DeepEqual(a, b)
}
func registerStructuredOwnerFixture(t *testing.T, cli *CLI, owner, destination string) {
	t.Helper()
	home, _, _, err := cli.servicePaths()
	if err != nil {
		t.Fatal(err)
	}
	trust, err := ownerinventory.NewSSHHostTrust(destination + " ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	if err := (ownerinventory.Connections{Root: filepath.Join(home, "owner-inventory")}).Write(ownerinventory.Connection{HostID: owner, Destination: destination, Trust: &trust}); err != nil {
		t.Fatal(err)
	}
}
