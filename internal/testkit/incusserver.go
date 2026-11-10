package testkit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// IncusServer implements the REST/WebSocket subset consumed by the official
// client adapter. It listens only on a Unix socket under a test-owned root.
type IncusServer struct {
	SocketPath string

	listener net.Listener
	server   *http.Server

	mu            sync.Mutex
	serverInfo    map[string]any
	instances     map[string]map[string]any
	projects      map[string]map[string]any
	connections   map[*websocket.Conn]struct{}
	connected     chan struct{}
	execStarted   chan struct{}
	execSteps     []IncusServerExecStep
	execResponder func(IncusServerExecCall) IncusServerExecStep
	execCalls     []IncusServerExecCall
	powerCalls    []IncusServerPowerCall
	powerErrors   []string
	configUpdates []IncusServerConfigUpdate
	// projectUpdates are accepted project PUT requests in order.
	projectUpdates    []IncusServerConfigUpdate
	updateHook        func(map[string]any) (int, string)
	projectUpdateHook func(map[string]any)
	operations        map[string]*incusOperation
	eventQuery        string
	nextOp            int
}

type IncusServerExecStep struct {
	Stdout         []byte
	Stderr         []byte
	ExitCode       int
	StartError     string
	OperationError string
	Release        <-chan struct{}
	AfterInput     func(IncusServerExecCall) int
	// Queued, returned by a responder, serves the call from the queued steps.
	Queued bool
}

type IncusServerExecCall struct {
	Project     string
	Name        string
	Command     []string
	Environment map[string]string
	User        uint32
	Group       uint32
	Stdin       []byte
}

type IncusServerPowerCall struct {
	Project string
	Name    string
	Action  string
	Force   bool
}

// IncusServerConfigUpdate records one accepted instance PUT.
type IncusServerConfigUpdate struct {
	Project string
	Name    string
	Config  map[string]string
}

type incusOperation struct {
	id        string
	step      IncusServerExecStep
	callIndex int
	cancelled chan struct{}
	stdinDone chan struct{}
	cancel    sync.Once
	resource  map[string][]string
	mu        sync.Mutex
	terminal  map[string]any
}

func NewIncusServer(root string) (*IncusServer, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("fake Incus root must be absolute")
	}
	socket := filepath.Join(root, "incus.socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	fake := &IncusServer{
		SocketPath: socket,
		listener:   listener,
		serverInfo: map[string]any{
			"api_extensions": []string{"projects", "instances", "instance_all_projects"},
			"environment":    map[string]any{"server": "incus", "server_version": "6.23"},
		},
		instances:   make(map[string]map[string]any),
		projects:    make(map[string]map[string]any),
		connections: make(map[*websocket.Conn]struct{}),
		connected:   make(chan struct{}, 1),
		execStarted: make(chan struct{}, 1),
		operations:  make(map[string]*incusOperation),
	}
	fake.server = &http.Server{Handler: fake}
	go func() { _ = fake.server.Serve(listener) }()
	return fake, nil
}

func (fake *IncusServer) WaitForExecCount(ctx context.Context, count int) error {
	for {
		fake.mu.Lock()
		current := len(fake.execCalls)
		fake.mu.Unlock()
		if current >= count {
			return nil
		}
		select {
		case <-fake.execStarted:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

func (fake *IncusServer) WaitForExecInput(ctx context.Context, index int) error {
	fake.mu.Lock()
	var done <-chan struct{}
	for _, operation := range fake.operations {
		if operation.callIndex == index {
			done = operation.stdinDone
			break
		}
	}
	fake.mu.Unlock()
	if done == nil {
		return fmt.Errorf("exec call %d has no operation", index)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (fake *IncusServer) QueueExec(steps ...IncusServerExecStep) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, step := range steps {
		step.Stdout = bytes.Clone(step.Stdout)
		step.Stderr = bytes.Clone(step.Stderr)
		fake.execSteps = append(fake.execSteps, step)
	}
}

func (fake *IncusServer) ExecCalls() []IncusServerExecCall {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	result := make([]IncusServerExecCall, len(fake.execCalls))
	for index, call := range fake.execCalls {
		call.Command = append([]string(nil), call.Command...)
		call.Environment = cloneStringMap(call.Environment)
		call.Stdin = bytes.Clone(call.Stdin)
		result[index] = call
	}
	return result
}

func (fake *IncusServer) PowerCalls() []IncusServerPowerCall {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]IncusServerPowerCall(nil), fake.powerCalls...)
}

func (fake *IncusServer) QueuePowerErrors(messages ...string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.powerErrors = append(fake.powerErrors, messages...)
}

func (fake *IncusServer) SetExtensions(extensions ...string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.serverInfo["api_extensions"] = append([]string(nil), extensions...)
}

// SetInventory replaces a complete canned observation without changing queued
// execution responses, calls or operation history.
func (fake *IncusServer) SetInventory(projects, instances map[string]map[string]any) {
	projectSnapshot := make(map[string]map[string]any, len(projects))
	instanceSnapshot := make(map[string]map[string]any, len(instances))
	for name, value := range projects {
		projectSnapshot[name] = cloneAnyMap(value)
	}
	for name, value := range instances {
		instanceSnapshot[name] = cloneAnyMap(value)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.projects, fake.instances = projectSnapshot, instanceSnapshot
}

func (fake *IncusServer) SetInstance(project, name string, instance map[string]any) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.instances[project+"/"+name] = cloneAnyMap(instance)
}

// ReplaceInstances publishes one complete project/name inventory snapshot.
func (fake *IncusServer) ReplaceInstances(instances map[string]map[string]any) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	replacement := make(map[string]map[string]any, len(instances))
	for name, instance := range instances {
		replacement[name] = cloneAnyMap(instance)
	}
	fake.instances = replacement
}

func (fake *IncusServer) SetProject(name string, project map[string]any) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.projects[name] = cloneAnyMap(project)
}

func (fake *IncusServer) WaitForEventClient(ctx context.Context) error {
	select {
	case <-fake.connected:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (fake *IncusServer) Emit(event any) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.connections) == 0 {
		return errors.New("fake Incus has no event client")
	}
	for connection := range fake.connections {
		if err := connection.WriteJSON(event); err != nil {
			return err
		}
	}
	return nil
}

func (fake *IncusServer) DisconnectEvents() {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for connection := range fake.connections {
		_ = connection.Close()
		delete(fake.connections, connection)
	}
}

func (fake *IncusServer) Close() error {
	fake.DisconnectEvents()
	err := fake.server.Close()
	_ = fake.listener.Close()
	_ = os.Remove(fake.SocketPath)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (fake *IncusServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.URL.Path == "/1.0":
		fake.mu.Lock()
		metadata := cloneAnyMap(fake.serverInfo)
		fake.mu.Unlock()
		writeIncusSync(writer, metadata)
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/exec") &&
		strings.HasPrefix(request.URL.Path, "/1.0/instances/"):
		fake.startExec(writer, request)
	case request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/state") &&
		strings.HasPrefix(request.URL.Path, "/1.0/instances/"):
		fake.setPower(writer, request)
	case request.Method == http.MethodPut && strings.HasPrefix(request.URL.Path, "/1.0/instances/") &&
		!strings.Contains(strings.TrimPrefix(request.URL.Path, "/1.0/instances/"), "/"):
		fake.updateInstance(writer, request)
	case strings.HasPrefix(request.URL.Path, "/1.0/operations/"):
		fake.serveOperation(writer, request)
	case request.Method == http.MethodGet && request.URL.Path == "/1.0/instances":
		fake.listInstances(writer, request)
	case request.Method == http.MethodPut && strings.HasPrefix(request.URL.Path, "/1.0/projects/"):
		fake.updateProject(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/1.0/projects/"):
		project := strings.TrimPrefix(request.URL.Path, "/1.0/projects/")
		fake.mu.Lock()
		metadata, found := fake.projects[project]
		metadata = cloneAnyMap(metadata)
		fake.mu.Unlock()
		if !found {
			writeIncusError(writer, http.StatusNotFound, "project not found")
			return
		}
		writer.Header().Set("ETag", incusServerETag(metadata))
		writeIncusSync(writer, metadata)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/1.0/instances/") && strings.HasSuffix(request.URL.Path, "/snapshots"):
		name := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/1.0/instances/"), "/snapshots")
		project := request.URL.Query().Get("project")
		if project == "" {
			project = "default"
		}
		fake.mu.Lock()
		_, found := fake.instances[project+"/"+name]
		fake.mu.Unlock()
		if !found {
			writeIncusError(writer, http.StatusNotFound, "instance not found")
			return
		}
		writeIncusSync(writer, []string{})
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/1.0/instances/"):
		name := strings.TrimPrefix(request.URL.Path, "/1.0/instances/")
		project := request.URL.Query().Get("project")
		fake.mu.Lock()
		instance, ok := fake.instances[project+"/"+name]
		instance = cloneAnyMap(instance)
		fake.mu.Unlock()
		if !ok {
			writeIncusError(writer, http.StatusNotFound, "instance not found")
			return
		}
		writer.Header().Set("ETag", incusServerETag(instance))
		writeIncusSync(writer, instance)
	case request.URL.Path == "/1.0/events":
		fake.mu.Lock()
		fake.eventQuery = request.URL.RawQuery
		fake.mu.Unlock()
		fake.serveEvents(writer, request)
	default:
		writeIncusError(writer, http.StatusNotFound, "endpoint not found")
	}
}

func (fake *IncusServer) listInstances(writer http.ResponseWriter, request *http.Request) {
	allProjects := request.URL.Query().Get("all-projects") == "true"
	selectedProject := request.URL.Query().Get("project")
	if selectedProject == "" {
		selectedProject = "default"
	}
	fake.mu.Lock()
	if _, found := fake.projects[selectedProject]; !allProjects && !found {
		fake.mu.Unlock()
		writeIncusError(writer, http.StatusNotFound, "project not found")
		return
	}
	instances := make([]map[string]any, 0, len(fake.instances))
	for key, source := range fake.instances {
		project, _, _ := strings.Cut(key, "/")
		if !allProjects && project != selectedProject {
			continue
		}
		instance := cloneAnyMap(source)
		if _, ok := instance["project"]; !ok {
			instance["project"] = project
		}
		instances = append(instances, instance)
	}
	fake.mu.Unlock()
	writeIncusSync(writer, instances)
}

// ConfigUpdates returns the accepted instance PUT requests in order.
func (fake *IncusServer) ConfigUpdates() []IncusServerConfigUpdate {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	updates := make([]IncusServerConfigUpdate, len(fake.configUpdates))
	for index, update := range fake.configUpdates {
		updates[index] = update
		updates[index].Config = cloneStringMap(update.Config)
	}
	return updates
}

// ProjectUpdates returns the accepted project PUT requests in order.
func (fake *IncusServer) ProjectUpdates() []IncusServerConfigUpdate {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	updates := make([]IncusServerConfigUpdate, len(fake.projectUpdates))
	for index, update := range fake.projectUpdates {
		updates[index] = update
		updates[index].Config = cloneStringMap(update.Config)
	}
	return updates
}

// OnInstanceUpdate runs before each instance PUT is checked. It may change the
// stored instance, as a concurrent writer would, or return a nonzero HTTP
// status and message to refuse the request.
func (fake *IncusServer) OnInstanceUpdate(hook func(instance map[string]any) (int, string)) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.updateHook = hook
}

// OnProjectUpdate models a concurrent edit before checking a project PUT's ETag.
func (fake *IncusServer) OnProjectUpdate(hook func(map[string]any)) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.projectUpdateHook = hook
}

// updateProject accepts only the If-Match ETag of the current project.
func (fake *IncusServer) updateProject(writer http.ResponseWriter, request *http.Request) {
	name := strings.TrimPrefix(request.URL.Path, "/1.0/projects/")
	var input struct {
		Config map[string]string `json:"config"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1024*1024)).Decode(&input); err != nil {
		writeIncusError(writer, http.StatusBadRequest, "invalid project request")
		return
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	project, ok := fake.projects[name]
	if !ok {
		writeIncusError(writer, http.StatusNotFound, "project not found")
		return
	}
	if fake.projectUpdateHook != nil {
		fake.projectUpdateHook(project)
	}
	if match := request.Header.Get("If-Match"); match == "" || match != incusServerETag(project) {
		writeIncusError(writer, http.StatusPreconditionFailed, "ETag doesn't match")
		return
	}
	project["config"] = cloneStringMap(input.Config)
	fake.projectUpdates = append(fake.projectUpdates, IncusServerConfigUpdate{Project: name, Name: name, Config: cloneStringMap(input.Config)})
	writeIncusSync(writer, map[string]any{})
}

func incusServerETag(instance map[string]any) string {
	encoded, _ := json.Marshal(instance)
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func incusServerConfig(value any) map[string]string {
	config := map[string]string{}
	switch typed := value.(type) {
	case map[string]string:
		maps.Copy(config, typed)
	case map[string]any:
		for key, entry := range typed {
			if text, ok := entry.(string); ok {
				config[key] = text
			}
		}
	}
	return config
}

// updateInstance accepts only the If-Match ETag of the current instance.
// Expanded configuration follows the local keys that the request changes.
func (fake *IncusServer) updateInstance(writer http.ResponseWriter, request *http.Request) {
	name := strings.TrimPrefix(request.URL.Path, "/1.0/instances/")
	project := request.URL.Query().Get("project")
	var input struct {
		Config map[string]string `json:"config"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1024*1024)).Decode(&input); err != nil {
		writeIncusError(writer, http.StatusBadRequest, "invalid instance request")
		return
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	instance, ok := fake.instances[project+"/"+name]
	if !ok {
		writeIncusError(writer, http.StatusNotFound, "instance not found")
		return
	}
	if fake.updateHook != nil {
		if status, message := fake.updateHook(instance); status != 0 {
			writeIncusError(writer, status, message)
			return
		}
	}
	if match := request.Header.Get("If-Match"); match == "" || match != incusServerETag(instance) {
		writeIncusError(writer, http.StatusPreconditionFailed, "ETag doesn't match")
		return
	}
	local, expanded := incusServerConfig(instance["config"]), incusServerConfig(instance["expanded_config"])
	for key := range local {
		if _, kept := input.Config[key]; !kept {
			delete(expanded, key)
		}
	}
	maps.Copy(expanded, input.Config)
	instance["config"], instance["expanded_config"] = cloneStringMap(input.Config), expanded
	fake.configUpdates = append(fake.configUpdates, IncusServerConfigUpdate{Project: project, Name: name, Config: cloneStringMap(input.Config)})
	fake.nextOp++
	id := fmt.Sprintf("operation-%d", fake.nextOp)
	stdinDone := make(chan struct{})
	close(stdinDone)
	operation := &incusOperation{
		id: id, cancelled: make(chan struct{}), stdinDone: stdinDone, callIndex: -1,
		resource: map[string][]string{"instances": {"/1.0/instances/" + name + "?project=" + project}},
	}
	fake.operations[id] = operation
	writeIncusAsync(writer, "/1.0/operations/"+id, operationMetadata(operation, "Running", 103, "", nil))
}

func (fake *IncusServer) setPower(writer http.ResponseWriter, request *http.Request) {
	name := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/1.0/instances/"), "/state")
	project := request.URL.Query().Get("project")
	var input struct {
		Action string `json:"action"`
		Force  bool   `json:"force"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64*1024)).Decode(&input); err != nil {
		writeIncusError(writer, http.StatusBadRequest, "invalid state request")
		return
	}
	fake.mu.Lock()
	instance, ok := fake.instances[project+"/"+name]
	if !ok {
		fake.mu.Unlock()
		writeIncusError(writer, http.StatusNotFound, "instance not found")
		return
	}
	fake.powerCalls = append(fake.powerCalls, IncusServerPowerCall{
		Project: project, Name: name, Action: input.Action, Force: input.Force,
	})
	var operationError string
	if len(fake.powerErrors) > 0 {
		operationError, fake.powerErrors = fake.powerErrors[0], fake.powerErrors[1:]
	}
	if operationError == "" {
		if input.Action == "start" {
			instance["status"] = "Running"
		} else {
			instance["status"] = "Stopped"
		}
	}
	fake.nextOp++
	id := fmt.Sprintf("operation-%d", fake.nextOp)
	stdinDone := make(chan struct{})
	close(stdinDone)
	operation := &incusOperation{
		id: id, cancelled: make(chan struct{}), stdinDone: stdinDone, callIndex: -1,
		step:     IncusServerExecStep{OperationError: operationError},
		resource: map[string][]string{"instances": {"/1.0/instances/" + name + "?project=" + project}},
	}
	fake.operations[id] = operation
	fake.mu.Unlock()
	writeIncusAsync(writer, "/1.0/operations/"+id,
		operationMetadata(operation, "Running", 103, "", nil))
}

func (fake *IncusServer) EventQuery() string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.eventQuery
}

func (fake *IncusServer) startExec(writer http.ResponseWriter, request *http.Request) {
	name := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/1.0/instances/"), "/exec")
	var input struct {
		Command     []string          `json:"command"`
		Environment map[string]string `json:"environment"`
		User        uint32            `json:"user"`
		Group       uint32            `json:"group"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64*1024)).Decode(&input); err != nil {
		writeIncusError(writer, http.StatusBadRequest, "invalid exec request")
		return
	}
	call := IncusServerExecCall{Project: request.URL.Query().Get("project"), Name: name, Command: append([]string(nil), input.Command...), Environment: cloneStringMap(input.Environment), User: input.User, Group: input.Group}
	fake.mu.Lock()
	responder := fake.execResponder
	fake.mu.Unlock()
	var step IncusServerExecStep
	if responder != nil {
		step = responder(call)
	}
	if responder == nil || step.Queued {
		fake.mu.Lock()
		if len(fake.execSteps) == 0 {
			fake.mu.Unlock()
			writeIncusError(writer, http.StatusInternalServerError, "fake exec steps exhausted")
			return
		}
		step = fake.execSteps[0]
		fake.execSteps = fake.execSteps[1:]
		fake.mu.Unlock()
	}
	if step.StartError != "" {
		writeIncusError(writer, http.StatusBadRequest, step.StartError)
		return
	}
	fake.mu.Lock()
	fake.nextOp++
	id := fmt.Sprintf("operation-%d", fake.nextOp)
	fake.execCalls = append(fake.execCalls, call)
	operation := &incusOperation{
		id: id, step: step, callIndex: len(fake.execCalls) - 1,
		cancelled: make(chan struct{}), stdinDone: make(chan struct{}),
		resource: map[string][]string{"instances": {"/1.0/instances/" + name + "?project=" + request.URL.Query().Get("project")}},
	}
	fake.operations[id] = operation
	fake.mu.Unlock()
	select {
	case fake.execStarted <- struct{}{}:
	default:
	}

	fds := map[string]string{
		"0": id + "-stdin", "1": id + "-stdout", "2": id + "-stderr",
	}
	writeIncusAsync(writer, "/1.0/operations/"+id, operationMetadata(operation, "Running", 103, "", fds))
}

func (fake *IncusServer) serveOperation(writer http.ResponseWriter, request *http.Request) {
	remainder := strings.TrimPrefix(request.URL.Path, "/1.0/operations/")
	id, suffix, _ := strings.Cut(remainder, "/")
	fake.mu.Lock()
	operation := fake.operations[id]
	fake.mu.Unlock()
	if operation == nil {
		writeIncusError(writer, http.StatusNotFound, "operation not found")
		return
	}
	switch {
	case request.Method == http.MethodGet && suffix == "":
		operation.mu.Lock()
		terminal := operation.terminal
		operation.mu.Unlock()
		if terminal != nil {
			writeIncusSync(writer, terminal)
		} else {
			writeIncusSync(writer, operationMetadata(operation, "Running", 103, "", nil))
		}
	case request.Method == http.MethodGet && suffix == "websocket":
		fake.serveOperationWebsocket(writer, request, operation)
	case request.Method == http.MethodGet && suffix == "wait":
		fake.waitOperation(writer, request, operation)
	case request.Method == http.MethodDelete && suffix == "":
		operation.cancel.Do(func() { close(operation.cancelled) })
		writeIncusSync(writer, map[string]any{})
	default:
		writeIncusError(writer, http.StatusNotFound, "operation endpoint not found")
	}
}

func (fake *IncusServer) serveOperationWebsocket(
	writer http.ResponseWriter,
	request *http.Request,
	operation *incusOperation,
) {
	secret := request.URL.Query().Get("secret")
	prefix := operation.id + "-"
	if !strings.HasPrefix(secret, prefix) {
		writeIncusError(writer, http.StatusForbidden, "invalid operation secret")
		return
	}
	stream := strings.TrimPrefix(secret, prefix)
	connection, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	switch stream {
	case "stdin":
		var stdin bytes.Buffer
		for {
			messageType, payload, err := connection.ReadMessage()
			if err != nil {
				break
			}
			_, _ = stdin.Write(payload)
			if messageType == websocket.TextMessage {
				break
			}
		}
		fake.mu.Lock()
		fake.execCalls[operation.callIndex].Stdin = bytes.Clone(stdin.Bytes())
		call := fake.execCalls[operation.callIndex]
		fake.mu.Unlock()
		if operation.step.AfterInput != nil {
			operation.step.ExitCode = operation.step.AfterInput(call)
		}
		close(operation.stdinDone)
	case "stdout", "stderr":
		output := operation.step.Stdout
		if stream == "stderr" {
			output = operation.step.Stderr
		}
		_ = connection.WriteMessage(websocket.BinaryMessage, output)
		// The client closes stdin when both output streams finish.
		select {
		case <-operation.stdinDone:
		case <-operation.cancelled:
			return
		case <-request.Context().Done():
			return
		}
		_ = connection.WriteMessage(websocket.TextMessage, nil)
	default:
		_ = connection.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "unknown stream"), time.Now().Add(time.Second))
	}
}

func (fake *IncusServer) waitOperation(
	writer http.ResponseWriter,
	request *http.Request,
	operation *incusOperation,
) {
	final := func(status string, code int, message string) {
		metadata := operationMetadata(operation, status, code, message, nil)
		operation.mu.Lock()
		operation.terminal = metadata
		operation.mu.Unlock()
		writeIncusSync(writer, metadata)
	}
	select {
	case <-operation.stdinDone:
	case <-operation.cancelled:
		final("Cancelled", 401, "operation cancelled")
		return
	case <-request.Context().Done():
		return
	}
	if operation.step.Release != nil {
		select {
		case <-operation.step.Release:
		case <-operation.cancelled:
			final("Cancelled", 401, "operation cancelled")
			return
		case <-request.Context().Done():
			return
		}
	}
	select {
	case <-operation.cancelled:
		final("Cancelled", 401, "operation cancelled")
		return
	default:
	}
	if operation.step.OperationError != "" {
		final("Failure", 400, operation.step.OperationError)
		return
	}
	final("Success", 200, "")
}

func operationMetadata(
	operation *incusOperation,
	status string,
	statusCode int,
	errorMessage string,
	fds map[string]string,
) map[string]any {
	metadata := map[string]any{"return": operation.step.ExitCode}
	if fds != nil {
		metadata["fds"] = fds
	}
	return map[string]any{
		"id": operation.id, "class": "websocket", "description": "Executing command",
		"created_at": time.Unix(100, 0).UTC(), "updated_at": time.Unix(100, 0).UTC(),
		"status": status, "status_code": statusCode, "resources": operation.resource,
		"metadata": metadata, "may_cancel": true, "err": errorMessage,
	}
}

func (fake *IncusServer) serveEvents(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Query().Get("all-projects") != "true" {
		writeIncusError(writer, http.StatusBadRequest, "all-projects event subscription required")
		return
	}
	connection, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
	if err != nil {
		return
	}
	fake.mu.Lock()
	fake.connections[connection] = struct{}{}
	fake.mu.Unlock()
	select {
	case fake.connected <- struct{}{}:
	default:
	}
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			break
		}
	}
	fake.mu.Lock()
	delete(fake.connections, connection)
	fake.mu.Unlock()
	_ = connection.Close()
}

func writeIncusSync(writer http.ResponseWriter, metadata any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"type": "sync", "status": "Success", "status_code": http.StatusOK, "metadata": metadata,
	})
}

func writeIncusAsync(writer http.ResponseWriter, operation string, metadata any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"type": "async", "status": "Operation created", "status_code": http.StatusAccepted,
		"operation": operation, "metadata": metadata,
	})
}

func writeIncusError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"type": "error", "error": message, "error_code": status, "status": fmt.Sprintf("Failure: %s", message),
	})
}

func cloneAnyMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

// SetExecResponder selects canned responses from captured request metadata.
// AfterInput may validate input and choose an exit; no guest command is run.
func (fake *IncusServer) SetExecResponder(responder func(IncusServerExecCall) IncusServerExecStep) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.execResponder = responder
}
