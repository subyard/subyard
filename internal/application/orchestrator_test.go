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
		Confirmation: domain.ConfirmationPromptDefaultYes, Confirmed: true,
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
