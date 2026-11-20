package releaseruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/domain"
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
	Registry            releasetransition.HistoryRegistryRelation  `json:"registry"`
	Admitted            bool                                       `json:"admitted"`
	Successor           bool                                       `json:"successor"`
	DeclaredPredecessor bool                                       `json:"declaredPredecessor"`
	DeclaredSuccessor   bool                                       `json:"declaredSuccessor"`
	Refusals            []HistoryCompatibilityRefusal              `json:"refusals"`
}

type HistoryCompatibilityRefusal struct {
	Reason  releasetransition.HistoryRefusalReason `json:"reason"`
	Message string                                 `json:"message"`
	Cause   string                                 `json:"cause,omitempty"`
}

// RunHistoryCompatibility prints the verdict as JSON. It exits 0 when the
// target is admitted, 1 when it is refused, and 2 for usage or verification
// errors. It takes no lock and writes nothing; a caller that acts on the
// verdict reselects under the owner barrier.
func RunHistoryCompatibility(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) != 3 {
		fmt.Fprintln(stderr, "usage: "+HistoryCompatibilityVerb+" RUNTIME_ROOT CONFIG_HOME TARGET_RELEASE")
		return 2
	}
	report, err := InspectHistoryCompatibility(arguments[0], arguments[1], releasetransition.ReleaseID(arguments[2]))
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

// InspectHistoryCompatibility evaluates whether the published release target
// may own the active migration history of runtimeRoot and configHome, with the
// same input assembly as the runtime's installed-engine selection.
func InspectHistoryCompatibility(runtimeRoot, configHome string, targetID releasetransition.ReleaseID) (HistoryCompatibilityReport, error) {
	if !filepath.IsAbs(runtimeRoot) || !filepath.IsAbs(configHome) {
		return HistoryCompatibilityReport{}, errors.New("runtime root and config home must be absolute")
	}
	runtimeRoot, configHome = filepath.Clean(runtimeRoot), filepath.Clean(configHome)
	if info, err := os.Stat(configHome); err != nil || !info.IsDir() {
		return HistoryCompatibilityReport{}, errors.New("config home is not an existing directory")
	}
	runtime := New(Config{})
	state, err := runtime.readCurrentSnapshot(runtimeRoot, configHome)
	if err != nil {
		return HistoryCompatibilityReport{}, fmt.Errorf("read active migration history: %w", err)
	}
	request := releasetransition.ProcessRequest{
		RuntimeRoot: runtimeRoot, ConfigHome: configHome, Target: targetID,
		Direction: releasetransition.DirectionActivateTarget,
	}
	if state.links.previous.present && state.links.previous.target == "releases/"+string(targetID) {
		request.Direction = releasetransition.DirectionActivatePrevious
	}
	active, err := runtime.verifySelectedActive(&releaseSelection{request: request, links: state.links}, nil)
	if err != nil {
		return HistoryCompatibilityReport{}, fmt.Errorf("verify active release: %w", err)
	}
	defer active.Close()
	target, err := runtime.verifyPublishedCandidateFiles(publishedCandidate{
		release: targetID, root: filepath.Join(runtimeRoot, "releases", string(targetID)),
	}, runtimeRoot, nil, false)
	if err != nil {
		return HistoryCompatibilityReport{}, fmt.Errorf("verify target release: %w", err)
	}
	defer target.Close()
	history, refusal, err := assembleHistorySelection(request, state.links, state, active, target)
	if refusal != "" {
		return HistoryCompatibilityReport{}, fmt.Errorf("%s: %w", refusal, err)
	}
	verdict := history.verdict
	report := HistoryCompatibilityReport{
		SchemaVersion: 1, Active: active.candidate.release, Target: targetID, Kind: verdict.Kind,
		Registry: verdict.Registry, Admitted: verdict.Admitted(), Successor: verdict.Successor,
		DeclaredPredecessor: verdict.DeclaredPredecessor, DeclaredSuccessor: verdict.DeclaredSuccessor,
		Refusals: []HistoryCompatibilityRefusal{},
	}
	for _, refusal := range verdict.Refusals {
		entry := HistoryCompatibilityRefusal{Reason: refusal.Reason, Message: refusal.Message}
		if refusal.Cause != nil {
			entry.Cause = refusal.Cause.Error()
		}
		report.Refusals = append(report.Refusals, entry)
	}
	return report, nil
}

// historySelection is the HistoryCompatibility input the runtime boundary
// assembles for an installed target engine.
type historySelection struct {
	activeRegistry, targetRegistry releasetransition.Fingerprint
	missingHistory                 bool
	firstPublication               bool
	publicationErr                 error
	verdict                        releasetransition.HistoryCompatibilityVerdict
}

const rollbackDeclarationRefusal = "a rollback declaration of the release pair cannot be verified"

// assembleHistorySelection is shared by captureTransitionSelection and the
// verb. A non-empty refusal reports a registry that cannot be verified, or an
// Equivalent relation whose rollback declaration cannot be verified.
func assembleHistorySelection(request releasetransition.ProcessRequest, links runtimeLinkSnapshot, state currentSnapshot,
	active, target *verifiedPublishedCandidate,
) (historySelection, string, error) {
	var history historySelection
	var err error
	activeRegistry, activeIdentity, err := selectedRegistry(active)
	history.activeRegistry = activeIdentity
	if err != nil {
		return history, "the active release's sealed migration registry cannot be verified", err
	}
	retained, identity, err := selectedRegistry(target)
	if err != nil {
		return history, "the selected release's sealed migration registry cannot be verified", err
	}
	history.targetRegistry = identity
	// Only an Equivalent relation depends on a declaration, so a read failure
	// refuses only that relation. The active release's declaration is its own
	// and parsed strictly; the target's may come from a newer release, so only
	// its predecessor digest is read.
	predecessor, activeDeclarationErr := declaredPredecessor(active, strictRollbackPredecessor)
	targetPredecessor, targetDeclarationErr := declaredPredecessor(target, releasetransition.ParseRollbackPredecessor)
	history.missingHistory = !state.ledger.Exists && !state.checkpoint.Exists
	// The installer can publish the first current link before the native
	// transition creates protected history. Let that same engine initialize it.
	// Current then names the target, so its registry is the active one.
	firstPublication := request.Direction == releasetransition.DirectionActivateTarget &&
		request.Replacement == nil && request.MigrationOwnerReplacement == nil &&
		links.current.target == "releases/"+string(request.Target) && !links.previous.present &&
		!state.journal.Exists && history.missingHistory
	if identity != "" && firstPublication {
		history.firstPublication, history.publicationErr = solePublishedRelease(request.RuntimeRoot, request.Target)
	}
	input := releasetransition.HistoryCompatibilityInput{
		Predecessor: predecessor.Digest, TargetPredecessor: targetPredecessor.Digest,
		TargetRegistryFeatures: targetPredecessor.RegistryFeatures,
	}
	if activeIdentity != "" {
		input.Active = &activeRegistry
	}
	if identity != "" {
		input.Target = &retained
	}
	if !history.firstPublication {
		input.History = retainedHistoryEvidence(target, retained, state.ledger, state.checkpoint)
	}
	history.verdict = releasetransition.HistoryCompatibility(input)
	// The older release of an Equivalent pair is named by the newer one's
	// declaration, so only that declaration can refuse it.
	declarationErr := activeDeclarationErr
	if history.verdict.Successor {
		declarationErr = targetDeclarationErr
	}
	if declarationErr != nil && history.verdict.Kind == releasetransition.HistoryEquivalent {
		return history, rollbackDeclarationRefusal, declarationErr
	}
	return history, "", nil
}

// rollbackPredecessorParser reads the predecessor digest and registry
// features from declaration bytes.
type rollbackPredecessorParser func([]byte) (releasetransition.RollbackPredecessor, error)

// strictRollbackPredecessor parses a release's own declaration in full.
func strictRollbackPredecessor(payload []byte) (releasetransition.RollbackPredecessor, error) {
	declaration, err := releasetransition.ParseRollbackDeclaration(payload)
	if err != nil {
		return releasetransition.RollbackPredecessor{}, err
	}
	return releasetransition.RollbackPredecessor{
		Digest: declaration.PredecessorRegistryDigest, RegistryFeatures: declaration.RegistryFeatures,
	}, nil
}

// needsSourceFloor reports when the source floor's upgrade advice may replace
// the selection: a registry relation that is not admitted, or an installed
// registry without history.
func (history historySelection) needsSourceFloor() bool {
	return !history.verdict.RegistryAdmitted() ||
		(history.targetRegistry != "" && history.missingHistory)
}

// refusal is the selection's refusal. A refused registry relation is decided
// before first-publication evidence, so that evidence's error is not reported then.
func (history historySelection) refusal() (string, error) {
	message, cause := history.verdict.Refusal()
	if history.verdict.RegistryAdmitted() {
		cause = domain.CollectRefusals(cause, history.publicationErr)
	}
	return message, cause
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

// declaredPredecessor is the predecessor registry digest a verified release
// declares. It reads config/release-rollback.json the way the checkpoint
// marker is read: only manifest-bound bytes captured during verification,
// unchanged since. A release without it declares no predecessor.
func declaredPredecessor(verified *verifiedPublishedCandidate, parse rollbackPredecessorParser) (releasetransition.RollbackPredecessor, error) {
	if !verified.rollbackBound {
		return releasetransition.RollbackPredecessor{}, nil
	}
	return readRollbackPredecessor(int(verified.root.Fd()), func(payload []byte) error {
		if sha256.Sum256(payload) != verified.rollbackDigest || !bytes.Equal(payload, verified.rollbackRaw) {
			return errors.New("rollback declaration does not match its verified manifest")
		}
		return nil
	}, parse)
}

// readRollbackPredecessor reads config/release-rollback.json below a release
// root without following links, bounded, checks the bytes and parses its
// predecessor digest.
func readRollbackPredecessor(rootFD int, check func([]byte) error, parse rollbackPredecessorParser) (releasetransition.RollbackPredecessor, error) {
	file, err := openCandidateFile(rootFD, "config/release-rollback.json")
	if err != nil {
		return releasetransition.RollbackPredecessor{}, err
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, releasetransition.MaxRollbackDeclarationBytes+1))
	if err != nil {
		return releasetransition.RollbackPredecessor{}, err
	}
	if err := check(payload); err != nil {
		return releasetransition.RollbackPredecessor{}, err
	}
	return parse(payload)
}

// EngineRollbackPredecessor reads the rollback declaration of the running
// engine's own release root with the reader verified releases use; the engine
// already trusts its root. A release without one declares no predecessor.
func EngineRollbackPredecessor(releaseRoot string) (releasetransition.Fingerprint, error) {
	root, err := os.Open(releaseRoot)
	if err != nil {
		return "", fmt.Errorf("open release root: %w", err)
	}
	defer root.Close()
	predecessor, err := readRollbackPredecessor(int(root.Fd()), func([]byte) error { return nil }, strictRollbackPredecessor)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read release rollback declaration: %w", err)
	}
	return predecessor.Digest, nil
}
