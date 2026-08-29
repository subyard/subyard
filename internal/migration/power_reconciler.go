package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/systemdunit"
)

const (
	powerReconcilerAbsent    = "absent"
	powerReconcilerInstalled = "installed"
)

func preparePowerReconciler(options ReleaseOptions) (string, error) {
	reconciler := powerReconcilerPath(options, "SUBYARD_POWER_RECONCILER_PATH",
		"/usr/local/libexec/subyard/yard-boot-reconcile")
	unit := powerReconcilerPath(options, "SUBYARD_POWER_UNIT_PATH",
		"/etc/systemd/system/subyard-power-reconcile.service")
	present := false
	for _, path := range []string{reconciler, unit} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("power reconciler path is a symbolic link: %s", path)
		}
		present = true
	}
	if present {
		return powerReconcilerInstalled, nil
	}
	return powerReconcilerAbsent, nil
}

func verifyPowerReconciler(
	ctx context.Context,
	options ReleaseOptions,
	before string,
) error {
	return verifyPowerReconcilerManagerState(ctx, options, before, false)
}

func verifyRestoredPowerReconciler(
	ctx context.Context,
	options ReleaseOptions,
	before string,
) error {
	return verifyPowerReconcilerManagerState(ctx, options, before, true)
}

func verifyPowerReconcilerManagerState(
	ctx context.Context,
	options ReleaseOptions,
	before string,
	allowSettledPrevious bool,
) error {
	if err := validatePowerReconcilerState(before); err != nil {
		return err
	}
	observed, err := preparePowerReconciler(options)
	if err != nil {
		return err
	}
	if observed != before {
		return fmt.Errorf("host power reconciler changed from %s to %s during runtime migration",
			before, observed)
	}
	if before == powerReconcilerAbsent {
		return nil
	}

	executable := powerReconcilerExecutable(options)
	reconciler := powerReconcilerPath(options, "SUBYARD_POWER_RECONCILER_PATH",
		"/usr/local/libexec/subyard/yard-boot-reconcile")
	if err := equalRegularFiles(executable, reconciler, true); err != nil {
		return fmt.Errorf("installed host power reconciler is stale: %w", err)
	}

	template, err := os.ReadFile(filepath.Join(options.RepositoryRoot,
		"config", "systemd", "subyard-power-reconcile.service.in"))
	if err != nil {
		return fmt.Errorf("read host power reconciler unit template: %w", err)
	}
	expected := bytes.ReplaceAll(template, []byte("@SUBYARD_POWER_RECONCILER@"), []byte(reconciler))
	unit := powerReconcilerPath(options, "SUBYARD_POWER_UNIT_PATH",
		"/etc/systemd/system/subyard-power-reconcile.service")
	actual, err := readRegularFile(unit, false)
	if err != nil {
		return fmt.Errorf("read installed host power reconciler unit: %w", err)
	}
	if !bytes.Equal(actual, expected) {
		return errors.New("installed host power reconciler unit is stale")
	}

	managerVerifier := systemdunit.RequireFreshLoaded
	if allowSettledPrevious {
		managerVerifier = systemdunit.RequireSettledPrevious
	}
	if err := managerVerifier(ctx, "systemctl", options.Environment, filepath.Base(unit)); err != nil {
		return fmt.Errorf("installed host power reconciler manager state is stale: %w", err)
	}
	command := exec.CommandContext(ctx, "systemctl", "is-enabled", "--quiet", filepath.Base(unit))
	command.Env = options.Environment
	command.Stdin, command.Stdout, command.Stderr = nil, io.Discard, io.Discard
	if err := command.Run(); err != nil {
		return errors.New("installed host power reconciler unit is not enabled")
	}
	return nil
}

func validatePowerReconcilerState(state string) error {
	if state != powerReconcilerAbsent && state != powerReconcilerInstalled {
		return fmt.Errorf("invalid prepared host power reconciler state %q", state)
	}
	return nil
}

func powerReconcilerExecutable(options ReleaseOptions) string {
	if options.Executable != "" {
		return options.Executable
	}
	return filepath.Join(options.RepositoryRoot, "bin", "yard-engine")
}

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

func powerReconcilerEnvironment(options ReleaseOptions, repositoryRoot string) []string {
	environment := append([]string(nil), options.Environment...)
	for name, value := range map[string]string{
		"SUBYARD_INTERNAL_MIGRATION_CHILD": "1",
		"SUBYARD_REPOSITORY_ROOT":          repositoryRoot,
		"SUBYARD_CONFIG_DIR":               filepath.Join(repositoryRoot, "config"),
	} {
		environment = replacePowerReconcilerEnvironment(environment, name, value)
	}
	return environment
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

func powerReconcilerPath(options ReleaseOptions, name, fallback string) string {
	prefix := name + "="
	for index := len(options.Environment) - 1; index >= 0; index-- {
		if value, ok := strings.CutPrefix(options.Environment[index], prefix); ok && value != "" {
			return value
		}
	}
	return fallback
}

func equalRegularFiles(expected, actual string, executable bool) error {
	expectedPayload, err := readRegularFile(expected, executable)
	if err != nil {
		return err
	}
	actualPayload, err := readRegularFile(actual, executable)
	if err != nil {
		return err
	}
	if !bytes.Equal(expectedPayload, actualPayload) {
		return errors.New("file contents differ")
	}
	return nil
}

func readRegularFile(path string, executable bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("not a regular non-symlink file")
	}
	if executable && info.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("file is not executable")
	}
	return os.ReadFile(path)
}
