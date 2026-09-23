package releaseruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

type currentOptions struct {
	root              string
	check, json, help bool
}

type currentReport struct {
	SchemaVersion int                                  `json:"schemaVersion"`
	Current       releasetransition.ReleaseID          `json:"current"`
	Owner         releasetransition.ReleaseID          `json:"owner,omitempty"`
	Outcome       releasetransition.Outcome            `json:"outcome"`
	Domains       []currentDomainReport                `json:"domains,omitempty"`
	Journal       *currentJournalReport                `json:"journal,omitempty"`
	Decisions     []releasetransition.RedactedDecision `json:"decisions,omitempty"`
	Blockers      []releasetransition.Blocker          `json:"blockers,omitempty"`
	MetadataIssue string                               `json:"metadataIssue,omitempty"`
	Next          string                               `json:"next,omitempty"`
}

type currentDomainReport struct {
	Domain        string   `json:"domain"`
	Epoch         int      `json:"epoch"`
	RequiredEpoch int      `json:"requiredEpoch"`
	LedgerPresent bool     `json:"ledgerPresent"`
	Applied       []string `json:"applied"`
	Pending       []string `json:"pending"`
}

type currentJournalReport struct {
	Transaction releasetransition.TransactionID     `json:"transaction"`
	Checkpoint  releasetransition.JournalCheckpoint `json:"checkpoint"`
	From        releasetransition.ReleaseID         `json:"from"`
	Target      releasetransition.ReleaseID         `json:"target"`
	Steps       []currentStepReport                 `json:"steps"`
}

type currentStepReport struct {
	ID         string                           `json:"id"`
	Migration  string                           `json:"migration"`
	Checkpoint releasetransition.StepCheckpoint `json:"checkpoint"`
}

type currentSnapshot struct {
	links           runtimeLinkSnapshot
	journal, ledger releasetransition.ProtectedSnapshot
}

// PrepareCurrentTransition inspects and completes the exact installed current
// release. It never selects, downloads, or publishes a release.
func (runtime *Runtime) PrepareCurrentTransition(ctx context.Context, arguments []string, configHome, yard string, inheritedSettingIDs []string) (Prepared, error) {
	parsed, err := runtime.parseCurrent(arguments)
	if err != nil {
		return Prepared{}, err
	}
	if parsed.help {
		return Prepared{Action: "migrate.help", run: func(context.Context) error {
			_, err := fmt.Fprintln(runtime.config.Stdout, "Usage: yard migrate [--check [--json]] [--yes]\nInspect or complete migrations and runtime readiness for the installed current release.")
			return err
		}}, nil
	}
	if !filepath.IsAbs(configHome) {
		return Prepared{}, errors.New("release transition config home must be absolute")
	}
	before, err := runtime.readCurrentSnapshot(parsed.root, configHome)
	if err != nil {
		return Prepared{}, redactReleaseInspectionError(err)
	}
	current := releaseLinksFromRuntimeSnapshot(before.links).Active
	if current == "" {
		return Prepared{}, errors.New("release-transition-uninitialized: current runtime is missing; use the verified bootstrap installer")
	}
	protected, err := runtime.inspectProtectedTransition(ctx, parsed.root, configHome, yard, inheritedSettingIDs)
	if err != nil {
		var public publicReleaseInspectionError
		if errors.As(err, &public) && parsed.check {
			if err := runtime.revalidateCurrentSnapshot(parsed.root, configHome, before); err != nil {
				return Prepared{}, err
			}
			return runtime.prepareCurrentReport(parsed, currentReport{Current: current, Outcome: public.outcome, Next: public.outcome.Retry}), nil
		}
		if errors.As(err, &public) {
			return Prepared{}, transitionOutcomeError(public.outcome)
		}
		return Prepared{}, redactReleaseInspectionError(err)
	}
	if protected != nil && yard == "default" && protected.journal.Checkpoint != releasetransition.JournalComplete &&
		protected.journal.Goal.Target == current && currentScopeBlocker(protected.inspection) {
		protected, err = runtime.recoverCurrentScope(ctx, parsed.root, configHome, before, protected)
		if err != nil {
			return Prepared{}, err
		}
	}
	var owner, target candidateVerification
	var request releasetransition.ProcessRequest
	var inspection releasetransition.Inspection
	var activationOwned bool
	if protected != nil {
		owner, target = protected.owner, protected.target
		request, inspection = protected.request, protected.inspection
		activationOwned = protected.activationReconciliationOwned
	} else {
		candidate := publishedCandidate{release: current, root: filepath.Join(parsed.root, "releases", string(current))}
		verified, err := runtime.verifyPublishedCandidate(ctx, candidate, parsed.root, nil)
		if err != nil {
			return Prepared{}, fmt.Errorf("verify installed current release: %w", err)
		}
		defer verified.Close()
		if !strings.HasPrefix(string(current), verified.version+"-") {
			return Prepared{}, errors.New("current release identity does not match its verified engine version")
		}
		owner = candidateVerification{candidate: candidate, digest: verified.manifestDigest, version: verified.version, registryDigest: verified.registryDigest}
		target = owner
		request = releasetransition.ProcessRequest{
			SchemaVersion: releasetransition.ProcessProtocolSchemaV1, Mode: releasetransition.ProcessInspect,
			RuntimeRoot: parsed.root, ConfigHome: configHome, Yard: yard, Target: current,
			Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: verified.manifestDigest,
			RegistryDigest: verified.registryDigest, InheritedSettingIDs: slices.Clone(inheritedSettingIDs),
		}
		response, err := runtime.invokeVerifiedCandidateTransition(ctx, verified, request, "")
		if err != nil {
			return Prepared{}, redactReleaseInspectionError(err)
		}
		goal := releasetransition.Goal{Target: current, Direction: request.Direction}
		if response.Outcome != nil && response.Inspection == nil {
			if response.Outcome.Status != releasetransition.StatusOperatorActionRequired {
				return Prepared{}, errors.New("current release returned an outcome without a readiness inspection")
			}
			if err := releasetransition.ValidateProcessOutcome(goal, *response.Outcome); err != nil {
				return Prepared{}, redactReleaseInspectionError(err)
			}
			if parsed.check {
				if err := runtime.revalidateCurrentSnapshot(parsed.root, configHome, before); err != nil {
					return Prepared{}, err
				}
				return runtime.prepareCurrentReport(parsed, currentReport{Current: current, Owner: current, Outcome: *response.Outcome, Next: response.Outcome.Retry}), nil
			}
			return Prepared{}, transitionOutcomeError(*response.Outcome)
		}
		if response.Inspection == nil || response.Outcome != nil {
			return Prepared{}, errors.New("current release returned an invalid transition inspection")
		}
		inspection, activationOwned = *response.Inspection, response.ActivationReconciliationOwned
	}
	goal := releasetransition.Goal{Target: request.Target, Direction: request.Direction}
	if err := releasetransition.ValidateProcessInspection(goal, inspection); err != nil {
		return Prepared{}, redactReleaseInspectionError(err)
	}
	if !activationOwned {
		outcome := publicReleaseOutcome(releaseLinksFromRuntimeSnapshot(before.links), request.Target, inspection.Outcome.Transaction,
			releasetransition.CodeDependencyUnavailable,
			"the installed transition owner cannot verify and reconcile runtime readiness",
			"run yard update to install a release with runtime readiness support")
		if parsed.check {
			if err := runtime.revalidateCurrentSnapshot(parsed.root, configHome, before); err != nil {
				return Prepared{}, err
			}
			return runtime.prepareCurrentReport(parsed, currentReport{Current: current, Owner: owner.candidate.release, Outcome: outcome, Next: outcome.Retry}), nil
		}
		return Prepared{}, transitionOutcomeError(outcome)
	}
	report := currentReport{Current: current, Owner: owner.candidate.release, Outcome: *inspection.Outcome, Decisions: inspection.Decisions, Blockers: inspection.Blockers}
	if protected != nil {
		journal := protected.journal
		report.Journal = &currentJournalReport{Transaction: journal.Transaction, Checkpoint: journal.Checkpoint, From: journal.Releases.From, Target: journal.Goal.Target, Steps: []currentStepReport{}}
		for _, step := range journal.Steps {
			report.Journal.Steps = append(report.Journal.Steps, currentStepReport{ID: step.ID, Migration: step.Migration, Checkpoint: step.Checkpoint})
		}
	}
	report.Domains, err = runtime.currentDomains(ctx, parsed.root, owner, before.ledger)
	if err != nil {
		// A validated owner diagnostic remains useful even when its registry or
		// ledger cannot be represented by this caller. Never invent domain state.
		if inspection.Outcome.Status != releasetransition.StatusOperatorActionRequired {
			return Prepared{}, redactReleaseInspectionError(err)
		}
		report.MetadataIssue = "migration registry or ledger details are unavailable"
	}
	if err := runtime.revalidateCurrentSnapshot(parsed.root, configHome, before); err != nil {
		return Prepared{}, err
	}
	actual := releaseLinksFromRuntimeSnapshot(before.links)
	outcome := inspection.Outcome
	if outcome.Active != actual.Active || (outcome.Previous == nil) != (actual.Previous == nil) ||
		(outcome.Previous != nil && *outcome.Previous != *actual.Previous) {
		return Prepared{}, errors.New("current release inspection does not match the observed runtime links")
	}
	if outcome.Status == releasetransition.StatusReady {
		for _, state := range report.Domains {
			if len(state.Pending) != 0 {
				return Prepared{}, errors.New("current release readiness disagrees with its pending migration ledger")
			}
		}
	}
	report.Next = "yard migrate"
	if report.Outcome.Status == releasetransition.StatusReady {
		report.Next = ""
	}
	if report.Outcome.Status == releasetransition.StatusOperatorActionRequired {
		report.Next = report.Outcome.Retry
	}
	if target.candidate.release != current {
		report.Next = "yard update"
		report.Blockers = append(slices.Clone(report.Blockers), releasetransition.Blocker{
			Code: releasetransition.CodePreconditionBlocked, Resource: "transition.current",
			Message: "the unfinished transition targets another release; current migration cannot activate it", Retry: "yard update",
		})
		if !parsed.check {
			return Prepared{}, errors.New("the unfinished release transition targets another release; next: yard update")
		}
	}
	if parsed.check {
		return runtime.prepareCurrentReport(parsed, report), nil
	}
	applyOutcome := *inspection.Outcome
	applyOutcome.Retry = CurrentReleaseRetry(applyOutcome)
	inspection.Outcome = &applyOutcome
	inspection.Blockers = slices.Clone(inspection.Blockers)
	for index := range inspection.Blockers {
		blockerOutcome := applyOutcome
		blockerOutcome.Retry = inspection.Blockers[index].Retry
		inspection.Blockers[index].Retry = CurrentReleaseRetry(blockerOutcome)
	}
	prepared, err := runtime.prepareInspectedCandidateTransition(options{root: parsed.root, expectedLinks: &before.links}, owner, target, request, inspection, activationOwned, nil)
	if err != nil {
		return Prepared{}, err
	}
	prepared.Action = "migrate.apply"
	prepared.RefreshConfigs = false
	execute := prepared.run
	prepared.run = func(ctx context.Context) error {
		actual, err := runtime.readCurrentSnapshot(parsed.root, configHome)
		if err != nil || !sameCurrentSnapshot(before, actual) {
			return fmt.Errorf("%w: current release or protected metadata changed after inspection; run yard migrate --check again", domain.ErrPlanStale)
		}
		if err := execute(ctx); err != nil {
			return err
		}
		if err := runtime.verifyCurrentConvergence(ctx, parsed.root, configHome, current, owner, request); err != nil {
			return err
		}
		_, err = fmt.Fprintf(runtime.config.Stdout, "Installed release %s: ready\n", current)
		return err
	}
	return prepared, nil
}

func (runtime *Runtime) parseCurrent(arguments []string) (currentOptions, error) {
	parsed := currentOptions{}
	for _, argument := range arguments {
		switch argument {
		case "--check":
			parsed.check = true
		case "--json":
			parsed.json = true
		case "--yes", "-y":
		case "--help", "-h":
			parsed.help = true
		default:
			return parsed, fmt.Errorf("unknown option %q", argument)
		}
	}
	if parsed.help {
		return parsed, nil
	}
	if parsed.json && !parsed.check {
		return parsed, errors.New("--json requires --check")
	}
	home := runtime.config.Environment["SUBYARD_HOME"]
	if home == "" {
		home = filepath.Join(runtime.config.Environment["HOME"], ".subyard")
	}
	root := first(runtime.config.Environment["YARD_RUNTIME_ROOT"], filepath.Join(home, "runtime"))
	var err error
	parsed.root, err = validateReleaseRoot(root, "runtime root")
	return parsed, err
}

func (runtime *Runtime) readCurrentSnapshot(root, configHome string) (currentSnapshot, error) {
	var result currentSnapshot
	var err error
	result.links, err = runtime.inspectRuntimeLinks(root)
	if err != nil {
		return result, err
	}
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		return result, err
	}
	result.journal, err = store.ReadCurrentJournal()
	if err != nil {
		return result, err
	}
	result.ledger, err = store.ReadLedger()
	return result, err
}

func sameCurrentSnapshot(left, right currentSnapshot) bool {
	return left.links == right.links && sameProtectedSnapshot(left.journal, right.journal) && sameProtectedSnapshot(left.ledger, right.ledger)
}

func (runtime *Runtime) currentDomains(ctx context.Context, root string, owner candidateVerification, snapshot releasetransition.ProtectedSnapshot) ([]currentDomainReport, error) {
	verified, err := runtime.verifyPublishedCandidate(ctx, owner.candidate, root, &owner.digest)
	if err != nil {
		return nil, err
	}
	defer verified.Close()
	file, err := openCandidateFile(int(verified.root.Fd()), "config/release-transition.json")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, releasetransition.MaxRegistryV2Bytes+1))
	if err != nil {
		return nil, err
	}
	registry, digest, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		return nil, err
	}
	if digest != owner.registryDigest {
		return nil, errors.New("verified transition registry changed during inspection")
	}
	ledger := releasetransition.BaselineLedgerV2(registry)
	if snapshot.Exists {
		ledger, _, err = releasetransition.ParseLedgerV2(snapshot.Payload, registry)
		if err != nil {
			return nil, err
		}
	}
	pending, err := registry.PendingPath(ledger)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(registry.CurrentEpochs))
	for name := range registry.CurrentEpochs {
		names = append(names, name)
	}
	sort.Strings(names)
	domains := make([]currentDomainReport, 0, len(names))
	for _, name := range names {
		state := ledger.Domains[name]
		row := currentDomainReport{Domain: name, Epoch: state.Epoch, RequiredEpoch: registry.CurrentEpochs[name], LedgerPresent: snapshot.Exists, Applied: append([]string{}, state.Applied...), Pending: []string{}}
		for _, migration := range pending {
			if migration.Domain == name {
				row.Pending = append(row.Pending, migration.ID)
			}
		}
		domains = append(domains, row)
	}
	return domains, nil
}

func (runtime *Runtime) prepareCurrentReport(parsed currentOptions, report currentReport) Prepared {
	report.SchemaVersion = 1
	retry := CurrentReleaseRetry(report.Outcome)
	if report.Next == report.Outcome.Retry {
		report.Next = retry
	}
	report.Outcome.Retry = retry
	report.Blockers = slices.Clone(report.Blockers)
	for index := range report.Blockers {
		blockerOutcome := report.Outcome
		blockerOutcome.Retry = report.Blockers[index].Retry
		report.Blockers[index].Retry = CurrentReleaseRetry(blockerOutcome)
	}
	return Prepared{Action: "migrate.check", TargetRelease: string(report.Current), run: func(context.Context) error {
		if parsed.json {
			return json.NewEncoder(runtime.config.Stdout).Encode(report)
		}
		var output strings.Builder
		fmt.Fprintf(&output, "Installed release: %s\nStatus: %s\n", report.Current, report.Outcome.Status)
		fmt.Fprintf(&output, "%s\n", report.Outcome.Message)
		if report.Owner != "" && report.Owner != report.Current {
			fmt.Fprintf(&output, "Transition owner: %s\n", report.Owner)
		}
		if report.Journal != nil && report.Journal.Target != report.Current {
			fmt.Fprintf(&output, "Pending transition target: %s\nMigration requirements: verified transition owner %s\n", report.Journal.Target, report.Owner)
		}
		for _, domain := range report.Domains {
			ledger := "recorded"
			if !domain.LedgerPresent {
				ledger = "baseline; ledger missing"
			}
			fmt.Fprintf(&output, "%s: epoch %d -> %d (%s)\n", domain.Domain, domain.Epoch, domain.RequiredEpoch, ledger)
			for _, id := range domain.Applied {
				fmt.Fprintf(&output, "  applied: %s\n", id)
			}
			for _, id := range domain.Pending {
				fmt.Fprintf(&output, "  pending: %s\n", id)
			}
		}
		if report.Journal != nil {
			fmt.Fprintf(&output, "Transaction: %s (%s), %s -> %s\n", report.Journal.Transaction, report.Journal.Checkpoint, report.Journal.From, report.Journal.Target)
			for _, step := range report.Journal.Steps {
				fmt.Fprintf(&output, "  step %s: %s\n", step.ID, step.Checkpoint)
			}
		}
		for _, decision := range report.Decisions {
			fmt.Fprintf(&output, "Action: %s\n", releaseDecisionConsequence(decision))
		}
		for _, blocker := range report.Blockers {
			fmt.Fprintf(&output, "Blocked: %s; next: %s\n", blocker.Message, blocker.Retry)
		}
		if report.MetadataIssue != "" {
			fmt.Fprintf(&output, "Details: %s\n", report.MetadataIssue)
		}
		if report.Next != "" {
			fmt.Fprintf(&output, "Next: %s\n", report.Next)
		}
		_, err := io.WriteString(runtime.config.Stdout, output.String())
		return err
	}}
}

// Comparing bounded snapshots before and after owner inspection avoids mixing
// ledger progress, transaction checkpoints, and runtime links from different states.
func (runtime *Runtime) revalidateCurrentSnapshot(root, configHome string, expected currentSnapshot) error {
	actual, err := runtime.readCurrentSnapshot(root, configHome)
	if err != nil || !sameCurrentSnapshot(expected, actual) {
		return fmt.Errorf("%w: current release or protected metadata changed; run yard migrate --check again", domain.ErrPlanStale)
	}
	return nil
}

// CurrentReleaseRetry routes ordinary same-release recovery to migrate while
// preserving cross-release activation and specific operator repair instructions.
func CurrentReleaseRetry(outcome releasetransition.Outcome) string {
	if outcome.Active == "" || outcome.Active != outcome.Target {
		return outcome.Retry
	}
	switch outcome.Retry {
	case "run yard update":
		return "run yard migrate"
	case "run yard update --check":
		return "run yard migrate --check"
	case "yard update":
		return "yard migrate"
	case "yard update --check":
		return "yard migrate --check"
	default:
		return outcome.Retry
	}
}

func currentScopeBlocker(inspection releasetransition.Inspection) bool {
	return inspection.Outcome != nil && inspection.Outcome.Status == releasetransition.StatusOperatorActionRequired &&
		len(inspection.Blockers) == 1 && inspection.Blockers[0].Code == releasetransition.CodePlanStale &&
		inspection.Blockers[0].Resource == "transition.observation-scope"
}

// An unfinished journal binds an opaque observation scope, not its yard name.
// Search only registered local contexts, and accept only one exact protected
// resume. Every candidate is read-only; none can authorize a new transaction.
func (runtime *Runtime) recoverCurrentScope(ctx context.Context, root, configHome string, before currentSnapshot, original *protectedTransitionInspection) (*protectedTransitionInspection, error) {
	const maximumContexts = 128
	failure := func(message string) (*protectedTransitionInspection, error) {
		blocked := *original
		blocked.inspection.Blockers = slices.Clone(original.inspection.Blockers)
		retry := "restore the original yard registration and configuration, then run yard migrate --check"
		blocked.inspection.Blockers[0].Message = message
		blocked.inspection.Blockers[0].Retry = retry
		outcome := *original.inspection.Outcome
		outcome.Message, outcome.Retry = message, retry
		blocked.inspection.Outcome = &outcome
		return &blocked, nil
	}
	verified, err := runtime.verifyPublishedCandidate(ctx, original.owner.candidate, root, &original.owner.digest)
	if err != nil {
		return nil, redactReleaseInspectionError(err)
	}
	defer verified.Close()
	ownerRoot := fmt.Sprintf("/proc/self/fd/%d", verified.root.Fd())
	names, err := config.YardNames(filepath.Join(ownerRoot, "config"), configHome)
	if err != nil {
		return failure("the original transition yard cannot be identified from registered contexts")
	}
	if len(names) > maximumContexts {
		return failure("too many registered contexts to identify the original transition yard safely")
	}
	operatorHome := first(runtime.config.Environment["SUBYARD_OPERATOR_HOME"], runtime.config.Environment["HOME"])
	environment := map[string]string{"HOME": operatorHome, "SUBYARD_CONFIG_HOME": configHome}
	if home := runtime.config.Environment["SUBYARD_HOME"]; home != "" {
		environment["SUBYARD_HOME"] = home
	}
	var selected *protectedTransitionInspection
	for _, name := range names {
		if name == "default" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		loaded, err := config.Load(config.LoadOptions{RepositoryRoot: ownerRoot, OperatorHome: operatorHome, YardName: name, Environment: environment})
		if err != nil || loaded.Context.AccessKind != domain.AccessLocal {
			continue
		}
		candidate, err := runtime.inspectProtectedTransition(ctx, root, configHome, name, original.request.InheritedSettingIDs)
		if err := runtime.revalidateCurrentSnapshot(root, configHome, before); err != nil {
			return nil, err
		}
		if err != nil || candidate == nil {
			continue
		}
		inspection := candidate.inspection
		outcome := inspection.Outcome
		actual := releaseLinksFromRuntimeSnapshot(before.links)
		if !sameProtectedSnapshot(candidate.journalSnapshot, original.journalSnapshot) || candidate.owner != original.owner || candidate.target != original.target ||
			!candidate.activationReconciliationOwned || len(inspection.Blockers) != 0 || outcome == nil || outcome.Status != releasetransition.StatusRecovering ||
			outcome.Transaction == nil || *outcome.Transaction != original.journal.Transaction || inspection.Resume == nil || *inspection.Resume != original.journal.Transaction ||
			inspection.Plan != original.journal.ResumePlan || outcome.Active != actual.Active || (outcome.Previous == nil) != (actual.Previous == nil) ||
			(outcome.Previous != nil && *outcome.Previous != *actual.Previous) {
			continue
		}
		if selected != nil {
			return failure("multiple local yard contexts match the protected transition; restore a unique original context before resuming")
		}
		selected = candidate
	}
	if selected == nil {
		return failure("no registered local yard matches the protected transition observation scope")
	}
	return selected, nil
}

// Completing historical activation may reveal a fresh current-release scope.
// Inspect that state without extending the previous authorization to new work.
func (runtime *Runtime) verifyCurrentConvergence(ctx context.Context, root, configHome string, current releasetransition.ReleaseID, owner candidateVerification, request releasetransition.ProcessRequest) error {
	inspection, activationOwned, err := runtime.inspectCompletedTransition(ctx, root, configHome, current, owner, request)
	if err != nil {
		return err
	}
	if !activationOwned {
		return errors.New("the installed transition owner cannot verify final runtime readiness; run yard update")
	}
	outcome := *inspection.Outcome
	if outcome.Status != releasetransition.StatusReady {
		outcome.Retry = CurrentReleaseRetry(outcome)
		return transitionOutcomeError(outcome)
	}
	return nil
}

func (runtime *Runtime) inspectCompletedTransition(ctx context.Context, root, configHome string, current releasetransition.ReleaseID, owner candidateVerification, request releasetransition.ProcessRequest) (releasetransition.Inspection, bool, error) {
	before, err := runtime.readCurrentSnapshot(root, configHome)
	if err != nil {
		return releasetransition.Inspection{}, false, redactReleaseInspectionError(err)
	}
	actual := releaseLinksFromRuntimeSnapshot(before.links)
	if actual.Active != current {
		return releasetransition.Inspection{}, false, fmt.Errorf("%w: current release changed during convergence", domain.ErrPlanStale)
	}
	protected, err := runtime.inspectProtectedTransition(ctx, root, configHome, request.Yard, request.InheritedSettingIDs)
	if err != nil {
		var public publicReleaseInspectionError
		if errors.As(err, &public) {
			return releasetransition.Inspection{}, false, transitionOutcomeError(public.outcome)
		}
		return releasetransition.Inspection{}, false, redactReleaseInspectionError(err)
	}
	var inspection releasetransition.Inspection
	var activationOwned bool
	if protected != nil {
		if protected.journal.Goal.Target != current {
			return releasetransition.Inspection{}, false, fmt.Errorf("%w: protected target changed during convergence", domain.ErrPlanStale)
		}
		inspection, activationOwned = protected.inspection, protected.activationReconciliationOwned
	} else {
		verified, err := runtime.verifyPublishedCandidate(ctx, owner.candidate, root, &owner.digest)
		if err != nil {
			return releasetransition.Inspection{}, false, redactReleaseInspectionError(err)
		}
		defer verified.Close()
		request.Mode, request.Execution = releasetransition.ProcessInspect, nil
		response, err := runtime.invokeVerifiedCandidateTransition(ctx, verified, request, "")
		if err != nil {
			return releasetransition.Inspection{}, false, redactReleaseInspectionError(err)
		}
		if response.Inspection == nil || response.Outcome != nil {
			return releasetransition.Inspection{}, false, errors.New("current release returned no final readiness inspection")
		}
		if err := releasetransition.ValidateProcessInspection(releasetransition.Goal{Target: current, Direction: request.Direction}, *response.Inspection); err != nil {
			return releasetransition.Inspection{}, false, redactReleaseInspectionError(err)
		}
		inspection, activationOwned = *response.Inspection, response.ActivationReconciliationOwned
	}
	if err := runtime.revalidateCurrentSnapshot(root, configHome, before); err != nil {
		return releasetransition.Inspection{}, false, err
	}
	outcome := *inspection.Outcome
	if outcome.Active != actual.Active || (outcome.Previous == nil) != (actual.Previous == nil) ||
		(outcome.Previous != nil && *outcome.Previous != *actual.Previous) {
		return releasetransition.Inspection{}, false, errors.New("final readiness inspection does not match actual runtime links")
	}
	return inspection, activationOwned, nil
}
