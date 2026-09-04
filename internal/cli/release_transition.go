package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

const releaseTransitionRequestLimit = 1 << 20

func (cli *CLI) runReleaseTransition(ctx context.Context, arguments []string) int {
	if len(arguments) != 0 {
		cli.errorf("internal: _release-transition accepts a JSON request on stdin")
		return 2
	}
	protocolOutput := cli.options.Stdout
	cli.options.Stdout = io.Discard
	defer func() { cli.options.Stdout = protocolOutput }()
	var request releasetransition.ProcessRequest
	decoder := json.NewDecoder(io.LimitReader(cli.options.Stdin, releaseTransitionRequestLimit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		cli.errorf("release transition request is invalid")
		return 2
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
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
	reconcilers := []releasetransition.V2ActivationReconciler{
		&materializedConfigActivationReconciler{
			cli: cli, yard: request.Yard, configHome: request.ConfigHome,
		},
		cli.brokerActivationReconciler(request),
		cli.routeConsumerActivationReconciler(request),
		cli.powerActivationReconciler(request),
	}
	ownerRegistration := cli.ownerRegistrationTransition(request)
	ingressFactory := cli.releaseTransitionIngressFactory(request)
	response, err := executeReleaseTransitionRequest(
		ctx, cli.options.RepositoryRoot, request,
		func(plan releasetransition.PlanToken, authorization releasetransition.Authorization) bool {
			return expectedAuthorization != "" && authorization == expectedAuthorization
		}, reconcilers, ownerRegistration, ingressFactory,
	)
	if err != nil {
		cli.errorf("release transition: %v", err)
		return 1
	}
	if err := json.NewEncoder(protocolOutput).Encode(response); err != nil {
		cli.errorf("release transition response: %v", err)
		return 1
	}
	return 0
}

type ownerRegistrationTransition struct {
	options func() (testyardmigration.Options, error)
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

func (transition *ownerRegistrationTransition) Commit(
	ctx context.Context,
	before releasetransition.OwnerRegistrationObservation,
) error {
	options, err := transition.options()
	if err != nil {
		return err
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
	return &ownerRegistrationTransition{options: func() (testyardmigration.Options, error) {
		return cli.releaseTransitionTestYardOptions(request)
	}}
}

type activationApplicability struct {
	state   string
	applies bool
	target  string
}

type activationStageReconciler struct {
	id                   string
	stage                ports.ReconcileStageID
	inspectApplicability func(context.Context) (activationApplicability, error)
	platform             func(context.Context, activationApplicability) (ports.ReconcileStageRunner, error)
	authorize            func(context.Context) error
	diagnostics          io.Writer
}

type activationStageError struct {
	phase string
	err   error
}

type routeConsumerActivationPort interface {
	PrepareOwner(context.Context) (string, error)
	Prepare(context.Context) (string, error)
	Verify(context.Context, string) error
	Commit(context.Context, string) error
}

type routeConsumerActivationReconciler struct {
	port routeConsumerActivationPort
}

func (*routeConsumerActivationReconciler) ID() string { return "test-yard-route-consumers" }

func (reconciler *routeConsumerActivationReconciler) Observe(
	ctx context.Context,
	releases releasetransition.ReleasePair,
	_ releasetransition.ReleaseLinks,
) (releasetransition.V2ActivationObservation, error) {
	if reconciler == nil || reconciler.port == nil {
		return releasetransition.V2ActivationObservation{}, errors.New("route consumer activation reconciler is unavailable")
	}
	owner, err := reconciler.port.PrepareOwner(ctx)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
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
	if !active {
		return releasetransition.V2ActivationObservation{
			Actual: desired, Desired: desired, Converged: true,
		}, nil
	}
	if owner != string(testyardmigration.StateCurrent) {
		actual, digestErr := activationStageFingerprint(struct {
			ID    string `json:"id"`
			Owner string `json:"owner"`
		}{reconciler.ID(), owner})
		return releasetransition.V2ActivationObservation{
			Actual: actual, Desired: desired, Converged: false,
		}, digestErr
	}
	before, err := reconciler.port.Prepare(ctx)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	if err := reconciler.port.Verify(ctx, before); err == nil {
		return releasetransition.V2ActivationObservation{
			Actual: desired, Desired: desired, Converged: true,
		}, nil
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return releasetransition.V2ActivationObservation{}, err
	}
	actual, err := activationStageFingerprint(struct {
		ID     string `json:"id"`
		Before string `json:"before"`
		Drift  bool   `json:"drift"`
	}{reconciler.ID(), before, true})
	return releasetransition.V2ActivationObservation{
		Actual: actual, Desired: desired, Converged: false,
	}, err
}

func (reconciler *routeConsumerActivationReconciler) Reconcile(
	ctx context.Context,
	_ releasetransition.ReleaseLinks,
) error {
	if reconciler == nil || reconciler.port == nil {
		return errors.New("route consumer activation reconciler is unavailable")
	}
	owner, err := reconciler.port.PrepareOwner(ctx)
	if err != nil {
		return err
	}
	if owner == string(testyardmigration.StateAbsent) {
		return nil
	}
	if owner != string(testyardmigration.StateCurrent) {
		return fmt.Errorf("route consumer activation requires current owner registration, found %s", owner)
	}
	before, err := reconciler.port.Prepare(ctx)
	if err != nil {
		return err
	}
	if err := reconciler.port.Verify(ctx, before); err == nil {
		return nil
	}
	if err := reconciler.port.Commit(ctx, before); err != nil {
		return err
	}
	return reconciler.port.Verify(ctx, before)
}

type testYardRouteConsumerActivation struct {
	options testyardmigration.Options
}

func (activation *testYardRouteConsumerActivation) PrepareOwner(
	ctx context.Context,
) (string, error) {
	state, err := testyardmigration.Prepare(ctx, activation.options)
	return string(state.State), err
}

func (activation *testYardRouteConsumerActivation) Prepare(ctx context.Context) (string, error) {
	return testyardmigration.PrepareRouteConsumers(ctx, activation.options)
}

func (activation *testYardRouteConsumerActivation) Verify(ctx context.Context, before string) error {
	return testyardmigration.VerifyRouteConsumers(ctx, activation.options, before)
}

func (activation *testYardRouteConsumerActivation) Commit(ctx context.Context, before string) error {
	return testyardmigration.CommitRouteConsumers(ctx, activation.options, before)
}

func (cli *CLI) routeConsumerActivationReconciler(
	request releasetransition.ProcessRequest,
) releasetransition.V2ActivationReconciler {
	options, err := cli.releaseTransitionTestYardOptions(request)
	if err != nil {
		return &routeConsumerActivationReconciler{port: routeConsumerActivationError{err: err}}
	}
	return &routeConsumerActivationReconciler{
		port: &testYardRouteConsumerActivation{options: options},
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
func (failure routeConsumerActivationError) Commit(context.Context, string) error { return failure.err }

func (failure activationStageError) Error() string { return failure.err.Error() }

func (failure activationStageError) Unwrap() error { return failure.err }

func (failure activationStageError) ActivationPhase() string { return failure.phase }

func (reconciler *activationStageReconciler) failure(phase string, err error) error {
	if err == nil {
		return nil
	}
	if reconciler.diagnostics != nil {
		fmt.Fprintf(
			reconciler.diagnostics,
			"yard: release transition activation reconciler %q %s: %v\n",
			reconciler.id,
			phase,
			err,
		)
	}
	return activationStageError{phase: phase, err: err}
}

func (reconciler *activationStageReconciler) ID() string { return reconciler.id }

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
	if !applicability.applies {
		return releasetransition.V2ActivationObservation{
			Actual: desired, Desired: desired, Converged: true,
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
		Actual: actual, Desired: desired, Converged: converged,
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
		}
		if err := platform.ApplyStage(ctx, reconciler.stage); err != nil {
			return reconciler.failure("apply", err)
		}
	}
	converged, err = platform.VerifyStage(ctx, reconciler.stage)
	if err != nil {
		return reconciler.failure("post-verify", err)
	}
	if !converged {
		return reconciler.failure(
			"post-verify",
			fmt.Errorf("activation stage %s did not converge", reconciler.stage),
		)
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
	options := func() (testyardmigration.Options, error) {
		return cli.releaseTransitionTestYardOptions(request)
	}
	return &activationStageReconciler{
		id: "test-vm-broker", stage: ports.ReconcileStageTestVMs,
		diagnostics: cli.options.Stderr,
		inspectApplicability: func(ctx context.Context) (activationApplicability, error) {
			values, err := options()
			if err != nil {
				return activationApplicability{}, err
			}
			state, yard, err := testyardmigration.PrepareBrokerRuntimeTarget(ctx, values)
			return activationApplicability{
				state: string(state), applies: state == testyardmigration.BrokerRuntimeActive,
				target: yard,
			}, err
		},
		platform: func(_ context.Context, applicability activationApplicability) (ports.ReconcileStageRunner, error) {
			if applicability.target == "" {
				return nil, errors.New("active test VM broker has no registered yard")
			}
			loaded, err := cli.resolveReleaseTransitionContext(
				applicability.target,
				request.ConfigHome,
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
	environment := freshMigrationEnvironment(cli.baseEnv, cli.options.RepositoryRoot)
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
		Environment: environmentList(cli.env, nil),
		Stdout:      cli.options.Stderr, Stderr: cli.options.Stderr,
		RunYard: cli.runReleaseTransitionYardCommand,
	}, nil
}

func (cli *CLI) releaseTransitionIngressFactory(
	request releasetransition.ProcessRequest,
) func(releasetransition.ReleasePair) (releasetransition.V2Ingress, error) {
	return func(releases releasetransition.ReleasePair) (releasetransition.V2Ingress, error) {
		testYard, err := cli.releaseTransitionTestYardOptions(request)
		if err != nil {
			return nil, err
		}
		options := migration.ReleaseOptions{
			RepositoryRoot: testYard.RepositoryRoot,
			RuntimeRoot:    request.RuntimeRoot, ConfigHome: testYard.ConfigHome,
			DataHome: testYard.DataHome, Executable: testYard.Executable, Incus: testYard.Incus,
			Environment: testYard.Environment, Diagnostics: testYard.Stdout, Stderr: testYard.Stderr,
		}
		facts, err := migration.NewV1ImportFactObserver(options)
		if err != nil {
			return nil, err
		}
		reader, err := migration.NewV1ImportReader(migration.V1ImportOptions{
			ConfigHome: request.ConfigHome, RuntimeRoot: request.RuntimeRoot, Facts: facts,
		})
		if err != nil {
			return nil, err
		}
		pair := migration.V1ImportRuntimePair{Current: "releases/" + string(releases.From)}
		if releases.Previous != nil {
			pair.Previous = "releases/" + string(*releases.Previous)
		}
		legacy, err := migration.NewV2LegacyIngress(reader, pair)
		if err != nil {
			return nil, err
		}
		compatibility := &migration.V2CompatibilityIngress{Legacy: legacy}
		if request.SourceIngress == nil {
			return compatibility, nil
		}
		source, err := migration.NewV2SourceIngress(migration.V2SourceIngressOptions{
			Descriptor:     *request.SourceIngress,
			RepositoryRoot: testYard.RepositoryRoot,
			RuntimeRoot:    request.RuntimeRoot,
			ConfigHome:     testYard.ConfigHome,
			Environment:    testYard.Environment,
			Stderr:         testYard.Stderr,
		})
		if err != nil {
			return nil, err
		}
		compatibility.Source = source
		return compatibility, nil
	}
}

func (cli *CLI) powerActivationReconciler(
	request releasetransition.ProcessRequest,
) releasetransition.V2ActivationReconciler {
	return &activationStageReconciler{
		id: "host-power", stage: ports.ReconcileStagePower,
		diagnostics: cli.options.Stderr,
		inspectApplicability: func(context.Context) (activationApplicability, error) {
			return inspectPowerActivationApplicability(cli.env)
		},
		platform: func(context.Context, activationApplicability) (ports.ReconcileStageRunner, error) {
			yard := request.Yard
			if yard == "" {
				yard = "default"
			}
			loaded, err := cli.resolveReleaseTransitionContext(yard, request.ConfigHome)
			if err != nil {
				return nil, err
			}
			powerYards, err := cli.powerYardContexts(loaded)
			if err != nil {
				return nil, err
			}
			return cli.initPlatformWithDispatcher(
				loaded,
				powerYards,
				filepath.Join(cli.options.RepositoryRoot, "bin", "yard-engine"),
			), nil
		},
		authorize: cli.releaseTransitionPrivilegeAuthorization,
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
	cli        *CLI
	yard       string
	configHome string
}

type releaseTransitionConfigApplier struct{ cli *CLI }

func (applier releaseTransitionConfigApplier) ApplyConfig(
	ctx context.Context,
	yard string,
) error {
	if applier.cli == nil {
		return errors.New("release transition config applier is unavailable")
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

func (reconciler *materializedConfigActivationReconciler) targets() ([]configTarget, error) {
	if reconciler == nil || reconciler.cli == nil {
		return nil, errors.New("materialized config reconciler is unavailable")
	}
	yard := reconciler.yard
	if yard == "" {
		yard = "default"
	}
	if !domain.SafeName(yard) {
		return nil, errors.New("materialized config yard is invalid")
	}
	loaded, err := reconciler.cli.resolveReleaseTransitionContext(yard, reconciler.configHome)
	if err != nil {
		return nil, err
	}
	return reconciler.cli.localConfigTargets(loaded, false)
}

func (reconciler *materializedConfigActivationReconciler) reconcileCLI() *CLI {
	operation := *reconciler.cli
	if operation.options.Config == nil {
		operation.options.DispatcherPath = filepath.Join(
			operation.options.RepositoryRoot, "bin", "yard-engine",
		)
		operation.options.Config = releaseTransitionConfigApplier{cli: &operation}
	}
	return &operation
}

func (reconciler *materializedConfigActivationReconciler) Observe(
	ctx context.Context,
	_ releasetransition.ReleasePair,
	_ releasetransition.ReleaseLinks,
) (releasetransition.V2ActivationObservation, error) {
	targets, err := reconciler.targets()
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	type targetFingerprint struct {
		Name        string `json:"name"`
		Fingerprint string `json:"fingerprint"`
		State       string `json:"state,omitempty"`
	}
	desired := make([]targetFingerprint, 0, len(targets))
	actual := make([]targetFingerprint, 0, len(targets))
	converged := true
	for _, target := range targets {
		assessment, assessErr := reconciler.cli.assessConfigTarget(ctx, target, true)
		if assessErr != nil {
			return releasetransition.V2ActivationObservation{}, assessErr
		}
		desired = append(desired, targetFingerprint{
			Name: target.Name, Fingerprint: assessment.DesiredFingerprint,
		})
		actual = append(actual, targetFingerprint{
			Name: target.Name, Fingerprint: assessment.MaterializedFingerprint,
			State: assessment.State,
		})
		converged = converged && !assessment.Changed
	}
	sort.Slice(desired, func(i, j int) bool { return desired[i].Name < desired[j].Name })
	sort.Slice(actual, func(i, j int) bool { return actual[i].Name < actual[j].Name })
	desiredPayload, err := json.Marshal(desired)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	actualPayload, err := json.Marshal(actual)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	if converged {
		actualPayload = desiredPayload
	}
	desiredDigest := sha256.Sum256(desiredPayload)
	actualDigest := sha256.Sum256(actualPayload)
	return releasetransition.V2ActivationObservation{
		Actual:    releasetransition.Fingerprint(fmt.Sprintf("%x", actualDigest[:])),
		Desired:   releasetransition.Fingerprint(fmt.Sprintf("%x", desiredDigest[:])),
		Converged: converged,
	}, nil
}

func (reconciler *materializedConfigActivationReconciler) Reconcile(
	ctx context.Context,
	_ releasetransition.ReleaseLinks,
) error {
	targets, err := reconciler.targets()
	if err != nil {
		return err
	}
	if code := reconciler.reconcileCLI().applyConfig(ctx, targets, true, reconciler.targets); code != 0 {
		return fmt.Errorf("materialized config reconcile returned status %d", code)
	}
	return nil
}

func (cli *CLI) resolveReleaseTransitionContext(
	yard string,
	configHome string,
) (config.Loaded, error) {
	environment := freshMigrationEnvironment(cli.baseEnv, cli.options.RepositoryRoot)
	if configHome != "" {
		environment["SUBYARD_CONFIG_HOME"] = configHome
	}
	operatorHome := environment["SUBYARD_OPERATOR_HOME"]
	if operatorHome == "" {
		operatorHome = environment["HOME"]
	}
	options := config.LoadOptions{
		RepositoryRoot: cli.options.RepositoryRoot,
		OperatorHome:   operatorHome,
		YardName:       yard,
		Environment:    environment,
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
	transition, err := releasetransition.NewV2Transition(releasetransition.V2Options{
		ConfigHome: request.ConfigHome, Releases: releases, Direction: request.Direction,
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
			return links.Observe()
		},
		ActivateLinks: func(_ context.Context, pair releasetransition.ReleasePair) (releasetransition.ReleaseLinks, error) {
			return links.Activate(pair)
		},
		Reconcilers: reconcilers, OwnerRegistration: ownerRegistration, Ingress: ingress,
		SourceIngress:       request.SourceIngress,
		RegistryPayload:     registry,
		ArtifactDigest:      request.ArtifactDigest,
		InheritedSettingIDs: request.InheritedSettingIDs,
		VerifyAuthorization: verifyAuthorization,
	})
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
		inspection, inspectErr := transition.Inspect(ctx, goal)
		if inspectErr != nil {
			return releasetransition.ProcessResponse{}, inspectErr
		}
		response.Inspection = &inspection
	case releasetransition.ProcessConverge:
		if request.Execution == nil {
			return releasetransition.ProcessResponse{}, errors.New("converge request has no execution")
		}
		outcome, convergeErr := transition.Converge(ctx, *request.Execution)
		if convergeErr != nil {
			return releasetransition.ProcessResponse{}, convergeErr
		}
		response.Outcome = &outcome
	}
	return response, nil
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
