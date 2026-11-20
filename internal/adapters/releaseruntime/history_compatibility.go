package releaseruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

// HistoryCompatibilityVerb is the read-only verdict an installer can run from
// a sealed execution image before it has a repository.
const HistoryCompatibilityVerb = "_history-compatibility"

type HistoryCompatibilityReport struct {
	SchemaVersion       int                                        `json:"schemaVersion"`
	Active              releasetransition.ReleaseID                `json:"active"`
	Target              releasetransition.ReleaseID                `json:"target"`
	Kind                releasetransition.HistoryCompatibilityKind `json:"kind"`
	Admitted            bool                                       `json:"admitted"`
	DeclaredPredecessor bool                                       `json:"declaredPredecessor"`
	Refusals            []HistoryCompatibilityRefusal              `json:"refusals"`
}

type HistoryCompatibilityRefusal struct {
	Reason  releasetransition.HistoryRefusalReason `json:"reason"`
	Message string                                 `json:"message"`
}

// RunHistoryCompatibility prints the verdict as JSON. It exits 0 when the
// target is admitted, 1 when it is refused, and 2 for usage or verification
// errors. It takes no lock and writes nothing; a caller that acts on the
// verdict reselects under the owner barrier.
func RunHistoryCompatibility(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) != 3 {
		fmt.Fprintln(stderr, "usage: "+HistoryCompatibilityVerb+" RUNTIME_ROOT CONFIG_HOME TARGET_RELEASE_DIR")
		return 2
	}
	report, err := InspectHistoryCompatibility(arguments[0], arguments[1], arguments[2])
	if err != nil {
		fmt.Fprintf(stderr, "history compatibility: %v\n", err)
		return 2
	}
	payload, err := json.Marshal(report)
	if err == nil {
		_, err = fmt.Fprintf(stdout, "%s\n", payload)
	}
	if err != nil {
		return 2
	}
	if !report.Admitted {
		return 1
	}
	return 0
}

// InspectHistoryCompatibility evaluates whether the sealed release at
// targetRoot, published or in a private draft runtime, may own the active
// migration history of runtimeRoot and configHome.
func InspectHistoryCompatibility(runtimeRoot, configHome, targetRoot string) (HistoryCompatibilityReport, error) {
	if !filepath.IsAbs(runtimeRoot) || !filepath.IsAbs(configHome) || !filepath.IsAbs(targetRoot) {
		return HistoryCompatibilityReport{}, errors.New("runtime root, config home and target release must be absolute")
	}
	runtimeRoot, targetRoot = filepath.Clean(runtimeRoot), filepath.Clean(targetRoot)
	if filepath.Base(filepath.Dir(targetRoot)) != "releases" {
		return HistoryCompatibilityReport{}, errors.New("target must be a release directory under releases/")
	}
	runtime := New(Config{})
	state, err := runtime.readCurrentSnapshot(runtimeRoot, configHome)
	if err != nil {
		return HistoryCompatibilityReport{}, fmt.Errorf("read active migration history: %w", err)
	}
	if !state.links.current.present {
		return HistoryCompatibilityReport{}, errors.New("active release is unavailable")
	}
	activeID := releaseLinksFromRuntimeSnapshot(state.links).Active
	active, err := runtime.verifyPublishedCandidateFiles(publishedCandidate{
		release: activeID, root: filepath.Join(runtimeRoot, state.links.current.target),
	}, runtimeRoot, nil, false)
	if err != nil {
		return HistoryCompatibilityReport{}, fmt.Errorf("verify active release: %w", err)
	}
	defer active.Close()
	targetID := releasetransition.ReleaseID(filepath.Base(targetRoot))
	target, err := runtime.verifyPublishedCandidateFiles(publishedCandidate{release: targetID, root: targetRoot},
		filepath.Dir(filepath.Dir(targetRoot)), nil, false)
	if err != nil {
		return HistoryCompatibilityReport{}, fmt.Errorf("verify target release: %w", err)
	}
	defer target.Close()
	_, activeRegistry, err := selectedRegistry(active)
	if err != nil {
		return HistoryCompatibilityReport{}, fmt.Errorf("the active release's sealed migration registry cannot be verified: %w", err)
	}
	registry, targetRegistry, err := selectedRegistry(target)
	if err != nil {
		return HistoryCompatibilityReport{}, fmt.Errorf("the selected release's sealed migration registry cannot be verified: %w", err)
	}
	declaration, err := verifiedRollbackDeclaration(active)
	if err != nil {
		return HistoryCompatibilityReport{}, fmt.Errorf("the active release's rollback declaration cannot be verified: %w", err)
	}
	input := releasetransition.HistoryCompatibilityInput{
		ActiveRegistry: activeRegistry, TargetRegistry: targetRegistry,
		History: retainedHistoryEvidence(target, registry, state.ledger, state.checkpoint),
	}
	if declaration != nil {
		input.Predecessor = declaration.PredecessorRegistryDigest
	}
	verdict := releasetransition.HistoryCompatibility(input)
	report := HistoryCompatibilityReport{
		SchemaVersion: 1, Active: activeID, Target: targetID, Kind: verdict.Kind,
		Admitted: verdict.Admitted(), DeclaredPredecessor: verdict.DeclaredPredecessor,
		Refusals: []HistoryCompatibilityRefusal{},
	}
	for _, refusal := range verdict.Refusals {
		report.Refusals = append(report.Refusals, HistoryCompatibilityRefusal{Reason: refusal.Reason, Message: refusal.Message})
	}
	return report, nil
}

// retainedHistoryEvidence binds the authoritative history to the target's
// sealed registry and checkpoint-reader marker.
func retainedHistoryEvidence(target *verifiedPublishedCandidate, registry releasetransition.RegistryV2,
	ledger, checkpoint releasetransition.ProtectedSnapshot,
) *releasetransition.HistoryEvidence {
	reader, err := verifiedMigrationCheckpointReader(target)
	return &releasetransition.HistoryEvidence{
		Registry: registry, RegistryDigest: target.registryDigest,
		Ledger: ledger, Checkpoint: checkpoint,
		CheckpointReader: reader, CheckpointReaderErr: err,
	}
}

// verifiedRollbackDeclaration reads config/release-rollback.json the way the
// checkpoint marker is read: only manifest-bound bytes captured during
// verification, unchanged since. A release without it declares no predecessor.
func verifiedRollbackDeclaration(verified *verifiedPublishedCandidate) (*releasetransition.RollbackDeclaration, error) {
	if !verified.rollbackBound {
		return nil, nil
	}
	file, err := openCandidateFile(int(verified.root.Fd()), "config/release-rollback.json")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, releasetransition.MaxRollbackDeclarationBytes+1))
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(payload) != verified.rollbackDigest || !bytes.Equal(payload, verified.rollbackRaw) {
		return nil, errors.New("rollback declaration does not match its verified manifest")
	}
	declaration, err := releasetransition.ParseRollbackDeclaration(payload)
	if err != nil {
		return nil, err
	}
	return &declaration, nil
}
