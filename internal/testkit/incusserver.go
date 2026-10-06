package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	mu          sync.Mutex
	serverInfo  map[string]any
	instances   map[string]map[string]any
	projects    map[string]map[string]any
	connections map[*websocket.Conn]struct{}
	connected   chan struct{}
	execStarted chan struct{}
	execSteps   []IncusServerExecStep
	execCalls   []IncusServerExecCall
	powerCalls  []IncusServerPowerCall
	powerErrors []string
	operations  map[string]*incusOperation
	eventQuery  string
	nextOp      int
}

type IncusServerExecStep struct {
	Stdout         []byte
	Stderr         []byte
	ExitCode       int
	StartError     string
	OperationError string
	Release        <-chan struct{}
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

type incusOperation struct {
	id        string
	step      IncusServerExecStep
	callIndex int
	cancelled chan struct{}
	stdinDone chan struct{}
	cancel    sync.Once
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

func (fake *IncusServer) SetInstance(project, name string, instance map[string]any) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.instances[project+"/"+name] = cloneAnyMap(instance)
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
	case strings.HasPrefix(request.URL.Path, "/1.0/operations/"):
		fake.serveOperation(writer, request)
	case request.Method == http.MethodGet && request.URL.Path == "/1.0/instances":
		fake.listInstances(writer, request)
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
		writeIncusSync(writer, metadata)
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
	if request.URL.Query().Get("all-projects") != "true" {
		writeIncusError(writer, http.StatusBadRequest, "all-projects is required")
		return
	}
	fake.mu.Lock()
	instances := make([]map[string]any, 0, len(fake.instances))
	for key, source := range fake.instances {
		instance := cloneAnyMap(source)
		project, _, _ := strings.Cut(key, "/")
		if _, ok := instance["project"]; !ok {
			instance["project"] = project
		}
		instances = append(instances, instance)
	}
	fake.mu.Unlock()
	writeIncusSync(writer, instances)
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
		step: IncusServerExecStep{OperationError: operationError},
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
	fake.mu.Lock()
	if len(fake.execSteps) == 0 {
		fake.mu.Unlock()
		writeIncusError(writer, http.StatusInternalServerError, "fake exec steps exhausted")
		return
	}
	step := fake.execSteps[0]
	fake.execSteps = fake.execSteps[1:]
	if step.StartError != "" {
		fake.mu.Unlock()
		writeIncusError(writer, http.StatusBadRequest, step.StartError)
		return
	}
	fake.nextOp++
	id := fmt.Sprintf("operation-%d", fake.nextOp)
	call := IncusServerExecCall{
		Project: request.URL.Query().Get("project"), Name: name,
		Command: append([]string(nil), input.Command...), Environment: cloneStringMap(input.Environment),
		User: input.User, Group: input.Group,
	}
	fake.execCalls = append(fake.execCalls, call)
	operation := &incusOperation{
		id: id, step: step, callIndex: len(fake.execCalls) - 1,
		cancelled: make(chan struct{}), stdinDone: make(chan struct{}),
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
		fake.mu.Unlock()
		close(operation.stdinDone)
	case "stdout":
		_ = connection.WriteMessage(websocket.BinaryMessage, operation.step.Stdout)
		_ = connection.WriteMessage(websocket.TextMessage, nil)
	case "stderr":
		_ = connection.WriteMessage(websocket.BinaryMessage, operation.step.Stderr)
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
	select {
	case <-operation.stdinDone:
	case <-operation.cancelled:
		writeIncusSync(writer, operationMetadata(operation, "Cancelled", 401, "operation cancelled", nil))
		return
	case <-request.Context().Done():
		return
	}
	if operation.step.Release != nil {
		select {
		case <-operation.step.Release:
		case <-operation.cancelled:
			writeIncusSync(writer, operationMetadata(operation, "Cancelled", 401, "operation cancelled", nil))
			return
		case <-request.Context().Done():
			return
		}
	}
	select {
	case <-operation.cancelled:
		writeIncusSync(writer, operationMetadata(operation, "Cancelled", 401, "operation cancelled", nil))
		return
	default:
	}
	if operation.step.OperationError != "" {
		writeIncusSync(writer, operationMetadata(operation, "Failure", 400, operation.step.OperationError, nil))
		return
	}
	writeIncusSync(writer, operationMetadata(operation, "Success", 200, "", nil))
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
		"status": status, "status_code": statusCode, "resources": map[string][]string{},
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
