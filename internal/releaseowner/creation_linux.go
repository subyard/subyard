//go:build linux

package releaseowner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"golang.org/x/sys/unix"
)

type ownerCreateIntent struct {
	Version       int     `json:"version"`
	Binding       Binding `json:"binding"`
	Recipe        string  `json:"recipe"`
	Source        string  `json:"source"`
	Scope         string  `json:"scope"`
	Kind          string  `json:"kind"`
	Domain        string  `json:"domain"`
	Index         uint64  `json:"index"`
	Token         string  `json:"token"`
	RequestDigest string  `json:"requestDigest"`
	Server        string  `json:"server"`
	Project       string  `json:"project"`
	Resource      string  `json:"resource"`
	Pool          string  `json:"pool"`
	Fingerprint   string  `json:"fingerprint,omitempty"`
}

type ownerCreateResult struct {
	Version        int               `json:"version"`
	Intent         ownerCreateIntent `json:"intent"`
	OperationID    string            `json:"operationID"`
	Status         int               `json:"status"`
	InstanceUUID   string            `json:"instanceUUID"`
	IdentityDigest string            `json:"identityDigest"`
	ETagDigest     string            `json:"etagDigest"`
}

func createFactKind(ref ports.NativeOwnerCreateRef, phase string) string {
	return fmt.Sprintf("%s-%03d-create-%s", ref.Domain, ref.Index, phase)
}

func createCommandKind(kind string) string {
	if kind == "project" {
		return "incus-owner-create-project"
	}
	return "incus-owner-create-volume"
}

func validOwnerCreate(v ports.NativeOwnerCreateIntent) bool {
	if v.Scope != "current" && v.Scope != "legacy" && v.Scope != "recreate" || len(v.Token) != 32 || strings.Trim(v.Token, "0123456789abcdef") != "" || !fingerprint(v.RequestDigest) || !fingerprint(v.Server) || !domain.SafeName(v.Project) || !domain.SafeName(v.Resource) || len(v.Project) > 256 || len(v.Resource) > 256 || v.Fingerprint != "" && (v.Kind != "instance" || !fingerprint(v.Fingerprint)) {
		return false
	}
	switch v.Kind {
	case "project":
		return v.Domain == "command" && v.Pool == "" && v.Project == v.Resource
	case "srv-volume":
		return v.Domain == "command" && len(v.Pool) <= 256 && domain.SafeName(v.Pool)
	case "instance":
		return v.Domain == "operation" && v.Pool == ""
	}
	return false
}

func (v ownerCreateIntent) request() ports.NativeOwnerCreateIntent {
	return ports.NativeOwnerCreateIntent{Scope: v.Scope, Kind: v.Kind, Domain: v.Domain, Token: v.Token, RequestDigest: v.RequestDigest, Server: v.Server, Project: v.Project, Resource: v.Resource, Pool: v.Pool, Fingerprint: v.Fingerprint}
}

func (o *invocationObserver) creationRoot() (Intent, error) {
	directory, err := o.session.factsDirectory(o.binding)
	if err != nil {
		return Intent{}, err
	}
	defer unix.Close(directory)
	raw, err := readFactAt(directory, factName(o.binding.Ordinal, "intent"))
	if err != nil {
		return Intent{}, err
	}
	var intent Intent
	if strict(raw, &intent) != nil {
		return intent, errors.New("owner create intent unavailable")
	}
	return o.restorationIntent(o.ownerToken, intent.Source)
}

func (o *invocationObserver) BeforeOwnerCreate(v ports.NativeOwnerCreateIntent) (ref ports.NativeOwnerCreateRef, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	defer func() {
		if err != nil {
			o.fault = errors.Join(o.fault, err)
		}
	}()
	if o.fault != nil || len(o.creates) != len(o.created) || !validOwnerCreate(v) {
		return ref, errors.New("invalid original owner create")
	}
	root, err := o.creationRoot()
	if err != nil {
		return ref, err
	}
	ref.Producer = ownerCreateProducer(o.binding)
	ref.Domain = v.Domain
	if v.Domain == "command" {
		ref.Index = uint64(len(o.commands) + 1)
	} else {
		if len(o.started) != len(o.accepted) {
			return ref, errors.New("previous owner create acceptance unresolved")
		}
		ref.Index = uint64(len(o.started) + 1)
	}
	if ref.Index > 256 {
		return ref, errors.New("owner create reservation exhausted")
	}
	directory, err := o.session.factsDirectory(o.binding)
	if err != nil {
		return ref, err
	}
	defer unix.Close(directory)
	reservation, err := reservationAt(directory, o.binding)
	if err != nil {
		return ref, err
	}
	// Charge the existing indexed intent and both bounded creation leaves before
	// POST. This checks capacity without introducing a separate reservation pool.
	reservation.Files -= 2
	if err := reservationCapacity(directory, o.binding, reservation, "", factName(o.binding.Ordinal, createFactKind(ref, "intent")), 3*(4<<10)); err != nil {
		return ref, err
	}
	fact := ownerCreateIntent{1, o.binding, root.Recipe, root.Source, v.Scope, v.Kind, v.Domain, ref.Index, v.Token, v.RequestDigest, v.Server, v.Project, v.Resource, v.Pool, v.Fingerprint}
	if v.Domain == "command" {
		command := commandFact{createCommandKind(v.Kind), v.RequestDigest}
		o.commands = append(o.commands, command)
		err = o.session.writeFact(o.binding, fmt.Sprintf("command-%03d-intent", ref.Index), command)
	} else {
		operation := ports.NativeOperation{Kind: "owner-create-instance", Server: v.Server, Project: v.Project, Resource: v.Resource}
		o.started = append(o.started, operation)
		err = o.session.writeFact(o.binding, fmt.Sprintf("operation-%03d-intent", ref.Index), operation)
	}
	if err != nil {
		return ref, err
	}
	if err = o.session.writeFact(o.binding, createFactKind(ref, "intent"), fact); err != nil {
		return ref, err
	}
	if o.creates == nil {
		o.creates = map[ports.NativeOwnerCreateRef]ownerCreateIntent{}
	}
	o.creates[ref] = fact
	return ref, nil
}

func (o *invocationObserver) RecordOwnerCreated(v ports.NativeOwnerCreated) (err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	defer func() {
		if err != nil {
			o.fault = errors.Join(o.fault, err)
		}
	}()
	intent, ok := o.creates[v.Ref]
	if o.fault != nil || !ok || v.Intent != intent.request() || v.Status != 200 || !fingerprint(v.IdentityDigest) || !fingerprint(v.ETagDigest) {
		return errors.New("owner create result differs")
	}
	if v.Ref.Domain == "command" {
		if v.OperationID != "" || v.InstanceUUID != "" || !o.commandTerminal[v.Ref.Index] || o.commands[v.Ref.Index-1] != (commandFact{createCommandKind(intent.Kind), intent.RequestDigest}) {
			return errors.New("owner create sync response unproved")
		}
	} else {
		if !domain.SafeID(v.InstanceUUID) || len(v.InstanceUUID) > 256 || !domain.SafeID(v.OperationID) || len(v.OperationID) > 256 || int(v.Ref.Index) > len(o.accepted) {
			return errors.New("owner create operation identity absent")
		}
		accepted := o.accepted[v.Ref.Index-1]
		terminal := o.terminal[accepted.ID]
		if accepted.ID != v.OperationID || accepted.Kind != "owner-create-instance" || accepted.Server != intent.Server || accepted.Project != intent.Project || accepted.Resource != intent.Resource || terminal.Status != 200 {
			return errors.New("owner create successful operation unproved")
		}
	}
	fact := ownerCreateResult{1, intent, v.OperationID, v.Status, v.InstanceUUID, v.IdentityDigest, v.ETagDigest}
	if err := o.session.writeFact(o.binding, createFactKind(v.Ref, "result"), fact); err != nil {
		return err
	}
	if o.created == nil {
		o.created = map[ports.NativeOwnerCreateRef]bool{}
	}
	o.created[v.Ref] = true
	return nil
}

func (o *invocationObserver) ReadOwnerCreated(ref ports.NativeOwnerCreateRef) (ports.NativeOwnerCreated, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fault != nil {
		return ports.NativeOwnerCreated{}, errors.New("owner create invocation unresolved")
	}
	if ref.Producer != ownerCreateProducer(o.binding) {
		origin := o.session.ownerOrigins[ref.Producer]
		if origin == nil || origin == o || !sameOwnerProducerScope(origin.binding, o.binding) {
			return ports.NativeOwnerCreated{}, errors.New("owner create producer unavailable")
		}
		return origin.ReadOwnerCreated(ref)
	}
	if o.restored {
		terminal, err := o.session.readTerminal(o.binding)
		if err != nil || terminal.Owner != "owner-registration" || terminal.Resource != "test-yard" {
			return ports.NativeOwnerCreated{}, errors.New("owner create original terminal unavailable")
		}
	}
	root, err := o.creationRoot()
	if err != nil || ref.Index == 0 || ref.Index > 256 || ref.Domain != "command" && ref.Domain != "operation" {
		return ports.NativeOwnerCreated{}, errors.New("owner create original reference unavailable")
	}
	directory, err := o.session.factsDirectory(o.binding)
	if err != nil {
		return ports.NativeOwnerCreated{}, err
	}
	defer unix.Close(directory)
	read := func(phase string, value any) error {
		raw, err := readFactAt(directory, factName(o.binding.Ordinal, createFactKind(ref, phase)))
		if err != nil || len(raw) > 4<<10 {
			return errors.New("owner create fact unavailable")
		}
		if strict(raw, value) != nil {
			return errors.New("owner create fact syntax")
		}
		canonical, err := json.Marshal(value)
		if err != nil || !bytes.Equal(append(canonical, '\n'), raw) {
			return errors.New("owner create fact noncanonical")
		}
		return nil
	}
	var intent ownerCreateIntent
	var result ownerCreateResult
	if read("intent", &intent) != nil || read("result", &result) != nil || intent.Version != 1 || intent.Binding != o.binding || intent.Recipe != root.Recipe || intent.Source != root.Source || intent.Index != ref.Index || intent.Domain != ref.Domain || !validOwnerCreate(intent.request()) || result.Version != 1 || result.Intent != intent || result.Status != 200 || !fingerprint(result.IdentityDigest) || !fingerprint(result.ETagDigest) {
		return ports.NativeOwnerCreated{}, errors.New("owner create original facts differ")
	}
	// Reading a fact is not a fresh invocation's success or replay authority.
	if !o.created[ref] || o.creates[ref] != intent {
		return ports.NativeOwnerCreated{}, errors.New("owner create original invocation not retained")
	}
	return ports.NativeOwnerCreated{Ref: ref, Intent: intent.request(), OperationID: result.OperationID, Status: result.Status, InstanceUUID: result.InstanceUUID, IdentityDigest: result.IdentityDigest, ETagDigest: result.ETagDigest}, nil
}
