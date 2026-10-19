package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/domain"
)

const brokerMetadataPath = "prepared/broker-original.json"

type brokerRecipe struct {
	Version                        int
	Binding, Source, Runtime, Sink string
	Power                          domain.DesiredPower
	Apply                          testvmsruntime.PreparedApplyRecord
	EngineParts                    []string
}

func (p *PreparedInstalledRuntime) PreparedBroker() *PreparedTestVMBroker { return p.broker }

func (p *PreparedInstalledRuntime) BrokerSourceBinding() string {
	if p == nil || p.owner != "test-vm-broker" {
		return ""
	}
	return brokerSourceBinding(p.files[brokerMetadataPath])
}

func brokerSourceBinding(raw []byte) string {
	var d brokerRecipe
	if json.Unmarshal(raw, &d) != nil {
		return ""
	}
	return installedRuntimeDigest(struct {
		Source, Inputs string
		State          [][]byte
		Payloads       map[string]string
		Power          domain.DesiredPower
	}{d.Source, d.Apply.Inputs, d.Apply.State, d.Apply.Payloads, d.Power})
}

func (runtime Runtime) PrepareTestVMBrokerRecipe(ctx context.Context, broker *PreparedTestVMBroker) (*PreparedInstalledRuntime, error) {
	if broker == nil || broker.runtime != runtime.configRuntimeBinding() {
		return nil, domain.ErrPlanStale
	}
	p, err := runtime.PrepareHostInstallerSources(ctx, "test-vms-host-sink")
	if err != nil {
		return nil, err
	}
	p.owner = "test-vm-broker"
	p.handler = ""
	p.leaves = nil
	p.broker = broker
	record, files, err := broker.apply.Record(broker.backend)
	if err != nil {
		return nil, err
	}
	d := brokerRecipe{Version: 1, Binding: broker.binding, Source: broker.source, Runtime: broker.runtime, Sink: broker.sink, Power: broker.power, Apply: record}
	for offset, index := 0, 0; offset < len(files["engine"]); index++ {
		end := min(len(files["engine"]), offset+(32<<20))
		path := fmt.Sprintf("prepared/broker/engine/%04d", index)
		p.files[path] = files["engine"][offset:end]
		d.EngineParts = append(d.EngineParts, path)
		offset = end
	}
	for _, name := range []string{"provision", "download", "recipes", "manifest"} {
		p.files["prepared/broker/"+name] = files[name]
	}
	p.files[brokerMetadataPath], err = json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if err := checkMaterializedRecipeSize(p); err != nil {
		return nil, err
	}
	p.executionFiles = map[string][]byte{}
	for path, payload := range p.files {
		if path != brokerMetadataPath && !slices.Contains(d.EngineParts, path) && path != "prepared/broker/provision" && path != "prepared/broker/download" && path != "prepared/broker/recipes" && path != "prepared/broker/manifest" {
			p.executionFiles[path] = payload
		}
	}
	if _, _, err := p.stageUsage(); err != nil {
		return nil, err
	}
	p.binding = installedRuntimeDigest(json.RawMessage(p.Recipe()))
	return p, nil
}

func (runtime Runtime) bindBrokerRecipe(ctx context.Context, r installedRecipe, raw []byte) (*PreparedInstalledRuntime, error) {
	if r.Profile != "" || r.Handler != "" || r.Launcher != "" || r.HookBefore != "" || len(r.Leaves) != 0 || len(r.Native) != 0 || r.Runtime != runtime.configRuntimeBinding() || r.Resources != runtime.installedResourceBinding() {
		return nil, domain.ErrPlanStale
	}
	p := &PreparedInstalledRuntime{owner: r.Owner, source: r.Source, runtime: r.Runtime, resources: r.Resources, files: map[string][]byte{}, executionFiles: map[string][]byte{}, afterOwner: r.AfterOwner}
	previous := ""
	for _, f := range r.Files {
		if f.Path <= previous {
			return nil, errors.New("broker recipe file order")
		}
		previous = f.Path
		p.files[f.Path] = f.Content
	}
	var d brokerRecipe
	decoder := json.NewDecoder(bytes.NewReader(p.files[brokerMetadataPath]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&d); err != nil {
		return nil, err
	}
	canonical, _ := json.Marshal(d)
	if !bytes.Equal(canonical, p.files[brokerMetadataPath]) || d.Version != 1 || d.Source != p.source || d.Runtime != p.runtime {
		return nil, domain.ErrPlanStale
	}
	if runtime.afterOwnerBroker != nil {
		value := runtime.afterOwnerBroker
		d.Apply.Native = value.Native
		d.Apply.Binding = value.ApplyBinding
		d.Sink = value.Sink
		d.Binding = value.Binding
	}
	_, paths, err := runtime.hostInstallerSourcePaths("test-vms-host-sink")
	if err != nil {
		return nil, err
	}
	if len(p.files) != len(paths)+5+len(d.EngineParts) || len(d.EngineParts) == 0 && d.Apply.EngineRole == "" || len(d.EngineParts) > 4 || len(d.EngineParts) != 0 && d.Apply.EngineRole != "" {
		return nil, domain.ErrPlanStale
	}
	for _, path := range paths {
		payload, ok := p.files[path]
		if !ok {
			return nil, domain.ErrPlanStale
		}
		p.executionFiles[path] = payload
	}
	for index, path := range d.EngineParts {
		if path != fmt.Sprintf("prepared/broker/engine/%04d", index) || len(p.files[path]) == 0 || len(p.files[path]) > 32<<20 || index+1 < len(d.EngineParts) && len(p.files[path]) != 32<<20 {
			return nil, domain.ErrPlanStale
		}
	}
	for path := range p.files {
		if path != brokerMetadataPath && !slices.Contains(paths, path) && !slices.Contains(d.EngineParts, path) && path != "prepared/broker/provision" && path != "prepared/broker/download" && path != "prepared/broker/recipes" && path != "prepared/broker/manifest" {
			return nil, domain.ErrPlanStale
		}
	}
	if source, err := runtime.SelectedRuntimeInputs("core-guest-runtime"); err != nil || source != p.source {
		return nil, domain.ErrPlanStale
	}
	backend, err := runtime.testVMBackend(d.Power)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, name := range []string{"provision", "download", "recipes", "manifest"} {
		files[name] = p.files["prepared/broker/"+name]
	}
	for _, path := range d.EngineParts {
		files["engine"] = append(files["engine"], p.files[path]...)
	}
	apply, err := backend.RestorePreparedApply(ctx, d.Apply, files)
	if err != nil {
		return nil, err
	}
	broker := &PreparedTestVMBroker{backend: backend, apply: apply, power: d.Power, source: d.Source, runtime: d.Runtime, sink: d.Sink, binding: d.Binding}
	derived := installedRuntimeDigest(struct {
		Native, Source, Runtime, Sink string
		Power                         domain.DesiredPower
	}{apply.Binding(), d.Source, d.Runtime, d.Sink, d.Power})
	if derived != d.Binding {
		apply.Close()
		return nil, domain.ErrPlanStale
	}
	p.broker = broker
	if err := checkMaterializedRecipeSize(p); err != nil {
		apply.Close()
		return nil, err
	}
	if _, _, err := p.stageUsage(); err != nil {
		apply.Close()
		return nil, err
	}
	p.binding = installedRuntimeDigest(json.RawMessage(raw))
	return p, nil
}

func (runtime Runtime) ApplyPreparedBrokerRecipe(ctx context.Context, p *PreparedInstalledRuntime) error {
	if p == nil || p.owner != "test-vm-broker" || p.broker == nil || p.runtime != runtime.configRuntimeBinding() || p.resources != runtime.installedResourceBinding() {
		return domain.ErrPlanStale
	}
	if source, err := runtime.SelectedRuntimeInputs("core-guest-runtime"); err != nil || source != p.source {
		return domain.ErrPlanStale
	}
	execution, err := runtime.preparedSourceExecution(ctx, p)
	if err != nil {
		return err
	}
	defer p.Close()
	err = runtime.applyPreparedTestVMBrokerRuntime(ctx, p.broker, execution)
	if err != nil && p.stageBinding != "" {
		p.retain.Store(true)
	}
	return err
}
