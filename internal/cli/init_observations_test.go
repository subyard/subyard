package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestInitStageTailBoundsWidthAndDrainsCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%t", canceled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started, release, done := make(chan int, 9), make(chan struct{}), make(chan struct{})
			var active, completed atomic.Int32
			go func() {
				collectInitStageTail(0, 9, func(index int) {
					if active.Add(1) > 3 {
						t.Error("stage collection exceeded width three")
					}
					defer active.Add(-1)
					started <- index
					select {
					case <-release:
					case <-ctx.Done():
					}
					completed.Add(1)
				})
				close(done)
			}()
			for range 3 {
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("stage collection did not start three independent reads")
				}
			}
			select {
			case <-started:
				t.Fatal("fourth stage started before a slot became free")
			default:
			}
			if canceled {
				cancel()
			} else {
				close(release)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("stage workers did not drain")
			}
			if active.Load() != 0 || completed.Load() != 9 {
				t.Fatalf("collection returned with live or missing reads: active=%d completed=%d", active.Load(), completed.Load())
			}
		})
	}
}

func TestInitStageObservationsPreserveOrderedErrorsAndConditionalReduction(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%t", pending), func(t *testing.T) {
			stages := application.InitStages(domain.Context{})[7:13]
			var output bytes.Buffer
			observer := &initStageObserver{Runtime: reconcileruntime.Runtime{Stderr: &output}, stages: stages, results: make([]initStageObservation, len(stages))}
			firstFailure, laterFailure := errors.New("first stage failure"), errors.New("later stage failure")
			laterDone := make(chan struct{})
			collectInitStageTail(0, len(stages), func(index int) {
				if index == 1 {
					<-laterDone
				}
				result := &observer.results[index]
				result.observed, result.Converged = true, true
				fmt.Fprintf(&result.output, "stage%d\n", index)
				if index == 1 {
					result.Converged, result.err = false, firstFailure
					if pending {
						result.err = nil
					}
				} else if index == 2 {
					result.err = laterFailure
					close(laterDone)
				}
			})
			plan, err := (application.Reconciler{Stages: stages, Runner: observer}).Plan(context.Background())
			if pending {
				if err != nil || plan.Pending() != len(stages)-1 || !plan.Steps[2].Conditional {
					t.Fatalf("later conditional error escaped: plan=%+v err=%v", plan, err)
				}
			} else if !errors.Is(err, firstFailure) || errors.Is(err, laterFailure) || !strings.Contains(err.Error(), `"`+string(stages[1].ID)+`"`) {
				t.Fatalf("completion order changed the first stage failure: %v", err)
			}
			if output.String() != "stage0\nstage1\n" {
				t.Fatalf("stage diagnostics escaped ordered reduction: %q", output.String())
			}
		})
	}
}

func TestInitReadyStageObserverKeepsColdAndStoppedTailSerial(t *testing.T) {
	for _, status := range []string{"absent", "stopped"} {
		t.Run(status, func(t *testing.T) {
			root := testkit.TempDir(t)
			if err := os.Mkdir(filepath.Join(root, "scripts"), 0o700); err != nil {
				t.Fatal(err)
			}
			canary := filepath.Join(root, "tail")
			testkit.WriteFile(t, filepath.Join(root, "scripts/profile-services.sh"), []byte("#!/bin/sh\nprintf reached > \"$CANARY\"\n"), 0o700)
			incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: status != "absent"}, Instances: map[string]ports.InstanceInfo{"subyard/yard": {Status: status}}}
			runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: incus, Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard"}, Environment: []string{"PATH=/usr/bin:/bin", "CANARY=" + canary}}
			stages := []application.ReconcileStage{{ID: ports.ReconcileStageMounts, Label: "mounts"}, {ID: ports.ReconcileStageProfileServices, Label: "services"}}
			runner := initReadyStageObserver(context.Background(), runtime, stages)
			if _, err := os.Stat(canary); !os.IsNotExist(err) {
				t.Fatalf("cold/stopped planning eagerly observed a tail stage: %v", err)
			}
			plan, err := (application.Reconciler{Stages: stages, Runner: runner}).Plan(context.Background())
			if err != nil || plan.Steps[1].Conditional != (status == "absent") {
				t.Fatalf("conditional traversal changed: plan=%+v err=%v", plan, err)
			}
			if _, err := os.Stat(canary); status == "absent" && !os.IsNotExist(err) || status == "stopped" && err != nil {
				t.Fatalf("tail did not follow serial conditional consumption: status=%s err=%v", status, err)
			}
		})
	}
}

func TestInitStageBatchEligibilityKeepsInjectedAndRepairCallersSerial(t *testing.T) {
	client := incusclient.New("unopened")
	for _, scenario := range []string{"native", "injected platform", "injected reader", "injected executor", "child", "repair", "resource", "reset", "profile", "source"} {
		t.Run(scenario, func(t *testing.T) {
			program := &CLI{}
			runtime := reconcileruntime.Runtime{Incus: client, Executor: client}
			execution := &initExecution{platform: runtime, mode: initReconcile}
			switch scenario {
			case "injected platform":
				program.options.InitPlatform = newInitPlatformFixture()
			case "injected reader":
				program.options.Incus = client
			case "injected executor":
				program.options.Executor = client
			case "child":
				program.releaseTransitionChild = true
			case "repair":
				program.profileInitRepair = &configApplyRepairPermit{}
			case "resource":
				execution.resourceCommand = "sample"
			case "reset":
				execution.mode = initReset
			case "profile":
				execution.requestedProfile = "sample"
			case "source":
				runtime.PreparedSources = &config.ProspectiveFiles{}
				execution.platform = runtime
			}
			if program.initBatchEligible(context.Background(), execution) != (scenario == "native") {
				t.Fatalf("unexpected native batching eligibility for %s", scenario)
			}
		})
	}
}

func TestInitStageBatchRefreshObservesLaterNativeDriftWithoutConsumingInput(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	state, calls := filepath.Join(root, "state"), filepath.Join(root, "calls")
	testkit.WriteFile(t, state, []byte("ready"), 0o600)
	testkit.WriteFile(t, filepath.Join(root, "scripts/profile-services.sh"), []byte("#!/bin/sh\nprintf 'check\\n' >> \"$CALLS\"\nif read -r ignored; then exit 9; fi\n[ \"$(cat \"$STATE\")\" != ready ] || exit 0\nprintf 'profile service check owner=sample native-exit=1 reason=unit-inactive\\n'\nexit 1\n"), 0o700)
	input := strings.NewReader("operator payload\n")
	incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true}, Instances: map[string]ports.InstanceInfo{"subyard/yard": {Status: "running"}}}
	runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: incus, Stdin: input, Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard"}, Environment: []string{"PATH=/usr/bin:/bin", "STATE=" + state, "CALLS=" + calls}}
	stages := []application.ReconcileStage{{ID: ports.ReconcileStageMounts, Label: "mounts"}, {ID: ports.ReconcileStageProfileServices, Label: "services"}}
	approved, err := (application.Reconciler{Stages: stages, Runner: initReadyStageObserver(context.Background(), runtime, stages)}).Plan(context.Background())
	if err != nil || approved.Pending() != 0 || input.Len() != len("operator payload\n") {
		t.Fatalf("initial read consumed input or required work: plan=%+v err=%v remaining=%d", approved, err, input.Len())
	}
	testkit.WriteFile(t, state, []byte("drift"), 0o600)
	_, err = (application.Reconciler{Stages: stages, Runner: initReadyStageObserver(context.Background(), runtime, stages)}).RefreshApproved(context.Background(), approved)
	if !errors.Is(err, domain.ErrPlanStale) || !strings.Contains(err.Error(), `init stage "profile-services" now requires work (owner=sample native-exit=1)`) || input.Len() != len("operator payload\n") {
		t.Fatalf("fresh batch accepted drift or consumed operator input: %v", err)
	}
	data, err := os.ReadFile(calls)
	if err != nil || strings.Count(string(data), "check") != 2 {
		t.Fatalf("later assessment reused prior native facts: calls=%q err=%v", data, err)
	}
}
