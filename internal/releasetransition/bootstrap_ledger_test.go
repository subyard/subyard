package releasetransition

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

// Model the real activation prerequisite: publication needs persisted authority,
// even when the registry's virtual baseline already has the required epochs.
type bootstrapAuthorityConsumer struct {
	store    *POSIXV2Store
	registry RegistryV2
	digest   Fingerprint
	ready    bool
}

func (*bootstrapAuthorityConsumer) ID() string { return "baseline-consumer" }
func (c *bootstrapAuthorityConsumer) Observe(context.Context, ReleasePair, ReleaseLinks) (V2ActivationObservation, error) {
	actual := digestA
	if c.ready {
		actual = digestB
	}
	return V2ActivationObservation{Actual: actual, Desired: digestB, Converged: c.ready}, nil
}
func (c *bootstrapAuthorityConsumer) Reconcile(context.Context, ReleaseLinks) error {
	state, err := c.store.ReadMigrationLedger(c.registry, c.digest)
	if err != nil {
		return err
	}
	if !state.AuthoritySnapshot.Exists {
		return errors.New("activation requires committed ledger authority")
	}
	if !sameLedgerV2(state.Ledger, freshInstalledLedger(c.registry)) {
		return errors.New("bootstrap ledger differs from canonical baseline")
	}
	c.ready = true
	return nil
}
func bootstrapConsumerOptions(t *testing.T, home string) (V2Options, *POSIXV2Store) {
	t.Helper()
	release := ReleaseID("0.29.0-bbbbbbbbbbbb")
	links := &ReleaseLinks{Active: release}
	options := currentHistoryOptions(t, home, links, release)
	r, d, err := ParseRegistryV2(options.RegistryPayload, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	options.Reconcilers = []V2ActivationReconciler{&bootstrapAuthorityConsumer{store: store, registry: r, digest: d}}
	return options, store
}
func TestBootstrapLedgerProcessHelper(t *testing.T) {
	point := os.Getenv("SUBYARD_TEST_BASELINE_CUT")
	if point == "" {
		t.Skip("subprocess helper")
	}
	home := os.Getenv("SUBYARD_TEST_BASELINE_HOME")
	options, _ := bootstrapConsumerOptions(t, home)
	options.fault = func(at string) error {
		if at == point {
			testkit.WriteFile(t, filepath.Join(home, "cut-reached"), []byte(point), 0600)
			for {
				time.Sleep(time.Second)
			}
		}
		return nil
	}
	engine, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := engine.Inspect(context.Background(), Goal{Target: options.Releases.Target, Direction: DirectionActivateTarget})
	if err != nil {
		t.Fatal(err)
	}
	out, err := engine.Converge(context.Background(), Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
	t.Fatalf("process did not reach cut: %#v %v", out, err)
}
func TestBootstrapLedgerKilledBeforeAndAfterPublicationResumes(t *testing.T) {
	for _, point := range []string{"before-baseline-ledger", "after-baseline-ledger", "after-step-verified"} {
		t.Run(point, func(t *testing.T) {
			home := testkit.TempDir(t)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-test.run=^TestBootstrapLedgerProcessHelper$")
			cmd.Env = append(os.Environ(), "SUBYARD_TEST_BASELINE_CUT="+point, "SUBYARD_TEST_BASELINE_HOME="+home)
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			for {
				if _, err := os.Stat(filepath.Join(home, "cut-reached")); err == nil {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("helper exited before durable cut: %v %s", err, output.String())
				case <-ctx.Done():
					t.Fatal("durable cut timeout")
				case <-time.After(10 * time.Millisecond):
				}
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = <-done; err == nil {
				t.Fatal("helper was not killed")
			}
			options, store := bootstrapConsumerOptions(t, home)
			before, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			journal, err := ParseJournal(before.Payload)
			if err != nil || len(journal.Steps) != 2 || journal.Steps[0].ID != baselineLedgerStep || journal.Steps[1].ID != "introduce-nm-device-guard-v4.ledger" {
				t.Fatalf("durable baseline intent: %#v %v", journal, err)
			}
			ledger, err := store.ReadLedger()
			if err != nil {
				t.Fatal(err)
			}
			if ledger.Exists != (point != "before-baseline-ledger") {
				t.Fatalf("unexpected publication at %s: %#v", point, ledger)
			}
			checkpoint, err := store.ReadMigrationCheckpoint()
			if err != nil || checkpoint.Exists {
				t.Fatal("checkpoint published before readiness", err)
			}
			engine, err := NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := engine.Inspect(context.Background(), journal.Goal)
			if err != nil || plan.Resume == nil || *plan.Resume != journal.Transaction {
				t.Fatalf("resume: %#v %v", plan, err)
			}
			out, err := engine.Converge(context.Background(), Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
			if err != nil || out.Status != StatusReady {
				t.Fatalf("resumed bootstrap: %#v %v", out, err)
			}
			after, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			complete, err := ParseJournal(after.Payload)
			if err != nil || complete.Checkpoint != JournalComplete || complete.Transaction != journal.Transaction || complete.AuthorizationPlan != journal.AuthorizationPlan {
				t.Fatalf("authorization changed: %#v %v", complete, err)
			}
			persisted, err := store.ReadLedger()
			if err != nil {
				t.Fatal(err)
			}
			installed, _, err := MarshalLedgerV2(freshInstalledLedger(engine.registry), engine.registry)
			if err != nil || !bytes.Equal(installed, persisted.Payload) {
				t.Fatalf("bootstrap did not record the floor baseline plus live steps: %s %v", persisted.Payload, err)
			}
			_, baseline, err := baselineLedgerIntent(engine.registry)
			if err != nil || ledger.Exists && !bytes.Equal(ledger.Payload, baseline) {
				t.Fatal("resume rewrote committed baseline", err)
			}
			if _, err := store.InspectSourceMetadata(engine.registry, engine.registryDigest); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestInitialLedgerBootstrapRequiresNoPriorState(t *testing.T) {
	for _, shape := range []string{"fresh", "ledger", "journal", "checkpoint", "previous", "different-source"} {
		t.Run(shape, func(t *testing.T) {
			home := testkit.TempDir(t)
			release := ReleaseID("0.29.0-bbbbbbbbbbbb")
			links := &ReleaseLinks{Active: release}
			options := currentHistoryOptions(t, home, links, release)
			r, d, err := ParseRegistryV2(options.RegistryPayload, BuiltinCapabilityCatalog())
			if err != nil {
				t.Fatal(err)
			}
			store, err := NewPOSIXV2Store(home)
			if err != nil {
				t.Fatal(err)
			}
			switch shape {
			case "ledger":
				_, p, err := baselineLedgerIntent(r)
				if err != nil {
					t.Fatal(err)
				}
				if err = store.CompareAndSwapLedger(absentProtectedSnapshot(), p); err != nil {
					t.Fatal(err)
				}
			case "journal":
				_, journal, _ := frozenAnchorFixture(t, "0.28.0")
				journal.Checkpoint = JournalComplete
				p, err := MarshalJournal(journal)
				if err != nil {
					t.Fatal(err)
				}
				if err = store.CompareAndSwapCurrentJournal(absentProtectedSnapshot(), p); err != nil {
					t.Fatal(err)
				}
			case "checkpoint":
				cp, err := newLedgerCheckpointV1(FreshBootstrapLedgerV2(r), r, d, absentProtectedSnapshot(), cloneEpochs(r.MinimumEpochs))
				if err != nil {
					t.Fatal(err)
				}
				p, _, err := MarshalLedgerCheckpointV1(cp, r, d, absentProtectedSnapshot())
				if err != nil {
					t.Fatal(err)
				}
				if err = store.compareAndSwap([]string{"release-transition", "v2"}, migrationLedgerCheckpointName, absentProtectedSnapshot(), p); err != nil {
					t.Fatal(err)
				}
			case "previous":
				links.Previous = releaseIDPointer("0.28.1-aaaaaaaaaaaa")
				options.Releases.Previous = cloneReleaseID(links.Previous)
			case "different-source":
				links.Active = "0.28.1-aaaaaaaaaaaa"
				options.Releases.From = links.Active
			}
			if err := os.MkdirAll(filepath.Join(home, "release-transition/v2"), 0700); err != nil {
				t.Fatal(err)
			}
			before := snapshotV2ProtectedTree(t, home)
			engine, err := NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := engine.Inspect(context.Background(), Goal{Target: release, Direction: DirectionActivateTarget})
			if shape == "fresh" && err != nil {
				t.Fatal(err)
			}
			found := false
			for _, decision := range plan.Decisions {
				if decision.Resource == "ledger.bootstrap" {
					found = true
				}
			}
			if found != (shape == "fresh") {
				t.Fatalf("prior state became bootstrap: %s %#v %v", shape, plan, err)
			}
			if !bytes.Equal(before, snapshotV2ProtectedTree(t, home)) {
				t.Fatal("bootstrap inspection wrote state")
			}
		})
	}
}

func TestBootstrapLedgerHistorySurvivesNextTransition(t *testing.T) {
	home := testkit.TempDir(t)
	release := ReleaseID("0.29.0-bbbbbbbbbbbb")
	links := &ReleaseLinks{Active: release}
	options := currentHistoryOptions(t, home, links, release)
	for _, target := range []ReleaseID{release, "0.30.0-cccccccccccc", "0.31.0-dddddddddddd"} {
		options.Releases = ReleasePair{From: links.Active, Previous: cloneReleaseID(links.Previous), Target: target}
		options.NewTransactionID = func() TransactionID {
			if target == release {
				return "tx-bootstrap-history"
			}
			return TransactionID("tx-next-" + string(target))
		}
		engine, err := NewV2Transition(options)
		if err != nil {
			t.Fatal(err)
		}
		goal := Goal{Target: target, Direction: DirectionActivateTarget}
		plan, err := engine.Inspect(context.Background(), goal)
		if err != nil {
			t.Fatal(err)
		}
		out, err := engine.Converge(context.Background(), Execution{Plan: plan.Plan, Authorization: v2TestAuthorization(plan.Plan)})
		if err != nil || out.Status != StatusReady || len(out.Warnings) != 0 {
			t.Fatalf("transition: %#v %v", out, err)
		}
		if _, err := engine.store.InspectSourceMetadata(engine.registry, engine.registryDigest); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.store.closedTransactionEvidence("tx-bootstrap-history", engine.registry); err != nil {
			t.Fatal("pinned initial ledger publication was not retained", err)
		}
	}
}

func TestFreshBootstrapRequiresPublishedLedgerForReady(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, readyReconciler := range []bool{false, true} {
			t.Run(fmt.Sprintf("checkpoint=%t/ready-reconciler=%t", checkpoint, readyReconciler), func(t *testing.T) {
				home := testkit.TempDir(t)
				release := ReleaseID("0.30.0-bbbbbbbbbbbb")
				links := &ReleaseLinks{Active: release}
				options := currentHistoryOptions(t, home, links, release)
				options.MigrationCheckpoint = checkpoint
				if readyReconciler {
					options.Reconcilers = []V2ActivationReconciler{&v2TestReconciler{converged: true}}
				}
				engine, err := NewV2Transition(options)
				if err != nil {
					t.Fatal(err)
				}
				goal := Goal{Target: release, Direction: DirectionActivateTarget}
				plan, err := engine.Inspect(context.Background(), goal)
				if err != nil {
					t.Fatal(err)
				}
				ledger, err := engine.store.ReadLedger()
				if err != nil {
					t.Fatal(err)
				}
				if ledger.Exists || plan.Outcome == nil || plan.Outcome.Status != StatusMigrationRequired || plan.Outcome.Code != CodeTransitionRequired || plan.Outcome.ReachedGoal || !plan.Assessment.Changed || len(plan.Outcome.Warnings) != 0 {
					t.Fatalf("unpublished baseline reported complete: ledger=%t inspection=%#v", ledger.Exists, plan)
				}
				out, err := engine.Converge(context.Background(), Execution{Plan: plan.Plan})
				if err != nil || out.Status != StatusOperatorActionRequired || out.Code != CodeConfirmationRequired || out.ReachedGoal || len(out.Warnings) != 0 {
					t.Fatalf("unpublished baseline bypassed consent: %#v %v", out, err)
				}
				for _, name := range []string{"ledger.json", "journal.json", migrationLedgerCheckpointName} {
					if _, err := os.Lstat(filepath.Join(home, "release-transition/v2", name)); !os.IsNotExist(err) {
						t.Fatalf("unconfirmed bootstrap wrote %s: %v", name, err)
					}
				}
				ledgerStateReady(t, home, options)
				ledger, err = engine.store.ReadLedger()
				if err != nil || !ledger.Exists {
					t.Fatalf("confirmed bootstrap did not publish ledger: %v", err)
				}
			})
		}
	}
}

// freshInstalledLedger is the floor baseline advanced through every live
// migration, which an initial bootstrap journals after its baseline step.
func freshInstalledLedger(registry RegistryV2) LedgerV2 {
	ledger := FreshBootstrapLedgerV2(registry)
	for _, migration := range registry.Migrations {
		next, err := ledger.Advance(registry, migration)
		if err != nil {
			panic(err)
		}
		ledger = next
	}
	return ledger
}
