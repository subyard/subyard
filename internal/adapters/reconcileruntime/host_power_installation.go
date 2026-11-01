package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"os"
	"reflect"
	"syscall"
)

// This bounded stdout is a completion payload, never command/invocation closure.
type hostPowerOutput struct{ buffer bytes.Buffer }

func (b *hostPowerOutput) Len() int       { return b.buffer.Len() }
func (b *hostPowerOutput) Bytes() []byte  { return b.buffer.Bytes() }
func (b *hostPowerOutput) String() string { return b.buffer.String() }

func (b *hostPowerOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4096 {
		return 0, errors.New("host installation completion bound")
	}
	return b.buffer.Write(p)
}

type hostPowerCompletion struct {
	output    hostPowerOutput
	command   uint64
	completed bool
}
type hostPowerTail struct {
	Version     int                              `json:"version"`
	FilesDigest string                           `json:"filesDigest"`
	Identities  []ports.NativeHostPowerIdentity  `json:"identities"`
	Generations []ports.NativeResourceGeneration `json:"generations"`
}

func hostPowerConsumer(v ports.NativeHostPowerConsumer) ownerpublication.Consumer {
	return ownerpublication.Consumer{EngineDigest: v.EngineDigest, UnitDigest: v.UnitDigest, ContractsDigest: v.ContractsDigest, FilesDigest: v.FilesDigest, Generations: v.Generations}
}
func hostPowerConsumerPort(v ownerpublication.Consumer) ports.NativeHostPowerConsumer {
	return ports.NativeHostPowerConsumer{EngineDigest: v.EngineDigest, UnitDigest: v.UnitDigest, ContractsDigest: v.ContractsDigest, FilesDigest: v.FilesDigest, Generations: v.Generations}
}
func (runtime Runtime) hostPowerIdentities() ([]ports.NativeHostPowerIdentity, error) {
	var ids []ports.NativeHostPowerIdentity
	for i, path := range runtime.publicationResourcePaths() {
		info, e := os.Lstat(path)
		if e != nil {
			return nil, e
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, domain.ErrPlanStale
		}
		ids = append(ids, ports.NativeHostPowerIdentity{Role: []string{"engine", "service", "publication"}[i], Device: uint64(st.Dev), Inode: st.Ino})
	}
	return ids, nil
}
func (runtime Runtime) recordHostPowerCompletion(ctx context.Context, c *hostPowerCompletion) error {
	observer, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeHostPowerInstallationObserver)
	if !ok || runtime.ownerPublication == nil || c == nil || !c.completed {
		return domain.ErrPlanStale
	}
	var tail hostPowerTail
	decoder := json.NewDecoder(bytes.NewReader(c.output.Bytes()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&tail) != nil {
		return domain.ErrPlanStale
	}
	raw, _ := json.Marshal(tail)
	if tail.Version != 1 || !bytes.Equal(append(raw, '\n'), c.output.Bytes()) {
		return domain.ErrPlanStale
	}
	owner := runtime.ownerPublication
	consumer, e := owner.original.CapturePublicationConsumer(&owner.record.Consumer)
	if e != nil {
		return e
	}
	ids, e := owner.original.hostPowerIdentities()
	if e != nil {
		return e
	}
	for _, g := range consumer.Generations {
		if g.Value != owner.record.Consumer.Generation {
			return domain.ErrPlanStale
		}
	}
	if consumer.FilesDigest != tail.FilesDigest || !reflect.DeepEqual(ids, tail.Identities) || !reflect.DeepEqual(consumer.Generations, tail.Generations) {
		return domain.ErrPlanStale
	}
	_, e = observer.RecordHostPowerInstallation(c.command, ports.NativeHostPowerInstallation{HostSource: string(owner.record.Power.Source), Output: owner.record.OutputDigest, Consumer: hostPowerConsumerPort(consumer), Identities: ids})
	if e != nil {
		return e
	}
	return owner.original.CheckPublicationConsumer(ctx, consumer)
}

// Import requires the fresh live observer's explicitly selected original producer.
// The restored observer remains unable to provide CheckedInvocation.
func (runtime Runtime) ImportOwnerPublicationInstallation(ctx context.Context, result ports.NativeHostPowerInstallation) (Runtime, error) {
	owner := runtime.ownerPublication
	observer, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeHostPowerInstallationObserver)
	if !ok || owner == nil || result.HostSource != string(owner.record.Power.Source) || result.Output != owner.record.OutputDigest {
		return Runtime{}, domain.ErrPlanStale
	}
	actual, e := observer.ReadHostPowerInstallation(result.Producer)
	if e != nil || actual == nil || !reflect.DeepEqual(*actual, result) {
		return Runtime{}, domain.ErrPlanStale
	}
	consumer := hostPowerConsumer(result.Consumer)
	if !consumer.Valid() || consumer.EngineDigest != owner.record.Consumer.EngineDigest || consumer.UnitDigest != owner.record.Consumer.UnitDigest || consumer.ContractsDigest != owner.record.Consumer.ContractsDigest {
		return Runtime{}, domain.ErrPlanStale
	}
	for _, g := range consumer.Generations {
		if g.Value != owner.record.Consumer.Generation {
			return Runtime{}, domain.ErrPlanStale
		}
	}
	ids, e := owner.original.hostPowerIdentities()
	if e != nil || !reflect.DeepEqual(ids, result.Identities) || owner.original.CheckPublicationConsumer(ctx, consumer) != nil {
		return Runtime{}, domain.ErrPlanStale
	}
	checked, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeCheckedInvocation)
	if !ok {
		return Runtime{}, domain.ErrPlanStale
	}
	intent, recipe, e := checked.CheckedInvocation()
	if e != nil {
		return Runtime{}, domain.ErrPlanStale
	}
	cap := &OwnerPublicationInstallation{proof: &result, consumer: consumer, invocation: ports.NativeInvocationKey(ctx), intent: intent, recipe: recipe, output: result.Output}
	if e := runtime.checkOwnerPublicationInstallation(ctx, cap); e != nil {
		return Runtime{}, e
	}
	owner.installation = cap
	return runtime, nil
}
