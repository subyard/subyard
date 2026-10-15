package releasetransition

import "testing"

func TestRuntimeCapabilitiesAreBoundedInputFreeAndSeparateFromFrozenResponses(t *testing.T) {
	request := RecoveryProcessRequest{SchemaVersion: ProcessRecoverySchemaV2, Mode: RecoveryProcessRuntimeCapabilities}
	encoded, err := MarshalRecoveryProcessRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRecoveryProcessRequest(encoded); err != nil {
		t.Fatal(err)
	}
	request.ConfigHome = "/private/config"
	if _, err := MarshalRecoveryProcessRequest(request); err == nil {
		t.Fatal("runtime probe accepted transition inputs")
	}
	response := RecoveryProcessResponse{SchemaVersion: ProcessRecoverySchemaV2, ActivationOwners: []string{"core-guest-runtime", "synthetic-runtime"}}
	encoded, err = MarshalRecoveryProcessResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRecoveryProcessResponse(encoded); err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][]string{{"duplicate", "duplicate"}, {"z", "a"}, {"unsafe/id"}, make([]string, 129)} {
		response.ActivationOwners = ids
		if _, err := MarshalRecoveryProcessResponse(response); err == nil {
			t.Fatalf("invalid runtime capability IDs accepted: %v", ids)
		}
	}
	response.ActivationOwners = []string{"synthetic-runtime"}
	capabilities := ActivationOnlyRecoveryCapabilities()
	response.Capabilities = &capabilities
	if _, err := MarshalRecoveryProcessResponse(response); err == nil {
		t.Fatal("runtime probe mixed frozen capability results")
	}
}
