package releasetransition

import "encoding/json"

const RollbackSourceOwnerStep = "rollback-source-owner-v1"

// RollbackSourceOwnerBinding is kind-independent provenance for S's exact
// sealed image. Its digest occupies an ordinary immutable V2 intent row, so R
// can verify the owner before executing S without decoding S's work recipes.
type RollbackSourceOwnerBinding struct {
	RuntimeRoot, ConfigHome        string
	Releases                       ReleasePair
	SourceArtifact, SourceRegistry Fingerprint
	TargetArtifact, TargetRegistry Fingerprint
}

func (binding RollbackSourceOwnerBinding) Fingerprint() (Fingerprint, error) {
	for _, root := range []string{binding.RuntimeRoot, binding.ConfigHome} {
		if err := validateSourceIngressRolePath(root); err != nil {
			return "", err
		}
	}
	if err := binding.Releases.Validate(); err != nil {
		return "", err
	}
	if binding.Releases.From == binding.Releases.Target {
		return "", invalid("rollback source owner requires distinct endpoints")
	}
	for _, pin := range []Fingerprint{binding.SourceArtifact, binding.SourceRegistry, binding.TargetArtifact, binding.TargetRegistry} {
		if err := validateFingerprint(pin, "rollback source owner binding"); err != nil {
			return "", err
		}
	}
	payload, err := json.Marshal(struct {
		Contract string
		Binding  RollbackSourceOwnerBinding
	}{RollbackSourceOwnerStep, binding})
	if err != nil {
		return "", err
	}
	return fingerprintPayload(payload), nil
}

func HasRollbackSourceOwner(journal JournalRecord) bool {
	for _, step := range journal.Steps {
		if step.ID == RollbackSourceOwnerStep {
			return true
		}
	}
	return false
}

func (binding RollbackSourceOwnerBinding) CheckJournal(journal JournalRecord) error {
	if err := journal.Validate(); err != nil {
		return err
	}
	pin, err := binding.Fingerprint()
	if err != nil {
		return err
	}
	if journal.SourceIngress != nil || IsRecoveryTransaction(journal.Transaction) || journal.Goal.Direction != DirectionActivatePrevious || !releasePairsEqual(journal.Releases, binding.Releases) || journal.RegistryDigest != binding.SourceRegistry || journal.ArtifactDigest != binding.TargetArtifact {
		return invalid("rollback source owner differs from the selected journal")
	}
	for _, step := range journal.Steps {
		if step.ID == RollbackSourceOwnerStep && step.Migration == RollbackSourceOwnerStep && step.Resource == "rollback.source-owner" && step.Decision == DecisionPreserve && step.Expected == pin && step.Desired == pin {
			return nil
		}
	}
	return invalid("rollback source owner does not match the exact sealed source")
}

func rollbackRequestSourceOwner(request RollbackTargetRequest) RollbackSourceOwnerBinding {
	return RollbackSourceOwnerBinding{RuntimeRoot: request.RuntimeRoot, ConfigHome: request.ConfigHome, Releases: request.Releases, SourceArtifact: request.SourceArtifact, SourceRegistry: request.SourceRegistry, TargetArtifact: request.TargetArtifact, TargetRegistry: request.TargetRegistry}
}
