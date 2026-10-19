//go:build linux

package releaseowner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"golang.org/x/sys/unix"
)

func ownerCreateProducer(binding Binding) string {
	raw, _ := json.Marshal(binding)
	return digest(raw)
}

// bindOwnerResults admits one explicitly selected original producer. It reads
// only that invocation's fixed leaves; nested success never replaces its outer
// terminal. The owning component matches results to its original capsule.
func (s *session) bindOwnerResults(ctx context.Context, request Request) (Reply, error) {
	producer := request.Producer
	if !bindingValid(producer) || request.Before == nil || !fingerprint(request.Before.PreparedRecipe) || len(request.Token) != 32 || !lowerHex(request.Token) {
		return Reply{}, errors.New("owner result admission scope")
	}
	if request.Binding != nil {
		if s.binding == nil || *request.Binding != *s.binding || len(s.leases) != 2 || s.ownerRelease == nil || !sameOwnerProducerScope(*producer, *s.binding) {
			return Reply{}, errors.New("owner result producer scope differs")
		}
	} else if s.context == nil || producer.Session != s.context.Session || producer.Artifact != s.context.Artifact || producer.Helper != s.image {
		return Reply{}, errors.New("owner result readonly context differs")
	}
	recipe, err := recipeFingerprint(Request{Op: "apply-owner", Before: request.Before, Token: request.Token})
	if err != nil || s.checkIntent(*producer, "owner-registration", "test-yard", recipe) != nil {
		return Reply{}, errors.New("owner result original intent differs")
	}
	terminal, err := s.readTerminal(*producer)
	if err != nil || terminal.Owner != "owner-registration" || terminal.Resource != "test-yard" {
		return Reply{Code: "unknown"}, nil
	}
	origin := &invocationObserver{session: s, binding: *producer, ownerToken: request.Token, restored: true, creates: map[ports.NativeOwnerCreateRef]ownerCreateIntent{}, created: map[ports.NativeOwnerCreateRef]bool{}}
	root, err := origin.creationRoot()
	if err != nil {
		return Reply{}, err
	}
	if s.ownerResultsPayload != "" && (s.ownerResultsPayload != request.Before.PreparedRecipe || s.ownerResultsRecipe != root.Recipe || s.ownerResultsSource != root.Source) {
		return Reply{}, errors.New("owner result original work differs")
	}
	directory, err := s.factsDirectory(*producer)
	if err != nil {
		return Reply{}, err
	}
	defer unix.Close(directory)
	var values []ports.NativeOwnerCreated
	semantic := map[string]bool{}
	for _, reservationDomain := range []string{"command", "operation"} {
		for index := uint64(1); index <= 256; index++ {
			ref := ports.NativeOwnerCreateRef{Producer: ownerCreateProducer(*producer), Domain: reservationDomain, Index: index}
			intentRaw, intentErr := readFactAt(directory, factName(producer.Ordinal, createFactKind(ref, "intent")))
			resultRaw, resultErr := readFactAt(directory, factName(producer.Ordinal, createFactKind(ref, "result")))
			if errors.Is(intentErr, unix.ENOENT) && errors.Is(resultErr, unix.ENOENT) {
				continue
			}
			var intent ownerCreateIntent
			var result ownerCreateResult
			if intentErr != nil || resultErr != nil || len(intentRaw) > 4<<10 || len(resultRaw) > 4<<10 || strict(intentRaw, &intent) != nil || strict(resultRaw, &result) != nil {
				return Reply{Code: "unknown"}, nil
			}
			canonical, _ := json.Marshal(intent)
			canonicalResult, _ := json.Marshal(result)
			if !bytes.Equal(append(canonical, '\n'), intentRaw) || !bytes.Equal(append(canonicalResult, '\n'), resultRaw) || intent.Version != 1 || intent.Binding != *producer || intent.Recipe != root.Recipe || intent.Source != root.Source || intent.Index != index || intent.Domain != reservationDomain || !validOwnerCreate(intent.request()) || result.Version != 1 || result.Intent != intent || result.Status != 200 || !fingerprint(result.IdentityDigest) || !fingerprint(result.ETagDigest) {
				return Reply{Code: "unknown"}, nil
			}
			if reservationDomain == "operation" {
				if index > uint64(len(terminal.Operations)) || terminal.Operations[index-1] != (ports.NativeOperation{Kind: "owner-create-instance", Server: intent.Server, Project: intent.Project, Resource: intent.Resource, ID: result.OperationID, Status: 200}) || !domain.SafeID(result.InstanceUUID) || len(result.InstanceUUID) > 256 || !domain.SafeID(result.OperationID) || len(result.OperationID) > 256 {
					return Reply{Code: "unknown"}, nil
				}
			} else if result.OperationID != "" || result.InstanceUUID != "" || !ownerCreatedCommandProven(directory, *producer, index, intent) {
				return Reply{Code: "unknown"}, nil
			}
			key := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s", intent.Scope, intent.Kind, intent.Server, intent.Project, intent.Resource, intent.Pool)
			if semantic[key] {
				return Reply{Code: "unknown"}, nil
			}
			semantic[key] = true
			origin.creates[ref], origin.created[ref] = intent, true
			value, err := origin.ReadOwnerCreated(ref)
			if err != nil {
				return Reply{Code: "unknown"}, nil
			}
			values = append(values, value)
		}
	}
	binder, ok := s.components.(interface {
		BindOwnerResults(context.Context, OwnerBefore, string, []ports.NativeOwnerCreated) error
	})
	if !ok {
		return Reply{Code: "unavailable"}, nil
	}
	merged := map[string]*invocationObserver{}
	all := []ports.NativeOwnerCreated{}
	roles := map[string]ports.NativeOwnerCreated{}
	include := func(value ports.NativeOwnerCreated) error {
		role := value.Intent.Scope + "\x00" + value.Intent.Kind
		if prior, found := roles[role]; found {
			if prior == value {
				return nil
			}
			return errors.New("owner result role has multiple producers")
		}
		if len(roles) >= 9 {
			return errors.New("owner result planned role bound")
		}
		roles[role] = value
		all = append(all, value)
		return nil
	}
	for key, prior := range s.ownerOrigins {
		priorRoot, err := prior.creationRoot()
		if err != nil || !sameOwnerProducerScope(prior.binding, *producer) || priorRoot.Recipe != root.Recipe || priorRoot.Source != root.Source {
			return Reply{}, errors.New("owner result lineage differs")
		}
		for ref := range prior.created {
			value, err := prior.ReadOwnerCreated(ref)
			if err != nil {
				return Reply{Code: "unknown"}, nil
			}
			if err = include(value); err != nil {
				return Reply{Code: "unknown"}, nil
			}
		}
		merged[key] = prior
	}
	for _, value := range values {
		if err := include(value); err != nil {
			return Reply{Code: "unknown"}, nil
		}
	}
	if len(values) > 0 {
		merged[ownerCreateProducer(*producer)] = origin
	}
	slices.SortFunc(all, func(a, b ports.NativeOwnerCreated) int {
		return strings.Compare(a.Intent.Scope+"\x00"+a.Intent.Kind, b.Intent.Scope+"\x00"+b.Intent.Kind)
	})
	previous := s.ownerOrigins
	s.ownerOrigins = merged
	if err := binder.BindOwnerResults(ports.WithNativeOperationObserver(ctx, origin), *request.Before, request.Token, all); err != nil {
		s.ownerOrigins = previous
		return Reply{Code: "stale", Diagnostic: componentDiagnostic(err)}, nil
	}
	s.ownerResultsPayload, s.ownerResultsRecipe, s.ownerResultsSource = request.Before.PreparedRecipe, root.Recipe, root.Source
	s.ownerCompleted = nil
	s.ownerCompletedTerminal = ""
	if terminal.State == "complete" {
		raw, err := readFactAt(directory, factName(producer.Ordinal, "terminal"))
		if err != nil {
			return Reply{}, err
		}
		copy := *producer
		s.ownerCompleted = &copy
		s.ownerCompletedTerminal = digest(raw)
	}
	return Reply{}, nil
}

func lowerHex(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

// This is the existing immutable Rust closure syntax, solely for correlating
// an original sync response after its command pair has been compacted.
type ownerCreateClosure struct {
	Version  int                      `json:"version"`
	Binding  Binding                  `json:"binding"`
	Intent   string                   `json:"intent"`
	Accepted string                   `json:"accepted"`
	Terminal string                   `json:"terminal"`
	Commands []commandFact            `json:"commands"`
	Facts    []ownerCreateClosureFact `json:"facts"`
}
type ownerCreateClosureFact struct {
	Name   string `json:"name"`
	Size   uint64 `json:"size"`
	SHA256 string `json:"sha256"`
}

func ownerCreatedCommandProven(directory int, binding Binding, index uint64, intent ownerCreateIntent) bool {
	expected := commandFact{Kind: createCommandKind(intent.Kind), Source: intent.RequestDigest}
	encoded, _ := json.Marshal(expected)
	encoded = append(encoded, '\n')
	missing := false
	for _, phase := range []string{"intent", "terminal"} {
		raw, err := readFactAt(directory, factName(binding.Ordinal, fmt.Sprintf("command-%03d-%s", index, phase)))
		if errors.Is(err, unix.ENOENT) {
			missing = true
			continue
		}
		if err != nil || !bytes.Equal(raw, encoded) {
			return false
		}
	}
	raw, err := readFactAt(directory, factName(binding.Ordinal, "closure"))
	if errors.Is(err, unix.ENOENT) {
		return !missing
	}
	var closure ownerCreateClosure
	if err != nil || strict(raw, &closure) != nil || closure.Version != 1 || closure.Binding != binding || index > uint64(len(closure.Commands)) || closure.Commands[index-1] != expected || len(closure.Commands) > 256 || len(closure.Facts) > 1280 {
		return false
	}
	canonical, _ := json.Marshal(closure)
	if !bytes.Equal(append(canonical, '\n'), raw) {
		return false
	}
	for _, check := range []struct{ kind, hash string }{{"intent", closure.Intent}, {"accepted", closure.Accepted}, {"terminal", closure.Terminal}} {
		raw, err := readFactAt(directory, factName(binding.Ordinal, check.kind))
		if err != nil || digest(raw) != check.hash {
			return false
		}
	}
	for _, phase := range []string{"intent", "terminal"} {
		name := factName(binding.Ordinal, fmt.Sprintf("command-%03d-%s", index, phase))
		found := false
		for _, fact := range closure.Facts {
			if fact.Name == name {
				if found || fact.Size != uint64(len(encoded)) || fact.SHA256 != digest(encoded) {
					return false
				}
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func sameOwnerProducerScope(producer, current Binding) bool {
	return producer.Session == current.Session && producer.Transaction == current.Transaction && producer.Plan == current.Plan && producer.Artifact == current.Artifact && producer.Helper == current.Helper
}
func (s *session) checkOwnerResultEffectBinding(binding Binding) error {
	if len(s.ownerOrigins) == 0 {
		return nil
	}
	if len(s.leases) != 2 || s.ownerRelease == nil {
		return errors.New("owner result effect lacks original leases")
	}
	for _, origin := range s.ownerOrigins {
		if !sameOwnerProducerScope(origin.binding, binding) {
			return errors.New("owner result effect scope differs")
		}
	}
	return nil
}
