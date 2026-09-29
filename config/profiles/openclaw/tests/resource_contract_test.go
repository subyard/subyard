package tests

import (
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit/profilecontract"
)

func TestShippedResourceContract(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	profilecontract.CheckResourceProfileContract(t, root, "openclaw", []profilecontract.ResourceActionExpectation{
		{Resource: "qa-bot-broker", LocalID: "up", Verb: "up", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, Recovery: domain.RecoveryRecreatable},
		{Resource: "qa-bot-broker", LocalID: "seed", Verb: "seed", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, Recovery: domain.RecoveryReversible},
		{Resource: "qa-bot-broker", LocalID: "expose", Verb: "expose", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactAccess, domain.ImpactSecurity, domain.ImpactTrust}, Recovery: domain.RecoveryReversible},
		{Resource: "qa-bot-broker", LocalID: "status", Verb: "status", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
		{Resource: "qa-bot-broker", LocalID: "logs", Verb: "logs", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
		{Resource: "qa-bot-broker", LocalID: "smoke", Verb: "smoke", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, Recovery: domain.RecoveryReversible},
		{Resource: "qa-bot-broker", LocalID: "down", Verb: "down", Effect: domain.ActionDestruction, Impacts: []domain.ActionImpact{domain.ImpactYardRuntime}, Recovery: domain.RecoveryRecreatable},
		{Resource: "qa-bot-broker", LocalID: "destroy", Verb: "destroy", Effect: domain.ActionDestruction, Impacts: []domain.ActionImpact{domain.ImpactYardRuntime}, Recovery: domain.RecoveryRecreatable},
		{Resource: "qa-bot-broker", LocalID: "destroy-purge", Verb: "destroy", Effect: domain.ActionDestruction, Impacts: []domain.ActionImpact{domain.ImpactPersistentData}, Recovery: domain.RecoveryIrreversible},
		{Resource: "staging-gateway", LocalID: "up", Verb: "up", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, Recovery: domain.RecoveryRecreatable},
		{Resource: "staging-gateway", LocalID: "start", Verb: "start", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, Recovery: domain.RecoveryReversible},
		{Resource: "staging-gateway", LocalID: "stop", Verb: "stop", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, Recovery: domain.RecoveryReversible},
		{Resource: "staging-gateway", LocalID: "status", Verb: "status", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
		{Resource: "staging-gateway", LocalID: "logs", Verb: "logs", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
		{Resource: "staging-gateway", LocalID: "shell", Verb: "shell", Effect: domain.ActionSession, Recovery: domain.RecoveryNotNeeded},
		{Resource: "staging-gateway", LocalID: "down", Verb: "down", Effect: domain.ActionDestruction, Impacts: []domain.ActionImpact{domain.ImpactYardRuntime}, Recovery: domain.RecoveryRecreatable},
		{Resource: "staging-gateway", LocalID: "destroy", Verb: "destroy", Effect: domain.ActionDestruction, Impacts: []domain.ActionImpact{domain.ImpactYardRuntime}, Recovery: domain.RecoveryRecreatable},
		{Resource: "staging-gateway", LocalID: "destroy-purge", Verb: "destroy", Effect: domain.ActionDestruction, Impacts: []domain.ActionImpact{domain.ImpactPersistentData}, Recovery: domain.RecoveryIrreversible},
		{Resource: "staging-gateway", LocalID: "list", Verb: "list", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
	}, map[string][]string{
		"qa-bot-broker":   {"up", "seed", "expose", "status", "logs", "smoke", "down", "destroy"},
		"staging-gateway": {"up", "start", "stop", "status", "logs", "shell", "down", "destroy", "list"},
	}, []string{"config/profiles/openclaw/resources/qa-bot-broker.res", "config/profiles/openclaw/resources/staging-gateway.res"}, nil,
		[]profilecontract.ResourceActionExpectation{
			{Resource: "staging", Verb: "e2e", LocalID: "e2e"},
		},
		[]profilecontract.ResourceActionExpectation{
			{Resource: "qa-pool", Verb: "destroy", LocalID: "destroy"},
			{Resource: "qa-pool", Verb: "destroy", LocalID: "destroy-purge"},
			{Resource: "staging", Verb: "destroy", LocalID: "destroy"},
			{Resource: "staging", Verb: "destroy", LocalID: "destroy-purge"},
		},
	)
}
