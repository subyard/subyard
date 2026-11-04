package releasetransition

import (
	"bytes"
	"testing"
)

func TestRollbackSourceCapabilityHasExactSeparateWireContract(t *testing.T) {
	requestBytes := []byte(`{"schemaVersion":2,"mode":"rollback-source-activation-capabilities"}`)
	request, err := ParseRecoveryProcessRequest(requestBytes)
	if err != nil || request.Mode != RecoveryProcessRollbackSourceCapabilities {
		t.Fatal(request, err)
	}
	encoded, err := MarshalRecoveryProcessRequest(request)
	if err != nil || !bytes.Equal(encoded, requestBytes) {
		t.Fatal(string(encoded), err)
	}
	capabilities := RollbackSourceActivationCapabilities()
	encoded, err = MarshalRecoveryProcessResponse(RecoveryProcessResponse{SchemaVersion: 2, Capabilities: &capabilities})
	want := `{"schemaVersion":2,"activationReconciliationOwned":false,"capabilities":{"contract":"rollback-source-activation-v1","processSchema":2,"journalSchema":2,"receiptSchema":0}}`
	if err != nil || string(encoded) != want {
		t.Fatal(string(encoded), err)
	}
	capabilities.ReceiptSchema = 1
	if capabilities.Validate() == nil {
		t.Fatal("another capability tuple was accepted")
	}
	for _, invalid := range []string{
		`{"schemaVersion":2,"mode":"rollback-source-activation-capabilities","direction":"activate-previous"}`,
		`{"schemaVersion":2,"mode":"rollback-source-activation-capabilities","execution":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","authorization":"confirmed"}}`,
	} {
		if _, err := ParseRecoveryProcessRequest([]byte(invalid)); err == nil {
			t.Fatal("probe acquired transition inputs")
		}
	}
}
