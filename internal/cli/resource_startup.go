package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

const startupResourcePrepareTimeout = 3 * time.Minute

// A startup assessment is entirely owner-side while the VM is stopped. The
// approved ingress shape is checked again after power changes, then the normal
// resource runner performs guest checks and publishes the same route.
type resourceStartup struct {
	loaded       config.Loaded
	definition   resource.Definition
	consequences []string
	ingress      *resourceIngress
	intent       *resourceStartupIntent
	localAction  string
	assessment   resource.PrepareResult
	scope        string
}

// A nil startup has no pending native resource effects. Legacy assessments
// remain executable through their dedicated path but cannot prove full steps.
func (startup *resourceStartup) stepsComplete() bool {
	return startup == nil || startup.assessment.Schema == resource.PrepareAssessmentSchemaV2
}

func (cli *CLI) prepareResourceStartup(ctx context.Context, loaded config.Loaded) (*resourceStartup, error) {
	definitions := cli.selectedStartupResources(loaded)
	if len(definitions) == 0 {
		return nil, nil
	}
	if len(definitions) > 1 {
		return nil, errors.New("more than one startup resource is selected for this yard")
	}
	definition := definitions[0]
	intent, err := cli.prepareResourceStartupIntent(ctx, loaded, definition, definition.BringUp)
	if err != nil || intent == nil {
		return nil, err
	}
	if intent.before != startupPending {
		return nil, nil
	}
	if definition.Proxy == nil || definition.Proxy.AddressPolicy != resource.ProxyAddressOwnerIPv4UDP {
		return nil, errors.New("startup resource requires public UDP ingress")
	}
	output, err := cli.prepareResourceMode(ctx, loaded, definition, []string{definition.BringUp}, "prepare-start")
	if err != nil {
		return nil, err
	}
	result, err := cli.resources.PrepareResult(cli.coreActions, definition.Command, definition.BringUp, output)
	if err != nil {
		return nil, err
	}
	assessment := result.Assessment
	if !assessment.Changed {
		return nil, errors.New("pending startup resource reported no activation")
	}
	local, ok := localResourceAction(definition, assessment.Action)
	if !ok {
		return nil, errors.New("startup resource returned an unknown action")
	}
	ingress, err := cli.prepareResourceIngress(ctx, loaded, definition, definition.BringUp)
	if err != nil || ingress == nil {
		return nil, err
	}
	assessment = ingress.augment(assessment)
	for index, consequence := range assessment.Consequences {
		assessment.Consequences[index] = strings.Replace(consequence, "update the stopped yard", "briefly restart the running yard after startup if the ACL changes", 1)
	}
	assessment = intent.augment(assessment)
	return &resourceStartup{loaded: loaded, definition: definition, consequences: assessment.Consequences,
		ingress: ingress, intent: intent, localAction: local, assessment: result, scope: operationStateDigest(definitions)}, nil
}

func (startup *resourceStartup) refresh(ctx context.Context, cli *CLI) error {
	if startup == nil {
		return nil
	}
	if operationStateDigest(cli.selectedStartupResources(startup.loaded)) != startup.scope {
		return fmt.Errorf("%w: startup resource selection changed", domain.ErrPlanStale)
	}
	if err := startup.intent.refresh(ctx, cli); err != nil {
		return err
	}
	if err := startup.ingress.refresh(ctx, cli); err != nil {
		return err
	}
	fresh, err := cli.prepareResourceStartup(ctx, startup.loaded)
	if err != nil {
		return err
	}
	if fresh == nil || fresh.localAction != startup.localAction ||
		!slices.Equal(fresh.consequences, startup.consequences) ||
		fresh.ingress.preview.Before.Fingerprint != startup.ingress.preview.Before.Fingerprint ||
		fresh.ingress.preview.After.Fingerprint != startup.ingress.preview.After.Fingerprint ||
		fresh.assessment.Binding != startup.assessment.Binding || !domain.EqualOperationSteps(fresh.assessment.Steps, startup.assessment.Steps) {
		return domain.ErrPlanStale
	}
	return nil
}

func sameStartupIngressEffects(before, after yardnetwork.IngressPreview) bool {
	if before.Target != after.Target || !reflect.DeepEqual(before.Before.Policy, after.Before.Policy) ||
		!reflect.DeepEqual(before.After.Policy, after.After.Policy) ||
		before.After.Changed != after.After.Changed || before.After.Physical != after.After.Physical ||
		len(before.After.Updates) != len(after.After.Updates) {
		return false
	}
	for i, original := range before.After.Updates {
		current := after.After.Updates[i]
		if current.Yard.Yard != original.Yard.Yard || !maps.Equal(current.NIC, original.NIC) ||
			current.Access != original.Access || current.RemoveACL != original.RemoveACL ||
			!reflect.DeepEqual(current.ACL, original.ACL) {
			return false
		}
	}
	return true
}

func (startup *resourceStartup) apply(ctx context.Context, cli *CLI, operationID string) error {
	if startup == nil {
		return nil
	}
	if err := startup.intent.refresh(ctx, cli); err != nil {
		return err
	}
	output, err := cli.prepareResourceModeTimeout(ctx, startup.loaded, startup.definition,
		[]string{startup.definition.BringUp}, "prepare", startupResourcePrepareTimeout)
	if err != nil {
		return err
	}
	prepared, err := cli.resources.PrepareResult(cli.coreActions, startup.definition.Command, startup.definition.BringUp, output)
	if err != nil {
		return err
	}
	assessment := prepared.Assessment
	if len(startup.assessment.Steps) != 0 {
		if err := domain.CheckOperationSteps(startup.assessment.Steps, prepared.Steps); err != nil {
			return err
		}
	}
	local, ok := localResourceAction(startup.definition, assessment.Action)
	if !ok || local != startup.localAction || (!assessment.Changed && prepared.Schema != resource.PrepareAssessmentSchemaV2) {
		return domain.ErrPlanStale
	}
	for _, consequence := range assessment.Consequences {
		if !slices.Contains(startup.consequences, consequence) {
			return fmt.Errorf("%w: startup resource consequences changed", domain.ErrPlanStale)
		}
	}
	ingress, err := cli.prepareResourceIngress(ctx, startup.loaded, startup.definition, startup.definition.BringUp)
	if err != nil {
		return err
	}
	if ingress == nil || !sameStartupIngressEffects(startup.ingress.preview, ingress.preview) {
		return fmt.Errorf("%w: startup ingress effects changed after yard start", domain.ErrPlanStale)
	}
	runner := &resourceApplyRunner{cli: cli, loaded: startup.loaded, definition: startup.definition,
		verb: startup.definition.BringUp, localAction: local, effect: assessment.Effect,
		arguments: []string{startup.definition.BringUp}, consequences: assessment.Consequences,
		ingress: ingress, startupIntent: startup.intent}
	if err := attachApprovedResourceVerification(runner, startup.assessment, prepared); err != nil {
		return err
	}
	result, _, err := runner.Run(ctx, domain.AdapterRequest{Schema: 1, OperationID: operationID,
		Adapter: "resource", Action: local}, nil)
	if err != nil {
		return err
	}
	if result.Status != "ok" {
		return errors.New("startup resource did not converge")
	}
	return nil
}

func (startup *resourceStartup) binding() string {
	if startup == nil {
		return ""
	}
	return operationStateDigest(struct {
		Resource      resource.Definition
		Assessment    resource.PrepareResult
		Ingress       yardnetwork.IngressPreview
		Before, After string
	}{startup.definition, startup.assessment, startup.ingress.preview, startup.intent.before, startup.intent.after})
}

func (startup *resourceStartup) steps() []domain.OperationStep {
	if startup == nil {
		return nil
	}
	steps := domain.CloneOperationSteps(startup.assessment.Steps)
	for index := range steps {
		steps[index].ID = "startup-" + steps[index].ID
		for dep := range steps[index].DependsOn {
			steps[index].DependsOn[dep] = "startup-" + steps[index].DependsOn[dep]
		}
		if steps[index].Decision == domain.StepConditional && len(steps[index].DependsOn) == 0 {
			steps[index].DependsOn = []string{"power"}
		}
	}
	return steps
}

// This captures the original pending set even when no activation is needed.
func (cli *CLI) startupScopeBinding(ctx context.Context, loaded config.Loaded) (string, error) {
	definitions := cli.selectedStartupResources(loaded)
	type selected struct {
		Definition resource.Definition
		Before     string
		After      string
	}
	var values []selected
	for _, definition := range definitions {
		intent, err := cli.prepareResourceStartupIntent(ctx, loaded, definition, definition.BringUp)
		if err != nil {
			return "", err
		}
		before, after := "", ""
		if intent != nil {
			before, after = intent.before, intent.after
		}
		values = append(values, selected{definition, before, after})
	}
	return operationStateDigest(values), nil
}
