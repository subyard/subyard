package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/Subyard/Subyard/internal/yardnetwork"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func ownerProjectRendererFixture(t *testing.T, root string, create bool, reporting, nested string) Runtime {
	t.Helper()
	runtime, _ := materializedIntegrationFixture(t, root, create)
	if create {
		helper, err := os.ReadFile("../../../scripts/lib/project-config.sh")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "scripts/lib"), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, ownerProjectRenderer), helper, 0644)
	}
	runtime.Yard.YardName = "test-yard"
	runtime.Yard.Paths.RepositoryRoot = root
	runtime.Yard.Paths.ConfigDir = filepath.Join(root, "config")
	runtime.Yard.Paths.ConfigHome = filepath.Join(root, "operator-config")
	runtime.Yard.Paths.DataHome = filepath.Join(root, "operator-data")
	runtime.Yard.Paths.HostBase = filepath.Join(root, "host-\xff-data")
	contextRaw, err := json.Marshal(runtime.Yard)
	if err != nil {
		t.Fatal(err)
	}
	runtime.Environment = append(runtime.Environment,
		"SUBYARD_HOME="+runtime.Yard.Paths.DataHome,
		"SUBYARD_CONFIG_HOME="+runtime.Yard.Paths.ConfigHome,
		"HOST_BASE="+runtime.Yard.Paths.HostBase,
		"SUBYARD_INSTALLATION_CONTEXT="+string(contextRaw),
		"YARD_NAME="+runtime.Yard.YardName,
		"YARD_INSTANCE_NAME="+runtime.Yard.YardInstanceName,
		"SRV_POOL=original-pool", "SRV_VOLUME=original-volume", "STORAGE_PATH="+filepath.Join(root, "storage"),
		"INCUS_NETWORK=original-bridge", "YARD_KIND=vm", "VM_FREE_PAGE_REPORTING="+reporting, "NESTED_E2E_VMS="+nested)
	return runtime
}

func ownerRenderedMap(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	if len(raw) == 0 || raw[len(raw)-1] != 0 {
		t.Fatal("renderer output is not bounded byte framing")
	}
	result := map[string]string{}
	for _, row := range bytes.Split(raw[:len(raw)-1], []byte{0}) {
		key, value, ok := strings.Cut(string(row), "=")
		if !ok {
			t.Fatal("renderer row is malformed")
		}
		if _, duplicate := result[key]; duplicate {
			t.Fatal("renderer row is repeated")
		}
		result[key] = value
	}
	return result
}

func TestOwnerProjectBindingSurvivesNativeMaterialization(t *testing.T) {
	runtime, _ := ownerInstanceRecipeRuntime(t, testkit.TempDir(t), true)
	ctx := ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(t.Context(), custodyTransportObserver{}), installedRuntimeDigest(runtime.RepositoryRoot))
	p, err := runtime.PrepareOwnerRuntimeSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	raw, err := runtime.RenderOwnerProjectConfig(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	project := ports.PreparedImageProject{Project: runtime.Yard.IncusProject, Absent: true, Server: strings.Repeat("c", 64), InitialConfig: raw}
	bound, err := runtime.WithOwnerYardProject(ctx, p, project, "current", nil)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := bound.PrepareOwnerInstancePolicy(ctx, p, "current")
	if err != nil {
		t.Fatal(err)
	}
	bound, err = bound.WithOwnerInstance(ctx, p, *instance)
	if err != nil {
		t.Fatal(err)
	}
	catalog := []byte(`[{"fingerprint":"` + strings.Repeat("a", 64) + `","type":"container","aliases":[{"name":"primary"}]}]`)
	want, err := bound.RenderOwnerInstanceConfig(ctx, p, catalog)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := bound.OwnerSourceRuntime(ctx, p)
	if err != nil {
		t.Fatal("original project did not survive native source materialization", err)
	}
	if _, err := execution.RenderOwnerProjectConfig(ctx, p); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("materialized view accepted the original renderer binding", err)
	}
	if execution.ownerProject == nil || !bytes.Equal(execution.ownerProject.original.InitialConfig, raw) || execution.environmentValue("SUBYARD_PREPARED_INSTALLED_STAGE") == "" {
		t.Fatal("native execution lost the checked original project or staging context")
	}
	got, err := execution.ownerInstance.rendererRuntime.RenderOwnerInstanceConfig(ctx, p, catalog)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("deferred image selection lost original rendering context", err)
	}
	testkit.WriteFile(t, filepath.Join(runtime.RepositoryRoot, ownerProjectRenderer), []byte("changed source"), 0644)
	if _, err := bound.OwnerSourceRuntime(ctx, p); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("checked project allowed changed original sources", err)
	}
}

func TestOwnerProjectRendererUsesOriginalSharedPolicyAndLosslessPaths(t *testing.T) {
	for _, setting := range [][2]string{{"0", "0"}, {"0", "1"}, {"1", "0"}, {"1", "1"}} {
		t.Run(strings.Join(setting[:], "-"), func(t *testing.T) {
			root := testkit.TempDir(t)
			runtime := ownerProjectRendererFixture(t, root, true, setting[0], setting[1])
			p, err := runtime.PrepareOwnerRuntimeSources(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			raw, err := runtime.RenderOwnerProjectConfig(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			values := ownerRenderedMap(t, raw)
			wantInterception := "block"
			if setting[1] == "1" {
				wantInterception = "allow"
			}
			if values["restricted.containers.interception"] != wantInterception || (values["restricted.virtual-machines.lowlevel"] == "allow") != (setting[0] == "1") || values["user.subyard.operator_uid"] != strconv.Itoa(os.Getuid()) || values["user.subyard.host_base"] != runtime.Yard.Paths.HostBase || values["user.subyard.data_home"] != runtime.Yard.Paths.DataHome || values["user.subyard.srv_pool"] != "original-pool" {
				t.Fatal("original renderer policy or root bytes changed")
			}
			if _, included := values["features.images"]; included {
				t.Fatal("yard renderer changed the separately prepared image namespace")
			}
			// A later path edit cannot replace the captured original helper body.
			testkit.WriteFile(t, filepath.Join(root, ownerProjectRenderer), []byte("exit 99\n"), 0644)
			again, err := runtime.RenderOwnerProjectConfig(context.Background(), p)
			if err != nil || !bytes.Equal(again, raw) {
				t.Fatal("renderer reread changed source", err)
			}
			if _, err := runtime.BindInstalledRuntimeContext(context.Background(), p.Recipe()); err == nil {
				t.Fatal("changed source passed original pre-dispatch guard")
			}
			metadata, _, err := decodeMaterializedMetadata(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range metadata.Integrations.Sources {
				if source.Layout == ownerProjectRenderer {
					p.files[source.Payload] = []byte("unapproved cached helper")
				}
			}
			if _, err := runtime.RenderOwnerProjectConfig(context.Background(), p); err == nil {
				t.Fatal("changed cached helper bytes were executed")
			}
		})
	}
}

func TestOwnerProjectRendererFreshHelperOriginalRecipe(t *testing.T) {
	const env = "SUBYARD_OWNER_PROJECT_RENDERER_ROOT"
	if root := os.Getenv(env); root != "" {
		runtime := ownerProjectRendererFixture(t, root, false, "1", "1")
		raw, err := os.ReadFile(filepath.Join(root, "original.recipe"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := runtime.BindInstalledRuntimeContext(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		output, err := runtime.RenderOwnerProjectConfig(context.Background(), p)
		if err != nil || os.WriteFile(filepath.Join(root, "restored.output"), output, 0600) != nil || !bytes.Equal(p.Recipe(), raw) {
			t.Fatal("fresh helper changed original renderer authority", err)
		}
		return
	}
	root := testkit.TempDir(t)
	runtime := ownerProjectRendererFixture(t, root, true, "1", "1")
	p, err := runtime.PrepareOwnerRuntimeSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	want, err := runtime.RenderOwnerProjectConfig(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "original.recipe"), p.Recipe(), 0600)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestOwnerProjectRendererFreshHelperOriginalRecipe$", "-test.count=1")
	child.Env = append(os.Environ(), env+"="+root)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("fresh helper refused original recipe: %v (%d diagnostic bytes)", err, len(output))
	}
	got, err := os.ReadFile(filepath.Join(root, "restored.output"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("fresh helper renderer bytes differ", err)
	}
}

func TestOwnerInstanceRendererKeepsOriginalSelectionAndInitialFlags(t *testing.T) {
	root := testkit.TempDir(t)
	runtime := ownerProjectRendererFixture(t, root, true, "0", "1")
	for _, leaf := range []string{"scripts/lib-power.sh", "scripts/lib/instance-create.sh", "scripts/lib-vm-storage.sh"} {
		raw, err := os.ReadFile(filepath.Join("../../..", leaf))
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, leaf), raw, 0644)
	}
	runtime.Yard.YardKind = "container"
	runtime.Environment = ownerRenderEnvironment(runtime.Environment, "YARD_KIND", "container")
	runtime.Environment = append(runtime.Environment, "YARD_IMAGE=images:primary", "YARD_IMAGE_FALLBACK=images:fallback", "LIMITS_CPU=3")
	p, err := runtime.PrepareOwnerRuntimeSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	catalog := []byte(`[{"fingerprint":"` + strings.Repeat("a", 64) + `","type":"","aliases":[{"name":"primary"}]}]`)
	raw, err := runtime.RenderOwnerInstanceConfig(context.Background(), p, catalog)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(string(raw), "\x00")
	if len(fields) < 3 || fields[0] != "images:primary" || fields[1] != "container" || fields[len(fields)-1] != "" {
		t.Fatal("original selected image framing changed")
	}
	for _, want := range []string{"limits.cpu=3", "security.nesting=true", "security.syscalls.intercept.bpf=true", "security.syscalls.intercept.bpf.devices=true", "boot.autostart=false", "user.subyard.initialized=false"} {
		found := false
		for _, field := range fields {
			if field == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("original initial flag %s omitted", want)
		}
	}
	fallback, err := runtime.RenderOwnerInstanceConfig(context.Background(), p, []byte(`[]`))
	if err != nil || !bytes.HasPrefix(fallback, []byte("images:fallback\x00")) {
		t.Fatal("original fallback decision changed", err)
	}
	if _, err := runtime.RenderOwnerInstanceConfig(context.Background(), p, []byte(`{}`)); err == nil {
		t.Fatal("invalid catalog selected image")
	}
	testkit.WriteFile(t, filepath.Join(root, "scripts/lib/instance-create.sh"), []byte("exit 99\n"), 0644)
	again, err := runtime.RenderOwnerInstanceConfig(context.Background(), p, catalog)
	if err != nil || !bytes.Equal(again, raw) {
		t.Fatal("after-capture renderer bytes replaced", err)
	}
	if _, err := runtime.BindInstalledRuntimeContext(context.Background(), p.Recipe()); err == nil {
		t.Fatal("changed original source admitted before dispatch")
	}
}

type ownerInstanceRuntimeFixture struct {
	*testkit.Incus
	calls, checks                int
	failCreate, failCheck        bool
	trace, mode                  string
	volumeCalls                  int
	powerWrites                  int
	guardOrder                   []string
	sdk                          *incusclient.Client
	server                       *testkit.IncusServer
	kind                         domain.YardKind
	weight                       string
	probes, cpuCalls, generation int
	diagnostics                  bytes.Buffer
}

// Creation/volume provenance remains the existing explicit causal fixture;
// all physical power, fixed agent exec and publication requests use the real SDK.
func (f *ownerInstanceRuntimeFixture) Instance(ctx context.Context, project, name string) (ports.InstanceInfo, error) {
	if f.sdk != nil {
		return f.sdk.Instance(ctx, project, name)
	}
	return f.Incus.Instance(ctx, project, name)
}
func (f *ownerInstanceRuntimeFixture) SetOriginalInstancePowerIf(ctx context.Context, project, name, action string, force bool, uuid string, kind domain.YardKind) error {
	f.event(action)
	return f.sdk.SetOriginalInstancePowerIf(ctx, project, name, action, force, uuid, kind)
}
func (f *ownerInstanceRuntimeFixture) VMCPUConvergedIf(ctx context.Context, project, name string, apply bool, uuid string) (bool, error) {
	instance, err := f.Instance(ctx, project, name)
	if err != nil {
		return false, err
	}
	if !apply || instance.Type != domain.YardVM || instance.LocalConfig["volatile.uuid"] != uuid || ports.NativeOperationObserverFrom(ctx) == nil {
		return false, domain.ErrPlanStale
	}
	f.cpuCalls++
	f.event(fmt.Sprintf("cpu%d-generation%d", f.cpuCalls, f.generation))
	if f.mode == "cpu-failure" || f.mode == "fresh-restart-cpu-failure" && f.cpuCalls >= 3 {
		return false, errors.New("fixture CPU failure")
	}
	return true, nil
}
func (f *ownerInstanceRuntimeFixture) ProbeOriginalInstanceAgentIf(ctx context.Context, project, name, uuid string, kind domain.YardKind) (bool, error) {
	if ports.NativeOperationObserverFrom(ctx) != nil {
		return false, errors.New("agent probe acquired mutation authority")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 5*time.Second {
		return false, errors.New("unbounded fixed probe")
	}
	f.probes++
	f.event("agent")
	if f.weight != "" && f.probes == 1 {
		return false, errors.New("image seeding")
	}
	f.generation = 2
	f.server.QueueExec(testkit.IncusServerExecStep{})
	return f.sdk.ProbeOriginalInstanceAgentIf(ctx, project, name, uuid, kind)
}
func (f *ownerInstanceRuntimeFixture) ReconcileOriginalPublicationsIf(ctx context.Context, project, name, uuid string, kind domain.YardKind, contracts []ownerpublication.Contract, addresses []netip.Addr, checks ...func() error) ([]string, error) {
	return f.sdk.ReconcileOriginalPublicationsIf(ctx, project, name, uuid, kind, contracts, addresses, checks...)
}
func (f *ownerInstanceRuntimeFixture) SetOriginalInstanceConfigIf(ctx context.Context, project, name, uuid string, kind domain.YardKind, before, values map[string]string) error {
	return f.sdk.SetOriginalInstanceConfigIf(ctx, project, name, uuid, kind, before, values)
}

type ownerInstanceStartLock struct{ held bool }

func (l *ownerInstanceStartLock) Acquire(context.Context) (func(), error) {
	if l.held {
		return nil, errors.New("instance start reacquired network lock")
	}
	l.held = true
	return func() { l.held = false }, nil
}
func (f *ownerInstanceRuntimeFixture) ApplyPreparedOwnerPolicyInstance(ctx context.Context, p ports.PreparedOwnerInstance, _ []string, _ string) (*ports.NativeOwnerCreated, ports.PreparedOwnerInstance, error) {
	f.event("instance-post")
	f.calls++
	if f.failCreate {
		return nil, p, errors.New("original response unavailable")
	}
	instance := ports.InstanceInfo{Name: p.Instance, Project: p.Project, Status: "Stopped", Config: map[string]string{"volatile.uuid": "original-created-uuid", "user.subyard.owner_create": "original-instance-token", "user.subyard.managed": "true", "user.subyard.name": "default", "user.subyard.bridge": "incusbr0", "user.subyard.desired_power": "running", "user.subyard.initialized": "false", "boot.autostart": "false"}, LocalConfig: map[string]string{}}
	if f.sdk != nil {
		instance.Type = f.kind
		instance.LocalConfig = maps.Clone(instance.Config)
		if f.weight != "" {
			instance.Config["user.subyard.vm_cpu_weight"] = f.weight
			instance.LocalConfig["user.subyard.vm_cpu_weight"] = f.weight
		}
		f.server.SetInstance(p.Project, p.Instance, map[string]any{"name": p.Instance, "type": ownerPublicationAPIKind(f.kind), "status": "Stopped", "config": instance.LocalConfig, "expanded_config": instance.Config})
	}
	f.Instances[p.Project+"/"+p.Instance] = instance
	return &ports.NativeOwnerCreated{InstanceUUID: "original-created-uuid", Ref: ports.NativeOwnerCreateRef{Domain: "operation", Index: 1}, Intent: ports.NativeOwnerCreateIntent{Token: "original-instance-token"}}, p, nil
}
func (*ownerInstanceRuntimeFixture) CheckOwnerImagePolicy(context.Context, ports.PreparedOwnerImagePolicy, []string, string) error {
	return nil
}
func (*ownerInstanceRuntimeFixture) OwnerCreateServer(context.Context) (string, error) {
	return strings.Repeat("c", 64), nil
}
func (*ownerInstanceRuntimeFixture) CaptureOwnerImagePolicy(context.Context, string, string, domain.YardKind, []string, string) (ports.PreparedOwnerImagePolicy, error) {
	return ports.PreparedOwnerImagePolicy{}, domain.ErrPlanStale
}
func (*ownerInstanceRuntimeFixture) ReadOwnerPolicyImageCatalog(context.Context, ports.PreparedOwnerImagePolicy, []string, string) ([]byte, error) {
	return []byte(`[]`), nil
}

func (f *ownerInstanceRuntimeFixture) CheckOwnerCreatedInstance(ctx context.Context, p ports.PreparedOwnerInstance, created ports.NativeOwnerCreated) error {
	f.checks++
	f.guardOrder = append(f.guardOrder, "instance")
	if f.failCheck {
		return domain.ErrPlanStale
	}
	if f.sdk != nil {
		i, err := f.Instance(ctx, p.Project, p.Instance)
		if err != nil || i.Config["volatile.uuid"] != created.InstanceUUID || i.LocalConfig["volatile.uuid"] != created.InstanceUUID || i.Type != f.kind {
			return domain.ErrPlanStale
		}
	}
	return nil
}

func (f *ownerInstanceRuntimeFixture) event(value string) {
	file, _ := os.OpenFile(f.trace, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if file != nil {
		_, _ = file.WriteString(value + "\n")
		_ = file.Close()
	}
}
func (f *ownerInstanceRuntimeFixture) CheckOwnerVolumeGuard(context.Context, ports.PreparedOwnerVolume) error {
	return nil
}
func (f *ownerInstanceRuntimeFixture) CheckPreparedOwnerVolumeInstance(_ context.Context, p ports.PreparedOwnerVolume, _ *ports.PreparedOwnerInstance, created *ports.NativeOwnerCreated) error {
	uuid := p.InstanceUUID
	if created != nil {
		uuid = created.InstanceUUID
	}
	if uuid == "" {
		return domain.ErrPlanStale
	}
	if f.sdk != nil && f.mode == "replacement" {
		trace, _ := os.ReadFile(f.trace)
		if strings.Contains(string(trace), "boundary\n") {
			instance, err := f.Instance(context.Background(), p.Project, p.Instance)
			if err != nil {
				return err
			}
			cfg := maps.Clone(instance.LocalConfig)
			cfg["volatile.uuid"] = "replacement"
			f.server.SetInstance(p.Project, p.Instance, map[string]any{"name": p.Instance, "type": ownerPublicationAPIKind(f.kind), "status": instance.Status, "config": cfg, "expanded_config": maps.Clone(cfg)})
		}
	}
	instance, err := f.Instance(context.Background(), p.Project, p.Instance)
	if err != nil || instance.Config["volatile.uuid"] != uuid {
		return domain.ErrPlanStale
	}
	return nil
}
func (f *ownerInstanceRuntimeFixture) ApplyPreparedOwnerVolume(context.Context, ports.PreparedOwnerVolume, *ports.PreparedOwnerInstance, *ports.NativeOwnerCreated) (*ports.NativeOwnerCreated, error) {
	f.volumeCalls++
	f.event("volume-post")
	if f.mode == "volume-unknown" {
		return nil, errors.New("unknown volume response")
	}
	return &ports.NativeOwnerCreated{Intent: ports.NativeOwnerCreateIntent{Token: "original-volume-token"}}, nil
}
func (f *ownerInstanceRuntimeFixture) AttachPreparedOwnerVolume(context.Context, ports.PreparedOwnerVolume, *ports.PreparedOwnerInstance, *ports.NativeOwnerCreated, *ports.NativeOwnerCreated) error {
	f.event("attach")
	if f.mode == "attach-failure" {
		return domain.ErrPlanStale
	}
	return nil
}
func TestOwnerInstanceRuntimeConsumesOriginalResultBeforeFixedCommand(t *testing.T) {
	for _, mode := range []string{"success", "retained-results", "missing-original", "foreign-present", "lost-response", "guard-refusal", "pre-command-failure", "before-srv-failure", "volume-unknown", "attach-failure"} {
		t.Run(mode, func(t *testing.T) {
			runtime, incus := appArmorRuntime(t, "", false)
			runtime.Yard.YardKind = domain.YardVM
			root := runtime.RepositoryRoot
			delete(incus.Instances, "subyard/yard")
			trace := filepath.Join(testkit.TempDir(t), "trace")
			client := &ownerInstanceRuntimeFixture{Incus: incus, failCreate: mode == "lost-response", failCheck: mode == "guard-refusal", trace: trace, mode: mode}
			runtime.Incus = client
			runtime.ownerVolume = &ownerVolumeExecution{original: ports.PreparedOwnerVolume{Project: "subyard", Instance: "yard", Scope: "current", Absent: true}}
			if mode != "missing-original" {
				runtime.ownerInstance = &ownerInstanceExecution{original: ports.PreparedOwnerInstance{Project: "subyard", Instance: "yard", Scope: "current"}}
			}
			if mode == "foreign-present" {
				incus.Instances["subyard/yard"] = ports.InstanceInfo{Name: "yard", Project: "subyard", Config: map[string]string{"volatile.uuid": "foreign"}}
			}
			_, unlock, err := uninstall.OwnerLock(context.Background(), root, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			marker := filepath.Join(filepath.Dir(trace), "dispatched")
			runtime.Environment = append(runtime.Environment, "SUBYARD_HOME="+root, "MARKER="+marker, "TRACE="+trace, "MODE="+mode, "SUBYARD_CONFIG_DIR="+filepath.Join(root, "config"), "SUBYARD_REPOSITORY_ROOT="+root,
				"SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID=forged-environment-value")
			if err := os.Mkdir(filepath.Join(root, "scripts"), 0700); err != nil {
				t.Fatal(err)
			}
			script := `#!/bin/sh
set -eu
[ -f /proc/self/fd/3 ]
owner_phase=whole
case "$1" in
 --owner-before-instance) [ -z "$SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID" ]; printf 'before-instance\n' >>"$TRACE"; [ "$MODE" != pre-command-failure ]; exit ;;
 --owner-before-srv)
   [ "$SUBYARD_PREPARED_OWNER_INSTANCE_UUID" = original-created-uuid ]
   if [ "$MODE" = retained-results ]; then
     [ -z "$SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID" ]
   else [ "$SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID" = original-created-uuid ]; fi
   printf 'before-srv\n' >>"$TRACE"; [ "$MODE" != before-srv-failure ]; exit ;;
  --owner-before-start) owner_phase=before-start ;;
  --owner-after-start) owner_phase=after-start ;;
  --owner-prepare-boot) owner_phase=prepare-boot ;;
  --owner-after-boot) owner_phase=after-boot ;;
 *) exit 91 ;;
esac
[ "$SUBYARD_PREPARED_OWNER_INSTANCE_UUID" = original-created-uuid ]
[ "$SUBYARD_PREPARED_OWNER_VOLUME_TOKEN" = original-volume-token ]
if [ "$MODE" = retained-results ]; then
 [ -z "$SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID" ]
else [ "$SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID" = original-created-uuid ]; fi
if [ "$owner_phase" = prepare-boot ]; then printf 'prepare-boot\n' >>"$TRACE"; printf 'ready\n'; exit 0; fi
if [ "$owner_phase" != after-start ]; then # original storage before start
 printf 'before-start\n' >>"$TRACE"
fi
[ "$owner_phase" != before-start ] || exit 0
if [ "$owner_phase" != after-start ]; then # original physical start
 exit 92
fi
printf 'after-start\n' >>"$TRACE"
printf original >"$MARKER"
`
			testkit.WriteFile(t, filepath.Join(root, "scripts/03-create-subyard.sh"), []byte(script), 0700)
			if runtime.ownerInstance != nil {
				policy := ports.PreparedOwnerImagePolicy{Version: 1, Primary: []byte("images:primary"), Fallback: []byte("images:fallback"), Kind: "virtual-machine"}
				runtime.ownerInstance.original.Policy = &policy
				runtime.ownerInstance.original.Rendered = []byte("images:primary\x00virtual-machine\x00")
			}
			if err := os.MkdirAll(filepath.Join(root, "config"), 0700); err != nil {
				t.Fatal(err)
			}
			for leaf, body := range map[string]string{"scripts/lib-power.sh": "# retained test source\n", "scripts/lib/instance-create.sh": "subyard_instance_config_render(){ printf 'images:primary\\0virtual-machine\\0'; }\nsubyard_instance_approved_config_render(){ subyard_instance_config_render; }\n"} {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, leaf)), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, leaf), []byte(body), 0700)
			}
			testkit.WriteFile(t, filepath.Join(root, "scripts/reconcile-integrations.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
			testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
			server, err := testkit.NewIncusServer(testkit.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			client.server = server
			socket, _ := ownerPublicationSnapshotSocket(t, server, mode)
			client.sdk = incusclient.New(socket)
			client.kind = domain.YardVM
			client.generation = 1
			if mode == "foreign-present" {
				cfg := map[string]string{"volatile.uuid": "foreign"}
				server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "type": "virtual-machine", "status": "Stopped", "config": cfg, "expanded_config": maps.Clone(cfg)})
			}
			lock := &ownerInstanceStartLock{}
			runtime.NetworkPolicy = &yardnetwork.Service{Host: ownerPublicationPolicyHost{}, Lock: lock, Guard: func(context.Context) error {
				if !lock.held {
					return errors.New("host guard escaped original lock")
				}
				return nil
			}}

			if mode == "retained-results" {
				value, _, err := client.ApplyPreparedOwnerPolicyInstance(context.Background(), runtime.ownerInstance.original, nil, "")
				if err != nil {
					t.Fatal(err)
				}
				runtime.ownerInstance.created = value
				runtime.ownerVolume.created = &ports.NativeOwnerCreated{Intent: ports.NativeOwnerCreateIntent{Token: "original-volume-token"}}
				client.calls = 0
				testkit.WriteFile(t, trace, nil, 0600)
			}
			sources, err := runtime.PrepareOwnerRuntimeSources(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer sources.FinishOwnerSources(nil)
			if runtime.ownerInstance != nil {
				runtime.ownerInstance.rendererRuntime = runtime
			}
			runtime, err = runtime.OwnerSourceRuntime(context.Background(), sources)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(ports.WithNativeOperationObserver(context.Background(), custodyTransportObserver{}), 5*time.Second)
			defer cancel()
			err = runtime.ApplyStage(ctx, ports.ReconcileStageInstance)
			_, statErr := os.Stat(marker)
			events, _ := os.ReadFile(trace)
			if mode == "retained-results" {
				if err != nil || statErr != nil || client.calls != 0 || client.volumeCalls != 0 || string(events) != "before-instance\nbefore-srv\nattach\nbefore-start\nstart\nagent\nafter-start\n" {
					t.Fatal("retained results did not prevent repeat creates", err, statErr, string(events))
				}
			} else if mode == "success" {
				if err != nil || statErr != nil || client.calls != 1 || client.checks < 1 || client.powerWrites != 1 || string(events) != "before-instance\ninstance-post\nbefore-srv\nvolume-post\nattach\nbefore-start\nstart\nagent\nprepare-boot\nafter-start\n" {
					t.Fatal("original source order/result/OFD chain changed", err, statErr, string(events))
				}
			} else {
				if err == nil || !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("unknown/failed part dispatched next portion", err, statErr)
				}
				if (mode == "missing-original" || mode == "foreign-present" || mode == "pre-command-failure") && client.calls != 0 {
					t.Fatal("pre-instance refusal submitted create")
				}
				if (mode == "lost-response" || mode == "guard-refusal" || mode == "before-srv-failure" || mode == "volume-unknown" || mode == "attach-failure") && client.calls != 1 {
					t.Fatal("causal case did not reach its original create boundary", mode, err, string(events))
				}
				if (mode == "lost-response" || mode == "guard-refusal" || mode == "before-srv-failure") && client.volumeCalls != 0 {
					t.Fatal("unproved previous portion submitted volume")
				}
			}
		})
	}
}

type ownerImageRuntimeFixture struct {
	*testkit.Incus
	planning          bool
	catalog, selected int
}

func (f *ownerImageRuntimeFixture) fixtureCatalog(context.Context, string, domain.YardKind, []string, string) ([]byte, error) {
	f.catalog++
	if !f.planning {
		return nil, errors.New("fresh catalog selection forbidden")
	}
	return []byte(`[{"fingerprint":"` + strings.Repeat("a", 64) + `","type":"container","aliases":[{"name":"primary"}]}]`), nil
}
func ownerInstanceRecipeRuntime(t *testing.T, root string, create bool) (Runtime, *ownerImageRuntimeFixture) {
	t.Helper()
	runtime := ownerProjectRendererFixture(t, root, create, "0", "1")
	if create {
		for _, leaf := range []string{"scripts/lib-power.sh", "scripts/lib/instance-create.sh", "scripts/lib-vm-storage.sh"} {
			raw, err := os.ReadFile(filepath.Join("../../..", leaf))
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, leaf), raw, 0644)
		}
	}
	runtime.Yard.YardKind = domain.YardContainer
	runtime.Environment = ownerRenderEnvironment(runtime.Environment, "YARD_KIND", "container")
	runtime.Environment = append(runtime.Environment, "YARD_IMAGE=images:primary", "YARD_IMAGE_FALLBACK=images:fallback")
	client := &ownerImageRuntimeFixture{Incus: &testkit.Incus{}, planning: create}
	runtime.Incus = client
	return runtime, client
}

type ownerOriginalCreateCapsule struct {
	Instance ports.PreparedOwnerInstance
	Volume   ports.PreparedOwnerVolume
}

func (f *ownerImageRuntimeFixture) PrepareOwnerVolumeGuard(_ context.Context, p ports.PreparedOwnerVolume) (ports.PreparedOwnerVolume, error) {
	return p, nil
}
func TestOwnerInstanceOriginalCapsuleFreshProcessWithoutCatalogPlanning(t *testing.T) {
	const key = "SUBYARD_OWNER_INSTANCE_ORIGINAL_ROOT"
	if root := os.Getenv(key); root != "" {
		runtime, client := ownerInstanceRecipeRuntime(t, root, false)
		sourceRaw, err := os.ReadFile(filepath.Join(root, "original.instance-source"))
		if err != nil {
			t.Fatal(err)
		}
		sources, err := runtime.BindInstalledRuntimeContext(context.Background(), sourceRaw)
		if err != nil {
			t.Fatal(err)
		}
		defer sources.Close()
		raw, err := os.ReadFile(filepath.Join(root, "original.instance-record"))
		if err != nil {
			t.Fatal(err)
		}
		var capsule ownerOriginalCreateCapsule
		if err := nativejson.DecodeLosslessNativeJSON(raw, &capsule, 32<<20); err != nil {
			t.Fatal(err)
		}
		original := capsule.Instance
		restored, err := runtime.WithOwnerInstance(context.Background(), sources, original)
		if err != nil || restored.ownerInstance == nil || client.catalog != 0 || client.selected != 0 {
			t.Fatal("fresh helper selected new image authority", err)
		}
		if _, err := runtime.WithOwnerVolume(context.Background(), sources, capsule.Volume); err != nil {
			t.Fatal("fresh helper lost original volume capsule", err)
		}
		foreignVolume := capsule.Volume
		foreignVolume.Pool = "foreign"
		if _, err := runtime.WithOwnerVolume(context.Background(), sources, foreignVolume); !errors.Is(err, domain.ErrPlanStale) {
			t.Fatal("foreign fixed pool admitted", err)
		}
		foreignVolume = capsule.Volume
		foreignVolume.Rendered = []byte("filesystem\x00\x00type=disk\x00pool=foreign\x00source=yard-srv\x00path=/srv\x00")
		if _, err := runtime.WithOwnerVolume(context.Background(), sources, foreignVolume); !errors.Is(err, domain.ErrPlanStale) {
			t.Fatal("foreign rendered original device admitted", err)
		}
		original.Instance = "foreign"
		if _, err := runtime.WithOwnerInstance(context.Background(), sources, original); !errors.Is(err, domain.ErrPlanStale) {
			t.Fatal("wrong instance scope admitted", err)
		}
		original = restored.ownerInstance.original
		original.Source = strings.Repeat("b", 64)
		if _, err := runtime.WithOwnerInstance(context.Background(), sources, original); !errors.Is(err, domain.ErrPlanStale) {
			t.Fatal("wrong original source admitted", err)
		}
		return
	}
	root := testkit.TempDir(t)
	runtime, client := ownerInstanceRecipeRuntime(t, root, true)
	sources, err := runtime.PrepareOwnerRuntimeSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sources.Close()
	original, err := runtime.PrepareOwnerInstancePolicy(context.Background(), sources, "current")
	if err != nil || client.catalog != 0 || client.selected != 0 {
		t.Fatal("original policy resolved before acceptance", err)
	}
	volume, err := runtime.PrepareOwnerVolume(context.Background(), sources, "current")
	if err != nil || volume == nil {
		t.Fatal("original volume capsule missing", err)
	}
	raw, err := nativejson.EncodeLosslessNativeJSON(ownerOriginalCreateCapsule{*original, *volume}, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "original.instance-source"), sources.Recipe(), 0600)
	testkit.WriteFile(t, filepath.Join(root, "original.instance-record"), raw, 0600)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestOwnerInstanceOriginalCapsuleFreshProcessWithoutCatalogPlanning$", "-test.count=1")
	command.Env = append(os.Environ(), key+"="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fresh helper refused original capsule: %v (%d diagnostic bytes)", err, len(output))
	}
}

func TestOwnerBlockVMPreInstancePortionPreservesOriginalRootDiskOrder(t *testing.T) {
	repository, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := testkit.TempDir(t)
	script := `set -euo pipefail
source "$1/tests/helpers/test-context.sh"
setup_test_context "$2"
export YARD_KIND=vm ROOT_DISK_SIZE=20GiB SRV_VOLUME_TYPE=block SRV_VOLUME_SIZE=4GiB
export YARD_IMAGE=images:debian/13 YARD_IMAGE_FALLBACK=images:ubuntu/24.04 VM_FREE_PAGE_REPORTING=0
export SRV_POOL=default SRV_VOLUME=yard-srv HOME="$2/home"
export ASSUME_YES=1 SUBYARD_NO_AUDIT=1 SUBYARD_POWER_DESIRED=stopped
export MOCK_ROOT="$2" MOCK_TRACE="$2/trace"
install -d -m 0700 "$2/bin"
export PATH="$2/bin:$PATH"
cat >"$2/bin/incus" <<'INCUS'
#!/bin/bash
set -eu
printf '%s\n' "$*" >>"$MOCK_TRACE"
case "$*" in
 'info'|'project show subyard') exit 0 ;;
 'info yard --project subyard') exit 1 ;;
 'profile device get default root size --project subyard') cat "$MOCK_ROOT/root-size" ;;
 'profile device set default root size=20GiB --project subyard') printf '20GiB\n' >"$MOCK_ROOT/root-size" ;;
 'config get yard volatile.uuid --project subyard') printf 'original-uuid\n' ;;
 'config get yard user.subyard.owner_create --project subyard') printf 'original-token\n' ;;
 'storage volume get default yard-srv user.subyard.owner_create --project subyard') printf 'foreign-volume-token\n' ;;
 'storage volume show default yard-srv --project subyard') exit 1 ;;
 'image list images: --project subyard --format=json') printf '[{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","type":"virtual-machine","aliases":[{"name":"debian/13"}]}]\n' ;;
 init*) exit 17 ;;
 *) exit 90 ;;
esac
INCUS
printf '#!/bin/sh\nexit 0\n' >"$2/bin/dpkg"
printf '#!/bin/sh\nprintf "inactive\\n"\nexit 3\n' >"$2/bin/systemctl"
chmod 0700 "$2/bin/"{incus,dpkg,systemctl}
: >"$2/root-size";: >"$2/trace"
# Stop the ordinary path at its first create request, after all preconditions.
if bash "$1/scripts/03-create-subyard.sh" --yes >"$2/ordinary-output" 2>&1;then exit 61;fi
# The first info yard occurs inside root-size preparation. Retain through the
# root write instead of inferring order from its resulting desired state.
sed -n '1,/^profile device set default root size=20GiB --project subyard$/p' "$2/trace" >"$2/ordinary-prefix"
grep -q '^init ' "$2/trace" || { cat "$2/ordinary-output" "$2/trace" >&2; exit 62; }
: >"$2/root-size";: >"$2/trace"
SUBYARD_PREPARED_OWNER_PHASE=1 bash "$1/scripts/03-create-subyard.sh" --owner-before-instance --yes >"$2/native-output" 2>&1
cmp "$2/ordinary-prefix" "$2/trace"
[ "$(cat "$2/root-size")" = 20GiB ]
! grep -Eq '^(init |image |storage volume )' "$2/trace"
# Native portions cannot be reached by an unbound ordinary selector.
if bash "$1/scripts/03-create-subyard.sh" --owner-before-instance --yes >"$2/refused-output" 2>&1;then exit 63;fi
# Even a retained volume has no fallback create authority in a native half.
: >"$2/trace"
if SUBYARD_PREPARED_OWNER_PHASE=1 SUBYARD_PREPARED_OWNER_INSTANCE_UUID=original-uuid \
 SRV_VOLUME_TYPE=filesystem bash "$1/scripts/03-create-subyard.sh" --owner-after-srv --yes >"$2/refused-output" 2>&1;then exit 64;fi
! grep -q '^storage volume create ' "$2/trace"
# An explicit create result's token cannot accept a foreign same-name volume.
: >"$2/trace"
if SUBYARD_PREPARED_OWNER_PHASE=1 SUBYARD_PREPARED_OWNER_INSTANCE_UUID=original-uuid \
 SUBYARD_PREPARED_OWNER_VOLUME_TOKEN=original-volume-token \
 bash "$1/scripts/03-create-subyard.sh" --owner-after-srv --yes >"$2/refused-output" 2>&1;then exit 65;fi
! grep -Eq '^(storage volume create |config device (add|remove) )' "$2/trace"
# Fixed new portions require the same original prepared admission. Unknown
# owner selectors must never fall through to the ordinary whole body.
for selector in --owner-before-start --owner-after-start --owner-unsupported;do
 : >"$2/trace"
 if bash "$1/scripts/03-create-subyard.sh" "$selector" --yes >"$2/refused-output" 2>&1;then exit 66;fi
 [ ! -s "$2/trace" ] || exit 67
done
`
	command := exec.Command("bash", "-eu", "-s", "--", repository, root)
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("original block-VM pre-instance trace changed: %v (%d fixture diagnostic bytes): %s", err, len(output), output)
	}
}

func (f *ownerImageRuntimeFixture) OwnerCreateServer(context.Context) (string, error) {
	return strings.Repeat("c", 64), nil
}

func (f *ownerImageRuntimeFixture) CaptureOwnerImagePolicy(_ context.Context, primary, fallback string, _ domain.YardKind, _ []string, _ string) (ports.PreparedOwnerImagePolicy, error) {
	return ports.PreparedOwnerImagePolicy{Version: 1, Primary: []byte(primary), Fallback: []byte(fallback), Kind: "container"}, nil
}
func (*ownerImageRuntimeFixture) CheckOwnerImagePolicy(context.Context, ports.PreparedOwnerImagePolicy, []string, string) error {
	return nil
}
func (f *ownerImageRuntimeFixture) ReadOwnerPolicyImageCatalog(ctx context.Context, p ports.PreparedOwnerImagePolicy, env []string, home string) ([]byte, error) {
	return f.fixtureCatalog(ctx, string(p.Primary), domain.YardContainer, env, home)
}

func TestOwnerPolicyCapsuleFreshProcessDoesNotResolveBeforeAcceptance(t *testing.T) {
	const key = "SUBYARD_OWNER_POLICY_CAPSULE_ROOT"
	if root := os.Getenv(key); root != "" {
		runtime, client := ownerInstanceRecipeRuntime(t, root, false)
		raw, err := os.ReadFile(filepath.Join(root, "policy.source"))
		if err != nil {
			t.Fatal(err)
		}
		sources, err := runtime.BindInstalledRuntimeContext(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		defer sources.Close()
		raw, err = os.ReadFile(filepath.Join(root, "policy.original"))
		if err != nil {
			t.Fatal(err)
		}
		var saved struct{ Instance ports.PreparedOwnerInstance }
		if err = nativejson.DecodeLosslessNativeJSON(raw, &saved, 32<<20); err != nil {
			t.Fatal(err)
		}
		bound, err := runtime.WithOwnerInstance(context.Background(), sources, saved.Instance)
		if err != nil || bound.ownerInstance == nil || client.catalog != 0 || client.selected != 0 {
			t.Fatal("fresh bind resolved image authority", err)
		}
		for _, change := range []string{"resolved-body", "kind", "primary", "flags", "target"} {
			p := saved.Instance
			policy := *p.Policy
			p.Policy = &policy
			switch change {
			case "resolved-body":
				p.Image.Version = 2
			case "kind":
				p.Policy.Kind = "virtual-machine"
			case "primary":
				p.Policy.Primary = []byte("images:foreign")
			case "flags":
				p.Rendered = append(bytes.Clone(p.Rendered), []byte("-c\x00security.nesting=false\x00")...)
			case "target":
				p.Server = strings.Repeat("f", 64)
			}
			if _, err = runtime.WithOwnerInstance(context.Background(), sources, p); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("malformed original policy admitted", change, err)
			}
		}
		return
	}
	root := testkit.TempDir(t)
	runtime, client := ownerInstanceRecipeRuntime(t, root, true)
	sources, err := runtime.PrepareOwnerRuntimeSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sources.Close()
	original, err := runtime.PrepareOwnerInstancePolicy(context.Background(), sources, "current")
	if err != nil || original == nil || client.catalog != 0 || client.selected != 0 {
		t.Fatal("preconsent image resolution occurred", err)
	}
	raw, err := nativejson.EncodeLosslessNativeJSON(struct{ Instance ports.PreparedOwnerInstance }{*original}, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "policy.original"), raw, 0600)
	testkit.WriteFile(t, filepath.Join(root, "policy.source"), sources.Recipe(), 0600)
	child := exec.Command(os.Args[0], "-test.run=^TestOwnerPolicyCapsuleFreshProcessDoesNotResolveBeforeAcceptance$")
	child.Env = append(os.Environ(), key+"="+root)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("fresh original policy: %v (%s)", err, output)
	}
}

type ownerRestoredResultObserver struct {
	custodyTransportObserver
	values []ports.NativeOwnerCreated
}

func (f ownerRestoredResultObserver) BeforeOwnerCreate(ports.NativeOwnerCreateIntent) (ports.NativeOwnerCreateRef, error) {
	return ports.NativeOwnerCreateRef{}, domain.ErrPlanStale
}
func (f ownerRestoredResultObserver) RecordOwnerCreated(ports.NativeOwnerCreated) error {
	return domain.ErrPlanStale
}
func (f ownerRestoredResultObserver) ReadOwnerCreated(ref ports.NativeOwnerCreateRef) (ports.NativeOwnerCreated, error) {
	for _, v := range f.values {
		if v.Ref == ref {
			return v, nil
		}
	}
	return ports.NativeOwnerCreated{}, domain.ErrPlanStale
}
func (f *ownerInstanceRuntimeFixture) CheckOwnerCreatedVolume(context.Context, ports.PreparedOwnerVolume, ports.NativeOwnerCreated) error {
	f.guardOrder = append(f.guardOrder, "volume")
	if f.failCheck {
		return domain.ErrPlanStale
	}
	return nil
}
func TestOwnerRestoredResultsRequireOriginalRefBeforeAnyCreate(t *testing.T) {
	for _, mode := range []string{"valid", "missing-original", "foreign-ref", "foreign-uuid", "actual-guard", "preexisting-volume"} {
		t.Run(mode, func(t *testing.T) {
			client := &ownerInstanceRuntimeFixture{failCheck: mode == "actual-guard"}
			runtime := Runtime{Incus: client, Yard: domain.Context{IncusProject: "subyard"}}
			runtime.ownerInstance = &ownerInstanceExecution{original: ports.PreparedOwnerInstance{Project: "subyard", Instance: "yard", Scope: "current"}}
			runtime.ownerVolume = &ownerVolumeExecution{original: ports.PreparedOwnerVolume{Project: "subyard", Volume: "srv", Scope: "current", Absent: mode != "preexisting-volume"}}
			a := ports.NativeOwnerCreated{Ref: ports.NativeOwnerCreateRef{Producer: strings.Repeat("a", 64), Domain: "operation", Index: 1}, Intent: ports.NativeOwnerCreateIntent{Kind: "instance", Scope: "current", Project: "subyard", Resource: "yard"}, InstanceUUID: "original-created-uuid"}
			b := ports.NativeOwnerCreated{Ref: ports.NativeOwnerCreateRef{Producer: strings.Repeat("a", 64), Domain: "command", Index: 1}, Intent: ports.NativeOwnerCreateIntent{Kind: "srv-volume", Scope: "current", Project: "subyard", Resource: "srv"}}
			originals := []ports.NativeOwnerCreated{a, b}
			values := append([]ports.NativeOwnerCreated(nil), originals...)
			if mode == "missing-original" {
				originals = nil
			}
			if mode == "foreign-ref" {
				values[0].Ref.Producer = strings.Repeat("b", 64)
			}
			if mode == "foreign-uuid" {
				values[0].InstanceUUID = "foreign"
			}
			ctx := ports.WithNativeOperationObserver(context.Background(), ownerRestoredResultObserver{values: originals})
			restored, err := runtime.WithOwnerCreatedResults(ctx, values)
			if mode == "valid" {
				if err != nil || restored.ownerInstance.created == nil || *restored.ownerInstance.created != a || restored.ownerVolume.created == nil || *restored.ownerVolume.created != b {
					t.Fatal("original result lost", err)
				}
				if strings.Join(client.guardOrder, ",") != "volume,instance" {
					t.Fatal("instance validated before original volume authority", client.guardOrder)
				}
			} else if err == nil {
				t.Fatal("foreign/unproved result accepted")
			}
			if client.calls != 0 || client.volumeCalls != 0 {
				t.Fatal("restoration submitted create")
			}
		})
	}
}

func (f *ownerInstanceRuntimeFixture) SetOwnerCreatedPower(ctx context.Context, p ports.PreparedOwnerInstance, created ports.NativeOwnerCreated, desired string, initialized bool) error {
	if f.failCheck {
		return domain.ErrPlanStale
	}
	f.powerWrites++
	if f.sdk != nil {
		return f.sdk.SetOriginalInstanceConfigIf(ctx, p.Project, p.Instance, created.InstanceUUID, f.kind, nil, map[string]string{"user.subyard.managed": "true", "user.subyard.initialized": strconv.FormatBool(initialized), "user.subyard.desired_power": desired, "user.subyard.name": "default", "user.subyard.bridge": "incusbr0", "boot.autostart": "false"})
	}
	return nil
}
func TestOwnerKnownInstancePowerUsesOnlyTypedSuccessorLeaf(t *testing.T) {
	for _, mode := range []string{"set", "ensure", "no-observer", "foreign-instance", "unknown-key", "wrong-name", "missing-init", "guard-refusal"} {
		t.Run(mode, func(t *testing.T) {
			client := &ownerInstanceRuntimeFixture{failCheck: mode == "guard-refusal"}
			runtime := Runtime{Incus: client, Yard: domain.Context{YardName: "default", IncusProject: "subyard", YardInstanceName: "yard", IncusBridge: "incusbr0"}}
			runtime.ownerInstance = &ownerInstanceExecution{original: ports.PreparedOwnerInstance{Project: "subyard", Instance: "yard"}, created: &ports.NativeOwnerCreated{InstanceUUID: "original-created-uuid"}}
			values := map[string]string{"user.subyard.name": "default", "user.subyard.bridge": "incusbr0", "boot.autostart": "false", "user.subyard.managed": "true", "user.subyard.desired_power": "running", "user.subyard.initialized": "false"}
			ctx := ports.WithNativeOperationObserver(context.Background(), custodyTransportObserver{})
			instance := "yard"
			switch mode {
			case "ensure":
				delete(values, "user.subyard.managed")
				delete(values, "user.subyard.desired_power")
				delete(values, "user.subyard.initialized")
			case "no-observer":
				ctx = context.Background()
			case "foreign-instance":
				instance = "foreign"
			case "unknown-key":
				values["arbitrary"] = "value"
			case "wrong-name":
				values["user.subyard.name"] = "foreign"
			case "missing-init":
				delete(values, "user.subyard.initialized")
			}
			err := runtime.powerService().Config.SetInstanceConfig(ctx, "subyard", instance, values)
			if mode == "set" {
				if err != nil || client.powerWrites != 1 {
					t.Fatal("typed power leaf notconsumed", err)
				}
			} else if mode == "ensure" {
				if err != nil || client.powerWrites != 0 || client.checks != 1 {
					t.Fatal("Ensure rewrote known original metadata", err)
				}
			} else if err == nil || client.powerWrites != 0 {
				t.Fatal("foreign/unproven power effect", err)
			}
		})
	}
}

type ownerProjectRuntimeFixture struct {
	*testkit.Incus
	trace string
	mode  string
	posts int
}

func (f *ownerProjectRuntimeFixture) ApplyPreparedOwnerYardProject(_ context.Context, p ports.PreparedImageProject, scope string) (*ports.NativeOwnerCreated, error) {
	f.posts++
	file, _ := os.OpenFile(f.trace, os.O_APPEND|os.O_WRONLY, 0600)
	if file != nil {
		file.WriteString("project-post\n")
		file.Close()
	}
	if f.mode == "unknown-response" {
		return nil, errors.New("original response unavailable")
	}
	return &ports.NativeOwnerCreated{Intent: ports.NativeOwnerCreateIntent{Scope: scope, Project: p.Project, Resource: p.Project, Token: "original-project-token"}}, nil
}
func (f *ownerProjectRuntimeFixture) CheckOwnerCreatedYardProject(context.Context, ports.PreparedImageProject, ports.NativeOwnerCreated) error {
	if f.mode == "guard-refusal" {
		return domain.ErrPlanStale
	}
	return nil
}
func TestOwnerYardProjectOriginalPortionsAndCausalResult(t *testing.T) {
	for _, mode := range []string{"success", "retained", "before-failure", "unknown-response", "guard-refusal", "after-failure", "missing-capsule"} {
		t.Run(mode, func(t *testing.T) {
			runtime, incus := appArmorRuntime(t, "", false)
			root := runtime.RepositoryRoot
			_, unlock, err := uninstall.OwnerLock(context.Background(), root, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			trace := filepath.Join(root, "project-trace")
			marker := filepath.Join(root, "project-marker")
			client := &ownerProjectRuntimeFixture{Incus: incus, trace: trace, mode: mode}
			runtime.Incus = client
			runtime.Environment = append(runtime.Environment, "SUBYARD_HOME="+root, "TRACE="+trace, "MARKER="+marker, "MODE="+mode)
			runtime.ownerProject = &ownerProjectExecution{original: ports.PreparedImageProject{Project: "subyard", Absent: true}, scope: "current"}
			if mode == "missing-capsule" {
				runtime.ownerProject = nil
			}
			if mode == "retained" {
				runtime.ownerProject.created = &ports.NativeOwnerCreated{Intent: ports.NativeOwnerCreateIntent{Scope: "current", Token: "original-project-token"}}
			}
			testkit.WriteFile(t, trace, nil, 0600)
			if err := os.MkdirAll(filepath.Join(root, "scripts"), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "scripts/02-create-project.sh"), []byte(`#!/bin/sh
set -eu
[ -f /proc/self/fd/3 ]
case "$1" in
 --owner-before-project) printf 'before-project\n' >>"$TRACE"; [ "$MODE" != before-failure ] ;;
 --owner-after-project) [ "$SUBYARD_PREPARED_OWNER_PROJECT_TOKEN" = original-project-token ]; printf 'after-project\n' >>"$TRACE"; [ "$MODE" != after-failure ]; printf original >"$MARKER" ;;
 *) exit 91 ;;
esac
`), 0700)
			body, err := os.ReadFile("../../../scripts/03-create-subyard.sh")
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "scripts/03-create-subyard.sh"), body, 0700)
			if err := os.MkdirAll(filepath.Join(root, "config"), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "scripts/reconcile-integrations.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
			testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
			runtime.Environment = append(runtime.Environment, "SUBYARD_CONFIG_DIR="+filepath.Join(root, "config"), "SUBYARD_REPOSITORY_ROOT="+root)
			sources, err := runtime.PrepareOwnerRuntimeSources(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer sources.FinishOwnerSources(nil)
			runtime, err = runtime.OwnerSourceRuntime(context.Background(), sources)
			if err != nil {
				t.Fatal(err)
			}
			ctx := ports.WithNativeOperationObserver(context.Background(), custodyTransportObserver{})
			err = runtime.ApplyStage(ctx, ports.ReconcileStageProject)
			events, _ := os.ReadFile(trace)
			_, markerErr := os.Stat(marker)
			if mode == "success" || mode == "retained" {
				want := "before-project\nproject-post\nafter-project\n"
				posts := 1
				if mode == "retained" {
					want = "before-project\nafter-project\n"
					posts = 0
				}
				if err != nil || markerErr != nil || string(events) != want || client.posts != posts || runtime.OwnerCreatedProject() == nil {
					t.Fatal("project portion/result/OFD order changed", err, markerErr, string(events))
				}
			} else if err == nil || !errors.Is(markerErr, os.ErrNotExist) {
				t.Fatal("failed/unproved project dispatched successor", err, markerErr)
			}
			if (mode == "before-failure" || mode == "missing-capsule") && client.posts != 0 {
				t.Fatal("precondition failure submitted project")
			}
		})
	}
}

func TestOwnerProjectNativePortionsShareOriginalPolicyOrder(t *testing.T) {
	repository, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := testkit.TempDir(t)
	script := `set -euo pipefail
source "$1/tests/helpers/test-context.sh"
setup_test_context "$2"
export HOME="$2/home" ASSUME_YES=1 SRV_POOL=default SRV_VOLUME=yard-srv STORAGE_POOL=default
export MOCK_LOG="$2/trace" MOCK_EXISTS="$2/project" MOCK_TOKEN=original-project-token
install -d -m 0700 "$2/bin"
export PATH="$2/bin:$PATH"
cat >"$2/bin/incus" <<'INCUS'
#!/bin/bash
set -eu
printf '%s\n' "$*" >>"$MOCK_LOG"
case "${1:-} ${2:-}" in
 'info ') exit 0 ;;
 'project show') test -f "$MOCK_EXISTS" ;;
 'project create') exit 91 ;;
 'project get')
  test -f "$MOCK_EXISTS"
  case "$4" in
   user.subyard.installation) printf '1\n' ;;
   user.subyard.operator_uid) id -u ;;
   user.subyard.data_home) printf '%s\n' "$SUBYARD_HOME" ;;
   user.subyard.config_home) printf '%s\n' "$SUBYARD_CONFIG_HOME" ;;
   user.subyard.owner_create) printf '%s\n' "$MOCK_TOKEN" ;;
  esac ;;
 'project set'|'project unset') exit 0 ;;
 'profile device') [ "$3" = list ] || [ "$3" = add ] ;;
 *) exit 92 ;;
esac
INCUS
chmod 0700 "$2/bin/incus"
: >"$MOCK_LOG"
SUBYARD_PREPARED_OWNER_PHASE=1 bash "$1/scripts/02-create-project.sh" --owner-before-project --yes >"$2/before-output" 2>&1
! grep -Eq '^(project (create|set|unset)|profile device add)' "$MOCK_LOG"
# A missing SDK result can never fall back to CLI project creation.
: >"$MOCK_LOG"
if SUBYARD_PREPARED_OWNER_PHASE=1 bash "$1/scripts/02-create-project.sh" --owner-after-project --yes >"$2/missing-output" 2>&1; then exit 61;fi
! grep -Eq '^(project (create|set|unset)|profile device add)' "$MOCK_LOG"
touch "$MOCK_EXISTS"
: >"$MOCK_LOG"
bash "$1/scripts/02-create-project.sh" --yes >"$2/ordinary-output" 2>&1
grep -E '^(project (set|unset)|profile device add)' "$MOCK_LOG" >"$2/ordinary-mutations"
: >"$MOCK_LOG"
SUBYARD_PREPARED_OWNER_PHASE=1 SUBYARD_PREPARED_OWNER_PROJECT_TOKEN=original-project-token \
 bash "$1/scripts/02-create-project.sh" --owner-after-project --yes >"$2/after-output" 2>&1
grep -E '^(project (set|unset)|profile device add)' "$MOCK_LOG" >"$2/native-mutations"
cmp "$2/ordinary-mutations" "$2/native-mutations"
! grep -q '^project create ' "$MOCK_LOG"
# Root disk and NIC ordering comes from the same original body.
grep '^profile device add ' "$MOCK_LOG" >"$2/device-order"
sed -n '1p' "$2/device-order" | grep -q 'default root disk'
sed -n '2p' "$2/device-order" | grep -q 'default eth0 nic'
: >"$MOCK_LOG"
if MOCK_TOKEN=foreign SUBYARD_PREPARED_OWNER_PHASE=1 SUBYARD_PREPARED_OWNER_PROJECT_TOKEN=original-project-token \
 bash "$1/scripts/02-create-project.sh" --owner-after-project --yes >"$2/foreign-output" 2>&1;then exit 62;fi
! grep -Eq '^(project (create|set|unset)|profile device add)' "$MOCK_LOG"
`
	command := exec.Command("bash", "-eu", "-s", "--", repository, root)
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native02 order/refusal changed: %v (%d diagnostic bytes)", err, len(output))
	}
}

// The new selected 03 portions are actual retained script bytes. Only its
// already-covered earlier portions and external disk/page-reporting contracts
// are fixtures here. SDK power, fixed agent exec and publication are real HTTP;
// host routing and QEMU/cgroup generation remain explicit host-free fixtures.
type ownerAdmissionRuntimeFixture struct {
	*ownerInstanceRuntimeFixture
	posts int
}

func (f *ownerAdmissionRuntimeFixture) ApplyPreparedOwnerYardProject(context.Context, ports.PreparedImageProject, string) (*ports.NativeOwnerCreated, error) {
	f.posts++
	return nil, errors.New("unsupported source reached project create")
}
func (f *ownerAdmissionRuntimeFixture) CheckOwnerCreatedYardProject(context.Context, ports.PreparedImageProject, ports.NativeOwnerCreated) error {
	return nil
}

func TestOwnerInstanceRetainedStartBodies(t *testing.T) {
	for _, kind := range []domain.YardKind{domain.YardVM, domain.YardContainer} {
		for _, created := range []bool{false, true} {
			for _, mode := range []string{"success", "old-script", "boundary-failure", "foreign-volume", "replacement", "cpu-failure", "tail-failure"} {
				if kind == domain.YardContainer && (mode == "cpu-failure" || mode == "tail-failure") {
					continue
				}
				t.Run(fmt.Sprintf("%s-created%v-%s", kind, created, mode), func(t *testing.T) {
					runtime, client, lock, sourceRoot := ownerInstanceRetainedBodyFixture(t, kind, created, mode)
					home := runtime.Yard.Paths.DataHome
					_, unlock, err := uninstall.OwnerLock(context.Background(), home, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer unlock()
					sources, err := runtime.PrepareOwnerRuntimeSources(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					defer sources.FinishOwnerSources(nil)
					runtime, err = runtime.OwnerSourceRuntime(context.Background(), sources)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(filepath.Join(runtime.RepositoryRoot, "bin/yard-engine")); !os.IsNotExist(err) {
						t.Fatal("retained source acquired dispatcher")
					}
					ctx, cancel := context.WithTimeout(ports.WithNativeOperationObserver(context.Background(), custodyTransportObserver{}), 8*time.Second)
					defer cancel()
					if mode == "old-script" {
						// Demonstrate the old admission behavior itself, without any Incus work:
						// its unknown new selector enters whole and reaches the first-body marker.
						err = runtime.runScriptEnvironment(context.Background(), nil, map[string]string{"SUBYARD_POWER_DESIRED": "stopped"}, "03-create-subyard.sh", "--owner-before-start", "--yes")
						if data, readErr := os.ReadFile(filepath.Join(home, "old-whole")); err != nil || readErr != nil || string(data) != "old-whole" {
							t.Fatal("old selector no longer demonstrates whole fallback", err, readErr)
						}
						if err := os.Remove(filepath.Join(home, "old-whole")); err != nil {
							t.Fatal(err)
						}
						// Count every HTTP mutation, including metadata and project calls,
						// rather than only the power endpoint. No write is expected here.
						var mutations atomic.Uint64
						socket := filepath.Join(testkit.TempDir(t), "admission.sock")
						listener, err := net.Listen("unix", socket)
						if err != nil {
							t.Fatal(err)
						}
						httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.Method != http.MethodGet && r.Method != http.MethodHead {
								mutations.Add(1)
							}
							client.server.ServeHTTP(w, r)
						})}
						go func() { _ = httpServer.Serve(listener) }()
						defer httpServer.Close()
						client.sdk = incusclient.New(socket)
						project := &ownerAdmissionRuntimeFixture{ownerInstanceRuntimeFixture: client}
						runtime.Incus = project
						runtime.ownerProject = &ownerProjectExecution{original: ports.PreparedImageProject{Project: "subyard", Absent: true}, scope: "current"}
						entries := []struct {
							name string
							call func() error
						}{
							{"apply-instance", func() error { return runtime.ApplyStage(ctx, ports.ReconcileStageInstance) }},
							{"apply-project", func() error { return runtime.ApplyStage(ctx, ports.ReconcileStageProject) }},
							{"check-instance", func() error {
								_, err := runtime.CheckStage(context.Background(), ports.ReconcileStageInstance)
								return err
							}},
							{"check-project", func() error {
								_, err := runtime.CheckStage(context.Background(), ports.ReconcileStageProject)
								return err
							}},
							{"observe-instance", func() error {
								_, err := runtime.ObserveStage(context.Background(), ports.ReconcileStageInstance)
								return err
							}},
							{"observe-project", func() error {
								_, err := runtime.ObserveStage(context.Background(), ports.ReconcileStageProject)
								return err
							}},
						}
						for _, entry := range entries {
							if err := entry.call(); !errors.Is(err, domain.ErrPlanStale) {
								t.Fatal(entry.name, "unsupported original source admitted", err)
							}
							if mutations.Load() != 0 || len(client.server.PowerCalls()) != 0 || client.calls != 0 || client.volumeCalls != 0 || client.powerWrites != 0 || project.posts != 0 || lock.held {
								t.Fatal(entry.name, "unsupported source reached owning effects", mutations.Load())
							}
							if data, _ := os.ReadFile(client.trace); len(data) != 0 {
								t.Fatal(entry.name, "unsupported source dispatched a portion", string(data))
							}
							if _, err := os.Stat(filepath.Join(home, "old-whole")); !errors.Is(err, os.ErrNotExist) {
								t.Fatal(entry.name, "unsupported whole body dispatched", err)
							}
						}
						return
					}
					err = runtime.ApplyStage(ctx, ports.ReconcileStageInstance)
					trace, _ := os.ReadFile(client.trace)
					events := string(trace)
					success := mode == "success"
					if (err == nil) != success {
						t.Fatal("instance start outcome", err, events, client.diagnostics.String())
					}
					if lock.held || client.calls != 0 || client.volumeCalls != 0 {
						t.Fatal("reacquired lock or recreated original result", events)
					}
					powers := client.server.PowerCalls()
					preStartFailure := mode == "boundary-failure" || mode == "foreign-volume" || mode == "replacement"
					if preStartFailure {
						if mode == "boundary-failure" && !strings.Contains(events, "boundary\n") {
							t.Fatal("boundary case did not reach its boundary", err, events, client.diagnostics.String())
						}
						if mode == "foreign-volume" && !strings.Contains(client.diagnostics.String(), "original owner SRV volume identity differs") {
							t.Fatal("token case failed before its original check", err, events, client.diagnostics.String())
						}
						if mode == "replacement" {
							i, _ := client.Instance(context.Background(), "subyard", "yard")
							if i.LocalConfig["volatile.uuid"] != "replacement" || !errors.Is(err, domain.ErrPlanStale) {
								t.Fatal("replacement case missed its original UUID check", err, events, client.diagnostics.String())
							}
						}
						if len(powers) != 0 || strings.Contains(events, "after-start") || client.probes != 0 || client.cpuCalls != 0 {
							t.Fatal("unproved before-start portion reached power/tail", events, powers)
						}
						return
					}
					if mode == "cpu-failure" {
						if len(powers) != 2 || powers[1].Action != "stop" || !powers[1].Force || client.cpuCalls != 1 || client.probes != 0 || strings.Contains(events, "after-start") {
							t.Fatal("CPU failure compensation changed", events, powers)
						}
						return
					}
					instance, readErr := client.Instance(context.Background(), "subyard", "yard")
					if readErr != nil || len(powers) != 1 || powers[0].Action != "start" || powers[0].Force || instance.Config["user.subyard.desired_power"] != "stopped" || instance.Config["user.subyard.initialized"] != "false" {
						t.Fatal("temporary start committed intent or changed target", readErr, instance, powers)
					}
					before := strings.Index(events, "boundary\n")
					start := strings.Index(events, "start\n")
					after := strings.Index(events, "after-start\n")
					if before < 0 || start <= before || after <= start {
						t.Fatal("storage/start/tail ordering changed", events)
					}
					if kind == domain.YardVM {
						cpu2 := strings.Index(events, "cpu2-generation2\n")
						mount := strings.Index(events, "mount\n")
						pin := strings.Index(events, "pin\n")
						report := strings.Index(events, "reporting\n")
						if client.cpuCalls != 2 || client.probes != 2 || cpu2 <= start || after <= cpu2 || mount <= after || pin <= mount || report <= pin {
							t.Fatal("replacement QEMU or VM tail lost its boundary", events)
						}
					} else if client.cpuCalls != 0 || client.probes != 1 {
						t.Fatal("container skipped fixed agent or acquired CPU", events)
					}
					// The materialized original source stays valid; no ambient source supplies
					// the chosen portions after capture.
					if sourceRoot == runtime.RepositoryRoot {
						t.Fatal("source was not retained")
					}
					if err := runtime.checkOwnerInstanceStartSource(); err != nil {
						t.Fatal("portion changed retained source", err)
					}
				})
			}
		}
	}
}

func ownerInstanceRetainedBodyFixture(t *testing.T, kind domain.YardKind, created bool, mode string) (Runtime, *ownerInstanceRuntimeFixture, *ownerInstanceStartLock, string) {
	t.Helper()
	runtime, incus := appArmorRuntime(t, "", false)
	root := runtime.RepositoryRoot
	home := testkit.TempDir(t)
	repository, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, leaf := range []string{"scripts/03-create-subyard.sh", "scripts/guest-disk-tmp.sh", "scripts/lib/runtime.sh", "scripts/lib/engine-context.sh", "scripts/lib/ui.sh", "scripts/lib-power.sh", "scripts/lib/host.sh", "scripts/lib/download.sh", "scripts/lib/instance-create.sh", "scripts/lib-vm-storage.sh", "scripts/lib-vm-page-reporting.sh"} {
		body, err := os.ReadFile(filepath.Join(repository, leaf))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, leaf)), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, leaf), body, 0700)
	}
	scriptPath := filepath.Join(root, "scripts/03-create-subyard.sh")
	body, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if mode == "old-script" {
		for _, line := range []string{"  --owner-before-start) owner_phase=before-start ;;\n", "  --owner-after-start) owner_phase=after-start ;;\n", "  --owner-*) die 'unknown original owner portion' ;;\n"} {
			body = bytes.ReplaceAll(body, []byte(line), nil)
		}
		gate := []byte(`if [ "$owner_phase" = whole ] || [ "$owner_phase" = before-instance ]; then`)
		body = bytes.Replace(body, gate, append(append([]byte(nil), gate...), []byte("\nif [ \"$owner_phase\" = whole ]; then printf old-whole >\"$SUBYARD_HOME/old-whole\"; exit 0; fi")...), 1)
	}
	// These earlier portions have their separate causal/ordinary contract tests.
	prefix := `#!/usr/bin/env bash
case "${1:-}" in
 --check-page-reporting) printf 'preflight\n' >>"$TRACE"; exit 0 ;;
 --owner-before-instance) [ -f /proc/self/fd/3 ]; printf 'before-instance\n' >>"$TRACE"; exit 0 ;;
 --owner-before-srv) [ -f /proc/self/fd/3 ]; printf 'before-srv\n' >>"$TRACE"; exit 0 ;;
 --owner-after-start) printf 'after-start\n' >>"$TRACE" ;;
esac
`
	testkit.WriteFile(t, scriptPath, append([]byte(prefix), body...), 0700)
	files := map[string]string{
		"scripts/reconcile-integrations.sh": "#!/bin/sh\nexit 0\n",
		"config/projects-changed.sh":        "#!/bin/sh\nexit 0\n",
		"scripts/lib-vm-storage.sh": `vm_storage_attach(){ printf 'storage-attach\n' >>"$TRACE"; }
vm_storage_check_device_boundary(){ [ "$1" = false ]; printf 'boundary\n' >>"$TRACE"; [ "$MODE" != boundary-failure ]; }
vm_storage_mount(){ printf 'mount\n' >>"$TRACE"; }
`,
		"scripts/lib-vm-page-reporting.sh": `vm_page_reporting_required(){ [ "$YARD_KIND" = vm ]; }
vm_page_reporting_prepare_guest(){ printf 'reporting\n' >>"$TRACE"; [ "$MODE" != tail-failure ]; }
`,
	}
	for leaf, value := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, leaf)), 0700); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(leaf, "scripts/lib-vm-") {
			original, err := os.ReadFile(filepath.Join(root, leaf))
			if err != nil {
				t.Fatal(err)
			}
			value = string(original) + "\n" + value
		}
		testkit.WriteFile(t, filepath.Join(root, leaf), []byte(value), 0700)
	}
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\n[ \"$*\" = 'show incus.service -p Environment --value' ]\n"), 0700)
	testkit.WriteFile(t, filepath.Join(bin, "incus"), []byte(`#!/bin/bash
set -eu
case "$*" in
 'config get yard volatile.uuid --project subyard') printf 'original-created-uuid\n' ;;
 'config get yard user.subyard.owner_create --project subyard') printf 'original-instance-token\n' ;;
 'storage volume get default yard-srv user.subyard.owner_create --project subyard') if [ "$MODE" = foreign-volume ];then printf foreign;else printf original-volume-token;fi ;;
 'storage volume show default yard-srv --project subyard') exit 0 ;;
 'config device list yard --project subyard') printf 'srv\neth0\n' ;;
 'exec yard --project subyard -T -- sh -s -- apply')
   cat >/dev/null; printf 'tmp-apply\n' >>"$TRACE"
   if [ "$MODE" = fresh-source-drift ]; then chmod u+w "$SUBYARD_REPOSITORY_ROOT/scripts/03-create-subyard.sh"; printf '\n# changed retained source\n' >>"$SUBYARD_REPOSITORY_ROOT/scripts/03-create-subyard.sh"; fi ;;
 'exec yard --project subyard -T -- sh -s -- check')
   cat >/dev/null; printf 'tmp-check\n' >>"$TRACE"
   [ "$MODE" != fresh-check-failure ] || exit 1
   if [ "$MODE" != fresh-ready ] && [ ! -f "$SUBYARD_HOME/tmp-booted" ]; then exit 10; fi ;;
 'exec yard --project subyard -T -- sh -s -- wait-boot') cat >/dev/null; printf 'tmp-boot\n' >>"$TRACE"; touch "$SUBYARD_HOME/tmp-booted" ;;
 'config device get yard srv pool --project subyard') printf default ;;
 'config device get yard srv source --project subyard') printf yard-srv ;;
 'config device get yard srv path --project subyard') printf /srv ;;
 'query /1.0/instances/yard?project=subyard') printf '{"devices":{"eth0":{"type":"nic","network":"incusbr0"}}}\n' ;;
 'config device set yard eth0 ipv4.address=10.80.0.10 --project subyard') printf 'pin\n' >>"$TRACE" ;;
 exec\ yard\ --project\ subyard*) printf '2: eth0 inet 10.80.0.10/24 scope global eth0\n' ;;
 *) printf 'unexpected shell Incus call: %s\n' "$*" >&2;exit 92 ;;
esac
`), 0700)
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	socket, _ := ownerPublicationSnapshotSocket(t, server, mode)
	client := &ownerInstanceRuntimeFixture{Incus: incus, sdk: incusclient.New(socket), server: server, kind: kind, mode: mode, trace: filepath.Join(home, "trace"), generation: 1}
	cfg := map[string]string{"volatile.uuid": "original-created-uuid", "user.subyard.owner_create": "original-instance-token", "user.subyard.managed": "true", "user.subyard.initialized": "false", "user.subyard.desired_power": "stopped", "user.subyard.name": "default", "user.subyard.bridge": "incusbr0", "boot.autostart": "false"}
	if kind == domain.YardVM {
		client.weight = "250"
		cfg["user.subyard.vm_cpu_weight"] = "250"
	}
	server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "type": ownerPublicationAPIKind(kind), "status": "Stopped", "config": cfg, "expanded_config": maps.Clone(cfg)})
	incus.ServerInfo.Version = "6.0.6"
	runtime.Yard.YardKind = kind
	runtime.Yard.Paths.DataHome = home
	runtime.Yard.Paths.ConfigHome = filepath.Join(home, "config")
	runtime.Incus = client
	runtime.ConfigWriter = client
	runtime.Stderr = &client.diagnostics
	runtime.Environment = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home, "SUBYARD_OPERATOR_HOME=" + home, "SUBYARD_CONFIG_DIR=" + filepath.Join(root, "config"), "SUBYARD_REPOSITORY_ROOT=" + root, "SUBYARD_CONFIG_HOME=" + runtime.Yard.Paths.ConfigHome, "SUBYARD_HOME=" + home, "STORAGE_PATH=" + filepath.Join(home, "storage"), "HOST_BASE=" + filepath.Join(home, "host"), "RESTRICTED_DISK_PATHS=" + filepath.Join(home, "host"), "ACCESS_KIND=local", "YARD_KIND=" + string(kind), "YARD_INSTANCE_NAME=yard", "INCUS_PROJECT=subyard", "INCUS_BRIDGE=incusbr0", "SSH_HOST=yard", "DEV_USER=dev", "DEV_UID=1000", "DEV_SUDO=0", "FORWARD_SSH_AGENT=0", "NESTED_E2E_VMS=0", "SUBYARD_PROFILE_GUEST_ENVIRONMENT_HOOKS=", "SUBYARD_ENGINE_CONTEXT=1", "SUBYARD_ENGINE_CONTEXT_SCHEMA=1", "SUBYARD_CONFIG_LOADED=1", "SUBYARD_POWER_DESIRED=stopped", "TRACE=" + client.trace, "MODE=" + mode, "VM_CPU_WEIGHT=" + client.weight, "SUBYARD_NO_AUDIT=1", "SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID=original-created-uuid"}
	if kind == domain.YardVM {
		runtime.Environment = append(runtime.Environment, "SRV_VOLUME_TYPE=block", "VM_FREE_PAGE_REPORTING=1")
	} else {
		runtime.Environment = append(runtime.Environment, "SRV_VOLUME_TYPE=filesystem")
	}
	volume := ports.PreparedOwnerVolume{Project: "subyard", Instance: "yard", Volume: "yard-srv", Scope: "current", InstanceUUID: "original-created-uuid", Absent: true}
	runtime.ownerVolume = &ownerVolumeExecution{original: volume}
	if created {
		runtime.ownerVolume.original.InstanceUUID = ""
		runtime.ownerInstance = &ownerInstanceExecution{original: ports.PreparedOwnerInstance{Project: "subyard", Instance: "yard", Scope: "current"}, created: &ports.NativeOwnerCreated{InstanceUUID: "original-created-uuid", Intent: ports.NativeOwnerCreateIntent{Token: "original-instance-token"}}}
	}
	if strings.HasPrefix(mode, "fresh-") {
		server.ReplaceInstances(nil)
		delete(incus.Instances, "subyard/yard")
		runtime.ownerInstance.created = nil
		policy := ports.PreparedOwnerImagePolicy{Version: 1, Primary: []byte("images:primary"), Fallback: []byte("images:fallback"), Kind: "virtual-machine"}
		runtime.ownerInstance.original.Policy = &policy
		runtime.ownerInstance.original.Rendered = []byte("images:primary\x00virtual-machine\x00")
		path := filepath.Join(root, "scripts/lib/instance-create.sh")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, []byte("\nsubyard_instance_config_render(){ printf 'images:primary\\0virtual-machine\\0'; }\nsubyard_instance_approved_config_render(){ subyard_instance_config_render; }\n")...)
		testkit.WriteFile(t, path, body, 0700)
	}
	runtime.ownerVolume.created = &ports.NativeOwnerCreated{Intent: ports.NativeOwnerCreateIntent{Token: "original-volume-token"}}
	lock := &ownerInstanceStartLock{}
	runtime.NetworkPolicy = &yardnetwork.Service{Host: ownerPublicationPolicyHost{}, Lock: lock, Guard: func(context.Context) error {
		if !lock.held {
			return errors.New("host guard escaped original lock")
		}
		return nil
	}}
	return runtime, client, lock, root
}

// Fresh storage setup uses the retained 03 portions and real typed SDK power
// calls. Guest exec and CPU host operations are fixtures, not physical evidence.
func TestOwnerFreshInstanceInitialStorageBoot(t *testing.T) {
	for _, mode := range []string{"fresh-ready", "fresh-restart", "fresh-check-failure", "fresh-source-drift", "fresh-restart-cpu-failure"} {
		t.Run(mode, func(t *testing.T) {
			runtime, client, lock, _ := ownerInstanceRetainedBodyFixture(t, domain.YardVM, true, mode)
			_, unlock, err := uninstall.OwnerLock(context.Background(), runtime.Yard.Paths.DataHome, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			sources, err := runtime.PrepareOwnerRuntimeSources(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer sources.FinishOwnerSources(nil)
			runtime.ownerInstance.rendererRuntime = runtime
			runtime, err = runtime.OwnerSourceRuntime(context.Background(), sources)
			if err != nil {
				t.Fatal(err)
			}
			rendered, renderErr := runtime.RenderOwnerInstanceConfig(context.Background(), runtime.ownerInstance.sources, []byte(`[]`))
			if renderErr != nil || !bytes.Equal(rendered, runtime.ownerInstance.original.Rendered) {
				t.Fatalf("fixture render %q expected %q: %v", rendered, runtime.ownerInstance.original.Rendered, renderErr)
			}
			ctx, cancel := context.WithTimeout(ports.WithNativeOperationObserver(context.Background(), custodyTransportObserver{}), 8*time.Second)
			defer cancel()
			err = runtime.ApplyStage(ctx, ports.ReconcileStageInstance)
			raw, _ := os.ReadFile(client.trace)
			events := string(raw)
			powers := client.server.PowerCalls()
			success := mode == "fresh-ready" || mode == "fresh-restart"
			if (err == nil) != success || client.calls != 1 || lock.held {
				t.Fatal("fresh outcome", err, events, client.diagnostics.String())
			}
			apply := strings.Index(events, "tmp-apply\n")
			cpu := strings.Index(events, "cpu2-generation2\n")
			if apply <= cpu || cpu < 0 {
				t.Fatal("guest storage preceded initial CPU/agent verification", events)
			}
			if !success {
				if strings.Contains(events, "after-start\n") {
					t.Fatal("failed boot reached provisioning tail", events)
				}
				if mode != "fresh-restart-cpu-failure" && len(powers) != 1 {
					t.Fatal("unproved storage result restarted guest", powers, events)
				}
				if mode == "fresh-source-drift" && !errors.Is(err, domain.ErrPlanStale) {
					t.Fatal("source drift not refused", err)
				}
				if mode == "fresh-restart-cpu-failure" && (len(powers) != 4 || powers[3].Action != "stop" || !powers[3].Force || strings.Contains(events, "tmp-boot\n")) {
					t.Fatal("restart CPU failure escaped compensation", powers, events)
				}
				return
			}
			expected := 1
			if mode == "fresh-restart" {
				expected = 3
			}
			if len(powers) != expected {
				t.Fatal("fresh power count", powers, events)
			}
			if mode == "fresh-restart" {
				stop := strings.Index(events, "stop\n")
				boot := strings.Index(events, "tmp-boot\n")
				after := strings.Index(events, "after-start\n")
				if powers[1].Action != "stop" || powers[1].Force || powers[2].Action != "start" || stop <= apply || boot <= stop || after <= boot || client.cpuCalls != 4 {
					t.Fatal("fresh restart ordering", powers, events)
				}
			} else if strings.Contains(events, "tmp-boot\n") {
				t.Fatal("ready disk restarted", events)
			}
		})
	}
}

func TestOwnerComponentResultRequiresForegroundDrain(t *testing.T) {
	for _, mode := range []string{"ordinary", "native", "probe"} {
		t.Run(mode, func(t *testing.T) {
			native := mode != "ordinary"
			runtime, _ := appArmorRuntime(t, "", false)
			home := testkit.TempDir(t)
			runtime.Yard.Paths.DataHome = home
			runtime.Environment = append(runtime.Environment, "SUBYARD_HOME="+home)
			_, unlock, err := uninstall.OwnerLock(context.Background(), home, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			script := filepath.Join(runtime.RepositoryRoot, "scripts/result.sh")
			if err := os.MkdirAll(filepath.Dir(script), 0700); err != nil {
				t.Fatal(err)
			}
			pidFile := filepath.Join(home, "child")
			testkit.WriteFile(t, script, []byte("#!/bin/sh\n/bin/sleep 30 </dev/null >/dev/null 2>&1 &\nprintf '%s' \"$!\" >\"$1\"\nprintf 'restart\\n'\n"), 0700)
			if mode == "probe" {
				raw, err := os.ReadFile(script)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, script, append(raw, []byte("exit 1\n")...), 0700)
			}
			var output bytes.Buffer
			ctx := context.Background()
			if native {
				ctx = ports.WithNativeOperationObserver(ctx, custodyTransportObserver{})
			}
			if mode == "probe" {
				ctx = readOnlyCommand(ctx)
			}
			err = runtime.runPathEnvironment(ctx, &output, runtime.Environment, script, pidFile)
			raw, readErr := os.ReadFile(pidFile)
			pid, parseErr := strconv.Atoi(string(raw))
			if readErr != nil || parseErr != nil {
				t.Fatal("child identity missing", readErr, parseErr)
			}
			// The shell has exited; PID1 owns reaping its orphan. Kill this exact fixture
			// child and wait until it no longer runs before releasing the owner barrier.
			defer func() {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) {
					state, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
					if os.IsNotExist(e) || e == nil && strings.Contains(string(state), ") Z ") {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Error("fixture child did not drain")
			}()
			if syscall.Kill(pid, 0) != nil {
				t.Fatal("background child was not alive")
			}
			if mode == "probe" {
				if _, probeErr := probeConverged(err); probeErr == nil {
					t.Fatal("undrained probe was swallowed as drift", err)
				}
			}
			if output.String() != "restart\n" || native && (err == nil || !strings.Contains(err.Error(), "foreground is not drained")) || !native && err != nil {
				t.Fatal("foreground contract", native, err, output.String())
			}
		})
	}
}
