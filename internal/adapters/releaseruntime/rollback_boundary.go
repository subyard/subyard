package releaseruntime

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/blang/semver/v4"
)

// Read only manifest-bound bytes, rechecked through the pinned release root.
// Declarations never authorize cleanup or bypass any other compatibility check.
func declaredRollbackBoundary(active, target *verifiedPublishedCandidate) (string, string, error) {
	if active.candidate.release == target.candidate.release {
		return "", "", nil
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
		return "the active release's rollback boundaries cannot be verified", retry, err
	}
	if len(declaration.Boundaries) == 0 {
		return "", "", nil
	}
	activeVersion, activeErr := releasetransition.ReleaseIDVersion(active.candidate.release)
	targetVersion, targetErr := releasetransition.ReleaseIDVersion(target.candidate.release)
	if err := domain.CollectRefusals(activeErr, targetErr); err != nil {
		return "release versions cannot be compared with the declared rollback boundaries", retry, err
	}
	if !targetVersion.LT(activeVersion) {
		return "", "", nil
	}
	var findings []error
	for _, boundary := range declaration.Boundaries {
		through := semver.MustParse(boundary.ThroughVersion) // strict declaration parser checked it
		if targetVersion.GT(through) {
			continue
		}
		detail := fmt.Sprintf("declared rollback boundary for %s through %s: %s; next step: %s", boundary.Component, boundary.ThroughVersion, boundary.Reason, boundary.Recovery)
		if len(boundary.Files) != 0 {
			detail += "; yard files: " + strings.Join(boundary.Files, ", ")
		}
		findings = append(findings, fmt.Errorf("%s", detail))
	}
	refusal := domain.CollectRefusals(findings...)
	if refusal == nil {
		return "", "", nil
	}
	return refusal.Error(), retry, refusal
}
