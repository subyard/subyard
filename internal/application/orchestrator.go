package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

var ErrDeclined = domain.ErrOperationDeclined

type Orchestrator struct {
	Clock                ports.Clock
	IDs                  ports.IDSource
	Prompt               ports.Prompter
	Runner               ports.AdapterRunner
	Audit                ports.AuditSink
	Events               ports.EventSink
	Actions              *domain.ActionRegistry
	mu                   sync.Mutex
	revision             uint64
	actionAuthorizations map[string]domain.OperationPlan
}

func (orchestrator *Orchestrator) Plan(ctx context.Context, yard domain.Context, policy domain.CommandPolicy, assumeYes bool) (domain.OperationPlan, error) {
	plan, err := orchestrator.Prepare(yard, policy)
	if err != nil {
		return domain.OperationPlan{}, err
	}
	return orchestrator.Confirm(ctx, plan, assumeYes)
}

func (orchestrator *Orchestrator) Prepare(yard domain.Context, policy domain.CommandPolicy) (domain.OperationPlan, error) {
	return orchestrator.prepare(yard, policy, nil, nil)
}

func (orchestrator *Orchestrator) PrepareAction(
	yard domain.Context,
	command string,
	remotePolicy domain.RemotePolicy,
	action domain.ActionID,
	delta domain.ActionDelta,
) (domain.OperationPlan, error) {
	if orchestrator.Actions == nil {
		return domain.OperationPlan{}, fmt.Errorf("%w: action registry is required", domain.ErrActionPolicyInvalid)
	}
	assessment, err := orchestrator.Actions.Assess(action, delta)
	if err != nil {
		return domain.OperationPlan{}, err
	}
	confirmation, request, err := orchestrator.Actions.Resolve(assessment)
	if err != nil {
		return domain.OperationPlan{}, err
	}
	policy := domain.CommandPolicy{
		Name: command, Effect: commandEffect(assessment.Effect), Confirmation: confirmationPolicy(confirmation),
		RemotePolicy: remotePolicy, Consequences: slices.Clone(assessment.Consequences),
	}
	return orchestrator.prepare(yard, policy, &assessment, request)
}

func (orchestrator *Orchestrator) PlanAction(
	ctx context.Context,
	yard domain.Context,
	command string,
	remotePolicy domain.RemotePolicy,
	action domain.ActionID,
	delta domain.ActionDelta,
	assumeYes bool,
) (domain.OperationPlan, error) {
	plan, err := orchestrator.PrepareAction(yard, command, remotePolicy, action, delta)
	if err != nil {
		return domain.OperationPlan{}, err
	}
	return orchestrator.Confirm(ctx, plan, assumeYes)
}

func (orchestrator *Orchestrator) prepare(
	yard domain.Context,
	policy domain.CommandPolicy,
	assessment *domain.ActionAssessment,
	request *domain.ConfirmationRequest,
) (domain.OperationPlan, error) {
	if orchestrator.Clock == nil || orchestrator.IDs == nil {
		return domain.OperationPlan{}, errors.New("clock and ID source are required")
	}
	if policy.Name == "" || (policy.Effect != domain.CommandRead && policy.Effect != domain.CommandMutate) ||
		!validConfirmationPolicy(policy.Confirmation) {
		return domain.OperationPlan{}, errors.New("invalid command policy")
	}
	if policy.RemotePolicy != domain.RemoteOnController && policy.RemotePolicy != domain.RemoteOnOwner &&
		policy.RemotePolicy != domain.RemoteDenied {
		return domain.OperationPlan{}, errors.New("invalid remote command policy")
	}
	target, err := Route(yard, policy.RemotePolicy)
	if err != nil {
		return domain.OperationPlan{}, fmt.Errorf("command %q: %w", policy.Name, err)
	}
	operationID := orchestrator.IDs.NewID()
	if !domain.SafeID(operationID) {
		return domain.OperationPlan{}, errors.New("ID source returned an invalid operation ID")
	}
	plan := domain.OperationPlan{
		OperationID: operationID, Command: policy.Name, Effect: policy.Effect,
		Confirmation: policy.Confirmation, Target: target,
		Consequences: append([]string(nil), policy.Consequences...),
		Confirmed:    policy.Confirmation == domain.ConfirmationNever, CreatedAt: orchestrator.Clock.Now().UTC(),
	}
	if assessment != nil {
		copy := assessment.Clone()
		plan.Assessment = &copy
	}
	if request != nil {
		copy := request.Clone()
		plan.ConfirmationRequest = &copy
	}
	return plan, nil
}

func (orchestrator *Orchestrator) Confirm(ctx context.Context, plan domain.OperationPlan, assumeYes bool) (domain.OperationPlan, error) {
	if err := orchestrator.validateActionPlan(plan); err != nil {
		return domain.OperationPlan{}, err
	}
	if !domain.SafeID(plan.OperationID) || plan.Command == "" ||
		(plan.Effect != domain.CommandRead && plan.Effect != domain.CommandMutate) ||
		!validConfirmationPolicy(plan.Confirmation) {
		return domain.OperationPlan{}, errors.New("invalid operation plan")
	}
	if plan.Assessment != nil && plan.Confirmed && plan.Confirmation != domain.ConfirmationNever &&
		!orchestrator.actionPlanAuthorized(plan) {
		return domain.OperationPlan{}, fmt.Errorf("%w: action plan was not confirmed by this orchestrator", domain.ErrActionPolicyInvalid)
	}
	if plan.Confirmation == domain.ConfirmationNever || plan.Confirmed || assumeYes {
		plan.Confirmed = true
		orchestrator.authorizeActionPlan(plan)
		return plan, nil
	}
	if orchestrator.Prompt == nil {
		return domain.OperationPlan{}, errors.New("operation requires a prompt port")
	}
	request := domain.ConfirmationRequest{
		Summary: plan.Command, Consequences: append([]string(nil), plan.Consequences...),
	}
	if plan.Assessment != nil {
		request = plan.ConfirmationRequest.Clone()
	} else {
		defaultValue, ok := confirmationDefault(plan.Confirmation)
		if !ok {
			return domain.OperationPlan{}, errors.New("operation confirmation policy requires no prompt")
		}
		request.Default = defaultValue
	}
	accepted, err := orchestrator.Prompt.Confirm(ctx, request)
	if err != nil {
		return domain.OperationPlan{}, err
	}
	if !accepted {
		return domain.OperationPlan{}, ErrDeclined
	}
	plan.Confirmed = true
	orchestrator.authorizeActionPlan(plan)
	return plan, nil
}

func (orchestrator *Orchestrator) validateActionPlan(plan domain.OperationPlan) error {
	if plan.Assessment == nil {
		if plan.ConfirmationRequest != nil {
			return fmt.Errorf("%w: confirmation request without assessment", domain.ErrActionPolicyInvalid)
		}
		return nil
	}
	if orchestrator.Actions == nil {
		return fmt.Errorf("%w: action registry is required", domain.ErrActionPolicyInvalid)
	}
	confirmation, request, err := orchestrator.Actions.Resolve(*plan.Assessment)
	if err != nil {
		return err
	}
	if plan.Confirmation != confirmationPolicy(confirmation) || plan.Effect != commandEffect(plan.Assessment.Effect) ||
		!slices.Equal(plan.Consequences, plan.Assessment.Consequences) {
		return fmt.Errorf("%w: action plan metadata does not match assessment", domain.ErrActionPolicyInvalid)
	}
	if !equalRequest(plan.ConfirmationRequest, request) {
		return fmt.Errorf("%w: action plan request does not match assessment", domain.ErrActionPolicyInvalid)
	}
	return nil
}

func commandEffect(effect domain.ActionEffect) domain.CommandEffect {
	if effect == domain.ActionRead {
		return domain.CommandRead
	}
	return domain.CommandMutate
}

func confirmationPolicy(policy domain.ActionConfirmationPolicy) domain.ConfirmationPolicy {
	return domain.ConfirmationPolicy(policy)
}

func equalRequest(left, right *domain.ConfirmationRequest) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Summary == right.Summary && left.Default == right.Default &&
		slices.Equal(left.Consequences, right.Consequences)
}

func (orchestrator *Orchestrator) authorizeActionPlan(plan domain.OperationPlan) {
	if plan.Assessment == nil {
		return
	}
	copy := cloneActionPlan(plan)
	orchestrator.mu.Lock()
	defer orchestrator.mu.Unlock()
	if orchestrator.actionAuthorizations == nil {
		orchestrator.actionAuthorizations = make(map[string]domain.OperationPlan)
	}
	orchestrator.actionAuthorizations[plan.OperationID] = copy
}

func (orchestrator *Orchestrator) actionPlanAuthorized(plan domain.OperationPlan) bool {
	orchestrator.mu.Lock()
	defer orchestrator.mu.Unlock()
	authorized, ok := orchestrator.actionAuthorizations[plan.OperationID]
	return ok && equalActionPlan(authorized, plan)
}

func cloneActionPlan(plan domain.OperationPlan) domain.OperationPlan {
	plan.Consequences = slices.Clone(plan.Consequences)
	if plan.Assessment != nil {
		copy := plan.Assessment.Clone()
		plan.Assessment = &copy
	}
	if plan.ConfirmationRequest != nil {
		copy := plan.ConfirmationRequest.Clone()
		plan.ConfirmationRequest = &copy
	}
	return plan
}

func equalActionPlan(left, right domain.OperationPlan) bool {
	return left.OperationID == right.OperationID && left.Command == right.Command &&
		left.Effect == right.Effect && left.Confirmation == right.Confirmation && left.Target == right.Target &&
		slices.Equal(left.Consequences, right.Consequences) && equalAssessment(left.Assessment, right.Assessment) &&
		equalRequest(left.ConfirmationRequest, right.ConfirmationRequest) && left.Confirmed == right.Confirmed &&
		left.CreatedAt.Equal(right.CreatedAt)
}

func equalAssessment(left, right *domain.ActionAssessment) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Action == right.Action && left.Effect == right.Effect && left.Changed == right.Changed &&
		slices.Equal(left.Impacts, right.Impacts) && left.Recovery == right.Recovery &&
		slices.Equal(left.Consequences, right.Consequences)
}

func validConfirmationPolicy(policy domain.ConfirmationPolicy) bool {
	return policy == domain.ConfirmationNever ||
		policy == domain.ConfirmationPromptDefaultYes || policy == domain.ConfirmationPromptDefaultNo
}

func confirmationDefault(policy domain.ConfirmationPolicy) (domain.ConfirmationDefault, bool) {
	switch policy {
	case domain.ConfirmationPromptDefaultNo:
		return domain.ConfirmationDefaultNo, true
	case domain.ConfirmationPromptDefaultYes:
		return domain.ConfirmationDefaultYes, true
	default:
		return "", false
	}
}

func Route(yard domain.Context, policy domain.RemotePolicy) (domain.ExecutionTarget, error) {
	if policy != domain.RemoteOnController && policy != domain.RemoteOnOwner && policy != domain.RemoteDenied {
		return "", errors.New("invalid remote command policy")
	}
	if yard.AccessKind != domain.AccessRemote {
		return domain.TargetLocalOwner, nil
	}
	switch policy {
	case domain.RemoteOnController:
		return domain.TargetLocalController, nil
	case domain.RemoteOnOwner:
		return domain.TargetRemoteOwner, nil
	case domain.RemoteDenied:
		return "", errors.New("host-local command is denied for a remote yard")
	default:
		return "", errors.New("invalid remote command policy")
	}
}

func (orchestrator *Orchestrator) RunAdapter(
	ctx context.Context,
	plan domain.OperationPlan,
	request domain.AdapterRequest,
	protectedInput io.Reader,
) (domain.AdapterResult, string, error) {
	if plan.Assessment != nil {
		if err := orchestrator.validateActionPlan(plan); err != nil {
			return domain.AdapterResult{}, "", err
		}
		if !orchestrator.actionPlanAuthorized(plan) {
			return domain.AdapterResult{}, "", fmt.Errorf("%w: action plan is not authorized", domain.ErrActionPolicyInvalid)
		}
	}
	if orchestrator.Runner == nil {
		return domain.AdapterResult{}, "", errors.New("adapter runner is required")
	}
	if request.OperationID != plan.OperationID {
		return domain.AdapterResult{}, "", errors.New("adapter request does not belong to the operation plan")
	}
	if !plan.Confirmed {
		return domain.AdapterResult{}, "", errors.New("adapter request belongs to an unconfirmed operation plan")
	}
	if err := orchestrator.record(ctx, plan, "operation.started", nil); err != nil {
		return domain.AdapterResult{}, "", err
	}
	result, stderr, runErr := orchestrator.Runner.Run(ctx, request, protectedInput)
	data := map[string]any{"status": result.Status}
	if runErr != nil {
		data["status"] = "error"
		data["error"] = runErr.Error()
	}
	recordErr := orchestrator.record(ctx, plan, "operation.finished", data)
	if runErr != nil {
		return result, stderr, runErr
	}
	if recordErr != nil {
		return result, stderr, recordErr
	}
	return result, stderr, nil
}

func (orchestrator *Orchestrator) record(ctx context.Context, plan domain.OperationPlan, kind string, data map[string]any) error {
	orchestrator.mu.Lock()
	orchestrator.revision++
	revision := orchestrator.revision
	orchestrator.mu.Unlock()
	event := domain.OperationEvent{
		OperationID: plan.OperationID, Sequence: revision, Revision: revision,
		Kind: kind, At: orchestrator.Clock.Now().UTC(), Data: data,
	}
	if orchestrator.Audit != nil {
		if err := orchestrator.Audit.WriteAudit(ctx, event); err != nil {
			return fmt.Errorf("write operation audit: %w", err)
		}
	}
	if orchestrator.Events != nil {
		if err := orchestrator.Events.Publish(ctx, event); err != nil {
			return fmt.Errorf("publish operation event: %w", err)
		}
	}
	return nil
}
