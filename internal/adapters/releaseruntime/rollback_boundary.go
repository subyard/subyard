package releaseruntime

import (
	"encoding/json"
	"fmt"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/blang/semver/v4"
)

// Read only manifest-bound bytes, rechecked through the pinned release root.
// Declarations never authorize cleanup or bypass any other compatibility check.
func declaredRollbackBoundary(active, target *verifiedPublishedCandidate) (string, string, []releasetransition.RollbackBoundary, error) {
	if active.candidate.release == target.candidate.release {
		return "", "", nil, nil
	}
	var declaration releasetransition.RollbackDeclaration
	_, err := declaredPredecessor(active, func(payload []byte) (releasetransition.Fingerprint, error) {
		// Older declarations carry no boundary. An identical registry does not
		// depend on their predecessor digest; preserve that existing admission rule.
		var shape struct {
			Boundaries []json.RawMessage `json:"boundaries"`
		}
		if err := json.Unmarshal(payload, &shape); err != nil {
			return "", err
		}
		if len(shape.Boundaries) == 0 {
			return "", nil
		}
		var err error
		declaration, err = releasetransition.ParseRollbackDeclaration(payload)
		return declaration.PredecessorRegistryDigest, err
	})
	retry := "keep the current release active and inspect its config/release-rollback.json for manual recovery"
	if err != nil {
		return "the active release's rollback boundaries cannot be verified", retry, nil, err
	}
	if len(declaration.Boundaries) == 0 {
		return "", "", nil, nil
	}
	activeVersion, activeErr := releasetransition.ReleaseIDVersion(active.candidate.release)
	targetVersion, targetErr := releasetransition.ReleaseIDVersion(target.candidate.release)
	if err := domain.CollectRefusals(activeErr, targetErr); err != nil {
		// Unversioned legacy identities cannot turn accepted agent limitations into
		// refusals. Show all cleanup notices when there is no blocking declaration.
		var cleanup []releasetransition.RollbackBoundary
		for _, boundary := range declaration.Boundaries {
			if boundary.Component != "agents" {
				return "release versions cannot be compared with the declared rollback boundaries", retry, nil, err
			}
			cleanup = append(cleanup, boundary)
		}
		return "", "", cleanup, nil
	}
	if !targetVersion.LT(activeVersion) {
		return "", "", nil, nil
	}
	var findings []error
	var cleanup []releasetransition.RollbackBoundary
	for _, boundary := range declaration.Boundaries {
		through := semver.MustParse(boundary.ThroughVersion) // strict declaration parser checked it
		if targetVersion.GT(through) {
			continue
		}
		if boundary.Component == "agents" {
			cleanup = append(cleanup, boundary)
			continue
		}
		detail := fmt.Sprintf("declared rollback boundary for %s through %s: %s; next step: %s", boundary.Component, boundary.ThroughVersion, boundary.Reason, boundary.Recovery)
		findings = append(findings, fmt.Errorf("%s", detail))
	}
	refusal := domain.CollectRefusals(findings...)
	if refusal == nil {
		return "", "", cleanup, nil
	}
	return refusal.Error(), retry, cleanup, refusal
}

// AgentCleanupConsequences projects accepted limitations into the existing plan
// consequences. It adds no cleanup action, authorization, or protocol family.
func (prepared Prepared) AgentCleanupConsequences() []string {
	var notes []string
	for _, cleanup := range prepared.AgentCleanup {
		notes = append(notes, "Agent integration rollback limitation (manual cleanup; rollback proceeds): "+cleanup.Reason)
		for _, path := range cleanup.Files {
			notes = append(notes, "Surviving yard file: "+path)
		}
		notes = append(notes, "Manual cleanup: "+cleanup.Recovery)
	}
	return notes
}
