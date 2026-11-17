package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

const releaseTransitionRequestLimit = 1 << 20

func (cli *CLI) runReleaseTransition(ctx context.Context, arguments []string) int {
	// Released callers can still supply their old operator descriptor. Ignore
	// its contents and prevent native child helpers from inheriting it.
	if fd, err := strconv.Atoi(os.Getenv("SUBYARD_MIGRATION_OPERATOR_INPUT_FD")); err == nil && fd >= 3 && fd <= 64 {
		syscall.CloseOnExec(fd)
	}
	if len(arguments) != 0 {
		cli.errorf("internal: _release-transition accepts a JSON request on stdin")
		return 2
	}
	protocolOutput := cli.options.Stdout
	cli.options.Stdout = io.Discard
	defer func() { cli.options.Stdout = protocolOutput }()
	payload, err := io.ReadAll(io.LimitReader(cli.options.Stdin, releaseTransitionRequestLimit+1))
	var envelope struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err != nil || len(payload) > releaseTransitionRequestLimit || json.Unmarshal(payload, &envelope) != nil {
		cli.errorf("release transition request is invalid")
		return 2
	}
	if envelope.SchemaVersion == releasetransition.RollbackTargetProcessSchemaV1 {
		return cli.runRollbackTargetProcess(ctx, payload, protocolOutput)
	}
	var request releasetransition.ProcessRequest
	var recovery *releasetransition.ActivationOnlyRecoveryRequest
	if envelope.SchemaVersion == releasetransition.ProcessRecoverySchemaV2 {
		parsed, err := releasetransition.ParseRecoveryProcessRequest(payload)
		if err != nil {
			cli.errorf("release recovery request is invalid")
			return 2
		}
		if parsed.Mode == releasetransition.RecoveryProcessRuntimeCapabilities {
			var ids []string
			for _, owner := range cli.nonConfigActivationReconcilers(releasetransition.ProcessRequest{}) {
				ids = append(ids, owner.ID())
			}
			slices.Sort(ids)
			encoded, err := releasetransition.MarshalRecoveryProcessResponse(releasetransition.RecoveryProcessResponse{SchemaVersion: releasetransition.ProcessRecoverySchemaV2, ActivationOwners: ids})
			if err != nil {
				return 1
			}
			_, err = fmt.Fprintln(protocolOutput, string(encoded))
			if err != nil {
				return 1
			}
			return 0
		}
		if parsed.Mode == releasetransition.RecoveryProcessRollbackSourceCapabilities || parsed.Mode == releasetransition.RecoveryProcessCapabilities || parsed.Mode == releasetransition.RecoveryProcessLifecycleCapabilities || parsed.Mode == releasetransition.RecoveryProcessMigrationOwnerCapabilities {
			capabilities := releasetransition.ActivationOnlyRecoveryCapabilities()
			if parsed.Mode == releasetransition.RecoveryProcessLifecycleCapabilities {
				capabilities = releasetransition.ActivationRecoveryLifecycleCapabilities()
			}
			if parsed.Mode == releasetransition.RecoveryProcessMigrationOwnerCapabilities {
				capabilities = releasetransition.MigrationOwnerReplacementCapabilities()
			}
			if parsed.Mode == releasetransition.RecoveryProcessRollbackSourceCapabilities {
				capabilities = releasetransition.RollbackSourceActivationCapabilities()
			}
			response := releasetransition.RecoveryProcessResponse{SchemaVersion: releasetransition.ProcessRecoverySchemaV2, Capabilities: &capabilities}
			encoded, err := releasetransition.MarshalRecoveryProcessResponse(response)
			if err != nil {
				return 1
			}
			_, err = fmt.Fprintln(protocolOutput, string(encoded))
			if err != nil {
				return 1
			}
			return 0
		}
		request, recovery = parsed.TransitionRequest(), parsed.Recovery
	} else if envelope.SchemaVersion != releasetransition.ProcessProtocolSchemaV1 || json.Unmarshal(payload, &request) != nil {
		cli.errorf("release transition request is invalid")
		return 2
	}
	expectedAuthorization, err := readReleaseTransitionGrant()
	if err != nil {
		cli.errorf("release transition authorization channel is invalid")
		return 2
	}
	// The engine-only transition ingress bypasses the normal command path that
	// establishes the operation context used by activation reconcilers.
	if !domain.SafeID(cli.ensureOperationID()) {
		cli.errorf("release transition operation context is invalid")
		return 2
	}
	if err := releaseruntime.CheckSourcePolicy(ctx, request); err != nil {
		cli.errorf("release transition: %v", err)
		return 1
	}
	operation := cli.releaseActivationOperation(request)
	reconcilers := operation.bindServiceRegistrationActivation(request, operation.activationReconcilers(request))
	ownerRegistration := operation.ownerRegistrationTransition(request)
	ingressFactory := operation.releaseTransitionIngressFactory(request)
	ctx = releasetransition.WithMigrationDiagnosticWriter(ctx, cli.options.Stderr)
	response, err := executeReleaseTransitionRequest(
		ctx, cli.options.RepositoryRoot, request,
		func(plan releasetransition.PlanToken, authorization releasetransition.Authorization) bool {
			return expectedAuthorization != "" && authorization == expectedAuthorization
		}, reconcilers, ownerRegistration, ingressFactory, recovery,
	)
	if err != nil {
		cli.errorf("release transition: %v", err)
		return 1
	}
	var publicResponse any = response
	if recovery != nil || request.MigrationOwnerReplacement != nil {
		publicResponse = releasetransition.RecoveryProcessResponse{
			SchemaVersion:                 releasetransition.ProcessRecoverySchemaV2,
			ActivationReconciliationOwned: response.ActivationReconciliationOwned,
			Inspection:                    response.Inspection, Outcome: response.Outcome,
		}
	}
	if err := json.NewEncoder(protocolOutput).Encode(publicResponse); err != nil {
		cli.errorf("release transition response: %v", err)
		return 1
	}
	return 0
}

func (cli *CLI) activationReconcilers(request releasetransition.ProcessRequest) []releasetransition.V2ActivationReconciler {
	cli = cli.releaseActivationOperation(request)
	reconcilers := cli.nonConfigActivationReconcilers(request)
	// Integration reconciliation runs project hooks, so repair installed profile runtimes first.
	materialized := &materializedConfigActivationReconciler{
		cli: cli, yard: request.Yard, configHome: request.ConfigHome,
		goal: releasetransition.Goal{
			Target: request.Target, Direction: request.Direction,
		},
		artifactDigest: request.ArtifactDigest,
		registryDigest: request.RegistryDigest,
		sourceIngress:  request.SourceIngress != nil,
		deferral:       &cli.activation.pending,
	}
	cli.activation.pending.observe = func(ctx context.Context, releases releasetransition.ReleasePair) (releasetransition.V2ActivationObservation, error) {
		return materialized.observe(ctx, releases, nil)
	}
	return slices.Insert(reconcilers, len(cli.profileRuntimeIDs()), releasetransition.V2ActivationReconciler(materialized))
}

// Keep the complete set of non-config activation owners shared by release
// transitions and bounded materialized-config repair admission.
func (cli *CLI) nonConfigActivationReconcilers(request releasetransition.ProcessRequest) []releasetransition.V2ActivationReconciler {
	cli = cli.releaseActivationOperation(request)
	var result []releasetransition.V2ActivationReconciler
	for _, id := range cli.profileRuntimeIDs() {
		result = append(result, cli.profileActivationReconciler(request, id))
	}
	return append(result, []releasetransition.V2ActivationReconciler{
		&installationActivationReconciler{cli: cli, request: request},
		cli.profileActivationReconciler(request, "core-guest-runtime"),
		cli.profileActivationReconciler(request, "key-tools-runtime"),
		cli.brokerActivationReconciler(request),
		cli.routeConsumerActivationReconciler(request),
		cli.networkGuardActivationReconciler(request),
		cli.powerActivationReconciler(request),
	}...)
}

type ownerRegistrationTransition struct {
	registeredYards, guestDiskTmp []releasetransition.V2ActivationReconciler
	endpointSettings              []string
	options                       func() (testyardmigration.Options, error)
}

func (transition *ownerRegistrationTransition) registeredMigrationOwners() ([]releasetransition.V2ActivationReconciler, []releasetransition.V2ActivationReconciler, []string) {
	return transition.registeredYards, transition.guestDiskTmp, transition.endpointSettings
}

func (transition *ownerRegistrationTransition) Prepare(
	ctx context.Context,
	prospective releasetransition.V2SettingsSnapshotView,
) (releasetransition.OwnerRegistrationObservation, error) {
	options, err := transition.options()
	if err != nil {
		return releasetransition.OwnerRegistrationObservation{}, err
	}
	var prepared testyardmigration.Prepared
	if prospective == nil {
		prepared, err = testyardmigration.Prepare(ctx, options)
	} else {
		prepared, err = testyardmigration.PrepareProspective(
			ctx, options, prospective.ReadSnapshot,
		)
	}
	return releasetransition.OwnerRegistrationObservation{
		State:        releasetransition.OwnerRegistrationState(prepared.State),
		Registration: releasetransition.Fingerprint(prepared.RegistrationDigest),
		Overrides:    releasetransition.Fingerprint(prepared.OverridesDigest),
		Controller:   releasetransition.Fingerprint(prepared.ControllerDigest),
		SharedImages: prepared.SharedImages,
	}, err
}

// A fresh update leaves a legacy/conflicting test owner for explicit recreation.
func (transition *ownerRegistrationTransition) FreshWarning(ctx context.Context, view releasetransition.V2SettingsSnapshotView) (string, error) {
	options, err := transition.options()
	if err != nil {
		return "", err
	}
	if view != nil {
		files, err := prospectiveActivationFiles(options.ConfigHome, view)
		if err != nil {
			return "", err
		}
		options.ProspectiveFiles = files
	}
	return testYardRecreation(ctx, options)
}

type testYardRecreateRefusal struct{}

func (err testYardRecreateRefusal) Error() string {
	message, next := err.ActivationDiagnostic()
	return message + "; " + next
}
func (testYardRecreateRefusal) MigrationRefusal() bool { return true }
func (err testYardRecreateRefusal) ActivationDiagnostic() (string, string) {
	return "test-yard registration or routes require recreation; preserve other sessions' VMs and confirm removal explicitly", "run yard update --check for the registered-owner recreation commands"
}

func (transition *ownerRegistrationTransition) Commit(
	ctx context.Context,
	before releasetransition.OwnerRegistrationObservation,
) error {
	options, err := transition.options()
	if err != nil {
		return err
	}
	if !before.TerminalCleanup {
		ctx = ownerMigrationChildContext(ctx)
	}
	options.RecoveryToken = before.RecoveryToken
	options.TerminalCleanup = before.TerminalCleanup
	return testyardmigration.Commit(ctx, options, testyardmigration.Prepared{
		State:              testyardmigration.State(before.State),
		RegistrationDigest: string(before.Registration),
		OverridesDigest:    string(before.Overrides),
		ControllerDigest:   string(before.Controller),
		SharedImages:       before.SharedImages,
	})
}

func (transition *ownerRegistrationTransition) Observe(
	ctx context.Context,
	before releasetransition.OwnerRegistrationObservation,
) (releasetransition.OwnerRegistrationProgress, error) {
	options, err := transition.options()
	if err != nil {
		return "", err
	}
	options.RecoveryToken = before.RecoveryToken
	options.TerminalCleanup = before.TerminalCleanup
	progress, err := testyardmigration.ObserveProgress(
		ctx, options, testyardmigration.Prepared{
			State:              testyardmigration.State(before.State),
			RegistrationDigest: string(before.Registration),
			OverridesDigest:    string(before.Overrides),
			ControllerDigest:   string(before.Controller),
			SharedImages:       before.SharedImages,
		},
	)
	return releasetransition.OwnerRegistrationProgress(progress), err
}

func (cli *CLI) ownerRegistrationTransition(
	request releasetransition.ProcessRequest,
) releasetransition.V2OwnerRegistration {
	return &ownerRegistrationTransition{endpointSettings: cli.resources.EndpointSettingNames(), registeredYards: cli.registeredYardMigrations(request, false), guestDiskTmp: cli.registeredYardMigrations(request, true), options: func() (testyardmigration.Options, error) {
		return cli.releaseTransitionTestYardOptions(request)
	}}
}

type activationApplicability struct {
	state   string
	applies bool
	target  string
}

type activationStageReconciler struct {
	configuration        *CLI
	prospective          func(releasetransition.V2SettingsSnapshotView) (*activationStageReconciler, error)
	sourceActivation     func(context.Context, releasetransition.ReleasePair, releasetransition.ReleaseLinks, releasetransition.V2SourceActivationView) (releasetransition.V2SourceActivationPlan, error)
	id                   string
	stage                ports.ReconcileStageID
	inspectApplicability func(context.Context) (activationApplicability, error)
	platform             func(context.Context, activationApplicability) (ports.ReconcileStageRunner, error)
	authorize            func(context.Context) error
	planned              func() []string
	after                func(context.Context) error
}

type activationStageError struct {
	phase string
	err   error
}

type routeConsumerActivationPort interface {
	PrepareOwner(context.Context) (string, error)
	Prepare(context.Context) (string, error)
	Verify(context.Context, string) error
}

type routeConsumerActivationReconciler struct {
	configuration    *CLI
	sourceActivation func(context.Context, releasetransition.ReleasePair, releasetransition.ReleaseLinks, releasetransition.V2SourceActivationView) (releasetransition.V2SourceActivationPlan, error)
	port             routeConsumerActivationPort
}

func (*routeConsumerActivationReconciler) ID() string { return "test-yard-route-consumers" }

func (reconciler *routeConsumerActivationReconciler) ObserveProspective(ctx context.Context, releases releasetransition.ReleasePair, links releasetransition.ReleaseLinks, view releasetransition.V2SettingsSnapshotView) (releasetransition.V2ActivationObservation, error) {
	port, ok := reconciler.port.(interface {
		Prospective(releasetransition.V2SettingsSnapshotView) (routeConsumerActivationPort, error)
	})
	if !ok || view == nil {
		return reconciler.Observe(ctx, releases, links)
	}
	projected, err := port.Prospective(view)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	return (&routeConsumerActivationReconciler{configuration: reconciler.configuration, port: projected}).Observe(ctx, releases, links)
}

func (reconciler *routeConsumerActivationReconciler) Observe(
	ctx context.Context,
	releases releasetransition.ReleasePair,
	_ releasetransition.ReleaseLinks,
) (releasetransition.V2ActivationObservation, error) {
	if reconciler == nil || reconciler.port == nil {
		return releasetransition.V2ActivationObservation{}, errors.New("route consumer activation reconciler is unavailable")
	}
	warning := ""
	var err error
	if probe, ok := reconciler.port.(interface {
		releaseRecreation(context.Context) (string, error)
	}); ok {
		warning, err = probe.releaseRecreation(ctx)
		if err != nil {
			return releasetransition.V2ActivationObservation{}, err
		}
	}
	owner := string(testyardmigration.StateAbsent)
	if warning == "" {
		owner, err = reconciler.port.PrepareOwner(ctx)
		if err != nil {
			return releasetransition.V2ActivationObservation{}, err
		}
	}
	active := owner != string(testyardmigration.StateAbsent)
	desired, err := activationStageFingerprint(struct {
		ID     string                      `json:"id"`
		Target releasetransition.ReleaseID `json:"target"`
		Active bool                        `json:"active"`
	}{reconciler.ID(), releases.Target, active})
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	if warning != "" {
		return releasetransition.V2ActivationObservation{Actual: desired, Desired: desired, Converged: true, Warnings: []string{warning}}, nil
	}
	if !active {
		return releasetransition.V2ActivationObservation{
			Actual: desired, Desired: desired, Converged: true,
		}, nil
	}
	if owner != string(testyardmigration.StateCurrent) {
		return releasetransition.V2ActivationObservation{Actual: desired, Desired: desired, Converged: true, Warnings: []string{testYardRecreateRefusal{}.Error()}}, nil
	}

	before, err := reconciler.port.Prepare(ctx)
	if err == nil {
		err = reconciler.port.Verify(ctx, before)
	}
	if err == nil {
		return releasetransition.V2ActivationObservation{Actual: desired, Desired: desired, Converged: true}, nil
	}
	var conflict interface{ ActivationConflict() bool }
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &conflict) || !conflict.ActivationConflict() {
		return releasetransition.V2ActivationObservation{}, err
	}

	warning = testYardRecreateRefusal{}.Error()
	if probe, ok := reconciler.port.(interface{ releaseRecreationHint() (string, error) }); ok {
		warning, err = probe.releaseRecreationHint()
		if err != nil {
			return releasetransition.V2ActivationObservation{}, err
		}
	}
	return releasetransition.V2ActivationObservation{Actual: desired, Desired: desired, Converged: true, Warnings: []string{warning}}, nil

}

func (reconciler *routeConsumerActivationReconciler) Reconcile(
	ctx context.Context,
	_ releasetransition.ReleaseLinks,
) error {
	if reconciler == nil || reconciler.port == nil {
		return errors.New("route consumer activation reconciler is unavailable")
	}
	if probe, ok := reconciler.port.(interface {
		releaseRecreation(context.Context) (string, error)
	}); ok {
		warning, err := probe.releaseRecreation(ctx)
		if err != nil {
			return err
		}
		if warning != "" {
			return nil
		}
	}
	ctx = routeMigrationChildContext(ctx)
	owner, err := reconciler.port.PrepareOwner(ctx)
	if err != nil {
		return err
	}
	if owner == string(testyardmigration.StateAbsent) {
		return nil
	}
	if owner != string(testyardmigration.StateCurrent) {
		return testYardRecreateRefusal{}
	}
	before, err := reconciler.port.Prepare(ctx)
	if err != nil {
		return err
	}
	if err := reconciler.port.Verify(ctx, before); err == nil {
		return nil
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return testYardRecreateRefusal{}
}

type testYardRouteConsumerActivation struct {
	options testyardmigration.Options
}

func (activation *testYardRouteConsumerActivation) releaseRecreation(ctx context.Context) (string, error) {
	return testYardRecreation(ctx, activation.options)
}
func (activation *testYardRouteConsumerActivation) releaseRecreationHint() (string, error) {
	return testyardmigration.RecreationHint(activation.options, false)
}

func prospectiveActivationFiles(root string, view releasetransition.V2SettingsSnapshotView) (*config.ProspectiveFiles, error) {
	if view == nil {
		return nil, nil
	}
	yards, err := view.ListYards()
	if err != nil {
		return nil, err
	}
	return &config.ProspectiveFiles{Root: root, Yards: yards, ReadSnapshot: view.ReadSnapshot}, nil
}

func (activation *testYardRouteConsumerActivation) Prospective(view releasetransition.V2SettingsSnapshotView) (routeConsumerActivationPort, error) {
	files, err := prospectiveActivationFiles(activation.options.ConfigHome, view)
	if err != nil {
		return nil, err
	}
	projected := *activation
	projected.options.ProspectiveFiles = files
	return &projected, nil
}

func (activation *testYardRouteConsumerActivation) PrepareOwner(
	ctx context.Context,
) (string, error) {
	state, err := testyardmigration.PrepareRouteConsumerActivation(ctx, activation.options)
	return string(state), err
}

func (activation *testYardRouteConsumerActivation) Prepare(ctx context.Context) (string, error) {
	return testyardmigration.PrepareRouteConsumersActivation(ctx, activation.options)
}

func (activation *testYardRouteConsumerActivation) Verify(ctx context.Context, before string) error {
	return testyardmigration.VerifyRouteConsumersActivation(ctx, activation.options, before)
}

func (cli *CLI) routeConsumerActivationReconciler(
	request releasetransition.ProcessRequest,
) releasetransition.V2ActivationReconciler {
	options, err := cli.releaseTransitionTestYardOptions(request)
	if err != nil {
		return &routeConsumerActivationReconciler{configuration: cli, port: routeConsumerActivationError{err: err}}
	}
	return &routeConsumerActivationReconciler{
		configuration:    cli,
		sourceActivation: cli.prepareRouteSourceActivation(request),
		port:             &testYardRouteConsumerActivation{options: options},
	}
}

type routeConsumerActivationError struct{ err error }

func (failure routeConsumerActivationError) PrepareOwner(context.Context) (string, error) {
	return "", failure.err
}
func (failure routeConsumerActivationError) Prepare(context.Context) (string, error) {
	return "", failure.err
}
func (failure routeConsumerActivationError) Verify(context.Context, string) error { return failure.err }

func (failure activationStageError) Error() string { return failure.err.Error() }

func (failure activationStageError) Unwrap() error { return failure.err }

func (failure activationStageError) ActivationPhase() string { return failure.phase }

func (reconciler *activationStageReconciler) failure(phase string, err error) error {
	if err == nil {
		return nil
	}
	return activationStageError{phase: phase, err: err}
}

func (reconciler *activationStageReconciler) ID() string { return reconciler.id }

func (reconciler *activationStageReconciler) ObserveProspective(ctx context.Context, releases releasetransition.ReleasePair, links releasetransition.ReleaseLinks, view releasetransition.V2SettingsSnapshotView) (releasetransition.V2ActivationObservation, error) {
	if view == nil || reconciler.prospective == nil {
		return reconciler.Observe(ctx, releases, links)
	}
	projected, err := reconciler.prospective(view)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	if projected != nil {
		projected.planned = reconciler.planned
	}
	return projected.Observe(ctx, releases, links)
}

func (reconciler *activationStageReconciler) Observe(
	ctx context.Context,
	releases releasetransition.ReleasePair,
	links releasetransition.ReleaseLinks,
) (releasetransition.V2ActivationObservation, error) {
	if reconciler == nil || reconciler.inspectApplicability == nil || reconciler.platform == nil {
		return releasetransition.V2ActivationObservation{}, errors.New("activation stage reconciler is unavailable")
	}
	applicability, err := reconciler.inspectApplicability(ctx)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	desired, err := activationStageFingerprint(struct {
		ID     string                      `json:"id"`
		Stage  ports.ReconcileStageID      `json:"stage"`
		State  string                      `json:"state"`
		Target releasetransition.ReleaseID `json:"target"`
	}{reconciler.id, reconciler.stage, applicability.state, releases.Target})
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	var planned []string
	if reconciler.planned != nil {
		planned = reconciler.planned()
	}
	if !applicability.applies {
		return releasetransition.V2ActivationObservation{
			Actual: desired, Desired: desired, Converged: true, Planned: planned,
		}, nil
	}
	platform, err := reconciler.platform(ctx, applicability)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	converged, err := platform.VerifyStage(ctx, reconciler.stage)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	actual := desired
	if !converged {
		actual, err = activationStageFingerprint(struct {
			ID     string                      `json:"id"`
			Stage  ports.ReconcileStageID      `json:"stage"`
			State  string                      `json:"state"`
			Active releasetransition.ReleaseID `json:"active"`
			Drift  bool                        `json:"drift"`
		}{reconciler.id, reconciler.stage, applicability.state, links.Active, true})
		if err != nil {
			return releasetransition.V2ActivationObservation{}, err
		}
	}
	return releasetransition.V2ActivationObservation{
		Actual: actual, Desired: desired, Converged: converged, Planned: planned,
	}, nil
}

func (reconciler *activationStageReconciler) Reconcile(
	ctx context.Context,
	_ releasetransition.ReleaseLinks,
) error {
	if reconciler == nil || reconciler.inspectApplicability == nil || reconciler.platform == nil {
		return errors.New("activation stage reconciler is unavailable")
	}
	applicability, err := reconciler.inspectApplicability(ctx)
	if err != nil || !applicability.applies {
		return reconciler.failure("applicability", err)
	}
	platform, err := reconciler.platform(ctx, applicability)
	if err != nil {
		return reconciler.failure("platform", err)
	}
	converged, err := platform.VerifyStage(ctx, reconciler.stage)
	if err != nil {
		return reconciler.failure("pre-verify", err)
	}
	if !converged {
		if reconciler.authorize != nil {
			if err := reconciler.authorize(ctx); err != nil {
				return reconciler.failure("authorization", err)
			}
			// Rebuild so the guarded scripts receive the verified sudo authorization.
			if platform, err = reconciler.platform(ctx, applicability); err != nil {
				return reconciler.failure("platform", err)
			}
		}
		if err := platform.ApplyStage(ctx, reconciler.stage); err != nil {
			return reconciler.failure("apply", err)
		}
	}
	if verifier, ok := platform.(interface {
		VerifyStageWithDiagnostic(context.Context, ports.ReconcileStageID) (bool, error)
	}); ok {
		converged, err = verifier.VerifyStageWithDiagnostic(ctx, reconciler.stage)
	} else {
		converged, err = platform.VerifyStage(ctx, reconciler.stage)
	}
	if err != nil {
		return reconciler.failure("post-verify", err)
	}
	if !converged {
		return reconciler.failure(
			"post-verify",
			fmt.Errorf("activation stage %s did not converge", reconciler.stage),
		)
	}
	if reconciler.after != nil {
		return reconciler.after(ctx)
	}
	return nil
}

func activationStageFingerprint(value any) (releasetransition.Fingerprint, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return releasetransition.Fingerprint(fmt.Sprintf("%x", digest[:])), nil
}

func (cli *CLI) brokerActivationReconciler(
	request releasetransition.ProcessRequest,
) releasetransition.V2ActivationReconciler {
	return cli.brokerActivationWithFiles(request, nil)
}

func (cli *CLI) brokerActivationWithFiles(request releasetransition.ProcessRequest, files *config.ProspectiveFiles) *activationStageReconciler {
	options := func() (testyardmigration.Options, error) {
		values, err := cli.releaseTransitionTestYardOptions(request)
		values.ProspectiveFiles = files
		return values, err
	}
	return &activationStageReconciler{
		configuration: cli,
		prospective: func(view releasetransition.V2SettingsSnapshotView) (*activationStageReconciler, error) {
			projected, err := prospectiveActivationFiles(request.ConfigHome, view)
			if err != nil {
				return nil, err
			}
			return cli.brokerActivationWithFiles(request, projected), nil
		},
		sourceActivation: cli.prepareBrokerSourceActivation(request),
		id:               "test-vm-broker", stage: ports.ReconcileStageTestVMs,
		inspectApplicability: func(ctx context.Context) (activationApplicability, error) {
			values, err := options()
			if err != nil {
				return activationApplicability{}, err
			}
			warning, err := testYardRecreation(ctx, values)
			if err != nil {
				return activationApplicability{}, err
			}
			if warning != "" {
				return activationApplicability{state: string(testyardmigration.BrokerRuntimeAbsent)}, nil
			}
			state, yard, err := testyardmigration.PrepareBrokerRuntimeTarget(ctx, values)
			// Release repair leaves a deferred broker yard untouched; its state
			// stays in Desired, like the materialized configuration of that yard.
			deferred := err == nil && cli.activation != nil && cli.activation.repair && cli.activation.pending.decided(ctx, releasetransition.ReleasePair{})[yard]
			return activationApplicability{
				state: string(state), applies: state == testyardmigration.BrokerRuntimeActive && !deferred,
				target: yard,
			}, err
		},
		platform: func(ctx context.Context, applicability activationApplicability) (ports.ReconcileStageRunner, error) {
			if applicability.target == "" {
				return nil, errors.New("active test VM broker has no registered yard")
			}
			loaded, err := cli.resolveReleaseTransitionContext(ctx,
				applicability.target,
				request.ConfigHome, files,
			)
			if err != nil {
				return nil, err
			}
			return cli.initPlatformWithDispatcher(
				loaded,
				[]domain.Context{loaded.Context},
				filepath.Join(cli.options.RepositoryRoot, "bin", "yard-engine"),
			), nil
		},
		authorize: cli.releaseTransitionPrivilegeAuthorization,
	}
}

func (cli *CLI) releaseTransitionTestYardOptions(
	request releasetransition.ProcessRequest,
) (testyardmigration.Options, error) {
	environment := cli.freshMigrationEnvironment(cli.baseEnv, cli.options.RepositoryRoot)
	dataHome := environment["SUBYARD_HOME"]
	if dataHome == "" {
		operatorHome := environment["SUBYARD_OPERATOR_HOME"]
		if operatorHome == "" {
			operatorHome = environment["HOME"]
		}
		if operatorHome == "" || !filepath.IsAbs(operatorHome) {
			return testyardmigration.Options{}, errors.New("release transition operator home is unavailable")
		}
		dataHome = filepath.Join(operatorHome, ".subyard")
	}
	return testyardmigration.Options{
		Executable:     filepath.Join(cli.options.RepositoryRoot, "bin", "yard-engine"),
		RepositoryRoot: cli.options.RepositoryRoot,
		Incus:          "incus",
		ConfigHome:     request.ConfigHome, DataHome: dataHome,
		Environment: environmentList(environment, nil),
		Stdout:      cli.options.Stderr, Stderr: cli.options.Stderr,
		RunYard: cli.runReleaseTransitionYardCommand,
		EnsureImageProject: func(ctx context.Context, yard string, shared bool) error {
			return cli.ensureReleaseTransitionImageProject(ctx, request.ConfigHome, yard, shared)
		},
	}, nil
}

func (cli *CLI) ensureReleaseTransitionImageProject(ctx context.Context, configHome, yard string, shared bool) error {
	if yard != testyardmigration.CurrentYard && yard != testyardmigration.LegacyYard {
		return domain.ErrPlanStale
	}
	incus, _ := cli.statusPorts()
	native, ok := incus.(interface {
		PrepareImageProject(context.Context, string, bool) (ports.PreparedImageProject, error)
		ApplyPreparedImageProject(context.Context, ports.PreparedImageProject) error
	})
	if !ok {
		return domain.ErrPlanStale
	}
	prepared, err := native.PrepareImageProject(ctx, "subyard-"+yard, shared)
	if err != nil {
		return err
	}
	if !prepared.Absent || !shared {
		return native.ApplyPreparedImageProject(ctx, prepared)
	}
	loaded, err := cli.resolveReleaseTransitionContext(ctx, yard, configHome)
	if err != nil {
		return err
	}
	if loaded.Context.YardName != yard || loaded.Context.IncusProject != prepared.Project || loaded.Context.AccessKind != domain.AccessLocal || loaded.Context.Paths.ConfigHome != configHome {
		return domain.ErrPlanStale
	}
	runtime, ok := cli.initPlatform(loaded, []domain.Context{loaded.Context}).(reconcileruntime.Runtime)
	if !ok {
		return domain.ErrPlanStale
	}
	sources, err := runtime.PrepareOwnerRuntimeSources(ctx)
	if err != nil {
		return err
	}
	defer sources.Close()
	prepared.InitialConfig, err = runtime.RenderOwnerProjectConfig(ctx, sources)
	if err != nil {
		return err
	}
	return native.ApplyPreparedImageProject(ctx, prepared)
}

func (cli *CLI) releaseTransitionIngressFactory(
	request releasetransition.ProcessRequest,
) func(releasetransition.ReleasePair) (releasetransition.V2Ingress, error) {
	return func(_ releasetransition.ReleasePair) (releasetransition.V2Ingress, error) {
		if request.SourceIngress != nil {
			sourceRoot, sourceErr := filepath.EvalSymlinks(request.SourceIngress.SourceRoot)
			runtimeRoot, runtimeErr := filepath.EvalSymlinks(request.RuntimeRoot)
			if sourceErr == nil && runtimeErr == nil && filepath.IsAbs(sourceRoot) && filepath.IsAbs(runtimeRoot) && pathWithinCLI(sourceRoot, runtimeRoot) {
				return nil, errors.New("source ingress path is inside the runtime root")
			}
			// Bootstrap passes a diagnostic class beside the descriptor in the
			// inherited environment; missing/unknown classes stay non-destructive.
			if cli.baseEnv["SUBYARD_SOURCE_INGRESS_V1_CLASS"] == "legacy" {
				return nil, errors.New(releaseruntime.LegacyReinstallGuidance)
			}
			return nil, fmt.Errorf("entrypoints point at the checkout %s; if this is an installation older than v0.11.0, see docs/reference/legacy-migration-support.md", request.SourceIngress.SourceRoot)
		}

		return &migration.V2CompatibilityIngress{}, nil
	}
}

func (cli *CLI) powerActivationReconciler(
	request releasetransition.ProcessRequest,
) releasetransition.V2ActivationReconciler {
	return &activationStageReconciler{
		configuration: cli,
		id:            "host-power", stage: ports.ReconcileStagePower,
		inspectApplicability: func(context.Context) (activationApplicability, error) {
			return inspectPowerActivationApplicability(cli.env)
		},
		platform: func(ctx context.Context, _ activationApplicability) (ports.ReconcileStageRunner, error) {
			// All registered yards must be loaded from candidate configuration,
			// without treating the active CLI's resolved settings as overrides.
			operation := *cli
			operation.baseEnv = cli.freshMigrationEnvironment(cli.baseEnv, cli.options.RepositoryRoot)
			operation.env = maps.Clone(operation.baseEnv)
			// The fresh environment drops inherited flags; keep only this process's verified one.
			if cli.env["SUBYARD_SUDO_PREAUTHORIZED"] == "1" {
				operation.env["SUBYARD_SUDO_PREAUTHORIZED"] = "1"
			}
			// Host power serves the yards that load. A yard whose own configuration
			// is invalid is reported by the yard-scoped owners, not here.
			loaded, targets, _, err := operation.tolerantActivationTargets(ctx, "default", request.ConfigHome, true, nil, false)
			if err != nil {
				return nil, err
			}
			powerYards := make([]domain.Context, 0, len(targets))
			for _, target := range targets {
				powerYards = append(powerYards, target.Loaded.Context)
			}
			return operation.initPlatformWithDispatcher(
				loaded,
				powerYards,
				filepath.Join(cli.options.RepositoryRoot, "bin", "yard-engine"),
			), nil
		},
		authorize: cli.releaseTransitionPrivilegeAuthorization,
	}
}

// networkGuardActivationReconciler migrates a guard written by a released version. It applies
// whenever such a guard exists, even where the boot reconciler was never installed, and runs
// only the host-power helper's guard-only action, which installs nothing else.
func (cli *CLI) networkGuardActivationReconciler(
	request releasetransition.ProcessRequest,
) releasetransition.V2ActivationReconciler {
	power := cli.powerActivationReconciler(request).(*activationStageReconciler)
	return &activationStageReconciler{
		configuration: cli,
		id:            "host-network-guard", stage: ports.ReconcileStageNetworkGuard,
		inspectApplicability: func(context.Context) (activationApplicability, error) {
			current, err := hostruntime.NetworkManagerGuardCurrent(hostruntime.NetworkManagerGuardPath)
			if err != nil {
				return activationApplicability{}, fmt.Errorf("inspect the NetworkManager guard: %w", err)
			}
			// The state stays the same across the rewrite, so the authorized Desired
			// fingerprint is stable through reconciliation and resume.
			return activationApplicability{state: "network-guard", applies: !current}, nil
		},
		platform:  power.platform,
		authorize: cli.releaseTransitionPrivilegeAuthorization,
		planned: func() []string {
			if current, err := hostruntime.NetworkManagerGuardCurrent(hostruntime.NetworkManagerGuardPath); err == nil && current {
				return nil
			}
			return []string{"rewrite the released Subyard NetworkManager guard without reloading NetworkManager; reboot the host once afterwards to apply it safely"}
		},
	}
}

func (cli *CLI) releaseTransitionPrivilegeAuthorization(ctx context.Context) error {
	if cli.options.InitPlatform != nil {
		return nil
	}
	return cli.prepareSudoPrivileges(ctx, cli.options.Stderr, cli.effectiveUID(), "update")
}

func inspectPowerActivationApplicability(
	environment map[string]string,
) (activationApplicability, error) {
	paths := []struct {
		setting  string
		fallback string
	}{
		{"SUBYARD_POWER_RECONCILER_PATH", "/usr/local/libexec/subyard/yard-boot-reconcile"},
		{"SUBYARD_POWER_UNIT_PATH", "/etc/systemd/system/subyard-power-reconcile.service"},
	}
	present := false
	for _, candidate := range paths {
		path := environment[candidate.setting]
		if path == "" {
			path = candidate.fallback
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return activationApplicability{}, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return activationApplicability{}, fmt.Errorf("power reconciler path is a symbolic link: %s", path)
		}
		present = true
	}
	if !present {
		return activationApplicability{state: "absent"}, nil
	}
	return activationApplicability{state: "installed", applies: true}, nil
}

type materializedConfigActivationReconciler struct {
	cli                       *CLI
	yard                      string
	configHome                string
	goal                      releasetransition.Goal
	artifactDigest            releasetransition.Fingerprint
	registryDigest            releasetransition.Fingerprint
	allLocal                  bool
	scopeResolved             bool
	integrationPlans          map[string]reconcileruntime.IntegrationPlan
	sourceIngress             bool
	projectedIntegrationPlans bool
	projectedUnavailable      map[string]projectedIntegrationUnavailable
	recoveryApply             *configApplyExecution
	// deferral records the yards this owner defers for pending configuration;
	// it is the activation's shared record when the owner belongs to one.
	deferral *pendingYardDeferral
}

type releaseTransitionConfigApplier struct {
	cli             *CLI
	approvedTargets []configTarget
}

func (applier releaseTransitionConfigApplier) ApplyConfig(
	ctx context.Context,
	yard string,
) error {
	if applier.cli == nil {
		return errors.New("release transition config applier is unavailable")
	}
	if applier.approvedTargets != nil {
		index := slices.IndexFunc(applier.approvedTargets, func(target configTarget) bool { return target.Name == yard })
		if index < 0 {
			return errors.New("config target is outside the activation plan")
		}
		target := applier.approvedTargets[index]
		unlock, err := lockIntegrationYard(ctx, target.Loaded)
		if err != nil {
			return err
		}
		defer unlock()
		loaded, err := applier.cli.resolveReleaseTransitionContext(ctx, yard, target.Loaded.Context.Paths.ConfigHome)
		if err != nil {
			return err
		}
		fresh := configTarget{Name: yard, Loaded: loaded}
		if integrationConfigurationFingerprint(fresh.Loaded) != integrationConfigurationFingerprint(target.Loaded) {
			return fmt.Errorf("%w: yard %s activation configuration changed", domain.ErrPlanStale, yard)
		}
		// This is the same native config refresh as init --configs, with the
		// approved inputs the release plan observed. It does not author settings.
		platform := applier.cli.initPlatform(target.Loaded, nil)

		return platform.RefreshConfigs(ctx)
	}
	return applier.cli.runReleaseTransitionYardCommandIO(
		ctx,
		yard,
		applier.cli.options.Stdout,
		applier.cli.options.Stderr,
		"init",
		"--configs",
		"--yes",
	)
}

func (*materializedConfigActivationReconciler) ID() string { return "materialized-config" }

type materializedConfigActivationError struct {
	cause                        error
	yard, phase, command, action string
	configHome                   string
}

func (err materializedConfigActivationError) Error() string {
	return fmt.Sprintf("yard %s %s: %v", err.yard, err.phase, err.cause)
}

func (err materializedConfigActivationError) Unwrap() error           { return err.cause }
func (err materializedConfigActivationError) ActivationPhase() string { return err.action }
func (err materializedConfigActivationError) ActivationPendingConfiguration() bool {
	return err.action == "inspect" && pendingIntegrationConfiguration(err.cause)
}

// pendingIntegrationConfiguration requires every cause of an aggregate to be
// pending integration configuration, not only the first errors.As finds.
func pendingIntegrationConfiguration(err error) bool {
	switch typed := err.(type) {
	case nil:
		return false
	case interface{ Unwrap() []error }:
		causes := typed.Unwrap()
		for _, cause := range causes {
			if !pendingIntegrationConfiguration(cause) {
				return false
			}
		}
		return len(causes) != 0
	case interface{ PendingIntegrationConfiguration() bool }:
		return typed.PendingIntegrationConfiguration()
	}
	return pendingIntegrationConfiguration(errors.Unwrap(err))
}

// Keep independent public adapter findings independent at the release boundary.
// Wrapping their aggregate in one diagnostic would expose only its first cause.
func (err materializedConfigActivationError) collected() error {
	var refusals *domain.Refusals
	if !errors.As(err.cause, &refusals) {
		return err
	}
	causes := []error{}
	for _, cause := range refusals.Unwrap() {
		finding := err
		finding.cause = cause
		causes = append(causes, finding.collected())
	}
	return domain.CollectRefusals(causes...)
}

// appendTo adds each collected finding as its own activation refusal.
func (err materializedConfigActivationError) appendTo(refusals []error) []error {
	collected := err.collected()
	var findings *domain.Refusals
	if errors.As(collected, &findings) {
		return append(refusals, findings.Unwrap()...)
	}
	return append(refusals, collected)
}

func (err materializedConfigActivationError) ActivationDiagnostic() (string, string) {
	if errors.Is(err.cause, domain.ErrIncusAccessDenied) {
		return domain.IncusAccessDiagnostic(err.yard)
	}
	retry := fmt.Sprintf("run yard -Y %s %s", err.yard, err.command)
	// Preserve more specific, explicitly public diagnostics from the adapter.
	var diagnostic interface{ ActivationDiagnostic() (string, string) }
	if errors.As(err.cause, &diagnostic) {
		message, next := diagnostic.ActivationDiagnostic()
		if next != "" {
			retry = next
		}
		if err.action == "reconcile" && err.phase == "integrations" {
			retry = integrationActivationRetry(err.cause, retry)
		}
		if message != "" {
			return message, retry
		}
	}
	if source, ok := config.SourceDiagnostic(err.configHome, err.cause); ok {
		if choice := config.SourceAliasRepairRetry(err.configHome, err.yard, err.cause); choice != "" {
			retry = choice
		}
		return fmt.Sprintf("yard %s, %s %s: %s", err.yard, err.phase, err.action, source), retry
	}
	// Only validated yard names and fixed phase/command text cross this boundary.
	message := fmt.Sprintf("yard %s: cannot %s %s for release activation", err.yard, err.action, err.phase)
	var integration *integrationActivationFailure
	if errors.As(err.cause, &integration) {
		message += integration.diagnostic()
	}
	return message, retry
}

// Attribute a failed activation without exposing plans, sources or native errors.
type integrationActivationFailure struct {
	cause      error
	stage      string
	projected  bool
	comparison *integrationComparison
}

type integrationComparison struct {
	fingerprintEqual, scopeEqual, approvedChanged, freshChanged bool
}

func (failure *integrationActivationFailure) Error() string { return failure.cause.Error() }
func (failure *integrationActivationFailure) Unwrap() error { return failure.cause }
func (failure *integrationActivationFailure) ActivationDiagnostic() (string, string) {
	var diagnostic interface{ ActivationDiagnostic() (string, string) }
	if errors.As(failure.cause, &diagnostic) {
		message, retry := diagnostic.ActivationDiagnostic()
		return message, integrationActivationRetry(failure.cause, retry)
	}
	return "", "run yard migrate"
}

func integrationActivationRetry(cause error, retry string) string {
	var conflict interface{ ActivationConflict() bool }
	if reconcileruntime.IntegrationOwnershipConflict(cause) || errors.As(cause, &conflict) && conflict.ActivationConflict() {
		return retry
	}
	if strings.Contains(retry, " integration status") {
		return "run yard migrate"
	}
	return retry
}
func (failure *integrationActivationFailure) diagnostic() string {
	text := fmt.Sprintf(" (stage=%s projected=%t", failure.stage, failure.projected)
	if facts := failure.comparison; facts != nil {
		text += fmt.Sprintf(" fingerprint_equal=%t scope_equal=%t approved_changed=%t fresh_changed=%t plan_stale=%t",
			facts.fingerprintEqual, facts.scopeEqual, facts.approvedChanged, facts.freshChanged, errors.Is(failure.cause, domain.ErrPlanStale))
	}
	return text + ")"
}

func (reconciler *materializedConfigActivationReconciler) operation() *CLI {
	return reconciler.cli.releaseActivationOperation()
}

// coldInstallation is the single native-scope decision of release activation:
// it is recomputed for every operation because init can warm the installation.
func (cli *CLI) coldInstallation() bool {
	options, available, err := cli.mutationGateReleaseOptions()
	if cli.activation != nil {
		request := cli.activation.request
		// A request without its runtime root uses this installation's default.
		if request.RuntimeRoot == "" && err == nil && available {
			request.RuntimeRoot = options.RuntimeRoot
		}
		return coldInstallation(request.ConfigHome, request.RuntimeRoot)
	}
	if err != nil || !available {
		return false
	}
	return coldInstallation(options.ConfigHome, options.RuntimeRoot)
}

func (cli *CLI) releaseActivationOperation(requests ...releasetransition.ProcessRequest) *CLI {
	operation := *cli
	if len(requests) != 0 {
		if operation.activation == nil {
			request := requests[0]
			if request.RuntimeRoot == "" {
				request.RuntimeRoot = cli.env["YARD_RUNTIME_ROOT"]
			}
			checkpoint := activeReleaseRepairCheckpoint(request)
			operation.activation = &releaseConfiguration{request: request, repair: checkpoint != "", complete: checkpoint == releasetransition.JournalComplete, yards: map[string]releaseYardConfiguration{}}
		}
		operation.coldNative = operation.coldInstallation()
		// Binding owners must preserve their ordinary applicability inputs.
		// Yard loaders below already discard resolved caller settings.
		return &operation
	}
	operation.coldNative = operation.coldInstallation()
	environment := operation.freshMigrationEnvironment(
		cli.baseEnv,
		cli.options.RepositoryRoot,
	)
	if operationID := cli.env["SUBYARD_OPERATION_ID"]; operationID != "" {
		environment["SUBYARD_OPERATION_ID"] = operationID
	}
	operation.baseEnv = environment
	operation.env = maps.Clone(environment)
	return &operation
}

func (reconciler *materializedConfigActivationReconciler) Observe(
	ctx context.Context, releases releasetransition.ReleasePair, _ releasetransition.ReleaseLinks,
) (releasetransition.V2ActivationObservation, error) {
	// Another owner needed this activation's deferral first: reuse that observation.
	if kept, ok := reconciler.pendingDeferral().take(releases); ok {
		return kept.observation, kept.err
	}
	return reconciler.observe(ctx, releases, nil)
}

func (reconciler *materializedConfigActivationReconciler) ObserveProspective(
	ctx context.Context, releases releasetransition.ReleasePair, links releasetransition.ReleaseLinks,
	view releasetransition.V2SettingsSnapshotView,
) (releasetransition.V2ActivationObservation, error) {
	if view == nil {
		return reconciler.Observe(ctx, releases, links)
	}
	reconciler.pendingDeferral().take(releases)
	yards, err := view.ListYards()
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	return reconciler.observe(ctx, releases, &config.ProspectiveFiles{Root: reconciler.configHome, ReadSnapshot: view.ReadSnapshot, Yards: yards})
}

func (reconciler *materializedConfigActivationReconciler) observe(
	ctx context.Context, releases releasetransition.ReleasePair, files *config.ProspectiveFiles,
) (releasetransition.V2ActivationObservation, error) {
	if reconciler == nil || reconciler.cli == nil {
		return releasetransition.V2ActivationObservation{}, errors.New("materialized config reconciler is unavailable")
	}
	if err := reconciler.verifyProjectedUnavailable(ctx); err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	yard := reconciler.yard
	if yard == "" {
		yard = "default"
	}
	if !domain.SafeName(yard) {
		return releasetransition.V2ActivationObservation{}, errors.New("materialized config yard is invalid")
	}
	if err := reconciler.resolveScope(releases); err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	if reconciler.allLocal {
		yard = "default"
	}
	operation := reconciler.operation()
	_, targets, yardRefusals, err := operation.tolerantActivationTargets(ctx, yard, reconciler.configHome, reconciler.allLocal, files, true)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, (materializedConfigActivationError{err, yard, "local yard configurations", "config status --all-local", "inspect", reconciler.configHome}).collected()
	}
	activationTargets := make([]config.MaterializationActivationTarget, 0, len(targets))
	consequences, warnings := []string{}, []string{}
	captureIntegrationPlans := reconciler.integrationPlans == nil
	if captureIntegrationPlans {
		reconciler.integrationPlans = map[string]reconcileruntime.IntegrationPlan{}
	}
	// Each yard is observed independently. A refusal skips only that yard's
	// findings that depend on the failed read; every refusal is reported together.
	// A yard whose configuration cannot load is a refusal outside every yard span,
	// so it stays blocking, while the other yards keep their observed drift.
	var refusals []error
	if yardRefusals != nil {
		refusals = materializedConfigActivationError{yardRefusals, yard, "local yard configurations", "config status --all-local", "inspect", reconciler.configHome}.appendTo(refusals)
	}
	spans := make([]yardObservationSpan, 0, len(targets))
	for _, target := range targets {
		spans = append(spans, yardObservationSpan{name: target.Name, refusals: len(refusals), consequences: len(consequences)})
		if err := migration.CheckLegacyGitConfig(ctx, target.Loaded.Context); err != nil {
			refusals = materializedConfigActivationError{err, target.Name, "legacy gitconfig", "config status", "inspect", reconciler.configHome}.appendTo(refusals)
		}
		assessment, assessErr := operation.assessConfigTarget(ctx, target, true)
		if assessErr != nil {
			refusals = materializedConfigActivationError{assessErr, target.Name, "materialized config", "config status", "inspect", reconciler.configHome}.appendTo(refusals)
			continue
		}
		consequences = append(consequences, target.Loaded.IntegrationSelectionExplanation(), target.Loaded.EnvironmentProfileSelectionExplanation())
		var reader func(config.MaterializedAsset) ([]byte, error)
		if files != nil {
			reader = files.ReadAsset
		}
		integrationScope, managedPaths, err := operation.materializedIntegrationScope(target.Loaded, reader)
		if err != nil {
			refusals = materializedConfigActivationError{err, target.Name, "integration scope", "integration status", "inspect", reconciler.configHome}.appendTo(refusals)
			continue
		}
		// A deferred yard keeps its desired configuration and integration scope,
		// like a stopped yard, so deferral never changes the activation's Desired.
		spans[len(spans)-1].deferred = &config.MaterializationActivationTarget{Name: target.Name,
			Assessment: config.MaterializationAssessment{State: "deferred", DesiredFingerprint: assessment.DesiredFingerprint}, IntegrationScope: integrationScope}
		platform := operation.initPlatform(target.Loaded, nil)
		if runtime, ok := platform.(reconcileruntime.Runtime); ok && files != nil {
			runtime.SourceReader = files.ReadAsset
			platform = runtime
		}
		integration := reconcileruntime.IntegrationPlan{}
		if _, ok := platform.(reconcileruntime.Runtime); ok && target.Loaded.Integrations.AllowsCodingTools {
			if assessment.State == "drift" || assessment.State == "converged" {
				platform, _, err = prepareLegacyIntegrationAdoption(ctx, target.Loaded.Integrations, platform)
				if err != nil {
					refusals = materializedConfigActivationError{err, target.Name, "legacy integrations", "integration status", "inspect", reconciler.configHome}.appendTo(refusals)
					continue
				}
				integration, err = platform.(reconcileruntime.Runtime).IntegrationPlan(ctx)
				if err != nil {
					refusals = materializedConfigActivationError{err, target.Name, "integration plan", "integration status", "inspect", reconciler.configHome}.appendTo(refusals)
					continue
				}
				if captureIntegrationPlans {
					reconciler.integrationPlans[target.Name] = integration
				}
			}
		}
		integrationChanged := integration.Changed
		if operation.activation != nil && operation.activation.complete {
			// A failed ordinary init does not reopen the completed release. Keep
			// the full captured plan for any subsequently authorized repair.
			integrationChanged = integration.InstalledChanged()
		}
		// An installed-view repair must not rewrite owned integration state that the
		// pending yard init could not retire: that would discard the operator's change.
		if reconciler.cli.activation != nil && reconciler.cli.activation.repair &&
			integration.Changed {
			if err := operation.pendingInitIntegrationRefusal(ctx, target, reconciler.configHome, files); err != nil {
				refusals = append(refusals, err)
			}
		}
		if integrationChanged {
			if integration.AdoptionFingerprint != "" {
				for _, path := range managedPaths {
					consequences = append(consequences, fmt.Sprintf("yard %s: manage integration path (create if absent, adopt only exact matching legacy state): %s", target.Name, path))
				}
			} else {
				for _, step := range integration.Steps {
					consequences = append(consequences, "yard "+target.Name+": "+step)
				}
			}
			consequences = append(consequences, "yard "+target.Name+": reconcile selected integration packages, configs, links, hooks and services")
		}
		for _, warning := range integration.Warnings {
			warnings = append(warnings, "yard "+target.Name+": "+warning)
		}
		activationTargets = append(activationTargets, config.MaterializationActivationTarget{
			Name: target.Name, Assessment: config.MaterializationAssessment{State: assessment.State, Changed: assessment.Changed, DesiredFingerprint: assessment.DesiredFingerprint, MaterializedFingerprint: assessment.MaterializedFingerprint},
			IntegrationScope: integrationScope, IntegrationFingerprint: integration.Fingerprint, IntegrationChanged: integrationChanged,
		})
	}
	// Only release repair of the active release defers. Any other activation,
	// including a prospective source publication, stops with the findings.
	deferring := reconciler.cli.activation != nil && reconciler.cli.activation.repair && files == nil
	pending, err := reconciler.deferPendingYards(refusals, spans, &activationTargets, &consequences, deferring)
	if err != nil {
		// The yards observed beside the refused ones keep their drift.
		var observed []releasetransition.V2YardAttribution
		for _, target := range activationTargets {
			if target.Assessment.Changed || target.Assessment.State == "drift" || target.IntegrationChanged {
				observed = append(observed, releasetransition.V2YardAttribution{Yard: target.Name, Kind: releasetransition.YardFindingDrift})
			}
		}
		return releasetransition.V2ActivationObservation{}, observedYardsError{err, reconciler.configHome, observed}
	}
	assessed := config.AssessMaterializationActivation(activationTargets)
	// The frozen protocol requires a sorted warning list.
	slices.Sort(warnings)
	var yards []releasetransition.V2YardAttribution
	for _, target := range activationTargets {
		kind := ""
		switch {
		case target.Assessment.State == "deferred" && reconciler.pendingDeferral().deferred(target.Name):
			kind = releasetransition.YardFindingPending
		case target.Assessment.Changed || target.Assessment.State == "drift" || target.IntegrationChanged:
			kind = releasetransition.YardFindingDrift
		}
		if kind != "" {
			yards = append(yards, releasetransition.V2YardAttribution{Yard: target.Name, Kind: kind})
		}
	}
	return releasetransition.V2ActivationObservation{Actual: releasetransition.Fingerprint(assessed.Actual), Desired: releasetransition.Fingerprint(assessed.Desired), Converged: assessed.Converged, Consequences: consequences, Warnings: slices.Compact(warnings), Pending: pending, Yards: yards}, nil
}

// yardObservationSpan marks where one yard's refusals and consequences begin,
// and holds that yard's target as deferral keeps it.
type yardObservationSpan struct {
	name                   string
	refusals, consequences int
	deferred               *config.MaterializationActivationTarget
}

// deferPendingYards decides deferral when every refusal is pending configuration.
// Release repair defers each such yard as a whole, like a stopped yard: it gets
// one finding, its target stays in Desired without drift, and its consequences
// and saved integration plan leave the activation. Any other activation stops
// with the refusals, as before deferral existed.
func (reconciler *materializedConfigActivationReconciler) deferPendingYards(
	refusals []error, spans []yardObservationSpan,
	targets *[]config.MaterializationActivationTarget, consequences *[]string, deferring bool,
) ([]releasetransition.V2PendingConfiguration, error) {
	if len(refusals) == 0 {
		if deferring {
			reconciler.pendingDeferral().record(nil)
		}
		return nil, nil
	}
	// A refusal before the first yard's span belongs to no yard: it is never
	// pending configuration, so it keeps the observation refused.
	if len(spans) == 0 || spans[0].refusals > 0 {
		return nil, collectActivationRefusals(refusals)
	}
	var findings []releasetransition.V2PendingConfiguration
	deferred := map[string]*config.MaterializationActivationTarget{}
	kept := []string{}
	assessed := true
	for index, span := range spans {
		refusalEnd, consequenceEnd := len(refusals), len(*consequences)
		if index+1 < len(spans) {
			refusalEnd, consequenceEnd = spans[index+1].refusals, spans[index+1].consequences
		}
		own := refusals[span.refusals:refusalEnd]
		if len(own) == 0 {
			kept = append(kept, (*consequences)[span.consequences:consequenceEnd]...)
			continue
		}
		finding, pending := pendingYardFinding(span.name, own)
		if !pending {
			return nil, collectActivationRefusals(refusals)
		}
		findings = append(findings, finding)
		deferred[span.name] = span.deferred
		assessed = assessed && span.deferred != nil
	}
	if !deferring {
		return nil, collectActivationRefusals(refusals)
	}
	if !assessed || len(findings) > releasetransition.MaxPlanItems {
		// A yard refused before its assessment has no desired configuration to
		// keep, and the protocol bounds the findings it can report.
		return nil, pendingBesideDrift{collectActivationRefusals(refusals), "run yard migrate --check"}
	}
	*targets = slices.DeleteFunc(*targets, func(target config.MaterializationActivationTarget) bool {
		_, found := deferred[target.Name]
		return found
	})
	names := make(map[string]bool, len(deferred))
	for name, target := range deferred {
		*targets = append(*targets, *target)
		names[name] = true
		delete(reconciler.integrationPlans, name)
	}
	*consequences = kept
	reconciler.pendingDeferral().record(names)
	return findings, nil
}

// pendingYardFinding is the one finding of a deferred yard: its refusals in
// observation order behind the yard's name. Its next step is the yard's own
// init only when every refusal names it, otherwise the first other safe step
// that is no yard's init, else the yard's integration status. A text beyond the
// protocol bounds becomes the count of the yard's refusals.
func pendingYardFinding(yard string, refusals []error) (releasetransition.V2PendingConfiguration, bool) {
	items, pending := activationPendingFindings(collectActivationRefusals(refusals))
	if !pending {
		return releasetransition.V2PendingConfiguration{}, false
	}
	initStep := shellquote.Command([]string{"yard", "-Y", yard, "init"})
	finding := releasetransition.V2PendingConfiguration{Retry: initStep}
	other := ""
	messages := make([]string, len(items))
	for index, item := range items {
		messages[index] = item.Message
		if item.Retry == initStep {
			continue
		}
		finding.Retry = "run yard -Y " + yard + " integration status"
		_, named := pendingInitYards(&releasetransition.Inspection{Blockers: []releasetransition.Blocker{{Retry: item.Retry}}})
		if other == "" && !named && (releasetransition.V2PendingConfiguration{Message: "next step", Retry: item.Retry}).Validate() == nil {
			other = item.Retry
		}
	}
	if finding.Retry != initStep && other != "" {
		finding.Retry = other
	}
	finding.Message = strings.Join(messages, "; ")
	if !strings.HasPrefix(finding.Message, "yard "+yard+":") {
		finding.Message = "yard " + yard + ": " + finding.Message
	}
	if finding.Validate() != nil {
		finding.Message = fmt.Sprintf("yard %s: %d pending configuration findings; full details: yard -Y %s integration status", yard, len(items), yard)
	}
	return finding, true
}

// pendingBesideDrift keeps pending findings visible when they cannot be
// deferred beside other work. It is not pending configuration, so it admits no
// ordinary command, and it names the admitted next step.
type pendingBesideDrift struct {
	refusals error
	retry    string
}

func (err pendingBesideDrift) Error() string                    { return err.refusals.Error() }
func (err pendingBesideDrift) Unwrap() error                    { return err.refusals }
func (pendingBesideDrift) ActivationPendingConfiguration() bool { return false }
func (err pendingBesideDrift) ActivationDiagnostic() (string, string) {
	message := "release activation cannot be observed safely"
	if diagnostic, ok := err.refusals.(interface{ ActivationDiagnostic() (string, string) }); ok {
		message, _ = diagnostic.ActivationDiagnostic()
	}
	return message, err.retry
}

// pendingYardDeferral is the one record of yards deferred for pending
// configuration in an activation. The materialized-config owner decides it on
// each repair observation; every yard-scoped activation owner leaves those
// yards untouched. A deciding observation another owner needs first is kept
// for the materialized-config owner's next observation, so it runs once.
type pendingYardDeferral struct {
	decide   sync.Mutex
	mutex    sync.Mutex
	known    bool
	yards    map[string]bool
	releases releasetransition.ReleasePair
	observe  func(context.Context, releasetransition.ReleasePair) (releasetransition.V2ActivationObservation, error)
	kept     *keptActivationObservation
}

type keptActivationObservation struct {
	releases    releasetransition.ReleasePair
	observation releasetransition.V2ActivationObservation
	err         error
}

func (reconciler *materializedConfigActivationReconciler) pendingDeferral() *pendingYardDeferral {
	if reconciler.deferral == nil {
		reconciler.deferral = &pendingYardDeferral{}
	}
	return reconciler.deferral
}

func (deferral *pendingYardDeferral) record(yards map[string]bool) {
	deferral.mutex.Lock()
	defer deferral.mutex.Unlock()
	deferral.known, deferral.yards = true, maps.Clone(yards)
}

func (deferral *pendingYardDeferral) deferred(yard string) bool {
	deferral.mutex.Lock()
	defer deferral.mutex.Unlock()
	return deferral.yards[yard]
}

// take returns the kept deciding observation for these releases once, and
// discards any other.
func (deferral *pendingYardDeferral) take(releases releasetransition.ReleasePair) (*keptActivationObservation, bool) {
	deferral.mutex.Lock()
	defer deferral.mutex.Unlock()
	kept := deferral.kept
	deferral.kept = nil
	return kept, kept != nil && kept.releases == releases
}

// decided returns the deferred yards. When this activation has no decision yet,
// it first runs the deciding observation once, for releases or else those of
// the latest owner observation; a failed one defers nothing.
func (deferral *pendingYardDeferral) decided(ctx context.Context, releases releasetransition.ReleasePair) map[string]bool {
	deferral.decide.Lock()
	defer deferral.decide.Unlock()
	deferral.mutex.Lock()
	if releases == (releasetransition.ReleasePair{}) {
		releases = deferral.releases
	} else {
		deferral.releases = releases
	}
	known, observe := deferral.known, deferral.observe
	deferral.mutex.Unlock()
	if !known && observe != nil {
		observation, err := observe(ctx, releases)
		deferral.mutex.Lock()
		deferral.known, deferral.kept = true, &keptActivationObservation{releases, observation, err}
		deferral.mutex.Unlock()
	}
	deferral.mutex.Lock()
	defer deferral.mutex.Unlock()
	return maps.Clone(deferral.yards)
}

// activationPendingFindings reports whether every cause of a refusal is pending
// configuration, and returns its findings.
func activationPendingFindings(err error) ([]releasetransition.V2PendingConfiguration, bool) {
	switch typed := err.(type) {
	case nil:
		return nil, false
	case interface{ Unwrap() []error }:
		causes := typed.Unwrap()
		var findings []releasetransition.V2PendingConfiguration
		for _, cause := range causes {
			items, pending := activationPendingFindings(cause)
			if !pending {
				return nil, false
			}
			findings = append(findings, items...)
		}
		return findings, len(causes) != 0
	case interface{ ActivationPendingConfiguration() bool }:
		if !typed.ActivationPendingConfiguration() {
			return nil, false
		}
		var diagnostic interface{ ActivationDiagnostic() (string, string) }
		finding := releasetransition.V2PendingConfiguration{Message: "pending configuration awaits yard init"}
		if errors.As(err, &diagnostic) {
			finding.Message, finding.Retry = diagnostic.ActivationDiagnostic()
		}
		return []releasetransition.V2PendingConfiguration{finding}, true
	}
	return activationPendingFindings(errors.Unwrap(err))
}

// activationRefusals keeps every independent per-yard refusal of one activation
// owner. The release boundary publishes one bounded diagnostic and one retry.
type activationRefusals struct{ causes []error }

func collectActivationRefusals(causes []error) error {
	switch len(causes) {
	case 0:
		return nil
	case 1:
		return causes[0]
	}
	return &activationRefusals{causes: slices.Clone(causes)}
}

func (refusals *activationRefusals) Error() string {
	messages := make([]string, len(refusals.causes))
	for index, cause := range refusals.causes {
		messages[index] = cause.Error()
	}
	return strings.Join(messages, "; ")
}

func (refusals *activationRefusals) Unwrap() []error { return refusals.causes }

// ActivationDiagnostic lists each public refusal in observation order and retries
// the first. Over the protocol text bound, it keeps the first refusal, names how
// many were omitted and the status views that report them in full.
func (refusals *activationRefusals) ActivationDiagnostic() (string, string) {
	const limit, firstBudget = 512, 160
	messages := make([]string, 0, len(refusals.causes))
	actions := make([]string, 0, len(refusals.causes))
	for _, cause := range refusals.causes {
		message, action := "release activation cannot be observed safely", "run yard update --check"
		var diagnostic interface{ ActivationDiagnostic() (string, string) }
		if errors.As(cause, &diagnostic) {
			message, action = diagnostic.ActivationDiagnostic()
		}
		messages = append(messages, message)
		actions = append(actions, action)
	}
	retry := actions[0]
	if joined := strings.Join(messages, "; "); len(joined) <= limit {
		return joined, retry
	}
	omitted := len(messages) - 1
	const separator = "; first: "
	// The first refusal keeps at least firstBudget characters; status views take
	// only the remaining space and an oversized first retry is left to the action.
	head := fmt.Sprintf("%d release activation refusals, %d omitted; complete detail: %s", len(messages), omitted, retry)
	if len(head)+len(separator)+firstBudget > limit {
		head = fmt.Sprintf("%d release activation refusals, %d omitted", len(messages), omitted)
	} else {
		for index, action := range actions[1:] {
			if slices.Contains(actions[:index+1], action) {
				continue
			}
			rest := fmt.Sprintf(", and %d more", omitted-index)
			if len(head)+len(", ")+len(action)+len(rest)+len(separator)+firstBudget > limit {
				head += rest
				break
			}
			head += ", " + action
		}
	}
	prefix := head + separator
	first := messages[0]
	if budget := limit - len(prefix); len(first) > budget {
		end := budget - len("...")
		for end > 0 && !utf8.RuneStart(first[end]) {
			end--
		}
		first = strings.TrimSpace(first[:end]) + "..."
	}
	return prefix + first, retry
}

func (cli *CLI) appliedIntegrationTarget(ctx context.Context, target configTarget) (configTarget, error) {
	applied, _, err := cli.appliedIntegrationTargetBaseline(ctx, target)
	return applied, err
}

// appliedIntegrationTargetBaseline also reports whether the running yard records
// an applied settings baseline; without one its installed settings are unknown.
func (cli *CLI) appliedIntegrationTargetBaseline(ctx context.Context, target configTarget) (configTarget, bool, error) {
	observer, ok := cli.initPlatform(target.Loaded, nil).(interface {
		AppliedIntegrationConfiguration(context.Context) (reconcileruntime.AppliedIntegrationConfiguration, error)
	})
	if !ok {
		return target, false, nil
	}
	incus, _ := cli.statusPorts()
	instance, err := incus.Instance(ctx, target.Loaded.Context.IncusProject, target.Loaded.Context.YardInstanceName)
	if err != nil || !strings.EqualFold(instance.Status, "running") {
		return target, false, nil // The ordinary assessment owns absent/stopped/read failures.
	}
	if err := domain.NativeTargetOwnership(target.Loaded.Context, instance.Config); err != nil {
		return target, false, err
	}
	applied, err := observer.AppliedIntegrationConfiguration(ctx)
	if err != nil {
		return target, false, err
	}
	baseline := applied.Settings != nil
	target.installed = &installedConfigurationEvidence{applied: applied, known: map[string]bool{}}
	for name := range applied.Identity {
		target.installed.known[name] = true
	}
	if applied.Installed {
		target.installed.known["CODING_TOOL_INTEGRATIONS"] = true
	}
	loaded := target.Loaded
	if err := loaded.Catalog.ValidateIntegrationInitSettings(applied.Settings); err != nil {
		return target, false, err
	}
	if applied.Installed {
		loaded.Environment = loaded.Catalog.WithAppliedIntegrationInitSettings(loaded.Environment, applied.Settings)
	} else {
		loaded.Environment = maps.Clone(loaded.Environment)
	}
	for name, value := range applied.Identity {
		loaded.Environment[name] = value
	}
	if user, ok := applied.Identity["DEV_USER"]; ok {
		loaded.Context.DevUser = user
		loaded.Context.DevUID, _ = strconv.Atoi(applied.Identity["DEV_UID"])
	}
	if value, ok := applied.Settings["DEV_USER"]; ok {
		loaded.Context.DevUser = value
	}
	if value, ok := applied.Settings["DEV_UID"]; ok {
		loaded.Context.DevUID, _ = strconv.Atoi(value)
	}
	if profiles, ok := observer.(interface {
		AppliedEnvironmentProfiles(context.Context) ([]string, bool, error)
	}); ok {
		selected, known, err := profiles.AppliedEnvironmentProfiles(ctx)
		if err != nil {
			return target, false, err
		}
		if known {
			// This is passive installed-state inspection, not a new profile
			// selection. Retained disabled profiles must remain observable.
			target.installed.known["ENVIRONMENT_PROFILES"] = true
			loaded.Environment["ENVIRONMENT_PROFILES"] = strings.Join(selected, " ")
			loaded.EnvironmentProfiles = config.EnvironmentProfileSelection{Present: true, Requested: slices.Clone(selected), Effective: slices.Clone(selected)}
		}
	}
	if !applied.Installed {
		target.Loaded = loaded
		return target, baseline, nil
	}
	loaded, err = config.WithIntegrationSelection(loaded, applied.Selected)
	for _, name := range []string{"HOST_LINKS", "INTEGRATION_HOST_LINKS"} {
		if value, ok := applied.Settings[name]; ok && err == nil {
			loaded.Environment[name] = value
		}
	}

	if err == nil {
		target.Loaded = loaded
	}
	return target, baseline, err
}

func (reconciler *materializedConfigActivationReconciler) resolveScope(
	releases releasetransition.ReleasePair,
) error {
	if reconciler.scopeResolved {
		return nil
	}
	var err error
	reconciler.allLocal, err = reconciler.cli.activation.materializedScope(reconciler.cli, releases,
		reconciler.configHome, reconciler.goal, reconciler.artifactDigest, reconciler.registryDigest, reconciler.sourceIngress)
	reconciler.scopeResolved = err == nil
	return err
}

func (reconciler *materializedConfigActivationReconciler) UseSourceStableActivationScope() func() {
	if reconciler == nil || (reconciler.allLocal && reconciler.scopeResolved) {
		return nil
	}
	allLocal, resolved := reconciler.allLocal, reconciler.scopeResolved
	reconciler.allLocal, reconciler.scopeResolved = true, true
	// Plans captured for selected scope cannot authorize additional targets.
	reconciler.integrationPlans = nil
	reconciler.pendingDeferral().take(releasetransition.ReleasePair{})
	return func() {
		reconciler.allLocal, reconciler.scopeResolved = allLocal, resolved
		reconciler.integrationPlans = nil
	}
}

func releaseSourceMigrationsComplete(
	repositoryRoot string,
	store *releasetransition.POSIXV2Store,
) (bool, error) {
	registry, state, err := releaseSourceMigrationState(repositoryRoot, store)
	if err != nil || !state.AuthoritySnapshot.Exists {
		return false, err
	}
	pending, err := registry.PendingPath(state.Ledger)
	return len(pending) == 0 && err == nil, err
}

func releaseCheckpointPublicationPending(repositoryRoot string, store *releasetransition.POSIXV2Store) (bool, error) {
	capability, err := os.ReadFile(filepath.Join(repositoryRoot, "config", "release-checkpoint.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := releasetransition.ValidateMigrationCheckpointCapability(capability); err != nil {
		return false, err
	}
	_, state, err := releaseSourceMigrationState(repositoryRoot, store)
	return state.RequiresCheckpointPublication(), err
}

func releaseSourceMigrationState(
	repositoryRoot string,
	store *releasetransition.POSIXV2Store,
) (registry releasetransition.RegistryV2, state releasetransition.MigrationLedgerSnapshot, err error) {
	projection, err := store.ReadLedger()
	if err != nil {
		return registry, state, err
	}
	checkpoint, err := store.ReadMigrationCheckpoint()
	if err != nil || (!projection.Exists && !checkpoint.Exists) {
		return registry, state, err
	}
	payload, err := os.ReadFile(filepath.Join(repositoryRoot, "config", "release-transition.json"))
	if err != nil {
		return registry, state, err
	}
	registry, digest, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil {
		return registry, state, err
	}
	state, err = releasetransition.ParseMigrationLedger(projection, checkpoint, registry, digest)
	return registry, state, err
}

func (reconciler *materializedConfigActivationReconciler) reconcileCLI() *CLI {
	operation := reconciler.operation()
	if operation.options.Config == nil {
		operation.options.DispatcherPath = filepath.Join(
			operation.options.RepositoryRoot, "bin", "yard-engine",
		)
		operation.options.Config = releaseTransitionConfigApplier{cli: operation}
	}
	return operation
}

func (reconciler *materializedConfigActivationReconciler) PrepareActivationRecovery(
	ctx context.Context, releases releasetransition.ReleasePair, links releasetransition.ReleaseLinks,
) (releasetransition.Fingerprint, error) {
	// Recapture native ownership plans from actual state, including any known
	// partial integration apply. Unknown ownership remains an adapter failure.
	reconciler.integrationPlans = nil
	reconciler.pendingDeferral().take(releases)
	if _, err := reconciler.Observe(ctx, releases, links); err != nil {
		return "", err
	}
	operation := reconciler.reconcileCLI()
	loaded, err := operation.resolveReleaseTransitionContext(ctx, "default", reconciler.configHome, nil)
	if err != nil {
		return "", (materializedConfigActivationError{err, "default", "configuration", "config status", "inspect", reconciler.configHome}).collected()
	}
	targets, err := reconciler.configTargets(ctx, operation, loaded, true)
	if err != nil {
		return "", err
	}
	selector := func() ([]configTarget, error) { return reconciler.configTargets(ctx, operation, loaded, true) }
	execution, err := operation.prepareConfigApply(ctx, targets, selector)
	if err != nil {
		return "", err
	}
	reconciler.recoveryApply = execution
	return activationStageFingerprint(struct {
		Configs      []domain.OperationStep
		Integrations map[string]string
	}{execution.steps(), integrationRecoveryBindings(reconciler.integrationPlans)})
}

func integrationRecoveryBindings(plans map[string]reconcileruntime.IntegrationPlan) map[string]string {
	bindings := make(map[string]string, len(plans))
	for yard, plan := range plans {
		bindings[yard] = plan.StateBinding()
	}
	return bindings
}

func (reconciler *materializedConfigActivationReconciler) Reconcile(
	ctx context.Context,
	_ releasetransition.ReleaseLinks,
) error {
	if reconciler == nil || reconciler.cli == nil {
		return errors.New("materialized config reconciler is unavailable")
	}
	reconciler.pendingDeferral().take(releasetransition.ReleasePair{})

	operation := reconciler.reconcileCLI()
	yard := reconciler.yard
	if yard == "" || reconciler.allLocal {
		yard = "default"
	}
	loaded, err := operation.resolveReleaseTransitionContext(ctx, yard, reconciler.configHome, nil)
	if err != nil {
		return (materializedConfigActivationError{err, yard, "configuration", "config status", "apply", reconciler.configHome}).collected()
	}
	targets, err := reconciler.configTargets(ctx, operation, loaded, reconciler.allLocal)
	if err != nil {
		return err
	}
	if reconciler.recoveryApply != nil {
		if err := operation.refreshConfigApply(ctx, reconciler.recoveryApply); err != nil {
			return err
		}
	}
	// Enroll only the exact legacy state shown in the top-level release plan,
	// before the ordinary config refresh can replace any unowned bytes.
	for _, target := range targets {
		if err := reconciler.reconcileIntegrationTarget(ctx, operation, target); err != nil {
			return (materializedConfigActivationError{err, target.Name, "integrations", "integration status", "reconcile", reconciler.configHome}).collected()
		}
	}
	selector := func() ([]configTarget, error) {
		return reconciler.configTargets(ctx, operation, loaded, reconciler.allLocal)
	}
	execution := reconciler.recoveryApply
	if execution == nil {
		execution, err = operation.prepareConfigApply(ctx, targets, selector)
		if err != nil {
			return err
		}
	}
	if applier, ok := operation.options.Config.(releaseTransitionConfigApplier); ok {
		applier.approvedTargets = targets
		operation.options.Config = applier
	}
	return operation.executeConfigApply(ctx, execution, execution.steps(), operation.options.Stdout)
}

// configTargets selects the same durable configuration view for assessment,
// native integration effects, materialized refresh and every freshness check.
// Yards the latest observation deferred are left out, so no effect reaches them.
func (reconciler *materializedConfigActivationReconciler) configTargets(ctx context.Context, operation *CLI, loaded config.Loaded, allLocal bool) ([]configTarget, error) {
	_, targets, err := operation.releaseActivationTargets(ctx, loaded.Context.YardName, reconciler.configHome, allLocal, nil)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(targets, func(target configTarget) bool { return reconciler.pendingDeferral().deferred(target.Name) }), nil
}

func (cli *CLI) resolveReleaseTransitionContext(
	ctx context.Context,
	yard string,
	configHome string,
	files ...*config.ProspectiveFiles,
) (config.Loaded, error) {
	if cli.activation != nil {
		return cli.activation.load(ctx, cli, yard, configHome, files...)
	}
	return cli.loadReleaseTransitionContext(yard, configHome, nil, files...)
}

// discoverReleaseContext loads a yard's context without any guest read. When a
// desired provisioning source is missing it returns a bootstrap context whose
// placeholder sources only locate the yard, with those placeholders named in
// sources; sources is nil when no such discovery is possible.
func (cli *CLI) discoverReleaseContext(yard, home string) (config.Loaded, config.Loaded, map[string]string, error) {
	loaded, originalErr := cli.loadReleaseTransitionContext(yard, home, nil)
	if originalErr == nil {
		return loaded, loaded, nil, nil
	}
	sources := map[string]string{}
	bootstrap, err := loaded, originalErr
	for err != nil {
		var source *config.SourceError
		if !errors.As(err, &source) || !cli.catalog.IntegrationInitSetting(source.Key) ||
			!strings.HasPrefix(source.Key, "AGENT_") || !strings.HasSuffix(source.Key, "_PROVISION") {
			return config.Loaded{}, config.Loaded{}, nil, originalErr
		}
		if _, seen := sources[source.Key]; seen {
			return config.Loaded{}, config.Loaded{}, nil, originalErr
		}
		sources[source.Key] = filepath.Join(cli.options.RepositoryRoot, "config", "commands.registry")
		bootstrap, err = cli.loadReleaseTransitionContext(yard, home, sources)
	}
	return loaded, bootstrap, sources, originalErr
}

// Only installed-view repairs may replace desired provisioning sources before
// strict loading. An inert, already-required file lets context discovery locate
// the installed receipt without opening unavailable desired provisioning inputs;
// the final load validates the recorded sources, or retains the original error
// when no applied baseline is available. Ordinary loading is always strict.
func (cli *CLI) resolveInstalledReleaseContext(ctx context.Context, yard, home string) (config.Loaded, error) {
	loaded, bootstrap, sources, originalErr := cli.discoverReleaseContext(yard, home)
	if originalErr == nil {
		return loaded, nil
	}
	if sources == nil {
		return config.Loaded{}, originalErr
	}
	installed, known, err := cli.appliedIntegrationTargetBaseline(ctx, configTarget{Name: yard, Loaded: bootstrap})
	if err != nil {
		return config.Loaded{}, err
	}
	if !known {
		return config.Loaded{}, originalErr
	}
	for name := range sources {
		if source := installed.Loaded.Environment[name]; source != "" {
			sources[name] = source
		}
	}
	loaded, err = cli.loadReleaseTransitionContext(yard, home, sources)
	// Discovery placeholders are never consumer inputs. The ordinary installed
	// projection restores recorded values before observation or application.
	for name := range sources {
		if loaded.Environment[name] == filepath.Join(cli.options.RepositoryRoot, "config", "commands.registry") {
			delete(loaded.Environment, name)
		}
	}
	return loaded, err
}

func (cli *CLI) releaseActivationTargets(ctx context.Context, yard, home string, allLocal bool, files *config.ProspectiveFiles) (config.Loaded, []configTarget, error) {
	loaded, err := cli.resolveReleaseTransitionContext(ctx, yard, home, files)
	if err != nil {
		return loaded, nil, err
	}
	targets, addresses, err := cli.configTargets(ctx, loaded, allLocal, true)
	if !allLocal {
		return loaded, targets, domain.CollectRefusals(err, cli.checkNativeTargets(loaded, false, nil))
	}
	for _, target := range targets {
		addresses = append(addresses, target.Loaded)
	}
	return loaded, targets, domain.CollectRefusals(err, config.NativeTargetCollisions(addresses, nil, ""))
}

func (cli *CLI) loadReleaseTransitionContext(yard, configHome string, sources map[string]string, files ...*config.ProspectiveFiles) (config.Loaded, error) {
	environment := cli.freshMigrationEnvironment(cli.baseEnv, cli.options.RepositoryRoot)
	maps.Copy(environment, sources)
	if configHome != "" {
		environment["SUBYARD_CONFIG_HOME"] = configHome
	}
	operatorHome := environment["SUBYARD_OPERATOR_HOME"]
	if operatorHome == "" {
		operatorHome = environment["HOME"]
	}
	options := config.LoadOptions{
		Catalog: &cli.catalog, RepositoryRoot: cli.options.RepositoryRoot,
		OperatorHome: operatorHome,
		YardName:     yard,
		Environment:  environment,
	}
	if len(files) != 0 {
		options.ProspectiveFiles = files[0]
	}
	loaded, err := config.Load(options)
	if err == nil || yard != testyardmigration.LegacyYard ||
		!releaseTransitionRegistrationAbsent(configHome, yard) {
		return loaded, err
	}
	options.YardName = testyardmigration.CurrentYard
	return config.Load(options)
}

func releaseTransitionRegistrationAbsent(configHome, yard string) bool {
	if configHome == "" || !filepath.IsAbs(configHome) || !domain.SafeName(yard) {
		return false
	}
	for _, path := range []string{
		filepath.Join(configHome, "yards", yard, "config.env"),
		filepath.Join(configHome, "yards", yard+".env"),
	} {
		snapshot, err := config.ReadPersistentFileSnapshot(configHome, path)
		if err != nil || snapshot.Exists {
			return false
		}
	}
	return true
}

func executeReleaseTransitionRequest(
	ctx context.Context,
	repositoryRoot string,
	request releasetransition.ProcessRequest,
	verifyAuthorization func(releasetransition.PlanToken, releasetransition.Authorization) bool,
	reconcilers []releasetransition.V2ActivationReconciler,
	ownerRegistration releasetransition.V2OwnerRegistration,
	ingressFactory func(releasetransition.ReleasePair) (releasetransition.V2Ingress, error),
	recoveryRequests ...*releasetransition.ActivationOnlyRecoveryRequest,
) (releasetransition.ProcessResponse, error) {
	if request.SchemaVersion != releasetransition.ProcessProtocolSchemaV1 ||
		(request.Mode != releasetransition.ProcessInspect && request.Mode != releasetransition.ProcessConverge) {
		return releasetransition.ProcessResponse{}, errors.New("unsupported release transition process request")
	}
	if request.SourceIngress != nil {
		if err := request.SourceIngress.Validate(); err != nil {
			return releasetransition.ProcessResponse{}, fmt.Errorf(
				"source ingress descriptor is invalid: %w", err,
			)
		}
	}
	if !filepath.IsAbs(repositoryRoot) || !filepath.IsAbs(request.RuntimeRoot) ||
		!filepath.IsAbs(request.ConfigHome) {
		return releasetransition.ProcessResponse{}, errors.New("release transition roots must be absolute")
	}
	var rollbackTarget *releasetransition.RollbackTarget
	if request.Direction == releasetransition.DirectionActivatePrevious {
		var err error
		rollbackTarget, err = releaseruntime.VerifyRollbackTarget(ctx, request.RuntimeRoot, request.Target, request.ArtifactDigest)
		if err != nil {
			return releasetransition.ProcessResponse{}, err
		}
	}
	registry, err := os.ReadFile(filepath.Join(repositoryRoot, "config", "release-transition.json"))
	if err != nil {
		return releasetransition.ProcessResponse{}, fmt.Errorf("read release transition registry: %w", err)
	}
	if request.RegistryDigest != "" {
		digest := sha256.Sum256(registry)
		if releasetransition.Fingerprint(hex.EncodeToString(digest[:])) != request.RegistryDigest {
			return releasetransition.ProcessResponse{},
				errors.New("release transition registry does not match the verified release manifest")
		}
	}
	links, err := releasetransition.NewRuntimeLinkStore(request.RuntimeRoot)
	if err != nil {
		return releasetransition.ProcessResponse{}, err
	}
	observed, err := links.Observe()
	if err != nil {
		return releasetransition.ProcessResponse{}, err
	}
	releases := releasetransition.ReleasePair{
		From: observed.Active, Previous: observed.Previous, Target: request.Target,
	}
	response := releasetransition.ProcessResponse{
		SchemaVersion:                 releasetransition.ProcessProtocolSchemaV1,
		ActivationReconciliationOwned: len(reconcilers) != 0,
	}
	var ingress releasetransition.V2Ingress
	if ingressFactory != nil {
		ingress, err = ingressFactory(releases)
		if err != nil {
			return releasetransition.ProcessResponse{}, err
		}
	}
	transitionOptions := candidateTransitionOptions(request, releasetransition.V2Options{
		ConfigHome: request.ConfigHome, Releases: releases, Direction: request.Direction,
		OperatorHome:       os.Getenv("SUBYARD_OPERATOR_HOME"),
		RecoveryContract:   request.RecoveryContract,
		CandidateConfigDir: filepath.Join(repositoryRoot, "config"),
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
			return links.Observe()
		},
		ActivateLinks: func(_ context.Context, pair releasetransition.ReleasePair) (releasetransition.ReleaseLinks, error) {
			return links.Activate(pair)
		},
		Reconcilers: reconcilers, OwnerRegistration: ownerRegistration, Ingress: ingress,
		RegistryPayload:     registry,
		ArtifactDigest:      request.ArtifactDigest,
		RollbackTarget:      rollbackTarget,
		InheritedSettingIDs: request.InheritedSettingIDs,
		VerifyAuthorization: verifyAuthorization,
	})
	if owner, ok := ownerRegistration.(interface {
		registeredMigrationOwners() ([]releasetransition.V2ActivationReconciler, []releasetransition.V2ActivationReconciler, []string)
	}); ok {
		transitionOptions.RegisteredYards, transitionOptions.GuestDiskTmp, transitionOptions.RegisteredEndpointSettings = owner.registeredMigrationOwners()
	}
	transitionOptions.MigrationOwnerReplacement = request.MigrationOwnerReplacement
	if request.MigrationOwnerReplacement != nil {
		transitionOptions.MigrationOwnerAssets = func(ctx context.Context) (releasetransition.MigrationOwnerAssetsProof, error) {
			return releaseruntime.VerifyMigrationOwnerReplacementAssets(ctx, repositoryRoot, request)
		}
	}
	transitionOptions.CompletedMetadata = func(ctx context.Context) (releasetransition.Fingerprint, error) {
		runtime := releaseruntime.NewSourceInspector(repositoryRoot, io.Discard)
		defer runtime.Close()
		proof, err := runtime.InspectSourceAdmission(ctx, request.RuntimeRoot, request.ConfigHome)
		if err != nil {
			// Unsupported new admission leaves the existing ordinary/historical
			// compatibility path intact. Its absence cannot satisfy NEW admission.
			return "", nil
		}
		return releasetransition.Fingerprint(proof.Binding), nil
	}
	checkpointCapability, capabilityErr := os.ReadFile(filepath.Join(repositoryRoot, "config", "release-checkpoint.json"))
	if capabilityErr == nil {
		if err := releasetransition.ValidateMigrationCheckpointCapability(checkpointCapability); err != nil {
			return releasetransition.ProcessResponse{}, err
		}
		transitionOptions.MigrationCheckpoint = true
	} else if !errors.Is(capabilityErr, os.ErrNotExist) {
		return releasetransition.ProcessResponse{}, capabilityErr
	}
	if request.Direction == releasetransition.DirectionActivateTarget {
		store, storeErr := releasetransition.NewPOSIXV2Store(request.ConfigHome)
		if storeErr != nil {
			return releasetransition.ProcessResponse{}, storeErr
		}
		checkpoint, checkpointErr := store.ReadMigrationCheckpoint()
		if checkpointErr != nil {
			return releasetransition.ProcessResponse{}, checkpointErr
		}
		if checkpoint.Exists {
			transitionOptions.SourceMigrationCheckpoint, err = releaseruntime.VerifyMigrationCheckpointReader(
				ctx, request.RuntimeRoot, releases.From,
			)
			if err != nil {
				return releasetransition.ProcessResponse{}, err
			}
		}
	}
	transition, err := releasetransition.NewV2Transition(transitionOptions)
	if err != nil {
		if errors.Is(err, releasetransition.ErrRegistryInvalid) {
			response.Outcome = &releasetransition.Outcome{
				Status: releasetransition.StatusOperatorActionRequired,
				Active: observed.Active, Previous: observed.Previous, Target: request.Target,
				Code:    releasetransition.CodeRegistryInvalid,
				Message: "the candidate release transition registry is invalid",
				Retry:   "install a release with a valid transition registry, then run yard update --check",
			}
			return response, nil
		}
		return releasetransition.ProcessResponse{}, err
	}
	goal := releasetransition.Goal{Target: request.Target, Direction: request.Direction}
	switch request.Mode {
	case releasetransition.ProcessInspect:
		if request.Execution != nil {
			return releasetransition.ProcessResponse{}, errors.New("inspect request contains an execution")
		}
		var inspection releasetransition.Inspection
		var inspectErr error
		if len(recoveryRequests) != 0 && recoveryRequests[0] != nil {
			inspection, inspectErr = transition.InspectActivationRecovery(ctx, *recoveryRequests[0])
		} else {
			inspection, inspectErr = transition.InspectProcessV1(ctx, goal)
		}
		if inspectErr != nil {
			return releasetransition.ProcessResponse{}, inspectErr
		}
		// V1 has no read-only rollback-compatibility assessment. Its existing
		// standalone blocked outcome preserves the failure without granting a plan.
		if inspection.Outcome != nil && inspection.Outcome.Code == releasetransition.CodeRollbackIncompatible &&
			inspection.Outcome.Status == releasetransition.StatusOperatorActionRequired {
			response.Outcome = inspection.Outcome
		} else {
			response.Inspection = &inspection
		}
	case releasetransition.ProcessConverge:
		if request.Execution == nil {
			return releasetransition.ProcessResponse{}, errors.New("converge request has no execution")
		}
		var outcome releasetransition.Outcome
		var convergeErr error
		if len(recoveryRequests) != 0 && recoveryRequests[0] != nil {
			outcome, convergeErr = transition.ConvergeActivationRecovery(ctx, *recoveryRequests[0], *request.Execution)
		} else {
			outcome, convergeErr = transition.Converge(ctx, *request.Execution)
		}
		if convergeErr != nil {
			return releasetransition.ProcessResponse{}, convergeErr
		}
		response.Outcome = &outcome
	}
	return response, nil
}

func candidateTransitionOptions(
	request releasetransition.ProcessRequest,
	options releasetransition.V2Options,
) releasetransition.V2Options {
	options.SourceIngress = request.SourceIngress
	options.Replacement = request.Replacement
	options.CandidateVersion = Version
	return options
}

func readReleaseTransitionGrant() (releasetransition.Authorization, error) {
	const grantFD = 3
	if os.Getenv("SUBYARD_RELEASE_TRANSITION_GRANT_FD") != "3" {
		return "", nil
	}
	file := os.NewFile(grantFD, "release-transition-authorization")
	if file == nil {
		return "", nil
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil {
		return "", err
	}
	if len(payload) == 0 {
		return "", nil
	}
	if len(payload) > 1024 || payload[len(payload)-1] != '\n' {
		return "", errors.New("authorization grant is invalid")
	}
	payload = payload[:len(payload)-1]
	for _, value := range payload {
		if value < 0x21 || value > 0x7e {
			return "", errors.New("authorization grant is invalid")
		}
	}
	return releasetransition.Authorization(payload), nil
}

// verifiedNativeMigrations reads this transaction's own durable evidence. Native
// migrations run after the plan's approval and publish instance metadata, so their
// verified steps explain exactly that change. Unavailable evidence explains nothing.
func (reconciler *materializedConfigActivationReconciler) verifiedNativeMigrations(operation *CLI, yard string) reconcileruntime.VerifiedNativeMigrations {
	store, err := releasetransition.NewPOSIXV2Store(reconciler.configHome)
	if err != nil {
		return reconcileruntime.VerifiedNativeMigrations{}
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return reconcileruntime.VerifiedNativeMigrations{}
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || journal.Goal != reconciler.goal || journal.ArtifactDigest != reconciler.artifactDigest {
		return reconcileruntime.VerifiedNativeMigrations{}
	}
	payload, err := os.ReadFile(filepath.Join(operation.options.RepositoryRoot, "config", "release-transition.json"))
	if err != nil {
		return reconcileruntime.VerifiedNativeMigrations{}
	}
	registry, digest, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil || digest != journal.RegistryDigest {
		return reconcileruntime.VerifiedNativeMigrations{}
	}
	registered, guestDiskTmp := releasetransition.VerifiedNativeMigrationKinds(registry, journal, yard)
	return reconcileruntime.VerifiedNativeMigrations{RegisteredYard: registered, GuestDiskTmp: guestDiskTmp}
}

func (reconciler *materializedConfigActivationReconciler) reconcileIntegrationTarget(ctx context.Context, operation *CLI, target configTarget) (err error) {
	approved, selected := reconciler.integrationPlans[target.Name]
	if !selected {
		return nil
	}
	stage := "lock"
	var comparison *integrationComparison
	defer func() {
		if err != nil {
			err = &integrationActivationFailure{cause: err, stage: stage, projected: reconciler.projectedIntegrationPlans, comparison: comparison}
		}
	}()
	platform := operation.initPlatform(target.Loaded, nil)
	// A stopped target is left untouched even if its saved plan was running.
	// Keep its target and integration scope in Desired; use the target's runtime.
	stage = "observe-fresh"
	native := platform.(reconcileruntime.Runtime).Incus
	instance, err := native.Instance(ctx, target.Loaded.Context.IncusProject, target.Loaded.Context.YardInstanceName)
	if err != nil {
		return err
	}
	if strings.EqualFold(instance.Status, "stopped") || operation.noteYardUninitialized(target, instance) {
		return nil
	}

	stage = "lock"
	unlock, err := lockIntegrationYard(ctx, target.Loaded)
	if err != nil {
		return err
	}
	defer unlock()
	if !reconciler.projectedIntegrationPlans {
		stage = "legacy-adoption"
		platform, _, err = prepareLegacyIntegrationAdoption(ctx, target.Loaded.Integrations, platform)
		if err != nil {
			return err
		}
	}
	runtime := platform.(reconcileruntime.Runtime)
	if reconciler.projectedIntegrationPlans {
		runtime.AdoptLegacyIntegrations = approved.AdoptionFingerprint != ""
		runtime.LegacyIntegrationFingerprint = approved.AdoptionFingerprint
	}
	stage = "observe-fresh"
	plan, err := runtime.IntegrationPlan(ctx)
	if err != nil {
		return err
	}
	comparison = &integrationComparison{approved.Fingerprint == plan.Fingerprint, approved.Scope == plan.Scope, approved.Changed, plan.Changed}
	compared, err := reconcileruntime.ProjectIntegrationPlanAfterNativeMigrations(approved, plan, reconciler.verifiedNativeMigrations(operation, target.Name))
	if err != nil {
		return err
	}
	compared, err = runtime.ProjectIntegrationPlanAfterProfileActivation(ctx, approved, compared)
	if err != nil {
		return err
	}
	if reconciler.projectedIntegrationPlans {
		stage = "compare-projected"
		if err := reconcileruntime.CheckIntegrationPlan(approved, compared); err != nil {
			return err
		}
	} else {
		stage = "compare-exact"
		if compared.Fingerprint != approved.Fingerprint {
			return fmt.Errorf("%w: yard %s integrations changed", domain.ErrPlanStale, target.Name)
		}
	}
	stage, comparison = "apply", nil
	return runtime.ApplyIntegrations(ctx, plan)
}
