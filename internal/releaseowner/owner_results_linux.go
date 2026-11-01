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
	_, err := s.ownerResultIntent(*producer, request.Before, request.Token, request.ProducerAction)
	if err != nil {
		return Reply{}, errors.New("owner result original intent differs")
	}
	if request.Binding != nil && request.Action != nil {
		if !request.Action.sameProgram(request.ProducerAction) {
			return Reply{}, errors.New("owner result consumer program differs")
		}
		if _, err := s.ownerResultIntent(*request.Binding, request.Before, request.Token, request.Action); err != nil {
			return Reply{}, err
		}
	} else if request.Action != nil {
		return Reply{}, errors.New("owner result readonly action")
	}
	if request.Binding != nil && request.Action == nil && request.ProducerAction != nil {
		d, err := s.factsDirectory(*request.Binding)
		if err != nil {
			return Reply{}, err
		}
		raw, err := readFactAt(d, factName(request.Binding.Ordinal, "intent"))
		unix.Close(d)
		var intent Intent
		if err != nil || strict(raw, &intent) != nil || intent.Owner == "owner-registration" || s.checkIntent(*request.Binding, intent.Owner, intent.Resource, intent.Recipe) != nil {
			return Reply{}, errors.New("owner result consumer action missing")
		}
	}
	terminal, err := s.readTerminal(*producer)
	if err != nil || terminal.Owner != "owner-registration" || terminal.Resource != "test-yard" || terminal.State != "complete" && terminal.State != "failed" {
		return Reply{Code: "unknown"}, nil
	}
	origin := &invocationObserver{session: s, binding: *producer, ownerToken: request.Token, action: request.ProducerAction, restored: true, creates: map[ports.NativeOwnerCreateRef]ownerCreateIntent{}, created: map[ports.NativeOwnerCreateRef]bool{}}
	root, err := origin.creationRoot()
	if err != nil {
		return Reply{}, err
	}
	identity := root.Recipe
	if request.ProducerAction != nil {
		identity = request.ProducerAction.Program
	}
	if s.ownerResultsPayload != "" && (s.ownerResultsPayload != request.Before.PreparedRecipe || s.ownerResultsRecipe != identity || s.ownerResultsSource != root.Source) {
		return Reply{}, errors.New("owner result original work differs")
	}
	directory, err := s.factsDirectory(*producer)
	if err != nil {
		return Reply{}, err
	}
	defer unix.Close(directory)
	physical, err := readOwnerPhysical(directory, *producer, root, request.ProducerAction)
	if err != nil {
		return Reply{Code: "unknown"}, nil
	}
	physicalBinder, supportsPhysical := s.components.(OwnerPhysical)
	if physical != "" && !supportsPhysical || request.ProducerAction != nil && supportsPhysical && physical == "" {
		return Reply{Code: "unknown"}, nil
	}
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
		if request.RetainedOnly && key == ownerCreateProducer(*producer) {
			continue
		}
		priorRoot, err := prior.creationRoot()
		compatible := prior.action == nil && origin.action == nil && priorRoot.Recipe == root.Recipe || prior.action.sameProgram(origin.action)
		_, intentErr := s.ownerResultIntent(prior.binding, request.Before, request.Token, prior.action)
		if err != nil || intentErr != nil || !sameOwnerProducerScope(prior.binding, *producer) || !compatible || priorRoot.Source != root.Source {
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
		if request.RetainedOnly {
			continue
		}
		if err := include(value); err != nil {
			return Reply{Code: "unknown"}, nil
		}
	}
	installation, installErr := origin.ReadHostPowerInstallation("")
	if installErr != nil {
		return Reply{Code: "unknown"}, nil
	}
	if len(values) > 0 || installation != nil || origin.action != nil {
		merged[ownerCreateProducer(*producer)] = origin
	}
	slices.SortFunc(all, func(a, b ports.NativeOwnerCreated) int {
		return strings.Compare(a.Intent.Scope+"\x00"+a.Intent.Kind, b.Intent.Scope+"\x00"+b.Intent.Kind)
	})
	previous, previousActions := s.ownerOrigins, s.ownerActions
	s.ownerOrigins = merged
	if origin.action != nil {
		s.ownerActions = make(map[Binding]OwnerActionReference, len(previousActions)+2)
		for binding, action := range previousActions {
			s.ownerActions[binding] = action
		}
		s.ownerActions[*producer] = *origin.action
		if request.Action != nil {
			s.ownerActions[*request.Binding] = *request.Action
		}
	}
	if request.RetainedOnly {
		// Validate the full producer against the original component capsule before
		// removing its create-read capability from the admitted result set.
		if err := binder.BindOwnerResults(ports.WithNativeOperationObserver(ctx, origin), *request.Before, request.Token, values); err != nil {
			s.ownerOrigins, s.ownerActions = previous, previousActions
			return Reply{Code: "stale", Diagnostic: componentDiagnostic(err)}, nil
		}
		origin.creates, origin.created = nil, nil
	}
	if err := binder.BindOwnerResults(ports.WithNativeOperationObserver(ctx, origin), *request.Before, request.Token, all); err != nil {
		s.ownerOrigins, s.ownerActions = previous, previousActions
		return Reply{Code: "stale", Diagnostic: componentDiagnostic(err)}, nil
	}
	if physical != "" && !request.RetainedOnly {
		if err := physicalBinder.BindOwnerPhysical(ports.WithNativeOperationObserver(ctx, origin), *request.Before, request.Token, *request.ProducerAction, physical); err != nil {
			s.ownerOrigins, s.ownerActions = previous, previousActions
			return Reply{Code: "stale", Diagnostic: componentDiagnostic(err)}, nil
		}
	}
	s.ownerResultsPayload, s.ownerResultsRecipe, s.ownerResultsSource = request.Before.PreparedRecipe, identity, root.Source
	s.ownerCompleted = nil
	s.ownerCompletedTerminal = ""
	if terminal.State == "complete" && (origin.action == nil || origin.action.Action.Operation == "verify-desired") {
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

// The action reference is bound into the producer's own immutable request,
// rather than accepted as a separate label on a previously unrelated result.
func (s *session) ownerResultIntent(binding Binding, before *OwnerBefore, token string, action *OwnerActionReference) (string, error) {
	op := "apply-owner"
	if action != nil {
		op = "apply-owner-action"
	}
	recipe, err := recipeFingerprint(Request{Op: op, Binding: &binding, Before: before, Token: token, Action: action})
	if err != nil || s.checkIntent(binding, "owner-registration", "test-yard", recipe) != nil {
		return "", errors.New("owner result original intent differs")
	}
	return recipe, nil
}

// Both references were admitted against their own exact native intents. The
// Rust owner alone selects eligible slots and compensated attempt lineage.
func (o *invocationObserver) sameOwnerActionProgram(prior *invocationObserver) bool {
	if !o.action.valid(o.binding) || !prior.action.valid(prior.binding) || !o.action.sameProgram(prior.action) {
		return false
	}
	a, ok := o.session.ownerActions[o.binding]
	b, priorOK := o.session.ownerActions[prior.binding]
	return ok && priorOK && a == *o.action && b == *prior.action
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
		if origin.action == nil {
			continue
		}
		d, err := s.factsDirectory(binding)
		if err != nil {
			return err
		}
		raw, err := readFactAt(d, factName(binding.Ordinal, "intent"))
		unix.Close(d)
		var intent Intent
		if err != nil || strict(raw, &intent) != nil || s.checkIntent(binding, intent.Owner, intent.Resource, intent.Recipe) != nil {
			return errors.New("owner result consumer intent differs")
		}
		if intent.Owner == "owner-registration" {
			action, ok := s.ownerActions[binding]
			if !ok || !action.valid(binding) || !action.sameProgram(origin.action) {
				return errors.New("owner result consumer program differs")
			}
		}
	}
	return nil
}
