package ownerpublication

import (
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// OriginalTarget is the original owner identity and approved final power state.
// It is never inferred from a later instance with the same name.
type OriginalTarget struct {
	Project, Name, UUID string
	Desired             domain.DesiredPower
	Kind                domain.YardKind
	Scope               string
}

// Check fences publication restoration against power or ownership drift.
func (target OriginalTarget) Check(instance ports.InstanceInfo) error {
	if target.UUID == "" || target.Desired != domain.PowerRunning && target.Desired != domain.PowerStopped || instance.Project != target.Project || instance.Name != target.Name || (target.Kind != domain.YardVM && target.Kind != domain.YardContainer) || instance.Type != target.Kind || !strings.EqualFold(instance.Status, string(target.Desired)) {
		return domain.ErrPlanStale
	}
	for _, config := range []map[string]string{instance.Config, instance.LocalConfig} {
		if config["volatile.uuid"] != target.UUID || config["user.subyard.managed"] != "true" || config["user.subyard.initialized"] != "true" || config["user.subyard.desired_power"] != string(target.Desired) {
			return domain.ErrPlanStale
		}
	}
	return nil
}
