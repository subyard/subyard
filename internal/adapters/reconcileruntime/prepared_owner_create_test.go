package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/uninstall"
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
	calls, checks         int
	failCreate, failCheck bool
	trace, mode           string
	volumeCalls           int
	powerWrites           int
	guardOrder            []string
}

func (f *ownerInstanceRuntimeFixture) ApplyPreparedOwnerPolicyInstance(ctx context.Context, p ports.PreparedOwnerInstance, _ []string, _ string) (*ports.NativeOwnerCreated, ports.PreparedOwnerInstance, error) {
	f.event("instance-post")
	f.calls++
	if f.failCreate {
		return nil, p, errors.New("original response unavailable")
	}
	instance := ports.InstanceInfo{Name: p.Instance, Project: p.Project, Status: "Stopped", Config: map[string]string{"volatile.uuid": "original-created-uuid", "user.subyard.managed": "true", "user.subyard.name": "default", "user.subyard.bridge": "incusbr0", "user.subyard.desired_power": "running", "user.subyard.initialized": "false", "boot.autostart": "false"}, LocalConfig: map[string]string{}}
	f.Instances[p.Project+"/"+p.Instance] = instance
	return &ports.NativeOwnerCreated{InstanceUUID: "original-created-uuid", Ref: ports.NativeOwnerCreateRef{Domain: "operation", Index: 1}}, p, nil
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

func (f *ownerInstanceRuntimeFixture) CheckOwnerCreatedInstance(context.Context, ports.PreparedOwnerInstance, ports.NativeOwnerCreated) error {
	f.checks++
	f.guardOrder = append(f.guardOrder, "instance")
	if f.failCheck {
		return domain.ErrPlanStale
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
	if created == nil {
		return domain.ErrPlanStale
	}
	instance, err := f.Instance(context.Background(), p.Project, p.Instance)
	if err != nil || instance.Config["volatile.uuid"] != created.InstanceUUID {
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
			trace := filepath.Join(root, "trace")
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
			marker := filepath.Join(root, "dispatched")
			runtime.Environment = append(runtime.Environment, "SUBYARD_HOME="+root, "MARKER="+marker, "TRACE="+trace, "MODE="+mode,
				"SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID=forged-environment-value")
			if err := os.Mkdir(filepath.Join(root, "scripts"), 0700); err != nil {
				t.Fatal(err)
			}
			script := `#!/bin/sh
set -eu
[ -f /proc/self/fd/3 ]
case "$1" in
 --owner-before-instance) [ -z "$SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID" ]; printf 'before-instance\n' >>"$TRACE"; [ "$MODE" != pre-command-failure ] ;;
 --owner-before-srv|--owner-after-srv)
   [ "$SUBYARD_PREPARED_OWNER_INSTANCE_UUID" = original-created-uuid ]
   if [ "$MODE" = retained-results ]; then
     [ -z "$SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID" ]
   else [ "$SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID" = original-created-uuid ]; fi
   if [ "$1" = --owner-before-srv ]; then
     printf 'before-srv\n' >>"$TRACE"; [ "$MODE" != before-srv-failure ]
   else
     [ "$SUBYARD_PREPARED_OWNER_VOLUME_TOKEN" = original-volume-token ]; printf 'after-srv\n' >>"$TRACE"; printf original >"$MARKER"
   fi ;;
 *) exit 91 ;;
esac
`
			testkit.WriteFile(t, filepath.Join(root, "scripts/03-create-subyard.sh"), []byte(script), 0700)
			if runtime.ownerInstance != nil {
				policy := ports.PreparedOwnerImagePolicy{Version: 1, Primary: []byte("images:primary"), Fallback: []byte("images:fallback"), Kind: "virtual-machine"}
				runtime.ownerInstance.original.Policy = &policy
				runtime.ownerInstance.original.Rendered = []byte("images:primary\x00virtual-machine\x00")
				sources := &PreparedInstalledRuntime{owner: "owner-native-runtime", runtime: runtime.configRuntimeBinding(), resources: runtime.installedResourceBinding(), ownerInputs: []ownerSourceInputBinding{}, files: map[string][]byte{}}
				d := &materializedIntegrations{}
				for leaf, body := range map[string]string{"scripts/lib-power.sh": "# retained test source\n", "scripts/lib/instance-create.sh": "subyard_instance_config_render(){ printf 'images:primary\\0virtual-machine\\0'; }\nsubyard_instance_approved_config_render(){ subyard_instance_config_render; }\n"} {
					key := materializedBlobPath([]byte(body))
					sources.files[key] = []byte(body)
					d.Sources = append(d.Sources, materializedExecutionSource{Layout: leaf, Payload: key})
				}
				if err := writeMaterializedMetadata(sources, materializedRecipe{Version: 1, Integrations: d}); err != nil {
					t.Fatal(err)
				}
				runtime.ownerInstance.sources = sources
			}

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
			ctx := ports.WithNativeOperationObserver(context.Background(), custodyTransportObserver{})
			err = runtime.ApplyStage(ctx, ports.ReconcileStageInstance)
			_, statErr := os.Stat(marker)
			events, _ := os.ReadFile(trace)
			if mode == "retained-results" {
				if err != nil || statErr != nil || client.calls != 0 || client.volumeCalls != 0 || string(events) != "before-instance\nbefore-srv\nattach\nafter-srv\n" {
					t.Fatal("retained results did not prevent repeat creates", err, statErr, string(events))
				}
			} else if mode == "success" {
				if err != nil || statErr != nil || client.calls != 1 || client.checks != 1 || client.powerWrites != 1 || string(events) != "before-instance\ninstance-post\nbefore-srv\nvolume-post\nattach\nafter-srv\n" {
					t.Fatal("original source order/result/OFD chain changed", err, statErr, string(events))
				}
			} else {
				if err == nil || !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("unknown/failed part dispatched next portion", err, statErr)
				}
				if (mode == "missing-original" || mode == "foreign-present" || mode == "pre-command-failure") && client.calls != 0 {
					t.Fatal("pre-instance refusal submitted create")
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

func (f *ownerInstanceRuntimeFixture) SetOwnerCreatedPower(_ context.Context, _ ports.PreparedOwnerInstance, _ ports.NativeOwnerCreated, _ string, _ bool) error {
	if f.failCheck {
		return domain.ErrPlanStale
	}
	f.powerWrites++
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
