package reconcileruntime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
)

// PublicationConsumerPlan binds bytes before consent. File identities and new
// custody generations are captured only from the checked installation result.
type PublicationConsumerPlan struct {
	EngineDigest, UnitDigest, PowerUnitDigest, ContractsDigest string
	Generation, FilesBefore                                    string
	Before                                                     []ports.NativeResourceGeneration
}

func (runtime Runtime) publicationResourcePaths() []string {
	return []string{runtime.environmentDefault("SUBYARD_POWER_RECONCILER_PATH", "/usr/local/libexec/subyard/yard-boot-reconcile"), runtime.environmentDefault("SUBYARD_POWER_UNIT_PATH", "/etc/systemd/system/subyard-power-reconcile.service"), runtime.publicationUnitPath()}
}
func (runtime Runtime) publicationUnitPath() string {
	return filepath.Join(filepath.Dir(runtime.environmentDefault("SUBYARD_POWER_UNIT_PATH", "/etc/systemd/system/subyard-power-reconcile.service")), ownerpublication.HandoffService)
}
func (runtime Runtime) PreparePublicationConsumer(p *PreparedHostPower) (*PublicationConsumerPlan, error) {
	if p == nil || p.runtime != runtime.configRuntimeBinding() {
		return nil, domain.ErrPlanStale
	}
	source, err := runtime.SelectedRuntimeInputs("core-guest-runtime")
	if err != nil {
		return nil, err
	}
	if source != p.source {
		return nil, domain.ErrPlanStale
	}
	digest, err := p.engineDigest()
	if err != nil || digest != p.engineSHA {
		return nil, domain.ErrPlanStale
	}
	contracts, err := ownerpublication.Load(runtime.RepositoryRoot)
	if err != nil {
		return nil, err
	}
	encoded, err := ownerpublication.Encode(contracts)
	if err != nil {
		return nil, err
	}
	template, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "config/systemd/subyard-publication-reconcile.service.in"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	paths := runtime.hostPowerResourcePaths()
	if len(paths) != 3 {
		return nil, domain.ErrPlanStale
	}
	if err := ownerpublication.ValidatePublicationPaths(paths[0], paths[1]); err != nil {
		return nil, err
	}
	unit := ownerpublication.RenderPublicationUnit(string(template), paths[0], paths[1], encoded)
	powerTemplate, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "config/systemd/subyard-power-reconcile.service.in"))
	if err != nil {
		return nil, err
	}
	powerUnit := strings.ReplaceAll(string(powerTemplate), "@SUBYARD_POWER_RECONCILER@", paths[0]) + "\n[Service]\nEnvironment=SUBYARD_PUBLICATION_CONTRACTS=" + encoded + "\n"
	generations, err := runtime.CaptureHostPowerCustody()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return &PublicationConsumerPlan{EngineDigest: p.engineSHA, UnitDigest: ownerpublication.Digest([]byte(unit)), PowerUnitDigest: ownerpublication.Digest([]byte(powerUnit)), ContractsDigest: ownerpublication.Digest([]byte(encoded)), Generation: fmt.Sprintf("%x", nonce), FilesBefore: p.before, Before: generations}, nil
}
func protectedPublicationFile(path string, mode os.FileMode) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != mode || st.Uid != 0 || st.Nlink != 1 || info.Size() > 128<<20 {
		return nil, domain.ErrPlanStale
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	ps, ok := parent.Sys().(*syscall.Stat_t)
	if !ok || !parent.IsDir() || ps.Uid != 0 || parent.Mode().Perm()&0022 != 0 {
		return nil, domain.ErrPlanStale
	}
	return os.ReadFile(path)
}
func (runtime Runtime) CapturePublicationConsumer(plan *PublicationConsumerPlan) (ownerpublication.Consumer, error) {
	if plan == nil {
		return ownerpublication.Consumer{}, domain.ErrPlanStale
	}
	paths := runtime.publicationResourcePaths()
	if len(paths) != 3 {
		return ownerpublication.Consumer{}, domain.ErrPlanStale
	}
	engine, err := protectedPublicationFile(paths[0], 0755)
	if err != nil {
		return ownerpublication.Consumer{}, err
	}
	unit, err := protectedPublicationFile(paths[2], 0644)
	if err != nil {
		return ownerpublication.Consumer{}, err
	}
	if ownerpublication.Digest(engine) != plan.EngineDigest || ownerpublication.Digest(unit) != plan.UnitDigest {
		return ownerpublication.Consumer{}, domain.ErrPlanStale
	}
	if plan.PowerUnitDigest != "" {
		powerUnit, err := protectedPublicationFile(paths[1], 0644)
		if err != nil || ownerpublication.Digest(powerUnit) != plan.PowerUnitDigest {
			return ownerpublication.Consumer{}, domain.ErrPlanStale
		}
	}
	marker := "\nEnvironment=SUBYARD_PUBLICATION_CONTRACTS="
	_, encoded, ok := strings.Cut(string(unit), marker)
	encoded = strings.TrimSuffix(encoded, "\n")
	if !ok || strings.ContainsAny(encoded, "\r\n") || ownerpublication.Digest([]byte(encoded)) != plan.ContractsDigest {
		return ownerpublication.Consumer{}, domain.ErrPlanStale
	}
	if _, err := ownerpublication.Decode(encoded); err != nil {
		return ownerpublication.Consumer{}, err
	}
	gens, err := captureHostPowerCustody(paths)
	if err != nil {
		return ownerpublication.Consumer{}, err
	}
	selected := len(gens) == 3 && ownerHostPowerDigest(plan.Generation)
	for _, g := range gens {
		selected = selected && g.Value == plan.Generation
	}
	if len(plan.Before) > 0 && !selected && !reflect.DeepEqual(gens, plan.Before) {
		return ownerpublication.Consumer{}, domain.ErrPlanStale
	}
	files, err := nativeInstalledFiles(paths)
	if err != nil {
		return ownerpublication.Consumer{}, err
	}
	if len(plan.Before) > 0 && !selected && files != plan.FilesBefore {
		return ownerpublication.Consumer{}, domain.ErrPlanStale
	}
	result := ownerpublication.Consumer{EngineDigest: plan.EngineDigest, UnitDigest: plan.UnitDigest, ContractsDigest: plan.ContractsDigest, FilesDigest: files, Generations: gens}
	if !result.Valid() {
		return ownerpublication.Consumer{}, domain.ErrPlanStale
	}
	return result, nil
}
func (runtime Runtime) CheckPublicationConsumer(ctx context.Context, expected ownerpublication.Consumer) error {
	actual, err := runtime.CapturePublicationConsumer(&PublicationConsumerPlan{EngineDigest: expected.EngineDigest, UnitDigest: expected.UnitDigest, ContractsDigest: expected.ContractsDigest})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return domain.ErrPlanStale
	}
	return runtime.checkPublicationManager(ctx)
}
func (runtime Runtime) StartPublicationConsumer(ctx context.Context, expected ownerpublication.Consumer) error {
	if err := runtime.CheckPublicationConsumer(ctx, expected); err != nil {
		return err
	}
	systemctl := "/usr/bin/systemctl"
	arguments := []string{"start", "--no-block", ownerpublication.HandoffService}
	command := exec.CommandContext(ctx, systemctl, arguments...)
	if os.Geteuid() != 0 {
		if runtime.environmentValue("SUBYARD_SUDO_PREAUTHORIZED") != "1" {
			return domain.ErrPlanStale
		}
		command = exec.CommandContext(ctx, "/usr/bin/sudo", append([]string{"-n", "--", systemctl}, arguments...)...)
	}
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	finish, err := runtime.beginNativeCommand(ctx, "publication-delivery-start", systemctl, command.Env, arguments)
	if err != nil {
		return err
	}
	return finish(command.Run())
}

// PublicationWorkerConsumer proves this process is the installed image paired
// with the protected unit/contracts. Request data never supplies a path.
func (runtime Runtime) PublicationWorkerConsumer(ctx context.Context, encoded string) (ownerpublication.Consumer, error) {
	paths := runtime.publicationResourcePaths()
	if len(paths) != 3 {
		return ownerpublication.Consumer{}, domain.ErrPlanStale
	}
	if err := runtime.publicationSelf(); err != nil {
		return ownerpublication.Consumer{}, err
	}
	image, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		return ownerpublication.Consumer{}, err
	}
	unit, err := protectedPublicationFile(paths[2], 0644)
	if err != nil {
		return ownerpublication.Consumer{}, err
	}
	if !strings.Contains(string(unit), "\nExecStart="+ownerpublication.PublicationCommand(paths[0], paths[1])+"\n") {
		return ownerpublication.Consumer{}, domain.ErrPlanStale
	}
	plan := &PublicationConsumerPlan{EngineDigest: ownerpublication.Digest(image), UnitDigest: ownerpublication.Digest(unit), ContractsDigest: ownerpublication.Digest([]byte(encoded))}
	consumer, err := runtime.CapturePublicationConsumer(plan)
	if err != nil {
		return consumer, err
	}
	if err := runtime.publicationSelf(); err != nil {
		return ownerpublication.Consumer{}, err
	}
	return consumer, runtime.checkPublicationManager(ctx)
}
func (runtime Runtime) publicationSelf() error {
	executing, err := os.Stat("/proc/self/exe")
	if err != nil {
		return err
	}
	installed, err := os.Stat(runtime.publicationResourcePaths()[0])
	if err != nil {
		return err
	}
	if !os.SameFile(executing, installed) {
		return domain.ErrPlanStale
	}
	return nil
}
func (runtime Runtime) checkPublicationManager(ctx context.Context) error {
	systemctl := "/usr/bin/systemctl"
	command := exec.CommandContext(ctx, systemctl, "show", ownerpublication.HandoffService, "--property=LoadState", "--property=NeedDaemonReload", "--property=FragmentPath", "--property=DropInPaths")
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	raw, err := command.Output()
	if err != nil {
		return err
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return domain.ErrPlanStale
		}
		if _, ok := values[k]; ok {
			return domain.ErrPlanStale
		}
		values[k] = v
	}
	if len(values) != 4 || values["LoadState"] != "loaded" || values["NeedDaemonReload"] != "no" || values["FragmentPath"] != runtime.publicationUnitPath() || values["DropInPaths"] != "" {
		return domain.ErrPlanStale
	}
	return nil
}

func (runtime Runtime) ApplyOwnerPublicationPower(ctx context.Context, prepared *PreparedHostPower, sources *PreparedInstalledRuntime, plan *PublicationConsumerPlan) error {
	if plan == nil && runtime.ownerPublication != nil {
		plan = &runtime.ownerPublication.record.Consumer
	}
	if plan == nil {
		return runtime.ApplyOwnerHostPower(ctx, prepared, sources)
	}
	if runtime.ownerPublication != nil && runtime.ownerPublication.installation != nil {
		if _, err := runtime.ownerPublicationConsumer(ctx); err != nil {
			return err
		}
		return sources.FinishOwnerSources(nil)
	}
	vector, err := hostPowerVector(plan.Before)
	if err != nil || !ownerHostPowerDigest(plan.Generation) {
		return domain.ErrPlanStale
	}
	current, err := runtime.CaptureHostPowerCustody()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, plan.Before) {
		return domain.ErrPlanStale
	}
	execution, err := runtime.OwnerSourceRuntime(ctx, sources)
	if err != nil {
		return err
	}
	if runtime.ownerPublication != nil {
		if _, e := runtime.ownerPublicationConsumer(ctx); e == nil {
			return sources.FinishOwnerSources(nil)
		}
		execution.hostPowerCompletion = &hostPowerCompletion{}
	}
	err = runtime.applyPreparedHostPowerRuntime(ctx, prepared, map[string]string{"SUBYARD_NATIVE_EXPECTED_VECTOR": vector, "SUBYARD_NATIVE_GENERATION": plan.Generation}, execution)
	err = errors.Join(err, sources.FinishOwnerSources(err))
	if err == nil && runtime.ownerPublication != nil {
		err = runtime.recordHostPowerCompletion(ctx, execution.hostPowerCompletion)
	}
	if err == nil && runtime.ownerPublication != nil {
		err = runtime.captureOwnerPublicationInstallation(ctx, true)
	}
	return err
}

// OwnerPublicationRecord is optional original-init data. Historical records
// retain their exact host-power grammar and need no new consumer without receipts.
type OwnerPublicationRecord struct {
	OutputDigest string
	Power        OwnerHostPowerRecord
	Consumer     PublicationConsumerPlan
}
type ownerPublicationExecution struct {
	record       OwnerPublicationRecord
	installation *OwnerPublicationInstallation
	original     Runtime
	observed     *ports.InstanceInfo
}

func (runtime Runtime) PrepareOwnerPublication(ctx context.Context) (*OwnerPublicationRecord, error) {
	if _, err := os.Stat(filepath.Join(runtime.RepositoryRoot, "config/systemd/subyard-publication-reconcile.service.in")); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	power, err := runtime.PrepareHostPowerRuntime(ctx)
	if err != nil {
		return nil, err
	}
	defer power.Close()
	record, err := runtime.RecordOwnerHostPower(power)
	if err != nil {
		return nil, err
	}
	consumer, err := runtime.PreparePublicationConsumer(power)
	if err != nil {
		return nil, err
	}
	recorded := &OwnerPublicationRecord{Power: record, Consumer: *consumer}
	recorded.OutputDigest = runtime.ownerPublicationOutput(recorded)
	return recorded, nil
}
func (runtime Runtime) OwnerPublicationRecord() *OwnerPublicationRecord {
	if runtime.ownerPublication == nil {
		return nil
	}
	record := runtime.ownerPublication.record
	return &record
}
func (runtime Runtime) RestoreOwnerPublication(record *OwnerPublicationRecord) (Runtime, error) {
	if record == nil {
		runtime.ownerPublication = nil
		return runtime, nil
	}
	if record.OutputDigest != runtime.ownerPublicationOutput(record) || !record.Power.Present || record.Power.EngineDigest != record.Consumer.EngineDigest || !ownerHostPowerDigest(record.Consumer.PowerUnitDigest) || string(record.Power.NativeBefore) != record.Consumer.FilesBefore || !ownerHostPowerDigest(record.Consumer.Generation) {
		return Runtime{}, domain.ErrPlanStale
	}
	if _, err := hostPowerVector(record.Consumer.Before); err != nil || len(record.Consumer.Before) != 3 {
		return Runtime{}, domain.ErrPlanStale
	}
	original := runtime
	original.ownerPublication = nil
	runtime.ownerPublication = &ownerPublicationExecution{record: *record, original: original}
	return runtime, nil
}
func (runtime Runtime) applyOwnerPublicationPower(ctx context.Context) error {
	owner := runtime.ownerPublication
	if owner == nil || runtime.ownerSources == nil || runtime.ownerSources.check() != nil {
		return domain.ErrPlanStale
	}
	if owner.installation != nil {
		_, err := runtime.ownerPublicationConsumer(ctx)
		return err
	}
	prepared, err := owner.original.RestoreOwnerHostPower(ctx, owner.record.Power)
	if err != nil {
		return err
	}
	defer prepared.Close()
	plan := owner.record.Consumer
	vector, err := hostPowerVector(plan.Before)
	if err != nil {
		return err
	}
	current, err := owner.original.CaptureHostPowerCustody()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, plan.Before) {
		return domain.ErrPlanStale
	}
	if _, e := runtime.ownerPublicationConsumer(ctx); e == nil {
		return nil
	}
	runtime.hostPowerCompletion = &hostPowerCompletion{}
	if err := owner.original.applyPreparedHostPowerRuntime(ctx, prepared, map[string]string{"SUBYARD_NATIVE_EXPECTED_VECTOR": vector, "SUBYARD_NATIVE_GENERATION": plan.Generation}, runtime); err != nil {
		return err
	}
	if err := runtime.recordHostPowerCompletion(ctx, runtime.hostPowerCompletion); err != nil {
		return err
	}
	return runtime.captureOwnerPublicationInstallation(ctx, true)
}

// The immutable capability is a checked result of this original graph, not a
// request to discover or select an installed worker in a later invocation.
type OwnerPublicationInstallation struct {
	consumer                           ownerpublication.Consumer
	proof                              *ports.NativeHostPowerInstallation
	invocation, intent, recipe, output string
}

func (runtime Runtime) ownerPublicationOutput(record *OwnerPublicationRecord) string {
	configHome := runtime.environmentValue("SUBYARD_CONFIG_HOME")
	if configHome == "" {
		configHome = runtime.Yard.Paths.ConfigHome
	}
	dataHome := runtime.environmentValue("SUBYARD_HOME")
	if dataHome == "" {
		dataHome = runtime.Yard.Paths.DataHome
	}
	paths := runtime.publicationResourcePaths()
	boundPaths := make([][]byte, len(paths))
	for i, path := range paths {
		boundPaths[i] = []byte(path)
	}
	raw, _ := json.Marshal(struct {
		UID                                        int
		Operator                                   []byte
		Config, Data                               []byte
		Paths                                      [][]byte
		Engine, Unit, PowerUnit, Contracts, Before string
		Generations                                []ports.NativeResourceGeneration
	}{os.Getuid(), record.Power.Operator, []byte(configHome), []byte(dataHome), boundPaths, record.Consumer.EngineDigest, record.Consumer.UnitDigest, record.Consumer.PowerUnitDigest, record.Consumer.ContractsDigest, record.Consumer.FilesBefore, record.Consumer.Before})
	return ownerpublication.Digest(raw)
}
func (runtime Runtime) OwnerPublicationInstallation() *OwnerPublicationInstallation {
	if runtime.ownerPublication == nil {
		return nil
	}
	return runtime.ownerPublication.installation
}
func (runtime Runtime) WithOwnerPublicationInstallation(ctx context.Context, result *OwnerPublicationInstallation) (Runtime, error) {
	if result == nil {
		return runtime, nil
	}
	if runtime.ownerPublication == nil || runtime.ownerPublication.record.OutputDigest != result.output {
		return Runtime{}, domain.ErrPlanStale
	}
	if err := runtime.checkOwnerPublicationInstallation(ctx, result); err != nil {
		return Runtime{}, err
	}
	runtime.ownerPublication.installation = result
	return runtime, nil
}
func (runtime Runtime) checkOwnerPublicationInstallation(ctx context.Context, result *OwnerPublicationInstallation) error {
	if runtime.ownerPublication == nil || result == nil || result.invocation != ports.NativeInvocationKey(ctx) || runtime.ownerPublication.record.OutputDigest != result.output {
		return domain.ErrPlanStale
	}
	checked, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeCheckedInvocation)
	if !ok {
		return domain.ErrPlanStale
	}
	intent, recipe, err := checked.CheckedInvocation()
	if err != nil {
		return err
	}
	if result.intent != intent || result.recipe != recipe {
		return domain.ErrPlanStale
	}
	if runtime.ownerSources != nil {
		if err := runtime.ownerSources.check(); err != nil {
			return err
		}
	} else {
		source, err := runtime.SelectedRuntimeInputs("core-guest-runtime")
		if err != nil || source != string(runtime.ownerPublication.record.Power.Source) {
			return domain.ErrPlanStale
		}
	}
	if result.proof != nil {
		observer, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeHostPowerInstallationObserver)
		if !ok {
			return domain.ErrPlanStale
		}
		proof, e := observer.ReadHostPowerInstallation(result.proof.Producer)
		if e != nil || proof == nil || !reflect.DeepEqual(*proof, *result.proof) {
			return domain.ErrPlanStale
		}
		ids, e := runtime.ownerPublication.original.hostPowerIdentities()
		if e != nil || !reflect.DeepEqual(ids, result.proof.Identities) {
			return domain.ErrPlanStale
		}
	}
	return runtime.ownerPublication.original.CheckPublicationConsumer(ctx, result.consumer)
}
func (runtime Runtime) captureOwnerPublicationInstallation(ctx context.Context, allowSelected bool) error {
	owner := runtime.ownerPublication
	if owner == nil {
		return domain.ErrPlanStale
	}
	consumer, err := owner.original.CapturePublicationConsumer(&owner.record.Consumer)
	if err != nil {
		return err
	}
	if !allowSelected && (!reflect.DeepEqual(consumer.Generations, owner.record.Consumer.Before) || consumer.FilesDigest != owner.record.Consumer.FilesBefore) {
		return domain.ErrPlanStale
	}
	checked, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeCheckedInvocation)
	if !ok || !ownerHostPowerDigest(ports.NativeInvocationKey(ctx)) {
		return domain.ErrPlanStale
	}
	intent, recipe, err := checked.CheckedInvocation()
	if err != nil {
		return err
	}
	var proof *ports.NativeHostPowerInstallation
	if allowSelected {
		observer, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeHostPowerInstallationObserver)
		if !ok {
			return domain.ErrPlanStale
		}
		result, e := observer.ReadHostPowerInstallation("")
		if e != nil || result == nil || result.Output != owner.record.OutputDigest || !reflect.DeepEqual(hostPowerConsumer(result.Consumer), consumer) {
			return domain.ErrPlanStale
		}
		proof = result
	}
	result := &OwnerPublicationInstallation{proof: proof, consumer: consumer, invocation: ports.NativeInvocationKey(ctx), intent: intent, recipe: recipe, output: owner.record.OutputDigest}
	if err := runtime.checkOwnerPublicationInstallation(ctx, result); err != nil {
		return err
	}
	owner.installation = result
	return nil
}
func (runtime Runtime) ownerPublicationConsumer(ctx context.Context) (ownerpublication.Consumer, error) {
	if runtime.ownerPublication == nil {
		return ownerpublication.Consumer{}, domain.ErrPlanStale
	}
	if runtime.ownerPublication.installation == nil {
		if err := runtime.captureOwnerPublicationInstallation(ctx, false); err != nil {
			return ownerpublication.Consumer{}, err
		}
	} else if err := runtime.checkOwnerPublicationInstallation(ctx, runtime.ownerPublication.installation); err != nil {
		return ownerpublication.Consumer{}, err
	}
	return runtime.ownerPublication.installation.consumer, nil
}

// OwnerPublicationConsumer returns only this invocation's checked installation result.
func (runtime Runtime) OwnerPublicationConsumer(ctx context.Context) (ownerpublication.Consumer, error) {
	return runtime.ownerPublicationConsumer(ctx)
}
