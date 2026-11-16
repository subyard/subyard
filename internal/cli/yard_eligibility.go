package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// A yard whose last init did not finish is not part of the installed release:
// init's instance stage writes user.subyard.initialized=false and only its
// finalize stage writes true; start and stop keep the mark. Release activation
// and readiness leave such a yard to its own init, like a stopped yard. An older
// engine without this rule observes the yard and fails closed.
func instanceUninitialized(target configTarget, instance ports.InstanceInfo) bool {
	return instance.Config["user.subyard.managed"] == "true" &&
		instance.Config["user.subyard.name"] == target.Name &&
		instance.Config["user.subyard.initialized"] == "false"
}

// noteYardUninitialized decides from an instance read whether release work
// leaves a local yard to its init. The decision is made once per engine
// process and may only narrow: a yard once left out stays left out, so the
// yard init enters mid-transaction is never touched afterwards.
func (cli *CLI) noteYardUninitialized(target configTarget, instance ports.InstanceInfo) bool {
	source := cli.activation
	if source == nil || target.Loaded.Context.AccessKind == domain.AccessRemote {
		return false
	}
	source.mutex.Lock()
	defer source.mutex.Unlock()
	if source.uninitialized[target.Name] {
		return true
	}
	if !instanceUninitialized(target, instance) {
		return false
	}
	if source.uninitialized == nil {
		source.uninitialized = map[string]bool{}
	}
	source.uninitialized[target.Name] = true
	return true
}

// releaseYardUninitialized reads the yard's instance and applies
// noteYardUninitialized. An absent instance is not uninitialized; any other
// read failure is the caller's finding for that yard.
func (cli *CLI) releaseYardUninitialized(ctx context.Context, target configTarget) (bool, error) {
	source := cli.activation
	if source == nil || target.Loaded.Context.AccessKind == domain.AccessRemote {
		return false, nil
	}
	source.mutex.Lock()
	decided := source.uninitialized[target.Name]
	source.mutex.Unlock()
	if decided {
		return true, nil
	}
	incus, _ := cli.statusPorts()
	if incus == nil || ports.LocalInstallationAbsent(incus) {
		return false, nil
	}
	instance, err := incus.Instance(ctx, target.Loaded.Context.IncusProject, target.Loaded.Context.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return cli.noteYardUninitialized(target, instance), nil
}

// uninitializedYardWarning is the one release warning of a yard left to its init.
func uninitializedYardWarning(yard string) string {
	next := "yard init"
	if domain.SafeName(yard) {
		next = "yard -Y " + yard + " init"
	}
	return fmt.Sprintf("yard %s: not initialized (its last init did not finish); release work skips it; next: %s", yard, next)
}

// uninitializedYardWarnings states each yard once, within the release
// outcome's warning bound.
func uninitializedYardWarnings(yards []string) []string {
	warnings := make([]string, 0, len(yards))
	for _, yard := range yards {
		warnings = append(warnings, uninitializedYardWarning(yard))
	}
	slices.Sort(warnings)
	warnings = slices.Compact(warnings)
	if len(warnings) > 16 {
		warnings = append(warnings[:16], fmt.Sprintf("%d additional yards are not initialized; release work skips them; next: yard -Y NAME init for each", len(warnings)-16))
	}
	return warnings
}
