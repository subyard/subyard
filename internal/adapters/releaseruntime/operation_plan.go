package releaseruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// Project the existing protected transition, retaining its own plan token,
// authorization and journal lifetime. No outer session owns durable recovery.
func releaseOperationSteps(request releasetransition.ProcessRequest, inspection releasetransition.Inspection) []domain.OperationStep {
	steps := []domain.OperationStep{}
	for index, decision := range inspection.Decisions {
		state := domain.StepApply
		if decision.Decision == releasetransition.DecisionPreserve {
			state = domain.StepSkip
		}
		steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("release.resource.%d", index), Target: strings.TrimSpace(decision.Resource + " " + decision.Scope),
			Observed: "native transition decision " + string(decision.Decision), Desired: strings.TrimSpace(string(decision.Decision) + " " + decision.Result), Decision: state,
			Preconditions: []string{"sealed candidate and native transition token match captured protected resource state"},
			Verify:        "native transition verifier proves the selected resource decision", Consequence: releaseDecisionConsequence(decision)})
	}
	observed := "active release " + string(inspection.Outcome.Active)
	decision := domain.StepApply
	if inspection.Outcome.ReachedGoal && !inspection.Assessment.Changed {
		decision = domain.StepSkip
	}
	if inspection.Resume != nil {
		observed = "unknown"
		decision = domain.StepConditional
	}
	step := domain.OperationStep{ID: "release.activate", Target: request.RuntimeRoot + "/current", Observed: observed, Desired: "verified active release " + string(request.Target), Decision: decision,
		Preconditions: []string{"native release roots, resource plan token and captured journal state are unchanged", "any recovery uses the existing durable authorization"},
		Verify:        "read actual runtime links and obtain native ready transition inspection", Consequence: "activate and verify release " + string(request.Target)}
	if len(steps) > 0 {
		step.DependsOn = []string{steps[len(steps)-1].ID}
	}
	return append(steps, step)
}

func releaseOperationBinding(request releasetransition.ProcessRequest, inspection releasetransition.Inspection, owner, target candidateVerification, links any, recovery *replacementRevalidation) string {
	var source any
	if recovery != nil {
		source = struct {
			Root             string
			Digest, Registry any
			Journal          releasetransition.ProtectedSnapshot
		}{recovery.source.candidate.root, recovery.source.digest, recovery.source.registryDigest, recovery.journalSnapshot}
	}
	payload, err := json.Marshal(struct {
		Request                       releasetransition.ProcessRequest
		Inspection                    releasetransition.Inspection
		OwnerRoot, TargetRoot         string
		OwnerDigest, TargetDigest     any
		OwnerRegistry, TargetRegistry any
		Links, Recovery               any
	}{
		request, inspection, owner.candidate.root, target.candidate.root, owner.digest, target.digest, owner.registryDigest, target.registryDigest, links, source})
	if err != nil {
		panic("release operation binding must be encodable")
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
