package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/Subyard/Subyard/internal/yardnetwork"
	"golang.org/x/sys/unix"
)

func ownerPublicationAPIKind(kind domain.YardKind) string {
	if kind == domain.YardVM {
		return "virtual-machine"
	}
	return "container"
}

type ownerPublicationFinalizerHost struct {
	*incusclient.Client
	server                       *testkit.IncusServer
	calls                        int
	mode                         string
	trace                        []string
	held                         bool
	powerWrites                  int
	probes, cpuCalls, generation int
	cancelAtBoundary             context.CancelFunc
}

func (h *ownerPublicationFinalizerHost) SetInstanceConfig(ctx context.Context, project, name string, values map[string]string) error {
	h.trace = append(h.trace, "metadata")
	return h.Client.SetInstanceConfigIf(ctx, project, name, "original", nil, values)
}
func (h *ownerPublicationFinalizerHost) SetOriginalInstanceConfigIf(ctx context.Context, project, name, uuid string, kind domain.YardKind, before, values map[string]string) error {
	h.powerWrites++
	if (h.mode == "replacement-before-commit" || h.mode == "intent-before-commit") && h.powerWrites == 2 {
		instance, err := h.Instance(ctx, project, name)
		if err != nil {
			return err
		}
		cfg := maps.Clone(instance.LocalConfig)
		cfg["volatile.uuid"] = "replacement"
		if h.mode == "intent-before-commit" {
			cfg["volatile.uuid"] = "original"
			cfg["user.subyard.desired_power"] = "stopped"
		}
		h.server.SetInstance(project, name, map[string]any{"name": name, "type": ownerPublicationAPIKind(instance.Type), "status": instance.Status, "config": cfg, "expanded_config": maps.Clone(cfg)})
	}
	return h.Client.SetOriginalInstanceConfigIf(ctx, project, name, uuid, kind, before, values)
}
func (h *ownerPublicationFinalizerHost) ReconcileRunningPublicationsIf(ctx context.Context, project, name, uuid string, kind domain.YardKind, contracts []ownerpublication.Contract, _ []netip.Addr, checks ...func() error) ([]string, error) {
	if !h.held {
		return nil, errors.New("publication escaped original network lock")
	}
	h.calls++
	h.trace = append(h.trace, "publication")
	if h.mode == "replacement" && h.calls == 1 {
		instance, err := h.Instance(ctx, project, name)
		if err != nil {
			return nil, err
		}
		cfg := maps.Clone(instance.LocalConfig)
		cfg["volatile.uuid"] = "replacement"
		h.server.SetInstance(project, name, map[string]any{"name": name, "type": ownerPublicationAPIKind(instance.Type), "status": "Running", "config": cfg, "expanded_config": maps.Clone(cfg)})
	}
	addresses := []netip.Addr(nil)
	if (h.mode == "delay" || h.mode == "weighted-inherit" || h.mode == "weighted-sealed") && h.calls > 1 || h.mode == "missing-ack" {
		addresses = []netip.Addr{netip.MustParseAddr("100.127.255.254")}
	}
	waiting, err := h.Client.ReconcileRunningPublicationsIf(ctx, project, name, uuid, kind, contracts, addresses, checks...)
	if h.mode == "cancel-after-waiting" && err == nil && len(waiting) != 0 {
		h.cancelAtBoundary()
	}
	return waiting, err
}

func (h *ownerPublicationFinalizerHost) SetOriginalInstancePowerIf(ctx context.Context, project, name, action string, force bool, uuid string, kind domain.YardKind) error {
	h.trace = append(h.trace, action)
	return h.Client.SetOriginalInstancePowerIf(ctx, project, name, action, force, uuid, kind)
}

// Host scheduling is an explicit generation fixture; SDK power, exec and
// publication requests below remain real. Physical cgroups require the VM lane.
func (h *ownerPublicationFinalizerHost) VMCPUConvergedIf(ctx context.Context, project, name string, apply bool, uuid string) (bool, error) {
	instance, err := h.Instance(ctx, project, name)
	if err != nil {
		return false, err
	}
	if !h.held || !apply || uuid != "original" || instance.LocalConfig["volatile.uuid"] != uuid || ports.NativeOperationObserverFrom(ctx) == nil {
		return false, domain.ErrPlanStale
	}
	h.cpuCalls++
	h.trace = append(h.trace, fmt.Sprintf("cpu%d-generation%d", h.cpuCalls, h.generation))
	return true, nil
}
func (h *ownerPublicationFinalizerHost) ProbeOriginalInstanceAgentIf(ctx context.Context, project, name, uuid string, kind domain.YardKind) (bool, error) {
	if ports.NativeOperationObserverFrom(ctx) != nil {
		return false, errors.New("probe reserved mutation authority")
	}
	h.probes++
	h.trace = append(h.trace, "agent")
	deadline, bounded := ctx.Deadline()
	if !bounded || time.Until(deadline) > 5*time.Second {
		return false, errors.New("agent probe deadline is unbounded")
	}
	if h.mode == "weighted-agent" || h.mode == "unweighted-agent" {
		h.cancelAtBoundary()
		return false, errors.New("agent unavailable")
	}
	if strings.HasPrefix(h.mode, "weighted-") && h.probes == 1 {
		return false, errors.New("agent seeding")
	}
	h.generation = 2
	step := testkit.IncusServerExecStep{}
	if h.mode == "weighted-drift" {
		step.AfterInput = func(testkit.IncusServerExecCall) int {
			instance, err := h.Instance(context.Background(), project, name)
			if err != nil {
				return 1
			}
			cfg := maps.Clone(instance.LocalConfig)
			cfg["user.subyard.vm_cpu_weight"] = "300"
			h.server.SetInstance(project, name, map[string]any{"name": name, "type": ownerPublicationAPIKind(kind), "status": instance.Status, "config": cfg, "expanded_config": maps.Clone(cfg)})
			return 0
		}
	}
	h.server.QueueExec(step)
	return h.Client.ProbeOriginalInstanceAgentIf(ctx, project, name, uuid, kind)
}
func (h *ownerPublicationFinalizerHost) ReconcileOriginalPublicationsIf(ctx context.Context, project, name, uuid string, kind domain.YardKind, contracts []ownerpublication.Contract, addresses []netip.Addr, checks ...func() error) ([]string, error) {
	h.trace = append(h.trace, "preamble")
	return h.Client.ReconcileOriginalPublicationsIf(ctx, project, name, uuid, kind, contracts, addresses, checks...)
}

type ownerPublicationPolicyHost struct{ yardnetwork.Host }

func (ownerPublicationPolicyHost) ReadPolicy(context.Context) (yardnetwork.StoredPolicy, error) {
	policy, err := yardnetwork.Decode(nil)
	return yardnetwork.StoredPolicy{Policy: policy}, err
}

type ownerPublicationPolicyLock struct {
	host *ownerPublicationFinalizerHost
}

func (lock ownerPublicationPolicyLock) Acquire(context.Context) (func(), error) {
	if lock.host.held {
		return nil, errors.New("network lock re-acquired")
	}
	lock.host.held = true
	lock.host.trace = append(lock.host.trace, "lock")
	return func() { lock.host.held = false; lock.host.trace = append(lock.host.trace, "unlock") }, nil
}

type ownerPublicationFinalizerObserver struct {
	custodyTransportObserver
	unknown bool
}

func (*ownerPublicationFinalizerObserver) CheckedInvocation() (string, string, error) {
	return strings.Repeat("a", 64), strings.Repeat("b", 64), nil
}

func (o *ownerPublicationFinalizerObserver) Uncertain() { o.unknown = true }

// The actual finalizer, captured source proof, metadata/publication SDK HTTP,
// native command observer and original OFD execute here. Typed physical power
// and fixed probes use the real SDK. Host guard and QEMU generation are explicit
// fixtures; this does not prove the complete init capsule or physical VM lane.
func TestNativeInitFinalizerWaitsOriginalPublication(t *testing.T) {
	if os.Getenv("SUBYARD_TEST_FINALIZER_NS") != "1" {
		probe := exec.Command("unshare", "--user", "--map-root-user", "--mount", "true")
		if err := probe.Run(); err != nil {
			t.Skip("private user namespace unavailable")
		}
		command := exec.Command("unshare", "--user", "--map-root-user", "--mount", os.Args[0], "-test.v", "-test.run=^TestNativeInitFinalizerWaitsOriginalPublication$", "-test.count=1")
		command.Env = append(os.Environ(), "SUBYARD_TEST_FINALIZER_NS=1")
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
		return
	}

	for _, kind := range []domain.YardKind{domain.YardVM, domain.YardContainer} {
		for _, mode := range []string{"handoff", "stale-consumer", "no-receipt", "delay", "stopped", "already-stopped", "replacement", "replacement-before-ensure", "replacement-before-commit", "intent-before-commit", "cancel-after-waiting", "missing-ack", "ordinary", "weighted-inherit", "weighted-sealed", "weighted-agent", "unweighted-agent", "weighted-drift", "weighted-mismatch"} {
			if kind == domain.YardContainer && (strings.HasPrefix(mode, "weighted-") || mode == "unweighted-agent" || mode == "ordinary") {
				continue
			}
			t.Run(string(kind)+"-"+mode, func(t *testing.T) {
				root := testkit.TempDir(t)
				runtime, _ := materializedIntegrationFixture(t, root, true)
				events := filepath.Join(root, "physical-events")
				testkit.WriteFile(t, filepath.Join(root, "scripts/lifecycle-guard.sh"), []byte("#!/bin/sh\ncase \"$*\" in 'start --reconcile'|'stop --reconcile') printf physical >>\"$PHYSICAL_EVENTS\";; 'start --publication-retry-only') printf queue >>\"$PHYSICAL_EVENTS\";; *) exit 99;; esac\n"), 0700)
				repository, err := filepath.Abs("../../..")
				if err != nil {
					t.Fatal(err)
				}
				registryPayload, err := os.ReadFile(filepath.Join(repository, "config/release-transition.json"))
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, "config/release-transition.json"), registryPayload, 0600)
				registry, _, err := releasetransition.ParseRegistryV2(registryPayload, releasetransition.BuiltinCapabilityCatalog())
				if err != nil {
					t.Fatal(err)
				}
				ledger := releasetransition.BaselineLedgerV2(registry)
				for _, migration := range registry.Migrations {
					ledger, err = ledger.Advance(registry, migration)
					if err != nil {
						t.Fatal(err)
					}
				}
				home := testkit.TempDir(t)
				configHome := testkit.TempDir(t)
				store, err := releasetransition.NewPOSIXV2Store(configHome)
				if err != nil {
					t.Fatal(err)
				}
				before, err := store.ReadLedger()
				if err != nil {
					t.Fatal(err)
				}
				payload, _, err := releasetransition.MarshalLedgerV2(ledger, registry)
				if err != nil {
					t.Fatal(err)
				}
				if err = store.CompareAndSwapLedger(before, payload); err != nil {
					t.Fatal(err)
				}
				server, err := testkit.NewIncusServer(testkit.TempDir(t))
				if err != nil {
					t.Fatal(err)
				}
				defer server.Close()
				desired := domain.PowerRunning
				status := "Stopped"
				if mode == "stopped" || mode == "already-stopped" {
					desired = domain.PowerStopped
					if mode == "stopped" {
						status = "Running"
					}
				}
				if mode == "ordinary" {
					status = "Running"
				}
				cfg := map[string]string{"volatile.uuid": "original", "user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.desired_power": string(desired), "user.subyard.name": "default", "user.subyard.bridge": "incusbr0", "boot.autostart": "false", previewroute.Key: "v1:100.127.255.254:23456"}
				receipt, _ := json.Marshal(ownerpublication.Receipt{Version: 1, Ownership: cfg[previewroute.Key], Device: previewroute.Device("100.127.255.254", "23456")})
				key := ownerpublication.Prefix + previewroute.DeviceName
				cfg[key] = string(receipt)
				if mode == "delay" || strings.HasPrefix(mode, "weighted-") || mode == "unweighted-agent" {
					cfg["user.subyard.initialized"] = "false"
				}
				if mode == "replacement-before-ensure" {
					cfg["volatile.uuid"] = "replacement"
					cfg["boot.autostart"] = "true"
				}
				if mode == "replacement-before-commit" || mode == "intent-before-commit" {
					cfg["user.subyard.initialized"] = "false"
				}
				if strings.HasPrefix(mode, "weighted-") {
					cfg["user.subyard.vm_cpu_weight"] = "250"
				}
				apiKind := ownerPublicationAPIKind(kind)
				server.SetInstance("p", "yard", map[string]any{"name": "yard", "type": apiKind, "status": status, "config": cfg, "expanded_config": maps.Clone(cfg)})
				socket, updates := ownerPublicationSnapshotSocket(t, server, mode)
				host := &ownerPublicationFinalizerHost{Client: incusclient.New(socket, "projects"), server: server, mode: mode, generation: 1}
				runtime.Yard = domain.Context{YardKind: kind, IncusProject: "p", YardInstanceName: "yard", IncusBridge: "incusbr0", Paths: domain.RuntimePaths{DataHome: home, ConfigHome: configHome}}
				runtime.Incus = host
				runtime.ConfigWriter = host
				runtime.Stderr = io.Discard
				runtime.Environment = append(runtime.Environment, "SUBYARD_HOME="+home, "PHYSICAL_EVENTS="+events)
				if mode == "weighted-sealed" {
					runtime.Environment = append(runtime.Environment, "VM_CPU_WEIGHT=250")
				}
				if mode == "weighted-mismatch" {
					runtime.Environment = append(runtime.Environment, "VM_CPU_WEIGHT=500")
				}
				runtime.NetworkPolicy = &yardnetwork.Service{Host: ownerPublicationPolicyHost{}, Lock: ownerPublicationPolicyLock{host}, Guard: func(context.Context) error {
					if !host.held {
						return errors.New("host guard escaped lock")
					}
					host.trace = append(host.trace, "guard")
					return nil
				}}
				ctx := context.Background()
				if mode == "handoff" || mode == "stale-consumer" {
					runtime = prepareFinalizerPublicationConsumer(t, runtime)
				}
				observer := &ownerPublicationFinalizerObserver{}
				var sources *PreparedInstalledRuntime
				if mode != "ordinary" {
					sources, err = runtime.PrepareOwnerRuntimeSources(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer sources.FinishOwnerSources(nil)
					runtime, err = runtime.OwnerSourceRuntime(ctx, sources)
					if err != nil {
						t.Fatal(err)
					}
					// Original prepared existing-instance UUID; no current GET supplies it.
					runtime.ownerVolume = &ownerVolumeExecution{original: ports.PreparedOwnerVolume{Project: "p", Instance: "yard", Scope: "current", InstanceUUID: "original"}}
					_, release, err := uninstall.OwnerLock(ctx, home, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer release()
					ctx = ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(ctx, observer), strings.Repeat("c", 64))
				}
				// The finite original context covers the entire finalizer. Cancellation
				// cases fire only after their actual SDK/agent boundary was reached.
				timeout := 4 * time.Second
				if mode == "handoff" || mode == "stale-consumer" {
					timeout = 40 * time.Second
				}
				ctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				host.cancelAtBoundary = cancel
				if mode == "no-receipt" {
					delete(cfg, key)
					server.SetInstance("p", "yard", map[string]any{"name": "yard", "type": apiKind, "status": status, "config": cfg, "expanded_config": maps.Clone(cfg)})
				}
				if mode == "stale-consumer" {
					testkit.WriteFile(t, runtime.publicationUnitPath()+".new", []byte("changed consumer"), 0644)
					if e := os.Rename(runtime.publicationUnitPath()+".new", runtime.publicationUnitPath()); e != nil {
						t.Fatal(e)
					}
				}
				workerDone := make(chan struct{})
				if mode == "handoff" {
					go func() {
						defer close(workerDone)
						tick := time.NewTicker(10 * time.Millisecond)
						defer tick.Stop()
						for {
							select {
							case <-ctx.Done():
								return
							case <-tick.C:
								instance, e := host.Instance(ctx, "p", "yard")
								if e != nil {
									continue
								}
								for k, v := range instance.LocalConfig {
									if !strings.HasPrefix(k, ownerpublication.HandoffPrefix) || v == "" {
										continue
									}
									request, e := ownerpublication.DecodeHandoff(v)
									if e != nil || request.Ack != "" {
										continue
									}
									ack := request
									ack.Ack = ack.ID
									if e := host.ExchangePublicationHandoff(ports.WithoutNativeOperationObserver(ctx), ack, ownerpublication.CoreContracts(), v, ack.Encode(), nil); e != nil {
										t.Error(e)
									}
									return
								}
							}
						}
					}()
				} else {
					close(workerDone)
				}
				err = runtime.ApplyStage(ctx, ports.ReconcileStageFinalize)
				cancel()
				<-workerDone
				success := mode == "handoff" || mode == "no-receipt" || mode == "stopped" || mode == "already-stopped" || mode == "ordinary"
				if (err == nil) != success {
					t.Fatal("finalizer outcome", err, host.trace)
				}
				if observer.unknown != false {
					t.Fatal("fabricated finalizer outcome", err, observer.unknown)
				}
				data, readErr := os.ReadFile(events)
				if mode == "replacement-before-ensure" {
					instance, readErr := host.Instance(context.Background(), "p", "yard")
					if readErr != nil || updates.Load() != 0 || host.calls != 0 || host.held || instance.LocalConfig["boot.autostart"] != "true" {
						t.Fatal("replacement reached effects before Ensure", readErr, updates.Load(), host.trace)
					}
					return
				}
				if mode == "ordinary" && readErr != nil {
					t.Fatal(readErr)
				}
				if mode == "ordinary" {
					if string(data) != "physicalqueue" || host.calls != 0 {
						t.Fatal("ordinary queue changed", string(data), host.trace)
					}
					return
				}
				if !os.IsNotExist(readErr) || host.held {
					t.Fatal("native queued or leaked lock", string(data), host.trace)
				}
				instance, readErr := host.Instance(context.Background(), "p", "yard")
				if readErr != nil {
					t.Fatal(readErr)
				}
				if mode == "no-receipt" {
					if instance.Config[key] != "" {
						t.Fatal("empty original gained a receipt")
					}
				} else if instance.Config[key] != string(receipt) || instance.Devices[previewroute.DeviceName] != nil {
					t.Fatal("pending receipt changed", instance)
				}
				if mode == "handoff" {
					if host.calls != 0 {
						t.Fatal("original waited for optional address", host.trace)
					}
					for k, v := range instance.LocalConfig {
						if strings.HasPrefix(k, ownerpublication.HandoffPrefix) && v != "" {
							t.Fatal("ACK was not drained")
						}
					}
					host.trace = append(host.trace, "tail")
					if !strings.HasSuffix(strings.Join(host.trace, ","), "unlock,tail") {
						t.Fatal(host.trace)
					}
				}
				if (mode == "replacement-before-commit" || mode == "intent-before-commit") && (updates.Load() != 0 || host.calls != 0 || instance.LocalConfig["user.subyard.initialized"] != "false") {
					t.Fatal("replacement metadata was changed", updates.Load(), instance, host.trace)
				}
				powers := server.PowerCalls()
				if mode == "already-stopped" {
					if len(powers) != 0 || host.probes != 0 || host.cpuCalls != 0 {
						t.Fatal("stopped original was restarted", powers, host.trace)
					}
				} else if mode == "stopped" {
					if len(powers) != 1 || powers[0].Action != "stop" || powers[0].Force || host.probes != 0 || host.cpuCalls != 0 {
						t.Fatal("stopped physical semantics changed", powers, host.trace)
					}
				} else if mode == "weighted-inherit" || mode == "weighted-sealed" {
					trace := strings.Join(host.trace, ",")
					if host.cpuCalls != 2 || !strings.Contains(trace, "cpu1-generation1,agent,agent,guard,preamble,guard,cpu2-generation2") {
						t.Fatal("weighted init lost second application after seeding", host.trace)
					}
				} else if mode == "weighted-agent" || mode == "unweighted-agent" {
					want := 1
					if mode == "weighted-agent" {
						want = 2
					}
					if len(powers) != want || want == 2 && (powers[1].Action != "stop" || !powers[1].Force) || host.calls != 0 || instance.LocalConfig["user.subyard.initialized"] != "false" {
						t.Fatal("agent failure changed compensation or committed", powers, host.trace)
					}
				}
				if mode == "weighted-drift" {
					if len(powers) != 2 || powers[1].Action != "stop" || !powers[1].Force || host.cpuCalls != 1 || host.calls != 0 || instance.LocalConfig["user.subyard.initialized"] != "false" {
						t.Fatal("weight drift reached CPU2 or commit", powers, host.trace)
					}
				}
				if mode == "weighted-mismatch" && (len(powers) != 0 || host.cpuCalls != 0 || host.probes != 0 || host.calls != 0) {
					t.Fatal("sealed weight mismatch reached effects", powers, host.trace)
				}
				if (mode == "stopped" || mode == "already-stopped") && host.calls != 0 {
					t.Fatal("stopped finalizer waited or attached")
				}
			})
		}
	}
}

// Local instance-update fixture: the shared IncusServer supplies power/exec;
// this owning test supplies ETags and atomic config/device SDK update responses.
func ownerPublicationSnapshotSocket(t *testing.T, fixture *testkit.IncusServer, mode string) (string, *atomic.Uint64) {
	t.Helper()
	socket := filepath.Join(testkit.TempDir(t), "incus.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var updates atomic.Uint64
	var sequence atomic.Uint64
	var operations sync.Map
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/1.0/operations/owning-publication-") {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/1.0/operations/"), "/wait")
			if operation, ok := operations.Load(id); ok {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "status": "Success", "status_code": 200, "metadata": operation})
				return
			}
		}

		name := strings.TrimPrefix(r.URL.Path, "/1.0/instances/")
		if strings.HasPrefix(r.URL.Path, "/1.0/instances/") && !strings.Contains(name, "/") {
			if r.Method == http.MethodGet {
				w.Header().Set("ETag", `"owning-snapshot"`)
			}
			if r.Method == http.MethodPut {
				updates.Add(1)
				if r.Header.Get("If-Match") != `"owning-snapshot"` {
					http.Error(w, "stale snapshot", http.StatusPreconditionFailed)
					return
				}
				var put map[string]any
				if json.NewDecoder(r.Body).Decode(&put) != nil {
					http.Error(w, "invalid update", 400)
					return
				}
				if cfg, ok := put["config"].(map[string]any); ok && mode == "missing-ack" && cfg[ownerpublication.Prefix+previewroute.DeviceName] == nil {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(w).Encode(map[string]any{"type": "async", "status": "Operation created", "status_code": 100, "metadata": map[string]any{"class": "task", "status": "Running", "status_code": 103}})
					return
				}
				get := r.Clone(r.Context())
				get.Method = http.MethodGet
				recorder := httptest.NewRecorder()
				fixture.ServeHTTP(recorder, get)
				var envelope struct {
					Metadata map[string]any `json:"metadata"`
				}
				if json.Unmarshal(recorder.Body.Bytes(), &envelope) != nil || envelope.Metadata == nil {
					http.Error(w, "missing instance", 404)
					return
				}
				for key, value := range put {
					envelope.Metadata[key] = value
				}
				envelope.Metadata["expanded_config"] = put["config"]
				envelope.Metadata["expanded_devices"] = put["devices"]
				project := r.URL.Query().Get("project")
				fixture.SetInstance(project, name, envelope.Metadata)
				id := fmt.Sprintf("owning-publication-%d", sequence.Add(1))
				operations.Store(id, map[string]any{"id": id, "class": "task", "status": "Success", "status_code": 200, "resources": map[string][]string{"instances": {"/1.0/instances/" + name + "?project=" + project}}})

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "async", "status": "Operation created", "status_code": 100, "operation": "/1.0/operations/" + id, "metadata": map[string]any{"id": id, "class": "task", "status": "Success", "status_code": 200, "resources": map[string][]string{"instances": {"/1.0/instances/" + name + "?project=" + project}}}})
				return
			}
		}
		fixture.ServeHTTP(w, r)
	})
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return socket, &updates
}

func prepareFinalizerPublicationConsumer(t *testing.T, runtime Runtime) Runtime {
	t.Helper()
	root := runtime.RepositoryRoot
	for _, path := range []string{"config/systemd", "bin", "installed"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	template, err := os.ReadFile("../../../config/systemd/subyard-publication-reconcile.service.in")
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/systemd/subyard-publication-reconcile.service.in"), template, 0644)
	powerTemplate, err := os.ReadFile("../../../config/systemd/subyard-power-reconcile.service.in")
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/systemd/subyard-power-reconcile.service.in"), powerTemplate, 0644)
	image, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "bin/yard-engine")
	testkit.WriteFile(t, source, image, 0755)
	engine := filepath.Join(root, "installed/engine")
	power := filepath.Join(root, "installed/power.service")
	passive := filepath.Join(filepath.Dir(power), ownerpublication.HandoffService)
	runtime.Environment = append(runtime.Environment, "SUBYARD_USER=root", "SUBYARD_POWER_ENGINE_SOURCE="+source, "SUBYARD_POWER_RECONCILER_PATH="+engine, "SUBYARD_POWER_UNIT_PATH="+power)
	contracts, err := ownerpublication.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := ownerpublication.Encode(contracts)
	if err != nil {
		t.Fatal(err)
	}
	body := ownerpublication.RenderPublicationUnit(string(template), engine, power, encoded)
	testkit.WriteFile(t, engine, image, 0755)
	testkit.WriteFile(t, power, []byte(strings.ReplaceAll(string(powerTemplate), "@SUBYARD_POWER_RECONCILER@", engine)+"\n[Service]\nEnvironment=SUBYARD_PUBLICATION_CONTRACTS="+encoded+"\n"), 0644)
	testkit.WriteFile(t, passive, []byte(body), 0644)
	manager := filepath.Join(root, "installed/systemctl")
	testkit.WriteFile(t, manager, []byte("#!/bin/sh\ncase \"$1\" in show) printf 'LoadState=loaded\\nNeedDaemonReload=no\\nFragmentPath="+passive+"\\nDropInPaths=\\n';; start) [ \"$3\" = "+ownerpublication.HandoffService+" ];; *) exit 2;; esac\n"), 0755)
	if err := unix.Mount(manager, "/usr/bin/systemctl", "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount("/usr/bin/systemctl", 0); err != nil {
			t.Error(err)
		}
	})
	record, err := runtime.PrepareOwnerPublication(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = runtime.RestoreOwnerPublication(record)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}
