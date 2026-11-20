package releaseruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	// The registry is verbatim from published v0.11.1 b4f203baa0cd81c383fe4a8426b96d855611542e.
	// This process fixture runs the current compiled catalog; the owning core
	// recovery tests separately exercise the exact historical catalog.
	fixture.registry = mustReadFile(t, filepath.Join("testdata", "v0.11.1-release-transition.json"))
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
		RegisteredYards: []releasetransition.V2ActivationReconciler{}, GuestDiskTmp: []releasetransition.V2ActivationReconciler{},
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
