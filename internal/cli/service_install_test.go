package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/ownerservice"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type serviceInstallPlatform struct {
	*initPlatformFixture
	installed func()
}

func (platform *serviceInstallPlatform) ApplyStage(ctx context.Context, stage ports.ReconcileStageID) error {
	if err := platform.initPlatformFixture.ApplyStage(ctx, stage); err != nil {
		return err
	}
	if stage == ports.ReconcileStageFinalize {
		platform.installed()
	}
	return nil
}

func TestInfrastructureServiceInstallBootstrapsOnlyAfterParentConsent(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent-runtime", true: "foreign-same-name"}[foreign], func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			environment = slices.DeleteFunc(environment, func(value string) bool { return strings.HasPrefix(value, "SSH_PORT=") })
			profileRoot := filepath.Join(root, "config/profiles/synthetic-service")
			presetRoot := filepath.Join(root, "config/yards/profiles")
			for _, path := range []string{profileRoot, presetRoot, filepath.Join(root, "data/fixture-routes")} {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, filepath.Join(profileRoot, "profile.json"), []byte(`{"schema_version":1,"infrastructure_service":{"id":"fixture-service","default_yard":"fixture-runtime","required_settings":{"YARD_KIND":"container"},"route_root":"fixture-routes"}}`), 0o644)
			testkit.WriteFile(t, filepath.Join(profileRoot, "yard.env"), []byte("YARD_TEMPLATE=synthetic-service\n"), 0o644)
			testkit.WriteFile(t, filepath.Join(presetRoot, "synthetic-service.env"), []byte("YARD_KIND=container\nSSH_PORT=2290\nALLOWS_CODING_TOOLS=false\n"), 0o644)
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
			platform := &serviceInstallPlatform{initPlatformFixture: newInitPlatformFixture()}
			platform.converged[ports.ReconcileStageInstance] = false
			prompt := &testkit.Prompt{Answers: []bool{true}}
			program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus, InitPlatform: platform, Prompt: prompt})
			if err != nil {
				t.Fatal(err)
			}
			definition := command.Definition{Name: "service", Handler: "@service", Remote: command.RemoteForward, Effect: command.EffectMutate, Confirmation: command.ConfirmationDynamic, Visibility: command.VisibilityPublic}
			yard, arguments, err := program.selectServiceInvocation(context.Background(), definition, []string{"install", "fixture-service"})
			if err != nil || yard != "fixture-runtime" {
				t.Fatalf("declared default: %s %v", yard, err)
			}
			loaded, bootstrap, err := program.loadServiceContext(yard, arguments)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Context.SSHPort != 2290 || loaded.Environment["ALLOWS_CODING_TOOLS"] != "false" || bootstrap == nil {
				t.Fatal("declared preset was not retained")
			}
			if foreign {
				incus.Reconcile = ports.ReconcileState{ProjectFound: true, InstanceFound: true, Instance: ports.InstanceInfo{Name: loaded.Context.YardInstanceName, Config: map[string]string{"volatile.uuid": "foreign"}}}
			}
			prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Bootstrap: bootstrap, Definition: definition, Arguments: arguments, ExplicitYard: true})
			target := filepath.Join(root, "state/yards/fixture-runtime/config.env")
			registryRoot := filepath.Join(root, "data/services")
			assertAbsent := func() {
				t.Helper()
				for _, path := range []string{target, registryRoot} {
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("preconsent write: %s %v", path, err)
					}
				}
			}
			assertAbsent()
			if foreign {
				if err == nil {
					prepared.Close()
					t.Fatal("foreign instance was accepted")
				}
				if len(platform.applied) != 0 {
					t.Fatal("foreign runtime changed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			platform.installed = func() {
				c := prepared.Loaded.Context
				instance := ports.InstanceInfo{Name: c.YardInstanceName, Project: c.IncusProject, Status: "Running", Config: map[string]string{"user.subyard.managed": "true", "user.subyard.name": c.YardName, "volatile.uuid": "installed-service-uuid"}}
				incus.Instances[c.IncusProject+"/"+c.YardInstanceName] = instance
				incus.Reconcile = ports.ReconcileState{ProjectFound: true, InstanceFound: true, Instance: instance, ProjectConfig: map[string]string{"user.subyard.installation": "1", "user.subyard.operator_uid": strconv.Itoa(os.Getuid()), "user.subyard.data_home": c.Paths.DataHome, "user.subyard.config_home": c.Paths.ConfigHome, "user.subyard.yard_name": c.YardName, "user.subyard.instance_name": c.YardInstanceName}}
			}
			orchestrator := program.operationOrchestrator(prepared.Plan.OperationID, prepared.Loaded, nil, &definition)
			if _, err := prepared.Confirm(context.Background(), orchestrator, false); err != nil {
				t.Fatal(err)
			}
			assertAbsent()
			result, err := prepared.Execute(context.Background(), orchestrator, io.Discard)
			if err != nil || result.Status != "ok" {
				t.Fatalf("install: %#v %v", result, err)
			}
			if len(prompt.Requests) != 1 || !slices.Contains(platform.applied, ports.ReconcileStageInstance) {
				t.Fatal("installation bypassed parent consent/native init")
			}
			content, err := os.ReadFile(target)
			if err != nil || string(content) != "YARD_TEMPLATE=synthetic-service\n" {
				t.Fatalf("unsafe bootstrap registration: %q %v", content, err)
			}
			_, _, owner, err := program.servicePaths()
			if err != nil {
				t.Fatal(err)
			}
			registry := ownerservice.Registry{Root: registryRoot}
			record, err := registry.Resolve(owner, "fixture-service", "")
			if err != nil {
				t.Fatal(err)
			}
			if record.Pool != "default" || record.UUID != "installed-service-uuid" || record.Yard != "fixture-runtime" || registry.Published(record, filepath.Join(root, "data/fixture-routes/services")) != nil {
				t.Fatal("installation did not publish exact owned runtime")
			}
			if _, err := program.loadServiceYard(record.Yard); err != nil {
				t.Fatalf("installed preset cannot be reloaded: %v", err)
			}
		})
	}
}

func TestInfrastructureServicePreparationRetainsResolvedPool(t *testing.T) {
	for _, invocation := range []struct{ transport, action string }{{"local", "install"}, {"owner-rpc", "install"}, {"local", "register"}, {"owner-rpc", "register"}} {
		transport, action := invocation.transport, invocation.action
		for _, scope := range []string{"sole-main", "ambiguous", "explicit-main", "unregistered"} {
			t.Run(action+"/"+transport+"/"+scope, func(t *testing.T) {
				root, environment, _ := nativeFixture(t)
				manifest := filepath.Join(root, "config/commands.registry")
				content, err := os.ReadFile(manifest)
				if err != nil {
					t.Fatal(err)
				}
				content = append(content, []byte("\nservice||@service||forward|mutate|dynamic|public|lifecycle|simple|service <command> ID|manage synthetic service|--owner --pool --yard --yes --help|register install start stop update remove\n")...)
				testkit.WriteFile(t, manifest, content, 0o644)
				incus := lifecycleIncus()
				platform := newInitPlatformFixture()
				var output, diagnostics bytes.Buffer
				arguments := []string{action, "fixture-service"}
				if scope == "explicit-main" {
					arguments = append(arguments, "--pool", "main")
				}
				if action == "register" || scope == "unregistered" {
					arguments = append(arguments, "--yard", "default")
				}
				program, err := New(Options{RepositoryRoot: root, Arguments: append([]string{"service"}, append(slices.Clone(arguments), "--yes")...), Environment: environment, Incus: incus, InitPlatform: platform, Stdout: &output, Stderr: &diagnostics})
				if err != nil {
					t.Fatal(err)
				}
				selection := infrastructureFixture(t, program, incus, "fixture-service")
				if err := selection.registry.Remove(selection.record, selection.publication); err != nil {
					t.Fatal(err)
				}
				main := selection.record
				main.Pool = "main"
				if scope != "unregistered" {
					if err := selection.registry.Save(main); err != nil {
						t.Fatal(err)
					}
					if err := selection.registry.Publish(main, selection.publication); err != nil {
						t.Fatal(err)
					}
				}
				if scope == "ambiguous" || scope == "explicit-main" {
					second := main
					second.Pool = "second"
					if err := selection.registry.Save(second); err != nil {
						t.Fatal(err)
					}
				}
				var operationErr error
				if transport == "local" {
					if code := program.Run(context.Background()); code != 0 {
						operationErr = errors.New(diagnostics.String())
					}
				} else {
					loaded, err := program.loadContext("default")
					if err != nil {
						t.Fatal(err)
					}
					handler := &rpcHandler{cli: program, loaded: loaded}
					session := exactOwnerSessionFixture(t, handler)
					var plan exactOperationPlan
					operationErr = session.call(context.Background(), "operation.plan", "install-pool", map[string]any{"command": "service", "arguments": append(slices.Clone(arguments), "--expect-owner", main.Owner), "exact": true}, &plan)
					if operationErr == nil {
						_, operationErr = session.execute(context.Background(), plan)
					}
				}
				if scope == "ambiguous" {
					if operationErr == nil || !strings.Contains(operationErr.Error(), "service_ambiguous") || len(platform.applied) != 0 {
						t.Fatalf("ambiguous install: %v effects=%v", operationErr, platform.applied)
					}
					return
				}
				if operationErr != nil {
					t.Fatal(operationErr)
				}
				if action == "register" && len(platform.applied) != 0 {
					t.Fatal("registration invoked runtime initialization")
				}
				expected := "main"
				if scope == "unregistered" {
					expected = "default"
				}
				record, err := selection.registry.Resolve(main.Owner, main.Service, expected)
				if err != nil || record.Pool != expected || scope != "unregistered" && record != main {
					t.Fatalf("install changed selected registration: %#v %v", record, err)
				}
				if scope != "unregistered" {
					if _, err := os.Lstat(filepath.Join(selection.registry.Root, main.Service, "default.json")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("install created an unrequested default pool", err)
					}
				}
				if scope == "sole-main" {
					records, err := selection.registry.List(main.Service)
					if err != nil || len(records) != 1 {
						t.Fatal("sole pool became ambiguous", err)
					}
				}
			})
		}
	}
}
