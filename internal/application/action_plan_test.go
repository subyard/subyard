package application

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestPrepareActionStoresAnIndependentJSONRoundTrippableAssessmentAndRequest(t *testing.T) {
	registry, err := NewCoreActionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewManualClock(time.Unix(100, 0))
	orchestrator := &Orchestrator{
		Clock: clock, IDs: &testkit.IDs{Values: []string{"operation-remote-add"}}, Actions: registry,
	}
	delta := domain.ActionDelta{Changed: true, Consequences: []string{"register demo on owner", "record SHA256:new"}}
	plan, err := orchestrator.PrepareAction(
		domain.Context{YardType: domain.YardLocal}, "remote", domain.RemoteOnController, "remote.add", delta,
	)
	if err != nil {
		t.Fatal(err)
	}
	delta.Consequences[0] = "tampered caller input"
	if plan.Command != "remote" || plan.Effect != domain.CommandMutate ||
		plan.Confirmation != domain.ConfirmationPromptDefaultYes || plan.Confirmed ||
		plan.Assessment == nil || plan.ConfirmationRequest == nil ||
		plan.Assessment.Consequences[0] != "register demo on owner" ||
		plan.ConfirmationRequest.Summary != "Register remote yard" ||
		plan.ConfirmationRequest.Default != domain.ConfirmationDefaultYes ||
		!reflect.DeepEqual(plan.ConfirmationRequest.Consequences, plan.Assessment.Consequences) {
		t.Fatalf("plan=%#v", plan)
	}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Assessment          map[string]json.RawMessage `json:"assessment"`
		ConfirmationRequest map[string]json.RawMessage `json:"confirmationRequest"`
	}
	if err := json.Unmarshal(payload, &document); err != nil ||
		document.Assessment["action"] == nil || document.Assessment["consequences"] == nil ||
		document.ConfirmationRequest["summary"] == nil || document.ConfirmationRequest["default"] == nil {
		t.Fatalf("serialized action plan=%s err=%v", payload, err)
	}
	var roundTrip domain.OperationPlan
	if err := json.Unmarshal(payload, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip.Assessment, plan.Assessment) ||
		!reflect.DeepEqual(roundTrip.ConfirmationRequest, plan.ConfirmationRequest) {
		t.Fatalf("round trip plan=%#v", roundTrip)
	}
}

func TestConfirmActionPlanUsesStoredRequestAndRejectsTamperingBeforeAssumeYes(t *testing.T) {
	registry, err := NewCoreActionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	prompt := &testkit.Prompt{Answers: []bool{true}}
	orchestrator := &Orchestrator{
		Clock:  testkit.NewManualClock(time.Unix(100, 0)),
		IDs:    &testkit.IDs{Values: []string{"operation-prompt", "operation-assessment", "operation-policy", "operation-request"}},
		Prompt: prompt, Actions: registry,
	}
	prepare := func() domain.OperationPlan {
		plan, err := orchestrator.PrepareAction(
			domain.Context{YardType: domain.YardLocal}, "remote", domain.RemoteOnController,
			"remote.add", domain.ActionDelta{Changed: true, Consequences: []string{"register demo on owner"}},
		)
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}

	confirmed, err := orchestrator.Confirm(context.Background(), prepare(), false)
	if err != nil || !confirmed.Confirmed || len(prompt.Requests) != 1 ||
		prompt.Requests[0].Summary != "Register remote yard" ||
		prompt.Requests[0].Default != domain.ConfirmationDefaultYes {
		t.Fatalf("confirmed=%#v requests=%#v err=%v", confirmed, prompt.Requests, err)
	}

	for name, tamper := range map[string]func(*domain.OperationPlan){
		"assessment": func(plan *domain.OperationPlan) {
			plan.Assessment.Impacts = []domain.ActionImpact{domain.ImpactPersistentData}
		},
		"policy": func(plan *domain.OperationPlan) {
			plan.Confirmation = domain.ConfirmationNever
		},
		"request": func(plan *domain.OperationPlan) {
			plan.ConfirmationRequest.Default = domain.ConfirmationDefaultNo
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := prepare()
			tamper(&plan)
			_, err := orchestrator.Confirm(context.Background(), plan, true)
			if !errors.Is(err, domain.ErrActionPolicyInvalid) ||
				domain.ActionPolicyErrorClass(err) != domain.ActionPolicyInvalid {
				t.Fatalf("tampered plan err=%v class=%q", err, domain.ActionPolicyErrorClass(err))
			}
		})
	}
}

func TestPlanActionReadIsConfirmedWithoutAPrompt(t *testing.T) {
	registry, err := NewCoreActionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	prompt := &testkit.Prompt{}
	orchestrator := &Orchestrator{
		Clock:  testkit.NewManualClock(time.Unix(100, 0)),
		IDs:    &testkit.IDs{Values: []string{"operation-remote-list"}},
		Prompt: prompt, Actions: registry,
	}
	plan, err := orchestrator.PlanAction(
		context.Background(), domain.Context{YardType: domain.YardLocal}, "remote", domain.RemoteOnController,
		"remote.list", domain.ActionDelta{}, false,
	)
	if err != nil || !plan.Confirmed || plan.Confirmation != domain.ConfirmationNever ||
		plan.Assessment == nil || plan.ConfirmationRequest != nil || len(prompt.Requests) != 0 {
		t.Fatalf("plan=%#v prompts=%#v err=%v", plan, prompt.Requests, err)
	}
}

func TestConfirmRejectsForgedPreconfirmedActionPlanBeforeAssumeYes(t *testing.T) {
	registry, err := NewCoreActionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	prompt := &testkit.Prompt{Answers: []bool{true}}
	orchestrator := &Orchestrator{
		Clock:  testkit.NewManualClock(time.Unix(100, 0)),
		IDs:    &testkit.IDs{Values: []string{"operation-forged-confirmation"}},
		Prompt: prompt, Actions: registry,
	}
	plan, err := orchestrator.PrepareAction(
		domain.Context{YardType: domain.YardLocal}, "remote", domain.RemoteOnController,
		"remote.add", domain.ActionDelta{Changed: true, Consequences: []string{"register demo on owner"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	plan.Confirmed = true
	if _, err := orchestrator.Confirm(context.Background(), plan, true); !errors.Is(err, domain.ErrActionPolicyInvalid) ||
		domain.ActionPolicyErrorClass(err) != domain.ActionPolicyInvalid || len(prompt.Requests) != 0 {
		t.Fatalf("forged confirmation err=%v class=%q prompts=%#v",
			err, domain.ActionPolicyErrorClass(err), prompt.Requests)
	}
}

func TestRunAdapterRequiresOrchestratorAuthorizationForActionPlans(t *testing.T) {
	registry, err := NewCoreActionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	newOrchestrator := func(operationID string) (*Orchestrator, *testkit.ScriptedAdapter) {
		runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
			Schema: 1, OperationID: operationID, Status: "ok",
		}}}}
		return &Orchestrator{
			Clock:  testkit.NewManualClock(time.Unix(100, 0)),
			IDs:    &testkit.IDs{Values: []string{operationID}},
			Runner: runner, Actions: registry,
		}, runner
	}
	prepare := func(t *testing.T, orchestrator *Orchestrator, action domain.ActionID, delta domain.ActionDelta) domain.OperationPlan {
		t.Helper()
		plan, err := orchestrator.PrepareAction(
			domain.Context{YardType: domain.YardLocal}, "remote", domain.RemoteOnController, action, delta,
		)
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}

	t.Run("forged boolean is rejected", func(t *testing.T) {
		orchestrator, runner := newOrchestrator("operation-forged-run")
		plan := prepare(t, orchestrator, "remote.add", domain.ActionDelta{
			Changed: true, Consequences: []string{"register demo on owner"},
		})
		plan.Confirmed = true
		_, _, err := orchestrator.RunAdapter(context.Background(), plan, domain.AdapterRequest{
			Schema: 1, OperationID: plan.OperationID, Adapter: "remote", Action: "add",
		}, nil)
		if !errors.Is(err, domain.ErrActionPolicyInvalid) || len(runner.Requests) != 0 {
			t.Fatalf("forged run err=%v requests=%#v", err, runner.Requests)
		}
	})

	t.Run("successful explicit confirmation authorizes apply", func(t *testing.T) {
		orchestrator, runner := newOrchestrator("operation-authorized-run")
		plan := prepare(t, orchestrator, "remote.add", domain.ActionDelta{
			Changed: true, Consequences: []string{"register demo on owner"},
		})
		plan, err = orchestrator.Confirm(context.Background(), plan, true)
		if err != nil {
			t.Fatal(err)
		}
		result, _, err := orchestrator.RunAdapter(context.Background(), plan, domain.AdapterRequest{
			Schema: 1, OperationID: plan.OperationID, Adapter: "remote", Action: "add",
		}, nil)
		if err != nil || result.Status != "ok" || len(runner.Requests) != 1 {
			t.Fatalf("authorized result=%#v err=%v requests=%#v", result, err, runner.Requests)
		}
	})

	t.Run("never action authorizes without prompt", func(t *testing.T) {
		orchestrator, runner := newOrchestrator("operation-authorized-read")
		prompt := &testkit.Prompt{}
		orchestrator.Prompt = prompt
		plan := prepare(t, orchestrator, "remote.list", domain.ActionDelta{})
		plan, err = orchestrator.Confirm(context.Background(), plan, false)
		if err != nil {
			t.Fatal(err)
		}
		result, _, err := orchestrator.RunAdapter(context.Background(), plan, domain.AdapterRequest{
			Schema: 1, OperationID: plan.OperationID, Adapter: "remote", Action: "list",
		}, nil)
		if err != nil || result.Status != "ok" || len(runner.Requests) != 1 || len(prompt.Requests) != 0 {
			t.Fatalf("read result=%#v err=%v requests=%#v prompts=%#v", result, err, runner.Requests, prompt.Requests)
		}
	})
}
