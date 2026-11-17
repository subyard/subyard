package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	rt "github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestPackagedRollbackTargetResumesAfterSourceDiesAtLinkSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	root, environment, _ := nativeFixture(t)
	home := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	targetRoot := sourceAdmissionToolingFixture(t, runtimeRoot)
	sourceCopy := sourceAdmissionToolingFixture(t, filepath.Join(root, "source-stage"))
	target := rt.ReleaseID(filepath.Base(targetRoot))
	pair := rt.ReleasePair{From: target + "-source", Target: target, Previous: &target}
	sourceRoot := filepath.Join(runtimeRoot, "releases", string(pair.From))
	if err := os.Rename(sourceCopy, sourceRoot); err != nil {
		t.Fatal(err)
	}
	sourceRegistry := mustReadConfigFixture(t, filepath.Join(sourceRoot, "config/release-transition.json"))
	sourceManifest := mustReadConfigFixture(t, filepath.Join(sourceRoot, "runtime-files.sha256"))
	registry, _, err := rt.ParseRegistryV2(sourceRegistry, rt.BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	// The two packaged engines have distinct sealed registries. Keep the
	// successor-only registration unapplied in the rollback's native cut.
	last := registry.Migrations[len(registry.Migrations)-1]
	registry.Migrations = registry.Migrations[:len(registry.Migrations)-1]
	registry.CurrentEpochs[last.Domain] = last.FromEpoch
	targetRegistry, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(targetRoot, "config/release-transition.json"), targetRegistry, 0o600)
	targetManifest := sourceFixtureManifest(t, targetRoot)
	for name, release := range map[string]rt.ReleaseID{"current": pair.From, "previous": pair.Target} {
		if err := os.Symlink("releases/"+string(release), filepath.Join(runtimeRoot, name)); err != nil {
			t.Fatal(err)
		}
	}

	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	server.SetExtensions("projects", "instances", "instance_all_projects", "storage", "network")
	environment = append(environment, "PATH=/usr/bin:/bin", "SUBYARD_INCUS_SOCKET="+server.SocketPath, "YARD_RUNTIME_ROOT="+runtimeRoot,
		"SUBYARD_POWER_RECONCILER_PATH="+filepath.Join(root, "absent-power"), "SUBYARD_POWER_UNIT_PATH="+filepath.Join(root, "absent-unit"))
	adapterConfig := releaseruntime.Config{Inputs: releaseruntime.CaptureCommandInputs(environment, nil), Stderr: os.Stderr}
	adapter := releaseruntime.New(adapterConfig)
	defer adapter.Close()
	store, err := rt.NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	native := rt.BaselineLedgerV2(registry)
	pending, err := registry.PendingPath(native)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range pending {
		prior := native.Domains[migration.Domain]
		native.Domains[migration.Domain] = rt.DomainLedgerV2{Epoch: migration.ToEpoch, Applied: append(prior.Applied, migration.ID)}
	}
	payload, _, err := rt.MarshalLedgerV2(native, registry)
	if err != nil {
		t.Fatal(err)
	}
	absent, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(absent, payload); err != nil {
		t.Fatal(err)
	}
	targetPin := rt.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(targetRegistry)))
	projection, err := store.ReadMigrationLedger(registry, targetPin)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConvertMigrationLedger(projection, registry, targetPin, registry, targetPin); err != nil {
		t.Fatal(err)
	}

	request := rt.RollbackTargetRequest{SchemaVersion: 3, Mode: "activate-under-v2", RuntimeRoot: runtimeRoot, ConfigHome: home, Yard: "default", Releases: pair,
		SourceArtifact: rt.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(sourceManifest))), SourceRegistry: rt.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(sourceRegistry))),
		TargetArtifact: rt.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(targetManifest))), TargetRegistry: targetPin, Projection: projection.ProjectionSnapshot.Fingerprint, NativeHistory: &native}
	inspection, err := adapter.InvokeRollbackTarget(ctx, request, nil)
	if err != nil || inspection.Inspection == nil || len(inspection.Inspection.Blockers) != 0 {
		t.Fatalf("packaged R inspection = %+v, %v", inspection, err)
	}
	record := rt.SourceRollbackActivation{Request: request, Plan: inspection.Inspection.Plan, SourceRegistry: sourceRegistry, TargetRegistry: targetRegistry}
	journal, err := store.StartSourceRollbackActivation(record, rt.Execution{Plan: record.Plan.Token, Authorization: "authorized-source-rollback"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	first := exec.CommandContext(ctx, executable, "-test.run", "^TestPackagedRollbackTargetUpdaterHelper$")
	first.Env = append(append([]string{}, environment...), "SUBYARD_ROLLBACK_TEST_CRASH=1")
	output, err := first.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("S did not die at the activation cut: %v: %s", err, output)
	}
	after, err := store.ReadCurrentJournal()
	if err != nil || !bytes.Equal(after.Payload, before.Payload) || journal.Checkpoint != rt.JournalActivationIntent {
		t.Fatalf("crash did not leave the exact ActivationIntent journal: %v", err)
	}
	links, err := rt.NewRuntimeLinkStore(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := links.Observe()
	if err != nil || observed.Active != pair.Target || observed.Previous == nil || *observed.Previous != pair.From {
		t.Fatalf("crash did not leave durably switched links: %+v, %v", observed, err)
	}

	fresh := releaseruntime.New(adapterConfig)
	defer fresh.Close()
	recoveryPath := filepath.Join(home, "release-transition", "v2", "transactions", string(journal.Transaction), "recovery", rt.RollbackTargetActivationStep+".json")
	recovery := mustReadConfigFixture(t, recoveryPath)
	testkit.WriteFile(t, recoveryPath, []byte("{}\n"), 0o600)
	if _, err := fresh.PrepareTransition(ctx, nil, home, "default"); err == nil {
		t.Fatal("production updater accepted a changed activation plan")
	}
	testkit.WriteFile(t, recoveryPath, recovery, 0o600)
	prepared, err := fresh.PrepareTransition(ctx, nil, home, "default")
	if err != nil {
		t.Fatalf("fresh production updater could not prepare recovery: %v", err)
	}
	if err := prepared.Execute(ctx); err != nil {
		t.Fatalf("fresh production updater could not complete recovery: %v", err)
	}
	proof, err := store.InspectSealedCompletedRollback(targetRegistry, request.TargetArtifact)
	if err != nil || proof == nil || proof.Journal.Transaction != journal.Transaction {
		t.Fatalf("recovered rollback has no exact closure: %+v, %v", proof, err)
	}
}

func TestPackagedRollbackTargetUpdaterHelper(t *testing.T) {
	if os.Getenv("SUBYARD_ROLLBACK_TEST_CRASH") != "1" {
		return
	}
	home, runtimeRoot := os.Getenv("SUBYARD_CONFIG_HOME"), os.Getenv("YARD_RUNTIME_ROOT")
	store, err := rt.NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := rt.ParseJournal(selected.Payload)
	if err != nil || journal.Checkpoint != rt.JournalActivationIntent {
		t.Fatalf("rollback activation requires ActivationIntent: %v", err)
	}
	links, err := rt.NewRuntimeLinkStore(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	request := rt.ProcessRequest{SchemaVersion: 1, Mode: rt.ProcessConverge, RuntimeRoot: runtimeRoot, ConfigHome: home, Yard: "default",
		Target: journal.Goal.Target, Direction: journal.Goal.Direction, ArtifactDigest: journal.ArtifactDigest, RegistryDigest: journal.RegistryDigest,
		Execution: &rt.Execution{Plan: journal.ResumePlan}}
	source := filepath.Join(runtimeRoot, "releases", string(journal.Releases.From))
	_, _, err = store.ProcessSourceRollbackActivation(context.Background(), request, source, func(pair rt.ReleasePair) (rt.ReleaseLinks, error) {
		observed, err := links.Activate(pair)
		if err != nil {
			return observed, err
		}
		return observed, syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}, func(context.Context, rt.RollbackTargetRequest, *os.File) (rt.RollbackTargetResponse, error) {
		t.Fatal("R ran before the injected crash")
		return rt.RollbackTargetResponse{}, nil
	})
	t.Fatalf("rollback crash point was not reached: %s", strings.TrimSpace(fmt.Sprint(err)))
}
