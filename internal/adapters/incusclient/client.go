package incusclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"

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
	operation, err := server.UseProject(project).UpdateInstanceState(name, api.InstanceStatePut{
		Action: action, Timeout: -1, Force: force,
	}, "")
	if err != nil {
		return normalizeError(action+" instance", err)
	}
	if err := operation.Wait(); err != nil {
		return normalizeError("wait for "+action+" instance", err)
	}
	return nil
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
		state.ProfileDevices = cloneDevices(profile.Devices)
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
	return client.exec(ctx, project, name, request, bytes.NewReader(request.Stdin))
}

func (client *Client) StreamExec(
	ctx context.Context,
	project string,
	name string,
	request ports.InstanceExecRequest,
	stdin io.Reader,
) (ports.InstanceExecResult, error) {
	return client.exec(ctx, project, name, request, stdin)
}

func (client *Client) exec(
	ctx context.Context,
	project string,
	name string,
	request ports.InstanceExecRequest,
	stdin io.Reader,
) (ports.InstanceExecResult, error) {
	if len(request.Command) == 0 {
		return ports.InstanceExecResult{}, errors.New("instance exec command is required")
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return ports.InstanceExecResult{}, err
	}
	if err := client.validateServerExtensions(server); err != nil {
		return ports.InstanceExecResult{}, err
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	dataDone := make(chan bool)
	operation, err := server.UseProject(project).ExecInstance(name, api.InstanceExecPost{
		Command: request.Command, Environment: cloneMap(request.Environment),
		WaitForWS: true, Interactive: false, User: request.User, Group: request.Group,
	}, &incus.InstanceExecArgs{
		Stdin: stdin, Stdout: &stdout, Stderr: &stderr,
		DataDone: dataDone,
	})
	if err != nil {
		return ports.InstanceExecResult{}, normalizeError("start instance command", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- operation.Wait() }()
	select {
	case <-ctx.Done():
		_ = operation.Cancel()
		<-wait
		<-dataDone
		return ports.InstanceExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()},
			fmt.Errorf("wait for instance command: %w", context.Cause(ctx))
	case err := <-wait:
		<-dataDone
		exitCode := 0
		if value, ok := operation.Get().Metadata["return"].(float64); ok {
			exitCode = int(value)
		}
		result := ports.InstanceExecResult{
			Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitCode,
		}
		if err != nil {
			return result, normalizeError("wait for instance command", err)
		}
		if exitCode != 0 {
			return result, fmt.Errorf("instance command exited with status %d", exitCode)
		}
		return result, nil
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
	if err := updateInstanceDevices(projectServer, instanceName, instance, etag); err != nil {
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
	if err := updateInstanceDevices(server.UseProject(project), instanceName, instance, etag); err != nil {
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
	operation, err := projectServer.UpdateInstance(instanceName, instance.Writable(), etag)
	if err != nil {
		return normalizeError("update instance config", err)
	}
	if err := operation.Wait(); err != nil {
		return normalizeError("wait for instance config update", err)
	}
	return nil
}

func updateInstanceDevices(
	server incus.InstanceServer,
	name string,
	instance *api.Instance,
	etag string,
) error {
	operation, err := server.UpdateInstance(name, instance.Writable(), etag)
	if err != nil {
		return normalizeError("update instance devices", err)
	}
	if err := operation.Wait(); err != nil {
		return normalizeError("wait for instance device update", err)
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
