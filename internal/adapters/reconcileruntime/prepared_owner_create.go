package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"github.com/Subyard/Subyard/internal/application"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

const ownerProjectRenderer = "scripts/lib/project-config.sh"

// RenderOwnerProjectConfig runs only the retained pure renderer bytes. It does
// not materialize an execution tree, query Incus, select a yard or mutate it.
func (runtime Runtime) RenderOwnerProjectConfig(ctx context.Context, original *PreparedInstalledRuntime) ([]byte, error) {
	if original == nil || original.closed.Load() || original.owner != "owner-native-runtime" || original.runtime != runtime.configRuntimeBinding() || original.resources != runtime.installedResourceBinding() || original.ownerInputs == nil {
		return nil, domain.ErrPlanStale
	}
	helper, err := original.ownerRendererBytes(ownerProjectRenderer)
	if err != nil {
		return nil, err
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Privileged shell mode disables inherited BASH_ENV/functions; this probe
	// executes only the fixed captured renderer, without changing process UID.
	command := exec.CommandContext(probe, "bash", "--noprofile", "--norc", "-peu", "-s")
	command.Env = append([]string(nil), runtime.Environment...)
	for index := range command.Env {
		command.Env[index] = runtime.bindingPath(command.Env[index])
	}
	command.Stdin = io.MultiReader(bytes.NewReader(helper), bytes.NewBufferString("\nsubyard_project_config_render\n"))
	output := &profileRuntimeBoundedBuffer{limit: 32 << 20}
	command.Stdout, command.Stderr = output, io.Discard
	if err := command.Run(); err != nil || output.exceeded {
		return nil, errors.New("original owner project rendering unavailable")
	}
	return bytes.Clone(output.Bytes()), nil
}

// ownerRendererBytes resolves only a fixed renderer leaf in the original
// approved source table and checks its content address before execution.
func (original *PreparedInstalledRuntime) ownerRendererBytes(layout string) ([]byte, error) {
	metadata, _, err := decodeMaterializedMetadata(original)
	if err != nil || metadata.Integrations == nil {
		return nil, domain.ErrPlanStale
	}
	for _, source := range metadata.Integrations.Sources {
		if source.Layout == layout {
			helper := original.files[source.Payload]
			if len(helper) == 0 || len(helper) > 32<<20 || source.Payload != materializedBlobPath(helper) {
				return nil, domain.ErrPlanStale
			}
			return helper, nil
		}
	}
	return nil, errors.New("original owner renderer unavailable")
}

// RenderOwnerInstanceConfig shares the original 03 image decision and flags.
// Its only input is the bounded original SDK image catalog on fixed fd3.
func (runtime Runtime) RenderOwnerInstanceConfig(ctx context.Context, original *PreparedInstalledRuntime, catalog []byte) ([]byte, error) {
	return runtime.renderOwnerInstanceConfig(ctx, original, catalog, nil)
}
func (runtime Runtime) renderOwnerInstanceConfig(ctx context.Context, original *PreparedInstalledRuntime, catalog, selected []byte) ([]byte, error) {
	if original == nil || original.closed.Load() || original.owner != "owner-native-runtime" || original.runtime != runtime.configRuntimeBinding() || original.resources != runtime.installedResourceBinding() || original.ownerInputs == nil || len(catalog) > 32<<20 {
		return nil, domain.ErrPlanStale
	}
	power, err := original.ownerRendererBytes("scripts/lib-power.sh")
	if err != nil {
		return nil, err
	}
	helper, err := original.ownerRendererBytes("scripts/lib/instance-create.sh")
	if err != nil {
		return nil, err
	}
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer read.Close()
	copied := make(chan error, 1)
	go func() { _, err := write.Write(catalog); copied <- errors.Join(err, write.Close()) }()
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(probe, "bash", "--noprofile", "--norc", "-peu", "-s")
	command.Env = append([]string(nil), runtime.Environment...)
	for i := range command.Env {
		command.Env[i] = runtime.bindingPath(command.Env[i])
	}
	command.Env = ownerRenderEnvironment(command.Env, "SUBYARD_POWER_DESIRED", string(application.InitialPower(runtime.Yard)))
	command.ExtraFiles = []*os.File{read}
	call := "\nwarn() { :; }\nsubyard_instance_config_render\n"
	if selected != nil {
		command.Args = append(command.Args, "--", string(selected))
		call = "\nsubyard_instance_approved_config_render \"$1\"\n"
	}
	command.Stdin = io.MultiReader(bytes.NewBufferString("unset SUBYARD_LIBPOWER_SOURCED\n"), bytes.NewReader(power), bytes.NewReader(helper), bytes.NewBufferString(call))
	output := &profileRuntimeBoundedBuffer{limit: 32 << 20}
	command.Stdout, command.Stderr = output, io.Discard
	runErr := command.Run()
	_ = read.Close()
	copyErr := <-copied
	if runErr != nil || copyErr != nil || output.exceeded {
		return nil, errors.New("original owner instance rendering unavailable")
	}
	return bytes.Clone(output.Bytes()), nil
}
func ownerRenderEnvironment(environment []string, key, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, key+"=") {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}

type ownerInstanceCreator interface {
	CheckOwnerCreatedInstance(context.Context, ports.PreparedOwnerInstance, ports.NativeOwnerCreated) error
}
type ownerInstanceExecution struct {
	original    ports.PreparedOwnerInstance
	created     *ports.NativeOwnerCreated
	resolved    *ports.PreparedOwnerInstance
	sources     *PreparedInstalledRuntime
	environment []string
	home        string
}

type ownerImagePolicyClient interface {
	OwnerCreateServer(context.Context) (string, error)
	CaptureOwnerImagePolicy(context.Context, string, string, domain.YardKind, []string, string) (ports.PreparedOwnerImagePolicy, error)
	CheckOwnerImagePolicy(context.Context, ports.PreparedOwnerImagePolicy, []string, string) error
	ReadOwnerPolicyImageCatalog(context.Context, ports.PreparedOwnerImagePolicy, []string, string) ([]byte, error)
}
type ownerPolicyInstanceCreator interface {
	ApplyPreparedOwnerPolicyInstance(context.Context, ports.PreparedOwnerInstance, []string, string) (*ports.NativeOwnerCreated, ports.PreparedOwnerInstance, error)
}

// PrepareOwnerInstancePolicy freezes only the original 03 decision inputs. The
// ordinary component resolves its catalog after acceptance, before its one POST.
func (runtime Runtime) PrepareOwnerInstancePolicy(ctx context.Context, sources *PreparedInstalledRuntime, scope string) (*ports.PreparedOwnerInstance, error) {
	client, ok := runtime.Incus.(ownerImagePolicyClient)
	if !ok || scope != "current" && scope != "legacy" && scope != "recreate" {
		return nil, domain.ErrPlanStale
	}
	server, err := client.OwnerCreateServer(ctx)
	if err != nil {
		return nil, err
	}
	env := ownerRenderEnvironment(runtime.Environment, "INCUS_PROJECT", runtime.Yard.IncusProject)
	primary := runtime.environmentDefault("YARD_IMAGE", "images:debian/13")
	fallback := runtime.environmentDefault("YARD_IMAGE_FALLBACK", "images:ubuntu/24.04")
	policy, err := client.CaptureOwnerImagePolicy(ctx, primary, fallback, runtime.Yard.YardKind, env, runtime.Yard.Paths.OperatorHome)
	if err != nil {
		return nil, err
	}
	rendered, err := runtime.renderOwnerInstanceConfig(ctx, sources, nil, []byte(primary))
	if err != nil {
		return nil, err
	}
	return &ports.PreparedOwnerInstance{Project: runtime.Yard.IncusProject, Instance: runtime.Yard.YardInstanceName, Runtime: runtime.configRuntimeBinding(), Source: sources.Binding(), Server: server, Scope: scope, Rendered: rendered, Policy: &policy}, nil
}

// WithOwnerInstance attaches only an original capsule authenticated against its
// checked source handle. It does not perform image selection or live planning.
func (runtime Runtime) WithOwnerInstance(ctx context.Context, sources *PreparedInstalledRuntime, p ports.PreparedOwnerInstance) (Runtime, error) {
	client, ok := runtime.Incus.(ownerImagePolicyClient)
	if !ok || sources == nil || sources.closed.Load() || sources.owner != "owner-native-runtime" || p.Project != runtime.Yard.IncusProject || p.Instance != runtime.Yard.YardInstanceName || p.Runtime != runtime.configRuntimeBinding() || p.Source != sources.Binding() || p.Scope != "current" && p.Scope != "legacy" && p.Scope != "recreate" || len(p.Rendered) == 0 || len(p.Rendered) > 32<<20 || p.Rendered[len(p.Rendered)-1] != 0 {
		return Runtime{}, domain.ErrPlanStale
	}
	if p.Policy == nil {
		return Runtime{}, domain.ErrPlanStale
	}
	if !reflect.DeepEqual(p.Image, ports.PreparedOwnerImageSource{}) || p.Policy.Version != 1 {
		return Runtime{}, domain.ErrPlanStale
	}
	env := ownerRenderEnvironment(runtime.Environment, "INCUS_PROJECT", runtime.Yard.IncusProject)
	if err := client.CheckOwnerImagePolicy(ctx, *p.Policy, env, runtime.Yard.Paths.OperatorHome); err != nil {
		return Runtime{}, err
	}
	if string(p.Policy.Primary) != runtime.environmentDefault("YARD_IMAGE", "images:debian/13") || string(p.Policy.Fallback) != runtime.environmentDefault("YARD_IMAGE_FALLBACK", "images:ubuntu/24.04") {
		return Runtime{}, domain.ErrPlanStale
	}
	selected := p.Policy.Primary
	rendered, renderErr := runtime.renderOwnerInstanceConfig(ctx, sources, nil, selected)
	expectedKind := "container"
	if runtime.Yard.YardKind == domain.YardVM {
		expectedKind = "virtual-machine"
	}
	if renderErr != nil || !bytes.Equal(rendered, p.Rendered) || p.Policy.Kind != expectedKind {
		return Runtime{}, domain.ErrPlanStale
	}
	server, err := client.OwnerCreateServer(ctx)
	if err != nil || p.Server == "" || p.Server != server {
		return Runtime{}, domain.ErrPlanStale
	}
	runtime.ownerInstance = &ownerInstanceExecution{original: p, sources: sources, environment: ownerRenderEnvironment(runtime.Environment, "INCUS_PROJECT", runtime.Yard.IncusProject), home: runtime.Yard.Paths.OperatorHome}
	return runtime, nil
}

func (runtime Runtime) RenderOwnerVolumeConfig(ctx context.Context, original *PreparedInstalledRuntime) ([]byte, error) {
	if original == nil || original.closed.Load() || original.owner != "owner-native-runtime" || original.runtime != runtime.configRuntimeBinding() || original.resources != runtime.installedResourceBinding() {
		return nil, domain.ErrPlanStale
	}
	helper, err := original.ownerRendererBytes("scripts/lib-vm-storage.sh")
	if err != nil {
		return nil, err
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(probe, "bash", "--noprofile", "--norc", "-peu", "-s")
	command.Env = append([]string(nil), runtime.Environment...)
	pool, volume := runtime.volumeNames()
	command.Env = ownerRenderEnvironment(command.Env, "SRV_POOL", pool)
	command.Env = ownerRenderEnvironment(command.Env, "SRV_VOLUME", volume)
	command.Stdin = io.MultiReader(bytes.NewReader(helper), bytes.NewBufferString("\nsubyard_srv_config_render\n"))
	output := &profileRuntimeBoundedBuffer{limit: 32 << 20}
	command.Stdout, command.Stderr = output, io.Discard
	if err := command.Run(); err != nil || output.exceeded {
		return nil, errors.New("original owner SRV rendering unavailable")
	}
	return bytes.Clone(output.Bytes()), nil
}

type ownerVolumeExecution struct {
	original    ports.PreparedOwnerVolume
	created     *ports.NativeOwnerCreated
	resolved    *ports.PreparedOwnerInstance
	sources     *PreparedInstalledRuntime
	environment []string
	home        string
}

func (runtime Runtime) PrepareOwnerVolume(ctx context.Context, sources *PreparedInstalledRuntime, scope string) (*ports.PreparedOwnerVolume, error) {
	if scope != "current" && scope != "legacy" && scope != "recreate" {
		return nil, domain.ErrPlanStale
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return nil, err
	}
	rendered, err := runtime.RenderOwnerVolumeConfig(ctx, sources)
	if err != nil {
		return nil, err
	}
	pool, volume := runtime.volumeNames()
	p := &ports.PreparedOwnerVolume{Project: runtime.Yard.IncusProject, Instance: runtime.Yard.YardInstanceName, Pool: pool, Volume: volume, Scope: scope, Runtime: runtime.configRuntimeBinding(), Source: sources.Binding(), Rendered: rendered, Absent: !state.VolumeFound}
	if state.InstanceFound {
		p.InstanceUUID = state.Instance.Config["volatile.uuid"]
		if p.InstanceUUID == "" {
			return nil, domain.ErrPlanStale
		}
	}
	reader := runtime.Incus
	if projected, ok := reader.(ownerInitPlanningReader); ok {
		reader = projected.IncusReader
	}
	connection, ok := reader.(interface {
		OwnerCreateServer(context.Context) (string, error)
	})
	if !ok {
		return nil, domain.ErrPlanStale
	}
	p.Server, err = connection.OwnerCreateServer(ctx)
	if err != nil {
		return nil, err
	}
	guard, ok := reader.(interface {
		PrepareOwnerVolumeGuard(context.Context, ports.PreparedOwnerVolume) (ports.PreparedOwnerVolume, error)
	})
	if !ok {
		return nil, domain.ErrPlanStale
	}
	// A genuinely projected future absence retains its owning removal proof;
	// its live absence is checked only after that original removal completes.
	if runtime.ownerInitPlanning && p.Absent {
		return p, nil
	}
	value, err := guard.PrepareOwnerVolumeGuard(ctx, *p)
	if err != nil {
		return nil, err
	}
	return &value, nil
}
func (runtime Runtime) WithOwnerVolume(ctx context.Context, sources *PreparedInstalledRuntime, p ports.PreparedOwnerVolume) (Runtime, error) {
	pool, volume := runtime.volumeNames()
	if sources == nil || sources.closed.Load() || p.Project != runtime.Yard.IncusProject || p.Instance != runtime.Yard.YardInstanceName || p.Pool != pool || p.Volume != volume || p.Runtime != runtime.configRuntimeBinding() || p.Source != sources.Binding() || p.Scope != "current" && p.Scope != "legacy" && p.Scope != "recreate" || len(p.Rendered) == 0 || p.Rendered[len(p.Rendered)-1] != 0 {
		return Runtime{}, domain.ErrPlanStale
	}
	if sources.owner != "owner-native-runtime" || sources.runtime != runtime.configRuntimeBinding() || sources.resources != runtime.installedResourceBinding() || p.Absent && (p.Identity != "" || p.ETag != "") || !p.Absent && (len(p.Identity) != 64 || len(p.ETag) != 64) || p.InstanceUUID != "" && !domain.SafeID(p.InstanceUUID) {
		return Runtime{}, domain.ErrPlanStale
	}
	rendered, err := runtime.RenderOwnerVolumeConfig(ctx, sources)
	if err != nil || !bytes.Equal(rendered, p.Rendered) {
		return Runtime{}, domain.ErrPlanStale
	}
	runtime.ownerVolume = &ownerVolumeExecution{original: p}
	return runtime, nil
}

type ownerVolumeCreator interface {
	CheckOwnerVolumeGuard(context.Context, ports.PreparedOwnerVolume) error
	CheckPreparedOwnerVolumeInstance(context.Context, ports.PreparedOwnerVolume, *ports.PreparedOwnerInstance, *ports.NativeOwnerCreated) error
	ApplyPreparedOwnerVolume(context.Context, ports.PreparedOwnerVolume, *ports.PreparedOwnerInstance, *ports.NativeOwnerCreated) (*ports.NativeOwnerCreated, error)
	AttachPreparedOwnerVolume(context.Context, ports.PreparedOwnerVolume, *ports.PreparedOwnerInstance, *ports.NativeOwnerCreated, *ports.NativeOwnerCreated) error
}

// WithOwnerCreatedResults attaches only authenticated original create results.
// It does not query names to establish authority or select a replacement plan.
func (runtime Runtime) WithOwnerCreatedResults(ctx context.Context, values []ports.NativeOwnerCreated) (Runtime, error) {
	reader, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeOwnerCreateObserver)
	if !ok {
		return Runtime{}, domain.ErrPlanStale
	}
	if runtime.ownerInstance != nil {
		copy := *runtime.ownerInstance
		runtime.ownerInstance = &copy
	}
	if runtime.ownerVolume != nil {
		copy := *runtime.ownerVolume
		runtime.ownerVolume = &copy
	}
	for _, value := range values {
		if value.Intent.Project != runtime.Yard.IncusProject {
			continue
		}
		switch value.Intent.Kind {
		case "instance":
			if runtime.ownerInstance == nil || value.Intent.Scope != runtime.ownerInstance.original.Scope || value.Intent.Resource != runtime.ownerInstance.original.Instance {
				continue
			}
			actual, err := reader.ReadOwnerCreated(value.Ref)
			if err != nil || actual != value || runtime.ownerInstance.created != nil && *runtime.ownerInstance.created != value {
				return Runtime{}, domain.ErrPlanStale
			}
			copy := value
			runtime.ownerInstance.created = &copy
		case "srv-volume":
			if runtime.ownerVolume == nil || value.Intent.Scope != runtime.ownerVolume.original.Scope || value.Intent.Resource != runtime.ownerVolume.original.Volume {
				continue
			}
			actual, err := reader.ReadOwnerCreated(value.Ref)
			if err != nil || actual != value || !runtime.ownerVolume.original.Absent || runtime.ownerVolume.created != nil && *runtime.ownerVolume.created != value {
				return Runtime{}, domain.ErrPlanStale
			}
			copy := value
			runtime.ownerVolume.created = &copy
		}
	}
	// Gather both references before any instance validation: an original attached
	// SRV device is admitted only alongside its authenticated volume authority.
	if runtime.ownerVolume != nil && runtime.ownerVolume.created != nil {
		client, ok := runtime.Incus.(interface {
			CheckOwnerCreatedVolume(context.Context, ports.PreparedOwnerVolume, ports.NativeOwnerCreated) error
		})
		if !ok {
			return Runtime{}, domain.ErrPlanStale
		}
		if err := client.CheckOwnerCreatedVolume(ctx, runtime.ownerVolume.original, *runtime.ownerVolume.created); err != nil {
			return Runtime{}, err
		}
	}
	if runtime.ownerInstance != nil && runtime.ownerInstance.created != nil {
		if err := runtime.checkCreatedOwnerInstance(ctx, runtime.ownerInstance.original, *runtime.ownerInstance.created); err != nil {
			return Runtime{}, err
		}
	}
	return runtime, nil
}
func (runtime Runtime) checkCreatedOwnerInstance(ctx context.Context, p ports.PreparedOwnerInstance, created ports.NativeOwnerCreated) error {
	client, ok := runtime.Incus.(ownerInstanceCreator)
	if !ok {
		return domain.ErrPlanStale
	}
	return client.CheckOwnerCreatedInstance(ctx, p, created)
}
func (runtime Runtime) checkOwnerVolumeInstance(ctx context.Context, p ports.PreparedOwnerVolume, instance *ports.PreparedOwnerInstance, created *ports.NativeOwnerCreated) error {
	client, ok := runtime.Incus.(ownerVolumeCreator)
	if !ok {
		return domain.ErrPlanStale
	}
	return client.CheckPreparedOwnerVolumeInstance(ctx, p, instance, created)
}

func (runtime Runtime) applyOwnerInstancePortions(ctx context.Context, environment map[string]string) error {
	// A retained create result proves identity, not that a guest is still unused.
	// Only this call's successful POST may authorize the initial storage restart.
	environment["SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID"] = ""
	if runtime.ownerVolume == nil {
		return domain.ErrPlanStale
	}
	client, ok := runtime.Incus.(ownerVolumeCreator)
	if !ok {
		return domain.ErrPlanStale
	}
	volume := runtime.ownerVolume.original
	if runtime.ownerInstance == nil {
		if volume.InstanceUUID == "" {
			return domain.ErrPlanStale
		}
		if err := runtime.checkOwnerVolumeInstance(ctx, volume, nil, nil); err != nil {
			return err
		}
	} else if volume.InstanceUUID != "" || runtime.ownerInstance.original.Scope != volume.Scope || runtime.ownerInstance.original.Source != volume.Source || runtime.ownerInstance.original.Project != volume.Project || runtime.ownerInstance.original.Instance != volume.Instance {
		return domain.ErrPlanStale
	}
	if runtime.ownerVolume.created != nil {
		checker, ok := runtime.Incus.(interface {
			CheckOwnerCreatedVolume(context.Context, ports.PreparedOwnerVolume, ports.NativeOwnerCreated) error
		})
		if !ok {
			return domain.ErrPlanStale
		}
		if err := checker.CheckOwnerCreatedVolume(ctx, volume, *runtime.ownerVolume.created); err != nil {
			return err
		}
	} else if err := client.CheckOwnerVolumeGuard(ctx, volume); err != nil {
		return err
	}
	environment["SUBYARD_PREPARED_OWNER_PHASE"] = "1"
	if runtime.ownerInstance != nil && runtime.ownerInstance.created != nil {
		environment["SUBYARD_PREPARED_OWNER_INSTANCE_UUID"] = runtime.ownerInstance.created.InstanceUUID
		environment["SUBYARD_PREPARED_OWNER_INSTANCE_TOKEN"] = runtime.ownerInstance.created.Intent.Token
	}
	if err := runtime.runScriptEnvironment(ctx, runtime.Stderr, environment, "03-create-subyard.sh", "--owner-before-instance", "--yes"); err != nil {
		return err
	}
	var original *ports.PreparedOwnerInstance
	var created *ports.NativeOwnerCreated
	freshInstance := false
	if runtime.ownerInstance != nil {
		original = &runtime.ownerInstance.original
		var value *ports.NativeOwnerCreated
		var err error
		if runtime.ownerInstance.created != nil {
			value = runtime.ownerInstance.created
		} else if original.Policy != nil {
			policyClient, ok := runtime.Incus.(ownerImagePolicyClient)
			if !ok {
				return domain.ErrPlanStale
			}
			policyCreator, ok := runtime.Incus.(ownerPolicyInstanceCreator)
			if !ok {
				return domain.ErrPlanStale
			}
			env := runtime.ownerInstance.environment
			home := runtime.ownerInstance.home
			if err = policyClient.CheckOwnerImagePolicy(ctx, *original.Policy, env, home); err != nil {
				return err
			}
			catalog, e := policyClient.ReadOwnerPolicyImageCatalog(ctx, *original.Policy, env, home)
			if e != nil {
				return e
			}
			rendered, e := runtime.RenderOwnerInstanceConfig(ctx, runtime.ownerInstance.sources, catalog)
			if e != nil {
				return e
			}
			selected, flags, ok := bytes.Cut(rendered, []byte{0})
			_, approvedFlags, approved := bytes.Cut(original.Rendered, []byte{0})
			if !ok || !approved || !bytes.Equal(flags, approvedFlags) || !bytes.Equal(selected, original.Policy.Primary) && !bytes.Equal(selected, original.Policy.Fallback) {
				return domain.ErrPlanStale
			}
			request := *original
			request.Rendered = rendered
			var resolved ports.PreparedOwnerInstance
			value, resolved, err = policyCreator.ApplyPreparedOwnerPolicyInstance(ctx, request, env, home)
			if err == nil {
				freshInstance = true
				runtime.ownerInstance.resolved = &resolved
				original = &resolved
			}
		} else {
			return domain.ErrPlanStale
		}
		if err != nil {
			return err
		}
		if value == nil {
			return domain.ErrPlanStale
		}
		runtime.ownerInstance.created = value
		created = value
		if err := runtime.checkCreatedOwnerInstance(ctx, *original, *created); err != nil {
			return err
		}
		environment["SUBYARD_PREPARED_OWNER_INSTANCE_UUID"] = created.InstanceUUID
		environment["SUBYARD_PREPARED_OWNER_INSTANCE_TOKEN"] = created.Intent.Token
		if freshInstance {
			environment["SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID"] = created.InstanceUUID
		}
	} else {
		environment["SUBYARD_PREPARED_OWNER_INSTANCE_UUID"] = volume.InstanceUUID
	}
	if err := runtime.checkOwnerVolumeInstance(ctx, volume, original, created); err != nil {
		return err
	}
	if err := runtime.runScriptEnvironment(ctx, runtime.Stderr, environment, "03-create-subyard.sh", "--owner-before-srv", "--yes"); err != nil {
		return err
	}
	if volume.Absent {
		value := runtime.ownerVolume.created
		var err error
		if value == nil {
			value, err = client.ApplyPreparedOwnerVolume(ctx, volume, original, created)
		}
		if err != nil {
			return err
		}
		if value == nil {
			return domain.ErrPlanStale
		}
		runtime.ownerVolume.created = value
		environment["SUBYARD_PREPARED_OWNER_VOLUME_TOKEN"] = value.Intent.Token
	}
	if err := client.AttachPreparedOwnerVolume(ctx, volume, original, created, runtime.ownerVolume.created); err != nil {
		return err
	}
	if err := runtime.checkOwnerVolumeInstance(ctx, volume, original, created); err != nil {
		return err
	}
	return runtime.runScriptEnvironment(ctx, runtime.Stderr, environment, "03-create-subyard.sh", "--owner-after-srv", "--yes")
}

// Only the original known instance can receive native successor power metadata.
// The SDK leaf checks that identity on the same GET used for its ETag update.
type ownerCreatedPowerWriter struct{ runtime Runtime }

func (writer ownerCreatedPowerWriter) SetInstanceConfig(ctx context.Context, project, instance string, values map[string]string) error {
	runtime := writer.runtime
	if ports.NativeOperationObserverFrom(ctx) == nil || runtime.ownerInstance == nil || runtime.ownerInstance.created == nil {
		return domain.ErrPlanStale
	}
	original := runtime.ownerInstance.original
	if runtime.ownerInstance.resolved != nil {
		original = *runtime.ownerInstance.resolved
	}
	if project != original.Project || instance != original.Instance {
		return domain.ErrPlanStale
	}
	name, bridge := runtime.Yard.YardName, runtime.Yard.IncusBridge
	if name == "" {
		name = "default"
	}
	if bridge == "" {
		bridge = "incusbr0"
	}
	if values["user.subyard.name"] != name || values["user.subyard.bridge"] != bridge || values["boot.autostart"] != "false" {
		return domain.ErrPlanStale
	}
	if len(values) == 3 {
		return runtime.checkCreatedOwnerInstance(ctx, original, *runtime.ownerInstance.created)
	}
	if len(values) != 6 || values["user.subyard.managed"] != "true" {
		return domain.ErrPlanStale
	}
	desired := values["user.subyard.desired_power"]
	initialized := values["user.subyard.initialized"]
	if desired != "running" && desired != "stopped" || initialized != "true" && initialized != "false" {
		return domain.ErrPlanStale
	}

	client, ok := runtime.Incus.(interface {
		SetOwnerCreatedPower(context.Context, ports.PreparedOwnerInstance, ports.NativeOwnerCreated, string, bool) error
	})
	if !ok {
		return domain.ErrPlanStale
	}
	return client.SetOwnerCreatedPower(ctx, original, *runtime.ownerInstance.created, desired, initialized == "true")
}

type ownerProjectExecution struct {
	original ports.PreparedImageProject
	scope    string
	created  *ports.NativeOwnerCreated
}

func (runtime Runtime) WithOwnerYardProject(ctx context.Context, sources *PreparedInstalledRuntime, p ports.PreparedImageProject, scope string, created *ports.NativeOwnerCreated) (Runtime, error) {
	client, ok := runtime.Incus.(interface {
		OwnerCreateServer(context.Context) (string, error)
	})
	if !ok || p.Project != runtime.Yard.IncusProject || scope != "current" && scope != "legacy" && scope != "recreate" || p.Absent && p.Config != nil {
		return Runtime{}, domain.ErrPlanStale
	}
	server, err := client.OwnerCreateServer(ctx)
	if err != nil || server != p.Server {
		return Runtime{}, domain.ErrPlanStale
	}
	rendered, err := runtime.RenderOwnerProjectConfig(ctx, sources)
	if err != nil || !bytes.Equal(rendered, p.InitialConfig) {
		return Runtime{}, domain.ErrPlanStale
	}
	state := &ownerProjectExecution{original: p, scope: scope}
	if created != nil {
		copy := *created
		state.created = &copy
	}
	runtime.ownerProject = state
	return runtime, nil
}
func (runtime Runtime) OwnerCreatedProject() *ports.NativeOwnerCreated {
	if runtime.ownerProject == nil || runtime.ownerProject.created == nil {
		return nil
	}
	copy := *runtime.ownerProject.created
	return &copy
}
func (runtime Runtime) checkOwnerProject(ctx context.Context) error {
	p := runtime.ownerProject
	if p == nil {
		return domain.ErrPlanStale
	}
	if p.created != nil {
		if p.created.Intent.Scope != p.scope {
			return domain.ErrPlanStale
		}
		if p.original.SharedImages {
			client, ok := runtime.Incus.(interface {
				CheckOwnerCreatedImageProject(context.Context, ports.PreparedImageProject, ports.NativeOwnerCreated) error
			})
			if !ok {
				return domain.ErrPlanStale
			}
			return client.CheckOwnerCreatedImageProject(ctx, p.original, *p.created)
		}
		client, ok := runtime.Incus.(interface {
			CheckOwnerCreatedYardProject(context.Context, ports.PreparedImageProject, ports.NativeOwnerCreated) error
		})
		if !ok {
			return domain.ErrPlanStale
		}
		return client.CheckOwnerCreatedYardProject(ctx, p.original, *p.created)
	}
	if p.original.Absent {
		return domain.ErrPlanStale
	}
	state, err := runtime.reconcileState(ctx)
	if err != nil || !state.ProjectFound || !reflect.DeepEqual(state.ProjectConfig, p.original.Config) {
		return domain.ErrPlanStale
	}
	return nil
}
func (runtime Runtime) applyOwnerProjectPortions(ctx context.Context) error {
	p := runtime.ownerProject
	if p == nil {
		return domain.ErrPlanStale
	}
	if !p.original.Absent || p.created != nil {
		if err := runtime.checkOwnerProject(ctx); err != nil {
			return err
		}
	} else if p.original.SharedImages {
		return domain.ErrPlanStale
	}
	env := map[string]string{"SUBYARD_PREPARED_OWNER_PHASE": "1"}
	if err := runtime.runScriptEnvironment(ctx, runtime.Stderr, env, "02-create-project.sh", "--owner-before-project", "--yes"); err != nil {
		return err
	}
	if p.original.Absent && p.created == nil {
		client, ok := runtime.Incus.(interface {
			ApplyPreparedOwnerYardProject(context.Context, ports.PreparedImageProject, string) (*ports.NativeOwnerCreated, error)
		})
		if !ok {
			return domain.ErrPlanStale
		}
		result, err := client.ApplyPreparedOwnerYardProject(ctx, p.original, p.scope)
		if err != nil {
			return err
		}
		if result == nil {
			return domain.ErrPlanStale
		}
		p.created = result
	}
	if err := runtime.checkOwnerProject(ctx); err != nil {
		return err
	}
	if p.created != nil {
		env["SUBYARD_PREPARED_OWNER_PROJECT_TOKEN"] = p.created.Intent.Token
	}
	return runtime.runScriptEnvironment(ctx, runtime.Stderr, env, "02-create-project.sh", "--owner-after-project", "--yes")
}
