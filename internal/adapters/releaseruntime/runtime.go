package releaseruntime

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/blang/semver/v4"
)

type Config struct {
	Environment    map[string]string
	Installer      string
	RepositoryRoot string
	Stdout         io.Writer
	Stderr         io.Writer
	HTTPClient     *http.Client
}

type Prepared struct {
	Action         domain.ActionID
	Changed        bool
	Consequences   []string
	RefreshConfigs bool
	ActiveLauncher string
	run            func(context.Context) error
}

func (prepared Prepared) Execute(ctx context.Context) error {
	if prepared.run == nil {
		return errors.New("release operation was not prepared")
	}
	return prepared.run(ctx)
}

type Runtime struct {
	config                 Config
	pinnedCandidateRoot    *os.File
	pinnedCandidateRelease releasetransition.ReleaseID
}

func New(config Config) *Runtime {
	if config.HTTPClient == nil {
		config.HTTPClient = http.DefaultClient
	}
	return &Runtime{config: config}
}

func (runtime *Runtime) Close() error {
	if runtime == nil || runtime.pinnedCandidateRoot == nil {
		return nil
	}
	root := runtime.pinnedCandidateRoot
	runtime.pinnedCandidateRoot = nil
	runtime.pinnedCandidateRelease = ""
	return root.Close()
}

type protectedTransitionInspection struct {
	journal                       releasetransition.JournalRecord
	journalSnapshot               releasetransition.ProtectedSnapshot
	owner                         candidateVerification
	target                        candidateVerification
	request                       releasetransition.ProcessRequest
	inspection                    releasetransition.Inspection
	activationReconciliationOwned bool
}

type redactedReleaseInspectionError struct{ cause error }

type publicReleaseInspectionError struct {
	cause   error
	outcome releasetransition.Outcome
}

func (err publicReleaseInspectionError) Error() string { return err.cause.Error() }

func (err publicReleaseInspectionError) Unwrap() error { return err.cause }

func (err redactedReleaseInspectionError) Error() string {
	return "release transition inspection failed"
}

func (err redactedReleaseInspectionError) Unwrap() error { return err.cause }

func redactReleaseInspectionError(err error) error {
	if err == nil {
		return nil
	}
	return redactedReleaseInspectionError{cause: err}
}

// InspectMutationGate reads an existing protected v2 transition and delegates
// its exact read-only inspection to the verified transition owner. A nil
// outcome means there is no v2 journal or its complete state is semantically ready.
func (runtime *Runtime) InspectMutationGate(
	ctx context.Context,
	runtimeRoot string,
	configHome string,
	yard string,
	inheritedSettingIDs []string,
) (*releasetransition.Outcome, error) {
	inspection, err := runtime.inspectProtectedTransition(
		ctx, runtimeRoot, configHome, yard, inheritedSettingIDs,
	)
	if err != nil {
		var public publicReleaseInspectionError
		if errors.As(err, &public) {
			return &public.outcome, nil
		}
		return nil, redactReleaseInspectionError(err)
	}
	if inspection == nil {
		return nil, nil
	}
	outcome := *inspection.inspection.Outcome
	if outcome.Status == releasetransition.StatusReady {
		return nil, nil
	}
	return &outcome, nil
}

func (runtime *Runtime) inspectProtectedTransition(
	ctx context.Context,
	runtimeRoot string,
	configHome string,
	yard string,
	inheritedSettingIDs []string,
) (*protectedTransitionInspection, error) {
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		return nil, err
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return nil, newPublicReleaseInspectionError(
			err, runtimeRoot, "unknown", nil, releasetransition.CodeJournalInvalid,
			"the protected release transition journal cannot be inspected safely",
			"restore protected release metadata from backup, then run yard update --check",
		)
	}
	if !snapshot.Exists {
		return nil, nil
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil {
		return nil, newPublicReleaseInspectionError(
			err, runtimeRoot, "unknown", nil, releasetransition.CodeJournalInvalid,
			"the protected release transition journal is invalid",
			"restore protected release metadata from backup, then run yard update --check",
		)
	}
	root, err := validateReleaseRoot(runtimeRoot, "runtime root")
	if err != nil {
		return nil, newPublicReleaseInspectionError(
			err, runtimeRoot, journal.Goal.Target, &journal.Transaction,
			releasetransition.CodeDependencyUnavailable,
			"the journal-selected release store cannot be inspected safely",
			"restore the journal-selected release, then run yard update --check",
		)
	}
	target := publishedCandidate{
		release: journal.Goal.Target,
		root:    filepath.Join(root, "releases", string(journal.Goal.Target)),
	}
	verifiedTarget, err := runtime.verifyPublishedCandidate(
		ctx, target, root, &journal.ArtifactDigest,
	)
	if err != nil {
		return nil, newPublicReleaseInspectionError(
			fmt.Errorf("verify journal-selected release: %w", err), runtimeRoot,
			journal.Goal.Target, &journal.Transaction,
			releasetransition.CodeDependencyUnavailable,
			"the journal-selected release is unavailable or unverified",
			"restore the journal-selected release, then run yard update --check",
		)
	}
	defer verifiedTarget.Close()
	if !strings.HasPrefix(string(target.release), verifiedTarget.version+"-") {
		return nil, newPublicReleaseInspectionError(
			errors.New("published release name does not match the verified engine version"),
			runtimeRoot, journal.Goal.Target, &journal.Transaction,
			releasetransition.CodeDependencyUnavailable,
			"the journal-selected release identity is inconsistent",
			"restore the journal-selected release, then run yard update --check",
		)
	}
	publicCandidateFailure := func(cause error) error {
		return newPublicReleaseInspectionError(
			cause, runtimeRoot, journal.Goal.Target, &journal.Transaction,
			releasetransition.CodeDependencyUnavailable,
			"the journal-selected release cannot provide a valid transition inspection",
			"restore the journal-selected release, then run yard update --check",
		)
	}
	verifiedOwner := verifiedTarget
	owner := target
	if verifiedTarget.registryDigest != journal.RegistryDigest {
		if journal.Goal.Direction != releasetransition.DirectionActivatePrevious {
			return nil, publicCandidateFailure(
				errors.New("journal target does not own the protected transition registry"),
			)
		}
		owner = publishedCandidate{
			release: journal.Releases.From,
			root:    filepath.Join(root, "releases", string(journal.Releases.From)),
		}
		verifiedOwner, err = runtime.verifyPublishedCandidate(ctx, owner, root, nil)
		if err != nil {
			return nil, publicCandidateFailure(
				fmt.Errorf("verify journal transition owner: %w", err),
			)
		}
		defer verifiedOwner.Close()
		if !strings.HasPrefix(string(owner.release), verifiedOwner.version+"-") ||
			verifiedOwner.registryDigest != journal.RegistryDigest {
			return nil, publicCandidateFailure(
				errors.New("journal transition owner does not match the protected registry"),
			)
		}
	}
	request := releasetransition.ProcessRequest{
		SchemaVersion:       releasetransition.ProcessProtocolSchemaV1,
		Mode:                releasetransition.ProcessInspect,
		RuntimeRoot:         root,
		ConfigHome:          configHome,
		Yard:                yard,
		Target:              journal.Goal.Target,
		Direction:           journal.Goal.Direction,
		ArtifactDigest:      journal.ArtifactDigest,
		RegistryDigest:      journal.RegistryDigest,
		InheritedSettingIDs: slices.Clone(inheritedSettingIDs),
		SourceIngress:       journal.SourceIngress,
	}
	response, err := runtime.invokeVerifiedCandidateTransition(ctx, verifiedOwner, request, "")
	if err != nil {
		return nil, publicCandidateFailure(err)
	}
	if response.Inspection == nil || response.Outcome != nil {
		return nil, publicCandidateFailure(errors.New("candidate returned an invalid release inspection"))
	}
	inspection := response.Inspection
	if err := inspection.ValidateOutcome(journal.Goal); err != nil {
		return nil, publicCandidateFailure(
			fmt.Errorf("candidate returned an inconsistent release inspection: %w", err),
		)
	}
	if inspection.Outcome.Transaction == nil ||
		*inspection.Outcome.Transaction != journal.Transaction {
		return nil, publicCandidateFailure(
			errors.New("candidate returned an inconsistent release inspection transaction"),
		)
	}
	if journal.Checkpoint != releasetransition.JournalComplete {
		if inspection.Resume == nil || *inspection.Resume != journal.Transaction ||
			inspection.Plan != journal.ResumePlan {
			return nil, publicCandidateFailure(
				errors.New("candidate returned an inconsistent release inspection resume plan"),
			)
		}
	} else if inspection.Resume != nil {
		return nil, publicCandidateFailure(
			errors.New("candidate returned a resume plan for a complete release transition"),
		)
	}
	return &protectedTransitionInspection{
		journal: journal, journalSnapshot: snapshot,
		owner: candidateVerification{
			candidate: owner, digest: verifiedOwner.manifestDigest, version: verifiedOwner.version,
		},
		target: candidateVerification{
			candidate: target, digest: verifiedTarget.manifestDigest, version: verifiedTarget.version,
		},
		request: request, inspection: *inspection,
		activationReconciliationOwned: response.ActivationReconciliationOwned,
	}, nil
}

func newPublicReleaseInspectionError(
	cause error,
	runtimeRoot string,
	target releasetransition.ReleaseID,
	transaction *releasetransition.TransactionID,
	code releasetransition.OutcomeCode,
	message string,
	retry string,
) error {
	outcome := observedPublicReleaseOutcome(
		runtimeRoot, target, transaction, code, message, retry,
	)
	return publicReleaseInspectionError{cause: cause, outcome: outcome}
}

func observedPublicReleaseOutcome(
	runtimeRoot string,
	target releasetransition.ReleaseID,
	transaction *releasetransition.TransactionID,
	code releasetransition.OutcomeCode,
	message string,
	retry string,
) releasetransition.Outcome {
	links := releasetransition.ReleaseLinks{}
	if store, err := releasetransition.NewRuntimeLinkStore(runtimeRoot); err == nil {
		if observed, observeErr := store.Observe(); observeErr == nil {
			links = observed
		}
	}
	return publicReleaseOutcome(links, target, transaction, code, message, retry)
}

func publicReleaseOutcome(
	links releasetransition.ReleaseLinks,
	target releasetransition.ReleaseID,
	transaction *releasetransition.TransactionID,
	code releasetransition.OutcomeCode,
	message string,
	retry string,
) releasetransition.Outcome {
	return releasetransition.Outcome{
		Status: releasetransition.StatusOperatorActionRequired,
		Active: links.Active, Previous: links.Previous, Target: target,
		Code: code, Message: message, Retry: retry, Transaction: transaction,
	}
}

// PrepareTransition performs download, verification, immutable publication and
// candidate-owned inspection before the caller asks for confirmation. Execute
// then carries only the exact inspected plan and a fresh opaque confirmation
// grant into candidate-owned convergence.
func (runtime *Runtime) PrepareTransition(
	ctx context.Context,
	arguments []string,
	configHome string,
	yard string,
	inheritedSettingIDs []string,
) (Prepared, error) {
	parsed, help, err := runtime.parse(arguments)
	if err != nil {
		return Prepared{}, err
	}
	if help {
		return runtime.prepareHelp(), nil
	}
	if !filepath.IsAbs(configHome) {
		return Prepared{}, errors.New("release transition config home must be absolute")
	}
	protected, err := runtime.inspectProtectedTransition(
		ctx, parsed.root, configHome, yard, inheritedSettingIDs,
	)
	if err != nil {
		var public publicReleaseInspectionError
		if errors.As(err, &public) {
			if parsed.check {
				return runtime.preparePublicInspectionOutcome(public.outcome), nil
			}
			return Prepared{}, transitionOutcomeError(public.outcome)
		}
		return Prepared{}, redactReleaseInspectionError(err)
	}
	if protected != nil {
		unfinished := protected.journal.Checkpoint != releasetransition.JournalComplete ||
			protected.inspection.Outcome.Status != releasetransition.StatusReady
		exactCompletedTarget := !parsed.offline && protected.journal.SourceIngress != nil &&
			parsed.version != "" && parsed.version == protected.target.version &&
			(parsed.tag == "" || parsed.tag == "v"+protected.target.version) && !parsed.rollback && !parsed.force
		if unfinished || exactCompletedTarget {
			if recovery, qualified := runtime.qualifyV0111StandaloneRecovery(
				ctx, parsed, protected,
			); qualified {
				defer recovery.candidate.Close()
				return runtime.prepareVerifiedReplacementTransition(
					ctx, parsed, recovery.candidate, recovery.request, recovery.revalidation,
				)
			}
			runtime.setV0111StandaloneRetry(ctx, parsed.root, protected)
			return runtime.prepareProtectedTransition(parsed, protected)
		}
	}
	if parsed.rollback {
		return runtime.prepareRetainedTransition(
			ctx, parsed, configHome, yard, inheritedSettingIDs,
		)
	}
	sourceIngress, err := sourceIngressRequestFromEnvironment(runtime.config.Environment)
	if err != nil {
		return Prepared{}, err
	}
	current, err := inspectRuntimeLink(parsed.root, "current")
	if err != nil {
		return Prepared{}, err
	}
	if !current.present {
		return Prepared{}, errors.New(
			"release-transition-uninitialized: current runtime is missing; use the verified bootstrap installer",
		)
	}
	publishOnly, err := installerSupportsPublishOnly(runtime.config.Installer)
	if err != nil {
		return Prepared{}, err
	}
	if !publishOnly {
		return Prepared{}, errors.New(
			"release-transition-unsupported: active runtime requires a bridge release with immutable publication support",
		)
	}
	if parsed.version == "" {
		if parsed.offline {
			return Prepared{}, errors.New("offline mode requires --version")
		}
		parsed.tag, err = runtime.latestTag(ctx, parsed.repository)
		if err != nil {
			return Prepared{}, err
		}
		parsed.version = strings.TrimPrefix(parsed.tag, "v")
	} else if parsed.tag == "" {
		parsed.tag = "v" + parsed.version
	}
	if !safeVersion(parsed.version) {
		return Prepared{}, fmt.Errorf("unsafe version %q", parsed.version)
	}
	if err := requirePreparedReleaseRoots(parsed, true); err != nil {
		return Prepared{}, err
	}
	candidate, _, err := runtime.publishCandidate(ctx, parsed)
	if err != nil {
		return Prepared{}, err
	}
	verified, err := runtime.verifyPublishedCandidate(ctx, candidate, parsed.root, nil)
	if err != nil {
		return Prepared{}, fmt.Errorf("published runtime is not verified: %w", err)
	}
	defer verified.Close()
	if verified.version != parsed.version {
		return Prepared{}, errors.New("published runtime version does not match the requested release")
	}
	if !strings.HasPrefix(string(candidate.release), verified.version+"-") {
		return Prepared{}, errors.New("published release name does not match the verified engine version")
	}
	request := releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Mode:          releasetransition.ProcessInspect,
		RuntimeRoot:   parsed.root, ConfigHome: configHome, Yard: yard,
		Target: candidate.release, Direction: releasetransition.DirectionActivateTarget,
		ArtifactDigest:      verified.manifestDigest,
		InheritedSettingIDs: slices.Clone(inheritedSettingIDs),
		SourceIngress:       sourceIngress,
	}
	prepared, err := runtime.prepareVerifiedCandidateTransition(ctx, parsed, verified, request)
	if err == nil {
		return prepared, nil
	}
	var public publicReleaseInspectionError
	if errors.As(err, &public) {
		if parsed.check {
			return runtime.preparePublicInspectionOutcome(public.outcome), nil
		}
		return Prepared{}, transitionOutcomeError(public.outcome)
	}
	outcome := observedPublicReleaseOutcome(
		parsed.root, request.Target, nil,
		releasetransition.CodeDependencyUnavailable,
		"the verified candidate cannot provide a valid release transition inspection",
		"install a compatible release, then run yard update --check",
	)
	if parsed.check {
		return runtime.preparePublicInspectionOutcome(outcome), nil
	}
	return Prepared{}, transitionOutcomeError(outcome)
}

type qualifiedReplacementRecovery struct {
	candidate    *verifiedPublishedCandidate
	request      releasetransition.ProcessRequest
	revalidation replacementRevalidation
}

type replacementRevalidation struct {
	source          candidateVerification
	journalSnapshot releasetransition.ProtectedSnapshot
}

const v0111RecoveryConsequence = "supersede the verified v0.11.1 recovery journal with the standalone candidate plan"

func (runtime *Runtime) qualifyV0111StandaloneRecovery(
	ctx context.Context,
	parsed options,
	protected *protectedTransitionInspection,
) (qualifiedReplacementRecovery, bool) {
	if !isV0111ScopeBlocker(protected) ||
		!parsed.offline || !parsed.versionExplicit || parsed.version == "" ||
		parsed.rollback || parsed.force ||
		(parsed.tag != "" && parsed.tag != "v"+parsed.version) ||
		runtime.config.RepositoryRoot == "" {
		return qualifiedReplacementRecovery{}, false
	}
	verified, qualified := runtime.verifiedStandaloneCandidateRoot(ctx, parsed.root)
	if !qualified {
		return qualifiedReplacementRecovery{}, false
	}
	if verified.version != parsed.version {
		verified.Close()
		return qualifiedReplacementRecovery{}, false
	}
	request := releasetransition.ProcessRequest{
		SchemaVersion:       releasetransition.ProcessProtocolSchemaV1,
		Mode:                releasetransition.ProcessInspect,
		RuntimeRoot:         parsed.root,
		ConfigHome:          protected.request.ConfigHome,
		Yard:                protected.request.Yard,
		Target:              verified.candidate.release,
		Direction:           releasetransition.DirectionActivateTarget,
		ArtifactDigest:      verified.manifestDigest,
		InheritedSettingIDs: slices.Clone(protected.request.InheritedSettingIDs),
		Replacement: &releasetransition.JournalReplacement{
			Transaction:   protected.journal.Transaction,
			Fingerprint:   protected.journalSnapshot.Fingerprint,
			Reason:        releasetransition.JournalReplacementPostActivationScopeV0111,
			SourceVersion: protected.target.version,
		},
	}
	return qualifiedReplacementRecovery{
		candidate: verified,
		request:   request,
		revalidation: replacementRevalidation{
			source: protected.target, journalSnapshot: protected.journalSnapshot,
		},
	}, true
}

func (runtime *Runtime) verifiedStandaloneCandidateRoot(
	ctx context.Context,
	runtimeRoot string,
) (*verifiedPublishedCandidate, bool) {
	if runtime.config.RepositoryRoot == "" {
		return nil, false
	}
	repositoryRoot, err := filepath.EvalSymlinks(runtime.config.RepositoryRoot)
	if err != nil || !filepath.IsAbs(repositoryRoot) {
		return nil, false
	}
	releaseID := releasetransition.ReleaseID(filepath.Base(repositoryRoot))
	if repositoryRoot != filepath.Join(runtimeRoot, "releases", string(releaseID)) {
		return nil, false
	}
	candidate := publishedCandidate{release: releaseID, root: repositoryRoot}
	verified, err := runtime.verifyPublishedCandidate(ctx, candidate, runtimeRoot, nil)
	if err != nil {
		return nil, false
	}
	if verified.registryDigest == "" {
		verified.Close()
		return nil, false
	}
	version, err := semver.Parse(verified.version)
	if err != nil || version.String() != verified.version ||
		!version.GT(semver.MustParse("0.11.1")) {
		verified.Close()
		return nil, false
	}
	return verified, true
}

func (runtime *Runtime) setV0111StandaloneRetry(
	ctx context.Context,
	runtimeRoot string,
	protected *protectedTransitionInspection,
) {
	if !isV0111ScopeBlocker(protected) {
		return
	}
	retry := "install a verified standalone release newer than v0.11.1, then run it with --offline and its exact --version"
	if candidate, available := runtime.verifiedStandaloneCandidateRoot(ctx, runtimeRoot); available {
		candidate.Close()
		retry = "run the verified standalone release with --offline and its exact --version"
	}
	protected.inspection.Blockers[0].Retry = retry
	protected.inspection.Outcome.Retry = retry
}

func isV0111ScopeBlocker(protected *protectedTransitionInspection) bool {
	return protected != nil && protected.target.version == "0.11.1" &&
		protected.journal.Checkpoint == releasetransition.JournalReconciling &&
		protected.journal.SourceIngress == nil &&
		protected.journal.Goal.Direction == releasetransition.DirectionActivateTarget &&
		protected.inspection.Resume != nil &&
		*protected.inspection.Resume == protected.journal.Transaction &&
		protected.inspection.Outcome != nil &&
		protected.inspection.Outcome.Status == releasetransition.StatusOperatorActionRequired &&
		protected.inspection.Outcome.Code == releasetransition.CodePlanStale &&
		len(protected.inspection.Blockers) == 1 &&
		protected.inspection.Blockers[0].Code == releasetransition.CodePlanStale &&
		protected.inspection.Blockers[0].Resource == "transition.observation-scope"
}

func sourceIngressRequestFromEnvironment(
	environment map[string]string,
) (*releasetransition.SourceIngressRequest, error) {
	roles := []struct {
		name string
		set  func(*releasetransition.SourceIngressRequest, string)
	}{
		{"SUBYARD_SOURCE_INGRESS_V1_ROOT", func(value *releasetransition.SourceIngressRequest, path string) { value.SourceRoot = path }},
		{"SUBYARD_SOURCE_INGRESS_V1_DATA", func(value *releasetransition.SourceIngressRequest, path string) { value.DataHome = path }},
		{"SUBYARD_SOURCE_INGRESS_V1_BIN", func(value *releasetransition.SourceIngressRequest, path string) { value.BinDir = path }},
		{"SUBYARD_SOURCE_INGRESS_V1_RC", func(value *releasetransition.SourceIngressRequest, path string) { value.RC = path }},
		{"SUBYARD_SOURCE_INGRESS_V1_LOGIN_RC", func(value *releasetransition.SourceIngressRequest, path string) { value.LoginRC = path }},
	}
	request := &releasetransition.SourceIngressRequest{
		SchemaVersion: releasetransition.SourceIngressRequestSchemaV1,
		Kind:          releasetransition.SourceIngressPreGoV1,
	}
	present := 0
	for _, role := range roles {
		if path := environment[role.name]; path != "" {
			role.set(request, path)
			present++
		}
	}
	if present == 0 {
		return nil, nil
	}
	if present != len(roles) {
		return nil, errors.New("source ingress environment is incomplete")
	}
	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("source ingress environment is invalid: %w", err)
	}
	return request, nil
}

func (runtime *Runtime) prepareProtectedTransition(
	parsed options,
	protected *protectedTransitionInspection,
) (Prepared, error) {
	exactTag := "v" + protected.target.version
	if parsed.rollback || parsed.force ||
		(parsed.version != "" && parsed.version != protected.target.version) ||
		(parsed.tag != "" && parsed.tag != exactTag) {
		retry := "restore the verified journal-selected release before retrying"
		if protected.inspection.Outcome != nil && protected.inspection.Outcome.Retry != "" {
			retry = protected.inspection.Outcome.Retry
		}
		return Prepared{}, fmt.Errorf(
			"an unfinished or unsafe release transition permits only exact target %s; next: %s",
			protected.target.candidate.release, retry,
		)
	}
	return runtime.prepareInspectedCandidateTransition(
		parsed, protected.owner, protected.target,
		protected.request, protected.inspection,
		protected.activationReconciliationOwned, nil,
	)
}

func (runtime *Runtime) prepareRetainedTransition(
	ctx context.Context,
	parsed options,
	configHome string,
	yard string,
	inheritedSettingIDs []string,
) (Prepared, error) {
	if err := requirePreparedReleaseRoots(parsed, false); err != nil {
		return Prepared{}, err
	}
	links, err := runtime.inspectRuntimeLinks(parsed.root)
	observed := releaseLinksFromRuntimeSnapshot(links)
	if err != nil {
		return Prepared{}, transitionOutcomeError(publicReleaseOutcome(
			observed, "unknown", nil,
			releasetransition.CodeRollbackIncompatible,
			"the runtime links do not provide a safe rollback horizon",
			"restore valid runtime links, then run yard update --rollback",
		))
	}
	if !links.current.present {
		return Prepared{}, transitionOutcomeError(publicReleaseOutcome(
			observed, "unknown", nil, releasetransition.CodeRollbackIncompatible,
			"the active release is unavailable for rollback",
			"restore the active release, then run yard update --rollback",
		))
	}
	if !links.previous.present {
		return Prepared{}, transitionOutcomeError(publicReleaseOutcome(
			observed, "unknown", nil, releasetransition.CodeRollbackExpired,
			"the retained previous release is no longer available for rollback",
			"install a new release to establish a fresh rollback horizon",
		))
	}
	release := releasetransition.ReleaseID(strings.TrimPrefix(links.previous.target, "releases/"))
	for _, state := range []runtimeLinkState{links.current, links.previous} {
		if err := validateRuntimeEngine(parsed.root, state); err != nil {
			return Prepared{}, transitionOutcomeError(publicReleaseOutcome(
				observed, release, nil, releasetransition.CodeRollbackIncompatible,
				"the retained rollback horizon is not executable",
				"restore a compatible retained release, then run yard update --rollback",
			))
		}
	}
	candidate := publishedCandidate{
		release: release,
		root:    filepath.Join(parsed.root, links.previous.target),
	}
	verified, err := runtime.verifyPublishedCandidate(ctx, candidate, parsed.root, nil)
	if err != nil {
		return Prepared{}, transitionOutcomeError(publicReleaseOutcome(
			observed, release, nil, releasetransition.CodeRollbackIncompatible,
			"the retained release is not verified for rollback",
			"restore a compatible retained release, then run yard update --rollback",
		))
	}
	defer verified.Close()
	request := releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Mode:          releasetransition.ProcessInspect,
		RuntimeRoot:   parsed.root, ConfigHome: configHome, Yard: yard,
		Target: release, Direction: releasetransition.DirectionActivatePrevious,
		ArtifactDigest:      verified.manifestDigest,
		InheritedSettingIDs: slices.Clone(inheritedSettingIDs),
	}
	owner := verified
	var active *verifiedPublishedCandidate
	// A pre-v2 target remains the verified rollback artifact, but it has no
	// transition registry. The verified active v2 release owns that exact goal.
	if verified.registryDigest == "" {
		activeCandidate := publishedCandidate{
			release: releasetransition.ReleaseID(
				strings.TrimPrefix(links.current.target, "releases/"),
			),
			root: filepath.Join(parsed.root, links.current.target),
		}
		active, err = runtime.verifyPublishedCandidate(ctx, activeCandidate, parsed.root, nil)
		if err != nil || active.registryDigest == "" {
			if active != nil {
				active.Close()
			}
			return Prepared{}, transitionOutcomeError(publicReleaseOutcome(
				observed, release, nil, releasetransition.CodeRollbackIncompatible,
				"the active release cannot own a transition to the retained release",
				"restore a compatible retained release, then run yard update --rollback",
			))
		}
		defer active.Close()
		owner = active
	}
	prepared, err := runtime.prepareVerifiedTransition(ctx, parsed, owner, verified, request, nil)
	if err != nil {
		return Prepared{}, transitionOutcomeError(publicReleaseOutcome(
			observed, release, nil, releasetransition.CodeRollbackIncompatible,
			"the retained release cannot provide a safe rollback plan",
			"restore a compatible retained release, then run yard update --rollback",
		))
	}
	return prepared, nil
}

type candidateVerification struct {
	candidate publishedCandidate
	digest    releasetransition.Fingerprint
	version   string
}

func (runtime *Runtime) prepareVerifiedCandidateTransition(
	ctx context.Context,
	parsed options,
	verified *verifiedPublishedCandidate,
	request releasetransition.ProcessRequest,
) (Prepared, error) {
	return runtime.prepareVerifiedTransition(ctx, parsed, verified, verified, request, nil)
}

func (runtime *Runtime) prepareVerifiedReplacementTransition(
	ctx context.Context,
	parsed options,
	verified *verifiedPublishedCandidate,
	request releasetransition.ProcessRequest,
	revalidation replacementRevalidation,
) (Prepared, error) {
	return runtime.prepareVerifiedTransition(
		ctx, parsed, verified, verified, request, &revalidation,
	)
}

func (runtime *Runtime) prepareVerifiedTransition(
	ctx context.Context,
	parsed options,
	owner *verifiedPublishedCandidate,
	target *verifiedPublishedCandidate,
	request releasetransition.ProcessRequest,
	revalidation *replacementRevalidation,
) (Prepared, error) {
	response, err := runtime.invokeVerifiedCandidateTransition(ctx, owner, request, "")
	if err != nil {
		return Prepared{}, err
	}
	if response.Outcome != nil {
		if response.Inspection != nil {
			return Prepared{}, errors.New("candidate returned an invalid release inspection")
		}
		goal := releasetransition.Goal{Target: request.Target, Direction: request.Direction}
		if err := response.Outcome.ValidateInspection(goal); err != nil {
			return Prepared{}, fmt.Errorf("candidate returned an inconsistent release outcome: %w", err)
		}
		if parsed.check {
			return runtime.preparePublicInspectionOutcome(*response.Outcome), nil
		}
		return Prepared{}, publicReleaseInspectionError{
			cause: transitionOutcomeError(*response.Outcome), outcome: *response.Outcome,
		}
	}
	if response.Inspection == nil {
		return Prepared{}, errors.New("candidate returned an invalid release inspection")
	}
	return runtime.prepareInspectedCandidateTransition(
		parsed,
		candidateVerification{
			candidate: owner.candidate, digest: owner.manifestDigest, version: owner.version,
		},
		candidateVerification{
			candidate: target.candidate, digest: target.manifestDigest, version: target.version,
		},
		request, *response.Inspection,
		response.ActivationReconciliationOwned, revalidation,
	)
}

func (runtime *Runtime) prepareInspectedCandidateTransition(
	parsed options,
	owner candidateVerification,
	target candidateVerification,
	request releasetransition.ProcessRequest,
	inspection releasetransition.Inspection,
	activationReconciliationOwned bool,
	revalidation *replacementRevalidation,
) (Prepared, error) {
	goal := releasetransition.Goal{Target: request.Target, Direction: request.Direction}
	if err := inspection.ValidateOutcome(goal); err != nil {
		return Prepared{}, fmt.Errorf("candidate returned an inconsistent release inspection: %w", err)
	}
	if revalidation != nil {
		inspection.Assessment.Consequences = v0111RecoveryConsequences(
			inspection.Assessment.Consequences,
		)
		if err := inspection.ValidateOutcome(goal); err != nil {
			return Prepared{}, fmt.Errorf("augmented recovery inspection is invalid: %w", err)
		}
	}
	if parsed.check {
		return Prepared{
			Action: "update.check", Changed: false, Consequences: nil,
			ActiveLauncher: filepath.Join(parsed.root, "current", "bin", "yard"),
			run: func(context.Context) error {
				return json.NewEncoder(runtime.config.Stdout).Encode(inspection)
			},
		}, nil
	}
	if inspection.Outcome.Status == releasetransition.StatusOperatorActionRequired {
		return Prepared{}, publicReleaseInspectionError{
			cause: transitionOutcomeError(*inspection.Outcome), outcome: *inspection.Outcome,
		}
	}
	consequences := slices.Clone(inspection.Assessment.Consequences)
	if request.Direction == releasetransition.DirectionActivatePrevious {
		consequences = append([]string{"reactivate the verified retained previous runtime"}, consequences...)
	}
	for _, decision := range inspection.Decisions {
		consequence := fmt.Sprintf("%s %s setting %s", decision.Decision, decision.Scope, decision.Resource)
		if decision.Result != "" {
			consequence += " to " + decision.Result
		}
		consequences = append(consequences, consequence)
	}
	changed := inspection.Assessment.Changed
	if inspection.Resume != nil {
		changed = false
	}
	return Prepared{
		Action: map[bool]domain.ActionID{
			true: "update.rollback", false: "update.activate",
		}[request.Direction == releasetransition.DirectionActivatePrevious],
		Changed: changed, Consequences: consequences,
		RefreshConfigs: !activationReconciliationOwned,
		ActiveLauncher: filepath.Join(parsed.root, "current", "bin", "yard"),
		run: func(ctx context.Context) error {
			if err := requirePreparedReleaseRoots(parsed, false); err != nil {
				return err
			}
			verifiedTarget, err := runtime.verifyPublishedCandidate(
				ctx, target.candidate, request.RuntimeRoot, &target.digest,
			)
			if err != nil {
				return fmt.Errorf("reverify target runtime: %w", err)
			}
			defer verifiedTarget.Close()
			if verifiedTarget.version != target.version {
				return fmt.Errorf("%w: target runtime version changed after inspection", domain.ErrPlanStale)
			}
			verifiedOwner := verifiedTarget
			if owner.candidate.release != target.candidate.release {
				verifiedOwner, err = runtime.verifyPublishedCandidate(
					ctx, owner.candidate, request.RuntimeRoot, &owner.digest,
				)
				if err != nil {
					return fmt.Errorf("reverify transition owner: %w", err)
				}
				defer verifiedOwner.Close()
				if verifiedOwner.version != owner.version {
					return fmt.Errorf("%w: transition owner version changed after inspection", domain.ErrPlanStale)
				}
			}
			if revalidation != nil {
				verifiedSource, verifyErr := runtime.verifyPublishedCandidate(
					ctx, revalidation.source.candidate, request.RuntimeRoot,
					&revalidation.source.digest,
				)
				if verifyErr != nil {
					return fmt.Errorf("reverify recovery source runtime: %w", verifyErr)
				}
				defer verifiedSource.Close()
				if verifiedSource.version != revalidation.source.version {
					return fmt.Errorf("%w: recovery source runtime version changed after inspection", domain.ErrPlanStale)
				}
				store, storeErr := releasetransition.NewPOSIXV2Store(request.ConfigHome)
				if storeErr != nil {
					return storeErr
				}
				journal, readErr := store.ReadCurrentJournal()
				if readErr != nil {
					return fmt.Errorf("revalidate recovery source journal: %w", readErr)
				}
				if !sameProtectedSnapshot(journal, revalidation.journalSnapshot) {
					return fmt.Errorf("%w: recovery source journal changed after inspection", domain.ErrPlanStale)
				}
			}
			grant := releasetransition.Authorization("")
			if inspection.Resume == nil && inspection.Assessment.Changed {
				var grantErr error
				grant, grantErr = newReleaseTransitionGrant()
				if grantErr != nil {
					return grantErr
				}
			}
			request.Mode = releasetransition.ProcessConverge
			request.Execution = &releasetransition.Execution{
				Plan: inspection.Plan, Authorization: grant,
			}
			converged, convergeErr := runtime.invokeVerifiedCandidateTransition(
				ctx, verifiedOwner, request, grant,
			)
			if convergeErr != nil {
				return convergeErr
			}
			if converged.Outcome == nil || converged.Inspection != nil {
				return errors.New("candidate returned an invalid release transition outcome")
			}
			goal := releasetransition.Goal{
				Target: request.Target, Direction: request.Direction,
			}
			if err := converged.Outcome.ValidateConvergence(goal, inspection); err != nil {
				return fmt.Errorf("candidate returned an inconsistent release transition outcome: %w", err)
			}
			if converged.Outcome.Status != releasetransition.StatusReady {
				if converged.Outcome.Code == releasetransition.CodePlanStale {
					request.Mode = releasetransition.ProcessInspect
					request.Execution = nil
					rechecked, recheckErr := runtime.invokeVerifiedCandidateTransition(
						ctx, verifiedOwner, request, "",
					)
					if recheckErr != nil {
						return recheckErr
					}
					if rechecked.Inspection == nil || rechecked.Outcome != nil {
						return errors.New("candidate returned an invalid release reinspection")
					}
					if err := rechecked.Inspection.ValidateOutcome(goal); err != nil {
						return fmt.Errorf("candidate returned an inconsistent release reinspection: %w", err)
					}
					if rechecked.Inspection.Outcome.Status == releasetransition.StatusReady {
						return nil
					}
					return transitionOutcomeError(*rechecked.Inspection.Outcome)
				}
				return transitionOutcomeError(*converged.Outcome)
			}
			for _, warning := range converged.Outcome.Warnings {
				if runtime.config.Stderr != nil {
					fmt.Fprintf(runtime.config.Stderr, "warning: release transition: %s\n", warning)
				}
			}
			return nil
		},
	}, nil
}

func v0111RecoveryConsequences(candidate []string) []string {
	consequences := make([]string, 0, len(candidate)+1)
	consequences = append(consequences, v0111RecoveryConsequence)
	for _, consequence := range candidate {
		if consequence != v0111RecoveryConsequence {
			consequences = append(consequences, consequence)
		}
	}
	return consequences
}

func sameProtectedSnapshot(
	left releasetransition.ProtectedSnapshot,
	right releasetransition.ProtectedSnapshot,
) bool {
	return left.Exists == right.Exists && left.Fingerprint == right.Fingerprint &&
		bytes.Equal(left.Payload, right.Payload)
}

func (runtime *Runtime) preparePublicInspectionOutcome(outcome releasetransition.Outcome) Prepared {
	return Prepared{
		Action: "update.check",
		run: func(context.Context) error {
			return json.NewEncoder(runtime.config.Stdout).Encode(struct {
				Outcome releasetransition.Outcome `json:"outcome"`
			}{Outcome: outcome})
		},
	}
}

func installerSupportsPublishOnly(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("runtime installer is not a regular file")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return false, errors.New("runtime installer changed while opening")
	}
	payload, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil {
		return false, err
	}
	if len(payload) > 1<<20 {
		return false, errors.New("runtime installer is too large")
	}
	return bytes.Contains(payload, []byte("--publish-only")), nil
}

type publishedCandidate struct {
	release releasetransition.ReleaseID
	root    string
}

func (runtime *Runtime) publishCandidate(
	ctx context.Context,
	options options,
) (publishedCandidate, string, error) {
	osName, arch := goruntime.GOOS, goruntime.GOARCH
	if osName != "linux" || arch != "amd64" && arch != "arm64" {
		return publishedCandidate{}, "", fmt.Errorf("unsupported platform %s/%s", osName, arch)
	}
	name := fmt.Sprintf("subyard-%s-%s-%s.tar.gz", options.version, osName, arch)
	directory := filepath.Join(options.cache, options.version)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return publishedCandidate{}, "", err
	}
	paths := make([]string, 4)
	for index, suffix := range []string{"", ".sha256", ".manifest.json", ".provenance.json"} {
		paths[index] = filepath.Join(directory, name+suffix)
		if err := runtime.fetch(ctx, options, name+suffix, paths[index]); err != nil {
			return publishedCandidate{}, "", fmt.Errorf(
				"release download failed; current runtime was not changed: %w", err,
			)
		}
	}
	target, digest, err := releaseBundleIdentity(options.version, paths[0])
	if err != nil {
		return publishedCandidate{}, "", fmt.Errorf("derive downloaded release identity: %w", err)
	}
	var output bytes.Buffer
	if err := runtime.installWithOutput(ctx, &output,
		"--runtime-root", options.root, "--publish-only",
		"--bundle", paths[0], "--checksum", paths[1],
		"--manifest", paths[2], "--provenance", paths[3],
	); err != nil {
		return publishedCandidate{}, "", err
	}
	if strings.TrimSpace(output.String()) != target {
		return publishedCandidate{}, "", errors.New("installer returned an unexpected published release identity")
	}
	release := releasetransition.ReleaseID(strings.TrimPrefix(target, "releases/"))
	root := filepath.Join(options.root, target)
	return publishedCandidate{release: release, root: root}, digest, nil
}

func newReleaseTransitionGrant() (releasetransition.Authorization, error) {
	var value [32]byte
	if _, err := crand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate release transition authorization: %w", err)
	}
	return releasetransition.Authorization("grant-v1-" + hex.EncodeToString(value[:])), nil
}

func transitionOutcomeError(outcome releasetransition.Outcome) error {
	previous := "none"
	if outcome.Previous != nil {
		previous = string(*outcome.Previous)
	}
	transaction := "none"
	if outcome.Transaction != nil {
		transaction = string(*outcome.Transaction)
	}
	return fmt.Errorf(
		"release transition %s: code=%s active=%s previous=%s target=%s transaction=%s; %s; next: %s",
		outcome.Status, outcome.Code, outcome.Active, previous, outcome.Target,
		transaction, outcome.Message, outcome.Retry,
	)
}

type options struct {
	channel, version, root, cache, repository, baseURL, tag string
	offline, check, rollback, force, versionExplicit        bool
}

func (runtime *Runtime) prepareHelp() Prepared {
	return Prepared{Action: "update.help", run: func(context.Context) error {
		fmt.Fprintln(runtime.config.Stdout, "Usage: yard update [--check] [--version VERSION] [--offline] [--rollback] [--force]")
		return nil
	}}
}

type runtimeLinkState struct {
	present bool
	target  string
}

type runtimeLinkSnapshot struct {
	current  runtimeLinkState
	previous runtimeLinkState
}

func (runtime *Runtime) inspectRuntimeLinks(root string) (runtimeLinkSnapshot, error) {
	current, err := inspectRuntimeLink(root, "current")
	if err != nil {
		return runtimeLinkSnapshot{}, err
	}
	previous, err := inspectRuntimeLink(root, "previous")
	if err != nil {
		return runtimeLinkSnapshot{current: current}, err
	}
	snapshot := runtimeLinkSnapshot{current: current, previous: previous}
	return snapshot, nil
}

func releaseLinksFromRuntimeSnapshot(snapshot runtimeLinkSnapshot) releasetransition.ReleaseLinks {
	links := releasetransition.ReleaseLinks{}
	if snapshot.current.present {
		links.Active = releasetransition.ReleaseID(strings.TrimPrefix(snapshot.current.target, "releases/"))
	}
	if snapshot.previous.present {
		previous := releasetransition.ReleaseID(strings.TrimPrefix(snapshot.previous.target, "releases/"))
		links.Previous = &previous
	}
	return links
}

func validateRuntimeEngine(root string, state runtimeLinkState) error {
	engine := filepath.Join(root, state.target, "bin", "yard-engine")
	info, err := os.Lstat(engine)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("runtime %q has no executable yard-engine", state.target)
	}
	return nil
}

func inspectRuntimeLink(root, name string) (runtimeLinkState, error) {
	path := filepath.Join(root, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return runtimeLinkState{}, nil
	}
	if err != nil {
		return runtimeLinkState{}, fmt.Errorf("inspect runtime %s link: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return runtimeLinkState{}, fmt.Errorf("runtime %s must be a symbolic link", name)
	}
	target, err := os.Readlink(path)
	if err != nil {
		return runtimeLinkState{}, fmt.Errorf("read runtime %s link: %w", name, err)
	}
	clean := filepath.Clean(target)
	relative := strings.TrimPrefix(target, "releases/")
	if clean != target || filepath.IsAbs(target) || !strings.HasPrefix(target, "releases/") ||
		relative == "" || relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
		return runtimeLinkState{}, fmt.Errorf("runtime %s link has an unsafe target", name)
	}
	destinationInfo, err := os.Stat(filepath.Join(root, target))
	if err != nil || !destinationInfo.IsDir() {
		return runtimeLinkState{}, fmt.Errorf("runtime %s link target is unavailable", name)
	}
	return runtimeLinkState{present: true, target: target}, nil
}

func (runtime *Runtime) parse(arguments []string) (options, bool, error) {
	home := runtime.config.Environment["SUBYARD_HOME"]
	if home == "" {
		home = filepath.Join(runtime.config.Environment["HOME"], ".subyard")
	}
	result := options{channel: "stable", root: filepath.Join(home, "runtime"), cache: filepath.Join(home, "releases"),
		repository: first(runtime.config.Environment["YARD_RELEASE_REPOSITORY"], "Subyard/Subyard"),
		version:    runtime.config.Environment["YARD_RELEASE_VERSION"], baseURL: runtime.config.Environment["YARD_RELEASE_BASE_URL"],
		tag: runtime.config.Environment["YARD_RELEASE_TAG"]}
	if value := runtime.config.Environment["YARD_RUNTIME_ROOT"]; value != "" {
		result.root = value
	}
	if value := runtime.config.Environment["YARD_RELEASE_CACHE"]; value != "" {
		result.cache = value
	}
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "--channel", "--version", "--runtime-root":
			index++
			if index >= len(arguments) {
				return result, false, fmt.Errorf("%s needs a value", arguments[index-1])
			}
			switch arguments[index-1] {
			case "--channel":
				result.channel = arguments[index]
			case "--version":
				result.version = arguments[index]
				result.versionExplicit = true
			case "--runtime-root":
				result.root = arguments[index]
			}
		case "--offline":
			result.offline = true
		case "--check":
			result.check = true
		case "--rollback":
			result.rollback = true
		case "--force":
			result.force = true
		case "-y", "--yes":
		case "-h", "--help":
			return result, true, nil
		default:
			return result, false, fmt.Errorf("unknown option %q", arguments[index])
		}
	}
	if result.channel != "stable" {
		return result, false, fmt.Errorf("unsupported channel %q", result.channel)
	}
	if result.rollback && (result.offline || result.check || result.force || result.version != "") {
		return result, false, errors.New("--rollback cannot be combined with update options")
	}
	validatedRoot, err := validateReleaseRoot(result.root, "runtime root")
	if err != nil {
		return result, false, err
	}
	validatedCache, err := validateReleaseRoot(result.cache, "release cache")
	if err != nil {
		return result, false, err
	}
	result.root = validatedRoot
	result.cache = validatedCache
	return result, false, nil
}

func validateReleaseRoot(path, name string) (string, error) {
	clean := filepath.Clean(path)
	if path == "" || !filepath.IsAbs(path) || clean == string(filepath.Separator) {
		return "", fmt.Errorf("%s must be an absolute non-root path", name)
	}
	current := string(filepath.Separator)
	components := strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator))
	operatorUID := uint32(os.Getuid())
	privateAncestor := false
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return clean, nil
		}
		if err != nil {
			return "", fmt.Errorf("inspect %s path %s: %w", name, current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%s path has a symlink or non-directory ancestor: %s", name, current)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 && stat.Uid != operatorUID {
			return "", fmt.Errorf("%s path has unsafe ownership: %s", name, current)
		}
		final := index == len(components)-1
		stickySystemAncestor := stat.Uid == 0 && info.Mode()&os.ModeSticky != 0 && !final
		containedOperatorAncestor := stat.Uid == operatorUID && privateAncestor && !final
		if info.Mode().Perm()&0o022 != 0 && !stickySystemAncestor && !containedOperatorAncestor {
			return "", fmt.Errorf("%s path has unsafe permissions: %s", name, current)
		}
		if stat.Uid == operatorUID && info.Mode().Perm()&0o077 == 0 {
			privateAncestor = true
		}
		if final && stat.Uid != operatorUID {
			return "", fmt.Errorf("%s is not operator-owned: %s", name, current)
		}
	}
	return clean, nil
}

func requirePreparedReleaseRoots(options options, requireCache bool) error {
	for _, root := range []struct {
		path, name string
	}{
		{path: options.root, name: "runtime root"},
		{path: options.cache, name: "release cache"},
	} {
		if root.name == "release cache" && !requireCache {
			continue
		}
		validated, err := validateReleaseRoot(root.path, root.name)
		if err != nil || validated != root.path {
			if err != nil {
				return fmt.Errorf("%w: %s changed after planning: %v", domain.ErrPlanStale, root.name, err)
			}
			return fmt.Errorf("%w: %s changed after planning", domain.ErrPlanStale, root.name)
		}
	}
	return nil
}

func releaseBundleIdentity(version, bundle string) (string, string, error) {
	info, err := os.Lstat(bundle)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", errors.New("downloaded release bundle is not a regular file")
	}
	file, err := os.OpenFile(bundle, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", "", err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return "", "", errors.New("downloaded release bundle changed while opening")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", "", err
	}
	encoded := hex.EncodeToString(digest.Sum(nil))
	return filepath.Join("releases", version+"-"+encoded[:12]), encoded, nil
}

func releaseMigrationEnvironment(
	values map[string]string,
	repositoryRoot string,
	runtimeRoot string,
) []string {
	environment := make(map[string]string)
	if values == nil {
		for _, assignment := range os.Environ() {
			name, value, ok := strings.Cut(assignment, "=")
			if ok {
				environment[name] = value
			}
		}
	} else {
		for name, value := range values {
			environment[name] = value
		}
	}
	delete(environment, "YARD_ENGINE_PATH")
	delete(environment, "SUBYARD_RELEASE_TRANSITION_GRANT_FD")
	environment["SUBYARD_REPOSITORY_ROOT"] = repositoryRoot
	environment["YARD_RUNTIME_ROOT"] = runtimeRoot
	environment["SUBYARD_INTERNAL_MIGRATION_CHILD"] = "1"
	return commandEnvironment(environment)
}

func (runtime *Runtime) fetch(ctx context.Context, options options, name, destination string) error {
	if options.offline {
		info, err := os.Lstat(destination)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("cached release asset is unavailable")
		}
		return nil
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".download-*")
	if err != nil {
		return err
	}
	path := temporary.Name()
	defer os.Remove(path)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	var reader io.ReadCloser
	if strings.HasPrefix(options.baseURL, "file://") {
		reader, err = os.Open(filepath.Join(strings.TrimPrefix(options.baseURL, "file://"), name))
	} else {
		url := options.baseURL
		if url == "" {
			url = fmt.Sprintf("https://github.com/%s/releases/download/%s", options.repository, options.tag)
		}
		if !strings.HasPrefix(url, "https://") {
			temporary.Close()
			return errors.New("release base URL must use https:// or file://")
		}
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(url, "/")+"/"+name, nil)
		if requestErr != nil {
			temporary.Close()
			return requestErr
		}
		response, requestErr := runtime.config.HTTPClient.Do(request)
		if requestErr != nil {
			temporary.Close()
			return requestErr
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			temporary.Close()
			return fmt.Errorf("download returned %s", response.Status)
		}
		reader = response.Body
	}
	if err != nil {
		temporary.Close()
		return err
	}
	_, copyErr := io.Copy(temporary, reader)
	closeReaderErr, closeFileErr := reader.Close(), temporary.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeReaderErr != nil {
		return closeReaderErr
	}
	if closeFileErr != nil {
		return closeFileErr
	}
	return os.Rename(path, destination)
}

func (runtime *Runtime) latestTag(ctx context.Context, repository string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repository+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := runtime.config.HTTPClient.Do(request)
	if err != nil {
		return "", errors.New("could not resolve the stable release")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub latest release request returned %s", response.Status)
	}
	var payload struct {
		Tag string `json:"tag_name"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload) != nil || payload.Tag == "" {
		return "", errors.New("latest release has no valid tag")
	}
	return payload.Tag, nil
}

func (runtime *Runtime) installWithOutput(
	ctx context.Context,
	stdout io.Writer,
	arguments ...string,
) error {
	command := exec.CommandContext(ctx, runtime.config.Installer, arguments...)
	command.Env = commandEnvironment(runtime.config.Environment)
	command.Stdout = stdout
	command.Stderr = runtime.config.Stderr
	return command.Run()
}

func commandEnvironment(values map[string]string) []string {
	if values == nil {
		return os.Environ()
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func safeVersion(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character != '.' && character != '_' && character != '+' && character != '-' && (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') {
			return false
		}
	}
	return true
}

func first(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
