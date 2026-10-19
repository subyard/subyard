//go:build linux

package releaseowner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/sys/unix"
)

type AfterOwnerCapacity struct {
	Version         int    `json:"version"`
	Phase           string `json:"phase"`
	MetadataBytes   int    `json:"metadataBytes"`
	DescriptorBytes int    `json:"descriptorBytes"`
}
type AfterOwnerDelta struct {
	InstanceUUID    string `json:"instanceUuid"`
	State           string `json:"state"`
	Actual          string `json:"actual"`
	Desired         string `json:"desired"`
	ReportedActual  string `json:"reportedActual"`
	ReportedDesired string `json:"reportedDesired"`
	Before          string `json:"before"`
	ScopeState      string `json:"scopeState"`
}
type AfterOwnerMetadata struct {
	Version          int             `json:"version"`
	Phase            string          `json:"phase"`
	Original         string          `json:"original"`
	Producer         Binding         `json:"producer"`
	ProducerTerminal string          `json:"producerTerminal"`
	Delta            AfterOwnerDelta `json:"delta"`
	Physical         []byte          `json:"physical,omitempty"`
}
type AfterOwnerCompletion struct {
	Producer Binding
	Terminal string
}

func ParseAfterOwner(value string) (string, string, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] != "v1" || !fingerprint(parts[2]) {
		return "", "", errors.New("after-owner descriptor")
	}
	switch parts[1] {
	case "runtime", "materialized-files", "materialized-integrations", "broker", "routes":
		return parts[1], parts[2], nil
	}
	return "", "", errors.New("after-owner kind")
}
func (d AfterOwnerDelta) Apply(original Target, recipe string) Target {
	original.InstanceUUID = d.InstanceUUID
	original.State = d.State
	original.Actual = d.Actual
	original.Desired = d.Desired
	original.ReportedActual = d.ReportedActual
	original.ReportedDesired = d.ReportedDesired
	original.Before = d.Before
	original.ScopeState = d.ScopeState
	original.PreparedRecipe = recipe
	return original
}
func AfterOwnerPhysicalTarget(original, physical Target) error {
	_, hash, err := ParseAfterOwner(original.AfterOwner)
	if err != nil || hash != original.PreparedRecipe || !fingerprint(physical.PreparedRecipe) {
		return errors.New("after-owner original recipe")
	}
	if original.InstanceUUID != "absent" && physical.InstanceUUID != original.InstanceUUID || physical.InstanceUUID == "absent" || !domain.SafeID(physical.InstanceUUID) || len(physical.InstanceUUID) > 128 {
		return errors.New("after-owner instance identity")
	}
	if original.State == "stopped" && physical.State != "stopped" && physical.State != "deferred" {
		return errors.New("after-owner stopped target")
	}
	d := AfterOwnerDelta{physical.InstanceUUID, physical.State, physical.Actual, physical.Desired, physical.ReportedActual, physical.ReportedDesired, physical.Before, physical.ScopeState}
	if d.Apply(original, physical.PreparedRecipe) != physical {
		return errors.New("after-owner logical scope differs")
	}
	return nil
}
func (s *session) ownerCompletion() (AfterOwnerCompletion, error) {
	if s.ownerCompleted == nil || s.ownerResultsPayload == "" {
		return AfterOwnerCompletion{}, errors.New("after-owner closed original owner unavailable")
	}
	b := *s.ownerCompleted
	terminal, err := s.readTerminal(b)
	if err != nil || terminal.State != "complete" || terminal.Owner != "owner-registration" || terminal.Resource != "test-yard" {
		return AfterOwnerCompletion{}, errors.New("after-owner terminal unavailable")
	}
	directory, err := s.factsDirectory(b)
	if err != nil {
		return AfterOwnerCompletion{}, err
	}
	defer unix.Close(directory)
	raw, err := readFactAt(directory, factName(b.Ordinal, "terminal"))
	if err != nil {
		return AfterOwnerCompletion{}, err
	}
	if digest(raw) != s.ownerCompletedTerminal {
		return AfterOwnerCompletion{}, errors.New("after-owner terminal changed")
	}
	return AfterOwnerCompletion{b, s.ownerCompletedTerminal}, nil
}
func (s *session) resolveAfterOwner(ctx context.Context, r Request) (Reply, error) {
	if r.Target == nil {
		return Reply{}, errors.New("after-owner target missing")
	}
	if _, _, err := ParseAfterOwner(r.Target.AfterOwner); err != nil {
		return Reply{}, err
	}
	if r.Binding != nil {
		if err := s.checkOwnerResultEffectBinding(*r.Binding); err != nil {
			return Reply{}, err
		}
	}
	completed, err := s.ownerCompletion()
	if err != nil {
		return Reply{Code: "unknown"}, nil
	}
	component, ok := s.components.(interface {
		ResolveAfterOwner(context.Context, Target, AfterOwnerCompletion) (Target, string, error)
	})
	if !ok {
		return Reply{Code: "unavailable"}, nil
	}
	target, recipe, err := component.ResolveAfterOwner(ctx, *r.Target, completed)
	if err != nil {
		return Reply{Code: "unavailable", Diagnostic: componentDiagnostic(err)}, nil
	}
	if recipe == "" || target.PreparedRecipe != recipe || AfterOwnerPhysicalTarget(*r.Target, target) != nil {
		return Reply{}, errors.New("after-owner physical target differs")
	}
	return Reply{Targets: []Target{target}, ResolvedRecipe: recipe}, nil
}
func DecodeAfterOwnerMetadata(raw []byte) (AfterOwnerMetadata, error) {
	var value AfterOwnerMetadata
	if len(raw) > 32<<20 || strict(raw, &value) != nil {
		return value, errors.New("after-owner metadata")
	}
	canonical, _ := json.Marshal(value)
	if !bytes.Equal(raw, canonical) || value.Version != 1 || value.Phase != "resolved" || !fingerprint(value.Original) || !bindingValid(&value.Producer) || !fingerprint(value.ProducerTerminal) {
		return value, errors.New("after-owner metadata binding")
	}
	return value, nil
}

func DecodeAfterOwnerCapacity(raw []byte) (AfterOwnerCapacity, error) {
	var value AfterOwnerCapacity
	if strict(raw, &value) != nil {
		return value, errors.New("after-owner capacity")
	}
	canonical, _ := json.Marshal(value)
	if !bytes.Equal(raw, canonical) || value.Version != 1 || value.Phase != "template" || value.MetadataBytes < len(raw) || value.MetadataBytes > 32<<20 || value.DescriptorBytes < 1 || value.DescriptorBytes > 32<<20 {
		return value, errors.New("after-owner capacity bound")
	}
	return value, nil
}
