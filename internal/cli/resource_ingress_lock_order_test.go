package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

const lockOrderDeadline = 30 * time.Second

// migrateLikeHolder takes the owner barrier, then the per-yard lock, in the
// release-transition order. barrier is signalled once the barrier is held.
func migrateLikeHolder(ctx context.Context, program *CLI, barrier chan<- struct{}, proceed <-chan struct{}) error {
	loaded, err := program.loadContext("private")
	if err != nil {
		return err
	}
	_, releaseBarrier, err := uninstall.OwnerLock(ctx, loaded.Context.Paths.DataHome, nil)
	if err != nil {
		return err
	}
	defer releaseBarrier()
	close(barrier)
	if proceed != nil {
		select {
		case <-proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	unlock, err := lockIntegrationYard(ctx, loaded)
	if err != nil {
		return err
	}
	unlock()
	return nil
}

func waitForFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", filepath.Base(path))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// A public-ingress resource verb and a migrate-like owner of the barrier that
// then needs the yard lock must both finish in either interleaving. This pins
// the prepared v2 path's order (owner barrier, then yard lock); the regression
// for the removed legacy path's deadlock is TestResourceIngressRefusesLegacyPlan.
func TestResourceIngressVerbAndBarrierHolderDoNotDeadlock(t *testing.T) {
	for _, order := range []string{"barrier-first", "verb-first"} {
		t.Run(order, func(t *testing.T) {
			program, home, output := shutdownGateFixture(t, "attributed")
			sync := filepath.Join(home, "lock-order")
			if err := os.Mkdir(sync, 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), lockOrderDeadline)
			defer cancel()
			verbDone := make(chan int, 1)
			holderDone := make(chan error, 1)
			barrier := make(chan struct{})
			waitHolder := func() {
				t.Helper()
				select {
				case err := <-holderDone:
					if err != nil {
						t.Fatalf("barrier holder did not get the yard lock: %v", err)
					}
				case <-ctx.Done():
					t.Fatal("barrier holder did not finish")
				}
			}
			switch order {
			case "barrier-first":
				// After the retry native apply needs no pause.
				testkit.WriteFile(t, filepath.Join(sync, "go"), nil, 0o600)
				proceed := make(chan struct{})
				go func() { holderDone <- migrateLikeHolder(ctx, program, barrier, proceed) }()
				<-barrier
				go func() { verbDone <- program.Run(ctx) }()
				select {
				case code := <-verbDone:
					if code != 1 || !strings.Contains(output.String(), "release state is changing; retry") {
						t.Fatalf("contended admission: code=%d output=%s", code, output)
					}
				case <-ctx.Done():
					t.Fatal("admission waited on the owner barrier")
				}
				close(proceed)
				waitHolder()
				go func() { verbDone <- program.Run(ctx) }()
			case "verb-first":
				go func() { verbDone <- program.Run(ctx) }()
				waitForFile(t, ctx, filepath.Join(sync, "applying"))
				go func() { holderDone <- migrateLikeHolder(ctx, program, barrier, nil) }()
				select {
				case <-barrier:
					t.Fatal("owner barrier was free while the resource apply held the yard lock")
				case <-time.After(300 * time.Millisecond):
				}
				testkit.WriteFile(t, filepath.Join(sync, "go"), nil, 0o600)
			}
			var code int
			select {
			case code = <-verbDone:
			case <-ctx.Done():
				t.Fatalf("resource verb did not finish: %s", output)
			}
			if order == "verb-first" {
				waitHolder()
			}
			// The fixture's native shutdown reports an unverified guest after
			// closing ingress; reaching it proves the verb was not refused.
			if code != 1 || !strings.Contains(output.String(), "guest shutdown is unverified") {
				t.Fatalf("resource verb did not reach native apply: code=%d output=%s", code, output)
			}
		})
	}
}

// A legacy (v1) plan for a public-ingress lifecycle verb is refused before
// taking the yard lock or running the action.
func TestResourceIngressRefusesLegacyPlan(t *testing.T) {
	program, home, output := shutdownGateFixture(t, "attributed-v1")
	ctx, cancel := context.WithTimeout(context.Background(), lockOrderDeadline)
	defer cancel()
	loaded, err := program.loadContext("private")
	if err != nil {
		t.Fatal(err)
	}
	unlockYard, err := lockIntegrationYard(ctx, loaded)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockYard()
	verbDone := make(chan int, 1)
	go func() { verbDone <- program.Run(ctx) }()
	select {
	case code := <-verbDone:
		if code != 1 || !strings.Contains(output.String(), "relay: resource_plan_invalid: public ingress down requires a prepared yard.resource-action-assessment.v2 plan") {
			t.Fatalf("legacy ingress plan not refused clearly: code=%d output=%s", code, output)
		}
	case <-ctx.Done():
		t.Fatalf("legacy ingress plan waited on held locks: %s", output)
	}
	if _, err := os.Stat(filepath.Join(home, "cleanup-pending")); !os.IsNotExist(err) {
		t.Fatalf("refused legacy plan reached native apply: %v", err)
	}
}
