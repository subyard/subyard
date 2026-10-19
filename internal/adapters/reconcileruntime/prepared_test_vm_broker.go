package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/domain"
)

type PreparedTestVMBroker struct {
	backend                        *testvmsruntime.Backend
	apply                          *testvmsruntime.PreparedApply
	power                          domain.DesiredPower
	source, runtime, sink, binding string
}

func (p *PreparedTestVMBroker) Binding() string {
	if p == nil {
		return ""
	}
	return p.binding
}
func (p *PreparedTestVMBroker) Close() error {
	if p == nil {
		return nil
	}
	return p.apply.Close()
}

func (runtime Runtime) brokerSinkFiles() (string, error) {
	return nativeInstalledFiles([]string{
		runtime.environmentDefault("SUBYARD_TEST_VMS_SINK_PATH", "/usr/local/libexec/subyard/test-vms-host-sink"),
		runtime.environmentDefault("SUBYARD_TEST_VMS_SINK_SERVICE_PATH", "/etc/systemd/system/subyard-test-vms-host-sink.service"),
		runtime.environmentDefault("SUBYARD_TEST_VMS_SINK_TIMER_PATH", "/etc/systemd/system/subyard-test-vms-host-sink.timer"),
	})
}

func (runtime Runtime) PrepareTestVMBrokerRuntime(ctx context.Context) (*PreparedTestVMBroker, error) {
	intent, err := runtime.powerService().Intent(ctx, runtime.Yard)
	if err != nil {
		return nil, err
	}
	backend, err := runtime.testVMBackend(intent.Desired)
	if err != nil {
		return nil, err
	}
	prepared, err := backend.PrepareApply(ctx)
	if err != nil {
		return nil, err
	}
	p := &PreparedTestVMBroker{backend: backend, apply: prepared, power: intent.Desired, runtime: runtime.configRuntimeBinding()}
	ok := false
	defer func() {
		if !ok {
			p.Close()
		}
	}()
	if p.source, err = runtime.SelectedRuntimeInputs("core-guest-runtime"); err != nil {
		return nil, err
	}
	if runtime.Yard.NestedE2EVMs {
		if p.sink, err = runtime.brokerSinkFiles(); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(struct {
		Native, Source, Runtime, Sink string
		Power                         domain.DesiredPower
	}{prepared.Binding(), p.source, p.runtime, p.sink, p.power})
	if err != nil {
		return nil, err
	}
	p.binding = fmt.Sprintf("%x", sha256.Sum256(raw))
	ok = true
	return p, nil
}

func (runtime Runtime) ApplyPreparedTestVMBrokerRuntime(ctx context.Context, prepared *PreparedTestVMBroker) error {
	return runtime.applyPreparedTestVMBrokerRuntime(ctx, prepared)
}
func (runtime Runtime) applyPreparedTestVMBrokerRuntime(ctx context.Context, prepared *PreparedTestVMBroker, cached ...Runtime) error {
	if prepared == nil || prepared.runtime != runtime.configRuntimeBinding() {
		return domain.ErrPlanStale
	}
	source, err := runtime.SelectedRuntimeInputs("core-guest-runtime")
	if err != nil {
		return err
	}
	if source != prepared.source {
		return domain.ErrPlanStale
	}
	intent, err := runtime.powerService().Intent(ctx, runtime.Yard)
	if err != nil {
		return err
	}
	if intent.Desired != prepared.power {
		return domain.ErrPlanStale
	}
	if runtime.Yard.NestedE2EVMs {
		sink, err := runtime.brokerSinkFiles()
		if err != nil {
			return err
		}
		if sink != prepared.sink && !runtime.testVMHostSinkConverged(ctx) {
			return domain.ErrPlanStale
		}
		engine, engineSHA, err := prepared.apply.PinnedEngineSource()
		if err != nil {
			return err
		}
		execution := runtime
		if len(cached) == 1 {
			execution = cached[0]
		}
		if err := execution.runScriptEnvironment(ctx, runtime.Stderr, map[string]string{"SUBYARD_PREPARED_INSTALLED_CLEANUP": "1", "SUBYARD_TEST_VMS_SINK_ENGINE_SOURCE": engine, "SUBYARD_NATIVE_ENGINE_SHA256": engineSHA, "SUBYARD_NATIVE_INSTALLED_BEFORE": sink}, "install-test-vms-host-sink.sh", "--yes"); err != nil {
			return err
		}
	}
	return prepared.backend.ApplyPrepared(ctx, prepared.apply)
}
