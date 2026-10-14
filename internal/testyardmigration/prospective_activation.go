package testyardmigration

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/application"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
)

// ActivationProjection describes only the registration and override publication
// owned by the exact prepared migration. Backend activity is assessed separately.
type ActivationProjection struct {
	Sources map[string]string
	Removed []string
}

func PrepareActivationProjection(options Options, before Prepared, read func(string) (config.PersistentFileSnapshot, error)) (ActivationProjection, error) {
	if err := validateOptions(&options); err != nil {
		return ActivationProjection{}, err
	}
	if err := validatePrepared(before); err != nil {
		return ActivationProjection{}, err
	}
	projection := ActivationProjection{Sources: map[string]string{}}
	if before.State == StateAbsent || before.State == StateCurrent {
		return projection, nil
	}
	shape, _, migrates := preparedLegacyState(before.State)
	if !migrates || read == nil {
		return projection, errors.New("prepared owner projection is unavailable")
	}
	oldPath, currentPath := registrationPaths(options, shape)
	source := oldPath
	snapshot, err := read(source)
	if err != nil {
		return projection, err
	}
	if !snapshot.Exists {
		source = currentPath
		snapshot, err = read(source)
	}
	if err != nil {
		return projection, err
	}
	if !snapshot.Exists || fmt.Sprintf("%x", sha256.Sum256(snapshot.Content)) != before.RegistrationDigest {
		return projection, errors.New("prepared owner registration changed before activation projection")
	}
	projection.Sources[currentPath] = source
	projection.Removed = append(projection.Removed, oldPath)
	if filepath.Base(oldPath) != "config.env" {
		return projection, nil
	}
	oldRoot, currentRoot := filepath.Join(filepath.Dir(oldPath), "overrides"), filepath.Join(filepath.Dir(currentPath), "overrides")
	oldDigest, err := overridesStateDigestWithOptions(options, oldRoot)
	if err != nil {
		return projection, err
	}
	currentDigest, err := overridesStateDigestWithOptions(options, currentRoot)
	if err != nil {
		return projection, err
	}
	absent := absentStateDigest()
	if before.OverridesDigest == absent {
		if oldDigest != absent || currentDigest != absent {
			return projection, errors.New("prepared owner overrides changed before activation projection")
		}
		return projection, nil
	}
	sourceRoot := oldRoot
	if oldDigest == absent && currentDigest == before.OverridesDigest {
		sourceRoot = currentRoot
	} else if oldDigest != before.OverridesDigest || currentDigest != absent {
		return projection, errors.New("prepared owner overrides changed before activation projection")
	}
	err = filepath.WalkDir(sourceRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("prepared owner override cannot be a symlink")
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil || strings.HasPrefix(relative, "..") {
			return errors.New("prepared owner override path is unsafe")
		}
		projection.Sources[filepath.Join(currentRoot, relative)] = path
		projection.Removed = append(projection.Removed, filepath.Join(oldRoot, relative))
		return nil
	})
	slices.Sort(projection.Removed)
	projection.Removed = slices.Compact(projection.Removed)
	return projection, err
}

// PrepareProjectedActivationApplicability projects only the backend creation
// mandated by Commit. Adoption/current state retains guarded native backend facts.
func PrepareProjectedActivationApplicability(ctx context.Context, options Options, before Prepared, loaded config.Loaded) (BrokerRuntimeState, State, error) {
	if err := validateOptions(&options); err != nil {
		return "", "", err
	}
	if err := validatePrepared(before); err != nil {
		return "", "", err
	}
	_, adopt, migrates := preparedLegacyState(before.State)
	if migrates && !adopt {
		broker := BrokerRuntimeInactive
		if loaded.Context.NestedE2EVMs && application.InitialPower(loaded.Context) == application.PowerRunning {
			broker = BrokerRuntimeActive
		}
		return broker, StateCurrent, nil
	}
	if before.State == StateAbsent {
		return BrokerRuntimeAbsent, StateAbsent, nil
	}
	route, err := prepareCanonicalRouteActivationBackend(ctx, options)
	if err != nil {
		return "", "", err
	}
	broker, err := inspectBrokerRuntimeLoaded(ctx, options, CurrentYard, loaded)
	return broker.state, route, err
}
