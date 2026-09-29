package tests

import (
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit/profilecontract"
)

func TestShippedResourceContract(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	profilecontract.CheckResourceProfileContract(t, root, "hermes", []profilecontract.ResourceActionExpectation{
		{Resource: "dashboard", LocalID: "up", Verb: "up", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactAccess, domain.ImpactSecurity, domain.ImpactTrust}, Recovery: domain.RecoveryReversible},
		{Resource: "dashboard", LocalID: "is-up", Verb: "is-up", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
		{Resource: "dashboard", LocalID: "status", Verb: "status", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
		{Resource: "dashboard", LocalID: "down", Verb: "down", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactAccess, domain.ImpactSecurity, domain.ImpactTrust}, Recovery: domain.RecoveryReversible},
	}, map[string][]string{"dashboard": {"up", "is-up", "status", "down"}}, []string{"config/profiles/hermes/resources/dashboard.res"}, nil, nil, nil)
}
