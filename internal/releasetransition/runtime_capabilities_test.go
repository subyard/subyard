package releasetransition

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRuntimeCapabilitiesAreBoundedInputFreeAndSeparateFromFrozenResponses(t *testing.T) {
	request := RecoveryProcessRequest{SchemaVersion: ProcessRecoverySchemaV2, Mode: RecoveryProcessRuntimeCapabilities}
	encoded, err := MarshalRecoveryProcessRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := ParseRecoveryProcessRequest(encoded); err != nil || !reflect.DeepEqual(decoded, request) {
		t.Fatalf("runtime probe round trip = %#v, %v", decoded, err)
	}
	// The released V1 ingress strictly decodes fields before validating schema
	// and mode. Use its plain request shape without the newer custom codec.
	type frozenRequest ProcessRequest
	var legacy frozenRequest
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&legacy); err == nil || !strings.Contains(err.Error(), `unknown field "contract"`) {
		t.Fatalf("frozen V1 ingress did not reject the probe before execution: %v", err)
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
