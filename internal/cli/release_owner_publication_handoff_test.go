package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/sys/unix"
)

type handoffLifecycleObserver struct{ physicalStartupObserver }

func (*handoffLifecycleObserver) CheckedInvocation() (string, string, error) {
	return strings.Repeat("a", 64), strings.Repeat("b", 64), nil
}

// The private user namespace exercises real protected-file and SDK paths. Its
// manager stub proves delivery ordering, not physical PID1 installation.
func TestOwnerPublicationHandoffReturnsThroughOriginalCapsule(t *testing.T) {
	if os.Getenv("SUBYARD_TEST_PUBLICATION_NONROOT_CONSUMER") == "1" {
		if os.Geteuid() == 0 {
			t.Fatal("non-root consumer control remained root")
		}
		var input struct {
			Environment []string
			Consumer    ownerpublication.Consumer
			Authorized  bool
		}
		if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
			t.Fatal("consumer control input", err)
		}
		runtime := reconcileruntime.Runtime{Environment: input.Environment}
		if err := runtime.CheckPublicationConsumer(context.Background(), input.Consumer); err != nil {
			t.Fatal("exact protected consumer rejected before scheduling", err)
		}
		observer := &handoffLifecycleObserver{}
		ctx := ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(context.Background(), observer), strings.Repeat("c", 64))
		err := runtime.StartPublicationConsumer(ctx, input.Consumer)
		if !input.Authorized {
			if !errors.Is(err, domain.ErrPlanStale) || observer.commands != 0 {
				t.Fatal("unauthorized consumer did not refuse before command", err, observer.commands)
			}
		} else if err != nil || observer.commands != 1 || len(observer.commandTerminals) != 1 {
			t.Fatal("authorized consumer did not use actual command boundary", err, observer.commands)
		}
		t.Logf("exact consumer: authorized=%t commands=%d terminals=%d stale=%t", input.Authorized, observer.commands, len(observer.commandTerminals), errors.Is(err, domain.ErrPlanStale))
		return
	}
	if os.Getenv("SUBYARD_TEST_PUBLICATION_CLI_NS") != "1" {
		unshare, e := exec.LookPath("unshare")
		if e != nil {
			t.Skip("user namespace unavailable")
		}
		namespace := []string{"--user", "--map-auto", "--map-root-user", "--mount"}
		subordinate := exec.Command(unshare, append(namespace, "true")...).Run() == nil
		if !subordinate {
			namespace = []string{"--user", "--map-root-user", "--mount"}
			if exec.Command(unshare, append(namespace, "true")...).Run() != nil {
				t.Skip("user namespace unavailable")
			}
		}
		command := exec.Command(unshare, append(namespace, os.Args[0], "-test.v", "-test.run=^TestOwnerPublicationHandoffReturnsThroughOriginalCapsule$", "-test.count=1")...)
		command.Env = append(os.Environ(), "SUBYARD_TEST_PUBLICATION_CLI_NS=1")
		if subordinate {
			command.Env = append(command.Env, "SUBYARD_TEST_PUBLICATION_NONROOT_MAP=1")
		}
		if output, e := command.CombinedOutput(); e != nil {
			t.Fatal(e, string(output))
		} else {
			t.Logf("executed private namespace: %s", output)
		}
		return
	}
	for _, kind := range []domain.YardKind{domain.YardVM, domain.YardContainer} {
		t.Run(string(kind), func(t *testing.T) {
			cli, loaded, host, _, lock, fallback := ownerLifecycleKindFixture(t, kind, kind == domain.YardVM, "")
			delete(cli.env, "SUBYARD_NO_AUDIT")
			delete(cli.baseEnv, "SUBYARD_NO_AUDIT")
			delete(loaded.Environment, "SUBYARD_NO_AUDIT")
			nativeInstallationFixture(t, environmentList(cli.baseEnv, nil))
			root := cli.options.RepositoryRoot
			sourceRoot := repositoryRoot(t)
			seedCurrentReleaseLedger(t, root, loaded.Context.Paths.ConfigHome, sourceRoot)
			i, e := host.Instance(context.Background(), "subyard", "yard")
			if e != nil {
				t.Fatal(e)
			}
			cfg := maps.Clone(i.LocalConfig)
			cfg[previewroute.Key] = "v1:100.127.255.254:23456"
			receipt, _ := json.Marshal(ownerpublication.Receipt{Version: 1, Ownership: cfg[previewroute.Key], Device: previewroute.Device("100.127.255.254", "23456")})
			receiptKey := ownerpublication.Prefix + previewroute.DeviceName
			cfg[receiptKey] = string(receipt)
			host.server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "type": ownerLifecycleAPIKind(kind), "status": "Stopped", "config": cfg, "expanded_config": maps.Clone(cfg)})
			for _, path := range []string{"config/systemd", "consumer-state"} {
				if e := os.MkdirAll(filepath.Join(root, path), 0700); e != nil {
					t.Fatal(e)
				}
			}
			template, e := os.ReadFile(filepath.Join(sourceRoot, "config/systemd/subyard-publication-reconcile.service.in"))
			if e != nil {
				t.Fatal(e)
			}
			testkit.WriteFile(t, filepath.Join(root, "config/systemd/subyard-publication-reconcile.service.in"), template, 0644)
			powerTemplate, e := os.ReadFile("../../config/systemd/subyard-power-reconcile.service.in")
			if e != nil {
				t.Fatal(e)
			}
			testkit.WriteFile(t, filepath.Join(root, "config/systemd/subyard-power-reconcile.service.in"), powerTemplate, 0644)
			installed := filepath.Join(root, "consumer-state")
			engine := filepath.Join(installed, "engine")
			power := filepath.Join(installed, "power.service")
			passive := filepath.Join(installed, ownerpublication.HandoffService)
			signal := filepath.Join(installed, "start")
			image, e := os.ReadFile("/proc/self/exe")
			if e != nil {
				t.Fatal(e)
			}
			testkit.WriteFile(t, engine, image, 0755)
			manager := filepath.Join(installed, "systemctl")
			script := "#!/bin/sh\ncase \"$1\" in\nshow) printf 'LoadState=loaded\\nNeedDaemonReload=no\\nFragmentPath=" + passive + "\\nDropInPaths=\\n';;\nstart) [ \"$2\" = --no-block ] && [ \"$3\" = " + ownerpublication.HandoffService + " ] || exit 2; : > " + signal + ";;\n*) exit 2;;\nesac\n"
			testkit.WriteFile(t, manager, []byte(script), 0755)
			if e := unix.Mount(manager, "/usr/bin/systemctl", "", unix.MS_BIND, ""); e != nil {
				t.Fatal("private manager mount", e)
			}
			defer unix.Unmount("/usr/bin/systemctl", 0)
			testkit.WriteFile(t, filepath.Join(root, "scripts/reconcile-integrations.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
			testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
			values := maps.Clone(loaded.Environment)
			values["PATH"] = installed + ":/usr/bin:/bin"
			values["SUBYARD_POWER_ENGINE_SOURCE"] = engine
			values["SUBYARD_POWER_RECONCILER_PATH"] = engine
			values["SUBYARD_POWER_UNIT_PATH"] = power
			values["SUBYARD_USER"] = "root"
			environment := environmentList(values, nil)
			runtime := reconcileruntime.Runtime{RepositoryRoot: root, LogicalRepositoryRoot: root, Environment: environment, Yard: loaded.Context, Incus: host}
			prepared, e := runtime.PrepareHostPowerRuntime(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			defer prepared.Close()
			consumerPlan, e := runtime.PreparePublicationConsumer(prepared)
			if e != nil {
				t.Fatal(e)
			}
			contracts, e := ownerpublication.Load(root)
			if e != nil {
				t.Fatal(e)
			}
			encoded, e := ownerpublication.Encode(contracts)
			if e != nil {
				t.Fatal(e)
			}
			body := ownerpublication.RenderPublicationUnit(string(template), engine, power, encoded)
			testkit.WriteFile(t, power, []byte(strings.ReplaceAll(string(powerTemplate), "@SUBYARD_POWER_RECONCILER@", engine)+"\n[Service]\nEnvironment=SUBYARD_PUBLICATION_CONTRACTS="+encoded+"\n"), 0644)
			testkit.WriteFile(t, passive, []byte(body), 0644)
			prepared.Close()
			prepared, e = runtime.PrepareHostPowerRuntime(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			defer prepared.Close()
			consumerPlan, e = runtime.PreparePublicationConsumer(prepared)
			if e != nil {
				t.Fatal(e)
			}
			consumer, e := runtime.CapturePublicationConsumer(consumerPlan)
			if e != nil {
				t.Fatal(e)
			}
			original, e := captureOwnerStart(context.Background(), cli, loaded, root)
			if e != nil {
				t.Fatal(e)
			}
			original.Publication = consumerPlan
			sources, e := runtime.PrepareOwnerRuntimeSources(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			if e := sources.Close(); e != nil {
				t.Fatal(e)
			}
			if e := os.MkdirAll(loaded.Context.Paths.DataHome, 0700); e != nil {
				t.Fatal(e)
			}
			unlock, e := cli.admitOwnerWrite(context.Background(), loaded)
			if e != nil {
				t.Fatal(e)
			}
			defer unlock()
			held, e := uninstall.BorrowOwnerLock(loaded.Context.Paths.DataHome)
			if e != nil || held == nil {
				t.Fatal("admitted owner descriptor unavailable", e)
			}
			before, e := held.Stat()
			if e != nil {
				t.Fatal(e)
			}
			held.Close()
			observer := &handoffLifecycleObserver{}
			ctx, cancel := context.WithTimeout(ports.WithNativeOwnerHome(ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(context.Background(), observer), strings.Repeat("c", 64)), loaded.Context.Paths.DataHome), 20*time.Second)
			defer cancel()
			acked := make(chan struct{})
			restore := make(chan struct{})
			workerDone := make(chan error, 1)
			go func() {
				workerCtx := ports.WithoutNativeOperationObserver(ctx)
				for {
					if _, e := os.Stat(signal); e == nil {
						break
					}
					select {
					case <-ctx.Done():
						workerDone <- ctx.Err()
						return
					case <-time.After(5 * time.Millisecond):
					}
				}
				current, e := host.Client.Instance(workerCtx, "subyard", "yard")
				if e != nil {
					workerDone <- e
					return
				}
				worker := ownerpublication.PublicationWorker{Contracts: contracts, Inventory: func(context.Context) ([]ports.InstanceInfo, error) { return []ports.InstanceInfo{current}, nil }, Consumer: func(call context.Context, expected ownerpublication.Consumer) error {
					return runtime.CheckPublicationConsumer(call, expected)
				}, Ack: func(call context.Context, h ownerpublication.Handoff, before, after string) error {
					e := host.Client.ExchangePublicationHandoff(call, h, contracts, before, after, func() error { return runtime.CheckPublicationConsumer(call, consumer) })
					if e == nil {
						close(acked)
					}
					return e
				}, Restore: func(context.Context, ports.InstanceInfo) ([]string, error) {
					select {
					case <-restore:
						return []string{"waiting"}, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}}
				waiting, e := worker.Run(workerCtx)
				if !waiting && e == nil {
					e = errors.New("receipt retry lost")
				}
				workerDone <- e
			}()
			children := &preparedOwnerChildren{operation: cli, inits: map[string]preparedOwnerInitChild{loaded.Context.YardName: {loaded: loaded, runtime: runtime, start: original, sources: sources, publicationConsumer: &consumer}}}
			e = children.runStart(ctx, loaded.Context.YardName, io.Discard)
			close(restore)
			if workerErr := <-workerDone; e != nil || workerErr != nil {
				t.Fatal("original capsule did not finish after ACK", e, workerErr)
			}
			select {
			case <-acked:
			default:
				t.Fatal("callback returned without delivery")
			}
			log, err := os.ReadFile(filepath.Join(loaded.Context.Paths.DataHome, "logs/yard.log"))
			if err != nil || !strings.Contains(string(log), " -- operation.started\n") || !strings.Contains(string(log), " -- operation.finished\n") {
				t.Fatal("actual admitted child audit records missing", err)
			}
			held, err = uninstall.BorrowOwnerLock(loaded.Context.Paths.DataHome)
			if err != nil || held == nil {
				t.Fatal("owner OFD lost after capsule", err)
			}
			after, err := held.Stat()
			if err != nil || !os.SameFile(before, after) || uninstall.ValidateOwnerLockDescriptor(loaded.Context.Paths.DataHome, uint32(os.Getuid()), held) != nil {
				t.Fatal("capsule owner identity changed", err)
			}
			held.Close()
			actual, e := host.Instance(context.Background(), "subyard", "yard")
			if e != nil || actual.Config[receiptKey] != string(receipt) || actual.Config[ownerpublication.HandoffPrefix+previewroute.DeviceName] != "" || len(actual.Devices[previewroute.DeviceName]) != 0 || lock.held || len(fallback.Requests) != 0 || host.cpuCalls != map[domain.YardKind]int{domain.YardVM: 2, domain.YardContainer: 0}[kind] || observer.unknown || len(observer.started) != len(observer.terminal) {
				t.Fatal("handoff changed listener/power or original custody", e, host.trace, observer)
			}
			if len(host.server.PowerCalls()) != 1 || host.server.PowerCalls()[0].Action != "start" {
				t.Fatal("passive worker changed power")
			}
			if kind == domain.YardContainer {
				t.Run("non-root-consumer", func(t *testing.T) {
					if os.Getenv("SUBYARD_TEST_PUBLICATION_NONROOT_MAP") != "1" {
						t.Skip("subordinate namespace unavailable")
					}
					// Keep root-owned protected files and execute the actual consumer
					// guard as a subordinate non-root user in this private namespace.
					// Only the test manager's start is inert; its checked show remains exact.
					testkit.WriteFile(t, manager, []byte(strings.Replace(script, ": > "+signal, "exit 0", 1)), 0755)
					sudo := filepath.Join(installed, "sudo")
					testkit.WriteFile(t, sudo, []byte("#!/bin/sh\n[ \"$1\" = -n ] && [ \"$2\" = -- ] || exit 2\nshift 2\nexec \"$@\"\n"), 0755)
					if err := unix.Mount(sudo, "/usr/bin/sudo", "", unix.MS_BIND, ""); err != nil {
						t.Fatal(err)
					}
					defer unix.Unmount("/usr/bin/sudo", 0)
					for _, path := range []string{root, filepath.Dir(root)} {
						if err := os.Chown(path, 1000, 1000); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.Chmod(installed, 0755); err != nil {
						t.Fatal(err)
					}
					if environmentMap(runtime.Environment)["SUBYARD_SUDO_PREAUTHORIZED"] != "" {
						t.Fatal("preparation fixture unexpectedly preauthorized runtime")
					}
					for _, authorized := range []bool{false, true} {
						childEnvironment := runtime.Environment
						if authorized {
							if err := cli.prepareSudoPrivileges(ctx, io.Discard, 0, "start"); err != nil {
								t.Fatal(err)
							}
							childEnvironment = environmentList(environmentMap(runtime.Environment), map[string]string{"SUBYARD_SUDO_PREAUTHORIZED": cli.env["SUBYARD_SUDO_PREAUTHORIZED"]})
						}
						input, err := json.Marshal(struct {
							Environment []string
							Consumer    ownerpublication.Consumer
							Authorized  bool
						}{childEnvironment, consumer, authorized})
						if err != nil {
							t.Fatal(err)
						}
						command := exec.CommandContext(ctx, engine, "-test.v", "-test.run=^TestOwnerPublicationHandoffReturnsThroughOriginalCapsule$", "-test.count=1")
						command.Dir = "/tmp"
						command.Env = []string{"PATH=/usr/bin:/bin", "SUBYARD_TEST_PUBLICATION_NONROOT_CONSUMER=1"}
						command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000}}
						command.Stdin = strings.NewReader(string(input))
						if output, err := command.CombinedOutput(); err != nil {
							t.Fatalf("non-root consumer authorized=%t: %v: %s", authorized, err, output)
						} else {
							t.Logf("non-root consumer authorized=%t: %s", authorized, output)
						}
					}
				})
			}

		})
	}
}
