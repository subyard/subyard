package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// The broker's source record remains unchanged. Only its four physical
// observations are measured after the original owner invocation completes.
type afterOwnerBrokerPhysical struct {
	Version      int    `json:"version"`
	Native       string `json:"native"`
	ApplyBinding string `json:"applyBinding"`
	Sink         string `json:"sink"`
	Binding      string `json:"binding"`
}

func AfterOwnerBrokerPhysicalBound() int {
	f := strings.Repeat("f", 64)
	raw, _ := json.Marshal(afterOwnerBrokerPhysical{1, f, f, f, f})
	return len(raw)
}

func (runtime Runtime) PrepareAfterOwnerBroker(ctx context.Context, template []byte) (*PreparedInstalledRuntime, error) {
	broker, err := runtime.prepareTestVMBrokerRuntime(ctx, false)
	if err != nil {
		return nil, err
	}
	defer broker.Close()
	p, err := runtime.PrepareTestVMBrokerRecipe(ctx, broker)
	if err != nil {
		return nil, err
	}
	// The recipe owns source bytes; the closed temporary backend is not reused.
	p.broker = nil
	return p.reserveAfterOwnerMetadata(template)
}

func (runtime Runtime) BindAfterOwnerBroker(ctx context.Context, raw, physical []byte) (*PreparedInstalledRuntime, error) {
	value := afterOwnerBrokerPhysical{Version: 1}
	if physical != nil {
		d := json.NewDecoder(bytes.NewReader(physical))
		d.DisallowUnknownFields()
		if d.Decode(&value) != nil {
			return nil, domain.ErrPlanStale
		}
		canonical, _ := json.Marshal(value)
		if !bytes.Equal(canonical, physical) || value.Version != 1 {
			return nil, domain.ErrPlanStale
		}
		for _, s := range []string{value.Native, value.ApplyBinding, value.Sink, value.Binding} {
			if s != "" && !profileRuntimeDigest.MatchString(s) {
				return nil, domain.ErrPlanStale
			}
		}
	}
	// A nil physical record is an inert source bind, retaining original hashes.
	if physical == nil {
		var r installedRecipe
		if json.Unmarshal(raw, &r) != nil || r.Owner != "test-vm-broker" {
			return nil, domain.ErrPlanStale
		}
		for _, f := range r.Files {
			if f.Path == brokerMetadataPath {
				var b brokerRecipe
				if json.Unmarshal(f.Content, &b) != nil {
					return nil, domain.ErrPlanStale
				}
				value.Native = b.Apply.Native
				value.ApplyBinding = b.Apply.Binding
				value.Sink = b.Sink
				value.Binding = b.Binding
			}
		}
	}
	runtime.afterOwnerBroker = &value
	return runtime.BindInstalledRuntimeContext(ctx, raw)
}

func (runtime Runtime) ResolveAfterOwnerBroker(ctx context.Context, raw []byte) (*PreparedInstalledRuntime, []byte, error) {
	original, err := runtime.BindAfterOwnerBroker(ctx, raw, nil)
	if err != nil {
		return nil, nil, err
	}
	defer original.Close()
	physical, err := runtime.afterOwnerBrokerObservation(ctx, original.broker)
	if err != nil {
		return nil, nil, err
	}
	p, err := runtime.BindAfterOwnerBroker(ctx, raw, physical)
	return p, physical, err
}
func (runtime Runtime) afterOwnerBrokerObservation(ctx context.Context, b *PreparedTestVMBroker) ([]byte, error) {
	apply, err := b.backend.ResolvePreparedApply(ctx, b.apply)
	if err != nil {
		return nil, err
	}
	defer apply.Close()
	record, _, err := apply.Record(b.backend)
	if err != nil {
		return nil, err
	}
	sink := ""
	if runtime.Yard.NestedE2EVMs {
		sink, err = runtime.brokerSinkFiles()
		if err != nil {
			return nil, err
		}
	}
	binding := installedRuntimeDigest(struct {
		Native, Source, Runtime, Sink string
		Power                         domain.DesiredPower
	}{apply.Binding(), b.source, b.runtime, sink, b.power})
	physical, _ := json.Marshal(afterOwnerBrokerPhysical{1, record.Native, apply.Binding(), sink, binding})
	return physical, nil
}
