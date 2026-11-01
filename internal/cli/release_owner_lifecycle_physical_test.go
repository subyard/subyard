package cli

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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

// SDK power and publication calls use the official client and local API fixture.
// The existing SDK tests own config ETag checks; this fixture records that
// lifecycle calls the UUID writer. QEMU generation and agent readiness are
// simulated here; actual cgroup placement belongs to the physical VM lane.
type ownerLifecycleCPUHost struct {
	*incusclient.Client
	server                       *testkit.IncusServer
	trace                        []string
	fault                        string
	cpuCalls, probes, generation int
	publicationWaitCalls         int
	kind                         domain.YardKind
}

func (h *ownerLifecycleCPUHost) VMCPUConverged(_ context.Context, _, _ string, _ bool) (bool, error) {
	return false, nil
}
func (h *ownerLifecycleCPUHost) VMCPUConvergedIf(ctx context.Context, project, name string, apply bool, uuid string) (bool, error) {
	i, err := h.Instance(ctx, project, name)
	if err != nil {
		return false, err
	}
	if uuid != "original" || i.Config["volatile.uuid"] != uuid || !apply || ports.NativeOperationObserverFrom(ctx) == nil {
		return false, domain.ErrPlanStale
	}
	h.cpuCalls++
	h.trace = append(h.trace, fmt.Sprintf("cpu%d-generation%d", h.cpuCalls, h.generation))
	if h.fault == fmt.Sprintf("cpu%d", h.cpuCalls) {
		return false, errors.New("CPU fixture failure")
	}
	return true, nil
}
func (h *ownerLifecycleCPUHost) SetInstancePowerIf(ctx context.Context, project, name, action string, force bool, uuid string) error {
	h.trace = append(h.trace, action)
	return h.Client.SetInstancePowerIf(ctx, project, name, action, force, uuid)
}
func (h *ownerLifecycleCPUHost) SetOriginalInstancePowerIf(ctx context.Context, project, name, action string, force bool, uuid string, kind domain.YardKind) error {
	h.trace = append(h.trace, action)
	return h.Client.SetOriginalInstancePowerIf(ctx, project, name, action, force, uuid, kind)
}
func (h *ownerLifecycleCPUHost) SetOriginalInstanceConfigIf(ctx context.Context, project, name, uuid string, kind domain.YardKind, before, values map[string]string) error {
	phase := "ensure"
	if values["user.subyard.desired_power"] == "running" {
		phase = "commit"
	}
	h.trace = append(h.trace, phase)
	if phase == "commit" && h.fault == "commit-replacement" {
		h.replace(ctx, project, name)
	}
	return h.Client.SetOriginalInstanceConfigIf(ctx, project, name, uuid, kind, before, values)
}
func (h *ownerLifecycleCPUHost) SetInstanceConfigIf(ctx context.Context, project, name, uuid string, before, values map[string]string) error {
	phase := "ensure"
	if values["user.subyard.desired_power"] == "running" {
		phase = "commit"
	}
	h.trace = append(h.trace, phase)
	if phase == "commit" && h.fault == "commit-replacement" {
		h.replace(ctx, project, name)
	}
	instance, err := h.Instance(ctx, project, name)
	if err != nil {
		return err
	}
	if uuid == "" || instance.LocalConfig["volatile.uuid"] != uuid {
		return domain.ErrPlanStale
	}
	for key, value := range before {
		if instance.LocalConfig[key] != value {
			return domain.ErrPlanStale
		}
	}
	cfg := maps.Clone(instance.LocalConfig)
	for key, value := range values {
		cfg[key] = value
	}
	h.server.SetInstance(project, name, map[string]any{"name": name, "type": "virtual-machine", "status": instance.Status, "config": cfg, "expanded_config": maps.Clone(cfg)})
	return nil
}
func (h *ownerLifecycleCPUHost) replace(ctx context.Context, project, name string) {
	i, _ := h.Instance(ctx, project, name)
	cfg := maps.Clone(i.LocalConfig)
	cfg["volatile.uuid"] = "replacement"
	h.server.SetInstance(project, name, map[string]any{"name": name, "type": ownerLifecycleAPIKind(h.kind), "status": i.Status, "config": cfg, "expanded_config": maps.Clone(cfg)})
}
func (h *ownerLifecycleCPUHost) ProbeInstanceAgentIf(ctx context.Context, project, name, uuid string) (bool, error) {
	return h.ProbeOriginalInstanceAgentIf(ctx, project, name, uuid, domain.YardVM)
}
func (h *ownerLifecycleCPUHost) ProbeOriginalInstanceAgentIf(ctx context.Context, project, name, uuid string, kind domain.YardKind) (bool, error) {
	h.probes++
	h.trace = append(h.trace, "agent")
	if ports.NativeOperationObserverFrom(ctx) != nil || uuid != "original" {
		return false, errors.New("agent probe reserved mutation authority")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 5*time.Second {
		return false, errors.New("agent probe deadline is unbounded")
	}
	if h.fault == "agent" {
		return false, errors.New("agent unavailable")
	}
	if h.fault == "agent-replacement" {
		h.replace(ctx, project, name)
	}
	if h.probes == 1 {
		return false, errors.New("agent seeding")
	}
	h.generation = 2
	h.server.QueueExec(testkit.IncusServerExecStep{})
	return h.Client.ProbeOriginalInstanceAgentIf(ctx, project, name, uuid, kind)
}
func (h *ownerLifecycleCPUHost) ReconcilePublicationsIf(ctx context.Context, project, name, uuid string, contracts []ownerpublication.Contract, addresses []netip.Addr, checks ...func() error) ([]string, error) {
	h.trace = append(h.trace, "publications")
	return h.Client.ReconcilePublicationsIf(ctx, project, name, uuid, contracts, addresses, checks...)
}

func (h *ownerLifecycleCPUHost) ReconcileOriginalPublicationsIf(ctx context.Context, project, name, uuid string, kind domain.YardKind, contracts []ownerpublication.Contract, addresses []netip.Addr, checks ...func() error) ([]string, error) {
	h.trace = append(h.trace, "publications")
	return h.Client.ReconcileOriginalPublicationsIf(ctx, project, name, uuid, kind, contracts, addresses, checks...)
}
func (h *ownerLifecycleCPUHost) ReconcileRunningPublicationsIf(ctx context.Context, project, name, uuid string, kind domain.YardKind, contracts []ownerpublication.Contract, addresses []netip.Addr, checks ...func() error) ([]string, error) {
	h.publicationWaitCalls++
	h.trace = append(h.trace, "publication-wait")
	if h.fault == "publication-delay" {
		addresses = nil
		if h.publicationWaitCalls > 1 {
			addresses = []netip.Addr{netip.MustParseAddr("100.127.255.254")}
		}
	}
	return h.Client.ReconcileRunningPublicationsIf(ctx, project, name, uuid, kind, contracts, addresses, checks...)
}

type ownerLifecycleTraceLock struct {
	host *ownerLifecycleCPUHost
	held bool
}

func (l *ownerLifecycleTraceLock) Acquire(context.Context) (func(), error) {
	l.held = true
	l.host.trace = append(l.host.trace, "lock")
	return func() { l.host.trace = append(l.host.trace, "unlock"); l.held = false }, nil
}

func ownerLifecycleCPUFixture(t *testing.T, weighted bool, fault string) (*CLI, config.Loaded, *ownerLifecycleCPUHost, *lifecycleExecution, *ownerLifecycleTraceLock, *testkit.ScriptedAdapter) {
	return ownerLifecycleKindFixture(t, domain.YardVM, weighted, fault)
}
func ownerLifecycleAPIKind(kind domain.YardKind) string {
	if kind == domain.YardVM {
		return "virtual-machine"
	}
	return "container"
}
func ownerLifecycleKindFixture(t *testing.T, kind domain.YardKind, weighted bool, fault string) (*CLI, config.Loaded, *ownerLifecycleCPUHost, *lifecycleExecution, *ownerLifecycleTraceLock, *testkit.ScriptedAdapter) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	metadata := map[string]string{"volatile.uuid": "original", "boot.autostart": "false", "user.subyard.managed": "true", "user.subyard.name": "default", "user.subyard.bridge": "incusbr0", "user.subyard.desired_power": "stopped", "user.subyard.initialized": "true"}
	if weighted {
		metadata["user.subyard.vm_cpu_weight"] = "250"
	}
	server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "type": ownerLifecycleAPIKind(kind), "status": "Stopped", "config": metadata, "expanded_config": maps.Clone(metadata)})
	host := &ownerLifecycleCPUHost{Client: incusclient.New(ownerPublicationSnapshotSocket(t, server), "projects"), server: server, fault: fault, generation: 1, kind: kind}
	lock := &ownerLifecycleTraceLock{host: host}
	fallback := &testkit.ScriptedAdapter{}
	policy := &yardnetwork.Service{Host: testNetworkPolicyHost{}, Lock: lock}
	cli, err := New(Options{RepositoryRoot: root, WorkingDir: root, Environment: environment, Incus: host, AdapterRunner: fallback, NetworkPolicy: policy, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Context.YardKind = kind
	loaded.Context.IncusBridge = "incusbr0"
	execution := &lifecycleExecution{action: "start"}
	if err := cli.observeLifecycleExecution(context.Background(), loaded.Context, execution); err != nil {
		t.Fatal(err)
	}
	guards := 0
	execution.ownerPhysical = ownerStartPhysical{cli: cli, yard: loaded.Context, uuid: "original", weight: metadata["user.subyard.vm_cpu_weight"]}
	policy.Guard = func(context.Context) error {
		if !lock.held {
			return errors.New("host check escaped network policy lock")
		}
		guards++
		host.trace = append(host.trace, "guard")
		if fault == "host" && guards == 2 {
			return errors.New("unsafe host route")
		}
		return nil
	}
	execution.ownerPowerConfig = ownerStartPowerConfig{cli: cli, yard: loaded.Context, uuid: "original"}
	return cli, loaded, host, execution, lock, fallback
}

func TestOwnerLifecycleNativeCPUOrderingAndFailClosed(t *testing.T) {
	for _, fault := range []string{"", "cpu1", "cpu2", "agent", "host", "agent-replacement", "commit-replacement", "unweighted-agent"} {
		t.Run(fault, func(t *testing.T) {
			weighted := fault != "unweighted-agent"
			actualFault := fault
			if !weighted {
				actualFault = "agent"
			}
			cli, loaded, host, execution, lock, fallback := ownerLifecycleCPUFixture(t, weighted, actualFault)
			if _, err := os.Stat(filepath.Join(cli.options.RepositoryRoot, "bin/yard-engine")); !os.IsNotExist(err) {
				t.Fatal("fixture unexpectedly contains a retained engine")
			}
			timeout := 5 * time.Second
			if actualFault == "agent" {
				timeout = 200 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			observer := &physicalStartupObserver{}
			ctx = ports.WithNativeOperationObserver(ctx, observer)
			orchestrator, plan, err := ownerChildOrchestrator(ctx, cli, loaded, "start", cli.options.RepositoryRoot)
			if err != nil {
				t.Fatal(err)
			}
			result, err := cli.executeLifecycle(ctx, orchestrator, loaded.Context, plan, execution, io.Discard)
			if fault == "" {
				if err != nil || result.Status != "ok" {
					t.Fatal(result, err, host.trace)
				}
				want := []string{"lock", "ensure", "guard", "publications", "start", "guard", "cpu1-generation1", "agent", "agent", "guard", "publications", "guard", "cpu2-generation2", "guard", "commit", "unlock"}
				if !slices.Equal(host.trace, want) {
					t.Fatalf("lifecycle ordering changed: %v", host.trace)
				}
			} else {
				if err == nil {
					t.Fatal("failed physical start committed", fault, host.trace)
				}
				calls := host.server.PowerCalls()
				stop := fault != "unweighted-agent" && fault != "agent-replacement" && fault != "commit-replacement"
				want := 1
				if stop {
					want = 2
				}
				if len(calls) != want || stop && (!calls[1].Force || calls[1].Action != "stop") {
					t.Fatalf("incorrect original compensation: %v %v", calls, err)
				}
				if fault != "commit-replacement" && slices.Contains(host.trace, "commit") {
					t.Fatal("failure committed desired running", host.trace)
				}
			}
			if lock.held || len(fallback.Requests) != 0 {
				t.Fatal("native start escaped its owner boundary")
			}
			if len(observer.started) != len(observer.accepted) || len(observer.accepted) != len(observer.terminal) || observer.unknown {
				t.Fatalf("read-only probe left unmatched intent: %+v", observer)
			}
		})
	}
}

func TestOwnerLifecycleDeferredPublicationRequiresCapturedConsumer(t *testing.T) {
	cli, loaded, host, execution, _, fallback := ownerLifecycleCPUFixture(t, true, "")
	instance, err := host.Instance(context.Background(), "subyard", "yard")
	if err != nil {
		t.Fatal(err)
	}
	cfg := maps.Clone(instance.LocalConfig)
	device := previewroute.Device("100.127.255.254", "23456")
	receipt, _ := json.Marshal(ownerpublication.Receipt{Version: 1, Ownership: "v1:100.127.255.254:23456", Device: device})
	cfg[previewroute.Key] = "v1:100.127.255.254:23456"
	cfg[ownerpublication.Prefix+previewroute.DeviceName] = string(receipt)
	host.server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "type": "virtual-machine", "status": "Stopped", "config": cfg, "expanded_config": maps.Clone(cfg)})
	execution.observed = nil
	execution.approvedSteps = nil
	if err := cli.observeLifecycleExecution(context.Background(), loaded.Context, execution); err != nil {
		t.Fatal(err)
	}
	observer := &physicalStartupObserver{}
	ctx, cancel := context.WithTimeout(ports.WithNativeOperationObserver(context.Background(), observer), 2*time.Second)
	defer cancel()
	orchestrator, plan, err := ownerChildOrchestrator(ctx, cli, loaded, "start", cli.options.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cli.executeLifecycle(ctx, orchestrator, loaded.Context, plan, execution, io.Discard)
	if !errors.Is(err, domain.ErrPlanStale) || observer.unknown || len(fallback.Requests) != 0 {
		t.Fatal("unowned publication job was admitted", err, fallback.Requests)
	}
	instance, err = host.Instance(context.Background(), "subyard", "yard")
	if err != nil || instance.Config["user.subyard.desired_power"] != "running" || instance.Config[ownerpublication.Prefix+previewroute.DeviceName] != string(receipt) {
		t.Fatal("deferred original intent was lost", instance, err)
	}
}

// Exercise the owning reconstruction seam itself, including its exact captured
// lifecycle record and retained source recipe. The retained tree has no engine.
func TestOwnerLifecycleOriginalCapsuleUsesNativeWeightedStart(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		t.Run(fmt.Sprint(delayed), func(t *testing.T) { ownerLifecycleOriginalCapsule(t, domain.YardVM, delayed) })
	}
}
func TestOwnerLifecycleOriginalContainerUsesNativeStart(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		t.Run(fmt.Sprint(delayed), func(t *testing.T) { ownerLifecycleOriginalCapsule(t, domain.YardContainer, delayed) })
	}
}
func ownerLifecycleOriginalCapsule(t *testing.T, kind domain.YardKind, delayed bool) {
	t.Helper()
	cli, loaded, host, _, _, fallback := ownerLifecycleKindFixture(t, kind, kind == domain.YardVM, "")
	root := cli.options.RepositoryRoot
	if delayed {
		host.fault = "publication-delay"
		sourceRoot := repositoryRoot(t)
		if !filepath.IsAbs(sourceRoot) {
			var err error
			sourceRoot, err = os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat(filepath.Join(sourceRoot, "config", "release-transition.json")); err != nil {
			t.Fatal("resolve owning source fixture", err)
		}
		seedCurrentReleaseLedger(t, root, loaded.Context.Paths.ConfigHome, sourceRoot)
		current, err := host.Instance(context.Background(), "subyard", "yard")
		if err != nil {
			t.Fatal(err)
		}
		cfg := maps.Clone(current.LocalConfig)
		cfg[previewroute.Key] = "v1:100.127.255.254:23456"
		receipt, _ := json.Marshal(ownerpublication.Receipt{Version: 1, Ownership: cfg[previewroute.Key], Device: previewroute.Device("100.127.255.254", "23456")})
		cfg[ownerpublication.Prefix+previewroute.DeviceName] = string(receipt)
		host.server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "type": ownerLifecycleAPIKind(kind), "status": "Stopped", "config": cfg, "expanded_config": maps.Clone(cfg)})
	}
	testkit.WriteFile(t, filepath.Join(root, "scripts/reconcile-integrations.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	runtime := reconcileruntime.Runtime{RepositoryRoot: root, LogicalRepositoryRoot: root, Environment: environmentList(loaded.Environment, nil), Yard: loaded.Context, Incus: host}
	original, err := captureOwnerStart(context.Background(), cli, loaded, root)
	if err != nil {
		t.Fatal("original lifecycle capture", err)
	}
	sources, err := runtime.PrepareOwnerRuntimeSources(context.Background())
	if err != nil {
		t.Fatal("original source capture", err)
	}
	if err := sources.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(loaded.Context.Paths.DataHome, 0700); err != nil {
		t.Fatal(err)
	}
	_, unlock, err := uninstall.OwnerLock(context.Background(), loaded.Context.Paths.DataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	observer := &physicalStartupObserver{}
	bound := ports.WithNativeOwnerHome(ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(context.Background(), observer), nativeValueDigest([]string{nativeValueDigest(original), testkit.TempDir(t)})), loaded.Context.Paths.DataHome)
	ctx, cancel := context.WithTimeout(bound, 5*time.Second)
	defer cancel()
	children := &preparedOwnerChildren{operation: cli, inits: map[string]preparedOwnerInitChild{loaded.Context.YardName: {loaded: loaded, runtime: runtime, start: original, sources: sources}}}
	runErr := children.runStart(ctx, loaded.Context.YardName, io.Discard)
	if delayed {
		if !errors.Is(runErr, domain.ErrPlanStale) {
			t.Fatal("missing compatible consumer admitted", runErr)
		}
	} else if runErr != nil {
		t.Fatal("original retained weighted start", runErr, host.trace)
	}
	if host.cpuCalls != map[bool]int{true: 2, false: 0}[kind == domain.YardVM] || host.generation != 2 || len(fallback.Requests) != 0 || observer.unknown {
		t.Fatal("original capsule bypassed native lifecycle", host.trace, observer)
	}
	if _, err := os.Stat(filepath.Join(root, "bin/yard-engine")); !os.IsNotExist(err) {
		t.Fatal("fixture acquired an executable dispatcher")
	}
	if delayed {
		instance, err := host.Instance(context.Background(), "subyard", "yard")
		if err != nil || host.publicationWaitCalls != 0 || instance.Config[ownerpublication.Prefix+previewroute.DeviceName] == "" || len(instance.Devices[previewroute.DeviceName]) != 0 {
			t.Fatal("missing consumer changed original receipt", err, instance, host.trace)
		}
	}
}

func TestOwnerLifecycleOverridesLeaveOrdinaryPublicationRetry(t *testing.T) {
	cli, loaded, _, _, _, fallback := ownerLifecycleCPUFixture(t, false, "")
	ordinary := lifecycleIncus()
	instance := ordinary.Instances["subyard/yard"]
	instance.Status = "Stopped"
	instance.Config[ownerpublication.Prefix+"fixture"] = `{"version":1,"device":{"listen":"tcp:192.0.2.1:12345"}}`
	ordinary.Instances["subyard/yard"] = instance
	cli.options.Incus = ordinary
	fallback.Steps = []testkit.AdapterStep{
		{Result: domain.AdapterResult{Schema: 1, Status: "ok"}, Apply: func(domain.AdapterRequest) {
			i := ordinary.Instances["subyard/yard"]
			i.Status = "Running"
			ordinary.Instances["subyard/yard"] = i
		}},
		{Result: domain.AdapterResult{Schema: 1, Status: "ok"}},
	}
	execution := &lifecycleExecution{action: "start"}
	if err := cli.observeLifecycleExecution(context.Background(), loaded.Context, execution); err != nil {
		t.Fatal(err)
	}
	orchestrator, plan, err := ownerChildOrchestrator(context.Background(), cli, loaded, "start", cli.options.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.executeLifecycle(context.Background(), orchestrator, loaded.Context, plan, execution, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(fallback.Requests) != 2 || !slices.Equal(fallback.Requests[1].Arguments, []string{"start", "--publication-retry-only"}) {
		t.Fatal("ordinary publication retry changed", fallback.Requests)
	}
}

func TestOwnerLifecycleNativeRequestValidationPrecedesEffects(t *testing.T) {
	for _, fault := range []string{"schema", "operation", "action", "arguments", "protected"} {
		t.Run(fault, func(t *testing.T) {
			_, _, host, execution, _, _ := ownerLifecycleCPUFixture(t, true, "")
			request := domain.AdapterRequest{Schema: 1, OperationID: "original-operation", Adapter: "lifecycle", Action: "start", Arguments: []string{"start"}}
			var protected io.Reader
			switch fault {
			case "schema":
				request.Schema = 2
			case "operation":
				request.OperationID = "../foreign"
			case "action":
				request.Action = "stop"
			case "arguments":
				request.Arguments = []string{"start", "--force"}
			case "protected":
				protected = strings.NewReader("unapproved")
			}
			ctx := ports.WithNativeOperationObserver(context.Background(), &physicalStartupObserver{})
			if _, _, err := execution.ownerPhysical.Run(ctx, request, protected); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("unapproved native request reached lifecycle", err)
			}
			if len(host.server.PowerCalls()) != 0 || host.cpuCalls != 0 {
				t.Fatal("invalid request changed native state")
			}
		})
	}
}

// Local instance-update fixture: the shared IncusServer supplies power/exec;
// this owning test supplies ETags and atomic config/device SDK update responses.
func ownerPublicationSnapshotSocket(t *testing.T, fixture *testkit.IncusServer) string {
	t.Helper()
	socket := filepath.Join(testkit.TempDir(t), "incus.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
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
				if r.Header.Get("If-Match") != `"owning-snapshot"` {
					http.Error(w, "stale snapshot", http.StatusPreconditionFailed)
					return
				}
				var put map[string]any
				if json.NewDecoder(r.Body).Decode(&put) != nil {
					http.Error(w, "invalid update", 400)
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
	return socket
}
