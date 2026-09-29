package tests

import (
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit/profilecontract"
)

func TestShippedResourceContract(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	profilecontract.CheckResourceProfileContract(t, root, "orca", []profilecontract.ResourceActionExpectation{
		{Resource: "orca", LocalID: "up", Verb: "up", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactAccess, domain.ImpactHostIncus, domain.ImpactHostNetwork, domain.ImpactHostOS, domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactSecurity, domain.ImpactTrust, domain.ImpactYardRuntime}, Recovery: domain.RecoveryRecreatable},
		{Resource: "orca", LocalID: "is-up", Verb: "is-up", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
		{Resource: "orca", LocalID: "status", Verb: "status", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
		{Resource: "orca", LocalID: "pair", Verb: "pair", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactExternalSystem}, Recovery: domain.RecoveryReversible},
		{Resource: "orca", LocalID: "restart", Verb: "restart", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactYardRuntime}, Recovery: domain.RecoveryReversible},
		{Resource: "orca", LocalID: "sync", Verb: "sync", Effect: domain.ActionBoundedWrite, Recovery: domain.RecoveryNotNeeded},
		{Resource: "orca", LocalID: "logs", Verb: "logs", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
		{Resource: "orca", LocalID: "down", Verb: "down", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactHostOS}, Recovery: domain.RecoveryReversible},
	}, map[string][]string{"orca": {"up", "is-up", "status", "pair", "restart", "sync", "logs", "down"}}, []string{"config/profiles/orca/resources/orca.res"}, nil, nil, nil)
}
