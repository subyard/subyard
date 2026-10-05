package cli

import (
	"fmt"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resourceendpoint"
)

func (bootstrap *profileBootstrap) operationSteps() []domain.OperationStep {
	if bootstrap == nil {
		return nil
	}
	steps := []domain.OperationStep{}
	if bootstrap.selectionPath != "" {
		steps = append(steps, domain.OperationStep{ID: "bootstrap.profile", Target: bootstrap.selectionPath,
			Observed: "captured protected profile selection", Desired: "selected profiles [" + bootstrap.profiles + "]", Decision: domain.StepApply,
			Preconditions: []string{"protected selection matches its exact captured baseline"}, Verify: "read published selected profiles and validate protected mode", Consequence: "enable profile " + bootstrap.profile})
	}
	if bootstrap.endpoint != nil {
		observed, decision := "unreserved", domain.StepApply
		host, port, exists, err := resourceendpoint.ReadSaved(bootstrap.request.Directory, bootstrap.request.Yard, bootstrap.request.Resource)
		if err == nil && exists {
			observed = fmt.Sprintf("%s:%d", host, port)
			if host == bootstrap.endpoint.Host && port == bootstrap.endpoint.Port {
				decision = domain.StepSkip
			}
		}
		steps = append(steps, domain.OperationStep{ID: "bootstrap.endpoint", Target: bootstrap.request.Directory + ":" + bootstrap.request.Yard + "/" + bootstrap.request.Resource,
			Observed: observed, Desired: fmt.Sprintf("%s:%d", bootstrap.endpoint.Host, bootstrap.endpoint.Port), Decision: decision,
			Preconditions: []string{"captured configured owner ports and endpoint allocator guard are unchanged"}, Verify: "read saved endpoint and confirm exact owner address and port", Consequence: "reserve the approved owner endpoint"})
	}
	if bootstrap.init != nil {
		for _, step := range bootstrap.init.operationSteps() {
			step.ID = "bootstrap." + step.ID
			for index := range step.DependsOn {
				step.DependsOn[index] = "bootstrap." + step.DependsOn[index]
			}
			if len(steps) > 0 && len(step.DependsOn) == 0 {
				step.DependsOn = []string{steps[len(steps)-1].ID}
			}
			steps = append(steps, step)
		}
	}
	return steps
}

func (bootstrap *profileBootstrap) stateBinding() string {
	if bootstrap == nil {
		return ""
	}
	init := ""
	if bootstrap.init != nil {
		init = bootstrap.init.stateBinding()
	}
	return operationStateDigest(struct {
		Profile, Command, Path, Profiles, Init string
		Before                                 config.PersistentFileSnapshot
		Endpoint                               *resourceendpoint.Plan
		Request                                resourceendpoint.Request
		Context                                domain.Context
		Settings                               map[string]config.SettingTrace
	}{
		bootstrap.profile, bootstrap.command, bootstrap.selectionPath, bootstrap.profiles, init, bootstrap.selection, bootstrap.endpoint, bootstrap.request, bootstrap.initial.Context, bootstrap.initial.Settings})
}
