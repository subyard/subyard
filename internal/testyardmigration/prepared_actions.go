package testyardmigration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// A durable rename with an unresolved outcome retains its recovery boundary.
func PreparedRegistrationActionUncertain(err error) bool {
	return isInjectedFault(err) || isResumableOwnerMutationError(err)
}

// ApplyPreparedRegistrationAction executes one fixed filesystem operation. The
// caller owns ordering, authorization and recovery; no yard command runs here.
func ApplyPreparedRegistrationAction(ctx context.Context, options Options, before Prepared, action string) error {
	if err := validateOptions(&options); err != nil {
		return err
	}
	if options.ProspectiveFiles != nil {
		return errors.New("owner filesystem action requires actual files")
	}
	if err := validatePrepared(before); err != nil {
		return err
	}
	shape, adopt, legacy := preparedLegacyState(before.State)
	if !legacy {
		return errors.New("owner filesystem action requires prepared legacy state")
	}
	if err := validateRecoveryToken(options.RecoveryToken); err != nil {
		return err
	}
	options.TerminalCleanup = action == "cleanup-owned-archives"
	old, current := registrationPaths(options, shape)
	if _, err := preparedRegistrationArchive(options, before); err != nil {
		return err
	}
	controller := filepath.Join(options.DataHome, "e2e", "controllers", LegacyYard)
	checkOriginal := func() error {
		actual, err := inspectRegistration(options)
		if err != nil {
			return err
		}
		if !equivalentLegacyState(actual.state, before.State) || actual.oldRegistration != old {
			return errors.New("original owner registration shape changed")
		}
		if err := registrationMatches(old, before); err != nil {
			return err
		}
		if err := validatePreparedAuxiliaryState(options, old, current, before, auxiliaryAtSource); err != nil {
			return err
		}
		projects, err := inspectProjects(ctx, options)
		if err != nil {
			return err
		}
		if !expectedOwnerProjects(projects, adopt) && !(options.AllowMissingLegacyProject && !adopt && !projects.legacy && !projects.current) {
			return errors.New("original owner project topology changed")
		}
		return validatePreparedImageNamespaces(ctx, options, projects, before)
	}
	switch action {
	case "validate-original", "verify-compensated":
		return checkOriginal()
	case "publish-registration":
		if err := validatePreparedAuxiliaryState(options, old, current, before, auxiliaryInProgress); err != nil {
			return err
		}
		if exists, err := ownedCurrentRegistration(current); err != nil {
			return err
		} else if exists {
			if err := registrationMatches(current, before); err != nil {
				return err
			}
			return repairAuthorizedRegistrationPublication(options, current, before)
		}
		if err := validateAuthorizedCurrentStateDirectory(current, adopt, options.RecoveryToken); err != nil {
			return err
		}
		source, err := openBoundPublishedRegistration(old, before)
		if err != nil {
			return err
		}
		defer source.close()
		if err := copyBoundRegistration(options, source, current, before); err != nil {
			return err
		}
		return registrationMatches(current, before)
	case "remove-empty-flat":
		return removeEmptyFlatLegacyDirectory(old, before)
	case "move-overrides":
		var retained *boundOverrides
		if hasPreparedOverrides(before.State) {
			var err error
			retained, err = openBoundOverrides(options, filepath.Join(filepath.Dir(old), "overrides"))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if retained != nil {
				defer retained.close()
				if retained.digest != before.OverridesDigest {
					return errors.New("original owner overrides changed")
				}
			}
		}
		if err := movePreparedAuxiliaryState(options, old, current, before, retained); err != nil {
			return err
		}
		return preparedActionOverrides(options, old, current, before, true)
	case "archive-registration":
		registration, err := openBoundPublishedRegistration(old, before)
		if errors.Is(err, os.ErrNotExist) {
			err = repairPreparedRegistrationArchive(options, before)
		} else if err == nil {
			defer registration.close()
			err = archivePreparedRegistration(options, registration, before)
		}
		if err != nil {
			return err
		}
		return validatePreparedRegistrationArchive(options, old, before, auxiliaryDesired)
	case "archive-controller":
		err := archivePreparedController(options, controller, before)
		if err == nil {
			err = validatePreparedControllerState(options, before, auxiliaryDesired)
		}
		if err != nil {
			// As in Commit, a failed durable archive is resumable, not a grant
			// to compensate an operation with no controller-restore primitive.
			return resumableOwnerMutationError{err: err}
		}
		return nil
	case "restore-registration":
		return restorePreparedRegistration(options, before)
	case "restore-overrides":
		if err := restorePreparedAuxiliaryState(options, old, current, before); err != nil {
			return err
		}
		return preparedActionOverrides(options, old, current, before, false)
	case "park-current-registration":
		if err := registrationMatches(old, before); err != nil {
			return err
		}
		state, err := openBoundRegistration(current, before)
		if errors.Is(err, os.ErrNotExist) {
			name, nameErr := ownerRecoveryName("registration-scratch", options.RecoveryToken)
			if nameErr != nil {
				return nameErr
			}
			parked, parkedErr := openBoundRegistration(filepath.Join(registrationStagingParent(current), name), before)
			if errors.Is(parkedErr, os.ErrNotExist) {
				return nil
			}
			if parkedErr == nil {
				parked.close()
			}
			return parkedErr
		}
		if err != nil {
			return err
		}
		defer state.close()
		return parkBoundRegistration(options, state, current, before)
	case "cleanup-owned-archives":
		if err := Verify(options, before); err != nil {
			return err
		}
		projects, err := inspectProjects(ctx, options)
		if err != nil {
			return err
		}
		if projects.legacy || !projects.current {
			return errors.New("owner archive cleanup requires canonical project topology")
		}
		if err := validatePreparedImageNamespaces(ctx, options, projects, before); err != nil {
			return err
		}
		return cleanupPreparedOwnerArchives(options, before)
	default:
		return errors.New("unknown owner filesystem action")
	}
}

func preparedActionOverrides(options Options, old, current string, before Prepared, moved bool) error {
	if filepath.Base(old) != "config.env" {
		return nil
	}
	wantOld, wantCurrent := before.OverridesDigest, absentStateDigest()
	if moved {
		wantOld, wantCurrent = wantCurrent, wantOld
	}
	for path, want := range map[string]string{
		filepath.Join(filepath.Dir(old), "overrides"):     wantOld,
		filepath.Join(filepath.Dir(current), "overrides"): wantCurrent,
	} {
		got, err := overridesStateDigestWithOptions(options, path)
		if err != nil {
			return err
		}
		if got != want {
			return errors.New("owner overrides action did not reach its exact postcondition")
		}
	}
	return nil
}
