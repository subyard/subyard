// Package testkit contains deterministic, side-effect-free implementations of
// application ports. Production packages must never import it.
package testkit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type MemoryState struct {
	mu      sync.RWMutex
	records map[string]domain.ProjectRecord
	Err     error
}

func NewMemoryState(records ...domain.ProjectRecord) *MemoryState {
	state := &MemoryState{records: make(map[string]domain.ProjectRecord)}
	for _, record := range records {
		state.records[record.ProjectID] = record
	}
	return state
}

func (state *MemoryState) List(context.Context) ([]domain.ProjectRecord, error) {
	state.mu.RLock()
	defer state.mu.RUnlock()
	if state.Err != nil {
		return nil, state.Err
	}
	result := make([]domain.ProjectRecord, 0, len(state.records))
	for _, record := range state.records {
		result = append(result, record)
	}
	slices.SortFunc(result, func(left, right domain.ProjectRecord) int {
		if left.ProjectID < right.ProjectID {
			return -1
		}
		if left.ProjectID > right.ProjectID {
			return 1
		}
		return 0
	})
	return result, nil
}

func (state *MemoryState) Get(_ context.Context, id string) (domain.ProjectRecord, error) {
	state.mu.RLock()
	defer state.mu.RUnlock()
	if state.Err != nil {
		return domain.ProjectRecord{}, state.Err
	}
	record, ok := state.records[id]
	if !ok {
		return domain.ProjectRecord{}, errors.New("not found")
	}
	return record, nil
}

func (state *MemoryState) Put(_ context.Context, record domain.ProjectRecord) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.Err != nil {
		return state.Err
	}
	state.records[record.ProjectID] = record
	return nil
}

func (state *MemoryState) Delete(_ context.Context, id string) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.Err != nil {
		return state.Err
	}
	delete(state.records, id)
	return nil
}

type ManualClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []clockWaiter
}

type clockWaiter struct {
	at      time.Time
	channel chan time.Time
}

func NewManualClock(now time.Time) *ManualClock { return &ManualClock{now: now} }

func (clock *ManualClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *ManualClock) After(delay time.Duration) <-chan time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	channel := make(chan time.Time, 1)
	clock.waiters = append(clock.waiters, clockWaiter{at: clock.now.Add(delay), channel: channel})
	return channel
}

func (clock *ManualClock) Advance(delay time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(delay)
	pending := clock.waiters[:0]
	for _, waiter := range clock.waiters {
		if !waiter.at.After(clock.now) {
			waiter.channel <- clock.now
			close(waiter.channel)
		} else {
			pending = append(pending, waiter)
		}
	}
	clock.waiters = pending
}

type IDs struct {
	mu     sync.Mutex
	Values []string
}

func (source *IDs) NewID() string {
	source.mu.Lock()
	defer source.mu.Unlock()
	if len(source.Values) == 0 {
		panic("testkit IDs exhausted")
	}
	value := source.Values[0]
	source.Values = source.Values[1:]
	return value
}

type Prompt struct {
	Answers  []bool
	Err      error
	Seen     []string
	Requests []domain.ConfirmationRequest
}

func (prompt *Prompt) Confirm(_ context.Context, request domain.ConfirmationRequest) (bool, error) {
	prompt.Seen = append(prompt.Seen, request.Summary)
	request.Consequences = append([]string(nil), request.Consequences...)
	prompt.Requests = append(prompt.Requests, request)
	if prompt.Err != nil {
		return false, prompt.Err
	}
	if len(prompt.Answers) == 0 {
		return false, errors.New("testkit prompt answers exhausted")
	}
	answer := prompt.Answers[0]
	prompt.Answers = prompt.Answers[1:]
	return answer, nil
}

type AdapterStep struct {
	Result domain.AdapterResult
	Stderr string
	Err    error
}

type ScriptedAdapter struct {
	mu       sync.Mutex
	Steps    []AdapterStep
	Requests []domain.AdapterRequest
	Secrets  [][]byte
}

func (adapter *ScriptedAdapter) Run(_ context.Context, request domain.AdapterRequest, secret io.Reader) (domain.AdapterResult, string, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.Requests = append(adapter.Requests, request)
	var protected []byte
	if secret != nil {
		protected, _ = io.ReadAll(secret)
	}
	adapter.Secrets = append(adapter.Secrets, protected)
	if len(adapter.Steps) == 0 {
		return domain.AdapterResult{}, "", errors.New("testkit adapter steps exhausted")
	}
	step := adapter.Steps[0]
	adapter.Steps = adapter.Steps[1:]
	return step.Result, step.Stderr, step.Err
}

type Incus struct {
	ServerInfo    ports.ServerInfo
	Instances     map[string]ports.InstanceInfo
	Reconcile     ports.ReconcileState
	EventsOut     chan domain.OperationEvent
	ErrorsOut     chan error
	Err           error
	ExecSteps     []IncusExecStep
	ExecCalls     []IncusExecCall
	ConfigUpdates []InstanceConfigUpdate
	PowerUpdates  []InstancePowerUpdate
}

type InstanceConfigUpdate struct {
	Project string
	Name    string
	Values  map[string]string
}

type InstancePowerUpdate struct {
	Project string
	Name    string
	Action  string
	Force   bool
}

type IncusExecStep struct {
	Result ports.InstanceExecResult
	Err    error
}

type IncusExecCall struct {
	Project string
	Name    string
	Request ports.InstanceExecRequest
}

func (fake *Incus) Server(context.Context) (ports.ServerInfo, error) {
	return fake.ServerInfo, fake.Err
}

func (fake *Incus) Instance(_ context.Context, project, name string) (ports.InstanceInfo, error) {
	if fake.Err != nil {
		return ports.InstanceInfo{}, fake.Err
	}
	instance, ok := fake.Instances[project+"/"+name]
	if !ok {
		return ports.InstanceInfo{}, ports.ErrInstanceNotFound
	}
	return instance, nil
}

func (fake *Incus) ListInstances(context.Context) ([]ports.InstanceInfo, error) {
	if fake.Err != nil {
		return nil, fake.Err
	}
	instances := make([]ports.InstanceInfo, 0, len(fake.Instances))
	for _, instance := range fake.Instances {
		instances = append(instances, instance)
	}
	return instances, nil
}

func (fake *Incus) SetInstancePower(
	_ context.Context,
	project string,
	name string,
	action string,
	force bool,
) error {
	if fake.Err != nil {
		return fake.Err
	}
	key := project + "/" + name
	instance, ok := fake.Instances[key]
	if !ok {
		return ports.ErrInstanceNotFound
	}
	if action != "start" && action != "stop" {
		return errors.New("invalid instance power action")
	}
	fake.PowerUpdates = append(fake.PowerUpdates, InstancePowerUpdate{
		Project: project, Name: name, Action: action, Force: force,
	})
	if action == "start" {
		instance.Status = "Running"
	} else {
		instance.Status = "Stopped"
	}
	fake.Instances[key] = instance
	return nil
}

func (fake *Incus) ReconcileState(
	context.Context, string, string, string, string, string,
) (ports.ReconcileState, error) {
	return fake.Reconcile, fake.Err
}

func (fake *Incus) SetInstanceConfig(
	_ context.Context,
	project string,
	name string,
	values map[string]string,
) error {
	if fake.Err != nil {
		return fake.Err
	}
	copyValues := make(map[string]string, len(values))
	for key, value := range values {
		copyValues[key] = value
	}
	fake.ConfigUpdates = append(fake.ConfigUpdates, InstanceConfigUpdate{
		Project: project, Name: name, Values: copyValues,
	})
	key := project + "/" + name
	instance, exists := fake.Instances[key]
	if !exists {
		return errors.New("instance not found")
	}
	if instance.LocalConfig == nil {
		instance.LocalConfig = make(map[string]string)
	}
	if instance.Config == nil {
		instance.Config = make(map[string]string)
	}
	for field, value := range values {
		instance.LocalConfig[field] = value
		instance.Config[field] = value
	}
	fake.Instances[key] = instance
	if fake.Reconcile.Instance.Name == name && fake.Reconcile.Instance.Project == project {
		fake.Reconcile.Instance = instance
	}
	return nil
}

func (fake *Incus) Events(context.Context, []string) (<-chan domain.OperationEvent, <-chan error) {
	if fake.EventsOut == nil {
		fake.EventsOut = make(chan domain.OperationEvent)
		close(fake.EventsOut)
	}
	if fake.ErrorsOut == nil {
		fake.ErrorsOut = make(chan error)
		close(fake.ErrorsOut)
	}
	return fake.EventsOut, fake.ErrorsOut
}

func (fake *Incus) Exec(
	_ context.Context,
	project string,
	name string,
	request ports.InstanceExecRequest,
) (ports.InstanceExecResult, error) {
	fake.ExecCalls = append(fake.ExecCalls, IncusExecCall{Project: project, Name: name, Request: request})
	if len(fake.ExecSteps) == 0 {
		return ports.InstanceExecResult{}, errors.New("testkit Incus exec steps exhausted")
	}
	step := fake.ExecSteps[0]
	fake.ExecSteps = fake.ExecSteps[1:]
	return step.Result, step.Err
}

type Audit struct {
	mu     sync.Mutex
	Events []domain.OperationEvent
	Err    error
}

func (sink *Audit) WriteAudit(_ context.Context, event domain.OperationEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.Events = append(sink.Events, event)
	return sink.Err
}

type Events struct{ Audit }

func (sink *Events) Publish(ctx context.Context, event domain.OperationEvent) error {
	return sink.WriteAudit(ctx, event)
}

type CredentialMetadataReader struct {
	Metadata []domain.CredentialMetadata
	Err      error
}

func (reader *CredentialMetadataReader) ListMetadata(context.Context) ([]domain.CredentialMetadata, error) {
	return slices.Clone(reader.Metadata), reader.Err
}

type CredentialCrypto struct {
	Payloads map[string][]byte
	Err      error
}

func (crypto *CredentialCrypto) Decrypt(_ context.Context, metadata domain.CredentialMetadata, writer io.Writer) error {
	if crypto.Err != nil {
		return crypto.Err
	}
	_, err := writer.Write(crypto.Payloads[metadata.RevisionID])
	return err
}

func (crypto *CredentialCrypto) Verify(context.Context, domain.CredentialMetadata) error {
	return crypto.Err
}

type RemoteStep struct {
	Response []byte
	Err      error
}

type RemoteCall struct {
	Target  string
	Request []byte
}

type ScriptedRemote struct {
	mu    sync.Mutex
	Steps []RemoteStep
	Calls []RemoteCall
}

func (remote *ScriptedRemote) Call(ctx context.Context, target string, request []byte) ([]byte, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	remote.Calls = append(remote.Calls, RemoteCall{Target: target, Request: bytes.Clone(request)})
	if len(remote.Steps) == 0 {
		return nil, errors.New("testkit remote steps exhausted")
	}
	step := remote.Steps[0]
	remote.Steps = remote.Steps[1:]
	return bytes.Clone(step.Response), step.Err
}
