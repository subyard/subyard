package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
		Code     string                      `json:"code"`
		Message  string                      `json:"message"`
		Action   string                      `json:"action"`
		Blockers []releasetransition.Blocker `json:"blockers"`
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
	// A client or relay that ignores data still shows every blocker and retry.
	if want := "release activation is blocked: release operator-action-required (precondition-blocked): active release-a, target release-b; " +
		"cause: yard default has pending configuration (activation.core.pending-configuration), next: finish the pending change, then run yard -Y default start; " +
		"cause: profile runtime is unavailable (activation.profile.route), next: restore the profile runtime"; fault.Code != "precondition-blocked" || fault.Message != want {
		t.Fatalf("message = %q", fault.Message)
	}
	data := decodeErrorData(t, fault)
	want := []rpc.Refusal{
		{Source: "release-gate", Code: "precondition-blocked", Resource: "activation.core.pending-configuration",
			Text: "yard default has pending configuration", Next: "finish the pending change",
			Action: &rpc.RefusalAction{Verb: "start", Command: "yard -Y default start"}},
		{Source: "release-gate", Code: "dependency-unavailable", Resource: "activation.profile.route",
			Text: "profile runtime is unavailable", Next: "restore the profile runtime"},
	}
	if !reflect.DeepEqual(data.Refusals, want) || data.Incomplete || data.Gate == nil ||
		data.Gate.Code != "precondition-blocked" || data.Gate.Message != outcome.Message || data.Gate.Blockers != nil {
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

// Decisions and the previous release reach no client and are not sent.
func TestMutationGateRPCErrorSendsOnlyRenderedGateFields(t *testing.T) {
	previous := releasetransition.ReleaseID("release-old")
	outcome := &releasetransition.Outcome{Status: releasetransition.StatusOperatorActionRequired,
		Code: releasetransition.CodeResourceConflict, Message: "conflict", Previous: &previous}
	inspection := &releasetransition.Inspection{Outcome: outcome,
		Decisions: []releasetransition.RedactedDecision{{Resource: "r0", Decision: "apply"}}}
	var data struct {
		Gate map[string]json.RawMessage `json:"gate"`
	}
	fault := mutationGateRPCError(outcome, inspection)
	if err := json.Unmarshal(fault.Data, &data); err != nil {
		t.Fatal(err)
	}
	keys := slices.Sorted(maps.Keys(data.Gate))
	if !slices.Equal(keys, []string{"action", "active", "code", "message", "status", "target", "transaction"}) {
		t.Fatalf("gate keys = %q", keys)
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

// A remote command refused by the owner's release gate shows every owner
// refusal and next step, in the owner's order, after the summary message.
func TestRemoteStartShowsEveryOwnerGateRefusalAndNextStep(t *testing.T) {
	fixture := newFixture(t)
	root, environment := fixture.Root, fixture.Environment
	remoteDirectory := filepath.Join(root, "state", "yards", "remote")
	if err := os.MkdirAll(remoteDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(remoteDirectory, "config.env"),
		"YARD_TYPE=remote\nREMOTE_DEST=owner.example\nREMOTE_YARD=inner\nSSH_PORT=4444\n", 0o600)
	outcome := &releasetransition.Outcome{
		Status: releasetransition.StatusMigrationRequired, Code: releasetransition.CodeTransitionRequired,
		Active: "release-a", Target: "release-b", Message: "release migration is required", Retry: "run yard migrate",
	}
	inspection := &releasetransition.Inspection{Outcome: outcome, Blockers: []releasetransition.Blocker{
		{Code: releasetransition.CodeTransitionRequired, Resource: "migration.settings",
			Message: "settings need migration\x1b[31m", Retry: "run yard migrate"},
		{Code: releasetransition.CodePreconditionBlocked, Resource: "activation.core.pending-configuration",
			Message: "yard inner has pending configuration", Retry: "finish the pending change, then run yard -Y inner start"},
	}}
	fault, err := json.Marshal(mutationGateRPCError(outcome, inspection))
	if err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(root, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(fakeBin, "owner-error.json"), string(fault), 0o600)
	writeCLIFile(t, filepath.Join(fakeBin, "owner.py"), `import json,os,struct,sys
root=os.path.dirname(sys.argv[0])
with open(root+'/owner-error.json') as source: fault=json.load(source)
def send(req,key,value):
 data=json.dumps({'version':1,'type':'response','id':req['id'],'operationId':req.get('operationId',''),key:value}).encode()
 sys.stdout.buffer.write(struct.pack('>I',len(data))+data);sys.stdout.buffer.flush()
while True:
 header=sys.stdin.buffer.read(4)
 if not header:break
 req=json.loads(sys.stdin.buffer.read(struct.unpack('>I',header)[0]))
 if req['method']=='rpc.negotiate':send(req,'result',{'capabilities':['operation-exact-plan-v1','operation-steps-v1']})
 elif req['method']=='operation.plan':send(req,'error',fault)
 else:sys.exit(6)
`, 0o600)
	writeCLIFile(t, filepath.Join(fakeBin, "ssh"), "#!/bin/sh\n"+trustedSSHMock(t)+"\nexec python3 '"+filepath.Join(fakeBin, "owner.py")+"' \"$@\"\n", 0o700)
	path := fakeBin + ":" + os.Getenv("PATH")
	t.Setenv("PATH", path)
	var stderr bytes.Buffer
	program := fixture.New(Options{
		Arguments:   []string{"-Y", "remote", "--yes", "start"},
		Environment: append(environment, "PATH="+path), Stderr: &stderr,
	})
	if code := program.Run(context.Background()); code == 0 {
		t.Fatalf("refused start succeeded: %s", stderr.String())
	}
	want := []string{
		"yard: prepare start: transition-required: the owner's release gate refused this command\n",
		"yard: owner release gate: release migration-required (transition-required): active release-a, target release-b\n",
		"yard: owner release gate: release migration is required\n",
		"yard: cause: settings need migration [31m (transition-required, migration.settings)\nyard: Next: run yard migrate\n",
		"yard: cause: yard inner has pending configuration (precondition-blocked, activation.core.pending-configuration)\n" +
			"yard: Next: finish the pending change, then run yard -Y inner start\n",
	}
	output, at := stderr.String(), 0
	for _, line := range want {
		found := strings.Index(output[at:], line)
		if found < 0 {
			t.Fatalf("stderr lacks %q in order:\n%s", line, output)
		}
		at += found + len(line)
	}
	// The gate's own next step is already a blocker's and is not repeated.
	if strings.Contains(output, "\x1b") || strings.Contains(output, "other causes") || strings.Count(output, "Next: run yard migrate") != 1 ||
		strings.Contains(output, "; cause:") {
		t.Fatalf("stderr = %q", output)
	}
}

func TestOwnerRefusalLinesRenderConfigRefusalsAndWhatWasLeftOut(t *testing.T) {
	setting, _ := domain.ConfigSettingName("HOST_LINKS")
	refusals := []error{errors.New("unclassified " + refusalSentinel)}
	for index := 0; index < rpc.MaxErrorRefusals+2; index++ {
		refusals = append(refusals, domain.NewConfigRefusal(domain.ConfigSettingNumeric, setting))
		setting, _ = domain.ConfigSettingName(fmt.Sprintf("SETTING_%d", index))
	}
	fault := operationRPCError(rpc.CodeInvalidParams, domain.CollectRefusals(refusals...))
	lines := ownerRefusalLines(fmt.Errorf("prepare: %w", fault))
	if len(lines) != 2*rpc.MaxErrorRefusals+2 || !strings.HasPrefix(lines[0], "cause: ") || !strings.HasPrefix(lines[1], "Next: ") ||
		lines[len(lines)-2] != "2 more refusals; review the owner's diagnostics" ||
		lines[len(lines)-1] != "other causes are reported only in the owner's diagnostics" ||
		strings.Contains(strings.Join(lines, "\n"), refusalSentinel) {
		t.Fatalf("lines = %q", lines)
	}
	if lines := ownerRefusalLines(&rpc.Error{Code: rpc.CodeInternal, Message: "m"}); lines != nil {
		t.Fatalf("plain error lines = %q", lines)
	}
}

// The owner message of a config refusal repeats its refusals; with valid data
// only a fixed summary and the refusal lines print, and any owner text is
// cleaned and bounded.
func TestOwnerErrorReportPrintsEachRefusalOnceAndCleansOwnerText(t *testing.T) {
	setting, _ := domain.ConfigSettingName("HOST_LINKS")
	fault := operationRPCError(rpc.CodeInvalidParams, domain.NewConfigRefusal(domain.ConfigSettingNumeric, setting))
	refusal := domain.NewConfigRefusal(domain.ConfigSettingNumeric, setting).Error()
	if !strings.Contains(fault.Message, refusal) {
		t.Fatalf("fixture message %q does not repeat the refusal", fault.Message)
	}
	text, lines, ok := ownerErrorReport(fmt.Errorf("prepare config: %w", fault))
	if !ok || text != "prepare config: invalid_params: the owner refused this request" ||
		strings.Count(text+strings.Join(lines, "\n"), refusal) != 1 {
		t.Fatalf("text = %q lines = %q", text, lines)
	}
	raw := &rpc.Error{Code: "plan_failed", Message: "owner said \u202eevil\x1b[2J\n" + strings.Repeat("x", 3000)}
	text, lines, ok = ownerErrorReport(fmt.Errorf("start: %w", raw))
	if !ok || lines != nil || !strings.HasPrefix(text, "start: plan_failed: owner said  evil [2J ") ||
		len(text) != len("start: plan_failed: ")+maxGateMessage || strings.ContainsAny(text, "\u202e\x1b\n") {
		t.Fatalf("text = %q lines = %q", text, lines)
	}
	if _, _, ok := ownerErrorReport(errors.New("local")); ok {
		t.Fatal("a local error was treated as an owner error")
	}
}

// The rendering conveys what the gate JSON used to: the outcome message once,
// each blocker with its code, resource and retry, and the outcome's next step.
func TestOwnerRefusalLinesConveyTheGateMessageBlockersAndRetries(t *testing.T) {
	outcome := &releasetransition.Outcome{
		Status: releasetransition.StatusMigrationRequired, Code: releasetransition.CodeTransitionRequired,
		Active: "release-a", Target: "release-b", Message: "release migration is required", Retry: "run yard migrate",
	}
	want := []string{
		"owner release gate: release migration-required (transition-required): active release-a, target release-b",
		"cause: release migration is required (transition-required)",
		"Next: run yard migrate",
	}
	if lines := ownerRefusalLines(mutationGateRPCError(outcome, nil)); !slices.Equal(lines, want) {
		t.Fatalf("without blockers = %q", lines)
	}
	inspection := &releasetransition.Inspection{Outcome: outcome, Blockers: []releasetransition.Blocker{
		{Code: releasetransition.CodeDependencyUnavailable, Resource: "activation.profile.route",
			Message: "profile runtime is unavailable", Retry: "restore the profile runtime"},
	}}
	want = []string{
		want[0],
		"owner release gate: release migration is required",
		"cause: profile runtime is unavailable (dependency-unavailable, activation.profile.route)",
		"Next: restore the profile runtime",
		"Next: run yard migrate",
	}
	if lines := ownerRefusalLines(mutationGateRPCError(outcome, inspection)); !slices.Equal(lines, want) {
		t.Fatalf("with blockers = %q", lines)
	}
}

// A boundary label in error data is only a validated setting name, never a
// value or path from the request.
func TestConfigNamedRefusalLabelInErrorDataIsOnlyAValidatedName(t *testing.T) {
	setting, _ := domain.ConfigSettingName("HOST_LINKS")
	cause := domain.NewConfigRefusal(domain.ConfigSettingNumeric, setting)
	for name, want := range map[string]string{
		"HOST_LINKS":                        "HOST_LINKS: " + cause.Error(),
		"HOST_LINKS=" + refusalSentinel:     cause.Error(),
		"/home/operator/" + refusalSentinel: cause.Error(),
	} {
		if label := (configNamedRefusal{name, cause}).PublicConfigLabel(); label != "" && label != "HOST_LINKS" {
			t.Fatalf("label for %q = %q", name, label)
		}
		data := decodeErrorData(t, operationRPCError(rpc.CodeInvalidParams, configNamedRefusal{name, cause}))
		if len(data.Refusals) != 1 || data.Refusals[0].Text != want || strings.Contains(string(mustJSON(t, data)), refusalSentinel) {
			t.Fatalf("%q: data = %+v", name, data)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// A client that ignores data, or sits behind a relay that drops it, still sees
// every blocker and the next step in the message; this CLI shows the data
// instead, so its output does not repeat them.
func TestGateRefusalMessageIsSelfContainedWithoutData(t *testing.T) {
	outcome := &releasetransition.Outcome{
		Status: releasetransition.StatusMigrationRequired, Code: releasetransition.CodeTransitionRequired,
		Active: "release-a", Target: "release-b", Message: "release migration is required", Retry: "run yard migrate",
	}
	inspection := &releasetransition.Inspection{Outcome: outcome, Blockers: []releasetransition.Blocker{
		{Code: releasetransition.CodeTransitionRequired, Resource: "migration.settings", Message: "settings need migration"},
		{Code: releasetransition.CodePreconditionBlocked, Resource: "activation.core.pending-configuration",
			Message: "yard inner has pending configuration\x1b[2J", Retry: "finish the pending change, then run yard -Y inner start"},
	}}
	fault := mutationGateRPCError(outcome, inspection)
	want := "transition-required: release migration is required: release migration-required (transition-required): active release-a, target release-b; " +
		"next: run yard migrate; cause: settings need migration (migration.settings); " +
		"cause: yard inner has pending configuration [2J (activation.core.pending-configuration), next: finish the pending change, then run yard -Y inner start"
	// An older CLI prints the error text as is.
	if got := fault.Error(); got != want {
		t.Fatalf("old client text = %q", got)
	}
	stripped := &rpc.Error{Code: fault.Code, Message: fault.Message}
	if text, lines, _ := ownerErrorReport(fmt.Errorf("prepare start: %w", stripped)); text != "prepare start: "+want || lines != nil {
		t.Fatalf("data-less text = %q lines = %q", text, lines)
	}
	text, lines, _ := ownerErrorReport(fmt.Errorf("prepare start: %w", fault))
	if text != "prepare start: transition-required: the owner's release gate refused this command" || len(lines) == 0 ||
		strings.Contains(text+strings.Join(lines, "\n"), "; cause:") {
		t.Fatalf("new client text = %q lines = %q", text, lines)
	}
	long := &releasetransition.Inspection{Outcome: outcome}
	for index := 0; index < 100; index++ {
		long.Blockers = append(long.Blockers, releasetransition.Blocker{Code: releasetransition.CodeTransitionRequired,
			Message: fmt.Sprintf("blocker %d %s", index, strings.Repeat("x", 100))})
	}
	message := mutationGateRPCError(outcome, long).Message
	if len(message) > maxGateMessage || len(message) <= rpc.MaxRefusalText || !strings.HasSuffix(message, "…") ||
		!strings.Contains(message, "; next: run yard migrate; cause: blocker 0 ") {
		t.Fatalf("message is not bounded: %d bytes", len(message))
	}
	// Behind a relay that drops data, this CLI keeps every cause the owner sent.
	stripped = &rpc.Error{Code: fault.Code, Message: message}
	if text, _, _ := ownerErrorReport(stripped); text != "transition-required: "+message {
		t.Fatalf("data-less long text lost causes: %d of %d bytes", len(text), len(message))
	}
}
