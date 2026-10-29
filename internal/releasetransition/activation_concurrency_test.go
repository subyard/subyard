package releasetransition

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

type callbackActivationObserver struct {
	id      string
	observe func(context.Context) (V2ActivationObservation, error)
}

func (observer callbackActivationObserver) ID() string { return observer.id }
func (observer callbackActivationObserver) Observe(ctx context.Context, _ ReleasePair, _ ReleaseLinks) (V2ActivationObservation, error) {
	return observer.observe(ctx)
}
func (observer callbackActivationObserver) ObserveProspective(ctx context.Context, pair ReleasePair, links ReleaseLinks, _ V2SettingsSnapshotView) (V2ActivationObservation, error) {
	return observer.Observe(ctx, pair, links)
}
func (callbackActivationObserver) Reconcile(context.Context, ReleaseLinks) error {
	return errors.New("observation must not reconcile")
}

type concurrentActivationObserver struct{ callbackActivationObserver }

func (concurrentActivationObserver) ConcurrentActivationObservation() bool { return true }

func receiveActivationStart(t *testing.T, started <-chan string) string {
	t.Helper()
	select {
	case id := <-started:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("activation observer did not start")
		return ""
	}
}

func TestActivationObservationBoundsOverlapAndReducesInOrder(t *testing.T) {
	started := make(chan string, 8)
	releases := map[string]chan struct{}{}
	var running atomic.Int32
	observation := v2Observation{}
	transition := &V2Transition{}
	for _, id := range []string{"a", "b", "c", "d", "e", "serial", "f", "g"} {
		releases[id] = make(chan struct{})
		observer := callbackActivationObserver{id: id, observe: func(ctx context.Context) (V2ActivationObservation, error) {
			if id == "serial" && (running.Load() != 0 || len(observation.activationScope) != 5) {
				return V2ActivationObservation{}, errors.New("serial observer crossed an undrained group")
			}
			running.Add(1)
			defer running.Add(-1)
			started <- id
			select {
			case <-releases[id]:
			case <-ctx.Done():
				return V2ActivationObservation{}, ctx.Err()
			}
			return V2ActivationObservation{Actual: digestB, Desired: digestA, Consequences: []string{id}, Warnings: []string{"warning " + id}}, nil
		}}
		if id == "serial" {
			transition.options.Reconcilers = append(transition.options.Reconcilers, observer)
		} else {
			transition.options.Reconcilers = append(transition.options.Reconcilers, concurrentActivationObserver{observer})
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- transition.observeActivation(ctx, &observation) }()
	first := []string{receiveActivationStart(t, started), receiveActivationStart(t, started), receiveActivationStart(t, started)}
	slices.Sort(first)
	if !slices.Equal(first, []string{"a", "b", "c"}) || running.Load() != 3 {
		t.Fatalf("first group = %v, running=%d", first, running.Load())
	}
	select {
	case id := <-started:
		t.Fatalf("observer %s exceeded width three", id)
	default:
	}
	for _, id := range []string{"c", "b", "a"} {
		close(releases[id])
	}
	second := []string{receiveActivationStart(t, started), receiveActivationStart(t, started)}
	slices.Sort(second)
	if !slices.Equal(second, []string{"d", "e"}) {
		t.Fatalf("remaining group = %v", second)
	}
	close(releases["e"])
	close(releases["d"])
	if id := receiveActivationStart(t, started); id != "serial" {
		t.Fatalf("serial boundary replaced by %s", id)
	}
	close(releases["serial"])
	for range 2 {
		id := receiveActivationStart(t, started)
		if id != "f" && id != "g" {
			t.Fatalf("unexpected final observer %s", id)
		}
		close(releases[id])
	}
	if err := <-done; err != nil || running.Load() != 0 {
		t.Fatalf("observation did not drain: err=%v running=%d", err, running.Load())
	}
	want := []string{"a", "b", "c", "d", "e", "serial", "f", "g"}
	var ids []string
	for _, scope := range observation.activationScope {
		ids = append(ids, scope.ID)
	}
	if !slices.Equal(ids, want) || !slices.Equal(observation.activationConsequences, want) || len(observation.activationWarnings) != len(want) {
		t.Fatalf("noncanonical reduction: scope=%v consequences=%v warnings=%v", ids, observation.activationConsequences, observation.activationWarnings)
	}
}

func TestActivationObservationKeepsDefaultAndProspectiveCallsSerial(t *testing.T) {
	for _, prospective := range []bool{false, true} {
		t.Run(fmt.Sprintf("prospective=%t", prospective), func(t *testing.T) {
			started := make(chan string, 2)
			release := make(chan struct{})
			transition := &V2Transition{}
			for _, id := range []string{"a", "b"} {
				observer := callbackActivationObserver{id: id, observe: func(ctx context.Context) (V2ActivationObservation, error) {
					started <- id
					if id == "a" {
						select {
						case <-release:
						case <-ctx.Done():
							return V2ActivationObservation{}, ctx.Err()
						}
					}
					return V2ActivationObservation{Actual: digestA, Desired: digestA, Converged: true}, nil
				}}
				if prospective {
					transition.options.Reconcilers = append(transition.options.Reconcilers, concurrentActivationObserver{observer})
				} else {
					transition.options.Reconcilers = append(transition.options.Reconcilers, observer)
				}
			}
			observation := v2Observation{}
			if prospective {
				observation.prospectiveSettings = v2MemorySettingsView{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- transition.observeActivation(ctx, &observation) }()
			if id := receiveActivationStart(t, started); id != "a" {
				t.Fatalf("first serial observer = %s", id)
			}
			select {
			case id := <-started:
				t.Fatalf("serial observer %s overlapped", id)
			default:
			}
			close(release)
			if id := receiveActivationStart(t, started); id != "b" {
				t.Fatalf("second serial observer = %s", id)
			}
			if err := <-done; err != nil || !observation.activationFixed {
				t.Fatalf("serial observation = %+v, err=%v", observation, err)
			}
		})
	}
}

func TestActivationObservationCancellationDrainsBeforeSerialBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 4)
	var running, completed atomic.Int32
	transition := &V2Transition{}
	for _, id := range []string{"a", "b", "c", "d"} {
		transition.options.Reconcilers = append(transition.options.Reconcilers, concurrentActivationObserver{callbackActivationObserver{id: id, observe: func(ctx context.Context) (V2ActivationObservation, error) {
			running.Add(1)
			started <- id
			<-ctx.Done()
			running.Add(-1)
			completed.Add(1)
			return V2ActivationObservation{}, ctx.Err()
		}}})
	}
	transition.options.Reconcilers = append(transition.options.Reconcilers, callbackActivationObserver{id: "serial", observe: func(context.Context) (V2ActivationObservation, error) {
		if running.Load() != 0 || completed.Load() != 4 {
			return V2ActivationObservation{}, errors.New("cancelled group was not drained")
		}
		return V2ActivationObservation{Actual: digestA, Desired: digestA, Converged: true}, nil
	}})
	observation := v2Observation{}
	done := make(chan error, 1)
	go func() { done <- transition.observeActivation(ctx, &observation) }()
	for range 3 {
		receiveActivationStart(t, started)
	}
	cancel()
	if err := <-done; err != nil || running.Load() != 0 || len(observation.blockers) != 4 {
		t.Fatalf("cancelled observation = %+v, err=%v running=%d", observation, err, running.Load())
	}
	if !errors.Is(observation.activationObservationFailure, context.Canceled) {
		t.Fatalf("first ordered failure = %v", observation.activationObservationFailure)
	}
	for index, blocker := range observation.blockers {
		if blocker.Resource != "activation."+string(rune('a'+index)) {
			t.Fatalf("blocker order = %+v", observation.blockers)
		}
	}
}

func TestActivationObservationSelectsFirstRegisteredFailure(t *testing.T) {
	first, second := errors.New("first observer failure"), errors.New("second observer failure")
	secondFinished := make(chan struct{})
	transition := &V2Transition{options: V2Options{Reconcilers: []V2ActivationReconciler{
		concurrentActivationObserver{callbackActivationObserver{id: "first", observe: func(ctx context.Context) (V2ActivationObservation, error) {
			select {
			case <-secondFinished:
				return V2ActivationObservation{}, first
			case <-ctx.Done():
				return V2ActivationObservation{}, ctx.Err()
			}
		}}},
		concurrentActivationObserver{callbackActivationObserver{id: "second", observe: func(context.Context) (V2ActivationObservation, error) {
			close(secondFinished)
			return V2ActivationObservation{}, second
		}}},
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observation := v2Observation{}
	if err := transition.observeActivation(ctx, &observation); err != nil || observation.activationObservationFailure != first ||
		len(observation.blockers) != 2 || observation.blockers[0].Resource != "activation.first" || observation.blockers[1].Resource != "activation.second" {
		t.Fatalf("completion order changed the first failure: %+v, err=%v", observation, err)
	}
}
