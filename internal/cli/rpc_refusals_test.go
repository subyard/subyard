package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const refusalSentinel = "SENTINEL-value"

type errorData struct {
	SchemaVersion int           `json:"schemaVersion"`
	Refusals      []rpc.Refusal `json:"refusals"`
	Omitted       int           `json:"omitted"`
	Incomplete    bool          `json:"incomplete"`
	Gate          *struct {
		Code      string                               `json:"code"`
		Action    string                               `json:"action"`
		Decisions []releasetransition.RedactedDecision `json:"decisions"`
		Blockers  []releasetransition.Blocker          `json:"blockers"`
	} `json:"gate"`
}

func decodeErrorData(t *testing.T, fault *rpc.Error) errorData {
	t.Helper()
	var data errorData
	if err := json.Unmarshal(fault.Data, &data); err != nil || data.SchemaVersion != 1 {
		t.Fatalf("error data %s: %v", fault.Data, err)
	}
	return data
}

func TestOperationRPCErrorCarriesEveryTypedConfigRefusalWithoutValues(t *testing.T) {
	setting, _ := domain.ConfigSettingName("HOST_LINKS")
	profile, _ := domain.ConfigProfileName("route-client")
	cause := domain.CollectRefusals(
		domain.NewConfigRefusal(domain.ConfigRoleForbidsSetting, setting),
		domain.WithRefusalCause(domain.NewConfigRefusal(domain.ConfigProfileRequiresHostAccess, profile),
			errors.New("raw "+refusalSentinel+" /home/operator/private")),
		domain.SettingRefusal(domain.ConfigSettingNumeric, refusalSentinel), // unvalidated name
		errors.New("unclassified "+refusalSentinel),
	)
	fault := operationRPCError(rpc.CodePlanFailed, fmt.Errorf("prepare: %w", cause))
	if fault.Code != rpc.CodePlanFailed {
		t.Fatalf("code = %s", fault.Code)
	}
	data := decodeErrorData(t, fault)
	want := []rpc.Refusal{
		{Source: "config", Code: "profile-requires-host-access",
			Text: domain.NewConfigRefusal(domain.ConfigProfileRequiresHostAccess, profile).Error(),
			Next: domain.NewConfigRefusal(domain.ConfigProfileRequiresHostAccess, profile).Next()},
		{Source: "config", Code: "role-forbids-setting", Text: "selected yard role forbids non-empty HOST_LINKS",
			Next: domain.NewConfigRefusal(domain.ConfigRoleForbidsSetting, setting).Next()},
	}
	if !reflect.DeepEqual(data.Refusals, want) || !data.Incomplete || data.Gate != nil {
		t.Fatalf("data = %+v", data)
	}
	if strings.Contains(string(fault.Data), refusalSentinel) || strings.Contains(string(fault.Data), "/home/") {
		t.Fatalf("data leaked a value: %s", fault.Data)
	}
}

func TestOperationRPCErrorWithoutTypedRefusalsHasNoData(t *testing.T) {
	for _, err := range []error{errors.New("plain " + refusalSentinel), domain.ErrPlanStale} {
		if fault := operationRPCError(rpc.CodePlanFailed, err); fault.Data != nil || fault.Message != err.Error() {
			t.Fatalf("fault = %#v", fault)
		}
	}
}

func TestMutationGateRPCErrorCarriesEveryBlockerAndTypedAction(t *testing.T) {
	outcome := &releasetransition.Outcome{
		Status: releasetransition.StatusOperatorActionRequired, Code: releasetransition.CodePreconditionBlocked,
		Active: "release-a", Target: "release-b", Message: "release activation is blocked",
	}
	inspection := &releasetransition.Inspection{Outcome: outcome,
		Decisions: []releasetransition.RedactedDecision{{Resource: "core.guest", Decision: "apply"}},
		Blockers: []releasetransition.Blocker{
			{Code: releasetransition.CodePreconditionBlocked, Resource: "activation.core.pending-configuration",
				Message: "yard default has pending configuration", Retry: "finish the pending change, then run yard -Y default start"},
			{Code: releasetransition.CodeDependencyUnavailable, Resource: "activation.profile.route",
				Message: "profile runtime is unavailable", Retry: "restore the profile runtime"},
			{Code: releasetransition.CodeDependencyUnavailable, Resource: "activation.profile.route",
				Message: "profile runtime is unavailable", Retry: "restore the profile runtime"},
		}}
	fault := mutationGateRPCError(outcome, inspection)
	if fault.Code != "precondition-blocked" || fault.Message != outcome.Message {
		t.Fatalf("fault = %#v", fault)
	}
	data := decodeErrorData(t, fault)
	want := []rpc.Refusal{
		{Source: "release-gate", Code: "precondition-blocked", Resource: "activation.core.pending-configuration",
			Text: "yard default has pending configuration", Next: "finish the pending change",
			Action: &rpc.RefusalAction{Verb: "start", Yard: "default", Command: "yard -Y default start"}},
		{Source: "release-gate", Code: "dependency-unavailable", Resource: "activation.profile.route",
			Text: "profile runtime is unavailable", Next: "restore the profile runtime"},
	}
	if !reflect.DeepEqual(data.Refusals, want) || data.Incomplete || data.Gate == nil ||
		data.Gate.Code != "precondition-blocked" || len(data.Gate.Decisions) != 1 || data.Gate.Blockers != nil {
		t.Fatalf("data = %s", fault.Data)
	}
}

func TestMutationGateRPCErrorWithoutBlockersRefusesWithTheOutcome(t *testing.T) {
	outcome := &releasetransition.Outcome{
		Status: releasetransition.StatusMigrationRequired, Code: releasetransition.CodeTransitionRequired,
		Active: "release-a", Target: "release-b", Message: "release migration is required", Retry: "run yard migrate",
	}
	fault := mutationGateRPCError(outcome, nil)
	data := decodeErrorData(t, fault)
	action := releaseRetryAction(t, data)
	if len(data.Refusals) != 1 || data.Refusals[0].Code != "transition-required" ||
		data.Refusals[0].Text != outcome.Message || data.Gate.Action == "" || action == nil ||
		action.Command != strings.TrimPrefix(data.Gate.Action, "run ") {
		t.Fatalf("data = %s", fault.Data)
	}
}

func releaseRetryAction(t *testing.T, data errorData) *rpc.RefusalAction {
	t.Helper()
	if len(data.Refusals) == 0 {
		t.Fatal("no refusals")
	}
	return data.Refusals[0].Action
}

func TestMutationGateRPCErrorBoundsDecisions(t *testing.T) {
	outcome := &releasetransition.Outcome{Status: releasetransition.StatusOperatorActionRequired,
		Code: releasetransition.CodeResourceConflict, Message: "conflict"}
	inspection := &releasetransition.Inspection{Outcome: outcome}
	for index := 0; index < maxGateDecisions+5; index++ {
		inspection.Decisions = append(inspection.Decisions, releasetransition.RedactedDecision{Resource: fmt.Sprintf("r%d", index), Decision: "apply"})
	}
	data := decodeErrorData(t, mutationGateRPCError(outcome, inspection))
	if len(data.Gate.Decisions) != maxGateDecisions || !data.Incomplete {
		t.Fatalf("decisions = %d incomplete=%v", len(data.Gate.Decisions), data.Incomplete)
	}
}

func TestRPCErrorDataMatchesTheFrameSchemaAtItsBounds(t *testing.T) {
	schema, err := rpcSchemaCompiler(t).Compile(filepath.Join(repositoryRoot(t), "api/yard-rpc/v1/frame.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	long := &releasetransition.Inspection{}
	outcome := &releasetransition.Outcome{Status: releasetransition.StatusOperatorActionRequired,
		Code: releasetransition.CodeVerificationFailed, Message: strings.Repeat("m\n", 600), Retry: "run yard update --check"}
	long.Outcome = outcome
	for index := 0; index < rpc.MaxErrorRefusals+4; index++ {
		long.Blockers = append(long.Blockers, releasetransition.Blocker{Code: releasetransition.CodeVerificationFailed,
			Resource: strings.Repeat("r", 200), Message: fmt.Sprintf("%d %s", index, strings.Repeat("é", 400)), Retry: strings.Repeat("x", 900)})
		long.Decisions = append(long.Decisions, releasetransition.RedactedDecision{Resource: fmt.Sprintf("r%d", index), Decision: "apply"})
	}
	setting, _ := domain.ConfigSettingName("HOST_LINKS")
	for _, fault := range []*rpc.Error{
		mutationGateRPCError(outcome, long),
		mutationGateRPCError(&releasetransition.Outcome{Status: releasetransition.StatusMigrationRequired, Code: releasetransition.CodeTransitionRequired}, nil),
		operationRPCError(rpc.CodeInvalidParams, domain.NewConfigRefusal(domain.ConfigSettingNumeric, setting)),
	} {
		encoded, err := json.Marshal(rpc.Response{Version: 1, Type: "response", ID: "id", Error: fault})
		if err != nil {
			t.Fatal(err)
		}
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); err != nil {
			t.Fatalf("%s: %v", encoded, err)
		}
	}
}

// rpcGatePayload is the public gate outcome of a gate refusal as encoded in its
// error data, or the message of any other error.
func rpcGatePayload(fault *rpc.Error) []byte {
	var data struct {
		Gate json.RawMessage `json:"gate"`
	}
	if json.Unmarshal(fault.Data, &data) == nil && len(data.Gate) != 0 {
		return data.Gate
	}
	return []byte(fault.Message)
}
