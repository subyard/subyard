package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

// The frozen ordinary V1 inspection must first establish the exact protected
// predecessor. Fresh recovery is a separate negotiated request, never a changed
// interpretation of its resume token or old authorization.
func (runtime *Runtime) inspectActivationRecovery(
	ctx context.Context,
	verifiedOwner *verifiedPublishedCandidate,
	protected *protectedTransitionInspection,
) (*protectedTransitionInspection, error) {
	if protected == nil || !currentScopeBlocker(protected.inspection) {
		return protected, nil
	}
	journal := protected.journal
	if journal.Checkpoint != releasetransition.JournalReconciling ||
		journal.Goal.Direction != releasetransition.DirectionActivateTarget ||
		journal.SourceIngress != nil || len(journal.Steps) != 0 || protected.owner != protected.target ||
		!protected.activationReconciliationOwned || releasetransition.IsRecoveryTransaction(journal.Transaction) {
		return protected, nil
	}
	observed, err := runtime.inspectRuntimeLinks(protected.request.RuntimeRoot)
	if err != nil {
		return nil, err
	}
	actual := releaseLinksFromRuntimeSnapshot(observed)
	expectedPrevious := journal.Releases.Previous
	if journal.Releases.From != journal.Releases.Target {
		from := journal.Releases.From
		expectedPrevious = &from
	}
	if actual.Active != journal.Goal.Target || !sameRecoveryReleaseIDs(actual.Previous, expectedPrevious) {
		return protected, nil
	}
	probe := releasetransition.RecoveryProcessRequest{SchemaVersion: releasetransition.ProcessRecoverySchemaV2,
		Mode: releasetransition.RecoveryProcessLifecycleCapabilities}
	capabilities, probeErr := runtime.invokeVerifiedRecoveryTransition(ctx, verifiedOwner, protected.request.RuntimeRoot, probe, "")
	if probeErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		probe.Mode = releasetransition.RecoveryProcessCapabilities
		capabilities, probeErr = runtime.invokeVerifiedRecoveryTransition(ctx, verifiedOwner, protected.request.RuntimeRoot, probe, "")
	}
	if probeErr != nil || capabilities.Capabilities == nil || capabilities.Capabilities.Validate() != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		blocked := *protected
		blocked.inspection.Blockers = slices.Clone(protected.inspection.Blockers)
		message := "the sealed transition owner does not support fresh activation recovery; restore exactly known original activation inputs to resume"
		retry := "restore exactly known original yard registration and configuration, then run yard migrate --check"
		blocked.inspection.Blockers[0].Message, blocked.inspection.Blockers[0].Retry = message, retry
		outcome := *protected.inspection.Outcome
		outcome.Message, outcome.Retry = message, retry
		blocked.inspection.Outcome = &outcome
		return &blocked, nil
	}
	request := releasetransition.RecoveryProcessRequest{
		SchemaVersion: releasetransition.ProcessRecoverySchemaV2, Mode: releasetransition.RecoveryProcessInspect,
		RuntimeRoot: protected.request.RuntimeRoot, ConfigHome: protected.request.ConfigHome,
		Yard: protected.request.Yard, Target: journal.Goal.Target, Direction: journal.Goal.Direction,
		ArtifactDigest: journal.ArtifactDigest, RegistryDigest: journal.RegistryDigest,
		InheritedSettingIDs: slices.Clone(protected.request.InheritedSettingIDs),
		Recovery:            &releasetransition.ActivationOnlyRecoveryRequest{Transaction: journal.Transaction, Fingerprint: protected.journalSnapshot.Fingerprint},
	}
	if capabilities.Capabilities.Contract == releasetransition.ActivationOnlyRecoveryContractV2 {
		request.Contract = capabilities.Capabilities.Contract
	}
	response, err := runtime.invokeVerifiedRecoveryTransition(ctx, verifiedOwner, request.RuntimeRoot, request, "")
	if err != nil {
		return nil, err
	}
	if response.Inspection == nil || response.Outcome != nil || response.Capabilities != nil || !response.ActivationReconciliationOwned {
		return nil, errors.New("recovery owner returned an invalid fresh inspection")
	}
	inspection := *response.Inspection
	if err := releasetransition.ValidateProcessInspection(journal.Goal, inspection); err != nil {
		return nil, fmt.Errorf("recovery owner returned an inconsistent fresh inspection: %w", err)
	}
	outcome := inspection.Outcome
	if inspection.Resume != nil || inspection.Plan == journal.AuthorizationPlan || inspection.Plan == journal.ResumePlan ||
		outcome == nil || outcome.Transaction == nil || *outcome.Transaction != journal.Transaction ||
		outcome.Active != actual.Active || !sameRecoveryReleaseIDs(outcome.Previous, actual.Previous) ||
		(outcome.Status != releasetransition.StatusRecovering && outcome.Status != releasetransition.StatusOperatorActionRequired) ||
		outcome.ReachedGoal || !inspection.Assessment.Changed {
		return nil, errors.New("recovery owner did not bind a new assessment to the exact predecessor")
	}
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		return nil, err
	}
	current, err := store.ReadCurrentJournal()
	if err != nil || !sameProtectedSnapshot(current, protected.journalSnapshot) {
		return nil, errors.New("recovery predecessor changed during read-only inspection")
	}
	links, err := runtime.inspectRuntimeLinks(request.RuntimeRoot)
	if err != nil || links != observed {
		return nil, errors.New("recovery runtime links changed during read-only inspection")
	}
	recovered := *protected
	recovered.inspection, recovered.recoveryRequest = inspection, &request
	return &recovered, nil
}

func sameRecoveryReleaseIDs(left, right *releasetransition.ReleaseID) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

// The same sealed engine/assets run both protocols. No unverified executable,
// semver inference or broader delegate selection can satisfy this gate.
func (runtime *Runtime) invokeVerifiedRecoveryTransition(
	ctx context.Context,
	owner *verifiedPublishedCandidate,
	runtimeRoot string,
	request releasetransition.RecoveryProcessRequest,
	grant releasetransition.Authorization,
) (releasetransition.RecoveryProcessResponse, error) {
	if owner == nil || owner.root == nil || owner.engine == nil || owner.registryDigest == "" {
		return releasetransition.RecoveryProcessResponse{}, errors.New("verified recovery owner is unavailable")
	}
	capabilityProbe := request.Mode == releasetransition.RecoveryProcessCapabilities || request.Mode == releasetransition.RecoveryProcessLifecycleCapabilities
	if !capabilityProbe &&
		(request.RegistryDigest != owner.registryDigest || request.ArtifactDigest != owner.manifestDigest || request.Target != owner.candidate.release) {
		return releasetransition.RecoveryProcessResponse{}, errors.New("recovery request does not match its sealed owner")
	}
	if err := runtime.pinCandidateRoot(owner); err != nil {
		return releasetransition.RecoveryProcessResponse{}, err
	}
	payload, err := releasetransition.MarshalRecoveryProcessRequest(request)
	if err != nil {
		return releasetransition.RecoveryProcessResponse{}, err
	}
	var stdout boundedResponseBuffer
	processRuntime := *runtime
	if capabilityProbe {
		// An older owner may reject the optional probe before V1 fallback.
		processRuntime.config.Stderr = io.Discard
	}
	if err := processRuntime.runVerifiedRuntimeEngine(ctx, owner, owner, runtimeRoot, []string{"_release-transition"},
		bytes.NewReader(append(payload, '\n')), &stdout, grant); err != nil {
		return releasetransition.RecoveryProcessResponse{}, fmt.Errorf("verified recovery process failed: %w", err)
	}
	response, err := releasetransition.ParseRecoveryProcessResponse(stdout.Bytes())
	if err != nil {
		return releasetransition.RecoveryProcessResponse{}, err
	}
	if capabilityProbe {
		if response.Capabilities == nil {
			return releasetransition.RecoveryProcessResponse{}, errors.New("recovery capability probe returned transition results")
		}
		expected := releasetransition.ActivationOnlyRecoveryCapabilities()
		if request.Mode == releasetransition.RecoveryProcessLifecycleCapabilities {
			expected = releasetransition.ActivationRecoveryLifecycleCapabilities()
		}
		if *response.Capabilities != expected {
			return releasetransition.RecoveryProcessResponse{}, errors.New("recovery capability probe returned another contract")
		}
	} else if response.Capabilities != nil ||
		request.Mode == releasetransition.RecoveryProcessInspect && response.Inspection == nil ||
		request.Mode == releasetransition.RecoveryProcessConverge && response.Outcome == nil {
		return releasetransition.RecoveryProcessResponse{}, errors.New("recovery process returned results for another mode")
	}
	return response, nil
}

func activationRecoveryOperationBinding(binding string, request *releasetransition.RecoveryProcessRequest) string {
	if request == nil {
		return binding
	}
	payload, err := json.Marshal(struct {
		Binding string
		Request *releasetransition.RecoveryProcessRequest
	}{binding, request})
	if err != nil {
		panic("recovery operation binding must be encodable")
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func (runtime *Runtime) invokePreparedTransitionExecution(
	ctx context.Context,
	owner *verifiedPublishedCandidate,
	delegate *candidateVerification,
	request releasetransition.ProcessRequest,
	grant releasetransition.Authorization,
	recoveryRequest *releasetransition.RecoveryProcessRequest,
) (releasetransition.ProcessResponse, error) {
	if recoveryRequest == nil {
		return runtime.invokeTransitionExecution(ctx, owner, delegate, request, grant)
	}
	if delegate != nil {
		return releasetransition.ProcessResponse{}, errors.New("fresh activation recovery cannot delegate to another owner")
	}
	recovery := *recoveryRequest
	recovery.Mode = releasetransition.RecoveryProcessMode(request.Mode)
	recovery.Execution = request.Execution
	response, err := runtime.invokeVerifiedRecoveryTransition(ctx, owner, request.RuntimeRoot, recovery, grant)
	if err != nil {
		return releasetransition.ProcessResponse{}, err
	}
	if recovery.Mode == releasetransition.RecoveryProcessConverge && response.Outcome != nil {
		if err := runtime.validateFreshRecoveryConvergence(recovery, *response.Outcome); err != nil {
			return releasetransition.ProcessResponse{}, err
		}
	}
	return releasetransition.ProcessResponse{SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		ActivationReconciliationOwned: response.ActivationReconciliationOwned,
		Inspection:                    response.Inspection, Outcome: response.Outcome}, nil
}

func (runtime *Runtime) validateFreshRecoveryConvergence(request releasetransition.RecoveryProcessRequest, outcome releasetransition.Outcome) error {
	if outcome.Transaction == nil || *outcome.Transaction == request.Recovery.Transaction {
		if outcome.Status == releasetransition.StatusReady {
			return errors.New("fresh recovery returned ready without a successor transaction")
		}
		return nil
	}
	if !releasetransition.IsRecoveryTransaction(*outcome.Transaction) {
		return errors.New("fresh recovery returned an unrecognized successor identity")
	}
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		return err
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return errors.New("fresh recovery successor journal is unavailable")
	}
	current, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || current.Transaction != *outcome.Transaction {
		return errors.New("fresh recovery outcome disagrees with the current successor")
	}
	receipt, err := store.ValidatedCurrentRecovery(current)
	contract := request.Contract
	if contract == "" {
		contract = releasetransition.ActivationOnlyRecoveryContractV1
	}
	if err != nil || receipt.Contract != contract || receipt.Replacement != *request.Recovery || receipt.Successor.AuthorizationPlan != request.Execution.Plan ||
		receipt.Owner.Release != request.Target || receipt.Owner.Artifact != request.ArtifactDigest || receipt.Owner.Registry != request.RegistryDigest {
		return errors.New("fresh recovery successor does not match the inspected predecessor, owner and new plan")
	}
	if outcome.Status == releasetransition.StatusReady && current.Checkpoint != releasetransition.JournalComplete {
		return errors.New("fresh recovery returned ready before durable successor completion")
	}
	return nil
}
