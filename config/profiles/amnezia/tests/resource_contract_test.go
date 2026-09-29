package amnezia_test

import (
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit/profilecontract"
)

func TestShippedResourceContract(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	profilecontract.CheckResourceProfileContract(t, root, "amnezia", []profilecontract.ResourceActionExpectation{
		{Resource: "vpn", LocalID: "up", Verb: "up", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactAccess, domain.ImpactHostIncus, domain.ImpactHostNetwork, domain.ImpactSecurity, domain.ImpactSharedWorkload, domain.ImpactTrust}, Recovery: domain.RecoveryReversible},
		{Resource: "vpn", LocalID: "down", Verb: "down", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactAccess, domain.ImpactHostIncus, domain.ImpactHostNetwork, domain.ImpactSecurity, domain.ImpactSharedWorkload, domain.ImpactTrust}, Recovery: domain.RecoveryReversible},
		{Resource: "vpn", LocalID: "status", Verb: "status", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
		{Resource: "vpn", LocalID: "is-up", Verb: "is-up", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
	}, map[string][]string{"vpn": {"up", "down", "status", "is-up"}}, []string{"config/profiles/amnezia/resources/vpn.res"}, nil, nil, nil)
}
