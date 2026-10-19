package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/domain"
)

// PreparedHostPower retains the operator, source and exact installed file set
// assessed before consent. It does not select release work or start yards.
type PreparedHostPower struct {
	operator, source, before, runtime, binding string
	engineSource, engineBefore, engineSHA      string
	engine                                     *os.File
	closeOnce                                  sync.Once
}

func (p *PreparedHostPower) Close() error {
	if p == nil {
		return nil
	}
	var err error
	p.closeOnce.Do(func() {
		if p.engine != nil {
			err = p.engine.Close()
		}
	})
	return err
}

func (p *PreparedHostPower) Binding() string {
	if p == nil {
		return ""
	}
	return p.binding
}

func (p *PreparedHostPower) NativeBefore() string {
	if p == nil {
		return ""
	}
	return p.before
}

func (runtime Runtime) hostPowerFiles() (string, error) {
	return nativeInstalledFiles([]string{
		runtime.environmentDefault("SUBYARD_POWER_RECONCILER_PATH", "/usr/local/libexec/subyard/yard-boot-reconcile"),
		runtime.environmentDefault("SUBYARD_POWER_UNIT_PATH", "/etc/systemd/system/subyard-power-reconcile.service"),
	})
}

func nativeInstalledFiles(paths []string) (string, error) {
	return nativeInstalledFilesLabeled(paths, func(path string) string { return path })
}

func nativeInstalledFilesLabeled(paths []string, label func(string) string) (string, error) {
	facts := map[string]any{}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			facts[label(path)] = "absent"
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("unsafe installed host power file")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return "", errors.New("host power file identity unavailable")
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		facts[label(path)] = []any{stat.Dev, stat.Ino, stat.Uid, stat.Gid, stat.Mode, stat.Size, stat.Mtim, stat.Ctim, fmt.Sprintf("%x", sha256.Sum256(payload))}
	}
	raw, err := json.Marshal(facts)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

func (runtime Runtime) PrepareHostPowerRuntime(context.Context) (*PreparedHostPower, error) {
	operator, err := runtime.operatorIdentity()
	if err != nil {
		return nil, err
	}
	source, err := runtime.SelectedRuntimeInputs("core-guest-runtime")
	if err != nil {
		return nil, err
	}
	before, err := runtime.hostPowerFiles()
	if err != nil {
		return nil, err
	}
	p := &PreparedHostPower{operator: operator.Username, source: source, before: before, runtime: runtime.configRuntimeBinding()}
	p.engineSource = runtime.environmentValue("SUBYARD_POWER_ENGINE_SOURCE")
	p.engine, err = os.OpenFile(p.engineSource, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			p.Close()
		}
	}()
	info, err := p.engine.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Size() > 128<<20 {
		return nil, errors.New("unsafe prepared host engine")
	}
	p.engineBefore, err = nativeInstalledFiles([]string{p.engineSource})
	if err != nil {
		return nil, err
	}
	engineDigest, err := p.engineDigest()
	if err != nil {
		return nil, err
	}
	p.engineSHA = engineDigest
	sealed, err := testvmsruntime.SealPreparedEngine(p.engine, engineDigest)
	if err != nil {
		return nil, err
	}
	p.engine.Close()
	p.engine = sealed
	currentEngineBefore, err := nativeInstalledFiles([]string{p.engineSource})
	if err != nil || currentEngineBefore != p.engineBefore {
		return nil, domain.ErrPlanStale
	}
	engineBinding, err := nativeInstalledFilesLabeled([]string{p.engineSource}, runtime.bindingPath)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(struct {
		Operator, Source, Runtime  string
		EngineBefore, EngineDigest string
		Yards                      []domain.Context
	}{p.operator, p.source, p.runtime, engineBinding, engineDigest, runtime.bindingYards()})
	if err != nil {
		return nil, err
	}
	p.binding = fmt.Sprintf("%x", sha256.Sum256(raw))
	ok = true
	return p, nil
}

func (p *PreparedHostPower) engineDigest() (string, error) {
	if p == nil || p.engine == nil {
		return "", domain.ErrPlanStale
	}
	hasher := sha256.New()
	count, err := io.Copy(hasher, io.NewSectionReader(p.engine, 0, (128<<20)+1))
	if err != nil || count > 128<<20 {
		return "", errors.New("prepared host engine bound")
	}
	return fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

func (runtime Runtime) ApplyPreparedHostPowerRuntime(ctx context.Context, prepared *PreparedHostPower) error {
	return runtime.applyPreparedHostPowerRuntime(ctx, prepared, nil)
}

func (runtime Runtime) applyPreparedHostPowerRuntime(ctx context.Context, prepared *PreparedHostPower, environment map[string]string, cached ...Runtime) error {
	if prepared == nil || prepared.runtime != runtime.configRuntimeBinding() {
		return domain.ErrPlanStale
	}
	source, err := runtime.SelectedRuntimeInputs("core-guest-runtime")
	if err != nil {
		return err
	}
	if source != prepared.source {
		return domain.ErrPlanStale
	}
	engineBefore, err := nativeInstalledFiles([]string{prepared.engineSource})
	if err != nil {
		return err
	}
	if engineBefore != prepared.engineBefore {
		return domain.ErrPlanStale
	}
	engineSHA, err := prepared.engineDigest()
	if err != nil || engineSHA != prepared.engineSHA {
		return domain.ErrPlanStale
	}
	execution := runtime
	if len(cached) == 1 {
		execution = cached[0]
	}
	if execution.powerReconcilerConverged(ctx) {
		return nil
	}
	before, err := runtime.hostPowerFiles()
	if err != nil {
		return err
	}
	if before != prepared.before {
		return domain.ErrPlanStale
	}
	if environment == nil {
		environment = map[string]string{}
	}
	if len(cached) == 1 {
		environment["SUBYARD_PREPARED_INSTALLED_CLEANUP"] = "1"
	}
	environment["SUBYARD_USER"] = prepared.operator
	environment["SUBYARD_NATIVE_INSTALLED_BEFORE"] = prepared.before
	environment["SUBYARD_POWER_ENGINE_SOURCE"] = fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), prepared.engine.Fd())
	environment["SUBYARD_NATIVE_ENGINE_SHA256"] = prepared.engineSHA
	if err := execution.runScriptEnvironment(ctx, runtime.Stderr, environment, "install-power-reconciler.sh", "--yes"); err != nil {
		return err
	}
	if !execution.powerReconcilerConverged(ctx) {
		return errors.New("host power runtime did not converge")
	}
	return nil
}
