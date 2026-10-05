package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestCredentialExactInvocationTransportBoundary(t *testing.T) {
	definition := command.Definition{Name: "keys"}
	for _, args := range [][]string{{"revoke", "id"}, {"--yes", "delete", "id"}, {"resolve", "id", "--choose", "revision"}, {"auto-sync", "pause"}, {"materialize", "--all"}, {"sync", "--all"}, {"rollback", "id", "rev"}, {"trust", "@peer"}, {"untrust", "@peer"}, {"move", "id", "@peer"}} {
		if !credentialExactInvocation(definition, args) {
			t.Fatalf("metadata invocation rejected: %v", args)
		}
	}
	for _, args := range [][]string{{"add", "label"}, {"import", "file"}, {"import", "file", "--dry-run"}, {"rotate", "id"}, {"resolve", "id", "--rotate"}, {"resolve", "id", "--choose", "rev", "--rotate"}, {"status"}, {"auto-sync", "status"}} {
		if credentialExactInvocation(definition, args) {
			t.Fatalf("dedicated invocation accepted: %v", args)
		}
	}
	definition.Arg0 = "_exchange"
	if credentialExactInvocation(definition, []string{"revoke", "id"}) {
		t.Fatal("internal exchange exposed as exact public invocation")
	}
}

func TestPreparedKeysRetainsNativeMetadataAssessment(t *testing.T) {
	input := &credentialInputProbe{reader: strings.NewReader("protected-value")}
	program, loaded, definition, root := credentialCLIFixture(t, input, &testkit.Prompt{}, true)
	writeCLICredentialRecord(t, root, "active")
	prepared := &preparedCommand{CLI: program, Loaded: loaded, Definition: definition, Arguments: []string{"revoke", "cred-0123456789abcdef0123456789abcdef"}}
	if err := prepared.prepareKeys(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(prepared.exactState) != 64 || len(prepared.steps()) != 1 || prepared.refresh != nil || !prepared.executeNoOp {
		t.Fatal("native captured plan not retained")
	}
	action, delta, err := prepared.assess(context.Background())
	if err != nil || action != "keys.revoke" || !delta.Changed {
		t.Fatalf("native assessment mismatch: %s %#v %v", action, delta, err)
	}
	if err := domain.ValidateOperationSteps(prepared.steps()); err != nil {
		t.Fatal(err)
	}
	if input.reads != 0 {
		t.Fatal("metadata preparation read protected input")
	}
}
