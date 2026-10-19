package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"slices"
)

func hostInstallGeneration(resource, owner string) (string, error) {
	path := filepath.Join(filepath.Dir(resource), "."+filepath.Base(resource)+".native-custody.json")
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return "0", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 || stat.Nlink != 1 || stat.Uid != 0 || info.Size() > 4096 {
		return "", errors.New("unsafe host installer generation")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", err
	}
	var value struct {
		Generation string `json:"generation"`
		Owner      string `json:"owner"`
		Version    int    `json:"version"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || value.Version != 1 || value.Owner != owner || !profileRuntimeDigest.MatchString(value.Generation) {
		return "", errors.New("invalid host installer generation")
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(append(canonical, '\n'), raw) {
		return "", errors.New("noncanonical host installer generation")
	}
	return value.Generation, nil
}

func (runtime Runtime) hostPowerResourcePaths() []string {
	return []string{runtime.environmentDefault("SUBYARD_POWER_RECONCILER_PATH", "/usr/local/libexec/subyard/yard-boot-reconcile"), runtime.environmentDefault("SUBYARD_POWER_UNIT_PATH", "/etc/systemd/system/subyard-power-reconcile.service")}
}
func (runtime Runtime) CaptureHostPowerCustody() ([]ports.NativeResourceGeneration, error) {
	paths := runtime.hostPowerResourcePaths()
	roles := []string{"engine", "service"}
	result := make([]ports.NativeResourceGeneration, len(paths))
	if paths[0] == paths[1] {
		return nil, errors.New("overlapping host installer roles")
	}
	for index, path := range paths {
		value, err := hostInstallGeneration(path, "host-power")
		if err != nil {
			return nil, err
		}
		result[index] = ports.NativeResourceGeneration{Role: roles[index], Value: value}
	}
	return result, nil
}
func hostPowerVector(expected []ports.NativeResourceGeneration) (string, error) {
	if len(expected) != 2 || expected[0].Role != "engine" || expected[1].Role != "service" {
		return "", domain.ErrPlanStale
	}
	for _, entry := range expected {
		if !validCustodyGeneration(entry.Value) {
			return "", domain.ErrPlanStale
		}
	}
	raw, err := json.Marshal(expected)
	return string(raw), err
}
func (runtime Runtime) ApplyPreparedHostPowerRuntimeCustody(ctx context.Context, prepared *PreparedHostPower, expected []ports.NativeResourceGeneration, generation string) error {
	vector, err := hostPowerVector(expected)
	if err != nil || !profileRuntimeDigest.MatchString(generation) {
		return domain.ErrPlanStale
	}
	return runtime.applyPreparedHostPowerRuntime(ctx, prepared, map[string]string{"SUBYARD_NATIVE_EXPECTED_VECTOR": vector, "SUBYARD_NATIVE_GENERATION": generation})
}
func (runtime Runtime) FencePreparedHostPowerRuntime(ctx context.Context, prepared *PreparedHostPower, expected []ports.NativeResourceGeneration, generation, successor string) error {
	return runtime.fencePreparedHostPowerRuntime(ctx, prepared, expected, generation, successor)
}
func (runtime Runtime) fencePreparedHostPowerRuntime(ctx context.Context, prepared *PreparedHostPower, expected []ports.NativeResourceGeneration, generation, successor string, cached ...Runtime) error {
	vector, err := hostPowerVector(expected)
	if err != nil || prepared == nil || prepared.runtime != runtime.configRuntimeBinding() || !profileRuntimeDigest.MatchString(generation) || !profileRuntimeDigest.MatchString(successor) || successor == generation || slices.ContainsFunc(expected, func(entry ports.NativeResourceGeneration) bool { return entry.Value == successor }) {
		return domain.ErrPlanStale
	}
	source, err := runtime.SelectedRuntimeInputs("core-guest-runtime")
	if err != nil {
		return err
	}
	before, err := runtime.hostPowerFiles()
	if err != nil {
		return err
	}
	if source != prepared.source || before != prepared.before {
		return domain.ErrPlanStale
	}
	execution := runtime
	if len(cached) == 1 {
		execution = cached[0]
	}
	if err := execution.runScriptEnvironment(ctx, runtime.Stderr, map[string]string{"SUBYARD_PREPARED_INSTALLED_CLEANUP": "1", "SUBYARD_USER": prepared.operator, "SUBYARD_NATIVE_EXPECTED_VECTOR": vector, "SUBYARD_NATIVE_GENERATION": generation, "SUBYARD_NATIVE_SUCCESSOR": successor, "SUBYARD_NATIVE_INSTALLED_BEFORE": prepared.before}, "install-power-reconciler.sh", "--yes", "--native-fence"); err != nil {
		return err
	}
	current, err := runtime.CaptureHostPowerCustody()
	if err != nil || len(current) != len(expected) {
		return errors.New("host installer fence was not verified")
	}
	for _, entry := range current {
		if entry.Value != successor {
			return errors.New("host installer fence was not verified")
		}
	}
	return nil
}
