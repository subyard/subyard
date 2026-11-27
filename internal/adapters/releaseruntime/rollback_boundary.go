package releaseruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/blang/semver/v4"
)

// Read only manifest-bound bytes, rechecked through the pinned release root.
// Declarations never authorize cleanup or bypass any other compatibility check.
func declaredRollbackChanges(active, target *verifiedPublishedCandidate) ([]releasetransition.RollbackBoundary, error) {
	if active.candidate.release == target.candidate.release {
		return nil, nil
	}
	var declaration releasetransition.RollbackDeclaration
	_, err := declaredPredecessor(active, func(payload []byte) (releasetransition.Fingerprint, error) {
		var shape struct {
			Boundaries []json.RawMessage `json:"boundaries"`
		}
		if err := json.Unmarshal(payload, &shape); err != nil {
			return "", err
		}
		// Identical registries do not depend on older declarations' predecessor digests.
		if len(shape.Boundaries) == 0 {
			return "", nil
		}
		var err error
		declaration, err = releasetransition.ParseRollbackDeclaration(payload)
		return declaration.PredecessorRegistryDigest, err
	})
	if err != nil {
		return nil, err
	}
	if len(declaration.Boundaries) == 0 {
		return nil, nil
	}
	activeVersion, activeErr := candidateVersion(active)
	targetVersion, targetErr := candidateVersion(target)
	if err := domain.CollectRefusals(activeErr, targetErr); err != nil {
		for _, boundary := range declaration.Boundaries {
			if boundary.Component != "agents" {
				return nil, err
			}
		}
		return declaration.Boundaries, nil // Legacy identities cannot turn notices into refusals.
	}
	if !targetVersion.LT(activeVersion) {
		return nil, nil
	}
	var applicable []releasetransition.RollbackBoundary
	for _, boundary := range declaration.Boundaries {
		if !targetVersion.GT(semver.MustParse(boundary.ThroughVersion)) {
			applicable = append(applicable, boundary)
		}
	}
	return applicable, nil
}

func declaredRollbackBoundary(active, target *verifiedPublishedCandidate) (string, string, []releasetransition.RollbackBoundary, error) {
	boundaries, err := declaredRollbackChanges(active, target)
	retry := "keep the current release active and inspect its config/release-rollback.json for manual recovery"
	if err != nil {
		return "the active release's rollback boundaries cannot be verified", retry, nil, err
	}
	var findings []error
	var cleanup []releasetransition.RollbackBoundary
	for _, boundary := range boundaries {
		if boundary.Component == "agents" {
			cleanup = append(cleanup, boundary)
			continue
		}
		findings = append(findings, fmt.Errorf("declared rollback boundary for %s through %s: %s; next step: %s", boundary.Component, boundary.ThroughVersion, boundary.Reason, boundary.Recovery))
	}
	refusal := domain.CollectRefusals(findings...)
	if refusal == nil {
		return "", "", cleanup, nil
	}
	return refusal.Error(), retry, cleanup, refusal
}

// Protected recovery reads the journal's original source independently of the
// transition engine owner, even after the current link has switched to the older target. It does not reassess authorization.
func (runtime *Runtime) protectedAgentCleanup(ctx context.Context, root string, source releasetransition.ReleaseID, target candidateVerification) ([]releasetransition.RollbackBoundary, error) {
	active, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: source, root: filepath.Join(root, "releases", string(source))}, root, nil)
	if err != nil {
		return nil, err
	}
	defer active.Close()
	retained, err := runtime.verifyPublishedCandidateFiles(target.candidate, root, &target.digest, false)
	if err != nil {
		return nil, err
	}
	defer retained.Close()
	retained.version = target.version
	boundaries, err := declaredRollbackChanges(active, retained)
	if err != nil {
		return nil, err
	}
	var cleanup []releasetransition.RollbackBoundary
	for _, boundary := range boundaries {
		if boundary.Component == "agents" {
			cleanup = append(cleanup, boundary)
		}
	}
	return cleanup, nil
}

// boundaryInspectionRefusal names the declared agent boundaries when the
// retained release refuses to plan: a file it refuses at one of their paths is
// the likely cause, and their recovery replaces the generic next step.
func boundaryInspectionRefusal(message, next string, boundaries []releasetransition.RollbackBoundary) (string, string) {
	if len(boundaries) == 0 {
		return message, next
	}
	var causes, recoveries []string
	for _, boundary := range boundaries {
		causes = append(causes, boundary.Reason+" Paths: "+strings.Join(boundary.Files, ", "))
		recoveries = append(recoveries, boundary.Recovery)
	}
	return message + "; declared rollback limitations: " + strings.Join(causes, "; "),
		"follow the declared recovery, then retry yard update --rollback: " + strings.Join(recoveries, " ")
}

// AgentCleanupConsequences projects accepted limitations into the existing plan
// consequences. It adds no cleanup action, authorization, or protocol family.
func (prepared Prepared) AgentCleanupConsequences() []string {
	var notes []string
	if prepared.AgentCleanupWarning != "" {
		notes = append(notes, prepared.AgentCleanupWarning)
	}
	for _, cleanup := range prepared.AgentCleanup {
		notes = append(notes, "Agent integration rollback limitation (manual cleanup; rollback proceeds): "+cleanup.Reason)
		for _, path := range cleanup.Files {
			notes = append(notes, "Affected yard path: "+path)
		}
		notes = append(notes, "Manual cleanup: "+cleanup.Recovery)
	}
	return notes
}
