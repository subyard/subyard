package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/domain"
)

// OwnerHostPowerRecord retains the original host-power consent without an
// executable body or a caller-selected asset path. Byte columns preserve Go
// strings exactly when the enclosing native record is encoded as JSON.
type OwnerHostPowerRecord struct {
	Present                                          bool
	Operator, Source, NativeBefore, Runtime, Binding []byte
	EngineRole, EngineDigest, EngineIdentity         string
	Repository                                       OwnerHostPowerRepositoryIdentity
}

// OwnerHostPowerRepositoryIdentity identifies the pinned directory independently
// of the process-local descriptor used to reach it. Directory timestamps are
// deliberately excluded: publishing files does not replace the directory.
type OwnerHostPowerRepositoryIdentity struct {
	Device, Inode  uint64
	UID, GID, Mode uint32
}

const ownerHostPowerEngineRole = "yard-engine"

func (runtime Runtime) ownerHostPowerRepository() (OwnerHostPowerRepositoryIdentity, error) {
	info, err := os.Stat(runtime.RepositoryRoot)
	if err != nil {
		return OwnerHostPowerRepositoryIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return OwnerHostPowerRepositoryIdentity{}, domain.ErrPlanStale
	}
	return OwnerHostPowerRepositoryIdentity{stat.Dev, stat.Ino, stat.Uid, stat.Gid, stat.Mode}, nil
}

func (runtime Runtime) ownerHostPowerEngineSource() (string, error) {
	fixed := filepath.Join(runtime.RepositoryRoot, "bin", ownerHostPowerEngineRole)
	source := runtime.environmentValue("SUBYARD_POWER_ENGINE_SOURCE")
	// A child may need the owning process's explicit descriptor spelling. Only
	// this process's alias of this exact retained root is interchangeable.
	alias := fixed
	if strings.HasPrefix(runtime.RepositoryRoot, "/proc/self/fd/") {
		alias = strings.Replace(fixed, "/proc/self/", "/proc/"+strconv.Itoa(os.Getpid())+"/", 1)
	}
	if source != fixed && source != alias {
		return "", domain.ErrPlanStale
	}
	return source, nil
}

func (runtime Runtime) ownerHostPowerBinding(p *PreparedHostPower) (string, error) {
	engineBinding, err := nativeInstalledFilesLabeled([]string{p.engineSource}, runtime.bindingPath)
	if err != nil {
		return "", err
	}
	// Keep the original PrepareHostPowerRuntime binding grammar and field order.
	raw, err := json.Marshal(struct {
		Operator, Source, Runtime  string
		EngineBefore, EngineDigest string
		Yards                      []domain.Context
	}{p.operator, p.source, p.runtime, engineBinding, p.engineSHA, runtime.bindingYards()})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

func ownerHostPowerDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

// RecordOwnerHostPower checks the original prepared source and retains only the
// fixed engine role and its original identity. A nil preparation is absent.
func (runtime Runtime) RecordOwnerHostPower(p *PreparedHostPower) (OwnerHostPowerRecord, error) {
	if p == nil {
		return OwnerHostPowerRecord{}, nil
	}
	source, err := runtime.ownerHostPowerEngineSource()
	if err != nil || p.engineSource != source || p.runtime != runtime.configRuntimeBinding() {
		return OwnerHostPowerRecord{}, domain.ErrPlanStale
	}
	operator, err := runtime.operatorIdentity()
	if err != nil {
		return OwnerHostPowerRecord{}, err
	}
	inputs, err := runtime.SelectedRuntimeInputs("core-guest-runtime")
	if err != nil {
		return OwnerHostPowerRecord{}, err
	}
	before, err := nativeInstalledFiles([]string{source})
	if err != nil {
		return OwnerHostPowerRecord{}, err
	}
	digest, err := p.engineDigest()
	if err != nil || digest != p.engineSHA || before != p.engineBefore || operator.Username != p.operator || inputs != p.source {
		return OwnerHostPowerRecord{}, domain.ErrPlanStale
	}
	binding, err := runtime.ownerHostPowerBinding(p)
	if err != nil || binding != p.binding || !ownerHostPowerDigest(p.before) {
		return OwnerHostPowerRecord{}, domain.ErrPlanStale
	}
	identity, err := nativeInstalledFilesLabeled([]string{source}, func(string) string { return ownerHostPowerEngineRole })
	if err != nil {
		return OwnerHostPowerRecord{}, err
	}
	repository, err := runtime.ownerHostPowerRepository()
	if err != nil {
		return OwnerHostPowerRecord{}, err
	}
	// Do not relabel a source that changed during the record checks.
	current, err := nativeInstalledFiles([]string{source})
	if err != nil || current != p.engineBefore {
		return OwnerHostPowerRecord{}, domain.ErrPlanStale
	}
	return OwnerHostPowerRecord{true, []byte(p.operator), []byte(p.source), []byte(p.before), []byte(p.runtime), []byte(p.binding), ownerHostPowerEngineRole, p.engineSHA, identity, repository}, nil
}

// RestoreOwnerHostPower reopens only the pinned root's fixed engine asset and
// seals it against the original digest. Installed files are not reassessed:
// their original before-state remains the apply boundary's precondition.
func (runtime Runtime) RestoreOwnerHostPower(ctx context.Context, record OwnerHostPowerRecord) (*PreparedHostPower, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !record.Present {
		if len(record.Operator)+len(record.Source)+len(record.NativeBefore)+len(record.Runtime)+len(record.Binding) != 0 || record.EngineRole != "" || record.EngineDigest != "" || record.EngineIdentity != "" || record.Repository != (OwnerHostPowerRepositoryIdentity{}) {
			return nil, domain.ErrPlanStale
		}
		return nil, nil
	}
	if record.EngineRole != ownerHostPowerEngineRole || len(record.Operator) == 0 || !ownerHostPowerDigest(string(record.Source)) || !ownerHostPowerDigest(string(record.NativeBefore)) || !ownerHostPowerDigest(string(record.Runtime)) || !ownerHostPowerDigest(string(record.Binding)) || !ownerHostPowerDigest(record.EngineDigest) || !ownerHostPowerDigest(record.EngineIdentity) || string(record.Runtime) != runtime.configRuntimeBinding() {
		return nil, domain.ErrPlanStale
	}
	repository, err := runtime.ownerHostPowerRepository()
	if err != nil || repository != record.Repository {
		return nil, domain.ErrPlanStale
	}
	operator, err := runtime.operatorIdentity()
	if err != nil {
		return nil, err
	}
	inputs, err := runtime.SelectedRuntimeInputs("core-guest-runtime")
	if err != nil {
		return nil, err
	}
	if operator.Username != string(record.Operator) || inputs != string(record.Source) {
		return nil, domain.ErrPlanStale
	}
	source, err := runtime.ownerHostPowerEngineSource()
	if err != nil {
		return nil, err
	}
	engine, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer engine.Close()
	info, err := engine.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Size() > 128<<20 {
		return nil, domain.ErrPlanStale
	}
	checkIdentity := func() error {
		current, err := os.Lstat(source)
		if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
			return domain.ErrPlanStale
		}
		identity, err := nativeInstalledFilesLabeled([]string{source}, func(string) string { return ownerHostPowerEngineRole })
		if err != nil || identity != record.EngineIdentity {
			return domain.ErrPlanStale
		}
		return nil
	}
	if err := checkIdentity(); err != nil {
		return nil, err
	}
	p := &PreparedHostPower{operator: string(record.Operator), source: string(record.Source), before: string(record.NativeBefore), runtime: string(record.Runtime), binding: string(record.Binding), engineSource: source, engineSHA: record.EngineDigest}
	p.engineBefore, err = nativeInstalledFiles([]string{source})
	if err != nil {
		return nil, err
	}
	binding, err := runtime.ownerHostPowerBinding(p)
	if err != nil || binding != p.binding {
		return nil, domain.ErrPlanStale
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.engine, err = testvmsruntime.SealPreparedEngine(engine, p.engineSHA)
	if err != nil {
		return nil, err
	}
	if err := checkIdentity(); err != nil {
		p.Close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}
