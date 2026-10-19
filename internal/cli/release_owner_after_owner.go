package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

type releaseAfterOwner struct {
	original   releaseowner.Target
	prepared   releaseActivationPrepared
	completion releaseowner.AfterOwnerCompletion
	physical   []byte
}

func activationKey(t releaseowner.Target) string { return t.Owner + ":" + t.Yard + ":" + t.Class }
func (c *releaseOwnerComponents) needsAfterOwner(yard string) bool {
	if c.activationActual || c.ownerChildren == nil || yard != testyardmigration.CurrentYard {
		return false
	}
	switch testyardmigration.State(c.ownerChildren.before.State) {
	case testyardmigration.StateAbsent, testyardmigration.StateCurrent, testyardmigration.StateLegacyFlatAdoptCurrent, testyardmigration.StateLegacyDirectoryAdoptCurrent:
		return false
	}
	child, ok := c.ownerChildren.inits[yard]
	return ok && child.execution != nil
}
func afterOwnerTemplate(kind string, originalBefore ...string) []byte {
	f := strings.Repeat("f", 64)
	before := f
	if kind == "routes" && len(originalBefore) == 1 {
		before = originalBefore[0]
	}
	physical := make([]byte, reconcileruntime.AfterOwnerMaterializedPhysicalBound())
	if kind == "routes" {
		physical = make([]byte, reconcileruntime.AfterOwnerRoutePhysicalBound())
	}
	if kind == "broker" {
		physical = make([]byte, reconcileruntime.AfterOwnerBrokerPhysicalBound())
	}
	if kind == "runtime" {
		before = strings.Repeat("x", 256)
		physical = nil
	}
	return afterOwnerSizedTemplate(before, physical)
}
func afterOwnerSizedTemplate(before string, physical []byte) []byte {
	f := strings.Repeat("f", 64)
	b := releaseowner.Binding{Session: f, Transaction: strings.Repeat("x", 128), Plan: "plan-v1-" + f, Journal: f, Artifact: f, Helper: f, Ordinal: 4096}
	m := releaseowner.AfterOwnerMetadata{Version: 1, Phase: "resolved", Original: f, Producer: b, ProducerTerminal: f, Delta: releaseowner.AfterOwnerDelta{InstanceUUID: strings.Repeat("x", 128), State: "deferred", Actual: f, Desired: f, ReportedActual: f, ReportedDesired: f, Before: before, ScopeState: strings.Repeat("x", 32)}, Physical: physical}
	raw, _ := json.Marshal(m)
	template, _ := json.Marshal(releaseowner.AfterOwnerCapacity{Version: 1, Phase: "template", MetadataBytes: len(raw), DescriptorBytes: len(raw)})
	return template
}
func (c *releaseOwnerComponents) prepareAfterOwnerFiles(ctx context.Context, prepared releaseActivationPrepared) (releaseActivationPrepared, error) {
	runtime, ok := prepared.platform.(reconcileruntime.Runtime)
	if !ok {
		return prepared, domain.ErrPlanStale
	}
	p, err := runtime.PrepareAfterOwnerMaterializedConfigs(ctx, afterOwnerTemplate("materialized-files"))
	if err != nil {
		return prepared, err
	}
	if err = p.ShareSources(c.retainPreparedSource); err != nil {
		return prepared, errors.Join(err, p.Close())
	}
	prepared.nativeInstalled = p
	prepared.value.Source = nativeValueDigest(struct{ Source, Component string }{prepared.value.Source, p.MaterializedSourceBinding()})
	prepared.value.PreparedRecipe = p.Binding()
	prepared.value.AfterOwner = "v1:materialized-files:" + p.Binding()
	if c.installedRecipes == nil {
		c.installedRecipes = map[string]*reconcileruntime.PreparedInstalledRuntime{}
	}
	c.installedRecipes[p.Binding()] = p
	c.closers = append(c.closers, p)
	return prepared, nil
}
func (c *releaseOwnerComponents) bindAfterOwnerRecipe(ctx context.Context, hash string, raw []byte, target releaseowner.Target) error {
	kind, originalHash, err := releaseowner.ParseAfterOwner(target.AfterOwner)
	if err != nil || (kind != "materialized-files" && kind != "runtime" && kind != "broker" && kind != "routes" && kind != "materialized-integrations") || kind == "materialized-files" && target.Class != "materialized-files" || kind == "runtime" && target.Class != "profile-runtime" || kind == "broker" && (target.Owner != "test-vm-broker" || target.Class != "stage") || kind == "routes" && (target.Owner != "test-yard-route-consumers" || target.Class != "route-consumers") || kind == "materialized-integrations" && (target.Owner != "materialized-config" || target.Class != "materialized-integrations") || target.PreparedRecipe != hash || nativeDigest(raw) != hash {
		return domain.ErrPlanStale
	}
	key := activationKey(target)
	prepared, ok := c.activation[key]
	if !ok {
		return domain.ErrPlanStale
	}
	runtime, ok := prepared.platform.(reconcileruntime.Runtime)
	if !ok {
		return domain.ErrPlanStale
	}
	if originalHash == hash {
		template, err := afterOwnerRecipeMetadata(raw)
		if err != nil {
			return err
		}
		expectedTemplate := afterOwnerTemplate(kind, target.Before)
		if kind == "materialized-integrations" {
			source, sourceErr := runtime.BindAfterOwnerIntegrations(ctx, raw, nil)
			if sourceErr != nil {
				return sourceErr
			}
			bound, boundErr := source.AfterOwnerIntegrationPhysicalBound()
			source.Close()
			if boundErr != nil {
				return boundErr
			}
			expectedTemplate = afterOwnerSizedTemplate(strings.Repeat("f", 64), make([]byte, bound))
		}
		if _, err := releaseowner.DecodeAfterOwnerCapacity(template); err != nil || !bytes.Equal(template, expectedTemplate) {
			return domain.ErrPlanStale
		}

		// Original target/body arrives from the immutable Rust bundle. Inert source
		// binding requires no physical guest and cannot dispatch the conditional.
		if prepared.value.Owner != target.Owner || prepared.value.Yard != target.Yard || prepared.value.Class != target.Class || prepared.value.Project != target.Project || prepared.value.Instance != target.Instance {
			return domain.ErrPlanStale
		}
		var p *reconcileruntime.PreparedInstalledRuntime
		if kind == "materialized-integrations" {
			p, err = runtime.BindAfterOwnerIntegrations(ctx, raw, nil)
		} else if kind == "routes" {
			p, err = runtime.BindAfterOwnerRoutes(ctx, raw, nil)
		} else if kind == "broker" {
			p, err = runtime.BindAfterOwnerBroker(ctx, raw, nil)
		} else if kind == "runtime" {
			p, err = runtime.BindAfterOwnerRuntimeSources(ctx, raw)
		} else {
			p, err = runtime.BindAfterOwnerMaterializedConfigSource(ctx, raw)
		}
		if err != nil {
			return err
		}
		prepared.nativeInstalled = p
		prepared.nativeConfigs = nil
		prepared.value = target
		if c.afterOwnerOriginals == nil {
			c.afterOwnerOriginals = map[string]releaseAfterOwner{}
		}
		c.afterOwnerOriginals[originalHash] = releaseAfterOwner{original: target, prepared: prepared}
		c.activation[key] = prepared
		c.closers = append(c.closers, p)
		if c.installedRecipes == nil {
			c.installedRecipes = map[string]*reconcileruntime.PreparedInstalledRuntime{}
		}
		c.installedRecipes[hash] = p
		return nil
	}
	original, ok := c.afterOwnerOriginals[originalHash]
	if !ok {
		return domain.ErrPlanStale
	}
	template, err := afterOwnerRecipeMetadata(original.prepared.nativeInstalled.Recipe())
	if err != nil {
		return err
	}
	capacity, err := releaseowner.DecodeAfterOwnerCapacity(template)
	if err != nil {
		return err
	}
	metadata, err := afterOwnerRecipeMetadata(raw)
	if err != nil {
		return err
	}
	m, err := releaseowner.DecodeAfterOwnerMetadata(metadata)
	if err != nil {
		return err
	}
	if len(metadata) > capacity.MetadataBytes || len(metadata) > capacity.DescriptorBytes {
		return domain.ErrPlanStale
	}
	if m.Original != nativeValueDigest(original.original) || m.Delta.Apply(original.original, hash) != target || releaseowner.AfterOwnerPhysicalTarget(original.original, target) != nil {
		return domain.ErrPlanStale
	}
	if err = c.validateAfterOwnerCompletion(m); err != nil {
		return err
	}
	expected, err := original.prepared.nativeInstalled.WithAfterOwnerMetadata(metadata)
	if err != nil {
		return err
	}
	defer expected.Close()
	if !bytes.Equal(expected.Recipe(), raw) {
		return domain.ErrPlanStale
	}
	var p *reconcileruntime.PreparedInstalledRuntime
	if kind == "materialized-integrations" {
		p, err = runtime.BindAfterOwnerIntegrations(ctx, raw, m.Physical)
	} else if kind == "routes" {
		p, err = runtime.BindAfterOwnerRoutes(ctx, raw, m.Physical)
	} else if kind == "broker" {
		p, err = runtime.BindAfterOwnerBroker(ctx, raw, m.Physical)
	} else if kind == "runtime" {
		if len(m.Physical) != 0 {
			return domain.ErrPlanStale
		}
		p, err = runtime.BindAfterOwnerRuntimeSources(ctx, raw)
	} else {
		p, err = runtime.BindAfterOwnerMaterializedConfigs(ctx, raw, m.Physical)
	}
	if err != nil {
		return err
	}
	if kind == "materialized-files" || kind == "materialized-integrations" {
		configs := p.PreparedConfigs()
		if configs == nil || configs.InstanceUUID() != target.InstanceUUID {
			p.Close()
			return domain.ErrPlanStale
		}
	}
	prepared = original.prepared
	prepared.nativeInstalled = p
	prepared.nativeConfigs = p.PreparedConfigs()
	prepared.broker = p.PreparedBroker()
	prepared.nativeIntegrations = p.PreparedIntegrations()
	if prepared.nativeIntegrations != nil {
		prepared.integration = prepared.nativeIntegrations.Plan()
	}
	prepared.runtime = ports.RuntimeObservation{State: ports.RuntimeState(target.State), Actual: target.ReportedActual, Desired: target.ReportedDesired}
	prepared.value = target
	assessment := configTargetAssessment{Target: prepared.configTarget, State: target.ScopeState, Changed: target.State == "stale", DesiredFingerprint: target.ReportedDesired, MaterializedFingerprint: target.ReportedActual}
	prepared.configs = &configApplyExecution{approved: []configTargetAssessment{assessment}, current: []configTargetAssessment{assessment}, selector: func() ([]configTarget, error) { return []configTarget{prepared.configTarget}, nil }}
	if c.afterOwnerViews == nil {
		c.afterOwnerViews = map[string]releaseAfterOwner{}
	}
	c.afterOwnerViews[key] = releaseAfterOwner{original: original.original, prepared: prepared, completion: releaseowner.AfterOwnerCompletion{Producer: m.Producer, Terminal: m.ProducerTerminal}, physical: bytes.Clone(m.Physical)}
	c.activation[key] = prepared
	c.closers = append(c.closers, p)
	if c.installedRecipes == nil {
		c.installedRecipes = map[string]*reconcileruntime.PreparedInstalledRuntime{}
	}
	c.installedRecipes[hash] = p
	return nil
}
func afterOwnerRecipeMetadata(raw []byte) ([]byte, error) {
	var recipe struct {
		AfterOwner []byte `json:"afterOwner"`
	}
	if json.Unmarshal(raw, &recipe) != nil || len(recipe.AfterOwner) == 0 || len(recipe.AfterOwner) > 32<<20 {
		return nil, domain.ErrPlanStale
	}
	return bytes.Clone(recipe.AfterOwner), nil
}

func (c *releaseOwnerComponents) validateAfterOwnerCompletion(m releaseowner.AfterOwnerMetadata) error {
	// The session authenticates the immutable original terminal. Component state
	// may only reuse that exact explicitly supplied completion, never find latest.
	if c.afterOwnerCompletion == nil || *c.afterOwnerCompletion != (releaseowner.AfterOwnerCompletion{Producer: m.Producer, Terminal: m.ProducerTerminal}) {
		return domain.ErrPlanStale
	}
	return nil
}
func (c *releaseOwnerComponents) ResolveAfterOwner(ctx context.Context, target releaseowner.Target, completed releaseowner.AfterOwnerCompletion) (releaseowner.Target, string, error) {
	kind, hash, err := releaseowner.ParseAfterOwner(target.AfterOwner)
	if err != nil || (kind != "materialized-files" && kind != "runtime" && kind != "broker" && kind != "routes" && kind != "materialized-integrations") {
		return target, "", domain.ErrPlanStale
	}
	original, ok := c.afterOwnerOriginals[hash]
	if !ok || original.original != target || c.ownerChildren == nil || c.ownerResultsRecipe != c.ownerChildren.binding() {
		return target, "", domain.ErrPlanStale
	}
	if err := c.BindAfterOwnerCompletion(completed); err != nil {
		return target, "", err
	}
	physicalTarget, err := c.ResolveOwnerTarget(ctx, target)
	if err != nil {
		return target, "", err
	}
	if physicalTarget.InstanceUUID == "absent" {
		return target, "", domain.ErrPlanStale
	}
	runtime := original.prepared.platform.(reconcileruntime.Runtime)
	var p *reconcileruntime.PreparedInstalledRuntime
	var physical []byte
	if kind == "materialized-integrations" {
		p, physical, err = runtime.ResolveAfterOwnerIntegrations(ctx, original.prepared.nativeInstalled.Recipe())
		if err != nil {
			return target, "", err
		}
		plan, stopped, observeErr := runtime.ObserveAfterOwnerIntegrations(ctx, p)
		if observeErr != nil {
			p.Close()
			return target, "", observeErr
		}
		physicalTarget.State = "current"
		physicalTarget.Desired = nativeValueDigest(struct{ Scope string }{plan.Scope})
		physicalTarget.Actual = physicalTarget.Desired
		if plan.Changed {
			physicalTarget.State = "stale"
			physicalTarget.Actual = plan.Fingerprint
		}
		if stopped {
			physicalTarget.State = "deferred"
		}
		physicalTarget.ReportedActual, physicalTarget.ReportedDesired = plan.Fingerprint, plan.Scope
		physicalTarget.Before = p.PreparedIntegrations().Binding()
	} else if kind == "routes" {
		source, sourceErr := runtime.BindAfterOwnerRoutes(ctx, original.prepared.nativeInstalled.Recipe(), nil)
		if sourceErr != nil {
			return target, "", sourceErr
		}
		defer source.Close()
		native, _ := c.cli.statusPorts()
		reader, ok := native.(testyardmigration.RouteDevicePreparer)
		if !ok {
			return target, "", domain.ErrPlanStale
		}
		var captured *ports.PreparedReadOnlyDisk
		for _, d := range source.PreparedRoutes().Devices {
			if d.Project == target.Project && d.Instance == target.Instance {
				v := d
				captured = &v
			}
		}
		if captured == nil {
			return target, "", domain.ErrPlanStale
		}
		current, readErr := reader.PrepareReadOnlyDisk(ctx, captured.Project, captured.Instance, captured.Device, captured.Source, captured.Destination)
		if readErr != nil || current.UUID != physicalTarget.InstanceUUID {
			return target, "", domain.ErrPlanStale
		}
		p, physical, err = runtime.ResolveAfterOwnerRoutes(ctx, source.Recipe(), current)
		if err != nil {
			return target, "", err
		}
		route, ok := original.prepared.route.(*testYardRouteConsumerActivation)
		if !ok {
			p.Close()
			return target, "", domain.ErrPlanStale
		}
		physicalTarget.State = "stale"
		physicalTarget.Actual = target.Source
		if route.Verify(ctx, original.prepared.before) == nil {
			physicalTarget.State = "current"
			physicalTarget.Actual = physicalTarget.Desired
		}
	} else if kind == "broker" {
		p, physical, err = runtime.ResolveAfterOwnerBroker(ctx, original.prepared.nativeInstalled.Recipe())
		if err != nil {
			return target, "", err
		}
		converged, verifyErr := runtime.VerifyStage(ctx, original.prepared.stage)
		if verifyErr != nil {
			p.Close()
			return target, "", verifyErr
		}
		physicalTarget.State = "current"
		physicalTarget.Actual = physicalTarget.Desired
		if !converged {
			physicalTarget.State = "stale"
			physicalTarget.Actual = nativeValueDigest(struct {
				Desired string
				Drift   bool
			}{physicalTarget.Desired, true})
		}
		physicalTarget.Before = p.PreparedBroker().Binding()
	} else if kind == "runtime" {
		p, err = runtime.BindAfterOwnerRuntimeSources(ctx, original.prepared.nativeInstalled.Recipe())
		if err != nil {
			return target, "", err
		}
		observed, generation, capable, observeErr := runtime.ObservePreparedAfterOwnerRuntime(ctx, p)
		if observeErr != nil {
			_ = p.Close()
			return target, "", observeErr
		}
		physicalTarget.State = string(observed.State)
		physicalTarget.ReportedActual = observed.Actual
		physicalTarget.ReportedDesired = observed.Desired
		physicalTarget.Actual = observed.Actual
		physicalTarget.Desired = observed.Desired
		if physicalTarget.Actual == "" {
			physicalTarget.Actual = nativeValueDigest(struct {
				State string `json:"state"`
			}{physicalTarget.State})
		}
		if physicalTarget.Desired == "" {
			physicalTarget.Desired = target.Desired
		}
		if capable {
			physicalTarget.Before, err = releaseowner.PrepareFenceRecipe(c.context.Session, physicalTarget, generation)
			if err != nil {
				_ = p.Close()
				return target, "", err
			}
		}
	} else {
		p, physical, err = runtime.ResolveAfterOwnerMaterializedConfigs(ctx, original.prepared.nativeInstalled.Recipe())
		if err != nil {
			return target, "", err
		}
		assessment, err := c.cli.assessConfigTarget(ctx, original.prepared.configTarget, true)
		if err != nil {
			return target, "", err
		}
		physicalTarget.State = "current"
		if assessment.Changed {
			physicalTarget.State = "stale"
		}
		if assessment.State == "stopped" {
			physicalTarget.State = "deferred"
		}
		physicalTarget.ScopeState = assessment.State
		physicalTarget.ReportedActual = assessment.MaterializedFingerprint
		physicalTarget.ReportedDesired = assessment.DesiredFingerprint
		physicalTarget.Actual = assessment.MaterializedFingerprint
		physicalTarget.Desired = assessment.DesiredFingerprint
		physicalTarget.Before = p.PreparedConfigs().Binding()
	}
	if kind == "broker" || kind == "routes" {
		instance, readErr := runtime.Incus.Instance(ctx, target.Project, target.Instance)
		if readErr != nil || instance.Config["volatile.uuid"] != physicalTarget.InstanceUUID {
			p.Close()
			return target, "", domain.ErrPlanStale
		}
		if strings.EqualFold(instance.Status, "stopped") {
			physicalTarget.State = "deferred"
		} else if !strings.EqualFold(instance.Status, "running") {
			p.Close()
			return target, "", domain.ErrPlanStale
		}
	}
	defer p.Close()
	delta := releaseowner.AfterOwnerDelta{InstanceUUID: physicalTarget.InstanceUUID, State: physicalTarget.State, Actual: physicalTarget.Actual, Desired: physicalTarget.Desired, ReportedActual: physicalTarget.ReportedActual, ReportedDesired: physicalTarget.ReportedDesired, Before: physicalTarget.Before, ScopeState: physicalTarget.ScopeState}
	metadata, _ := json.Marshal(releaseowner.AfterOwnerMetadata{Version: 1, Phase: "resolved", Original: nativeValueDigest(target), Producer: completed.Producer, ProducerTerminal: completed.Terminal, Delta: delta, Physical: physical})
	derived, err := p.WithAfterOwnerMetadata(metadata)
	if err != nil {
		return target, "", err
	}
	defer derived.Close()
	physicalTarget.PreparedRecipe = derived.Binding()
	raw := derived.Recipe()
	if err = c.bindAfterOwnerRecipe(ctx, derived.Binding(), raw, physicalTarget); err != nil {
		return target, "", err
	}
	if c.installedBound == nil {
		c.installedBound = map[string]bool{}
	}
	c.installedBound[derived.Binding()] = true
	return physicalTarget, derived.Binding(), nil
}

func (c *releaseOwnerComponents) BindAfterOwnerCompletion(completed releaseowner.AfterOwnerCompletion) error {
	if completed.Producer.Session != c.context.Session || completed.Producer.Artifact != c.context.Artifact || c.ownerChildren == nil || c.ownerResultsRecipe != c.ownerChildren.binding() {
		return domain.ErrPlanStale
	}
	c.afterOwnerCompletion = &completed
	return nil
}
func (c *releaseOwnerComponents) observeAfterOwnerViews(ctx context.Context, owner string, values []releaseowner.Target) ([]releaseowner.Target, error) {
	for index, current := range values {
		bound, ok := c.afterOwnerViews[activationKey(current)]
		if !ok || current.Owner != owner {
			continue
		}
		original := bound.prepared
		if err := c.validateAfterOwnerCompletion(releaseowner.AfterOwnerMetadata{Producer: bound.completion.Producer, ProducerTerminal: bound.completion.Terminal}); err != nil {
			return nil, err
		}
		resolved, err := c.ResolveOwnerTarget(ctx, bound.original)
		if err != nil || resolved.InstanceUUID != original.value.InstanceUUID {
			return nil, domain.ErrPlanStale
		}
		runtime, ok := original.platform.(reconcileruntime.Runtime)
		if !ok {
			return nil, domain.ErrPlanStale
		}
		kind, _, _ := releaseowner.ParseAfterOwner(bound.original.AfterOwner)
		stopped := false
		if kind == "broker" || kind == "routes" {
			instance, readErr := runtime.Incus.Instance(ctx, original.value.Project, original.value.Instance)
			if readErr != nil || instance.Config["volatile.uuid"] != original.value.InstanceUUID {
				return nil, domain.ErrPlanStale
			}
			stopped = strings.EqualFold(instance.Status, "stopped")
			if !stopped && !strings.EqualFold(instance.Status, "running") {
				return nil, domain.ErrPlanStale
			}
		}
		if kind == "materialized-integrations" {
			check, err := runtime.BindAfterOwnerIntegrations(ctx, original.nativeInstalled.Recipe(), bound.physical)
			if err != nil {
				return nil, err
			}
			plan, stopped, err := runtime.ObserveAfterOwnerIntegrations(ctx, check)
			check.Close()
			if err != nil {
				return nil, err
			}
			value := original.value
			value.State = "current"
			value.Actual = value.Desired
			if plan.Changed {
				value.State = "stale"
				value.Actual = plan.Fingerprint
			}
			if stopped {
				value.State = "deferred"
			}
			value.ReportedActual = plan.Fingerprint
			values[index] = value
			original.value = value
			c.activation[activationKey(value)] = original
			continue
		}
		if kind == "routes" {
			check, err := runtime.BindAfterOwnerRoutes(ctx, original.nativeInstalled.Recipe(), bound.physical)
			if err != nil {
				return nil, err
			}
			check.Close()
			route, ok := original.route.(*testYardRouteConsumerActivation)
			if !ok {
				return nil, domain.ErrPlanStale
			}
			value := original.value
			value.State = "stale"
			value.Actual = bound.original.Source
			verifyErr := route.Verify(ctx, original.before)
			if errors.Is(verifyErr, context.Canceled) || errors.Is(verifyErr, context.DeadlineExceeded) {
				return nil, verifyErr
			}
			if verifyErr == nil {
				value.State = "current"
				value.Actual = value.Desired
			}
			if stopped {
				value.State = "deferred"
			}
			values[index] = value
			original.value = value
			c.activation[activationKey(value)] = original
			continue
		}
		if kind == "broker" {
			check, err := runtime.BindAfterOwnerBroker(ctx, original.nativeInstalled.Recipe(), bound.physical)
			if err != nil {
				return nil, err
			}
			converged, err := runtime.VerifyStage(ctx, original.stage)
			check.Close()
			if err != nil {
				return nil, err
			}
			value := original.value
			value.State = "current"
			value.Actual = value.Desired
			if !converged {
				value.State = "stale"
				value.Actual = nativeValueDigest(struct {
					Desired string
					Drift   bool
				}{value.Desired, true})
			}
			if stopped {
				value.State = "deferred"
			}
			values[index] = value
			original.value = value
			c.activation[activationKey(value)] = original
			continue
		}
		if kind == "runtime" {
			check, err := runtime.BindAfterOwnerRuntimeSources(ctx, original.nativeInstalled.Recipe())
			if err != nil {
				return nil, err
			}
			observed, generation, capable, err := runtime.ObservePreparedAfterOwnerRuntime(ctx, check)
			_ = check.Close()
			if err != nil {
				return nil, err
			}
			if observed.Desired != original.value.ReportedDesired {
				return nil, domain.ErrPlanStale
			}
			if capable {
				before, err := releaseowner.ParseFenceRecipe(original.value.Before)
				if err != nil || generation != before.Expected && generation != before.Generation {
					return nil, domain.ErrPlanStale
				}
			}
			value := original.value
			value.State = string(observed.State)
			value.ReportedActual = observed.Actual
			value.Actual = observed.Actual
			if value.Actual == "" {
				value.Actual = nativeValueDigest(struct {
					State string `json:"state"`
				}{value.State})
			}
			values[index] = value
			original.value = value
			c.activation[activationKey(value)] = original
			continue
		}
		check, err := runtime.BindAfterOwnerMaterializedConfigs(ctx, original.nativeInstalled.Recipe(), bound.physical)
		if err != nil {
			return nil, err
		}
		_ = check.Close()
		if err = c.cli.refreshConfigApply(ctx, original.configs); err != nil {
			return nil, err
		}
		assessment := original.configs.current[0]
		value := original.value
		value.State = "current"
		if assessment.Changed {
			value.State = "stale"
		}
		if assessment.State == "stopped" {
			value.State = "deferred"
		}
		value.Actual = assessment.MaterializedFingerprint
		value.ReportedActual = assessment.MaterializedFingerprint
		// Preserve the one measured Desired/Before/recipe; observation only checks
		// drift/convergence against it and cannot publish another resolution.
		values[index] = value
		original.value = value
		c.activation[activationKey(value)] = original
	}
	return values, nil
}

func (c *releaseOwnerComponents) prepareAfterOwnerRuntime(ctx context.Context, prepared releaseActivationPrepared) (releaseActivationPrepared, error) {
	runtime, ok := prepared.platform.(reconcileruntime.Runtime)
	if !ok {
		return prepared, domain.ErrPlanStale
	}
	var source *reconcileruntime.PreparedInstalledRuntime
	var err error
	if coreInstalledRuntimeID(prepared.value.Owner) {
		source, err = runtime.PrepareInstalledRuntime(ctx, prepared.value.Owner)
	} else {
		source, err = runtime.PrepareProfileRuntimeSources(ctx, prepared.value.Owner)
	}
	if err != nil {
		return prepared, err
	}
	defer source.Close()
	p, err := source.ReserveAfterOwnerMetadata(afterOwnerTemplate("runtime"))
	if err != nil {
		return prepared, err
	}
	if err = p.ShareSources(c.retainPreparedSource); err != nil {
		return prepared, errors.Join(err, p.Close())
	}
	prepared.nativeInstalled = p
	prepared.value.Source = nativeValueDigest(struct{ Source, Recipe string }{prepared.value.Source, p.Binding()})
	prepared.value.PreparedRecipe = p.Binding()
	prepared.value.AfterOwner = "v1:runtime:" + p.Binding()
	if c.installedRecipes == nil {
		c.installedRecipes = map[string]*reconcileruntime.PreparedInstalledRuntime{}
	}
	c.installedRecipes[p.Binding()] = p
	c.closers = append(c.closers, p)
	return prepared, nil
}

func (c *releaseOwnerComponents) prepareAfterOwnerIntegration(ctx context.Context, target configTarget, runtime reconcileruntime.Runtime, source string) (releaseActivationPrepared, error) {
	if child, ok := c.ownerChildren.inits[target.Name]; ok && child.execution != nil {
		runtime.HookPlan = child.execution.hookPlan
		runtime.RuntimePlan = child.execution.runtimePlan
	}
	captured, err := runtime.PrepareAfterOwnerIntegrationSources(ctx)
	if err != nil {
		return releaseActivationPrepared{}, err
	}
	defer captured.Close()
	bound, err := captured.AfterOwnerIntegrationPhysicalBound()
	if err != nil {
		return releaseActivationPrepared{}, err
	}
	p, err := captured.ReserveAfterOwnerIntegrationMetadata(afterOwnerSizedTemplate(strings.Repeat("f", 64), make([]byte, bound)))
	if err != nil {
		return releaseActivationPrepared{}, err
	}
	if err = p.ShareSources(c.retainPreparedSource); err != nil {
		p.Close()
		return releaseActivationPrepared{}, err
	}
	plan := captured.PreparedIntegrations().Plan()
	desired := nativeValueDigest(struct{ Scope string }{plan.Scope})
	value := c.coreTarget("materialized-config", "materialized-integrations", target.Name, "deferred", "integrations", "", desired, desired, nativeValueDigest(struct{ Input, Scope, Component string }{source, plan.Scope, p.MaterializedSourceBinding()}))
	value.PreparedRecipe = p.Binding()
	value.AfterOwner = "v1:materialized-integrations:" + p.Binding()
	value, err = c.bindGuestTarget(ctx, target.Loaded, value)
	if err != nil {
		p.Close()
		return releaseActivationPrepared{}, err
	}
	if c.installedRecipes == nil {
		c.installedRecipes = map[string]*reconcileruntime.PreparedInstalledRuntime{}
	}
	c.installedRecipes[p.Binding()] = p
	c.closers = append(c.closers, p)
	return releaseActivationPrepared{value: value, platform: runtime, configTarget: target, nativeInstalled: p}, nil
}
