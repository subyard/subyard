package testyardmigration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
)

const (
	LegacyYard  = "e2e-yard"
	CurrentYard = "test-yard"
)

type Options struct {
	Executable              string
	RepositoryRoot          string
	Incus                   string
	ConfigHome              string
	DataHome                string
	Environment             []string
	Stdout                  io.Writer
	Stderr                  io.Writer
	RunYard                 func(context.Context, string, io.Writer, ...string) error
	fault                   func(string) error
	syncRegistrationFile    func(*os.File) error
	openRegistrationStaging func(int, string) (int, error)
	syncOwnerDirectory      func(string, int) error
	RecoveryToken           string
	TerminalCleanup         bool
}

type State string

// Prepared binds the exact legacy registration bytes and project image
// namespace observed before the outer transition authorizes mutation.
type Prepared struct {
	State              State
	RegistrationDigest string
	OverridesDigest    string
	ControllerDigest   string
	SharedImages       bool
}

// Progress is the closed set of owner-migration states that an authorized
// release transition may observe while resuming a previously prepared state.
type Progress string

const (
	StateAbsent                      State = "absent"
	StateLegacyDirectory             State = "legacy-directory"
	StateLegacyDirectoryProjects     State = "legacy-directory+projects"
	StateLegacyDirectoryOverrides    State = "legacy-directory+overrides"
	StateLegacyDirectoryState        State = "legacy-directory+projects+overrides"
	StateLegacyFlat                  State = "legacy-flat"
	StateLegacyDirectoryAdoptCurrent State = "legacy-directory-adopt-current"
	StateLegacyFlatAdoptCurrent      State = "legacy-flat-adopt-current"
	StateCurrent                     State = "current"
)

const (
	ProgressExpected   Progress = "expected"
	ProgressInProgress Progress = "in-progress"
	ProgressDesired    Progress = "desired"
)

type registration struct {
	state               State
	oldRegistration     string
	currentRegistration string
}

type registrationSet struct {
	legacy              bool
	current             bool
	legacyState         State
	oldRegistration     string
	currentRegistration string
}

// Prepare validates registration, Incus topology and live lease state without
// changing them.
func Prepare(ctx context.Context, options Options) (Prepared, error) {
	if err := validateOptions(&options); err != nil {
		return Prepared{}, err
	}
	observed, err := inspectRegistration(options)
	if err != nil {
		return Prepared{}, err
	}
	return prepareObserved(ctx, options, observed, "")
}

// PrepareProspective assesses the exact legacy registration content that
// preceding transition work will publish. It reads all other owner facts from
// their live locations and does not write the prospective registration.
func PrepareProspective(
	ctx context.Context,
	options Options,
	readSnapshot func(string) (config.PersistentFileSnapshot, error),
) (Prepared, error) {
	if err := validateOptions(&options); err != nil {
		return Prepared{}, err
	}
	if readSnapshot == nil {
		return Prepared{}, errors.New("prospective owner registration reader is unavailable")
	}
	actual, err := inspectRegistration(options)
	if err != nil {
		return Prepared{}, err
	}
	if actual.state == StateCurrent {
		return prepareObserved(ctx, options, actual, "")
	}
	observed := actual
	if actual.state == StateAbsent {
		yardsRoot := filepath.Join(options.ConfigHome, "yards")
		directory := filepath.Join(yardsRoot, LegacyYard)
		state := StateLegacyDirectory
		if exists, existsErr := pathExists(directory); existsErr != nil {
			return Prepared{}, existsErr
		} else if exists {
			state, err = inspectLegacyDirectoryState(directory)
			if err != nil {
				return Prepared{}, err
			}
		}
		observed = registration{
			state: state, oldRegistration: filepath.Join(directory, "config.env"),
			currentRegistration: filepath.Join(yardsRoot, CurrentYard, "config.env"),
		}
	} else if _, _, migrates := preparedLegacyState(actual.state); !migrates {
		return Prepared{}, errors.New("prospective owner registration conflicts with live registration")
	}
	snapshot, err := readSnapshot(observed.oldRegistration)
	if err != nil {
		return Prepared{}, err
	}
	if !snapshot.Exists {
		return prepareObserved(ctx, options, actual, "")
	}
	if !selectsTestVMs(string(snapshot.Content)) {
		return Prepared{}, errors.New("prospective owner registration does not select YARD_TEMPLATE=test-vms")
	}
	digest := sha256.Sum256(snapshot.Content)
	return prepareObserved(ctx, options, observed, fmt.Sprintf("%x", digest[:]))
}

func prepareObserved(
	ctx context.Context,
	options Options,
	observed registration,
	prospectiveDigest string,
) (Prepared, error) {
	if observed.state == StateAbsent {
		return Prepared{State: observed.state}, nil
	}
	projects, err := inspectProjects(ctx, options)
	if err != nil {
		return Prepared{}, err
	}
	if projects.legacy && projects.current {
		return Prepared{}, errors.New("both legacy and current test-yard Incus projects exist; refusing migration")
	}
	switch observed.state {
	case StateCurrent:
		if err := validateNoLegacyOwnerState(options); err != nil {
			return Prepared{}, err
		}
		if projects.legacy {
			return Prepared{}, errors.New("legacy test-yard Incus project conflicts with current registration")
		}
		prepared, prepareErr := preparedObservedRegistration(
			observed.state, observed.currentRegistration, prospectiveDigest,
		)
		if prepareErr != nil {
			return Prepared{}, prepareErr
		}
		if err := bindPreparedAuxiliaryFacts(
			options, observed.currentRegistration, &prepared,
		); err != nil {
			return Prepared{}, err
		}
		if projects.current {
			prepared.SharedImages, err = sharedImageNamespace(ctx, options, CurrentYard)
			if err != nil {
				return Prepared{}, err
			}
		}
		return prepared, nil
	case StateLegacyDirectory, StateLegacyDirectoryProjects,
		StateLegacyDirectoryOverrides, StateLegacyDirectoryState,
		StateLegacyFlat:
	default:
		return Prepared{}, fmt.Errorf("unknown test-yard registration state %q", observed.state)
	}
	adoptCurrent := projects.current && !projects.legacy
	if !adoptCurrent && !projects.legacy {
		return Prepared{}, errors.New("legacy test-yard Incus project is unavailable")
	}
	if err := validateCurrentStateDirectory(observed.currentRegistration, adoptCurrent); err != nil {
		return Prepared{}, err
	}
	if adoptCurrent && hasLegacyAuxiliaryState(observed.state) {
		return Prepared{}, errors.New(
			"legacy e2e-yard state conflicts with the existing canonical test-yard state",
		)
	}
	imageProject := LegacyYard
	if adoptCurrent {
		imageProject = CurrentYard
	}
	sharedImages, err := sharedImageNamespace(ctx, options, imageProject)
	if err != nil {
		return Prepared{}, err
	}
	prepared, err := preparedObservedRegistration(
		observed.state, observed.oldRegistration, prospectiveDigest,
	)
	if err != nil {
		return Prepared{}, err
	}
	if err := bindPreparedAuxiliaryFacts(options, observed.oldRegistration, &prepared); err != nil {
		return Prepared{}, err
	}
	prepared.SharedImages = sharedImages
	if adoptCurrent {
		prepared.State = adoptState(observed.state)
		return prepared, nil
	}
	if err := preflightLegacyLease(ctx, options, false); err != nil {
		return Prepared{}, err
	}
	return prepared, nil
}

func preparedObservedRegistration(state State, path, prospectiveDigest string) (Prepared, error) {
	if prospectiveDigest != "" {
		return Prepared{State: state, RegistrationDigest: prospectiveDigest}, nil
	}
	return preparedRegistration(state, path)
}

func preparedRegistration(state State, path string) (Prepared, error) {
	digest, err := registrationDigest(path)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{State: state, RegistrationDigest: digest}, nil
}

func registrationDigest(path string) (string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("%x", digest[:]), nil
}

func absentStateDigest() string {
	digest := sha256.Sum256([]byte("subyard-owner-state:absent-v1"))
	return fmt.Sprintf("%x", digest[:])
}

func overridesStateDigest(path string) (string, error) {
	return overridesStateDigestWithOptions(Options{}, path)
}

func bindPreparedAuxiliaryFacts(
	options Options,
	registrationPath string,
	prepared *Prepared,
) error {
	overrides := filepath.Join(filepath.Dir(registrationPath), "overrides")
	if filepath.Base(registrationPath) != "config.env" {
		overrides = ""
	}
	var err error
	if overrides == "" {
		prepared.OverridesDigest = absentStateDigest()
	} else {
		prepared.OverridesDigest, err = overridesStateDigestWithOptions(options, overrides)
		if err != nil {
			return fmt.Errorf("inspect owner overrides state: %w", err)
		}
	}
	controller, err := controllerStateDigest(options, filepath.Join(
		options.DataHome, "e2e", "controllers", LegacyYard,
	))
	if err != nil {
		return fmt.Errorf("inspect legacy controller state: %w", err)
	}
	prepared.ControllerDigest = controller
	return nil
}

// ObserveProgress validates the exact owner topology against the prepared
// legacy state without mutating it. Intermediate states are deliberately
// available only with that durable before-state binding; fresh inspection
// continues to use Prepare and rejects mixed registrations.
func ObserveProgress(ctx context.Context, options Options, before Prepared) (Progress, error) {
	if err := validateOptions(&options); err != nil {
		return "", err
	}
	if err := validatePrepared(before); err != nil {
		return "", err
	}
	shape, adoptCurrent, migrates := preparedLegacyState(before.State)
	if !migrates {
		return "", fmt.Errorf("unknown prepared test-yard state %q", before.State)
	}
	registrations, err := inspectRegistrationSet(options)
	if err != nil {
		return "", err
	}
	projects, err := inspectProjects(ctx, options)
	if err != nil {
		return "", err
	}
	if err := validatePreparedImageNamespaces(ctx, options, projects, before); err != nil {
		return "", err
	}
	oldRegistration, currentRegistration := registrationPaths(options, shape)
	if registrations.legacy && registrations.oldRegistration != oldRegistration {
		return "", errors.New("journaled legacy test-yard registration path changed")
	}
	if registrations.current && registrations.currentRegistration != currentRegistration {
		return "", errors.New("journaled current test-yard registration path changed")
	}
	if registrations.legacy && !registrations.current {
		if err := validatePreparedAuxiliaryState(
			options, oldRegistration, currentRegistration, before, auxiliaryAtSource,
		); err != nil {
			return "", err
		}
		if !equivalentLegacyState(registrations.legacyState, before.State) {
			return "", errors.New("owner migration facts do not match the prepared state")
		}
		if err := registrationMatches(registrations.oldRegistration, before); err != nil {
			return "", err
		}
		if !expectedOwnerProjects(projects, adoptCurrent) {
			if !adoptCurrent && !projects.legacy && !projects.current {
				return ProgressInProgress, nil
			}
			return "", errors.New("owner migration facts do not match the prepared state")
		}
		return ProgressExpected, nil
	}
	if !registrations.current {
		return "", errors.New("owner migration registrations are unavailable")
	}
	if registrations.legacy {
		if err := validatePreparedAuxiliaryState(
			options, oldRegistration, currentRegistration, before, auxiliaryInProgress,
		); err != nil {
			return "", err
		}
		projectsResumable := resumableOwnerProjects(projects, adoptCurrent) ||
			(!adoptCurrent && !projects.legacy && !projects.current)
		if !resumableLegacyState(before.State, registrations.legacyState) || !projectsResumable {
			return "", errors.New("owner migration intermediate facts are not authorized")
		}
		if err := registrationMatches(registrations.oldRegistration, before); err != nil {
			return "", err
		}
		if err := registrationMatches(currentRegistration, before); err != nil {
			return "", err
		}
		return ProgressInProgress, nil
	}
	if err := registrationMatches(registrations.currentRegistration, before); err != nil {
		return "", err
	}
	if err := validatePreparedAuxiliaryState(
		options, oldRegistration, currentRegistration, before, auxiliaryInProgress,
	); err != nil {
		return "", err
	}
	if projects.legacy {
		return "", errors.New("owner migration current registration has conflicting projects")
	}
	if !projects.current {
		return ProgressInProgress, nil
	}
	if err := run(ctx, options, CurrentYard, io.Discard, "check"); err != nil {
		return ProgressInProgress, nil
	}
	legacyController := filepath.Join(options.DataHome, "e2e", "controllers", LegacyYard)
	if exists, err := pathExists(legacyController); err != nil {
		return "", err
	} else if exists {
		return ProgressInProgress, nil
	}
	if err := validatePreparedAuxiliaryState(
		options, oldRegistration, currentRegistration, before, auxiliaryDesired,
	); err != nil {
		return "", err
	}
	return ProgressDesired, nil
}

func validatePrepared(prepared Prepared) error {
	if err := validateStateDigest(prepared.RegistrationDigest, "prepared registration identity"); err != nil {
		return err
	}
	if err := validateStateDigest(prepared.OverridesDigest, "prepared overrides identity"); err != nil {
		return err
	}
	if err := validateStateDigest(prepared.ControllerDigest, "prepared controller identity"); err != nil {
		return err
	}
	return nil
}

func validateStateDigest(value, label string) error {
	if len(value) != sha256.Size*2 || strings.Trim(value, "0123456789abcdef") != "" {
		return fmt.Errorf("%s is invalid", label)
	}
	return nil
}

func registrationMatches(path string, prepared Prepared) error {
	digest, err := registrationDigest(path)
	if err != nil {
		return err
	}
	if digest != prepared.RegistrationDigest {
		return errors.New("owner registration bytes changed outside the authorized transition")
	}
	return nil
}

func expectedOwnerProjects(projects projectSet, adoptCurrent bool) bool {
	return adoptCurrent && projects.current && !projects.legacy ||
		!adoptCurrent && projects.legacy && !projects.current
}

func validatePreparedImageNamespaces(
	ctx context.Context,
	options Options,
	projects projectSet,
	prepared Prepared,
) error {
	for _, project := range []struct {
		exists bool
		yard   string
	}{
		{exists: projects.legacy, yard: LegacyYard},
		{exists: projects.current, yard: CurrentYard},
	} {
		if !project.exists {
			continue
		}
		sharedImages, err := sharedImageNamespace(ctx, options, project.yard)
		if err != nil {
			return err
		}
		if sharedImages != prepared.SharedImages {
			return errors.New("test-yard project image namespace changed outside the authorized transition")
		}
	}
	return nil
}

func resumableOwnerProjects(projects projectSet, adoptCurrent bool) bool {
	return adoptCurrent && projects.current && !projects.legacy ||
		!adoptCurrent && (projects.legacy || projects.current)
}

func resumableLegacyState(before, actual State) bool {
	expected := expectedLegacyState(before)
	return actual == expected ||
		actual == StateLegacyDirectory && (expected == StateLegacyDirectoryProjects ||
			expected == StateLegacyDirectoryOverrides || expected == StateLegacyDirectoryState) ||
		actual == StateLegacyDirectoryOverrides && expected == StateLegacyDirectoryState
}

// Commit performs the prepared transition. Its precondition is persisted by
// the caller before this method runs, so interrupted teardown can resume
// without requiring the legacy yard to remain live.
func Commit(ctx context.Context, options Options, before Prepared) error {
	if err := validateOptions(&options); err != nil {
		return err
	}
	if before.State != StateAbsent && before.State != StateCurrent {
		if err := validatePrepared(before); err != nil {
			return err
		}
	}
	if options.TerminalCleanup {
		if err := validateRecoveryToken(options.RecoveryToken); err != nil {
			return err
		}
		progress, err := ObserveProgress(ctx, options, before)
		if err != nil {
			return err
		}
		if progress != ProgressDesired {
			return errors.New("owner recovery cleanup requires verified desired state")
		}
		return cleanupPreparedOwnerArchives(options, before)
	}
	shape, adoptCurrent, migrates := preparedLegacyState(before.State)
	if !migrates {
		return Verify(options, before)
	}
	if err := validateRecoveryToken(options.RecoveryToken); err != nil {
		return err
	}
	observed, err := inspectRegistrationSet(options)
	if err != nil {
		return err
	}
	oldRegistration, currentRegistration := registrationPaths(options, shape)
	legacyStateMatches := equivalentLegacyState(observed.legacyState, before.State)
	if observed.current {
		legacyStateMatches = resumableLegacyState(before.State, observed.legacyState)
	}
	if observed.legacy && (!legacyStateMatches || observed.oldRegistration != oldRegistration) {
		return errors.New("prepared legacy registration shape changed before commit")
	}
	if observed.current && observed.currentRegistration != currentRegistration {
		return errors.New("current test-yard registration shape conflicts with prepared migration")
	}
	if !observed.legacy && !observed.current {
		return errors.New("prepared test-yard registration disappeared before commit")
	}
	if observed.legacy {
		if err := registrationMatches(observed.oldRegistration, before); err != nil {
			return err
		}
	}
	if observed.current {
		if err := registrationMatches(observed.currentRegistration, before); err != nil {
			return err
		}
		if err := repairAuthorizedRegistrationPublication(
			options, observed.currentRegistration, before,
		); err != nil {
			return err
		}
	}
	auxiliaryStage := auxiliaryInProgress
	if observed.legacy && !observed.current {
		auxiliaryStage = auxiliaryAtSource
	}
	if err := validatePreparedAuxiliaryState(
		options, oldRegistration, currentRegistration, before, auxiliaryStage,
	); err != nil {
		return err
	}
	var retainedRegistration *boundRegistration
	if observed.legacy {
		retainedRegistration, err = openBoundPublishedRegistration(oldRegistration, before)
		if err != nil {
			return err
		}
		defer retainedRegistration.close()
	}
	var retainedOverrides *boundOverrides
	if observed.legacy && hasPreparedOverrides(before.State) {
		overridesPath := filepath.Join(filepath.Dir(oldRegistration), "overrides")
		if exists, existsErr := pathExists(overridesPath); existsErr != nil {
			return existsErr
		} else if exists {
			retainedOverrides, err = openBoundOverrides(options, overridesPath)
			if err != nil {
				return err
			}
			defer retainedOverrides.close()
			if retainedOverrides.digest != before.OverridesDigest {
				return errors.New("owner overrides state changed outside the authorized transition")
			}
		}
	}
	runYard := func(yard string, arguments ...string) error {
		return run(ctx, options, yard, nil, arguments...)
	}
	projects, err := inspectProjects(ctx, options)
	if err != nil {
		return err
	}
	if adoptCurrent {
		if projects.legacy || !projects.current {
			return errors.New("prepared canonical test-yard adoption topology changed before commit")
		}
	} else {
		if !projects.legacy && !projects.current {
			if observed.current {
				// features.images=true deliberately has no precreated canonical
				// project after legacy teardown. Canonical init below recreates it.
			} else if err := restoreLegacyOwnerForResume(ctx, options, runYard, before); err != nil {
				return err
			} else {
				projects, err = inspectProjects(ctx, options)
				if err != nil {
					return err
				}
			}
		}
		if projects.current && !projects.legacy && !observed.current {
			return errors.New("legacy test-yard project changed before registration copy")
		}
		if projects.legacy && projects.current && !observed.current {
			return errors.New("both test-yard projects appeared before registration copy")
		}
	}
	if observed.current && !observed.legacy {
		if err := repairPreparedRegistrationArchive(options, before); err != nil {
			return err
		}
		return finishCurrent(
			runYard, options, !adoptCurrent, oldRegistration, currentRegistration, before,
		)
	}
	if !adoptCurrent && projects.legacy && observed.legacy && !observed.current {
		_, instanceExists, inspectErr := legacyInstanceStatus(ctx, options)
		if inspectErr != nil {
			return inspectErr
		}
		if !instanceExists {
			if err := runYard(LegacyYard, "init", "--yes"); err != nil {
				return fmt.Errorf("resume compensated legacy test yard: %w", err)
			}
			if err := runYard(LegacyYard, "check"); err != nil {
				return fmt.Errorf("validate compensated legacy test yard: %w", err)
			}
		}
	}
	if !adoptCurrent && projects.legacy {
		if err := preflightLegacyLease(ctx, options, true); err != nil {
			return err
		}
	}
	imageYard := LegacyYard
	if !projects.legacy {
		imageYard = CurrentYard
	}
	sharedImages := before.SharedImages
	if projects.legacy || projects.current {
		actualSharedImages, sharedErr := sharedImageNamespace(ctx, options, imageYard)
		if sharedErr != nil {
			return sharedErr
		}
		if actualSharedImages != before.SharedImages {
			return errors.New("test-yard project image namespace changed outside the authorized transition")
		}
	}
	prepareProject := func(yard string) error { return ensureProject(ctx, options, yard, sharedImages) }
	registrationCopied := observed.current
	if !registrationCopied {
		if err := validatePreparedAuxiliaryState(
			options, oldRegistration, currentRegistration, before, auxiliaryAtSource,
		); err != nil {
			return err
		}
		if err := validateAuthorizedCurrentStateDirectory(
			currentRegistration, adoptCurrent, options.RecoveryToken,
		); err != nil {
			return err
		}
		if retainedRegistration == nil {
			return errors.New("legacy owner registration disappeared before publication")
		}
		if err := copyBoundRegistration(
			options, retainedRegistration, currentRegistration, before,
		); err != nil {
			return err
		}
		registrationCopied = true
	}
	recover := func(cause error) error {
		return recoverLegacy(
			ctx,
			options,
			runYard,
			oldRegistration,
			currentRegistration,
			registrationCopied,
			adoptCurrent,
			before,
			cause,
		)
	}
	if !adoptCurrent && projects.legacy {
		if err := prepareProject(CurrentYard); err != nil {
			return recover(err)
		}
		if err := inject(options, "after-current-project-prepare"); err != nil {
			return err
		}
		if err := runYard(LegacyYard, "teardown", "--yes"); err != nil {
			return recover(fmt.Errorf("teardown legacy test yard: %w", err))
		}
		if err := inject(options, "after-legacy-teardown"); err != nil {
			return err
		}
	}
	if err := removeEmptyFlatLegacyDirectory(oldRegistration, before); err != nil {
		return recover(err)
	}
	if err := movePreparedAuxiliaryState(
		options, oldRegistration, currentRegistration, before, retainedOverrides,
	); err != nil {
		return recover(err)
	}
	if hasPreparedOverrides(before.State) {
		if err := inject(options, "after-auxiliary-state-move"); err != nil {
			return err
		}
	}
	if retainedRegistration == nil {
		return recover(errors.New("legacy owner registration disappeared before removal"))
	}
	if err := archivePreparedRegistration(options, retainedRegistration, before); err != nil {
		if isInjectedFault(err) || isResumableOwnerMutationError(err) {
			return err
		}
		return recover(err)
	}
	if err := finishCurrent(
		runYard, options, !adoptCurrent, oldRegistration, currentRegistration, before,
	); err != nil {
		if isInjectedFault(err) || isResumableOwnerMutationError(err) {
			return err
		}
		return recover(err)
	}
	fmt.Fprintln(options.Stdout, "migrated test VM yard e2e-yard -> test-yard")
	return nil
}

// Verify checks the exact postcondition associated with a prepared state.
func Verify(options Options, before Prepared) error {
	if err := verifyState(options, before.State); err != nil {
		return err
	}
	if before.State == StateAbsent {
		return nil
	}
	observed, err := inspectRegistration(options)
	if err != nil {
		return err
	}
	if err := registrationMatches(observed.currentRegistration, before); err != nil {
		return err
	}
	shape, _, migrates := preparedLegacyState(before.State)
	if !migrates {
		return nil
	}
	oldRegistration, currentRegistration := registrationPaths(options, shape)
	return validatePreparedAuxiliaryState(
		options, oldRegistration, currentRegistration, before, auxiliaryDesired,
	)
}

// VerifyState retains the categorical read-only verifier for imported v1
// journals that predate exact owner-registration evidence.
func VerifyState(options Options, before State) error {
	return verifyState(options, before)
}

func verifyState(options Options, before State) error {
	if err := validateOptions(&options); err != nil {
		return err
	}
	observed, err := inspectRegistration(options)
	if err != nil {
		return err
	}
	if before == StateAbsent {
		if observed.state != StateAbsent {
			return fmt.Errorf("test-yard migration expected no registration, found %s", observed.state)
		}
		return nil
	}
	_, _, migrates := preparedLegacyState(before)
	if before != StateCurrent && !migrates {
		return fmt.Errorf("unknown prepared test-yard state %q", before)
	}
	if observed.state != StateCurrent {
		if before == StateCurrent {
			return fmt.Errorf("test-yard migration expected current registration, found %s", observed.state)
		}
		return fmt.Errorf("test-yard migration did not converge: found %s", observed.state)
	}
	return validateNoLegacyOwnerState(options)
}

// VerifyRollback checks that rollback restored the exact registration shape.
func VerifyRollback(options Options, before State) error {
	if err := validateOptions(&options); err != nil {
		return err
	}
	observed, err := inspectRegistration(options)
	if err != nil {
		return err
	}
	if before == StateCurrent {
		if observed.state != StateCurrent {
			return fmt.Errorf("test-yard no-op rollback expected current registration, found %s", observed.state)
		}
		return nil
	}
	if before == StateAbsent {
		switch observed.state {
		case StateAbsent,
			StateLegacyDirectory,
			StateLegacyDirectoryProjects,
			StateLegacyDirectoryOverrides,
			StateLegacyDirectoryState,
			StateLegacyFlat:
			return nil
		default:
			return fmt.Errorf("test-yard no-op rollback found migrated state %s", observed.state)
		}
	}
	_, _, migrates := preparedLegacyState(before)
	if migrates {
		before = expectedLegacyState(before)
	}
	if !equivalentLegacyState(observed.state, before) {
		return fmt.Errorf("test-yard rollback expected registration state %s, found %s", before, observed.state)
	}
	return nil
}

func validateOptions(options *Options) error {
	if options.Executable == "" || !filepath.IsAbs(options.Executable) {
		return errors.New("absolute migration executable is required")
	}
	if options.Incus == "" {
		options.Incus = "incus"
	}
	if options.ConfigHome == "" || !filepath.IsAbs(options.ConfigHome) {
		return errors.New("absolute configuration home is required")
	}
	if options.DataHome == "" || !filepath.IsAbs(options.DataHome) {
		return errors.New("absolute data home is required")
	}
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if options.Stderr == nil {
		options.Stderr = io.Discard
	}
	return nil
}

func inspectRegistration(options Options) (registration, error) {
	found, err := inspectRegistrationSet(options)
	if err != nil {
		return registration{}, err
	}
	if found.legacy && found.current {
		return registration{}, errors.New("test-yard already exists; refusing to replace either yard")
	}
	if found.legacy {
		_, currentRegistration := registrationPaths(options, found.legacyState)
		return registration{
			state:               found.legacyState,
			oldRegistration:     found.oldRegistration,
			currentRegistration: currentRegistration,
		}, nil
	}
	if found.current {
		return registration{
			state:               StateCurrent,
			currentRegistration: found.currentRegistration,
		}, nil
	}
	return registration{state: StateAbsent}, nil
}

func inspectRegistrationSet(options Options) (registrationSet, error) {
	yardsRoot := filepath.Join(options.ConfigHome, "yards")
	if exists, err := pathExists(yardsRoot); err != nil {
		return registrationSet{}, err
	} else if !exists {
		return registrationSet{}, nil
	}
	if err := ownedDirectory(yardsRoot); err != nil {
		return registrationSet{}, err
	}
	type candidate struct {
		path  string
		state State
	}
	legacy := []candidate{
		{filepath.Join(yardsRoot, LegacyYard, "config.env"), StateLegacyDirectory},
		{filepath.Join(yardsRoot, LegacyYard+".env"), StateLegacyFlat},
	}
	current := []candidate{
		{filepath.Join(yardsRoot, CurrentYard, "config.env"), StateCurrent},
		{filepath.Join(yardsRoot, CurrentYard+".env"), StateCurrent},
	}
	var foundLegacy, foundCurrent []candidate
	for _, group := range []struct {
		source           []candidate
		target           *[]candidate
		publishedCurrent bool
	}{
		{legacy, &foundLegacy, false},
		{current, &foundCurrent, true},
	} {
		for _, entry := range group.source {
			var exists bool
			var err error
			if group.publishedCurrent {
				exists, err = ownedCurrentRegistration(entry.path)
			} else {
				exists, err = ownedRegular(entry.path)
			}
			if err != nil {
				return registrationSet{}, err
			}
			if exists {
				if filepath.Base(entry.path) == "config.env" {
					if err := ownedDirectory(filepath.Dir(entry.path)); err != nil {
						return registrationSet{}, err
					}
				}
				*group.target = append(*group.target, entry)
			}
		}
	}
	if len(foundLegacy) > 1 {
		return registrationSet{}, errors.New("multiple e2e-yard registrations exist")
	}
	if len(foundCurrent) > 1 {
		return registrationSet{}, errors.New("multiple test-yard registrations exist")
	}
	result := registrationSet{}
	if len(foundLegacy) > 0 {
		payload, err := os.ReadFile(foundLegacy[0].path)
		if err != nil {
			return registrationSet{}, err
		}
		if !selectsTestVMs(string(payload)) {
			return registrationSet{}, errors.New("e2e-yard does not select YARD_TEMPLATE=test-vms")
		}
		if foundLegacy[0].state == StateLegacyDirectory {
			foundLegacy[0].state, err = inspectLegacyDirectoryState(
				filepath.Dir(foundLegacy[0].path),
			)
			if err != nil {
				return registrationSet{}, err
			}
		}
		result.legacy = true
		result.legacyState = foundLegacy[0].state
		result.oldRegistration = foundLegacy[0].path
	}
	if len(foundCurrent) > 0 {
		payload, err := os.ReadFile(foundCurrent[0].path)
		if err != nil {
			return registrationSet{}, err
		}
		if !selectsTestVMs(string(payload)) {
			return registrationSet{}, errors.New("test-yard does not select YARD_TEMPLATE=test-vms")
		}
		result.current = true
		result.currentRegistration = foundCurrent[0].path
	}
	if !result.legacy && !result.current {
		legacyDirectory := filepath.Join(yardsRoot, LegacyYard)
		if exists, err := pathExists(legacyDirectory); err != nil {
			return registrationSet{}, err
		} else if exists {
			recognized, err := validateSourceLinkedProjectState(legacyDirectory)
			if err != nil {
				return registrationSet{}, err
			}
			if !recognized {
				return registrationSet{}, fmt.Errorf(
					"yard registration directory %q is incomplete",
					LegacyYard,
				)
			}
		}
		currentDirectory := filepath.Join(yardsRoot, CurrentYard)
		if exists, err := pathExists(currentDirectory); err != nil {
			return registrationSet{}, err
		} else if exists {
			recognized, err := validateSourceLinkedProjectState(currentDirectory)
			if err != nil {
				return registrationSet{}, err
			}
			if !recognized {
				return registrationSet{}, fmt.Errorf(
					"yard registration directory %q is incomplete",
					CurrentYard,
				)
			}
		}
	}
	return result, nil
}

func run(
	ctx context.Context,
	options Options,
	yard string,
	stdout io.Writer,
	arguments ...string,
) error {
	if stdout == nil {
		stdout = options.Stdout
	}
	if options.RunYard != nil {
		return options.RunYard(ctx, yard, stdout, arguments...)
	}
	commandArguments := append([]string{"-Y", yard}, arguments...)
	command := exec.CommandContext(ctx, options.Executable, commandArguments...)
	command.Env = migrationChildEnvironment(options.Environment)
	command.Stdin = strings.NewReader("")
	command.Stdout, command.Stderr = stdout, options.Stderr
	return command.Run()
}

func withEnvironment(environment []string, name, value string) []string {
	result := make([]string, 0, len(environment)+1)
	prefix := name + "="
	for _, assignment := range environment {
		if strings.HasPrefix(assignment, prefix) {
			continue
		}
		result = append(result, assignment)
	}
	return append(result, prefix+value)
}

func withoutEnvironment(environment []string, name string) []string {
	result := make([]string, 0, len(environment))
	prefix := name + "="
	for _, assignment := range environment {
		if strings.HasPrefix(assignment, prefix) {
			continue
		}
		result = append(result, assignment)
	}
	return result
}

func selectedYardEnvironment(environment []string) []string {
	result := withoutEnvironment(environment, "SUBYARD_CONFIG_LOADED")
	result = withEnvironment(result, "SUBYARD_ENGINE_CONTEXT", "1")
	return withEnvironment(result, "SUBYARD_ENGINE_CONTEXT_SCHEMA", "1")
}

func migrationChildEnvironment(environment []string) []string {
	return withEnvironment(
		selectedYardEnvironment(environment),
		"SUBYARD_INTERNAL_MIGRATION_CHILD",
		"1",
	)
}

type projectSet struct {
	legacy  bool
	current bool
}

func preparedLegacyState(state State) (State, bool, bool) {
	shape := expectedLegacyState(state)
	adoptCurrent := shape != state
	switch shape {
	case StateLegacyDirectory, StateLegacyDirectoryProjects,
		StateLegacyDirectoryOverrides, StateLegacyDirectoryState:
		return StateLegacyDirectory, adoptCurrent, true
	case StateLegacyFlat:
		return StateLegacyFlat, adoptCurrent, true
	default:
		return "", false, false
	}
}

func adoptState(state State) State {
	if state == StateLegacyDirectory {
		return StateLegacyDirectoryAdoptCurrent
	}
	if state == StateLegacyFlat {
		return StateLegacyFlatAdoptCurrent
	}
	return state
}

func expectedLegacyState(state State) State {
	if state == StateLegacyDirectoryAdoptCurrent {
		return StateLegacyDirectory
	}
	if state == StateLegacyFlatAdoptCurrent {
		return StateLegacyFlat
	}
	return state
}

// Project registration state is generated by yard init and disposable. A
// rollback preserves the operator-owned registration shape and overrides while
// accepting that init may recreate the projects directory.
func equivalentLegacyState(observed, expected State) bool {
	expected = expectedLegacyState(expected)
	return observed == expected ||
		(observed == StateLegacyDirectory || observed == StateLegacyDirectoryProjects) &&
			(expected == StateLegacyDirectory || expected == StateLegacyDirectoryProjects) ||
		(observed == StateLegacyDirectoryOverrides || observed == StateLegacyDirectoryState) &&
			(expected == StateLegacyDirectoryOverrides || expected == StateLegacyDirectoryState)
}

func inspectLegacyDirectoryState(directory string) (State, error) {
	if err := ownedDirectory(directory); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", err
	}
	projects, overrides := false, false
	for _, entry := range entries {
		switch entry.Name() {
		case "config.env":
			continue
		case "projects":
			projects = true
		case "overrides":
			overrides = true
		default:
			return "", fmt.Errorf(
				"legacy e2e-yard registration contains unexpected state %q",
				entry.Name(),
			)
		}
		path := filepath.Join(directory, entry.Name())
		if err := validateOwnedAuxiliaryTree(path, entry.Name() == "projects"); err != nil {
			return "", fmt.Errorf("legacy e2e-yard %s state: %w", entry.Name(), err)
		}
	}
	switch {
	case projects && overrides:
		return StateLegacyDirectoryState, nil
	case projects:
		return StateLegacyDirectoryProjects, nil
	case overrides:
		return StateLegacyDirectoryOverrides, nil
	default:
		return StateLegacyDirectory, nil
	}
}

func validateSourceLinkedProjectState(directory string) (bool, error) {
	if err := ownedDirectory(directory); err != nil {
		return false, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return false, err
	}
	if len(entries) != 1 || entries[0].Name() != "projects" {
		return false, nil
	}
	if err := validateOwnedAuxiliaryTree(filepath.Join(directory, "projects"), true); err != nil {
		return false, fmt.Errorf("source-linked test-yard project state: %w", err)
	}
	return true, nil
}

func validateOwnedAuxiliaryTree(root string, legacyProjectModes bool) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%q is not owned real state", filepath.Base(path))
		}
		if info.IsDir() {
			if info.Mode().Perm()&0o022 != 0 {
				return fmt.Errorf("%q is a writable shared directory", filepath.Base(path))
			}
			return nil
		}
		if !info.Mode().IsRegular() || stat.Nlink != 1 ||
			info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return fmt.Errorf("%q is not a safe regular state file", filepath.Base(path))
		}
		permissions := info.Mode().Perm()
		if permissions&0o111 != 0 {
			return fmt.Errorf("%q is executable state", filepath.Base(path))
		}
		if legacyProjectModes {
			if permissions&0o600 != 0o600 || permissions&^0o666 != 0 {
				return fmt.Errorf("%q has unsafe project-state mode %o", filepath.Base(path), permissions)
			}
		} else if permissions&0o077 != 0 {
			return fmt.Errorf("%q has unsafe override mode %o", filepath.Base(path), permissions)
		}
		return nil
	})
}

func hasLegacyAuxiliaryState(state State) bool {
	projects, overrides := preparedAuxiliaryState(state)
	return projects || overrides
}

func preparedAuxiliaryState(state State) (projects, overrides bool) {
	return state == StateLegacyDirectoryProjects || state == StateLegacyDirectoryState,
		state == StateLegacyDirectoryOverrides || state == StateLegacyDirectoryState
}

func hasPreparedOverrides(state State) bool {
	_, overrides := preparedAuxiliaryState(state)
	return overrides
}

type auxiliaryValidationStage uint8

const (
	auxiliaryAtSource auxiliaryValidationStage = iota
	auxiliaryInProgress
	auxiliaryDesired
)

func validatePreparedAuxiliaryState(
	options Options,
	oldRegistration, currentRegistration string,
	prepared Prepared,
	stage auxiliaryValidationStage,
) error {
	oldOverrides, currentOverrides := "", ""
	if filepath.Base(oldRegistration) == "config.env" {
		oldOverrides = filepath.Join(filepath.Dir(oldRegistration), "overrides")
		currentOverrides = filepath.Join(filepath.Dir(currentRegistration), "overrides")
	}
	oldDigest, currentDigest := absentStateDigest(), absentStateDigest()
	var err error
	if oldOverrides != "" {
		oldDigest, err = overridesStateDigest(oldOverrides)
		if err != nil {
			return fmt.Errorf("inspect legacy owner overrides state: %w", err)
		}
		currentDigest, err = overridesStateDigest(currentOverrides)
		if err != nil {
			return fmt.Errorf("inspect canonical owner overrides state: %w", err)
		}
	}
	want := prepared.OverridesDigest
	absent := absentStateDigest()
	switch stage {
	case auxiliaryAtSource:
		if oldDigest != want || currentDigest != absent {
			return errors.New("owner overrides state changed outside the authorized transition")
		}
	case auxiliaryInProgress:
		valid := false
		if want == absent {
			valid = oldDigest == absent && currentDigest == absent
		} else {
			valid = (oldDigest == want && currentDigest == absent) ||
				(oldDigest == absent && currentDigest == want)
		}
		if !valid {
			return errors.New("owner overrides state changed outside the authorized transition")
		}
	case auxiliaryDesired:
		if oldDigest != absent || currentDigest != want {
			return errors.New("owner overrides state did not converge to the authorized state")
		}
	default:
		return errors.New("unknown auxiliary validation stage")
	}

	if err := validatePreparedRegistrationArchive(options, oldRegistration, prepared, stage); err != nil {
		return err
	}
	if err := validatePreparedControllerState(options, prepared, stage); err != nil {
		return fmt.Errorf("inspect legacy controller state: %w", err)
	}
	return nil
}

func movePreparedAuxiliaryState(
	options Options,
	oldRegistration, currentRegistration string,
	prepared Prepared,
	retained *boundOverrides,
) error {
	if filepath.Base(oldRegistration) != "config.env" {
		return nil
	}
	_, overrides := preparedAuxiliaryState(prepared.State)
	if !overrides {
		return nil
	}
	if err := validatePreparedAuxiliaryState(
		options, oldRegistration, currentRegistration, prepared, auxiliaryInProgress,
	); err != nil {
		return err
	}
	source := filepath.Join(filepath.Dir(oldRegistration), "overrides")
	destination := filepath.Join(filepath.Dir(currentRegistration), "overrides")
	if retained == nil {
		sourceExists, err := pathExists(source)
		if err != nil {
			return err
		}
		if sourceExists {
			return errors.New("prepared owner overrides source is not retained")
		}
		destinationDigest, err := overridesStateDigest(destination)
		if err != nil {
			return err
		}
		if destinationDigest != prepared.OverridesDigest {
			return errors.New("prepared owner overrides destination changed")
		}
		return repairMovedOwnerState(options, source, destination, "override-move")
	}
	if err := moveBoundOverrides(
		options, retained, destination, prepared.OverridesDigest,
		"before-auxiliary-state-move-cas",
	); err != nil {
		return fmt.Errorf("move legacy e2e-yard overrides state: %w", err)
	}
	return nil
}

func registrationPaths(options Options, shape State) (string, string) {
	yardsRoot := filepath.Join(options.ConfigHome, "yards")
	switch shape {
	case StateLegacyDirectory, StateLegacyDirectoryProjects,
		StateLegacyDirectoryOverrides, StateLegacyDirectoryState,
		StateLegacyDirectoryAdoptCurrent:
		return filepath.Join(yardsRoot, LegacyYard, "config.env"),
			filepath.Join(yardsRoot, CurrentYard, "config.env")
	}
	return filepath.Join(yardsRoot, LegacyYard+".env"),
		filepath.Join(yardsRoot, CurrentYard+".env")
}

func inject(options Options, point string) error {
	if options.fault == nil {
		return nil
	}
	if err := options.fault(point); err != nil {
		return injectedFault{err: err}
	}
	return nil
}

type injectedFault struct{ err error }

func (fault injectedFault) Error() string { return fault.err.Error() }
func (fault injectedFault) Unwrap() error { return fault.err }

func isInjectedFault(err error) bool {
	var fault injectedFault
	return errors.As(err, &fault)
}

type resumableOwnerMutationError struct{ err error }

func (failure resumableOwnerMutationError) Error() string { return failure.err.Error() }
func (failure resumableOwnerMutationError) Unwrap() error { return failure.err }

func isResumableOwnerMutationError(err error) bool {
	var failure resumableOwnerMutationError
	return errors.As(err, &failure)
}

func preflightLegacyLease(
	ctx context.Context,
	options Options,
	startStopped bool,
) error {
	restoreStopped, ready, err := temporarilyStartStoppedLegacyYard(
		ctx,
		options,
		startStopped,
	)
	if err != nil {
		return err
	}
	if !ready {
		return nil
	}
	var result error
	if err := run(ctx, options, LegacyYard, nil, "check"); err != nil {
		result = fmt.Errorf("validate legacy test yard: %w", err)
	} else {
		status, statusErr := runCaptured(ctx, options, LegacyYard, "test-vms", "status")
		if statusErr != nil {
			_, _ = options.Stderr.Write(status)
			result = fmt.Errorf("read legacy test VM leases before migration: %w", statusErr)
		} else {
			result = ensureNoActiveLease(status)
		}
	}
	if restoreStopped != nil {
		if err := restoreStopped(); err != nil {
			result = errors.Join(
				result,
				fmt.Errorf("restore stopped legacy test yard after lease preflight: %w", err),
			)
		}
	}
	return result
}

func temporarilyStartStoppedLegacyYard(
	ctx context.Context,
	options Options,
	startStopped bool,
) (func() error, bool, error) {
	status, exists, err := legacyInstanceStatus(ctx, options)
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, errors.New("legacy test-yard instance inventory is not canonical")
	}
	switch status {
	case "RUNNING":
		return nil, true, nil
	case "STOPPED":
	default:
		return nil, false, fmt.Errorf(
			"legacy test-yard instance is in unsupported state %q",
			status,
		)
	}
	desired, err := runIncus(
		ctx,
		options,
		"config",
		"get",
		"yard-"+LegacyYard,
		"user.subyard.desired_power",
		"--project",
		"subyard-"+LegacyYard,
	)
	if err != nil {
		return nil, false, fmt.Errorf("read legacy test-yard desired power: %w", err)
	}
	if strings.TrimSpace(string(desired)) != "running" {
		return nil, false, errors.New(
			"stopped legacy test-yard is not marked desired=running; refusing temporary start",
		)
	}
	if !startStopped {
		return nil, false, nil
	}
	if err := run(ctx, options, LegacyYard, nil, "start", "--yes"); err != nil {
		return nil, false, fmt.Errorf("temporarily start legacy test yard: %w", err)
	}
	return func() error {
		// The guarded start preserves desired=running. Stop only the live
		// instance here so the preflight restores the exact inconsistent state
		// it inspected; the product stop command would also persist
		// desired=stopped.
		_, err := runIncus(
			ctx,
			options,
			"stop",
			"yard-"+LegacyYard,
			"--project",
			"subyard-"+LegacyYard,
		)
		return err
	}, true, nil
}

func legacyInstanceStatus(ctx context.Context, options Options) (string, bool, error) {
	payload, err := runIncus(
		ctx,
		options,
		"list",
		"yard-"+LegacyYard,
		"--project",
		"subyard-"+LegacyYard,
		"--format=json",
	)
	if err != nil {
		return "", false, fmt.Errorf("inspect legacy test-yard instance state: %w", err)
	}
	var instances []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(payload, &instances); err != nil {
		return "", false, fmt.Errorf("decode legacy test-yard instance state: %w", err)
	}
	if len(instances) == 0 {
		return "", false, nil
	}
	if len(instances) != 1 || instances[0].Name != "yard-"+LegacyYard {
		return "", false, errors.New("legacy test-yard instance inventory is not canonical")
	}
	return strings.ToUpper(instances[0].Status), true, nil
}

func runCaptured(
	ctx context.Context,
	options Options,
	yard string,
	arguments ...string,
) ([]byte, error) {
	var output bytes.Buffer
	if options.RunYard != nil {
		err := options.RunYard(ctx, yard, &output, arguments...)
		return output.Bytes(), err
	}
	commandArguments := append([]string{"-Y", yard}, arguments...)
	command := exec.CommandContext(ctx, options.Executable, commandArguments...)
	command.Env = migrationChildEnvironment(options.Environment)
	command.Stdin = strings.NewReader("")
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	return output.Bytes(), err
}

func runIncus(ctx context.Context, options Options, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, options.Incus, arguments...)
	command.Env = options.Environment
	command.Stdin = strings.NewReader("")
	command.Stderr = options.Stderr
	return command.Output()
}

func inspectProjects(ctx context.Context, options Options) (projectSet, error) {
	payload, err := runIncus(ctx, options, "project", "list", "--format=json")
	if err != nil {
		return projectSet{}, fmt.Errorf("list test-yard Incus projects: %w", err)
	}
	var projects []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(payload, &projects); err != nil {
		return projectSet{}, fmt.Errorf("decode test-yard Incus projects: %w", err)
	}
	result := projectSet{}
	for _, project := range projects {
		switch project.Name {
		case "subyard-" + LegacyYard:
			if result.legacy {
				return projectSet{}, errors.New("duplicate legacy test-yard Incus project")
			}
			result.legacy = true
		case "subyard-" + CurrentYard:
			if result.current {
				return projectSet{}, errors.New("duplicate current test-yard Incus project")
			}
			result.current = true
		}
	}
	return result, nil
}

func sharedImageNamespace(ctx context.Context, options Options, yard string) (bool, error) {
	project := "subyard-" + yard
	payload, err := runIncus(ctx, options, "project", "get", project, "features.images")
	if err != nil {
		return false, fmt.Errorf("inspect %s image namespace: %w", yard, err)
	}
	switch strings.TrimSpace(string(payload)) {
	case "false":
		return true, nil
	case "true":
		return false, nil
	default:
		return false, fmt.Errorf("%s has an unknown features.images value", project)
	}
}

func ensureProject(ctx context.Context, options Options, yard string, sharedImages bool) error {
	projects, err := inspectProjects(ctx, options)
	if err != nil {
		return err
	}
	if yard == LegacyYard && projects.legacy || yard == CurrentYard && projects.current {
		return nil
	}
	if !sharedImages {
		return nil
	}
	project := "subyard-" + yard
	if _, err := runIncus(
		ctx,
		options,
		"project",
		"create",
		project,
		"-c",
		"features.images=false",
	); err != nil {
		return fmt.Errorf("preserve shared image namespace for %s: %w", yard, err)
	}
	return nil
}

func ensureNoActiveLease(payload []byte) error {
	for _, line := range strings.Split(string(payload), "\n") {
		if strings.TrimSpace(line) == "test-vms: down" {
			return nil
		}
	}
	var response struct {
		Pool *struct {
			Slots []struct {
				State string `json:"state"`
			} `json:"slots"`
		} `json:"pool"`
	}
	if json.Unmarshal(payload, &response) == nil && response.Pool != nil {
		for _, slot := range response.Pool.Slots {
			if slot.State != "available" {
				return fmt.Errorf(
					"legacy test yard has a %s lease slot; release it and retry yard update",
					slot.State,
				)
			}
		}
		return nil
	}
	for _, line := range strings.Split(string(payload), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "ttl_remaining_seconds" {
			seconds, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || seconds < 0 {
				break
			}
			if seconds > 0 {
				return errors.New("legacy test yard has an active lease; release it and retry yard update")
			}
			return nil
		}
	}
	return errors.New("could not prove that the legacy test yard has no active lease")
}

func finishCurrent(
	run func(string, ...string) error,
	options Options,
	initialize bool,
	oldRegistration, currentRegistration string,
	prepared Prepared,
) error {
	if initialize {
		if err := run(CurrentYard, "init", "--yes"); err != nil {
			return fmt.Errorf("initialize test-yard: %w", err)
		}
		if err := inject(options, "after-current-init"); err != nil {
			return err
		}
	}
	if err := run(CurrentYard, "check"); err != nil {
		return fmt.Errorf("validate test-yard: %w", err)
	}
	if err := inject(options, "after-current-check"); err != nil {
		return err
	}
	if err := validatePreparedAuxiliaryState(
		options, oldRegistration, currentRegistration, prepared, auxiliaryInProgress,
	); err != nil {
		return err
	}
	legacyController := filepath.Join(options.DataHome, "e2e", "controllers", LegacyYard)
	if err := archivePreparedController(options, legacyController, prepared); err != nil {
		return resumableOwnerMutationError{
			err: fmt.Errorf("remove legacy controller state: %w", err),
		}
	}
	return nil
}

func restoreLegacyOwnerForResume(
	ctx context.Context,
	options Options,
	runYard func(string, ...string) error,
	prepared Prepared,
) error {
	if err := ensureProject(ctx, options, LegacyYard, prepared.SharedImages); err != nil {
		return err
	}
	if err := runYard(LegacyYard, "init", "--yes"); err != nil {
		return fmt.Errorf("resume legacy test yard recreation: %w", err)
	}
	if err := runYard(LegacyYard, "check"); err != nil {
		return fmt.Errorf("validate resumed legacy test yard: %w", err)
	}
	return nil
}

func recoverLegacy(
	ctx context.Context,
	options Options,
	run func(string, ...string) error,
	oldRegistration, newRegistration string,
	registrationCopied, preserveCurrent bool,
	prepared Prepared,
	cause error,
) error {
	var recovery []error
	if registrationCopied {
		retainedCurrent, err := openBoundRegistration(newRegistration, prepared)
		if err != nil {
			return errors.Join(cause, fmt.Errorf("retain current registration for recovery: %w", err))
		}
		defer retainedCurrent.close()
		if err := restorePreparedRegistration(options, prepared); err != nil {
			if isInjectedFault(err) || isResumableOwnerMutationError(err) {
				return errors.Join(cause, err)
			}
			recovery = append(recovery, err)
		}
		if !preserveCurrent {
			if err := run(CurrentYard, "teardown", "--yes"); err != nil {
				recovery = append(recovery, fmt.Errorf("teardown failed test-yard: %w", err))
			} else if err := inject(options, "after-compensation-current-teardown"); err != nil {
				return errors.Join(cause, err)
			}
		}
		if !preserveCurrent {
			if err := restorePreparedAuxiliaryState(
				options,
				oldRegistration,
				newRegistration,
				prepared,
			); err != nil {
				recovery = append(recovery, err)
			} else if hasPreparedOverrides(prepared.State) {
				if err := inject(options, "after-compensation-auxiliary-restore"); err != nil {
					return errors.Join(cause, err)
				}
			}
			if err := parkBoundRegistration(options, retainedCurrent, newRegistration, prepared); err != nil {
				if isInjectedFault(err) || isResumableOwnerMutationError(err) {
					return errors.Join(cause, err)
				}
				recovery = append(recovery, err)
			}
		}
	}
	if !preserveCurrent {
		if err := ensureProject(ctx, options, LegacyYard, prepared.SharedImages); err != nil {
			recovery = append(recovery, err)
		} else if err := inject(options, "after-compensation-legacy-project-recreation"); err != nil {
			return errors.Join(cause, err)
		}
		if err := finishLegacyRollback(options, run); err != nil {
			recovery = append(recovery, err)
		}
	}
	if len(recovery) > 0 {
		return errors.Join(cause, fmt.Errorf("test-yard migration recovery failed: %w", errors.Join(recovery...)))
	}
	return cause
}

func finishLegacyRollback(
	options Options,
	runLegacy func(string, ...string) error,
) error {
	if err := runLegacy(LegacyYard, "init", "--yes"); err != nil {
		return fmt.Errorf("recreate legacy test yard: %w", err)
	}
	if err := inject(options, "after-compensation-legacy-init"); err != nil {
		return err
	}
	if err := runLegacy(LegacyYard, "check"); err != nil {
		return fmt.Errorf("validate legacy test yard: %w", err)
	}
	if err := inject(options, "after-compensation-legacy-check"); err != nil {
		return err
	}
	return nil
}

func restorePreparedAuxiliaryState(
	options Options,
	oldRegistration, newRegistration string,
	prepared Prepared,
) error {
	if filepath.Base(oldRegistration) != "config.env" {
		return nil
	}
	if err := validatePreparedAuxiliaryState(
		options, oldRegistration, newRegistration, prepared, auxiliaryInProgress,
	); err != nil {
		return err
	}
	if err := removeGeneratedProjectState(
		filepath.Join(filepath.Dir(newRegistration), "projects"),
	); err != nil {
		return err
	}
	_, overrides := preparedAuxiliaryState(prepared.State)
	source := filepath.Join(filepath.Dir(newRegistration), "overrides")
	destination := filepath.Join(filepath.Dir(oldRegistration), "overrides")
	if overrides {
		retained, err := openBoundOverrides(options, source)
		if errors.Is(err, os.ErrNotExist) {
			digest, digestErr := overridesStateDigest(destination)
			if digestErr != nil {
				return digestErr
			}
			if digest == prepared.OverridesDigest {
				return repairMovedOwnerState(options, source, destination, "override-move")
			}
		}
		if err != nil {
			return err
		}
		defer retained.close()
		if retained.digest != prepared.OverridesDigest {
			return errors.New("owner overrides state changed before compensation")
		}
		if err := moveBoundOverrides(
			options, retained, destination, prepared.OverridesDigest,
			"before-compensation-auxiliary-restore-cas",
		); err != nil {
			return fmt.Errorf("restore legacy e2e-yard overrides state: %w", err)
		}
		return nil
	}
	if exists, err := pathExists(source); err != nil {
		return err
	} else if exists {
		return errors.New("test-yard created unexpected override state during migration")
	}
	return nil
}

func removeGeneratedProjectState(path string) error {
	exists, err := pathExists(path)
	if err != nil || !exists {
		return err
	}
	if err := ownedDirectory(path); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return os.Remove(path)
	}
	if len(entries) != 1 || entries[0].Name() != ".lock" {
		return errors.New("test-yard project state changed during migration")
	}
	lock := filepath.Join(path, ".lock")
	safe, err := ownedRegular(lock)
	if err != nil {
		return err
	}
	if !safe {
		return errors.New("test-yard generated project lock is unsafe")
	}
	info, err := os.Stat(lock)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("test-yard generated project lock is not private")
	}
	if err := os.Remove(lock); err != nil {
		return err
	}
	return os.Remove(path)
}

func removeEmptyFlatLegacyDirectory(registration string, prepared Prepared) error {
	if prepared.State != StateLegacyFlat || filepath.Base(registration) == "config.env" {
		return nil
	}
	directory := filepath.Join(filepath.Dir(registration), LegacyYard)
	exists, err := pathExists(directory)
	if err != nil || !exists {
		return err
	}
	if err := ownedDirectory(directory); err != nil {
		return err
	}
	if err := os.Remove(directory); err != nil {
		return fmt.Errorf("remove empty flat owner directory: %w", err)
	}
	return nil
}

func ownedRegular(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !ok ||
		stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return false, errors.New("legacy test-yard registration is not an owned regular file")
	}
	return true, nil
}

func ownedCurrentRegistration(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !ok ||
		!safePublishedRegistrationMode(stat.Mode) ||
		stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return false, errors.New("current test-yard registration is not an owned safe-mode regular file")
	}
	return true, nil
}

func safePublishedRegistrationMode(mode uint32) bool {
	permissions := mode & 0o7777
	return permissions == 0o600 || permissions == 0o640 || permissions == 0o644
}

func ownedDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ok ||
		stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0o022 != 0 {
		return errors.New("test-yard registration root is not a private owned directory")
	}
	return nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func validateNoLegacyOwnerState(options Options) error {
	for _, path := range []string{
		filepath.Join(options.ConfigHome, "yards", LegacyYard),
		filepath.Join(options.ConfigHome, "yards", LegacyYard+".env"),
		filepath.Join(options.DataHome, "e2e", "controllers", LegacyYard),
	} {
		exists, err := pathExists(path)
		if err != nil {
			return err
		}
		if exists {
			return errors.New("legacy owner state remains beside the canonical registration")
		}
	}
	return nil
}

func selectsTestVMs(payload string) bool {
	for _, line := range strings.Split(payload, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		if line == "YARD_TEMPLATE=test-vms" ||
			line == `YARD_TEMPLATE="test-vms"` || line == "YARD_TEMPLATE='test-vms'" {
			return true
		}
	}
	return false
}
