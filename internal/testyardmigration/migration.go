package testyardmigration

import (
	"bytes"
	"context"
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
)

const (
	LegacyYard  = "e2e-yard"
	CurrentYard = "test-yard"
)

type Options struct {
	Executable     string
	RepositoryRoot string
	RuntimeRoot    string
	Incus          string
	ConfigHome     string
	DataHome       string
	Environment    []string
	Stdout         io.Writer
	Stderr         io.Writer
}

type State string

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
func Prepare(ctx context.Context, options Options) (State, error) {
	if err := validateOptions(&options); err != nil {
		return "", err
	}
	observed, err := inspectRegistration(options)
	if err != nil {
		return "", err
	}
	if observed.state == StateAbsent {
		return observed.state, nil
	}
	projects, err := inspectProjects(ctx, options)
	if err != nil {
		return "", err
	}
	if projects.legacy && projects.current {
		return "", errors.New("both legacy and current test-yard Incus projects exist; refusing migration")
	}
	switch observed.state {
	case StateCurrent:
		if projects.legacy {
			return "", errors.New("legacy test-yard Incus project conflicts with current registration")
		}
		if projects.current {
			if _, err := sharedImageNamespace(ctx, options, CurrentYard); err != nil {
				return "", err
			}
		}
		return observed.state, nil
	case StateLegacyDirectory, StateLegacyDirectoryProjects,
		StateLegacyDirectoryOverrides, StateLegacyDirectoryState,
		StateLegacyFlat:
	default:
		return "", fmt.Errorf("unknown test-yard registration state %q", observed.state)
	}
	adoptCurrent := projects.current && !projects.legacy
	if !adoptCurrent && !projects.legacy {
		return "", errors.New("legacy test-yard Incus project is unavailable")
	}
	if err := validateCurrentStateDirectory(observed.currentRegistration, adoptCurrent); err != nil {
		return "", err
	}
	if adoptCurrent && hasLegacyAuxiliaryState(observed.state) {
		return "", errors.New(
			"legacy e2e-yard state conflicts with the existing canonical test-yard state",
		)
	}
	imageProject := LegacyYard
	if adoptCurrent {
		imageProject = CurrentYard
	}
	if _, err := sharedImageNamespace(ctx, options, imageProject); err != nil {
		return "", err
	}
	if adoptCurrent {
		return adoptState(observed.state), nil
	}
	if err := preflightLegacyLease(ctx, options, false); err != nil {
		return "", err
	}
	return observed.state, nil
}

// Commit performs the prepared transition. Its precondition is persisted by
// the caller before this method runs, so interrupted teardown can resume
// without requiring the legacy yard to remain live.
func Commit(ctx context.Context, options Options, before State) error {
	if err := validateOptions(&options); err != nil {
		return err
	}
	shape, adoptCurrent, migrates := preparedLegacyState(before)
	if !migrates {
		return Verify(options, before)
	}
	observed, err := inspectRegistrationSet(options)
	if err != nil {
		return err
	}
	oldRegistration, currentRegistration := registrationPaths(options, shape)
	if observed.legacy && (observed.legacyState != expectedLegacyState(before) ||
		observed.oldRegistration != oldRegistration) {
		return errors.New("prepared legacy registration shape changed before commit")
	}
	if observed.current && observed.currentRegistration != currentRegistration {
		return errors.New("current test-yard registration shape conflicts with prepared migration")
	}
	if !observed.legacy && !observed.current {
		return errors.New("prepared test-yard registration disappeared before commit")
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
			return errors.New("prepared test-yard Incus projects disappeared before commit")
		}
		if projects.current && !projects.legacy && !observed.current {
			return errors.New("legacy test-yard project changed before registration copy")
		}
		if projects.legacy && projects.current && !observed.current {
			return errors.New("both test-yard projects appeared before registration copy")
		}
	}
	if observed.current && !observed.legacy {
		if filepath.Base(oldRegistration) == "config.env" {
			if err := removeEmptyDirectory(filepath.Dir(oldRegistration)); err != nil {
				return err
			}
		}
		return finishCurrent(runYard, options, !adoptCurrent)
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
	sharedImages, err := sharedImageNamespace(ctx, options, imageYard)
	if err != nil {
		return err
	}
	prepareProject := func(yard string) error {
		return ensureProject(ctx, options, yard, sharedImages)
	}
	registrationCopied := observed.current
	if !registrationCopied {
		if err := validateCurrentStateDirectory(currentRegistration, adoptCurrent); err != nil {
			return err
		}
		if err := copyRegistration(oldRegistration, currentRegistration); err != nil {
			return err
		}
		registrationCopied = true
	}
	recover := func(cause error) error {
		return recoverLegacy(
			runYard,
			runYard,
			nil,
			prepareProject,
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
		if err := runYard(LegacyYard, "teardown", "--yes"); err != nil {
			return recover(fmt.Errorf("teardown legacy test yard: %w", err))
		}
	}
	if err := movePreparedAuxiliaryState(oldRegistration, currentRegistration, before); err != nil {
		return recover(err)
	}
	if err := os.Remove(oldRegistration); err != nil && !errors.Is(err, os.ErrNotExist) {
		return recover(err)
	}
	if filepath.Base(oldRegistration) == "config.env" {
		if err := removeEmptyDirectory(filepath.Dir(oldRegistration)); err != nil {
			return recover(err)
		}
	}
	if err := finishCurrent(runYard, options, !adoptCurrent); err != nil {
		return recover(err)
	}
	fmt.Fprintln(options.Stdout, "migrated test VM yard e2e-yard -> test-yard")
	return nil
}

// Verify checks the exact postcondition associated with a prepared state.
func Verify(options Options, before State) error {
	if err := validateOptions(&options); err != nil {
		return err
	}
	observed, err := inspectRegistration(options)
	if err != nil {
		return err
	}
	switch before {
	case StateAbsent:
		if observed.state != StateAbsent {
			return fmt.Errorf("test-yard migration expected no registration, found %s", observed.state)
		}
	case StateCurrent:
		if observed.state != StateCurrent {
			return fmt.Errorf("test-yard migration expected current registration, found %s", observed.state)
		}
	case StateLegacyDirectory, StateLegacyDirectoryProjects,
		StateLegacyDirectoryOverrides, StateLegacyDirectoryState,
		StateLegacyFlat,
		StateLegacyDirectoryAdoptCurrent, StateLegacyFlatAdoptCurrent:
		if observed.state != StateCurrent {
			return fmt.Errorf("test-yard migration did not converge: found %s", observed.state)
		}
	default:
		return fmt.Errorf("unknown prepared test-yard state %q", before)
	}
	return nil
}

// VerifyRollback checks that rollback restored the exact registration shape.
func VerifyRollback(options Options, before State) error {
	if err := validateOptions(&options); err != nil {
		return err
	}
	if before == StateCurrent {
		observed, err := inspectRegistration(options)
		if err != nil {
			return err
		}
		if observed.state != StateCurrent {
			return fmt.Errorf(
				"test-yard no-op rollback expected current registration, found %s",
				observed.state,
			)
		}
		return nil
	}
	if before == StateAbsent {
		observed, err := inspectRegistration(options)
		if err != nil {
			return err
		}
		switch observed.state {
		case StateAbsent,
			StateLegacyDirectory,
			StateLegacyDirectoryProjects,
			StateLegacyDirectoryOverrides,
			StateLegacyDirectoryState,
			StateLegacyFlat:
			return nil
		default:
			return fmt.Errorf(
				"test-yard no-op rollback found migrated state %s",
				observed.state,
			)
		}
	}
	_, _, migrates := preparedLegacyState(before)
	if migrates {
		before = expectedLegacyState(before)
	}
	observed, err := inspectRegistration(options)
	if err != nil {
		return err
	}
	if !equivalentLegacyState(observed.state, before) {
		return fmt.Errorf(
			"test-yard rollback expected registration state %s, found %s",
			before,
			observed.state,
		)
	}
	return nil
}

// Rollback restores the legacy registration with the same runtime that removes
// the temporary canonical owner.
func Rollback(ctx context.Context, options Options, before State) error {
	return RollbackWithLegacyRuntime(ctx, options, options, before)
}

// RollbackWithLegacyRuntime lets the active migration runtime remove the
// temporary canonical owner while the retained source runtime recreates and
// validates the legacy owner it understands.
func RollbackWithLegacyRuntime(
	ctx context.Context,
	options Options,
	legacyOptions Options,
	before State,
) error {
	return RollbackWithLegacyRuntimeAndPower(ctx, options, legacyOptions, before, "")
}

// RollbackWithLegacyRuntimeAndPower also restores the legacy broker's durable
// power intent after the retained runtime recreates its owner. This keeps a
// resumed layout-1 migration faithful to the original broker state.
func RollbackWithLegacyRuntimeAndPower(
	ctx context.Context,
	options Options,
	legacyOptions Options,
	before State,
	desiredPower string,
) error {
	if err := validateOptions(&options); err != nil {
		return err
	}
	if err := validateOptions(&legacyOptions); err != nil {
		return err
	}
	if desiredPower != "" && desiredPower != "running" && desiredPower != "stopped" {
		return fmt.Errorf("invalid legacy desired power %q", desiredPower)
	}
	if before == StateAbsent || before == StateCurrent {
		return nil
	}
	shape, adoptCurrent, migrates := preparedLegacyState(before)
	if !migrates {
		return fmt.Errorf("unknown prepared test-yard state %q", before)
	}
	observed, err := inspectRegistrationSet(options)
	if err != nil {
		return err
	}
	runYard := func(yard string, arguments ...string) error {
		return run(ctx, options, yard, nil, arguments...)
	}
	runLegacyYard := func(yard string, arguments ...string) error {
		return run(ctx, legacyOptions, yard, nil, arguments...)
	}
	restoreLegacyPower := func() error {
		if desiredPower == "" {
			return nil
		}
		if _, err := runIncus(
			ctx,
			options,
			"config",
			"set",
			"yard-e2e-yard",
			"user.subyard.desired_power",
			desiredPower,
			"--project",
			"subyard-e2e-yard",
		); err != nil {
			return fmt.Errorf("restore legacy test VM broker desired power: %w", err)
		}
		return nil
	}
	oldRegistration, currentRegistration := registrationPaths(options, shape)
	if observed.legacy && !observed.current {
		if !equivalentLegacyState(observed.legacyState, before) ||
			observed.oldRegistration != oldRegistration {
			return errors.New("rollback found a different legacy registration shape")
		}
		if adoptCurrent {
			return nil
		}
		return finishLegacyRollback(runLegacyYard, restoreLegacyPower)
	}
	if !observed.current {
		return errors.New("cannot roll back test-yard migration without a current registration")
	}
	if observed.currentRegistration != currentRegistration {
		return errors.New("rollback found a different current registration shape")
	}
	projects, err := inspectProjects(ctx, options)
	if err != nil {
		return err
	}
	imageYard := CurrentYard
	if !projects.current && projects.legacy {
		imageYard = LegacyYard
	}
	sharedImages, err := sharedImageNamespace(ctx, options, imageYard)
	if err != nil {
		return err
	}
	prepareProject := func(yard string) error {
		return ensureProject(ctx, options, yard, sharedImages)
	}
	return recoverLegacy(
		runYard,
		runLegacyYard,
		restoreLegacyPower,
		prepareProject,
		oldRegistration,
		currentRegistration,
		true,
		adoptCurrent,
		before,
		nil,
	)
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
		return registration{
			state:               found.legacyState,
			oldRegistration:     found.oldRegistration,
			currentRegistration: expectedCurrentRegistration(options, found.legacyState),
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
		source []candidate
		target *[]candidate
	}{
		{legacy, &foundLegacy},
		{current, &foundCurrent},
	} {
		for _, entry := range group.source {
			exists, err := ownedRegular(entry.path)
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
	commandArguments := append([]string{"-Y", yard}, arguments...)
	command := exec.CommandContext(ctx, options.Executable, commandArguments...)
	command.Env = migrationChildEnvironment(options.Environment)
	command.Stdin = strings.NewReader("")
	if stdout == nil {
		stdout = options.Stdout
	}
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
	switch state {
	case StateLegacyDirectory, StateLegacyDirectoryProjects,
		StateLegacyDirectoryOverrides, StateLegacyDirectoryState:
		return StateLegacyDirectory, false, true
	case StateLegacyFlat:
		return StateLegacyFlat, false, true
	case StateLegacyDirectoryAdoptCurrent:
		return StateLegacyDirectory, true, true
	case StateLegacyFlatAdoptCurrent:
		return StateLegacyFlat, true, true
	default:
		return "", false, false
	}
}

func adoptState(state State) State {
	switch state {
	case StateLegacyDirectory:
		return StateLegacyDirectoryAdoptCurrent
	case StateLegacyDirectoryProjects,
		StateLegacyDirectoryOverrides,
		StateLegacyDirectoryState:
		return state
	case StateLegacyFlat:
		return StateLegacyFlatAdoptCurrent
	default:
		return state
	}
}

func expectedLegacyState(state State) State {
	switch state {
	case StateLegacyDirectoryAdoptCurrent:
		return StateLegacyDirectory
	case StateLegacyFlatAdoptCurrent:
		return StateLegacyFlat
	default:
		return state
	}
}

// Project registration state is generated by yard init and disposable. A
// rollback preserves the operator-owned registration shape and overrides while
// accepting that init may recreate the projects directory.
func equivalentLegacyState(observed, expected State) bool {
	expected = expectedLegacyState(expected)
	switch expected {
	case StateLegacyDirectory, StateLegacyDirectoryProjects:
		return observed == StateLegacyDirectory ||
			observed == StateLegacyDirectoryProjects
	case StateLegacyDirectoryOverrides, StateLegacyDirectoryState:
		return observed == StateLegacyDirectoryOverrides ||
			observed == StateLegacyDirectoryState
	case StateLegacyFlat:
		return observed == StateLegacyFlat
	default:
		return false
	}
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
	switch state {
	case StateLegacyDirectoryProjects:
		return true, false
	case StateLegacyDirectoryOverrides:
		return false, true
	case StateLegacyDirectoryState:
		return true, true
	default:
		return false, false
	}
}

func movePreparedAuxiliaryState(oldRegistration, currentRegistration string, state State) error {
	if filepath.Base(oldRegistration) != "config.env" {
		return nil
	}
	_, overrides := preparedAuxiliaryState(state)
	if !overrides {
		return nil
	}
	if err := moveAuxiliaryDirectory(
		filepath.Join(filepath.Dir(oldRegistration), "overrides"),
		filepath.Join(filepath.Dir(currentRegistration), "overrides"),
	); err != nil {
		return fmt.Errorf(
			"move legacy e2e-yard overrides state: %w",
			err,
		)
	}
	return nil
}

func moveAuxiliaryDirectory(source, destination string) error {
	sourceExists, err := pathExists(source)
	if err != nil {
		return err
	}
	destinationExists, err := pathExists(destination)
	if err != nil {
		return err
	}
	switch {
	case sourceExists && destinationExists:
		return errors.New("both source and destination state directories exist")
	case sourceExists:
		if err := ownedDirectory(source); err != nil {
			return err
		}
		return os.Rename(source, destination)
	case destinationExists:
		return ownedDirectory(destination)
	default:
		return errors.New("prepared state directory disappeared")
	}
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

func expectedCurrentRegistration(options Options, shape State) string {
	_, current := registrationPaths(options, shape)
	return current
}

func validateCurrentStateDirectory(currentRegistration string, adoptCurrent bool) error {
	if filepath.Base(currentRegistration) != "config.env" {
		return nil
	}
	directory := filepath.Dir(currentRegistration)
	exists, err := pathExists(directory)
	if err != nil || !exists {
		return err
	}
	if !adoptCurrent {
		return errors.New("test-yard directory already exists; refusing to replace it")
	}
	if err := ownedDirectory(directory); err != nil {
		return fmt.Errorf("existing test-yard state directory is unsafe: %w", err)
	}
	return nil
}

func copyRegistration(source, destination string) error {
	payload, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(payload); err == nil {
		err = file.Close()
	} else {
		_ = file.Close()
	}
	if err != nil {
		_ = os.Remove(destination)
		_ = removeEmptyDirectory(filepath.Dir(destination))
	}
	return err
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
		return nil, false, fmt.Errorf("inspect legacy test-yard instance state: %w", err)
	}
	var instances []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(payload, &instances); err != nil {
		return nil, false, fmt.Errorf("decode legacy test-yard instance state: %w", err)
	}
	if len(instances) != 1 || instances[0].Name != "yard-"+LegacyYard {
		return nil, false, errors.New("legacy test-yard instance inventory is not canonical")
	}
	switch strings.ToUpper(instances[0].Status) {
	case "RUNNING":
		return nil, true, nil
	case "STOPPED":
	default:
		return nil, false, fmt.Errorf(
			"legacy test-yard instance is in unsupported state %q",
			instances[0].Status,
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

func runCaptured(
	ctx context.Context,
	options Options,
	yard string,
	arguments ...string,
) ([]byte, error) {
	commandArguments := append([]string{"-Y", yard}, arguments...)
	command := exec.CommandContext(ctx, options.Executable, commandArguments...)
	command.Env = migrationChildEnvironment(options.Environment)
	command.Stdin = strings.NewReader("")
	var output bytes.Buffer
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
) error {
	if initialize {
		if err := run(CurrentYard, "init", "--yes"); err != nil {
			return fmt.Errorf("initialize test-yard: %w", err)
		}
	}
	if err := run(CurrentYard, "check"); err != nil {
		return fmt.Errorf("validate test-yard: %w", err)
	}
	legacyController := filepath.Join(options.DataHome, "e2e", "controllers", LegacyYard)
	if err := removeManagedLegacyController(legacyController); err != nil {
		return fmt.Errorf("remove legacy controller state: %w", err)
	}
	return nil
}

func recoverLegacy(
	run func(string, ...string) error,
	runLegacy func(string, ...string) error,
	restoreLegacyPower func() error,
	prepareProject func(string) error,
	oldRegistration, newRegistration string,
	registrationCopied, preserveCurrent bool,
	prepared State,
	cause error,
) error {
	var recovery []error
	if registrationCopied {
		if exists, err := pathExists(oldRegistration); err != nil {
			recovery = append(recovery, err)
		} else if !exists {
			payload, readErr := os.ReadFile(newRegistration)
			if readErr != nil {
				recovery = append(recovery, readErr)
			} else if mkdirErr := os.MkdirAll(filepath.Dir(oldRegistration), 0o700); mkdirErr != nil {
				recovery = append(recovery, mkdirErr)
			} else if writeErr := os.WriteFile(oldRegistration, payload, 0o600); writeErr != nil {
				recovery = append(recovery, writeErr)
			}
		}
		if !preserveCurrent {
			if err := run(CurrentYard, "teardown", "--yes"); err != nil {
				recovery = append(recovery, fmt.Errorf("teardown failed test-yard: %w", err))
			}
		}
		if !preserveCurrent {
			if err := restorePreparedAuxiliaryState(
				oldRegistration,
				newRegistration,
				prepared,
			); err != nil {
				recovery = append(recovery, err)
			}
		}
		if err := os.Remove(newRegistration); err != nil && !errors.Is(err, os.ErrNotExist) {
			recovery = append(recovery, err)
		}
		_ = removeEmptyDirectory(filepath.Dir(newRegistration))
	}
	if !preserveCurrent {
		if err := prepareProject(LegacyYard); err != nil {
			recovery = append(recovery, err)
		}
		if err := finishLegacyRollback(runLegacy, restoreLegacyPower); err != nil {
			recovery = append(recovery, err)
		}
	}
	if len(recovery) > 0 {
		return errors.Join(cause, fmt.Errorf("test-yard migration recovery failed: %w", errors.Join(recovery...)))
	}
	return cause
}

func finishLegacyRollback(
	runLegacy func(string, ...string) error,
	restoreLegacyPower func() error,
) error {
	if err := runLegacy(LegacyYard, "init", "--yes"); err != nil {
		return fmt.Errorf("recreate legacy test yard: %w", err)
	}
	if restoreLegacyPower != nil {
		if err := restoreLegacyPower(); err != nil {
			return err
		}
	}
	if err := runLegacy(LegacyYard, "check"); err != nil {
		return fmt.Errorf("validate legacy test yard: %w", err)
	}
	return nil
}

func restorePreparedAuxiliaryState(
	oldRegistration, newRegistration string,
	prepared State,
) error {
	if filepath.Base(oldRegistration) != "config.env" {
		return nil
	}
	if err := removeGeneratedProjectState(
		filepath.Join(filepath.Dir(newRegistration), "projects"),
	); err != nil {
		return err
	}
	_, overrides := preparedAuxiliaryState(prepared)
	source := filepath.Join(filepath.Dir(newRegistration), "overrides")
	destination := filepath.Join(filepath.Dir(oldRegistration), "overrides")
	if overrides {
		if err := moveAuxiliaryDirectory(source, destination); err != nil {
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

func removeManagedLegacyController(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
		info.Mode().Perm()&0o077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("legacy controller path is not an owned private directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return os.Remove(path)
	}
	allowed := map[string]bool{
		".operator-enrollment-v1": true,
		"agent-access.pub":        true,
		"route.tsv":               true,
		"known_hosts":             true,
	}
	markerFound := false
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return fmt.Errorf("unexpected legacy controller artifact %q", entry.Name())
		}
		artifact := filepath.Join(path, entry.Name())
		artifactInfo, err := os.Lstat(artifact)
		if err != nil {
			return err
		}
		artifactStat, ok := artifactInfo.Sys().(*syscall.Stat_t)
		if !ok || artifactInfo.Mode()&os.ModeSymlink != 0 ||
			!artifactInfo.Mode().IsRegular() || artifactStat.Nlink != 1 ||
			artifactStat.Uid != uint32(os.Geteuid()) {
			return fmt.Errorf("legacy controller artifact %q is unsafe", entry.Name())
		}
		if entry.Name() == ".operator-enrollment-v1" {
			payload, err := os.ReadFile(artifact)
			if err != nil || string(payload) != "managed\n" ||
				artifactInfo.Mode().Perm() != 0o600 {
				return errors.New("legacy controller marker is invalid")
			}
			markerFound = true
		}
	}
	if !markerFound {
		return errors.New("legacy controller marker is missing")
	}
	for _, entry := range entries {
		if entry.Name() == ".operator-enrollment-v1" {
			continue
		}
		if err := os.Remove(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(path, ".operator-enrollment-v1")); err != nil {
		return err
	}
	return os.Remove(path)
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

func removeEmptyDirectory(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTEMPTY) {
		return nil
	}
	return err
}
