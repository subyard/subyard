package releaseruntime

import (
	"path/filepath"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

// Low-level invocation tests still exchange complete wire messages, even when
// their assertion concerns a file descriptor rather than a release outcome.
const candidateProtocolFixtureResponse = `{"schemaVersion":1,"activationReconciliationOwned":false,"outcome":{"status":"operator-action-required","reachedGoal":false,"active":"release-a","target":"release-b","code":"dependency-unavailable","message":"fixture response","retry":"yard update --check"}}`

func candidateProtocolRequest(candidate *verifiedPublishedCandidate, runtimeRoot string) releasetransition.ProcessRequest {
	return releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Mode:          releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot,
		ConfigHome: filepath.Join(runtimeRoot, "config"), Yard: "default",
		Target: candidate.candidate.release, Direction: releasetransition.DirectionActivateTarget,
		ArtifactDigest: candidate.manifestDigest,
	}
}
