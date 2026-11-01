//go:build linux

package releaseowner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"golang.org/x/sys/unix"
)

// OwnerPhysical captures read-only bounded component output, opaque to the
// release boundary. Binding a receipt never re-captures its original output.
type OwnerPhysical interface {
	CaptureOwnerPhysical(context.Context, OwnerBefore, string, OwnerActionReference) (string, error)
	BindOwnerPhysical(context.Context, OwnerBefore, string, OwnerActionReference, string) error
}

type ownerPhysicalFact struct {
	Version  int     `json:"version"`
	Binding  Binding `json:"binding"`
	Recipe   string  `json:"recipe"`
	Source   string  `json:"source"`
	Physical string  `json:"physical"`
}

func (s *session) captureOwnerPhysical(ctx context.Context, request Request, observer *invocationObserver) error {
	component, ok := s.components.(OwnerPhysical)
	if !ok || request.Op != "apply-owner-action" || request.Before == nil || request.Action == nil {
		return nil
	}
	root, err := observer.creationRoot()
	if err != nil {
		return err
	}
	physical, err := component.CaptureOwnerPhysical(ctx, *request.Before, request.Token, *request.Action)
	if err != nil {
		return err
	}
	if !fingerprint(physical) {
		return errors.New("owner physical output digest")
	}
	return s.writeFact(observer.binding, "physical", ownerPhysicalFact{1, observer.binding, root.Recipe, root.Source, physical})
}

func readOwnerPhysical(directory int, binding Binding, root Intent, action *OwnerActionReference) (string, error) {
	raw, err := readFactAt(directory, factName(binding.Ordinal, "physical"))
	if errors.Is(err, unix.ENOENT) {
		return "", nil // Historical and synthetic components have no receipt.
	}
	var fact ownerPhysicalFact
	if err != nil || len(raw) > 4<<10 || strict(raw, &fact) != nil || action == nil || !action.valid(binding) || fact.Version != 1 || fact.Binding != binding || fact.Recipe != root.Recipe || fact.Source != root.Source || !fingerprint(fact.Physical) {
		return "", errors.New("owner physical output binding")
	}
	canonical, err := json.Marshal(fact)
	if err != nil || !bytes.Equal(append(canonical, '\n'), raw) {
		return "", errors.New("noncanonical owner physical output")
	}
	return fact.Physical, nil
}
