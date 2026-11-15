package releasetransition

import "slices"

func settingsRecoveryKind(m MigrationDefinitionV2) string {
	if m.Kind == localSSHPortMigration || m.Kind == registeredYardsMigration {
		return m.Kind
	}
	return ""
}
func validateEndpointRecovery(recovery settingsRecoveryV1, allowed []string) error {
	if recovery.Kind != registeredYardsMigration {
		return nil
	}
	for _, before := range recovery.Before {
		if !slices.Contains(allowed, before.Setting) {
			return invalid("registered endpoint recovery names an undeclared setting")
		}
	}
	return nil
}
