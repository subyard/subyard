package resource

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestAssessPrepareResultCombinesTrustedMetadataWithValidatedDelta(t *testing.T) {
	registry, actions := testActionRegistry(t)

	assessment, err := registry.AssessPrepareResult(actions, "svc", "destroy", []byte(
		`{"schema":"yard.resource-action-assessment.v1","action":"destroy-purge","changed":true,"consequences":["wipe persistent service data"]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ActionAssessment{
		Action:       "resource.sample.service.destroy-purge",
		Effect:       domain.ActionDestruction,
		Changed:      true,
		Impacts:      []domain.ActionImpact{domain.ImpactPersistentData},
		Recovery:     domain.RecoveryIrreversible,
		Consequences: []string{"wipe persistent service data"},
	}
	if !reflect.DeepEqual(assessment, want) {
		t.Fatalf("assessment = %#v, want %#v", assessment, want)
	}

	assessment.Consequences[0] = "tampered"
	second, err := registry.AssessPrepareResult(actions, "service", "destroy", []byte(
		`{"schema":"yard.resource-action-assessment.v1","action":"destroy","changed":true,"consequences":["remove runtime"]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if second.Action != "resource.sample.service.destroy" || second.Recovery != domain.RecoveryRecreatable ||
		!reflect.DeepEqual(second.Impacts, []domain.ActionImpact{domain.ImpactYardRuntime}) {
		t.Fatalf("runtime destroy assessment = %#v", second)
	}

	unchanged, err := registry.AssessPrepareResult(actions, "svc", "status", []byte(
		`{"schema":"yard.resource-action-assessment.v1","action":"status","changed":false,"consequences":[]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Action != "resource.sample.service.status" || unchanged.Effect != domain.ActionRead ||
		unchanged.Changed || unchanged.Recovery != domain.RecoveryNotNeeded || len(unchanged.Consequences) != 0 {
		t.Fatalf("unchanged assessment = %#v", unchanged)
	}
}

func TestAssessPrepareResultRejectsMalformedOrSensitiveOutput(t *testing.T) {
	registry, actions := testActionRegistry(t)
	validPrefix := `{"schema":"yard.resource-action-assessment.v1","action":"destroy","changed":true,"consequences":`
	consequenceList := func(values ...string) string {
		quoted := make([]string, len(values))
		for index, value := range values {
			quoted[index] = fmt.Sprintf("%q", value)
		}
		return validPrefix + "[" + strings.Join(quoted, ",") + "]}"
	}
	manyConsequences := make([]string, 65)
	for index := range manyConsequences {
		manyConsequences[index] = fmt.Sprintf("change %d", index)
	}
	tests := []struct {
		name   string
		output string
	}{
		{name: "empty", output: ""},
		{name: "malformed", output: "{"},
		{name: "wrong schema", output: `{"schema":"other","action":"destroy","changed":true,"consequences":["remove runtime"]}`},
		{name: "missing schema", output: `{"action":"destroy","changed":true,"consequences":["remove runtime"]}`},
		{name: "missing action", output: `{"schema":"yard.resource-action-assessment.v1","changed":true,"consequences":["remove runtime"]}`},
		{name: "missing changed", output: `{"schema":"yard.resource-action-assessment.v1","action":"destroy","consequences":["remove runtime"]}`},
		{name: "missing consequences", output: `{"schema":"yard.resource-action-assessment.v1","action":"destroy","changed":true}`},
		{name: "trailing document", output: consequenceList("remove runtime") + `{}`},
		{name: "trailing garbage", output: consequenceList("remove runtime") + ` trailing`},
		{name: "duplicate field", output: `{"schema":"yard.resource-action-assessment.v1","action":"destroy","action":"status","changed":false,"consequences":[]}`},
		{name: "case variant field", output: `{"Schema":"yard.resource-action-assessment.v1","action":"destroy","changed":false,"consequences":[]}`},
		{name: "case insensitive duplicate field", output: `{"schema":"yard.resource-action-assessment.v1","Schema":"yard.resource-action-assessment.v1","action":"destroy","changed":false,"consequences":[]}`},
		{name: "too many consequences", output: consequenceList(manyConsequences...)},
		{name: "empty consequence", output: consequenceList("")},
		{name: "whitespace consequence", output: consequenceList("  ")},
		{name: "untrimmed consequence", output: consequenceList(" remove runtime ")},
		{name: "long consequence", output: consequenceList(strings.Repeat("a", 513))},
		{name: "control character", output: consequenceList("remove\nruntime")},
		{name: "private key", output: consequenceList("-----BEGIN OPENSSH PRIVATE KEY-----")},
		{name: "named secret", output: consequenceList("api_token=ghp_abcdefghijklmnopqrstuvwxyz0123456789")},
		{name: "github token", output: consequenceList("rotate ghp_abcdefghijklmnopqrstuvwxyz0123456789")},
		{name: "openai token", output: consequenceList("rotate sk-proj-abcdefghijklmnopqrstuvwxyz0123456789")},
		{name: "slack token", output: consequenceList("rotate " + "xox" + "b-fixture")},
		{name: "jwt", output: consequenceList("replace eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signature")},
		{name: "opaque token", output: consequenceList("replace Abcdefghijklmnopqrstuvwxyz0123456789")},
		{name: "opaque token beside fingerprint", output: consequenceList("record SHA256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef fingerprint and replace Abcdefghijklmnopqrstuvwxyz0123456789")},
		{name: "opaque token labeled fingerprint", output: consequenceList("record fingerprint Abcdefghijklmnopqrstuvwxyz0123456789")},
		{name: "opaque token prefixed sha256", output: consequenceList("record sha256:Abcdefghijklmnopqrstuvwxyz0123456789")},
		{name: "ssh protected path", output: consequenceList("remove /home/dev/.ssh/id_ed25519")},
		{name: "runtime secret path", output: consequenceList("remove /run/secrets/service-token")},
		{name: "changed mutation without consequence", output: consequenceList()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := registry.AssessPrepareResult(actions, "svc", "destroy", []byte(test.output))
			if !errors.Is(err, ErrResourcePlanInvalid) || ResourceErrorClass(err) != ResourcePlanInvalid {
				t.Fatalf("error = %v, class = %q", err, ResourceErrorClass(err))
			}
		})
	}

	oversized := make([]byte, MaxPrepareOutputBytes+1)
	_, err := registry.AssessPrepareResult(actions, "svc", "destroy", oversized)
	if !errors.Is(err, ErrResourcePlanInvalid) || ResourceErrorClass(err) != ResourcePlanInvalid {
		t.Fatalf("oversized error = %v, class = %q", err, ResourceErrorClass(err))
	}
	_, err = registry.AssessPrepareResult(actions, "svc", "destroy", []byte{'{', 0xff, '}'})
	if !errors.Is(err, ErrResourcePlanInvalid) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
}

func TestAssessPrepareResultRejectsHandlerOwnedPolicyMetadata(t *testing.T) {
	registry, actions := testActionRegistry(t)
	for _, field := range []string{
		`"effect":"read"`,
		`"impacts":[]`,
		`"recovery":"not-needed"`,
		`"policy":"never"`,
		`"summary":"safe"`,
	} {
		output := `{"schema":"yard.resource-action-assessment.v1","action":"destroy-purge","changed":true,"consequences":["wipe data"],` + field + `}`
		_, err := registry.AssessPrepareResult(actions, "svc", "destroy", []byte(output))
		if !errors.Is(err, ErrResourcePlanInvalid) {
			t.Fatalf("handler metadata %s: error = %v", field, err)
		}
	}
}

func TestAssessPrepareResultRejectsUnknownOrVerbMismatchedActions(t *testing.T) {
	registry, actions := testActionRegistry(t)
	tests := []struct {
		resource string
		verb     string
		action   string
	}{
		{resource: "unknown", verb: "destroy", action: "destroy"},
		{resource: "svc", verb: "status", action: "destroy"},
		{resource: "svc", verb: "destroy", action: "status"},
		{resource: "svc", verb: "destroy", action: "undeclared"},
	}
	for _, test := range tests {
		output := fmt.Sprintf(`{"schema":"yard.resource-action-assessment.v1","action":%q,"changed":false,"consequences":[]}`, test.action)
		_, err := registry.AssessPrepareResult(actions, test.resource, test.verb, []byte(output))
		if !errors.Is(err, ErrResourceActionUnknown) || ResourceErrorClass(err) != ResourceActionUnknown {
			t.Fatalf("%s/%s/%s: error = %v, class = %q", test.resource, test.verb, test.action, err, ResourceErrorClass(err))
		}
	}
}

func TestAssessPrepareResultAllowsRedactedAndDigestConsequences(t *testing.T) {
	registry, actions := testActionRegistry(t)
	tests := []struct {
		name        string
		consequence string
	}{
		{name: "redacted", consequence: "rotate token [REDACTED]"},
		{name: "sha256", consequence: "record SHA256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef fingerprint"},
		{name: "fingerprint label", consequence: "record fingerprint 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{name: "checksum label", consequence: "record checksum 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{name: "digest label", consequence: "record digest 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := []byte(fmt.Sprintf(
				`{"schema":"yard.resource-action-assessment.v1","action":"destroy","changed":true,"consequences":[%q]}`,
				test.consequence,
			))
			if _, err := registry.AssessPrepareResult(actions, "svc", "destroy", output); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoadDerivesTypedActionsAndBoundsLookup(t *testing.T) {
	root := t.TempDir()
	writeTestResource(t, root, "sample", "service", `
COMMAND=svc
HANDLER=resources/service/handler.sh
TITLE="Sample service"
ACTION="status status read-only not-needed"
ACTION="terminal shell session not-needed"
ACTION="inspect inspect bounded-write not-needed"
ACTION="up up yard-change reversible"
ACTION="host host host-change reversible"
ACTION="publish publish external-change reversible"
ACTION="authorize authorize security-change reversible"
ACTION="share share shared-workload-change reversible"
ACTION="destroy destroy runtime-destruction recreatable"
ACTION="destroy-purge destroy persistent-data-destruction irreversible"
BRINGUP=up
SHUTDOWN=destroy
`)

	registry, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := registry.Lookup("svc")
	if !ok {
		t.Fatal("resource command is not indexed")
	}
	wantVerbs := []string{"status", "shell", "inspect", "up", "host", "publish", "authorize", "share", "destroy"}
	if !slices.Equal(definition.Verbs, wantVerbs) {
		t.Fatalf("derived verbs = %#v, want %#v", definition.Verbs, wantVerbs)
	}

	wantDefinitions := []domain.ActionDefinition{
		{Action: "resource.sample.service.status", Summary: "Sample service: status", Effect: domain.ActionRead, Recovery: domain.RecoveryNotNeeded},
		{Action: "resource.sample.service.terminal", Summary: "Sample service: terminal", Effect: domain.ActionSession, Recovery: domain.RecoveryNotNeeded},
		{Action: "resource.sample.service.inspect", Summary: "Sample service: inspect", Effect: domain.ActionBoundedWrite, Recovery: domain.RecoveryNotNeeded},
		{Action: "resource.sample.service.up", Summary: "Sample service: up", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactYardRuntime}, Recovery: domain.RecoveryReversible},
		{Action: "resource.sample.service.host", Summary: "Sample service: host", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactHostOS}, Recovery: domain.RecoveryReversible},
		{Action: "resource.sample.service.publish", Summary: "Sample service: publish", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactExternalSystem}, Recovery: domain.RecoveryReversible},
		{Action: "resource.sample.service.authorize", Summary: "Sample service: authorize", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactAccess, domain.ImpactSecurity, domain.ImpactTrust}, Recovery: domain.RecoveryReversible},
		{Action: "resource.sample.service.share", Summary: "Sample service: share", Effect: domain.ActionMutation, Impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, Recovery: domain.RecoveryReversible},
		{Action: "resource.sample.service.destroy", Summary: "Sample service: destroy", Effect: domain.ActionDestruction, Impacts: []domain.ActionImpact{domain.ImpactYardRuntime}, Recovery: domain.RecoveryRecreatable},
		{Action: "resource.sample.service.destroy-purge", Summary: "Sample service: destroy-purge", Effect: domain.ActionDestruction, Impacts: []domain.ActionImpact{domain.ImpactPersistentData}, Recovery: domain.RecoveryIrreversible},
	}
	if got := registry.ActionDefinitions(); !reflect.DeepEqual(got, wantDefinitions) {
		t.Fatalf("action definitions = %#v, want %#v", got, wantDefinitions)
	}

	for _, localID := range []string{"destroy", "destroy-purge"} {
		qualified, found := registry.LookupAction("svc", "destroy", localID)
		if !found || qualified != domain.ActionID("resource.sample.service."+localID) {
			t.Fatalf("lookup %q = %q, %t", localID, qualified, found)
		}
	}
	if _, found := registry.LookupAction("svc", "status", "destroy"); found {
		t.Fatal("verb-mismatched action was reachable")
	}
	if _, found := registry.LookupAction("unknown", "destroy", "destroy"); found {
		t.Fatal("action was reachable through an unknown resource")
	}

	definitions := registry.ActionDefinitions()
	definitions[0].Action = "tampered"
	if got := registry.ActionDefinitions()[0].Action; got != "resource.sample.service.status" {
		t.Fatalf("action definition mutation leaked into registry: %q", got)
	}
	definitions[6].Impacts[0] = domain.ImpactPersistentData
	if got := registry.ActionDefinitions()[6].Impacts[0]; got != domain.ImpactAccess {
		t.Fatalf("action impacts mutation leaked into registry: %q", got)
	}
}

func TestLoadRejectsInvalidActionDescriptors(t *testing.T) {
	tests := []struct {
		name   string
		record string
	}{
		{name: "legacy verbs", record: "VERBS=up down\nACTION=\"up up yard-change reversible\"\nACTION=\"down down yard-change reversible\""},
		{name: "missing action", record: ""},
		{name: "malformed action", record: "ACTION=\"up up yard-change\""},
		{name: "invalid local ID", record: "ACTION=\"../up up yard-change reversible\""},
		{name: "invalid public verb", record: "ACTION=\"up ../up yard-change reversible\""},
		{name: "duplicate local ID", record: "ACTION=\"same up yard-change reversible\"\nACTION=\"same down yard-change reversible\""},
		{name: "duplicate identical record", record: "ACTION=\"same up yard-change reversible\"\nACTION=\"same up yard-change reversible\""},
		{name: "unknown class", record: "ACTION=\"up up invisible reversible\""},
		{name: "unknown recovery", record: "ACTION=\"up up yard-change repaired\""},
		{name: "read recovery contradiction", record: "ACTION=\"up up read-only reversible\""},
		{name: "mutation recovery contradiction", record: "ACTION=\"up up yard-change not-needed\""},
		{name: "missing bringup", record: "ACTION=\"down down yard-change reversible\""},
		{name: "missing shutdown", record: "ACTION=\"up up yard-change reversible\""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeTestResource(t, root, "sample", "service", "HANDLER=resources/service/handler.sh\nTITLE=Sample\n"+test.record+"\n")
			if _, err := Load(root); err == nil {
				t.Fatal("invalid descriptor was accepted")
			}
		})
	}
}

func TestLoadRepositoryResources(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	registry, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	definitions := registry.Definitions()
	if len(definitions) == 0 {
		t.Fatal("repository resources were not found")
	}
	for _, definition := range definitions {
		if _, ok := registry.Lookup(definition.Command); !ok {
			t.Fatalf("resource command is not indexed: %s", definition.Command)
		}
		if byName, ok := registry.Lookup(definition.Name); !ok || byName.HandlerPath() != definition.HandlerPath() {
			t.Fatalf("resource name and command differ: %s", definition.Name)
		}
		content, err := os.ReadFile(definition.HandlerPath())
		if err != nil {
			t.Fatal(err)
		}
		source := string(content)
		if !strings.Contains(source, "subyard_require_engine_context") ||
			strings.Contains(source, "subyard_context_load") || strings.Contains(source, "lib/config.sh") {
			t.Errorf("resource handler does not consume only prepared context: %s", definition.HandlerPath())
		}
	}
}

func TestRepositoryResourceActionMatrix(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	registry, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	type expectedAction struct {
		resource string
		localID  string
		verb     string
		effect   domain.ActionEffect
		impacts  []domain.ActionImpact
		recovery domain.RecoveryClass
	}
	expected := []expectedAction{
		{resource: "emulator", localID: "up", verb: "up", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactHostOS}, recovery: domain.RecoveryReversible},
		{resource: "emulator", localID: "down", verb: "down", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactHostOS}, recovery: domain.RecoveryReversible},
		{resource: "emulator", localID: "status", verb: "status", effect: domain.ActionRead, recovery: domain.RecoveryNotNeeded},
		{resource: "emulator", localID: "view", verb: "view", effect: domain.ActionSession, recovery: domain.RecoveryNotNeeded},

		{resource: "qa-bot-broker", localID: "up", verb: "up", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, recovery: domain.RecoveryRecreatable},
		{resource: "qa-bot-broker", localID: "seed", verb: "seed", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, recovery: domain.RecoveryReversible},
		{resource: "qa-bot-broker", localID: "expose", verb: "expose", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactAccess, domain.ImpactSecurity, domain.ImpactTrust}, recovery: domain.RecoveryReversible},
		{resource: "qa-bot-broker", localID: "status", verb: "status", effect: domain.ActionRead, recovery: domain.RecoveryNotNeeded},
		{resource: "qa-bot-broker", localID: "logs", verb: "logs", effect: domain.ActionRead, recovery: domain.RecoveryNotNeeded},
		{resource: "qa-bot-broker", localID: "smoke", verb: "smoke", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, recovery: domain.RecoveryReversible},
		{resource: "qa-bot-broker", localID: "down", verb: "down", effect: domain.ActionDestruction, impacts: []domain.ActionImpact{domain.ImpactYardRuntime}, recovery: domain.RecoveryRecreatable},
		{resource: "qa-bot-broker", localID: "destroy", verb: "destroy", effect: domain.ActionDestruction, impacts: []domain.ActionImpact{domain.ImpactYardRuntime}, recovery: domain.RecoveryRecreatable},
		{resource: "qa-bot-broker", localID: "destroy-purge", verb: "destroy", effect: domain.ActionDestruction, impacts: []domain.ActionImpact{domain.ImpactPersistentData}, recovery: domain.RecoveryIrreversible},

		{resource: "staging-gateway", localID: "up", verb: "up", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, recovery: domain.RecoveryRecreatable},
		{resource: "staging-gateway", localID: "start", verb: "start", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, recovery: domain.RecoveryReversible},
		{resource: "staging-gateway", localID: "stop", verb: "stop", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactSharedWorkload}, recovery: domain.RecoveryReversible},
		{resource: "staging-gateway", localID: "status", verb: "status", effect: domain.ActionRead, recovery: domain.RecoveryNotNeeded},
		{resource: "staging-gateway", localID: "logs", verb: "logs", effect: domain.ActionRead, recovery: domain.RecoveryNotNeeded},
		{resource: "staging-gateway", localID: "shell", verb: "shell", effect: domain.ActionSession, recovery: domain.RecoveryNotNeeded},
		{resource: "staging-gateway", localID: "down", verb: "down", effect: domain.ActionDestruction, impacts: []domain.ActionImpact{domain.ImpactYardRuntime}, recovery: domain.RecoveryRecreatable},
		{resource: "staging-gateway", localID: "destroy", verb: "destroy", effect: domain.ActionDestruction, impacts: []domain.ActionImpact{domain.ImpactYardRuntime}, recovery: domain.RecoveryRecreatable},
		{resource: "staging-gateway", localID: "destroy-purge", verb: "destroy", effect: domain.ActionDestruction, impacts: []domain.ActionImpact{domain.ImpactPersistentData}, recovery: domain.RecoveryIrreversible},
		{resource: "staging-gateway", localID: "list", verb: "list", effect: domain.ActionRead, recovery: domain.RecoveryNotNeeded},

		{resource: "orca", localID: "up", verb: "up", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactHostOS}, recovery: domain.RecoveryRecreatable},
		{resource: "orca", localID: "is-up", verb: "is-up", effect: domain.ActionRead, recovery: domain.RecoveryNotNeeded},
		{resource: "orca", localID: "status", verb: "status", effect: domain.ActionRead, recovery: domain.RecoveryNotNeeded},
		{resource: "orca", localID: "pair", verb: "pair", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactExternalSystem}, recovery: domain.RecoveryReversible},
		{resource: "orca", localID: "sync", verb: "sync", effect: domain.ActionBoundedWrite, recovery: domain.RecoveryNotNeeded},
		{resource: "orca", localID: "logs", verb: "logs", effect: domain.ActionRead, recovery: domain.RecoveryNotNeeded},
		{resource: "orca", localID: "down", verb: "down", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactHostOS}, recovery: domain.RecoveryReversible},
	}

	definitions := make(map[domain.ActionID]domain.ActionDefinition)
	for _, definition := range registry.ActionDefinitions() {
		if _, duplicate := definitions[definition.Action]; duplicate {
			t.Fatalf("duplicate shipped action definition %q", definition.Action)
		}
		definitions[definition.Action] = definition
	}
	if len(definitions) != len(expected) {
		t.Fatalf("shipped action definitions = %d, want %d", len(definitions), len(expected))
	}
	for _, action := range expected {
		qualified, ok := registry.LookupAction(action.resource, action.verb, action.localID)
		if !ok {
			t.Errorf("missing %s action %s for verb %s", action.resource, action.localID, action.verb)
			continue
		}
		definition, ok := definitions[qualified]
		if !ok {
			t.Errorf("lookup %q has no domain definition", qualified)
			continue
		}
		if definition.Effect != action.effect || definition.Recovery != action.recovery ||
			!reflect.DeepEqual(definition.Impacts, action.impacts) {
			t.Errorf("%q classification = effect %q impacts %#v recovery %q", qualified, definition.Effect, definition.Impacts, definition.Recovery)
		}
	}

	wantVerbs := map[string][]string{
		"emulator":        {"up", "down", "status", "view"},
		"qa-bot-broker":   {"up", "seed", "expose", "status", "logs", "smoke", "down", "destroy"},
		"staging-gateway": {"up", "start", "stop", "status", "logs", "shell", "down", "destroy", "list"},
		"orca":            {"up", "is-up", "status", "pair", "sync", "logs", "down"},
	}
	for resourceName, verbs := range wantVerbs {
		definition, ok := registry.Lookup(resourceName)
		if !ok || !slices.Equal(definition.Verbs, verbs) {
			t.Errorf("%s verbs = %#v, want %#v", resourceName, definition.Verbs, verbs)
		}
	}
	if _, ok := registry.LookupAction("staging", "e2e", "e2e"); ok {
		t.Error("deferred staging e2e remains declared")
	}
	for _, resourceName := range []string{"qa-pool", "staging"} {
		for _, localID := range []string{"destroy", "destroy-purge"} {
			if _, ok := registry.LookupAction(resourceName, "destroy", localID); !ok {
				t.Errorf("%s %s is not reachable through destroy", resourceName, localID)
			}
		}
	}

	for _, descriptor := range []string{
		"config/profiles/android/resources/emulator.res",
		"config/profiles/openclaw/resources/qa-bot-broker.res",
		"config/profiles/openclaw/resources/staging-gateway.res",
		"config/profiles/orca/resources/orca.res",
	} {
		content, err := os.ReadFile(filepath.Join(root, descriptor))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), "VERBS=") {
			t.Errorf("legacy VERBS remains in %s", descriptor)
		}
	}
	emulatorHandler, err := os.ReadFile(filepath.Join(root, "config/profiles/android/resources/emulator/handler.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(emulatorHandler), "down | stop)") {
		t.Error("undeclared emulator stop alias remains reachable")
	}
}

func TestLoadRejectsEscapingHandler(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "config", "profiles", "profile", "resources")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "HANDLER=../../../../escape\nTITLE=x\nACTION=\"up up yard-change reversible\"\nACTION=\"down down yard-change reversible\"\n"
	if err := os.WriteFile(filepath.Join(directory, "service.res"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil {
		t.Fatal("escaping resource handler was accepted")
	}
}

func TestLoadRejectsHandlerThroughEscapingIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	resources := filepath.Join(root, "config", "profiles", "profile", "resources")
	if err := os.MkdirAll(resources, 0o700); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "handler.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(resources, "service")); err != nil {
		t.Fatal(err)
	}
	content := "COMMAND=svc\nHANDLER=resources/service/handler.sh\nTITLE=Service\nACTION=\"up up yard-change reversible\"\nACTION=\"down down yard-change reversible\"\n"
	if err := os.WriteFile(filepath.Join(resources, "service.res"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(root); err == nil {
		t.Fatal("handler escaping through an intermediate symlink was accepted")
	}
}

func TestLoadAllowsHandlerThroughInProfileIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	resources := filepath.Join(root, "config", "profiles", "profile", "resources")
	actualDirectory := filepath.Join(resources, "actual-service")
	if err := os.MkdirAll(actualDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	actualHandler := filepath.Join(actualDirectory, "handler.sh")
	if err := os.WriteFile(actualHandler, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("actual-service", filepath.Join(resources, "service")); err != nil {
		t.Fatal(err)
	}
	content := "COMMAND=svc\nHANDLER=resources/service/handler.sh\nTITLE=Service\nACTION=\"up up yard-change reversible\"\nACTION=\"down down yard-change reversible\"\n"
	if err := os.WriteFile(filepath.Join(resources, "service.res"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	registry, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := registry.Lookup("svc")
	if !ok || definition.HandlerPath() != actualHandler {
		t.Fatalf("resolved handler = %q, found %t; want %q", definition.HandlerPath(), ok, actualHandler)
	}
}

func TestLoadRejectsInvalidFinalHandler(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, handler string) {
				t.Helper()
				actual := filepath.Join(filepath.Dir(handler), "actual.sh")
				if err := os.WriteFile(actual, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("actual.sh", handler); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "directory",
			setup: func(t *testing.T, handler string) {
				t.Helper()
				if err := os.Mkdir(handler, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "non executable",
			setup: func(t *testing.T, handler string) {
				t.Helper()
				if err := os.WriteFile(handler, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			resources := filepath.Join(root, "config", "profiles", "profile", "resources")
			directory := filepath.Join(resources, "service")
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			test.setup(t, filepath.Join(directory, "handler.sh"))
			content := "COMMAND=svc\nHANDLER=resources/service/handler.sh\nTITLE=Service\nACTION=\"up up yard-change reversible\"\nACTION=\"down down yard-change reversible\"\n"
			if err := os.WriteFile(filepath.Join(resources, "service.res"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := Load(root); err == nil {
				t.Fatal("invalid final handler was accepted")
			}
		})
	}
}

func writeTestResource(t *testing.T, root, profile, name, descriptor string) {
	t.Helper()
	directory := filepath.Join(root, "config", "profiles", profile, "resources")
	if err := os.MkdirAll(filepath.Join(directory, name), 0o700); err != nil {
		t.Fatal(err)
	}
	handler := filepath.Join(directory, name, "handler.sh")
	if err := os.WriteFile(handler, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name+".res"), []byte(strings.TrimSpace(descriptor)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testActionRegistry(t *testing.T) (Registry, *domain.ActionRegistry) {
	t.Helper()
	root := t.TempDir()
	writeTestResource(t, root, "sample", "service", `
COMMAND=svc
HANDLER=resources/service/handler.sh
TITLE="Sample service"
ACTION="status status read-only not-needed"
ACTION="up up yard-change reversible"
ACTION="down down yard-change reversible"
ACTION="destroy destroy runtime-destruction recreatable"
ACTION="destroy-purge destroy persistent-data-destruction irreversible"
BRINGUP=up
SHUTDOWN=down
`)
	registry, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := domain.NewActionRegistry(registry.ActionDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	return registry, actions
}
