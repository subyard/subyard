package migration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"golang.org/x/crypto/ssh"
)

const (
	testPublishedV1PreviousRuntime = "releases/0.4.0-68b9925f6880"
	testPublishedV1041Runtime      = "releases/0.4.1-fc5b03078508"
	testPublishedV1042Runtime      = "releases/0.4.2-17608894ab09"
)

func TestV1MutationGateIsReadOnlyAndRoutesPublishedRecoveryToUpdate(t *testing.T) {
	options, journalPath, _ := publishedV1CompatibilityFixture(t, "0.4.1")
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := InspectMutationGate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.Status != "recovering" ||
		outcome.Code != "recovery-pending" || outcome.Transaction == nil ||
		outcome.Retry != "run yard update" {
		t.Fatalf("published v1 mutation gate = %#v", outcome)
	}
	after, err := os.ReadFile(journalPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("read-only v1 mutation gate changed the journal: %v", err)
	}
}

func TestV1MutationGateRequiresCompletedV2ImportAfterLivePairChanges(t *testing.T) {
	options, _, _ := publishedV1CompatibilityFixture(t, "0.4.1")
	replaceV1CompatibilityRuntimeLink(
		t, options.RuntimeRoot, "current", "releases/0.5.0-test-release",
	)
	replaceV1CompatibilityRuntimeLink(
		t, options.RuntimeRoot, "previous", testPublishedV1041Runtime,
	)
	outcome, err := InspectMutationGate(context.Background(), options)
	if err != nil || outcome == nil ||
		outcome.Status != releasetransition.StatusOperatorActionRequired {
		t.Fatalf("unproven v1 journal bypassed the mutation gate: outcome=%#v err=%v",
			outcome, err)
	}
	writeCompletedV2ImportProof(t, options, "0.4.1", "0.5.0-test-release")
	outcome, err = InspectMutationGate(context.Background(), options)
	if err != nil || outcome != nil {
		t.Fatalf("completed v2 import still gates ordinary mutations: outcome=%#v err=%v",
			outcome, err)
	}
	if err := os.Remove(filepath.Join(
		options.ConfigHome, "release-transition", "v2", "journal.json",
	)); err != nil {
		t.Fatal(err)
	}
	outcome, err = InspectMutationGate(context.Background(), options)
	if err != nil || outcome != nil {
		t.Fatalf("durable v2 import receipt did not outlive current journal: outcome=%#v err=%v",
			outcome, err)
	}
}

func TestV1MutationGateFailsClosedWhenLivePairIsUnsafe(t *testing.T) {
	options, _, _ := publishedV1CompatibilityFixture(t, "0.4.1")
	current := filepath.Join(options.RuntimeRoot, "current")
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside", current); err != nil {
		t.Fatal(err)
	}
	outcome, err := InspectMutationGate(context.Background(), options)
	if err != nil || outcome == nil || outcome.Status != "operator-action-required" {
		t.Fatalf("unsafe live pair bypassed v1 gate: outcome=%#v err=%v", outcome, err)
	}
}

func TestV1MutationGateHandlesOneCurrentJournalWithoutIndexingLegacy(t *testing.T) {
	options, journalPath, _ := publishedV1CompatibilityFixture(t, "0.4.1")
	tx := readV1CompatibilityTransaction(t, journalPath)
	if err := os.RemoveAll(filepath.Dir(journalPath)); err != nil {
		t.Fatal(err)
	}
	tx.ToRelease = options.Version
	currentPath := transactionPath(options.ConfigHome, options.Version)
	writeV1CompatibilityTransaction(t, currentPath, tx)
	outcome, err := InspectMutationGate(context.Background(), options)
	if err != nil || outcome == nil {
		t.Fatalf("current v1 mutation gate = %#v, err=%v", outcome, err)
	}
}

// publishedV1CompatibilityFixture constructs only the two immutable inputs
// that the read-only importer supports. Mutation/recovery behavior belongs to
// V2 transition tests, not to a second compatibility state machine.
func publishedV1CompatibilityFixture(
	t *testing.T,
	version string,
) (ReleaseOptions, string, string) {
	t.Helper()
	options, legacyRegistration, currentRegistration, _ := typedReleaseMigrationFixture(t, "0")
	if err := os.RemoveAll(filepath.Dir(legacyRegistration)); err != nil {
		t.Fatal(err)
	}
	writeMigrationFixture(t, currentRegistration, "YARD_TEMPLATE=test-vms\n")
	writeMigrationFixture(t, filepath.Join(filepath.Dir(options.Executable), "incus-state"), "current\n")

	registry, err := LoadRegistry(options.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	registry.Migrations[0].Resources = []string{"test-yard-owner", "test-yard-route-consumers"}
	registry.Migrations[0].Operations = append(registry.Migrations[0].Operations, Operation{
		ID: "test-yard-route-consumers", Kind: OperationKindTestYardRouteConsumersV1,
	})
	if version == "0.4.2" {
		registry.CurrentLayout = 3
		if err := os.RemoveAll(filepath.Join(
			options.DataHome, "e2e", "controllers", "e2e-yard",
		)); err != nil {
			t.Fatal(err)
		}
		registry.Migrations = append(registry.Migrations, Definition{
			ID: "refresh-test-vm-broker", FromLayout: 2, ToLayout: 3,
			Resources:      []string{"test-vm-broker-runtime"},
			FinalizePolicy: orderedFinalizePolicy, RollbackPolicy: orderedRollbackPolicy,
			Operations: []Operation{{
				ID: "test-vm-broker-runtime", Kind: OperationKindTestVMBrokerRuntimeV1,
			}},
		})
	}
	writeRegistryFixture(t, options.RegistryPath, registry)

	activateFixtureRelease(t, options)
	publishedRuntime := testPublishedV1041Runtime
	if version == "0.4.2" {
		publishedRuntime = testPublishedV1042Runtime
	}
	publishedPreviousRoot := filepath.Join(options.RuntimeRoot, testPublishedV1PreviousRuntime)
	publishedReleaseRoot := filepath.Join(options.RuntimeRoot, publishedRuntime)
	if err := os.Rename(
		filepath.Join(options.RuntimeRoot, "releases", "1.0.0-test-release"),
		publishedPreviousRoot,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(options.RepositoryRoot, publishedReleaseRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(
		filepath.Join(publishedReleaseRoot, "config", "migrations.json"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	replaceV1CompatibilityRuntimeLink(t, options.RuntimeRoot, "current", publishedRuntime)
	replaceV1CompatibilityRuntimeLink(t, options.RuntimeRoot, "previous", testPublishedV1PreviousRuntime)

	executablePayload, err := os.ReadFile(options.Executable)
	if err != nil {
		t.Fatal(err)
	}
	publishedExecutable := filepath.Join(publishedReleaseRoot, "bin", "yard-engine")
	writeMigrationFixture(t, publishedExecutable, string(executablePayload)+"# published\n")
	if err := os.Chmod(publishedExecutable, 0o700); err != nil {
		t.Fatal(err)
	}
	brokerDigestState := filepath.Join(t.TempDir(), "broker-digest")
	publishedExecutablePayload, err := os.ReadFile(publishedExecutable)
	if err != nil {
		t.Fatal(err)
	}
	publishedDigest := sha256.Sum256(publishedExecutablePayload)
	writeMigrationFixture(t, brokerDigestState, hex.EncodeToString(publishedDigest[:])+"\n")

	candidateRoot := filepath.Join(options.RuntimeRoot, "releases", "0.5.0-test-release")
	candidateRegistry := filepath.Join(candidateRoot, "config", "migrations.json")
	if err := os.MkdirAll(filepath.Dir(candidateRegistry), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRegistryFixture(t, candidateRegistry, registry)
	installV1BrokerRepositoryFixture(t, publishedPreviousRoot, publishedReleaseRoot, candidateRoot)
	options.RepositoryRoot = candidateRoot
	options.RegistryPath = candidateRegistry
	options.Environment = replaceTestEnvironment(
		options.Environment, "MIGRATION_CANDIDATE_REPOSITORY", candidateRoot,
	)
	options.Environment = replaceTestEnvironment(
		options.Environment, "SUBYARD_REPOSITORY_ROOT", candidateRoot,
	)
	options.Environment = replaceTestEnvironment(
		options.Environment, "SUBYARD_CONFIG_DIR", filepath.Join(candidateRoot, "config"),
	)
	writeMigrationFixture(
		t, filepath.Join(options.ConfigHome, "migrations", "state.json"),
		`{"schemaVersion":1,"layout":1,"currentRelease":"`+
			testPublishedV1PreviousRuntime+`"}`,
	)
	routeBefore := `{"schemaVersion":1,"active":true,"consumers":[{"project":"subyard","instance":"yard","yard":"default","mounted":false}]}`
	tx := transaction{
		FromLayout: 1, ToLayout: 2, FromRuntime: testPublishedV1PreviousRuntime,
		ToRelease: version, ToRuntime: publishedRuntime, Phase: "rolling-back",
		Migrations: []string{"migrate-test-yard-owner"}, RollbackOps: true,
		Operations: []transactionOperation{
			{
				MigrationID: "migrate-test-yard-owner", OperationID: "test-yard-owner",
				Kind: OperationKindTestYardOwnerV1, Before: "current", Phase: operationRollingBack,
			},
			{
				MigrationID: "migrate-test-yard-owner", OperationID: "test-yard-route-consumers",
				Kind: OperationKindTestYardRouteConsumersV1, Before: routeBefore,
				Phase: operationRolledBack,
			},
		},
	}
	routeState := filepath.Join(t.TempDir(), "route-state")
	writeMigrationFixture(t, routeState, "unmounted\n")
	installV1CanonicalRouteFixture(t, options.DataHome)
	installV1RouteIncusFixture(t, &options, routeState, brokerDigestState)
	if version == "0.4.2" {
		tx.ToLayout = 3
		tx.Migrations = append(tx.Migrations, "refresh-test-vm-broker")
		tx.Operations[0].Phase = operationCommitted
		tx.Operations[1].Phase = operationCommitted
		tx.Operations = append(tx.Operations, transactionOperation{
			MigrationID: "refresh-test-vm-broker", OperationID: "test-vm-broker-runtime",
			Kind: OperationKindTestVMBrokerRuntimeV1, Before: "active",
			Phase: operationRollingBack,
		})
		writeMigrationFixture(t, routeState, "mounted\n")
	}
	writeV1CompatibilityTransaction(t, transactionPath(options.ConfigHome, tx.ToRelease), tx)
	options.Version = "0.5.0-test"
	return options, transactionPath(options.ConfigHome, version), routeState
}

func installV1BrokerRepositoryFixture(t *testing.T, roots ...string) {
	t.Helper()
	for _, root := range roots {
		for _, relative := range []string{
			"incus.project.env", "subyard.env", "host.env", "ports.env",
			filepath.Join("yards", "profiles", "test-vms.env"),
		} {
			payload, err := os.ReadFile(filepath.Join("..", "..", "config", relative))
			if err != nil {
				t.Fatal(err)
			}
			writeMigrationFixture(t, filepath.Join(root, "config", relative), string(payload))
		}
	}
}

func installV1CanonicalRouteFixture(t *testing.T, dataHome string) {
	t.Helper()
	root := filepath.Join(dataHome, "e2e", "routes", "test-yard")
	generation := filepath.Join(root, ".route-published")
	writeMigrationFixture(t, filepath.Join(generation, "route.tsv"),
		"subyard-e2e-route-v1\nhostname\t10.0.0.2\nport\t22\nhost_key_alias\tsubyard-e2e-bastion\n")
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := ssh.NewPublicKey(privateKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	writeMigrationFixture(t, filepath.Join(generation, "known_hosts"),
		"subyard-e2e-bastion "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey)))+"\n")
	if err := os.Symlink(".route-published", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
}

func installV1RouteIncusFixture(
	t *testing.T,
	options *ReleaseOptions,
	routeState string,
	brokerDigestState string,
) {
	t.Helper()
	incus := filepath.Join(t.TempDir(), "incus")
	writeMigrationFixture(t, incus, `#!/bin/sh
set -eu
case "$*" in
  "project list --format=json") printf '[{"name":"subyard-test-yard"}]\n' ;;
  "list yard-test-yard --project subyard-test-yard --format=json")
    printf '[{"name":"yard-test-yard","status":"RUNNING","config":{"user.subyard.desired_power":"running"}}]\n' ;;
  "exec yard-test-yard --project subyard-test-yard -- systemctl is-active subyard-test-vms-broker.service") printf 'active\n' ;;
  "exec yard-test-yard --project subyard-test-yard -- sha256sum /usr/local/libexec/subyard/test-vms-inner")
    printf '%s  /usr/local/libexec/subyard/test-vms-inner\n' "$(cat "$V1_BROKER_DIGEST_STATE")" ;;
  "list --all-projects --format=json")
    if grep -q '^mounted$' "$V1_ROUTE_STATE"; then
      printf '[{"name":"yard-test-yard","project":"subyard-test-yard","status":"Stopped","config":{"user.subyard.managed":"true","user.subyard.name":"test-yard"},"devices":{"subyard-e2e-routes":{"type":"disk","source":"%s","path":"/var/lib/subyard/e2e-routes","readonly":"true"}},"expanded_devices":{}}]\n' "$V1_ROUTE_SOURCE"
    else
      printf '[{"name":"yard-test-yard","project":"subyard-test-yard","status":"Stopped","config":{"user.subyard.managed":"true","user.subyard.name":"test-yard"},"devices":{},"expanded_devices":{}}]\n'
    fi ;;
  *) exit 2 ;;
esac
`)
	if err := os.Chmod(incus, 0o700); err != nil {
		t.Fatal(err)
	}
	options.Incus = incus
	options.Environment = append(options.Environment,
		"V1_ROUTE_STATE="+routeState,
		"V1_ROUTE_SOURCE="+filepath.Join(options.DataHome, "e2e", "routes"),
		"V1_BROKER_DIGEST_STATE="+brokerDigestState,
	)
}

func replaceV1CompatibilityRuntimeLink(
	t *testing.T,
	runtimeRoot, name, target string,
) {
	t.Helper()
	path := filepath.Join(runtimeRoot, name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if filepath.IsAbs(target) || strings.HasPrefix(target, "../") {
		t.Fatalf("unsafe fixture runtime target %q", target)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func writeCompletedV2ImportProof(
	t *testing.T,
	options ReleaseOptions,
	version string,
	target releasetransition.ReleaseID,
) {
	t.Helper()
	published := testPublishedV1041Runtime
	if version == "0.4.2" {
		published = testPublishedV1042Runtime
	}
	pair := V1ImportRuntimePair{Current: published, Previous: testPublishedV1PreviousRuntime}
	reader, err := NewV1ImportReader(V1ImportOptions{
		ConfigHome: options.ConfigHome, RuntimeRoot: options.RuntimeRoot,
		Facts: V1ImportFactObserverFunc(func(
			context.Context,
			V1ImportFactExpectation,
		) (releasetransition.Fingerprint, error) {
			return "", os.ErrPermission
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	imported, err := reader.InspectBoundStatic(context.Background(), pair)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	from := releaseIDFromRuntimeTarget(published)
	previous := releaseIDFromRuntimeTarget(testPublishedV1PreviousRuntime)
	links := releasetransition.ReleaseLinks{Active: from, Previous: &previous}
	ingress := v1CompletedImportProofIngress{
		facts:  releasetransition.Fingerprint(strings.Repeat("d", sha256.Size*2)),
		static: imported.StaticDigest,
	}
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome: options.ConfigHome,
		Releases: releasetransition.ReleasePair{
			From: from, Previous: &previous, Target: target,
		},
		Direction: releasetransition.DirectionActivateTarget,
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
			return links, nil
		},
		ActivateLinks: func(
			context.Context,
			releasetransition.ReleasePair,
		) (releasetransition.ReleaseLinks, error) {
			links = releasetransition.ReleaseLinks{Active: target, Previous: &from}
			return links, nil
		},
		OwnerRegistration: v1CompletedImportProofOwner{},
		Ingress:           ingress,
		RegistryPayload:   registry,
		ArtifactDigest:    releasetransition.Fingerprint(strings.Repeat("e", sha256.Size*2)),
		NewTransactionID: func() releasetransition.TransactionID {
			return "tx-v1-import-proof"
		},
		VerifyAuthorization: func(
			releasetransition.PlanToken,
			releasetransition.Authorization,
		) bool {
			return true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	goal := releasetransition.Goal{
		Target: target, Direction: releasetransition.DirectionActivateTarget,
	}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(inspection.Blockers) != 0 {
		t.Fatalf("v2 import proof inspection = %#v, err=%v", inspection, err)
	}
	outcome, err := transition.Converge(context.Background(), releasetransition.Execution{
		Plan: inspection.Plan, Authorization: "test-proof",
	})
	if err != nil || outcome.Status != releasetransition.StatusReady {
		t.Fatalf("v2 import proof outcome = %#v, err=%v", outcome, err)
	}
}

type v1CompletedImportProofIngress struct {
	facts  releasetransition.Fingerprint
	static releasetransition.Fingerprint
}

func (ingress v1CompletedImportProofIngress) Inspect(
	context.Context,
	*releasetransition.V2IngressBinding,
) (releasetransition.V2IngressInspection, error) {
	return releasetransition.V2IngressInspection{
		Operations: []releasetransition.V2IngressOperation{{
			Kind: releasetransition.V2LegacyV1Import, Decision: releasetransition.DecisionCanonicalize,
			Expected: ingress.facts, Desired: ingress.facts, Static: ingress.static,
		}},
	}, nil
}

func (ingress v1CompletedImportProofIngress) Observe(
	_ context.Context,
	step releasetransition.V2IngressStep,
) (releasetransition.Fingerprint, error) {
	return step.Expected, nil
}

func (v1CompletedImportProofIngress) Apply(
	context.Context,
	releasetransition.V2IngressStep,
) error {
	return nil
}

func (v1CompletedImportProofIngress) Verify(
	context.Context,
	releasetransition.V2IngressStep,
) error {
	return nil
}

type v1CompletedImportProofOwner struct{}

func (v1CompletedImportProofOwner) Prepare(
	context.Context,
	releasetransition.V2SettingsSnapshotView,
) (releasetransition.OwnerRegistrationObservation, error) {
	return releasetransition.OwnerRegistrationObservation{
		State: releasetransition.OwnerRegistrationAbsent,
	}, nil
}

func (v1CompletedImportProofOwner) Observe(
	context.Context,
	releasetransition.OwnerRegistrationObservation,
) (releasetransition.OwnerRegistrationProgress, error) {
	return releasetransition.OwnerRegistrationExpected, nil
}

func (v1CompletedImportProofOwner) Commit(
	context.Context,
	releasetransition.OwnerRegistrationObservation,
) error {
	return nil
}

func readV1CompatibilityTransaction(t *testing.T, path string) transaction {
	t.Helper()
	tx, _, exists, err := readV1CompatibilityJournal(path)
	if err != nil || !exists {
		t.Fatalf("read v1 fixture journal: exists=%v err=%v", exists, err)
	}
	return tx
}

func writeV1CompatibilityTransaction(t *testing.T, path string, tx transaction) {
	t.Helper()
	tx.SchemaVersion = migrationStateSchema
	payload, err := json.Marshal(tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}
