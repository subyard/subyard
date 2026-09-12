package resourceendpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPreviewIsReadOnlyAndSelectsAutomaticEndpoint(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "resource-endpoints")
	protectParent(t, directory)
	manager := Manager{
		Discover: func(context.Context) (string, error) { return "owner.example.ts.net", nil },
		Occupied: func(_ context.Context, _ string, port int) (bool, error) { return port == 6768, nil },
	}
	plan, err := manager.Preview(context.Background(), Request{
		Directory: directory, Yard: "demo", Resource: "orca.orca",
		PreferredPort: 6768, ReservedPorts: []int{6769},
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if plan.Host != "owner.example.ts.net" || plan.Port != 6770 ||
		plan.HostSource != SourceAuto || plan.PortSource != SourceAuto || plan.Snapshot == "" {
		t.Fatalf("plan = %#v", plan)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Preview created state directory: %v", err)
	}
}

func TestCommitPersistsEndpointAndPreviewReusesItWithoutLiveProbe(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "resource-endpoints")
	protectParent(t, directory)
	manager := Manager{
		Discover: func(context.Context) (string, error) { return "owner.example.ts.net", nil },
		Occupied: func(context.Context, string, int) (bool, error) { return false, nil },
	}
	request := Request{Directory: directory, Yard: "demo", Resource: "orca.orca", PreferredPort: 6768}
	plan, err := manager.Preview(context.Background(), request)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if err := manager.Commit(context.Background(), request, plan); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	host, port, exists, err := ReadSaved(directory, "demo", "orca.orca")
	if err != nil || !exists || host != plan.Host || port != plan.Port {
		t.Fatalf("ReadSaved = %q %d %v, %v", host, port, exists, err)
	}
	manager.Discover = func(context.Context) (string, error) { return "", errors.New("must not discover") }
	manager.Occupied = func(context.Context, string, int) (bool, error) { return false, errors.New("must not probe") }
	saved, err := manager.Preview(context.Background(), request)
	if err != nil {
		t.Fatalf("saved Preview: %v", err)
	}
	if saved.Host != plan.Host || saved.Port != plan.Port ||
		saved.HostSource != SourceSaved || saved.PortSource != SourceSaved {
		t.Fatalf("saved plan = %#v", saved)
	}
}

func TestExplicitValuesWinAndAreReserved(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "resource-endpoints")
	protectParent(t, directory)
	manager := Manager{
		Discover: func(context.Context) (string, error) { return "", errors.New("must not discover") },
		Occupied: func(context.Context, string, int) (bool, error) { return false, errors.New("must not probe") },
	}
	first := Request{Directory: directory, Yard: "one", Resource: "orca.orca", Host: "127.0.0.1", Port: "17000", PreferredPort: 6768}
	plan, err := manager.Preview(context.Background(), first)
	if err != nil {
		t.Fatalf("Preview explicit: %v", err)
	}
	if plan.HostSource != SourceOverride || plan.PortSource != SourceOverride {
		t.Fatalf("explicit sources = %#v", plan)
	}
	if err := manager.Commit(context.Background(), first, plan); err != nil {
		t.Fatalf("Commit explicit: %v", err)
	}
	_, err = manager.Preview(context.Background(), Request{
		Directory: directory, Yard: "two", Resource: "orca.orca",
		Host: "127.0.0.1", Port: "17000", PreferredPort: 6768,
	})
	if !errors.Is(err, ErrPortReserved) {
		t.Fatalf("conflicting explicit Preview error = %v", err)
	}
}

func TestExplicitPortCannotUseCallerReservation(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "resource-endpoints")
	protectParent(t, directory)
	manager := Manager{Discover: func(context.Context) (string, error) { return "127.0.0.1", nil }}
	_, err := manager.Preview(context.Background(), Request{
		Directory: directory, Yard: "demo", Resource: "orca.orca", Host: "127.0.0.1",
		Port: "2222", PreferredPort: 6768, ReservedPorts: []int{2222},
	})
	if !errors.Is(err, ErrPortReserved) {
		t.Fatalf("reserved override error = %v", err)
	}
}

func TestCommitRejectsStaleAutomaticSelection(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "resource-endpoints")
	protectParent(t, directory)
	manager := Manager{
		Discover: func(context.Context) (string, error) { return "owner.example.ts.net", nil },
		Occupied: func(context.Context, string, int) (bool, error) { return false, nil },
	}
	requestOne := Request{Directory: directory, Yard: "one", Resource: "orca.orca", PreferredPort: 6768}
	requestTwo := Request{Directory: directory, Yard: "two", Resource: "orca.orca", PreferredPort: 6768}
	planOne, err := manager.Preview(context.Background(), requestOne)
	if err != nil {
		t.Fatal(err)
	}
	planTwo, err := manager.Preview(context.Background(), requestTwo)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Commit(context.Background(), requestOne, planOne); err != nil {
		t.Fatal(err)
	}
	if err := manager.Commit(context.Background(), requestTwo, planTwo); !errors.Is(err, ErrPlanStale) {
		t.Fatalf("stale Commit error = %v", err)
	}
}

func TestConcurrentCommitsSerializeInitialAllocation(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "resource-endpoints")
	protectParent(t, directory)
	manager := Manager{
		Discover: func(context.Context) (string, error) { return "owner.example.ts.net", nil },
		Occupied: func(context.Context, string, int) (bool, error) { return false, nil },
	}
	requests := []Request{
		{Directory: directory, Yard: "one", Resource: "orca.orca", PreferredPort: 6768},
		{Directory: directory, Yard: "two", Resource: "orca.orca", PreferredPort: 6768},
	}
	plans := make([]Plan, len(requests))
	for index, request := range requests {
		plan, err := manager.Preview(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		plans[index] = plan
	}
	if plans[0].Port != 6768 || plans[1].Port != 6768 {
		t.Fatalf("contenders did not preview the same initial port: %#v", plans)
	}

	type result struct {
		index int
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, len(requests))
	for index := range requests {
		go func(index int) {
			<-start
			results <- result{index: index, err: manager.Commit(
				context.Background(), requests[index], plans[index],
			)}
		}(index)
	}
	close(start)
	first, second := <-results, <-results
	observed := []result{first, second}
	winner, loser := -1, -1
	for _, item := range observed {
		switch {
		case item.err == nil:
			if winner != -1 {
				t.Fatalf("both concurrent commits succeeded: %#v", observed)
			}
			winner = item.index
		case errors.Is(item.err, ErrPlanStale):
			if loser != -1 {
				t.Fatalf("both concurrent commits were stale: %#v", observed)
			}
			loser = item.index
		default:
			t.Fatalf("concurrent Commit %d error = %v", item.index, item.err)
		}
	}
	if winner == -1 || loser == -1 {
		t.Fatalf("concurrent results lack one winner and loser: %#v", observed)
	}
	for index, request := range requests {
		host, port, exists, err := ReadSaved(directory, request.Yard, request.Resource)
		if err != nil {
			t.Fatal(err)
		}
		if index == winner {
			if !exists || host != plans[index].Host || port != 6768 {
				t.Fatalf("winner state = %q:%d exists=%v", host, port, exists)
			}
		} else if exists {
			t.Fatalf("loser unexpectedly has saved state %q:%d", host, port)
		}
	}

	fresh, err := manager.Preview(context.Background(), requests[loser])
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Port != 6769 {
		t.Fatalf("loser fresh port = %d, want 6769", fresh.Port)
	}
	if err := manager.Commit(context.Background(), requests[loser], fresh); err != nil {
		t.Fatalf("loser fresh Commit: %v", err)
	}
	for index, request := range requests {
		_, port, exists, err := ReadSaved(directory, request.Yard, request.Resource)
		if err != nil || !exists {
			t.Fatalf("final allocation %d exists=%v err=%v", index, exists, err)
		}
		want := 6769
		if index == winner {
			want = 6768
		}
		if port != want {
			t.Fatalf("final allocation %d port=%d want=%d", index, port, want)
		}
	}
}

func TestCommitWithDoesNotInvokeCallbackForStaleEndpointPlan(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "resource-endpoints")
	protectParent(t, directory)
	manager := Manager{
		Discover: func(context.Context) (string, error) { return "owner.example.ts.net", nil },
		Occupied: func(context.Context, string, int) (bool, error) { return false, nil },
	}
	first := Request{Directory: directory, Yard: "one", Resource: "orca.orca", PreferredPort: 6768}
	second := Request{Directory: directory, Yard: "two", Resource: "orca.orca", PreferredPort: 6768}
	firstPlan, err := manager.Preview(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	secondPlan, err := manager.Preview(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Commit(context.Background(), first, firstPlan); err != nil {
		t.Fatal(err)
	}
	called := false
	err = manager.CommitWith(context.Background(), second, secondPlan, func() error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrPlanStale) {
		t.Fatalf("stale CommitWith error = %v", err)
	}
	if called {
		t.Fatal("stale endpoint plan invoked before-publish callback")
	}
}

func TestCommitWithCallbackFailureDoesNotPublishEndpoint(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "resource-endpoints")
	protectParent(t, directory)
	manager := Manager{
		Discover: func(context.Context) (string, error) { return "owner.example.ts.net", nil },
		Occupied: func(context.Context, string, int) (bool, error) { return false, nil },
	}
	request := Request{Directory: directory, Yard: "demo", Resource: "orca.orca", PreferredPort: 6768}
	plan, err := manager.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	callbackErr := errors.New("profile changed")
	if err := manager.CommitWith(context.Background(), request, plan, func() error {
		return callbackErr
	}); !errors.Is(err, callbackErr) {
		t.Fatalf("CommitWith callback error = %v", err)
	}
	if _, _, exists, err := ReadSaved(directory, request.Yard, request.Resource); err != nil || exists {
		t.Fatalf("failed callback published endpoint: exists=%v err=%v", exists, err)
	}

	if err := manager.Commit(context.Background(), request, plan); err != nil {
		t.Fatal(err)
	}
	changedRequest := request
	changedRequest.Host = "127.0.0.1"
	changed, err := manager.Preview(context.Background(), changedRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CommitWith(context.Background(), changedRequest, changed, func() error {
		return callbackErr
	}); !errors.Is(err, callbackErr) {
		t.Fatalf("CommitWith update callback error = %v", err)
	}
	host, port, exists, err := ReadSaved(directory, request.Yard, request.Resource)
	if err != nil || !exists || host != plan.Host || port != plan.Port {
		t.Fatalf("failed callback changed endpoint to %q:%d: exists=%v err=%v", host, port, exists, err)
	}
}

func TestCommitWithInvokesCallbackForExistingExactAllocation(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "resource-endpoints")
	protectParent(t, directory)
	manager := Manager{
		Discover: func(context.Context) (string, error) { return "owner.example.ts.net", nil },
		Occupied: func(context.Context, string, int) (bool, error) { return false, nil },
	}
	request := Request{Directory: directory, Yard: "demo", Resource: "orca.orca", PreferredPort: 6768}
	plan, err := manager.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Commit(context.Background(), request, plan); err != nil {
		t.Fatal(err)
	}
	saved, err := manager.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	if err := manager.CommitWith(context.Background(), request, saved, func() error {
		called++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("exact allocation callback calls = %d, want 1", called)
	}
}

func TestReadSavedRejectsSymlinkState(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "resource-endpoints")
	protectParent(t, directory)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(target, []byte(`{"schema":1,"allocations":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(directory, "state.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ReadSaved(directory, "demo", "orca.orca"); err == nil {
		t.Fatal("symlink state was accepted")
	}
}

func protectParent(t *testing.T, directory string) {
	t.Helper()
	if err := os.Chmod(filepath.Dir(directory), 0o700); err != nil {
		t.Fatal(err)
	}
}
