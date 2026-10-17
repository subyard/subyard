package migration

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func previousRuntimeOptions(options ReleaseOptions) (ReleaseOptions, error) {
	if options.RuntimeRoot == "" || !filepath.IsAbs(options.RuntimeRoot) {
		return ReleaseOptions{}, errors.New("absolute runtime root is required for power reconciler rollback")
	}
	root, err := filepath.EvalSymlinks(options.RuntimeRoot)
	if err != nil {
		return ReleaseOptions{}, fmt.Errorf("resolve runtime root: %w", err)
	}
	previous, err := filepath.EvalSymlinks(filepath.Join(root, "previous"))
	if err != nil {
		return ReleaseOptions{}, fmt.Errorf("resolve previous runtime: %w", err)
	}
	relative, err := filepath.Rel(filepath.Join(root, "releases"), previous)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return ReleaseOptions{}, errors.New("previous runtime escapes the release store")
	}
	result := options
	result.RepositoryRoot = previous
	result.Executable = filepath.Join(previous, "bin", "yard-engine")
	executableInfo, err := os.Lstat(result.Executable)
	if err != nil {
		return ReleaseOptions{}, fmt.Errorf("inspect previous runtime executable: %w", err)
	}
	if !executableInfo.Mode().IsRegular() {
		return ReleaseOptions{}, errors.New("previous runtime executable is not a regular file")
	}
	if executableInfo.Mode().Perm()&0o111 == 0 {
		return ReleaseOptions{}, errors.New("previous runtime executable is not executable")
	}
	result.Environment = append([]string(nil), result.Environment...)
	result.Environment = removePowerReconcilerEnvironment(
		result.Environment,
		"SUBYARD_POWER_MIGRATION_PAYLOAD_ROOT",
	)
	result.Environment = replacePowerReconcilerEnvironment(
		result.Environment,
		"SUBYARD_REPOSITORY_ROOT",
		previous,
	)
	result.Environment = replacePowerReconcilerEnvironment(
		result.Environment,
		"SUBYARD_CONFIG_DIR",
		filepath.Join(previous, "config"),
	)
	return result, nil
}

func replacePowerReconcilerEnvironment(environment []string, name, value string) []string {
	environment = removePowerReconcilerEnvironment(environment, name)
	return append(environment, name+"="+value)
}

func removePowerReconcilerEnvironment(environment []string, name string) []string {
	prefix := name + "="
	filtered := environment[:0]
	for _, assignment := range environment {
		if !strings.HasPrefix(assignment, prefix) {
			filtered = append(filtered, assignment)
		}
	}
	return filtered
}
