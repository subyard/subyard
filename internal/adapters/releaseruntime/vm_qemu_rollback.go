package releaseruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// inspectVMQEMURollback refuses an older reader before it can mutate rollback
// state when a reporting VM already carries the headless owner value.
func (runtime *Runtime) inspectVMQEMURollback(ctx context.Context, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest) ([]string, error) {
	var output boundedResponseBuffer
	metadataErr := runtime.runVerifiedCandidate(ctx, target, request.RuntimeRoot, []string{"--command-effect", "_vm-start-policy"}, nil, &output, "")
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// This hidden command was introduced together with the headless reader.
	if target.commandManifestBound && metadataErr == nil && output.Len() <= maximumCandidateResponseBytes && strings.TrimSpace(output.String()) == "mutate" {
		return nil, nil
	}
	root := runtime.config.RepositoryRoot
	environment := runtime.config.Inputs.environment(runtime.config.Context)
	environment["SUBYARD_CONFIG_HOME"] = request.ConfigHome
	operator := first(environment["SUBYARD_OPERATOR_HOME"], environment["HOME"])
	names, err := config.YardNames(request.ConfigHome)
	if err != nil {
		return nil, errors.New("cannot safely list yards for VM rollback inspection")
	}
	var refusals []string
	for _, name := range names {
		loaded, err := config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: operator, YardName: name, Environment: environment})
		if err != nil {
			refusals = append(refusals, fmt.Sprintf("yard %s: cannot safely read settings for VM rollback inspection", name))
			continue
		}
		if loaded.Context.AccessKind != domain.AccessLocal || loaded.Context.YardKind != domain.YardVM || loaded.Environment["VM_FREE_PAGE_REPORTING"] != "1" {
			continue
		}
		if runtime.config.YardInstance == nil {
			refusals = append(refusals, fmt.Sprintf("yard %s: VM rollback instance reader is unavailable", name))
			continue
		}
		instance, err := runtime.config.YardInstance(ctx, loaded)
		if errors.Is(err, ports.ErrInstanceNotFound) {
			continue
		}
		if err != nil {
			refusals = append(refusals, fmt.Sprintf("yard %s: cannot safely read VM configuration for rollback", name))
			continue
		}
		if instance.Type == domain.YardVM && instance.LocalConfig["raw.qemu.conf"] == domain.VMRawQEMUConfig(true) {
			refusals = append(refusals, fmt.Sprintf("yard %s: the retained release cannot read its headless VM configuration with free page reporting", name))
		}
	}
	return refusals, nil
}

// inspectRollbackCompatibility returns every independent finding in the existing
// public error cause. Only its protocol outcome is bounded; the operation boundary
// renders the complete cause once, including for a new rollback's planning refusal.
func (runtime *Runtime) inspectRollbackCompatibility(ctx context.Context, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest, selection *releaseSelection) error {
	_, err := runtime.rollbackCompatibility(ctx, target, request, selection, false)
	return err
}

// rollbackCompatibility also returns the profile runtime observation when
// an owner is still to be prepared for the rollback; when preparing, such
// an owner is not a refusal.
func (runtime *Runtime) rollbackCompatibility(ctx context.Context, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest, selection *releaseSelection, preparing bool) (*formatObservation, error) {
	links, err := runtime.inspectRuntimeLinks(request.RuntimeRoot)
	if selection != nil && (err != nil || links != selection.links) {
		return nil, staleRetainedSelection(request.RuntimeRoot, request.Target)
	}
	if err != nil {
		return nil, err
	}
	observed := releaseLinksFromRuntimeSnapshot(links)
	if (selection == nil || selection.draft != nil) && request.Direction != releasetransition.DirectionActivatePrevious {
		return nil, nil
	}
	var migrationMessages []string
	var migrationErr error
	if selection != nil && selection.refusal != "" {
		migrationMessages, migrationErr = []string{selection.refusal}, selection.cause
	}
	layout, layoutErr := runtime.inspectConfigLayoutRollback(ctx, target, request)
	messages, vmErr := runtime.inspectVMQEMURollback(ctx, target, request)
	vmRefused := len(messages) != 0
	format, err := runtime.inspectProfileFormatRollback(ctx, target, request, selection)
	if err != nil {
		return nil, err
	}
	var pending *formatObservation
	if len(format.pending()) != 0 {
		pending = &format
	}
	if !preparing {
		for _, item := range format.pending() {
			format.refuse(fmt.Sprintf("yard %s: profile runtime %s must first be prepared for rollback", item.yard, item.owner), rollbackRetry, nil)
		}
	}
	if layout != nil {
		messages = append([]string{layout.Message}, messages...)
	}
	messages = append(append(migrationMessages, messages...), format.messages...)
	formatErr := domain.CollectOrderedRefusals(format.causes...)
	if len(messages) == 0 {
		return pending, domain.CollectRefusals(migrationErr, layoutErr, vmErr, formatErr)
	}
	formatOnly := len(messages) == len(format.messages)
	var outcome releasetransition.Outcome
	if layout != nil {
		outcome = *layout
	} else {
		outcome = publicReleaseOutcome(observed, request.Target, nil, releasetransition.CodeRollbackIncompatible,
			"", "retain a release supporting headless VMs with free page reporting, then run yard update --rollback")
	}
	if len(migrationMessages) != 0 {
		outcome.Retry = retainedRegistryRetry(selection)
	}
	if layout != nil && vmRefused && len(migrationMessages) == 0 {
		outcome.Retry = "retain a release supporting the installed configuration and reporting VM values, then run yard update --rollback"
	}
	if formatOnly {
		outcome.Retry = format.retry
	}
	outcome.Message = strings.Join(messages, "; ")
	complete := outcome.Message
	if len(complete) > 512 {
		reason := "rollback requires headless reporting VM reader support"
		if layout != nil {
			reason = "rollback requires Git configuration cache and headless reporting VM reader support"
		}
		if len(migrationMessages) != 0 {
			reason = "rollback requires matching migration registries and compatible installed state"
		}
		if formatOnly {
			reason = "rollback requires prepared profile runtimes in every local yard"
		}
		command := "yard update --version " + target.version
		if request.Direction == releasetransition.DirectionActivatePrevious {
			command = "yard update --rollback"
		}
		outcome.Message = fmt.Sprintf("%s; %d findings omitted; full details: stderr of %s", reason, len(messages), command)
	}
	cause := transitionOutcomeError(outcome)
	if complete != outcome.Message {
		cause = fmt.Errorf("%w; %s", cause, complete)
	}
	return nil, publicReleaseInspectionError{cause: domain.CollectRefusals(cause, migrationErr, layoutErr, vmErr, formatErr), outcome: outcome}
}
