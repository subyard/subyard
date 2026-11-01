package incusclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

type Client struct {
	socket             string
	requiredExtensions []string
}

func New(socket string, requiredExtensions ...string) *Client {
	return &Client{socket: socket, requiredExtensions: slices.Clone(requiredExtensions)}
}

// LocalInstallationAbsent proves a cold local install, not merely an unavailable
// daemon. Package removal can leave its database behind. Custom sockets do not
// establish where the server keeps that database, so they remain inconclusive.
func (client *Client) LocalInstallationAbsent() bool {
	if client.socket != "" || os.Getenv("INCUS_SOCKET") != "" {
		return false
	}
	if _, err := exec.LookPath("incus"); !errors.Is(err, exec.ErrNotFound) {
		return false
	}
	paths := []string{"/var/lib/incus", "/run/incus"}
	// Match the native client's INCUS_DIR override, including during rechecks.
	if directory := os.Getenv("INCUS_DIR"); directory != "" {
		if !filepath.IsAbs(directory) {
			return false
		}
		paths = []string{filepath.Clean(directory)}
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

func (client *Client) Server(ctx context.Context) (ports.ServerInfo, error) {
	server, err := client.connect(ctx, true)
	if err != nil {
		return ports.ServerInfo{}, err
	}
	info, _, err := server.GetServer()
	if err != nil {
		return ports.ServerInfo{}, normalizeError("get server", err)
	}
	if err := client.validateExtensions(info); err != nil {
		return ports.ServerInfo{}, err
	}
	return ports.ServerInfo{
		Environment:   info.Environment.Server,
		Version:       info.Environment.ServerVersion,
		APIExtensions: slices.Clone(info.APIExtensions),
	}, nil
}

func (client *Client) Instance(ctx context.Context, project, name string) (ports.InstanceInfo, error) {
	server, err := client.connect(ctx, true)
	if err != nil {
		return ports.InstanceInfo{}, err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return ports.InstanceInfo{}, err
	}
	instance, _, err := server.UseProject(project).GetInstance(name)
	if err != nil {
		if api.StatusErrorCheck(err, http.StatusNotFound) {
			return ports.InstanceInfo{}, fmt.Errorf("%w: %s/%s", ports.ErrInstanceNotFound, project, name)
		}
		return ports.InstanceInfo{}, normalizeError("get instance", err)
	}
	return instanceInfo(project, instance), nil
}

func (client *Client) ListInstances(ctx context.Context) ([]ports.InstanceInfo, error) {
	server, err := client.connect(ctx, true)
	if err != nil {
		return nil, err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return nil, err
	}
	instances, err := server.GetInstancesAllProjects(api.InstanceTypeAny)
	if err != nil {
		return nil, normalizeError("list instances", err)
	}
	result := make([]ports.InstanceInfo, 0, len(instances))
	for index := range instances {
		result = append(result, instanceInfo(instances[index].Project, &instances[index]))
	}
	return result, nil
}

func (client *Client) SetInstancePower(
	ctx context.Context,
	project string,
	name string,
	action string,
	force bool,
) error {
	if err := client.setInstancePower(ctx, project, name, action, force, "", domain.YardVM); err != nil {
		return err
	}
	if action == "start" {
		if err := client.applyVMCPU(ctx, project, name); err != nil {
			stopContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
			defer cancel()
			return errors.Join(err, client.SetInstancePower(stopContext, project, name, "stop", true))
		}
	}
	return nil
}

// SetInstancePowerIf changes only the original VM power state. The native
// lifecycle caller owns host guards, guest readiness and CPU scheduling.
func (client *Client) SetInstancePowerIf(ctx context.Context, project, name, action string, force bool, expectedUUID string) error {
	if expectedUUID == "" {
		return domain.ErrPlanStale
	}
	return client.setInstancePower(ctx, project, name, action, force, expectedUUID, domain.YardVM)
}

// SetOriginalInstancePowerIf retains the captured kind and UUID through the raw SDK leaf.
func (client *Client) SetOriginalInstancePowerIf(ctx context.Context, project, name, action string, force bool, uuid string, kind domain.YardKind) error {
	if uuid == "" || kind != domain.YardVM && kind != domain.YardContainer {
		return domain.ErrPlanStale
	}
	return client.setInstancePower(ctx, project, name, action, force, uuid, kind)
}

func (client *Client) setInstancePower(ctx context.Context, project, name, action string, force bool, expectedUUID string, kind domain.YardKind) error {
	if action != "start" && action != "stop" {
		return fmt.Errorf("invalid instance power action %q", action)
	}
	if force && action != "stop" {
		return errors.New("forced instance power is valid only for stop")
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return err
	}
	check := func() error {
		if kind == domain.YardVM {
			return client.checkVMUUID(ctx, project, name, expectedUUID)
		}
		return client.checkOriginalKindUUID(ctx, project, name, expectedUUID, kind)
	}
	if err := check(); err != nil {
		return err
	}
	if err := beforeNative(ctx, server, "instance-power", project, name); err != nil {
		return err
	}
	operation, err := server.UseProject(project).UpdateInstanceState(name, api.InstanceStatePut{
		Action: action, Timeout: -1, Force: force,
	}, "")
	if err != nil {
		return normalizeError(action+" instance", err)
	}
	if err := waitNative(ctx, server, operation, "instance-power", project, name); err != nil {
		return normalizeOperationError("wait for "+action+" instance", operation.Get(), err)
	}
	return check()
}

func (client *Client) ReconcileState(
	ctx context.Context,
	projectName string,
	instanceName string,
	pool string,
	volume string,
	network string,
) (ports.ReconcileState, error) {
	server, err := client.reconcileServer(ctx)
	if err != nil {
		return ports.ReconcileState{}, err
	}
	state := ports.ReconcileState{}
	_, _, err = server.GetStoragePool(pool)
	if err != nil {
		if !api.StatusErrorCheck(err, http.StatusNotFound) {
			return state, normalizeError("get storage pool", err)
		}
	} else {
		state.HostPoolFound = true
	}
	_, _, err = server.GetNetwork(network)
	if err != nil {
		if !api.StatusErrorCheck(err, http.StatusNotFound) {
			return state, normalizeError("get network", err)
		}
	} else {
		state.HostNetworkFound = true
	}
	project, _, err := server.GetProject(projectName)
	if err != nil {
		if api.StatusErrorCheck(err, http.StatusNotFound) {
			return state, nil
		}
		return state, normalizeError("get project", err)
	}
	state.ProjectFound = true
	state.ProjectConfig = cloneMap(project.Config)
	projectServer := server.UseProject(projectName)
	profile, _, err := projectServer.GetProfile("default")
	if err != nil {
		if !api.StatusErrorCheck(err, http.StatusNotFound) {
			return state, normalizeError("get profile", err)
		}
	} else {
		state.ProfileFound = true
		state.ProfileConfig = cloneMap(profile.Config)
		state.ProfileDevices = cloneDevices(profile.Devices)
		state.ProfileConfig = cloneMap(profile.Config)
	}
	instance, _, err := projectServer.GetInstance(instanceName)
	if err != nil {
		if !api.StatusErrorCheck(err, http.StatusNotFound) {
			return state, normalizeError("get instance", err)
		}
	} else {
		state.InstanceFound = true
		state.Instance = instanceInfo(projectName, instance)
	}
	_, _, err = projectServer.GetStoragePoolVolume(pool, "custom", volume)
	if err != nil {
		if !api.StatusErrorCheck(err, http.StatusNotFound) {
			return state, normalizeError("get storage volume", err)
		}
	} else {
		state.VolumeFound = true
	}
	return state, nil
}

func (client *Client) Exec(
	ctx context.Context,
	project string,
	name string,
	request ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	return client.execWithInput(ctx, project, name, request, bytes.NewReader(request.Stdin), false, true)
}

func (client *Client) StreamExec(
	ctx context.Context,
	project string,
	name string,
	request ports.InstanceExecRequest,
	stdin io.Reader,
) (ports.InstanceExecResult, error) {
	return client.exec(ctx, project, name, request, stdin, false)
}

func (client *Client) exec(
	ctx context.Context,
	project string,
	name string,
	request ports.InstanceExecRequest,
	stdin io.Reader,
	agentProbe bool,
) (ports.InstanceExecResult, error) {
	return client.execWithInput(ctx, project, name, request, stdin, agentProbe, false)
}

func (client *Client) execWithInput(
	ctx context.Context,
	project, name string,
	request ports.InstanceExecRequest,
	stdin io.Reader,
	agentProbe, boundedInput bool,
) (ports.InstanceExecResult, error) {
	if len(request.Command) == 0 {
		return ports.InstanceExecResult{}, errors.New("instance exec command is required")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	server, err := client.connect(ctx, true)
	if err != nil {
		return ports.InstanceExecResult{}, err
	}
	info, err := server.GetConnectionInfo()
	if err != nil {
		return ports.InstanceExecResult{}, err
	}
	httpClient, err := server.GetHTTPClient()
	if err != nil {
		return ports.InstanceExecResult{}, err
	}
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok || info.SocketPath == "" {
		return ports.InstanceExecResult{}, errors.New("instance exec requires its original Unix transport")
	}
	// The SDK websocket handshake detaches the request context. Keep each
	// connection inside this Exec, including its original wait/drain reserve.
	transport.DialContext = func(dialContext context.Context, _, _ string) (net.Conn, error) {
		dialContext, stop := context.WithCancel(dialContext)
		stopOwner := context.AfterFunc(ctx, stop)
		defer func() { stopOwner(); stop() }()
		connection, err := (&net.Dialer{}).DialContext(dialContext, "unix", info.SocketPath)
		if err == nil {
			context.AfterFunc(ctx, func() { _ = connection.Close() })
		}
		return connection, err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return ports.InstanceExecResult{}, err
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	dataDone := make(chan bool)
	var output, diagnostics io.Writer = &stdout, &stderr
	if agentProbe {
		// The fixed read-only probe has no output contract.
		output, diagnostics = io.Discard, io.Discard
	}
	if err := beforeNative(ctx, server, "instance-exec", project, name); err != nil {
		return ports.InstanceExecResult{}, err
	}
	operation, err := server.UseProject(project).ExecInstance(name, api.InstanceExecPost{
		Command: request.Command, Environment: cloneMap(request.Environment),
		WaitForWS: true, Interactive: false, User: request.User, Group: request.Group,
	}, &incus.InstanceExecArgs{
		Stdin: stdin, Stdout: output, Stderr: diagnostics,
		DataDone: dataDone,
	})
	if err != nil {
		if observer := ports.NativeOperationObserverFrom(ctx); observer != nil {
			observer.Uncertain()
		}
		err = errors.Join(normalizeError("start instance command", err), context.Cause(ctx))
		if agentProbe {
			err = fmt.Errorf("agent probe dispatch unproved: %w", err)
		}
		return ports.InstanceExecResult{}, err
	}
	if agentProbe {
		return ports.InstanceExecResult{}, finishInstanceAgentProbe(ctx, operation, dataDone)
	}
	wait := make(chan error, 1)
	var checked nativeWaitResult
	go func() { wait <- waitNativeChecked(ctx, server, operation, "instance-exec", project, name, &checked) }()
	select {
	case <-ctx.Done():
		_ = operation.Cancel()
		waited := <-wait
		observer := ports.NativeOperationObserverFrom(ctx)
		if observer != nil {
			observer.Uncertain()
		}
		if err := drainExecData(ctx, dataDone); err != nil {
			if observer != nil {
				return ports.InstanceExecResult{}, errors.Join(waited, err, context.Cause(ctx))
			}
			return ports.InstanceExecResult{}, err
		}
		canceled := fmt.Errorf("wait for instance command: %w", context.Cause(ctx))
		if observer != nil {
			canceled = errors.Join(waited, canceled)
		}
		return ports.InstanceExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, canceled
	case err := <-wait:
		if drainErr := drainExecData(ctx, dataDone); drainErr != nil {
			return ports.InstanceExecResult{}, errors.Join(err, drainErr)
		}
		exitCode := 0
		if value, ok := operation.Get().Metadata["return"].(float64); ok {
			exitCode = int(value)
		}
		result := ports.InstanceExecResult{
			Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitCode,
		}
		if err != nil {
			return result, normalizeOperationError("wait for instance command", operation.Get(), err)
		}
		if observer := ports.NativeOperationObserverFrom(ctx); observer != nil {
			if boundedInput {
				explicit, completedErr := completedNativeExec(ctx, server, operation, checked)
				result.ExitCode = explicit
				return result, completedErr
			}
			explicit, valid := explicitNativeExecExit(operation.Get().Metadata)
			if !valid {
				observer.Uncertain()
				return result, errors.New("native instance command return status unproved")
			}
			exitCode, result.ExitCode = explicit, explicit
		}
		if exitCode != 0 {
			return result, fmt.Errorf("instance command exited with status %d", exitCode)
		}
		return result, nil
	}
}

func drainExecData(ctx context.Context, done <-chan bool) error {
	observer := ports.NativeOperationObserverFrom(ctx)
	if observer == nil {
		<-done
		return context.Cause(ctx)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		observer.Uncertain()
		return errors.New("native exec streams did not drain")
	}
}

func (client *Client) EnsureDiskDevice(
	ctx context.Context,
	project string,
	instanceName string,
	deviceName string,
	source string,
	target string,
) (bool, error) {
	server, err := client.connect(ctx, true)
	if err != nil {
		return false, err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return false, err
	}
	projectServer := server.UseProject(project)
	instance, etag, err := projectServer.GetInstance(instanceName)
	if err != nil {
		return false, normalizeError("get instance", err)
	}
	desired := map[string]string{"type": "disk", "source": source, "path": target, "shift": "true"}
	if current, exists := instance.Devices[deviceName]; exists {
		if equalDevice(current, desired) {
			return false, nil
		}
		return false, fmt.Errorf("instance device %q already exists with different configuration", deviceName)
	}
	if instance.Devices == nil {
		instance.Devices = make(map[string]map[string]string)
	}
	instance.Devices[deviceName] = desired
	if err := updateInstanceDevices(ctx, projectServer, instanceName, instance, etag); err != nil {
		return false, err
	}
	return true, nil
}

func (client *Client) RemoveDevice(
	ctx context.Context,
	project string,
	instanceName string,
	deviceName string,
) (bool, error) {
	server, err := client.connect(ctx, true)
	if err != nil {
		return false, err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return false, err
	}
	instance, etag, err := server.UseProject(project).GetInstance(instanceName)
	if err != nil {
		return false, normalizeError("get instance", err)
	}
	if _, exists := instance.Devices[deviceName]; !exists {
		return false, nil
	}
	delete(instance.Devices, deviceName)
	if err := updateInstanceDevices(ctx, server.UseProject(project), instanceName, instance, etag); err != nil {
		return false, err
	}
	return true, nil
}

func (client *Client) SetInstanceConfig(
	ctx context.Context,
	project string,
	instanceName string,
	values map[string]string,
) error {
	return client.SetInstanceConfigIf(ctx, project, instanceName, "", nil, values)
}

// SetInstanceConfigIf checks the original instance and selected local keys in
// the same SDK GET/ETag update. An empty UUID retains ordinary Go behavior.
func (client *Client) SetInstanceConfigIf(ctx context.Context, project, instanceName, uuid string, before, values map[string]string) error {
	return client.setInstanceConfigIf(ctx, project, instanceName, uuid, domain.YardVM, false, before, values)
}

// SetOriginalInstanceConfigIf keeps the captured native owner kind and UUID.
// Ordinary and existing VM-only callers retain their existing entrypoints.
func (client *Client) SetOriginalInstanceConfigIf(ctx context.Context, project, name, uuid string, kind domain.YardKind, before, values map[string]string) error {
	if uuid == "" || kind != domain.YardVM && kind != domain.YardContainer {
		return domain.ErrPlanStale
	}
	if err := client.setInstanceConfigIf(ctx, project, name, uuid, kind, true, before, values); err != nil {
		return err
	}
	instance, err := client.Instance(ctx, project, name)
	if err != nil {
		return err
	}
	if instance.Project != project || instance.Name != name || instance.Type != kind || instance.Config["volatile.uuid"] != uuid || instance.LocalConfig["volatile.uuid"] != uuid {
		return domain.ErrPlanStale
	}
	return nil
}

func (client *Client) setInstanceConfigIf(ctx context.Context, project, instanceName, uuid string, kind domain.YardKind, original bool, before, values map[string]string) error {
	server, err := client.connect(ctx, true)
	if err != nil {
		return err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return err
	}
	projectServer := server.UseProject(project)
	instance, etag, err := projectServer.GetInstance(instanceName)
	if err != nil {
		return normalizeError("get instance", err)
	}
	if uuid != "" {
		if instance == nil || instance.Name != instanceName || instance.Project != "" && instance.Project != project || instance.Config["volatile.uuid"] != uuid || etag == "" {
			return domain.ErrPlanStale
		}
		if original {
			if instanceInfo(project, instance).Type != kind || instance.ExpandedConfig["volatile.uuid"] != uuid {
				return domain.ErrPlanStale
			}
		} else if instance.Type != "virtual-machine" {
			return domain.ErrPlanStale
		}
		for key, value := range before {
			if instance.Config[key] != value || instance.ExpandedConfig[key] != "" && instance.ExpandedConfig[key] != value {
				return domain.ErrPlanStale
			}
		}
	}
	changed := false
	if instance.Config == nil {
		instance.Config = make(map[string]string)
	}
	for key, value := range values {
		if instance.Config[key] != value {
			instance.Config[key] = value
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := beforeNative(ctx, projectServer, "instance-config", project, instanceName); err != nil {
		return err
	}
	operation, err := projectServer.UpdateInstance(instanceName, instance.Writable(), etag)
	if err != nil {
		return normalizeError("update instance config", err)
	}
	if err := waitNative(ctx, projectServer, operation, "instance-config", project, instanceName); err != nil {
		return normalizeOperationError("wait for instance config update", operation.Get(), err)
	}
	return nil
}

func updateInstanceDevices(
	ctx context.Context,
	server incus.InstanceServer,
	name string,
	instance *api.Instance,
	etag string,
) error {
	info, infoErr := server.GetConnectionInfo()
	if infoErr != nil && ports.NativeOperationObserverFrom(ctx) != nil {
		return infoErr
	}
	project := ""
	if info != nil {
		project = info.Project
	}
	if err := beforeNative(ctx, server, "instance-devices", project, name); err != nil {
		return err
	}
	operation, err := server.UpdateInstance(name, instance.Writable(), etag)
	if err != nil {
		return normalizeError("update instance devices", err)
	}
	if err := waitNative(ctx, server, operation, "instance-devices", project, name); err != nil {
		return normalizeOperationError("wait for instance device update", operation.Get(), err)
	}
	return nil
}

func equalDevice(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func (client *Client) Events(ctx context.Context, types []string) (<-chan domain.OperationEvent, <-chan error) {
	events := make(chan domain.OperationEvent, 64)
	errorsOut := make(chan error, 1)
	server, err := client.connect(ctx, false)
	if err != nil {
		close(events)
		errorsOut <- err
		close(errorsOut)
		return events, errorsOut
	}
	if err := client.validateServerExtensions(server); err != nil {
		close(events)
		errorsOut <- err
		close(errorsOut)
		return events, errorsOut
	}
	listener, err := server.GetEventsAllProjects()
	if err != nil {
		close(events)
		errorsOut <- normalizeError("open event stream", err)
		close(errorsOut)
		return events, errorsOut
	}
	var sequence atomic.Uint64
	var callbacksMu sync.Mutex
	callbacksDone := sync.NewCond(&callbacksMu)
	callbacksActive := 0
	callbacksStopping := false
	_, err = listener.AddHandler(types, func(event api.Event) {
		callbacksMu.Lock()
		if callbacksStopping {
			callbacksMu.Unlock()
			return
		}
		callbacksActive++
		callbacksMu.Unlock()
		defer func() {
			callbacksMu.Lock()
			callbacksActive--
			callbacksDone.Broadcast()
			callbacksMu.Unlock()
		}()
		data := make(map[string]any)
		if len(event.Metadata) != 0 {
			_ = json.Unmarshal(event.Metadata, &data)
		}
		data = publicEventData(data)
		current := sequence.Add(1)
		operationID, _ := data["id"].(string)
		normalized := domain.OperationEvent{
			OperationID: operationID,
			Sequence:    current,
			Revision:    current,
			Kind:        event.Type,
			At:          event.Timestamp.UTC(),
			Data:        data,
		}
		select {
		case events <- normalized:
		default:
			select {
			case errorsOut <- errors.New("Incus event consumer exceeded bounded buffer"):
			default:
			}
			listener.Disconnect()
		}
	})
	if err != nil {
		listener.Disconnect()
		close(events)
		errorsOut <- fmt.Errorf("register event handler: %w", err)
		close(errorsOut)
		return events, errorsOut
	}
	go func() {
		stopped := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				listener.Disconnect()
			case <-stopped:
			}
		}()
		err := listener.Wait()
		close(stopped)
		callbacksMu.Lock()
		callbacksStopping = true
		for callbacksActive != 0 {
			callbacksDone.Wait()
		}
		callbacksMu.Unlock()
		if err != nil && ctx.Err() == nil {
			select {
			case errorsOut <- normalizeError("event stream", err):
			default:
			}
		}
		close(events)
		close(errorsOut)
	}()
	return events, errorsOut
}

func publicEventData(source map[string]any) map[string]any {
	result := make(map[string]any)
	for _, key := range []string{"id", "action", "source", "project", "location", "requestor"} {
		if value, ok := source[key]; ok {
			result[key] = value
		}
	}
	return result
}

func (client *Client) connect(ctx context.Context, skipEvents bool) (incus.InstanceServer, error) {
	server, err := incus.ConnectIncusUnixWithContext(ctx, client.socket, &incus.ConnectionArgs{
		SkipGetEvents: skipEvents,
		SkipGetServer: true,
		UserAgent:     "subyard-engine",
	})
	if err != nil {
		return nil, normalizeError("connect to Incus", err)
	}
	return server, nil
}

func (client *Client) reconcileServer(ctx context.Context) (incus.InstanceServer, error) {
	server, err := client.connect(ctx, true)
	if err != nil {
		return nil, err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return nil, err
	}
	return server, nil
}

func (client *Client) validateServerExtensions(server incus.InstanceServer) error {
	if len(client.requiredExtensions) == 0 {
		return nil
	}
	info, _, err := server.GetServer()
	if err != nil {
		return normalizeError("get server extensions", err)
	}
	return client.validateExtensions(info)
}

func (client *Client) validateExtensions(info *api.Server) error {
	for _, extension := range client.requiredExtensions {
		if !slices.Contains(info.APIExtensions, extension) {
			return fmt.Errorf("Incus API extension %q is required", extension)
		}
	}
	return nil
}

func normalizeError(operation string, err error) error {
	if api.StatusErrorCheck(err, http.StatusServiceUnavailable) {
		return fmt.Errorf("%s: %w: %w", operation, ports.ErrIncusUnavailable, err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func normalizeOperationError(description string, operation api.Operation, err error) error {
	// Incus reports pool readiness as HTTP 503, but async operations retain only
	// the message in Err and the SDK returns errors.New(Err) from Wait. Recover
	// this exact server error without retrying arbitrary operation failures.
	pool, prefix := strings.CutPrefix(operation.Err, `Storage pool "`)
	pool, suffix := strings.CutSuffix(pool, `" unavailable on this server`)
	if prefix && suffix && pool != "" && !strings.ContainsAny(pool, "\"\\\r\n") {
		return fmt.Errorf("%s: %w: %w", description, ports.ErrIncusUnavailable, err)
	}
	return normalizeError(description, err)
}

func cloneMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneDevices(source map[string]map[string]string) map[string]map[string]string {
	result := make(map[string]map[string]string, len(source))
	for name, values := range source {
		result[name] = cloneMap(values)
	}
	return result
}

func instanceInfo(project string, instance *api.Instance) ports.InstanceInfo {
	instanceType := domain.YardKind(instance.Type)
	if instanceType == "virtual-machine" {
		instanceType = domain.YardVM
	}
	return ports.InstanceInfo{
		Name: instance.Name, Project: project, Type: instanceType, Status: instance.Status,
		Config: cloneMap(instance.ExpandedConfig), Devices: cloneDevices(instance.ExpandedDevices),
		LocalConfig: cloneMap(instance.Config), LocalDevices: cloneDevices(instance.Devices),
	}
}

// The official SDK decodes JSON operation metadata numbers as float64. Preserve
// signed nonzero statuses (including signal/exec failure), but never infer zero.
func explicitNativeExecExit(metadata map[string]any) (int, bool) {
	value, ok := metadata["return"].(float64)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
		return 0, false
	}
	code := int(value)
	return code, float64(code) == value
}
