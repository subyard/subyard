package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/ports"
)

type initStageObservation struct {
	ports.ReconcileStageObservation
	err      error
	output   bytes.Buffer
	observed bool
}

type initStageObserver struct {
	reconcileruntime.Runtime
	stages  []application.ReconcileStage
	results []initStageObservation
}

func (observer *initStageObserver) ObserveStage(ctx context.Context, id ports.ReconcileStageID) (ports.ReconcileStageObservation, error) {
	index := slices.IndexFunc(observer.stages, func(stage application.ReconcileStage) bool { return stage.ID == id })
	if index < 0 || !observer.results[index].observed {
		return observer.Runtime.ObserveStage(ctx, id)
	}
	result := &observer.results[index]
	if observer.Stderr != nil {
		if _, err := observer.Stderr.Write(result.output.Bytes()); err != nil {
			return ports.ReconcileStageObservation{}, err
		}
	}
	result.output.Reset()
	return result.ReconcileStageObservation, result.err
}

func (cli *CLI) initBatchEligible(ctx context.Context, execution *initExecution) bool {
	runtime, ok := execution.platform.(reconcileruntime.Runtime)
	_, nativeReader := runtime.Incus.(*incusclient.Client)
	_, nativeExecutor := runtime.Executor.(*incusclient.Client)
	return ok && nativeReader && nativeExecutor && cli.options.InitPlatform == nil && cli.options.Incus == nil && cli.options.Executor == nil && cli.options.NetworkPolicy == nil &&
		cli.options.IntegrationRuntime == nil && cli.options.ProfileRuntime == nil && !cli.retainedAdapterCompatibility &&
		!cli.releaseTransitionChild && cli.profileInitRepair == nil && execution.mode == initReconcile && execution.bootstrap == nil && execution.requestedProfile == "" && execution.resourceCommand == "" && execution.provisionProfile == "" && execution.legacyContinuation == nil && !runtime.AdoptLegacyIntegrations &&
		runtime.SourceReader == nil && runtime.PreparedSources == nil && runtime.LegacyContinuation == nil && runtime.ACLToolsAvailable == nil && ports.NativeOperationObserverFrom(ctx) == nil
}

func initReadyStageObserver(ctx context.Context, runtime reconcileruntime.Runtime, stages []application.ReconcileStage) ports.ReconcileStageRunner {
	observer := &initStageObserver{Runtime: runtime, stages: stages, results: make([]initStageObservation, len(stages))}
	observe := func(index int) {
		probe := runtime
		probe.Stdin = nil
		probe.Stdout, probe.Stderr = &observer.results[index].output, &observer.results[index].output
		observer.results[index].ReconcileStageObservation, observer.results[index].err = probe.ObserveStage(ctx, stages[index].ID)
		observer.results[index].observed = true
	}
	prefix := slices.IndexFunc(stages, func(stage application.ReconcileStage) bool { return stage.ID == ports.ReconcileStageMounts })
	if prefix < 0 || slices.ContainsFunc(stages, func(stage application.ReconcileStage) bool { return stage.ID == ports.ReconcileStageProfileRuntimes }) {
		return runtime
	}
	for index := 0; index <= prefix; index++ {
		observe(index)
		if result := observer.results[index]; result.err != nil || !result.Converged {
			return observer
		}
	}
	instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if err != nil || !strings.EqualFold(instance.Status, "running") {
		return observer
	}
	collectInitStageTail(prefix+1, len(stages), func(index int) {
		switch stages[index].ID {
		case ports.ReconcileStageProvision, ports.ReconcileStageProfileServices, ports.ReconcileStageExtras, ports.ReconcileStageKeys:
			observe(index)
		}
	})
	return observer
}

func collectInitStageTail(first, end int, observe func(int)) {
	slots := make(chan struct{}, 3)
	var workers sync.WaitGroup
	for index := first; index < end; index++ {
		slots <- struct{}{}
		workers.Go(func() { defer func() { <-slots }(); observe(index) })
	}
	workers.Wait()
}
