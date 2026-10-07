package tests

import (
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit/profilecontract"
)

func TestShippedResourceContract(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	registry, err := resource.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range registry.Definitions() {
		if definition.Profile == "android" && (definition.RemotePolicy("view") != domain.RemoteOnController || definition.RemotePolicy("run") != domain.RemoteOnOwner) {
			t.Fatal("Android viewer must run on controller while workloads stay on owner")
		}
	}
	profilecontract.CheckResourceProfileContract(t, root, "android", []profilecontract.ResourceActionExpectation{
		{Resource: "emulator", LocalID: "catalog", Verb: "catalog", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
		{Resource: "emulator", LocalID: "status", Verb: "status", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
		{Resource: "emulator", LocalID: "run", Verb: "run", Effect: domain.ActionSession, Recovery: domain.RecoveryNotNeeded},
		{Resource: "emulator", LocalID: "acquire", Verb: "acquire", Effect: domain.ActionBoundedWrite, Recovery: domain.RecoveryNotNeeded},
		{Resource: "emulator", LocalID: "renew", Verb: "renew", Effect: domain.ActionBoundedWrite, Recovery: domain.RecoveryNotNeeded},
		{Resource: "emulator", LocalID: "release", Verb: "release", Effect: domain.ActionBoundedWrite, Recovery: domain.RecoveryNotNeeded},
		{Resource: "emulator", LocalID: "cache", Verb: "cache", Effect: domain.ActionBoundedWrite, Recovery: domain.RecoveryNotNeeded},
		{Resource: "emulator", LocalID: "view", Verb: "view", Effect: domain.ActionSession, Recovery: domain.RecoveryNotNeeded},
		{Resource: "emulator", LocalID: "revoke", Verb: "revoke", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, Recovery: domain.RecoveryReversible},
		{Resource: "emulator", LocalID: "down", Verb: "down", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, Recovery: domain.RecoveryReversible},
	}, map[string][]string{"emulator": {"catalog", "status", "run", "acquire", "renew", "release", "cache", "view", "revoke", "down"}}, []string{"config/profiles/android/resources/emulator.res"}, map[string][]string{"config/profiles/android/resources/emulator/handler.sh": {"down | stop)"}}, nil, nil)
}
