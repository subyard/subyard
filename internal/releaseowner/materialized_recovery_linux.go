//go:build linux

package releaseowner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"golang.org/x/sys/unix"
)

// This closure proves only that the exact admitted materialized-files invocation
// is quiescent. Its missing whole terminal remains incomplete, hence FAILED;
// the original per-file owning guards decide whether a later retry may proceed.
func (s *session) queryMaterializedInvocation(ctx context.Context, request Request) (Reply, error) {
	unknown := func() (Reply, error) { return Reply{Code: "unknown"}, nil }
	if request.Binding == nil || s.binding == nil || *request.Binding != *s.binding || request.Target == nil || len(s.leases) != 2 || s.ownerRelease == nil {
		return unknown()
	}
	binding, target := *request.Binding, *request.Target
	kind, _, err := ParseAfterOwner(target.AfterOwner)
	if err != nil || kind != "materialized-files" || target.Class != "materialized-files" || target.Owner != "materialized-config" || target.State == "deferred" {
		return unknown()
	}
	completed, err := s.ownerCompletion()
	if err != nil || !sameOwnerProducerScope(completed.Producer, binding) || s.checkOwnerResultEffectBinding(binding) != nil {
		return unknown()
	}
	validator, ok := s.components.(interface {
		ValidateMaterializedAfterOwnerRecovery(context.Context, Target, AfterOwnerCompletion) error
	})
	if !ok || validator.ValidateMaterializedAfterOwnerRecovery(ctx, target, completed) != nil {
		return unknown()
	}
	directory, err := s.factsDirectory(binding)
	if err != nil {
		return unknown()
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
			return errors.New("noncanonical materialized invocation fact")
		}
		return nil
	}
	recipe, err := recipeFingerprint(Request{Op: "apply-activation", Target: &target})
	if err != nil {
		return unknown()
	}
	var intent Intent
	expected := Intent{Version: 1, Binding: binding, Owner: target.Owner, Resource: target.Yard, Expected: target.Actual, Desired: target.Desired, Source: target.Source, Recipe: recipe}
	if read("intent", &intent) != nil || intent != expected {
		return unknown()
	}
	var accepted Terminal
	if read("accepted", &accepted) != nil {
		return unknown()
	}
	canonical, _ := json.Marshal(accepted)
	expectedAccepted, _ := json.Marshal(Terminal{Version: 1, Binding: binding, Owner: target.Owner, Resource: target.Yard, State: "accepted", Operations: []ports.NativeOperation{}})
	if !bytes.Equal(canonical, expectedAccepted) {
		return unknown()
	}
	if terminal, err := s.readTerminal(binding); err == nil {
		if terminal.Owner != target.Owner || terminal.Resource != target.Yard {
			return unknown()
		}
		return Reply{Code: terminal.State, Terminal: terminal}, nil
	} else if !errors.Is(err, unix.ENOENT) {
		return unknown()
	}
	fd, err := unix.Openat(directory, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return unknown()
	}
	inventory := os.NewFile(uintptr(fd), "bounded materialized invocation inventory")
	names, readErr := inventory.Readdirnames(8193)
	inventory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || len(names) > 8192 {
		return unknown()
	}
	prefix := fmt.Sprintf("%03d.", binding.Ordinal)
	present := map[string]bool{}
	for _, name := range names {
		if strings.HasPrefix(strings.TrimPrefix(name, "."), prefix) {
			if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
				return unknown()
			}
			present[strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")] = true
		}
	}
	seen := map[string]bool{"reservation": true, "intent": true, "accepted": true}
	if _, err := reservationAt(directory, binding); err != nil {
		return unknown()
	}
	operations := []ports.NativeOperation{}
	ids := map[string]bool{}
	missingTerminal := false
	commands := 0
	for ordinal := 1; ordinal <= 256; ordinal++ {
		key := fmt.Sprintf("command-%03d", ordinal)
		if present[key+"-intent"] || present[key+"-terminal"] {
			if ordinal != commands+1 || !present[key+"-intent"] || !present[key+"-terminal"] {
				return unknown()
			}
			var before, after commandFact
			if read(key+"-intent", &before) != nil || read(key+"-terminal", &after) != nil || before != after || len(before.Kind) > 256 || !domain.SafeID(before.Kind) || !fingerprint(before.Source) {
				return unknown()
			}
			seen[key+"-intent"], seen[key+"-terminal"] = true, true
			commands++
		}
		key = fmt.Sprintf("operation-%03d", ordinal)
		if !present[key+"-intent"] && !present[key+"-accepted"] && !present[key+"-terminal"] {
			continue
		}
		if ordinal != len(operations)+1 || !present[key+"-intent"] || !present[key+"-accepted"] {
			return unknown()
		}
		var before, accepted ports.NativeOperation
		if read(key+"-intent", &before) != nil || read(key+"-accepted", &accepted) != nil {
			return unknown()
		}
		projection := accepted
		projection.ID = ""
		if projection != before || before.ID != "" || before.Status != 0 || before.Kind != "instance-exec" || !operationSizeValid(accepted) || !fingerprint(before.Server) || before.Project != target.Project || before.Resource != target.Instance || !domain.SafeID(accepted.ID) || ids[accepted.ID] {
			return unknown()
		}
		ids[accepted.ID] = true
		if present[key+"-terminal"] {
			var terminal ports.NativeOperation
			if read(key+"-terminal", &terminal) != nil {
				return unknown()
			}
			projection = terminal
			projection.Status = 0
			if projection != accepted || terminal.Status != 200 && terminal.Status != 400 && terminal.Status != 401 {
				return unknown()
			}
			seen[key+"-terminal"] = true
		} else {
			missingTerminal = true
		}
		seen[key+"-intent"], seen[key+"-accepted"] = true, true
		operations = append(operations, accepted)
	}
	for name := range present {
		if !seen[name] {
			return unknown()
		}
	}
	if missingTerminal {
		// Only previously accepted exact instance-exec IDs can reach the existing
		// refresh path. Tokens, create receipts, holes and opaque commands refused above.
		reply, err := s.queryInvocation(ctx, binding)
		if err != nil || reply.Terminal == nil || reply.Terminal.Binding != binding || len(reply.Terminal.Operations) != len(operations) {
			return unknown()
		}
	}
	for index, accepted := range operations {
		var terminal ports.NativeOperation
		if read(fmt.Sprintf("operation-%03d-terminal", index+1), &terminal) != nil {
			return unknown()
		}
		projection := terminal
		projection.Status = 0
		if projection != accepted || terminal.Status != 200 && terminal.Status != 400 && terminal.Status != 401 {
			return unknown()
		}
		operations[index] = terminal
	}
	failed := Terminal{Version: 1, Binding: binding, Owner: target.Owner, Resource: target.Yard, State: "failed", Operations: operations}
	if s.writeTerminal(failed) != nil {
		return unknown()
	}
	return Reply{Code: "failed", Terminal: &failed}, nil
}
