package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestPlanRoutesAndConfirmsMutation(t *testing.T) {
	clock := testkit.NewManualClock(time.Unix(100, 0))
	prompt := &testkit.Prompt{Answers: []bool{true}}
	orchestrator := &Orchestrator{Clock: clock, IDs: &testkit.IDs{Values: []string{"operation-1"}}, Prompt: prompt}
	plan, err := orchestrator.Plan(context.Background(), domain.Context{AccessKind: domain.AccessRemote}, domain.CommandPolicy{
		Name: "start", Effect: domain.CommandMutate, Confirmation: domain.ConfirmationPromptDefaultYes,
		RemotePolicy: domain.RemoteOnOwner,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Target != domain.TargetRemoteOwner || !plan.Confirmed || len(prompt.Seen) != 1 {
		t.Fatalf("unexpected plan: %#v", plan)
	}
}

func TestPlanDeclineAndRemoteDeny(t *testing.T) {
	orchestrator := &Orchestrator{
		Clock: testkit.NewManualClock(time.Unix(100, 0)), IDs: &testkit.IDs{Values: []string{"unused"}},
		Prompt: &testkit.Prompt{Answers: []bool{false}},
	}
	_, err := orchestrator.Plan(context.Background(), domain.Context{AccessKind: domain.AccessLocal}, domain.CommandPolicy{
		Name: "stop", Effect: domain.CommandMutate, Confirmation: domain.ConfirmationPromptDefaultYes,
		RemotePolicy: domain.RemoteOnOwner,
	}, false)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("expected decline, got %v", err)
	}
	_, err = orchestrator.Plan(context.Background(), domain.Context{AccessKind: domain.AccessRemote}, domain.CommandPolicy{
		Name: "bind", Effect: domain.CommandMutate, Confirmation: domain.ConfirmationPromptDefaultYes,
		RemotePolicy: domain.RemoteDenied,
	}, true)
	if err == nil {
		t.Fatal("remote bind was planned")
	}
}

func TestPlanRejectsInvalidRemotePolicyForLocalYard(t *testing.T) {
	orchestrator := &Orchestrator{
		Clock: testkit.NewManualClock(time.Unix(100, 0)), IDs: &testkit.IDs{Values: []string{"unused"}},
	}
	_, err := orchestrator.Plan(context.Background(), domain.Context{AccessKind: domain.AccessLocal}, domain.CommandPolicy{
		Name: "status", Effect: domain.CommandRead, Confirmation: domain.ConfirmationNever,
		RemotePolicy: "unknown",
	}, false)
	if err == nil || !strings.Contains(err.Error(), "invalid remote command policy") {
		t.Fatalf("invalid policy was accepted: %v", err)
	}
}

func TestRunAdapterCorrelatesAuditAndEvents(t *testing.T) {
	clock := testkit.NewManualClock(time.Unix(100, 0))
	audit := &testkit.Audit{}
	events := &testkit.Events{}
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "operation-1", Status: "ok",
	}}}}
	orchestrator := &Orchestrator{Clock: clock, Runner: runner, Audit: audit, Events: events}
	plan := domain.OperationPlan{
		OperationID: "operation-1", Command: "fixture", Effect: domain.CommandMutate,
		Confirmation: domain.ConfirmationPromptDefaultYes,
	}
	plan, err := orchestrator.Confirm(context.Background(), plan, true)
	if err != nil {
		t.Fatal(err)
	}
	request := domain.AdapterRequest{Schema: 1, OperationID: "operation-1", Adapter: "fixture", Action: "run"}
	if _, _, err := orchestrator.RunAdapter(context.Background(), plan, request, strings.NewReader("protected")); err != nil {
		t.Fatal(err)
	}
	if len(audit.Events) != 2 || len(events.Events) != 2 || audit.Events[0].OperationID != "operation-1" {
		t.Fatalf("events were not correlated: %#v %#v", audit.Events, events.Events)
	}
}

func TestPrepareKeepsMutationUnconfirmedUntilExplicitConfirmation(t *testing.T) {
	orchestrator := &Orchestrator{
		Clock: testkit.NewManualClock(time.Unix(100, 0)), IDs: &testkit.IDs{Values: []string{"operation-1"}},
	}
	plan, err := orchestrator.Prepare(domain.Context{AccessKind: domain.AccessLocal}, domain.CommandPolicy{
		Name: "start", Effect: domain.CommandMutate, Confirmation: domain.ConfirmationPromptDefaultYes,
		RemotePolicy: domain.RemoteOnOwner,
		Consequences: []string{"start the fixture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Confirmed || plan.Effect != domain.CommandMutate || len(plan.Consequences) != 1 {
		t.Fatalf("unexpected prepared plan: %#v", plan)
	}
	confirmed, err := orchestrator.Confirm(context.Background(), plan, true)
	if err != nil || !confirmed.Confirmed {
		t.Fatalf("explicit confirmation failed: plan=%#v err=%v", confirmed, err)
	}
	request := domain.AdapterRequest{Schema: 1, OperationID: plan.OperationID, Adapter: "fixture", Action: "run"}
	if _, _, err := orchestrator.RunAdapter(context.Background(), plan, request, nil); err == nil {
		t.Fatal("unconfirmed plan reached the adapter")
	}
}

func TestPlanSkipsPromptOnlyForExplicitNeverPolicy(t *testing.T) {
	prompt := &testkit.Prompt{}
	orchestrator := &Orchestrator{
		Clock: testkit.NewManualClock(time.Unix(100, 0)),
		IDs:   &testkit.IDs{Values: []string{"operation-launch"}}, Prompt: prompt,
	}
	plan, err := orchestrator.Plan(context.Background(), domain.Context{AccessKind: domain.AccessLocal}, domain.CommandPolicy{
		Name: "code", Effect: domain.CommandMutate, Confirmation: domain.ConfirmationNever,
		RemotePolicy: domain.RemoteOnController,
	}, false)
	if err != nil || !plan.Confirmed || len(prompt.Seen) != 0 {
		t.Fatalf("prompt-free plan = %#v, prompts=%#v, err=%v", plan, prompt.Seen, err)
	}
	if _, err := orchestrator.Prepare(domain.Context{AccessKind: domain.AccessLocal}, domain.CommandPolicy{
		Name: "missing", Effect: domain.CommandMutate, RemotePolicy: domain.RemoteOnOwner,
	}); err == nil {
		t.Fatal("missing confirmation policy was accepted")
	}
	if _, err := orchestrator.Prepare(domain.Context{AccessKind: domain.AccessLocal}, domain.CommandPolicy{
		Name: "unknown", Effect: domain.CommandMutate, Confirmation: "unknown", RemotePolicy: domain.RemoteOnOwner,
	}); err == nil {
		t.Fatal("unknown confirmation policy was accepted")
	}
}

func TestPrepareRejectsLegacyRequiredConfirmationPolicy(t *testing.T) {
	orchestrator := &Orchestrator{
		Clock: testkit.NewManualClock(time.Unix(100, 0)),
		IDs:   &testkit.IDs{Values: []string{"operation-legacy-policy"}},
	}
	_, err := orchestrator.Prepare(domain.Context{AccessKind: domain.AccessLocal}, domain.CommandPolicy{
		Name: "legacy mutation", Effect: domain.CommandMutate,
		Confirmation: domain.ConfirmationPolicy("required"), RemotePolicy: domain.RemoteOnOwner,
	})
	if err == nil {
		t.Fatal("legacy required confirmation reached a concrete operation plan")
	}
}

func TestPlanMapsConfirmationPoliciesToTypedRequests(t *testing.T) {
	for _, test := range []struct {
		name         string
		policy       domain.ConfirmationPolicy
		defaultValue domain.ConfirmationDefault
	}{
		{name: "default yes", policy: domain.ConfirmationPromptDefaultYes, defaultValue: domain.ConfirmationDefaultYes},
		{name: "default no", policy: domain.ConfirmationPromptDefaultNo, defaultValue: domain.ConfirmationDefaultNo},
	} {
		t.Run(test.name, func(t *testing.T) {
			prompt := &testkit.Prompt{Answers: []bool{true}}
			orchestrator := &Orchestrator{
				Clock: testkit.NewManualClock(time.Unix(100, 0)),
				IDs:   &testkit.IDs{Values: []string{"operation-typed-prompt"}}, Prompt: prompt,
			}
			plan, err := orchestrator.Plan(context.Background(), domain.Context{AccessKind: domain.AccessLocal}, domain.CommandPolicy{
				Name: "Update configuration", Effect: domain.CommandMutate, Confirmation: test.policy,
				RemotePolicy: domain.RemoteOnOwner, Consequences: []string{"replace local settings"},
			}, false)
			if err != nil || !plan.Confirmed || len(prompt.Requests) != 1 {
				t.Fatalf("plan=%#v requests=%#v err=%v", plan, prompt.Requests, err)
			}
			request := prompt.Requests[0]
			if request.Summary != "Update configuration" || request.Default != test.defaultValue ||
				len(request.Consequences) != 1 || request.Consequences[0] != "replace local settings" {
				t.Fatalf("request=%#v", request)
			}
		})
	}
}

func TestConfirmDeclineAndNonInteractiveErrorsPreventAdapterExecution(t *testing.T) {
	for _, test := range []struct {
		name   string
		prompt *testkit.Prompt
		want   error
	}{
		{name: "decline", prompt: &testkit.Prompt{Answers: []bool{false}}, want: domain.ErrOperationDeclined},
		{name: "non-interactive", prompt: &testkit.Prompt{Err: domain.ErrConfirmationRequired}, want: domain.ErrConfirmationRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &testkit.ScriptedAdapter{}
			orchestrator := &Orchestrator{
				Clock: testkit.NewManualClock(time.Unix(100, 0)),
				IDs:   &testkit.IDs{Values: []string{"operation-blocked"}}, Prompt: test.prompt, Runner: runner,
			}
			plan, err := orchestrator.Prepare(domain.Context{AccessKind: domain.AccessLocal}, domain.CommandPolicy{
				Name: "Update configuration", Effect: domain.CommandMutate,
				Confirmation: domain.ConfirmationPromptDefaultYes, RemotePolicy: domain.RemoteOnOwner,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = orchestrator.Confirm(context.Background(), plan, false)
			if !errors.Is(err, test.want) || domain.ConfirmationErrorClass(err) == "" {
				t.Fatalf("confirm error=%v class=%q", err, domain.ConfirmationErrorClass(err))
			}
			_, _, err = orchestrator.RunAdapter(context.Background(), plan, domain.AdapterRequest{
				Schema: 1, OperationID: plan.OperationID, Adapter: "fixture", Action: "run",
			}, nil)
			if err == nil || len(runner.Requests) != 0 {
				t.Fatalf("unconfirmed plan reached adapter: err=%v requests=%#v", err, runner.Requests)
			}
		})
	}
}

func TestGenericPlanAuthorityAndOwnerProjectionStayBound(t *testing.T) {
	ctx := context.Background()
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{Status: "ok"}}, {Result: domain.AdapterResult{Status: "ok"}}}}
	orchestrator := &Orchestrator{Clock: testkit.NewManualClock(time.Unix(100, 0)), Runner: runner}
	plan := domain.OperationPlan{OperationID: "generic", Command: "fixture", Effect: domain.CommandMutate,
		Confirmation: domain.ConfirmationPromptDefaultYes, Target: domain.TargetLocalOwner, Consequences: []string{"bounded effect"},
		Steps: []domain.OperationStep{{ID: "step", Target: "target", Observed: "old", Desired: "new", Decision: domain.StepApply, Verify: "check", Preconditions: []string{"owner"}}},
	}
	request := domain.AdapterRequest{OperationID: plan.OperationID}
	forged := plan.Clone()
	forged.Confirmed = true
	if _, err := orchestrator.Confirm(ctx, forged, true); !errors.Is(err, domain.ErrActionPolicyInvalid) {
		t.Fatalf("forged confirmation = %v", err)
	}
	if _, _, err := orchestrator.RunAdapter(ctx, forged, request, nil); !errors.Is(err, domain.ErrActionPolicyInvalid) {
		t.Fatalf("forged adapter = %v", err)
	}
	// Owner-resolved metadata may differ from the controller's registry.
	plan.Assessment = &domain.ActionAssessment{Action: "owner.action", Effect: domain.ActionMutation, Changed: true}
	plan.ConfirmationRequest = &domain.ConfirmationRequest{Summary: "owner action", Default: domain.ConfirmationDefaultYes, Consequences: []string{"bounded effect"}}
	approved, err := orchestrator.ConfirmResolved(ctx, plan, true)
	if err != nil {
		t.Fatal(err)
	}
	projection := approved.Clone()
	projection.Assessment = nil
	projection.ConfirmationRequest = nil
	for _, mutate := range []func(*domain.OperationPlan){
		func(p *domain.OperationPlan) { p.Target = domain.TargetRemoteOwner },
		func(p *domain.OperationPlan) { p.Confirmation = domain.ConfirmationNever },
		func(p *domain.OperationPlan) { p.Steps[0].Preconditions[0] = "foreign owner" },
		func(p *domain.OperationPlan) { p.Consequences[0] = "expanded effect" },
	} {
		changed := projection.Clone()
		mutate(&changed)
		if _, _, err := orchestrator.RunAdapter(ctx, changed, request, nil); !errors.Is(err, domain.ErrActionPolicyInvalid) {
			t.Fatalf("changed projection = %v", err)
		}
	}
	// Multiple adapter leaves share a single command approval.
	for range 2 {
		if _, _, err := orchestrator.RunAdapter(ctx, projection, request, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.Requests) != 2 {
		t.Fatalf("adapter calls = %d", len(runner.Requests))
	}
	approved.Steps[0].Preconditions[0] = "mutated returned snapshot"
	if _, _, err := orchestrator.RunAdapter(ctx, approved, request, nil); err == nil {
		t.Fatal("approval alias changed authorization")
	}
}

func TestResolvedOwnerConfirmationRejectsContradictoryPromptPolicy(t *testing.T) {
	for _, mismatch := range []string{"default", "consequences"} {
		t.Run(mismatch, func(t *testing.T) {
			prompt := &testkit.Prompt{Answers: []bool{true}}
			orchestrator := &Orchestrator{Prompt: prompt}
			plan := domain.OperationPlan{OperationID: "owner-confirmation", Command: "fixture", Effect: domain.CommandMutate,
				Confirmation: domain.ConfirmationPromptDefaultNo, Target: domain.TargetLocalOwner, Consequences: []string{"bounded"},
				Assessment:          &domain.ActionAssessment{Action: "owner.action", Effect: domain.ActionMutation, Changed: true},
				ConfirmationRequest: &domain.ConfirmationRequest{Summary: "owner action", Default: domain.ConfirmationDefaultNo, Consequences: []string{"bounded"}},
			}
			invalid := plan.Clone()
			if mismatch == "default" {
				invalid.ConfirmationRequest.Default = domain.ConfirmationDefaultYes
			} else {
				invalid.ConfirmationRequest.Consequences[0] = "different effect"
			}
			if _, err := orchestrator.ConfirmResolved(context.Background(), invalid, false); !errors.Is(err, domain.ErrActionPolicyInvalid) || len(prompt.Seen) != 0 {
				t.Fatalf("contradictory policy: err=%v prompts=%d", err, len(prompt.Seen))
			}
			if _, err := orchestrator.ConfirmResolved(context.Background(), plan, false); err != nil || len(prompt.Seen) != 1 || prompt.Requests[0].Default != domain.ConfirmationDefaultNo {
				t.Fatalf("resolved consent: err=%v requests=%#v", err, prompt.Requests)
			}
		})
	}
}
