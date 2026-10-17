package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/blang/semver/v4"
)

// VerifyMigrationOwnerReplacementAssets runs inside the candidate process on
// every native assessment, including the final assessment under the update lock.
// It reads exact sealed assets directly; a caller's version assertion is not proof.
func VerifyMigrationOwnerReplacementAssets(ctx context.Context, repositoryRoot string, request releasetransition.ProcessRequest) (releasetransition.MigrationOwnerAssetsProof, error) {
	inspector := NewSourceInspector(repositoryRoot, nil)
	runtime := inspector.runtime
	defer inspector.Close()
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		return releasetransition.MigrationOwnerAssetsProof{}, err
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists || request.MigrationOwnerReplacement == nil || snapshot.Fingerprint != request.MigrationOwnerReplacement.Fingerprint {
		return releasetransition.MigrationOwnerAssetsProof{}, errors.New("migration owner predecessor changed")
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || journal.ArtifactDigest != releasetransition.AffectedMigrationOwnerManifest || journal.RegistryDigest != releasetransition.AffectedMigrationOwnerRegistry {
		return releasetransition.MigrationOwnerAssetsProof{}, errors.New("unsupported migration owner artifact")
	}
	old := publishedCandidate{release: journal.Goal.Target, root: filepath.Join(request.RuntimeRoot, "releases", string(journal.Goal.Target))}
	verifiedOld, err := runtime.verifyPublishedCandidate(ctx, old, request.RuntimeRoot, &journal.ArtifactDigest)
	if err != nil {
		return releasetransition.MigrationOwnerAssetsProof{}, err
	}
	defer verifiedOld.Close()
	if verifiedOld.version != "0.21.0" || verifiedOld.registryDigest != journal.RegistryDigest || !strings.HasPrefix(string(old.release), "0.21.0-") {
		return releasetransition.MigrationOwnerAssetsProof{}, errors.New("affected migration owner identity differs")
	}
	candidate, ok := runtime.verifiedStandaloneCandidateRoot(ctx, request.RuntimeRoot)
	if !ok {
		return releasetransition.MigrationOwnerAssetsProof{}, errors.New("migration owner replacement requires a verified standalone candidate")
	}
	defer candidate.Close()
	version, err := semver.Parse(candidate.version)
	if err != nil || !version.GT(semver.MustParse("0.21.0")) || candidate.candidate.release != request.Target || candidate.manifestDigest != request.ArtifactDigest || candidate.registryDigest != journal.RegistryDigest {
		return releasetransition.MigrationOwnerAssetsProof{}, errors.New("migration owner candidate identity differs")
	}
	source := publishedCandidate{release: journal.Releases.From, root: filepath.Join(request.RuntimeRoot, "releases", string(journal.Releases.From))}
	verifiedSource, err := runtime.verifyPublishedCandidate(ctx, source, request.RuntimeRoot, nil)
	if err != nil {
		return releasetransition.MigrationOwnerAssetsProof{}, err
	}
	defer verifiedSource.Close()
	sourceInputs, err := sourceCandidateInputs(verifiedSource)
	if err != nil {
		return releasetransition.MigrationOwnerAssetsProof{}, err
	}
	sourceRegistry, err := os.ReadFile(filepath.Join(source.root, "config", "release-transition.json"))
	if err != nil {
		return releasetransition.MigrationOwnerAssetsProof{}, err
	}
	registryDigest := sha256.Sum256(sourceRegistry)
	if releasetransition.Fingerprint(hex.EncodeToString(registryDigest[:])) != verifiedSource.registryDigest {
		return releasetransition.MigrationOwnerAssetsProof{}, errors.New("retained source registry changed")
	}
	var previousProof *releasetransition.MigrationOwnerPreviousSource
	var previousInputs any
	if journal.Releases.Previous != nil {
		previous := publishedCandidate{release: *journal.Releases.Previous, root: filepath.Join(request.RuntimeRoot, "releases", string(*journal.Releases.Previous))}
		verifiedPrevious, err := runtime.verifyPublishedCandidate(ctx, previous, request.RuntimeRoot, nil)
		if err != nil {
			return releasetransition.MigrationOwnerAssetsProof{}, err
		}
		defer verifiedPrevious.Close()
		previousRegistry, err := os.ReadFile(filepath.Join(previous.root, "config", "release-transition.json"))
		if err != nil || !bytes.Equal(previousRegistry, sourceRegistry) || verifiedPrevious.registryDigest != verifiedSource.registryDigest {
			return releasetransition.MigrationOwnerAssetsProof{}, errors.New("journal-bound previous source does not have the identical sealed registry")
		}
		previousInputs, err = sourceCandidateInputs(verifiedPrevious)
		if err != nil {
			return releasetransition.MigrationOwnerAssetsProof{}, err
		}
		payload, err := json.Marshal(previousInputs)
		if err != nil {
			return releasetransition.MigrationOwnerAssetsProof{}, err
		}
		digest := sha256.Sum256(payload)
		previousProof = &releasetransition.MigrationOwnerPreviousSource{Release: previous.release, Artifact: verifiedPrevious.manifestDigest,
			Registry: verifiedPrevious.registryDigest, Inputs: releasetransition.Fingerprint(hex.EncodeToString(digest[:]))}
	}
	oldInputs, err := sourceCandidateInputs(verifiedOld)
	if err != nil {
		return releasetransition.MigrationOwnerAssetsProof{}, err
	}
	newInputs, err := sourceCandidateInputs(candidate)
	if err != nil {
		return releasetransition.MigrationOwnerAssetsProof{}, err
	}
	payload, err := json.Marshal(struct{ Old, Candidate, Source, Previous any }{oldInputs, newInputs, sourceInputs, previousInputs})
	digest := sha256.Sum256(payload)
	return releasetransition.MigrationOwnerAssetsProof{Binding: releasetransition.Fingerprint(hex.EncodeToString(digest[:])), SourceRegistry: sourceRegistry, PreviousSource: previousProof}, err
}

func (runtime *Runtime) prepareMigrationOwnerReplacement(ctx context.Context, parsed options, configHome, yard string, inherited []string) (Prepared, bool, error) {
	if !parsed.offline || !parsed.versionExplicit || parsed.version == "" || parsed.rollback || parsed.force || (parsed.tag != "" && parsed.tag != "v"+parsed.version) {
		return Prepared{}, false, nil
	}
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		return Prepared{}, false, nil
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return Prepared{}, false, nil
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || journal.Checkpoint != releasetransition.JournalReconciling || journal.ArtifactDigest != releasetransition.AffectedMigrationOwnerManifest || journal.RegistryDigest != releasetransition.AffectedMigrationOwnerRegistry {
		return Prepared{}, false, nil
	}
	canonical, err := releasetransition.MarshalJournal(journal)
	if err != nil || !bytes.Equal(canonical, snapshot.Payload) {
		return Prepared{}, true, errors.New("migration owner predecessor is not canonical")
	}
	candidate, ok := runtime.verifiedStandaloneCandidateRoot(ctx, parsed.root)
	if !ok {
		return Prepared{}, false, nil
	}
	defer candidate.Close()
	version, err := semver.Parse(candidate.version)
	if err != nil || !version.GT(semver.MustParse("0.21.0")) || candidate.version != parsed.version {
		return Prepared{}, false, nil
	}
	probe := releasetransition.RecoveryProcessRequest{SchemaVersion: releasetransition.ProcessRecoverySchemaV2, Mode: releasetransition.RecoveryProcessMigrationOwnerCapabilities}
	response, err := runtime.invokeVerifiedRecoveryTransition(ctx, candidate, parsed.root, probe, "")
	if err != nil || response.Capabilities == nil || *response.Capabilities != releasetransition.MigrationOwnerReplacementCapabilities() {
		return Prepared{}, true, errors.New("standalone candidate does not negotiate migration owner replacement")
	}
	request := releasetransition.RecoveryProcessRequest{SchemaVersion: releasetransition.ProcessRecoverySchemaV2, Mode: releasetransition.RecoveryProcessInspect,
		Contract: releasetransition.MigrationOwnerReplacementContractV1, RuntimeRoot: parsed.root, ConfigHome: configHome, Yard: yard,
		Target: candidate.candidate.release, Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: candidate.manifestDigest, RegistryDigest: candidate.registryDigest,
		InheritedSettingIDs: inherited, MigrationOwner: &releasetransition.MigrationOwnerReplacementRequest{Transaction: journal.Transaction, Fingerprint: snapshot.Fingerprint}}
	response, err = runtime.invokeVerifiedRecoveryTransition(ctx, candidate, parsed.root, request, "")
	if err != nil {
		return Prepared{}, true, err
	}
	if response.Inspection == nil || response.Outcome != nil {
		return Prepared{}, true, errors.New("migration owner candidate returned invalid inspection")
	}
	verification := candidateVerification{candidate: candidate.candidate, digest: candidate.manifestDigest, version: candidate.version, registryDigest: candidate.registryDigest}
	prepared, err := runtime.prepareInspectedCandidateTransitionWithDelegate(parsed, verification, verification, request.TransitionRequest(), *response.Inspection, response.ActivationReconciliationOwned, nil, nil, &request)
	return prepared, true, err
}

func validateMigrationOwnerConvergence(request releasetransition.RecoveryProcessRequest, outcome releasetransition.Outcome) error {
	if outcome.Transaction == nil || *outcome.Transaction == request.MigrationOwner.Transaction {
		if outcome.Status == releasetransition.StatusReady {
			return errors.New("migration owner replacement returned ready without successor")
		}
		return nil
	}
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		return err
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return errors.New("migration owner successor is unavailable")
	}
	current, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || current.Transaction != *outcome.Transaction || current.Goal.Target != request.Target || current.ArtifactDigest != request.ArtifactDigest || current.RegistryDigest != request.RegistryDigest || current.AuthorizationPlan != request.Execution.Plan {
		return errors.New("migration owner successor differs from inspected candidate and grant")
	}
	if err := store.ValidateMigrationOwnerSuccessor(current, *request.MigrationOwner); err != nil {
		return err
	}
	if outcome.Status == releasetransition.StatusReady && current.Checkpoint != releasetransition.JournalComplete {
		return errors.New("migration owner successor is not durably complete")
	}
	return nil
}
