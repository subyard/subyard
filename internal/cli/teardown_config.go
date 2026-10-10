package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

type teardownConfigDirectory struct {
	Path           string
	Device, Inode  uint64
	Mode, UID, GID uint32
}

type teardownConfigSnapshot struct {
	Artifacts   []teardownArtifact
	Directories []teardownConfigDirectory
	Marker      teardownArtifact
	Reset       bool
}

func captureTeardownConfig(yard domain.Context) (*teardownConfigSnapshot, error) {
	root, name := yard.Paths.ConfigHome, yard.YardName
	paths, err := config.YardResetPaths(root, name)
	if err != nil {
		return nil, err
	}
	if len(paths) > 256 {
		return nil, errors.New("local yard settings exceed the bounded reset artifact inventory")
	}
	reset, err := config.YardFallbackReset(root, name)
	if err != nil {
		return nil, err
	}
	snapshot := &teardownConfigSnapshot{Reset: reset, Marker: teardownArtifact{Path: config.YardResetMarkerPath(root, name)}}
	snapshot.Marker.Binding, err = teardownArtifactBinding(snapshot.Marker.Path)
	if err != nil {
		return nil, err
	}
	for _, directory := range []string{root, filepath.Join(root, "yards"), filepath.Join(root, "yards", name)} {
		info, err := os.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode().Perm()&0o022 != 0 || stat.Uid != uint32(os.Getuid()) {
			return nil, errors.New("yard reset namespace must be an operator-owned non-writable real directory")
		}
		snapshot.Directories = append(snapshot.Directories, teardownConfigDirectory{directory, uint64(stat.Dev), stat.Ino, stat.Mode, stat.Uid, stat.Gid})
	}
	for _, path := range paths {
		binding, err := teardownArtifactBinding(path)
		if err != nil {
			return nil, err
		}
		snapshot.Artifacts = append(snapshot.Artifacts, teardownArtifact{Path: path, Binding: binding})
	}
	payload, err := json.Marshal(snapshot.Artifacts)
	if err != nil {
		return nil, err
	}
	if len(payload) > 64<<10 {
		return nil, errors.New("local yard settings exceed the reset artifact guard size limit")
	}
	return snapshot, nil
}

func checkTeardownConfig(before, after *teardownConfigSnapshot) error {
	if before == nil || after == nil {
		return errors.New("captured local configuration reset scope is required")
	}
	if before.Marker != after.Marker {
		return fmt.Errorf("%w: selected local configuration reset scope changed", domain.ErrPlanStale)
	}
	approvedArtifacts := make(map[string]string, len(before.Artifacts))
	for _, artifact := range before.Artifacts {
		approvedArtifacts[artifact.Path] = artifact.Binding
	}
	for _, artifact := range after.Artifacts {
		binding, approved := approvedArtifacts[artifact.Path]
		if !approved || artifact.Binding != "" && artifact.Binding != binding {
			return fmt.Errorf("%w: selected local configuration reset scope changed", domain.ErrPlanStale)
		}
	}
	for _, approved := range before.Directories {
		found := false
		for _, current := range after.Directories {
			if approved == current {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%w: local configuration namespace changed", domain.ErrPlanStale)
		}
	}
	return nil
}

func (cli *CLI) resetTeardownConfiguration(ctx context.Context, loaded config.Loaded, execution *teardownExecution) error {
	yard := loaded.Context
	registration := func(path string) bool {
		return path == filepath.Join(yard.Paths.ConfigHome, "yards", yard.YardName, "config.env") || path == filepath.Join(yard.Paths.ConfigHome, "yards", yard.YardName+".env")
	}
	remove := func(registrations bool) error {
		payload, err := json.Marshal(execution.configSnapshot.Artifacts)
		if err != nil {
			return err
		}
		// The flat shadow goes first; the canonical registration remains loadable
		// through every earlier failure, with its original physical identity.
		for _, artifact := range execution.configSnapshot.Artifacts {
			if registration(artifact.Path) != registrations {
				continue
			}
			child := exec.CommandContext(ctx, "python3", filepath.Join(cli.options.RepositoryRoot, "scripts", "lib", "teardown-plan.py"), "remove-artifact", artifact.Path)
			child.Env = append(os.Environ(), "SUBYARD_TEARDOWN_ARTIFACTS="+string(payload))
			if err := child.Run(); err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) && exit.ExitCode() == 75 {
					return domain.ErrPlanStale
				}
				return fmt.Errorf("remove approved local yard settings: %w", err)
			}
		}
		return nil
	}
	err := config.ResetYardConfiguration(yard.Paths.ConfigHome, yard.YardName, func() error {
		current, err := captureTeardownConfig(yard)
		if err != nil {
			return err
		}
		return checkTeardownConfig(execution.configSnapshot, current)
	}, func() error { return remove(false) }, func() error { return remove(true) })
	if err != nil {
		return err
	}
	reset, err := config.YardFallbackReset(yard.Paths.ConfigHome, yard.YardName)
	if err != nil {
		return err
	}
	if !reset {
		return errors.New("yard reset ownership did not converge")
	}
	paths, err := config.YardResetPaths(yard.Paths.ConfigHome, yard.YardName)
	if err != nil {
		return err
	}
	for _, path := range paths {
		binding, err := teardownArtifactBinding(path)
		if err != nil {
			return err
		}
		if binding != "" {
			return errors.New("local yard settings remain after reset")
		}
	}
	return nil
}
