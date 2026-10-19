package releaseowner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"golang.org/x/sys/unix"
	"strings"
	"sync"
	"time"
)

// The invocation intent precedes every POST. SDK IDs are recorded immediately
// on receipt, before waiting; losing the response before an ID remains UNKNOWN.
type invocationObserver struct {
	mu              sync.Mutex
	session         *session
	binding         Binding
	ownerToken      string
	restored        bool
	accepted        []ports.NativeOperation
	started         []ports.NativeOperation
	terminal        map[string]ports.NativeOperation
	fault           error
	commands        []commandFact
	commandTerminal map[uint64]bool
	creates         map[ports.NativeOwnerCreateRef]ownerCreateIntent
	created         map[ports.NativeOwnerCreateRef]bool
}

// queryInvocation only refreshes identities that were durably accepted. It does
// not infer that the entire component finished, replay a missing request or
// turn a vanished UUID/opaque command into a drain witness.
func (s *session) queryInvocation(ctx context.Context, binding Binding) (Reply, error) {
	if terminal, err := s.readTerminal(binding); err == nil {
		return Reply{Terminal: terminal}, nil
	}
	reader, ok := s.components.(interface {
		QueryOperation(context.Context, ports.NativeOperation) (ports.NativeOperation, error)
	})
	directory, err := s.factsDirectory(binding)
	if err != nil {
		return Reply{Code: "unknown"}, nil
	}
	defer unix.Close(directory)
	read := func(kind string, target any) error {
		raw, err := readFactAt(directory, factName(binding.Ordinal, kind))
		if err != nil {
			return err
		}
		if err := strict(raw, target); err != nil {
			return err
		}
		canonical, err := json.Marshal(target)
		if err != nil || !bytes.Equal(append(canonical, '\n'), raw) {
			return errors.New("noncanonical native operation fact")
		}
		return nil
	}
	var accepted Terminal
	if err := read("accepted", &accepted); err != nil || accepted.Binding != binding || accepted.Version != 1 || accepted.State != "accepted" {
		return Reply{Code: "unknown"}, nil
	}
	result := accepted
	result.State, result.Operations = "unknown", []ports.NativeOperation{}
	for ordinal := 1; ordinal <= 256; ordinal++ {
		var intent, accepted, terminal ports.NativeOperation
		err := read(fmt.Sprintf("operation-%03d-intent", ordinal), &intent)
		if errors.Is(err, unix.ENOENT) {
			break
		}
		if err != nil || intent.ID != "" || intent.Status != 0 || !fingerprint(intent.Server) {
			return Reply{Code: "unknown"}, nil
		}
		if err := read(fmt.Sprintf("operation-%03d-accepted", ordinal), &accepted); err != nil {
			return Reply{Code: "unknown"}, nil
		}
		projection := accepted
		projection.ID = ""
		if projection != intent || !domain.SafeID(accepted.ID) || nativeImageToken(accepted.Kind) && (!nativeTokenUUID(accepted.ID) || !nativeTokenFingerprint(accepted.Resource)) {
			return Reply{Code: "unknown"}, nil
		}
		err = read(fmt.Sprintf("operation-%03d-terminal", ordinal), &terminal)
		if errors.Is(err, unix.ENOENT) {
			if nativeImageToken(accepted.Kind) {
				drain, available := s.components.(interface {
					DrainOwnerImageSecret(context.Context, ports.NativeOperation) (ports.NativeOperation, error)
				})
				if len(s.leases) != 2 || s.ownerRelease == nil || !available {
					return Reply{Code: "unknown"}, nil
				}
				cleanup, cancel := context.WithTimeout(ctx, 30*time.Second)
				terminal, err = drain.DrainOwnerImageSecret(cleanup, accepted)
				cancel()
			} else {
				if !ok {
					return Reply{Code: "unknown"}, nil
				}
				terminal, err = reader.QueryOperation(ctx, accepted)
			}
			projection = terminal
			projection.Status = 0
			if err != nil || projection != accepted {
				return Reply{Code: "unknown"}, nil
			}
			if terminal.Status == 200 || terminal.Status == 400 || terminal.Status == 401 {
				if err := s.writeFact(binding, fmt.Sprintf("operation-%03d-terminal", ordinal), terminal); err != nil {
					return Reply{Code: "unknown"}, nil
				}
			}
		} else if err != nil {
			return Reply{Code: "unknown"}, nil
		}
		projection = terminal
		projection.Status = 0
		if projection != accepted {
			return Reply{Code: "unknown"}, nil
		}
		result.Operations = append(result.Operations, terminal)
	}
	return Reply{Code: "unknown", Terminal: &result}, nil
}

type commandFact struct {
	Kind   string `json:"kind"`
	Source string `json:"source"`
}

func (o *invocationObserver) CommandStarted(kind, source string) (ordinal uint64, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	defer func() {
		if err != nil {
			o.fault = err
		}
	}()
	if o.fault != nil || len(kind) > 256 || !domain.SafeID(kind) || !fingerprint(source) || len(o.commands) >= 256 {
		return 0, errors.New("native command intent binding")
	}
	value := commandFact{kind, source}
	o.commands = append(o.commands, value)
	ordinal = uint64(len(o.commands))
	return ordinal, o.session.writeFact(o.binding, fmt.Sprintf("command-%03d-intent", ordinal), value)
}

func (o *invocationObserver) CommandTerminal(ordinal uint64) (err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	defer func() {
		if err != nil {
			o.fault = err
		}
	}()
	if ordinal == 0 || ordinal > uint64(len(o.commands)) || o.commandTerminal[ordinal] {
		return errors.New("native command terminal binding")
	}
	if err := o.session.writeFact(o.binding, fmt.Sprintf("command-%03d-terminal", ordinal), o.commands[ordinal-1]); err != nil {
		return err
	}
	if o.commandTerminal == nil {
		o.commandTerminal = map[uint64]bool{}
	}
	o.commandTerminal[ordinal] = true
	return nil
}

func (o *invocationObserver) Before(value ports.NativeOperation) (err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	defer func() {
		if err != nil {
			o.fault = err
		}
	}()
	if o.fault != nil || !operationSizeValid(value) || value.ID != "" || value.Status != 0 || !domain.SafeID(value.Kind) || !domain.SafeName(value.Project) || !domain.SafeName(value.Resource) || !fingerprint(value.Server) || nativeImageToken(value.Kind) && !nativeTokenFingerprint(value.Resource) || len(o.started) >= 256 || len(o.started) != len(o.accepted) {
		return errors.New("native operation intent binding")
	}
	o.started = append(o.started, value)
	return o.session.writeFact(o.binding, fmt.Sprintf("operation-%03d-intent", len(o.started)), value)
}

func (o *invocationObserver) Accepted(value ports.NativeOperation) (err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	defer func() {
		if err != nil {
			o.fault = err
		}
	}()
	if !operationSizeValid(value) || !domain.SafeID(value.ID) || !domain.SafeID(value.Kind) || !domain.SafeName(value.Project) || !domain.SafeName(value.Resource) || !fingerprint(value.Server) || value.Status != 0 || nativeImageToken(value.Kind) && (!nativeTokenFingerprint(value.Resource) || !nativeTokenUUID(value.ID)) || len(o.accepted) >= 256 {
		return errors.New("native operation identity")
	}
	before := value
	before.ID = ""
	if len(o.started) != len(o.accepted)+1 || o.started[len(o.accepted)] != before {
		return errors.New("native operation without exact prior intent")
	}
	for _, prior := range o.accepted {
		if prior.ID == value.ID {
			return errors.New("duplicate native operation")
		}
	}
	o.accepted = append(o.accepted, value)
	if o.terminal == nil {
		o.terminal = map[string]ports.NativeOperation{}
	}
	return o.session.writeFact(o.binding, fmt.Sprintf("operation-%03d-accepted", len(o.accepted)), value)
}

func operationSizeValid(value ports.NativeOperation) bool {
	return len(value.Kind) <= 256 && len(value.Project) <= 256 && len(value.Resource) <= 256 && len(value.ID) <= 256
}
func (o *invocationObserver) Terminal(value ports.NativeOperation) (err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	defer func() {
		if err != nil {
			o.fault = err
		}
	}()
	for n, prior := range o.accepted {
		if prior.ID == value.ID {
			copy := value
			copy.Status = prior.Status
			if copy != prior || (value.Status != 200 && value.Status != 400 && value.Status != 401) {
				return errors.New("native operation terminal binding")
			}
			if _, exists := o.terminal[value.ID]; exists {
				return errors.New("duplicate native operation terminal")
			}
			if err := o.session.writeFact(o.binding, fmt.Sprintf("operation-%03d-terminal", n+1), value); err != nil {
				return err
			}
			o.terminal[value.ID] = value
			return nil
		}
	}
	return errors.New("native operation without acceptance")
}

// failed refuses joined errors: an additional branch can represent an opaque
// effect whose closure the final SDK operation does not prove.
func (o *invocationObserver) localOnly() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.fault == nil && len(o.started) == 0 && len(o.accepted) == 0 && len(o.terminal) == 0 && len(o.commands) == 0 && len(o.commandTerminal) == 0
}

func nativeImageToken(kind string) bool {
	switch kind {
	case "owner-image-secret-current-primary", "owner-image-secret-current-fallback",
		"owner-image-secret-legacy-primary", "owner-image-secret-legacy-fallback",
		"owner-image-secret-recreate-primary", "owner-image-secret-recreate-fallback":
		return true
	}
	return false
}

func nativeTokenFingerprint(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func nativeTokenUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func nativeOperationSucceeded(operation ports.NativeOperation) bool {
	return operation.Status == 200 || operation.Status == 401 && nativeImageToken(operation.Kind) && nativeTokenFingerprint(operation.Resource) && nativeTokenUUID(operation.ID)
}

func (o *invocationObserver) failed(err error) bool {
	for err != nil {
		if failure, ok := err.(*ports.NativeOperationFailure); ok {
			o.mu.Lock()
			defer o.mu.Unlock()
			terminal, found := o.terminal[failure.Operation.ID]
			return o.fault == nil && found && terminal == failure.Operation && !nativeOperationSucceeded(terminal) && len(o.started) == len(o.accepted) && len(o.accepted) == len(o.terminal) && len(o.commands) == len(o.commandTerminal) && len(o.creates) == len(o.created)
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}
func (o *invocationObserver) closed() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fault != nil {
		return o.fault
	}
	if len(o.started) != len(o.accepted) || len(o.accepted) != len(o.terminal) || len(o.commands) != len(o.commandTerminal) {
		return errors.New("native operation remains unresolved")
	}
	if len(o.creates) != len(o.created) {
		return errors.New("native owner create result remains unresolved")
	}
	for _, terminal := range o.terminal {
		if !nativeOperationSucceeded(terminal) {
			return errors.New("native operation did not succeed")
		}
	}
	return nil
}

func (o *invocationObserver) Uncertain() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fault = errors.New("native operation closure is unknown")
}
func (o *invocationObserver) operations() []ports.NativeOperation {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := make([]ports.NativeOperation, 0, len(o.accepted))
	for _, accepted := range o.accepted {
		result = append(result, o.terminal[accepted.ID])
	}
	return result
}
