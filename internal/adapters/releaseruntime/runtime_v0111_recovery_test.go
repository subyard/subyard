package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

const v0111ProcessFixtureEnvironment = "SUBYARD_TEST_V0111_TRANSITION_PROCESS"

type v0111ProcessFixture struct {
	runtimeRoot string
	configHome  string
	statePath   string
	installer   string
	previous    releasetransition.ReleaseID
	source      publishedCandidate
	candidate   publishedCandidate
	sourceTx    releasetransition.TransactionID
	candidateTx releasetransition.TransactionID
	ledger      []byte
	journal     releasetransition.ProtectedSnapshot
	registry    []byte
}

type v0111FixtureBindings struct {
	sourceID          releasetransition.ReleaseID
	candidateID       releasetransition.ReleaseID
	sourceArtifact    releasetransition.Fingerprint
	candidateArtifact releasetransition.Fingerprint
	registry          releasetransition.Fingerprint
	catalog           releasetransition.Fingerprint
}

type v0111FixtureReconciler struct {
	statePath string
	desired   releasetransition.Fingerprint
	fail      bool
}

func (reconciler v0111FixtureReconciler) ID() string { return "runtime-config" }

func (reconciler v0111FixtureReconciler) Observe(
	_ context.Context,
	_ releasetransition.ReleasePair,
	_ releasetransition.ReleaseLinks,
) (releasetransition.V2ActivationObservation, error) {
	actualPayload, err := os.ReadFile(reconciler.statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return releasetransition.V2ActivationObservation{}, err
	}
	actual := v0111FixtureFingerprint("unreconciled")
	if err == nil {
		actual = releasetransition.Fingerprint(strings.TrimSpace(string(actualPayload)))
	}
	return releasetransition.V2ActivationObservation{
		Actual: actual, Desired: reconciler.desired, Converged: actual == reconciler.desired,
	}, nil
}

func (reconciler v0111FixtureReconciler) Reconcile(
	_ context.Context,
	_ releasetransition.ReleaseLinks,
) error {
	if reconciler.fail {
		return errors.New("fixture activation reconciliation failed")
	}
	return os.WriteFile(reconciler.statePath, []byte(reconciler.desired+"\n"), 0o600)
}

type v0111AbsentOwnerRegistration struct{}

func (v0111AbsentOwnerRegistration) Prepare(
	context.Context,
	releasetransition.V2SettingsSnapshotView,
) (releasetransition.OwnerRegistrationObservation, error) {
	return releasetransition.OwnerRegistrationObservation{
		State: releasetransition.OwnerRegistrationAbsent,
	}, nil
}

func (v0111AbsentOwnerRegistration) Observe(
	context.Context,
	releasetransition.OwnerRegistrationObservation,
) (releasetransition.OwnerRegistrationProgress, error) {
	return releasetransition.OwnerRegistrationExpected, nil
}

func (v0111AbsentOwnerRegistration) Commit(
	context.Context,
	releasetransition.OwnerRegistrationObservation,
) error {
	return nil
}

func TestV0111RecoveryUsesRealPublicTransitionAcrossProcesses(t *testing.T) {
	fixture, bindings := newV0111ProcessFixture(t)
	fixture = seedV0111PostActivationScopeJournal(t, fixture, bindings)

	var oldOutput bytes.Buffer
	oldRuntime := New(Config{
		RepositoryRoot: fixture.source.root,
		Environment:    fixture.environment(),
		Installer:      fixture.installer,
		Stdout:         &oldOutput,
		Stderr:         &bytes.Buffer{},
	})
	oldPrepared, err := oldRuntime.PrepareTransition(
		context.Background(),
		[]string{"--check", "--offline", "--version", "0.11.1", "--runtime-root", fixture.runtimeRoot},
		fixture.configHome, "recovery-yard", nil,
	)
	if err != nil {
		t.Fatalf("inspect unfinished source transition: %v", err)
	}
	if err := oldPrepared.Execute(context.Background()); err != nil {
		t.Fatalf("render unfinished source transition: %v", err)
	}
	if err := oldRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	var blocked releasetransition.Inspection
	if err := json.Unmarshal(bytes.TrimSpace(oldOutput.Bytes()), &blocked); err != nil {
		t.Fatalf("decode source inspection %q: %v", oldOutput.String(), err)
	}
	if len(blocked.Blockers) != 1 ||
		blocked.Blockers[0].Resource != "transition.observation-scope" {
		t.Fatalf("source inspection did not reproduce the scope blocker: %#v", blocked)
	}

	var candidateOutput bytes.Buffer
	candidateRuntime := New(Config{
		RepositoryRoot: fixture.candidate.root,
		Environment:    fixture.environment(),
		Installer:      fixture.installer,
		Stdout:         &candidateOutput,
		Stderr:         &bytes.Buffer{},
	})
	prepared, err := candidateRuntime.PrepareTransition(
		context.Background(),
		[]string{"--offline", "--version", "0.11.2", "--runtime-root", fixture.runtimeRoot},
		fixture.configHome, "recovery-yard", nil,
	)
	if err != nil {
		t.Fatalf("prepare standalone candidate recovery: %v", err)
	}
	if !prepared.Changed {
		t.Fatalf("candidate recovery preparation = %#v", prepared)
	}
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatalf("execute standalone candidate recovery: %v", err)
	}
	if err := candidateRuntime.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := releasetransition.NewPOSIXV2Store(fixture.configHome)
	if err != nil {
		t.Fatal(err)
	}
	currentSnapshot, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	current, err := releasetransition.ParseJournal(currentSnapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if current.Transaction != fixture.candidateTx ||
		current.Checkpoint != releasetransition.JournalComplete || len(current.Steps) != 0 ||
		current.ArtifactDigest != bindings.candidateArtifact ||
		current.RegistryDigest != bindings.registry || current.CatalogDigest != bindings.catalog {
		t.Fatalf("terminal candidate journal = %#v", current)
	}
	links, err := releasetransition.NewRuntimeLinkStore(fixture.runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	observedLinks, err := links.Observe()
	if err != nil {
		t.Fatal(err)
	}
	if observedLinks.Active != fixture.candidate.release || observedLinks.Previous == nil ||
		*observedLinks.Previous != fixture.source.release {
		t.Fatalf("terminal runtime links = %#v", observedLinks)
	}
	archiveSnapshot, err := store.ReadSupersededJournal(fixture.candidateTx)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := releasetransition.ParseSupersededJournal(archiveSnapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if archive.Journal.Transaction != fixture.sourceTx ||
		archive.Replacement.Fingerprint != fixture.journal.Fingerprint ||
		!bytes.Equal(mustReadFile(t, filepath.Join(
			fixture.configHome, "release-transition", "v2", "ledger.json",
		)), fixture.ledger) {
		t.Fatalf("recovery did not retain the exact source archive and ledger: %#v", archive)
	}
	if _, err := os.Stat(filepath.Join(
		fixture.configHome, "release-transition", "v2", "transactions", string(fixture.sourceTx),
	)); err != nil {
		t.Fatalf("source transaction evidence was not retained: %v", err)
	}
	if actual := releasetransition.Fingerprint(strings.TrimSpace(string(mustReadFile(t, fixture.statePath)))); actual != v0111FixtureFingerprint("candidate-scope") {
		t.Fatalf("activation config fixed point = %q", actual)
	}

	candidateManifest := mustReadFile(t, filepath.Join(fixture.candidate.root, "runtime-files.sha256"))
	candidateRegistry := mustReadFile(t, filepath.Join(
		fixture.candidate.root, "config", "release-transition.json",
	))
	for _, repositoryRoot := range []string{fixture.source.root, fixture.candidate.root} {
		response := inspectV0111ProcessRuntime(t, repositoryRoot, releasetransition.ProcessRequest{
			SchemaVersion:  releasetransition.ProcessProtocolSchemaV1,
			Mode:           releasetransition.ProcessInspect,
			RuntimeRoot:    fixture.runtimeRoot,
			ConfigHome:     fixture.configHome,
			Yard:           "recovery-yard",
			Target:         fixture.candidate.release,
			Direction:      releasetransition.DirectionActivateTarget,
			ArtifactDigest: v0111FixtureFingerprintBytes(candidateManifest),
			RegistryDigest: v0111FixtureFingerprintBytes(candidateRegistry),
		})
		if response.Inspection == nil || response.Inspection.Outcome == nil ||
			response.Inspection.Outcome.Status != releasetransition.StatusReady ||
			!response.Inspection.Outcome.ReachedGoal {
			t.Fatalf("same-version read from %s = %#v", repositoryRoot, response)
		}
	}
}

func inspectV0111ProcessRuntime(
	t *testing.T,
	repositoryRoot string,
	request releasetransition.ProcessRequest,
) releasetransition.ProcessResponse {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(repositoryRoot, "bin", "yard-engine"), "_release-transition")
	command.Stdin = bytes.NewReader(payload)
	var output, stderr bytes.Buffer
	command.Stdout, command.Stderr = &output, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("read transition from %s: %v: %s", repositoryRoot, err, stderr.String())
	}
	var response releasetransition.ProcessResponse
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response); err != nil {
		t.Fatalf("decode transition read %q: %v", output.String(), err)
	}
	return response
}

func newV0111ProcessFixture(t *testing.T) (v0111ProcessFixture, v0111FixtureBindings) {
	t.Helper()
	root := t.TempDir()
	fixture := v0111ProcessFixture{
		runtimeRoot: filepath.Join(root, "runtime"),
		configHome:  filepath.Join(root, "config"),
		statePath:   filepath.Join(root, "activation-state"),
		installer:   filepath.Join(root, "installer"),
		previous:    "0.9.1-previous",
		sourceTx:    "tx-source-v0111",
		candidateTx: "tx-recovery-v0112",
	}
	fixture.registry = mustReadFile(t, filepath.Join("..", "..", "..", "config", "release-transition.json"))
	if err := os.MkdirAll(filepath.Join(fixture.runtimeRoot, "releases", string(fixture.previous)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", string(fixture.previous)), filepath.Join(fixture.runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	fixture.source = writeV0111ProcessRuntime(
		t, fixture, "0.11.1-source", "0.11.1", fixture.sourceTx, fixture.registry,
	)
	fixture.candidate = writeV0111ProcessRuntime(
		t, fixture, "0.11.2-candidate", "0.11.2", fixture.candidateTx, fixture.registry,
	)
	if err := os.MkdirAll(fixture.configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	bindings := v0111FixtureBindings{
		sourceID:          fixture.source.release,
		candidateID:       fixture.candidate.release,
		sourceArtifact:    v0111FixtureFingerprintBytes(mustReadFile(t, filepath.Join(fixture.source.root, "runtime-files.sha256"))),
		candidateArtifact: v0111FixtureFingerprintBytes(mustReadFile(t, filepath.Join(fixture.candidate.root, "runtime-files.sha256"))),
		registry:          v0111FixtureFingerprintBytes(fixture.registry),
		catalog:           releasetransition.BuiltinCapabilityCatalog().Digest(),
	}
	return fixture, bindings
}

func seedV0111PostActivationScopeJournal(
	t *testing.T,
	fixture v0111ProcessFixture,
	bindings v0111FixtureBindings,
) v0111ProcessFixture {
	t.Helper()
	if fixture.source.release != bindings.sourceID ||
		fixture.candidate.release != bindings.candidateID {
		t.Fatal("fixture release IDs do not match the exact seed bindings")
	}
	links, err := releasetransition.NewRuntimeLinkStore(fixture.runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome: fixture.configHome,
		Releases: releasetransition.ReleasePair{
			From: fixture.previous, Target: fixture.source.release,
		},
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
			return links.Observe()
		},
		ActivateLinks: func(
			_ context.Context,
			pair releasetransition.ReleasePair,
		) (releasetransition.ReleaseLinks, error) {
			return links.Activate(pair)
		},
		Reconcilers: []releasetransition.V2ActivationReconciler{v0111FixtureReconciler{
			statePath: fixture.statePath, desired: v0111FixtureFingerprint("source-scope"), fail: true,
		}},
		OwnerRegistration: v0111AbsentOwnerRegistration{},
		RegistryPayload:   fixture.registry,
		ArtifactDigest:    bindings.sourceArtifact,
		CandidateVersion:  "0.11.1",
		NewTransactionID:  func() releasetransition.TransactionID { return fixture.sourceTx },
		VerifyAuthorization: func(
			releasetransition.PlanToken,
			releasetransition.Authorization,
		) bool {
			return true
		},
	})
	if err != nil {
		t.Fatalf("construct source transition: %v", err)
	}
	goal := releasetransition.Goal{
		Target: fixture.source.release, Direction: releasetransition.DirectionActivateTarget,
	}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatalf("inspect source transition: %v", err)
	}
	outcome, err := transition.Converge(context.Background(), releasetransition.Execution{
		Plan: inspection.Plan, Authorization: "fixture-authorization",
	})
	if err != nil {
		t.Fatalf("converge source transition: %v", err)
	}
	if outcome.Status != releasetransition.StatusRecovering {
		t.Fatalf("source fixture outcome = %#v", outcome)
	}
	store, err := releasetransition.NewPOSIXV2Store(fixture.configHome)
	if err != nil {
		t.Fatal(err)
	}
	fixture.journal, err = store.ReadCurrentJournal()
	if err != nil {
		t.Fatalf("read source journal: %v", err)
	}
	if !fixture.journal.Exists {
		t.Fatalf("source fixture produced no journal; outcome = %#v", outcome)
	}
	journal, err := releasetransition.ParseJournal(fixture.journal.Payload)
	if err != nil {
		t.Fatalf("parse source journal: %v", err)
	}
	if journal.Transaction != fixture.sourceTx ||
		journal.Checkpoint != releasetransition.JournalReconciling || len(journal.Steps) != 2 ||
		journal.ArtifactDigest != bindings.sourceArtifact ||
		journal.RegistryDigest != bindings.registry || journal.CatalogDigest != bindings.catalog {
		t.Fatalf("seeded source journal = %#v", journal)
	}
	for _, step := range journal.Steps {
		if step.Checkpoint != releasetransition.StepVerified || step.Evidence == nil ||
			step.Evidence.Checkpoint != releasetransition.EvidenceVerified {
			t.Fatalf("seeded source step = %#v", step)
		}
	}
	ledger, err := store.ReadLedger()
	if err != nil || !ledger.Exists {
		t.Fatalf("seeded ledger = %#v, %v", ledger, err)
	}
	fixture.ledger = append([]byte(nil), ledger.Payload...)
	if err := os.WriteFile(fixture.installer, []byte("#!/bin/sh\n# --publish-only\nexit 97\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func writeV0111ProcessRuntime(
	t *testing.T,
	fixture v0111ProcessFixture,
	releaseID string,
	version string,
	transaction releasetransition.TransactionID,
	registry []byte,
) publishedCandidate {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine %s\n' ;;
  _release-transition)
    %s=1 \
    SUBYARD_TEST_V0111_PREVIOUS=%q \
    SUBYARD_TEST_V0111_SOURCE=%q \
    SUBYARD_TEST_V0111_CANDIDATE=%q \
	SUBYARD_TEST_V0111_STATE=%q \
	SUBYARD_TEST_V0111_YARD=recovery-yard \
	SUBYARD_TEST_V0111_VERSION=%q \
    SUBYARD_TEST_V0111_TRANSACTION=%q \
    exec %q -test.run '^TestV0111TransitionProcessHelper$'
    ;;
  *) exit 64 ;;
esac
`, version, v0111ProcessFixtureEnvironment, fixture.previous, "0.11.1-source",
		"0.11.2-candidate", fixture.statePath, version, transaction, executable)
	candidate := writeVersionedRuntimeCandidate(t, fixture.runtimeRoot, releaseID, version, payload)
	registryPath := filepath.Join(candidate.root, "config", "release-transition.json")
	if err := os.WriteFile(registryPath, registry, 0o600); err != nil {
		t.Fatal(err)
	}
	engineDigest := sha256.Sum256([]byte(payload))
	registryDigest := sha256.Sum256(registry)
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n", engineDigest, registryDigest)
	if err := os.WriteFile(filepath.Join(candidate.root, "runtime-files.sha256"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return candidate
}

func TestV0111TransitionProcessHelper(t *testing.T) {
	if os.Getenv(v0111ProcessFixtureEnvironment) == "" {
		return
	}
	if err := runV0111TransitionProcessHelper(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runV0111TransitionProcessHelper(input io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(io.LimitReader(input, releasetransition.MaxProtectedRecordBytes))
	decoder.DisallowUnknownFields()
	var request releasetransition.ProcessRequest
	if err := decoder.Decode(&request); err != nil {
		return err
	}
	if request.SchemaVersion != releasetransition.ProcessProtocolSchemaV1 {
		return errors.New("unexpected process protocol schema")
	}
	if request.Yard != os.Getenv("SUBYARD_TEST_V0111_YARD") {
		return fmt.Errorf("process request selected yard %q", request.Yard)
	}
	links, err := releasetransition.NewRuntimeLinkStore(request.RuntimeRoot)
	if err != nil {
		return err
	}
	previous := releasetransition.ReleaseID(os.Getenv("SUBYARD_TEST_V0111_PREVIOUS"))
	source := releasetransition.ReleaseID(os.Getenv("SUBYARD_TEST_V0111_SOURCE"))
	releases := releasetransition.ReleasePair{From: previous, Target: request.Target}
	if request.Replacement != nil {
		releases = releasetransition.ReleasePair{From: source, Previous: &previous, Target: request.Target}
	}
	registry, err := os.ReadFile(filepath.Join(
		request.RuntimeRoot, "releases", string(request.Target), "config", "release-transition.json",
	))
	if err != nil {
		return err
	}
	manifest, err := os.ReadFile(filepath.Join(
		request.RuntimeRoot, "releases", string(request.Target), "runtime-files.sha256",
	))
	if err != nil {
		return err
	}
	if request.ArtifactDigest != v0111FixtureFingerprintBytes(manifest) ||
		(request.RegistryDigest != "" &&
			request.RegistryDigest != v0111FixtureFingerprintBytes(registry)) {
		return errors.New("process request does not match the exact fixture bindings")
	}
	reconciler := v0111FixtureReconciler{
		statePath: os.Getenv("SUBYARD_TEST_V0111_STATE"),
		desired:   v0111FixtureFingerprint("candidate-scope"),
	}
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome: request.ConfigHome, Releases: releases, Direction: request.Direction,
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
			return links.Observe()
		},
		ActivateLinks: func(
			_ context.Context,
			pair releasetransition.ReleasePair,
		) (releasetransition.ReleaseLinks, error) {
			return links.Activate(pair)
		},
		Reconcilers:       []releasetransition.V2ActivationReconciler{reconciler},
		OwnerRegistration: v0111AbsentOwnerRegistration{},
		RegistryPayload:   registry,
		ArtifactDigest:    request.ArtifactDigest,
		CandidateVersion:  os.Getenv("SUBYARD_TEST_V0111_VERSION"),
		Replacement:       request.Replacement,
		NewTransactionID: func() releasetransition.TransactionID {
			return releasetransition.TransactionID(os.Getenv("SUBYARD_TEST_V0111_TRANSACTION"))
		},
		VerifyAuthorization: func(
			_ releasetransition.PlanToken,
			authorization releasetransition.Authorization,
		) bool {
			return authorization != ""
		},
	})
	if err != nil {
		return err
	}
	response := releasetransition.ProcessResponse{
		SchemaVersion:                 releasetransition.ProcessProtocolSchemaV1,
		ActivationReconciliationOwned: true,
	}
	goal := releasetransition.Goal{Target: request.Target, Direction: request.Direction}
	switch request.Mode {
	case releasetransition.ProcessInspect:
		inspection, err := transition.Inspect(context.Background(), goal)
		if err != nil {
			return err
		}
		response.Inspection = &inspection
	case releasetransition.ProcessConverge:
		if request.Execution == nil {
			return errors.New("converge request has no execution")
		}
		outcome, err := transition.Converge(context.Background(), *request.Execution)
		if err != nil {
			return err
		}
		response.Outcome = &outcome
	default:
		return fmt.Errorf("unexpected process mode %q", request.Mode)
	}
	return json.NewEncoder(output).Encode(response)
}

func (fixture v0111ProcessFixture) environment() map[string]string {
	return map[string]string{
		"HOME":               filepath.Dir(fixture.runtimeRoot),
		"YARD_RELEASE_CACHE": filepath.Join(filepath.Dir(fixture.runtimeRoot), "cache"),
	}
}

func v0111FixtureFingerprint(value string) releasetransition.Fingerprint {
	return v0111FixtureFingerprintBytes([]byte(value))
}

func v0111FixtureFingerprintBytes(value []byte) releasetransition.Fingerprint {
	digest := sha256.Sum256(value)
	return releasetransition.Fingerprint(fmt.Sprintf("%x", digest))
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
