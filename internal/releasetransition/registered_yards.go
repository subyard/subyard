package releasetransition

import (
	"errors"

	"github.com/Subyard/Subyard/internal/domain"
)

const registeredYardsMigration = "registered-yards-v2-to-v3"
const guestDiskTmpMigration = "registered-guest-disk-tmp-v2-to-v3"

// Frozen native step grammar for retained journals. These kinds have no handler.
func nativeMigration(m MigrationDefinitionV2) bool {
	return m.FromEpoch == 2 && m.ToEpoch == 3 &&
		(m.Kind == registeredYardsMigration && m.Domain == "owner-registration" ||
			m.Kind == guestDiskTmpMigration && m.Domain == "power-metadata")
}

// VerifiedNativeMigrationKinds reports which native migrations this journal proves
// were authorized and verified for a yard. It confers no other authority.
func VerifiedNativeMigrationKinds(registry RegistryV2, journal JournalRecord, yard string) (registered, guestDiskTmp bool) {
	for _, step := range journal.Steps {
		if step.Resource != "yard."+yard || step.Checkpoint != StepVerified {
			continue
		}
		for _, migration := range registry.historyData().Migrations {
			if migration.ID == step.Migration && nativeMigration(migration) {
				registered = registered || migration.Kind == registeredYardsMigration
				guestDiskTmp = guestDiskTmp || migration.Kind == guestDiskTmpMigration
			}
		}
	}
	return registered, guestDiskTmp
}

// A settings-CAS recheck may refuse after authorization. This public cause is
// recoverable when the transient refusal is gone on the next observation.
type migrationBlockerError struct{ Blocker }

func (failure migrationBlockerError) Error() string { return failure.Message }
func (failure migrationBlockerError) ActivationDiagnostic() (string, string) {
	return failure.Message, failure.Retry
}

func refusalCauses(err error) []error {
	if _, public := err.(interface{ ActivationDiagnostic() (string, string) }); public {
		return []error{err}
	}
	var refusals *domain.Refusals
	if errors.As(err, &refusals) {
		var causes []error
		for _, cause := range refusals.Unwrap() {
			causes = append(causes, refusalCauses(cause)...)
		}
		return causes
	}
	return []error{err}
}
