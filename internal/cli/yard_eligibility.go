package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// A yard whose last init did not finish is not part of the installed release:
// init's instance stage writes user.subyard.initialized=false and only its
// finalize stage writes true; start and stop keep the mark. Release activation
// and readiness leave such a yard to its own init, like a stopped yard.
func instanceUninitialized(target configTarget, instance ports.InstanceInfo) bool {
	return instance.Config["user.subyard.managed"] == "true" &&
		instance.Config["user.subyard.name"] == target.Name &&
		instance.Config["user.subyard.initialized"] == "false"
}

// releaseYardUninitialized decides once per engine process whether a local yard
// is left out of release work as uninitialized. A later observation may only
// narrow the release scope: a yard once left out stays left out until a fresh
// process plans again.
func (cli *CLI) releaseYardUninitialized(ctx context.Context, target configTarget) (bool, error) {
	if target.Loaded.Context.AccessKind == domain.AccessRemote {
		return false, nil
	}
	source := cli.activation
	if source != nil {
		source.mutex.Lock()
		decided := source.uninitialized[target.Name]
		source.mutex.Unlock()
		if decided {
			return true, nil
		}
	}
	incus, _ := cli.statusPorts()
	if incus == nil {
		return false, nil
	}
	instance, err := incus.Instance(ctx, target.Loaded.Context.IncusProject, target.Loaded.Context.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !instanceUninitialized(target, instance) {
		return false, nil
	}
	if source != nil {
		source.mutex.Lock()
		if source.uninitialized == nil {
			source.uninitialized = map[string]bool{}
		}
		source.uninitialized[target.Name] = true
		source.mutex.Unlock()
	}
	return true, nil
}

// uninitializedYardWarning is the one release warning of a yard left to its init.
func uninitializedYardWarning(yard string) string {
	return fmt.Sprintf("yard %s: not initialized (its last init did not finish); release work skips it; next: yard -Y %s init", yard, yard)
}

// transitionalPowerState reports an Incus state other than running or stopped,
// which no release owner can observe or skip safely.
func transitionalPowerState(status string) bool {
	return !strings.EqualFold(status, "running") && !strings.EqualFold(status, "stopped")
}

// ActivationYardFindings attributes a yard-scoped owner's failure to yards. A
// configuration source error belongs to the yard whose own file it names and
// to no yard otherwise; any other failure belongs to the yard the owner was
// observing. Host-wide refusals, such as loading the local yard set, carry no
// yard of their own.
func (err materializedConfigActivationError) ActivationYardFindings() []releasetransition.V2YardAttribution {
	findings, ok := activationYardFindings(err.cause, err.yard, err.phase == "local yard configurations", err.configHome)
	if !ok {
		return nil
	}
	return findings
}

func activationYardFindings(err error, yard string, hostWide bool, home string) ([]releasetransition.V2YardAttribution, bool) {
	if err == nil {
		return nil, false
	}
	var inner materializedConfigActivationError
	if typed, ok := err.(materializedConfigActivationError); ok {
		return activationYardFindings(typed.cause, typed.yard, typed.phase == "local yard configurations", typed.configHome)
	}
	if refusals, ok := err.(*domain.Refusals); ok {
		var findings []releasetransition.V2YardAttribution
		for _, cause := range refusals.Unwrap() {
			own, ok := activationYardFindings(cause, yard, hostWide, home)
			if !ok {
				return nil, false
			}
			findings = append(findings, own...)
		}
		return findings, len(findings) != 0
	}
	if errors.As(err, &inner) {
		return activationYardFindings(inner, yard, hostWide, home)
	}
	var lost guestAgentLostError
	if errors.As(err, &lost) && domain.SafeName(lost.yard) {
		return []releasetransition.V2YardAttribution{{Yard: lost.yard, Kind: releasetransition.YardFindingUnreachable}}, true
	}
	var source *config.SourceError
	if errors.As(err, &source) {
		name := configSourceYard(home, source.Path)
		if name == "" {
			return nil, false
		}
		return []releasetransition.V2YardAttribution{{Yard: name, Kind: releasetransition.YardFindingConfig}}, true
	}
	if hostWide || !domain.SafeName(yard) {
		return nil, false
	}
	return []releasetransition.V2YardAttribution{{Yard: yard, Kind: releasetransition.YardFindingFailed}}, true
}

// configSourceYard names the yard whose own configuration file path is, or "".
func configSourceYard(home, path string) string {
	if home == "" || path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(home, path)
	}
	relative, err := filepath.Rel(filepath.Join(home, "yards"), filepath.Clean(path))
	if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
		return ""
	}
	name, _, _ := strings.Cut(relative, string(filepath.Separator))
	name = strings.TrimSuffix(name, ".env")
	if !domain.SafeName(name) {
		return ""
	}
	return name
}

// onlyYardConfigRefusals reports a refusal made only of named yards' own
// invalid configuration sources, which a host owner leaves to the yard-scoped
// owners. The default yard's configuration underlies every yard and the local
// yard set, so its refusal is never tolerated here.
func onlyYardConfigRefusals(err error, home string) bool {
	findings, attributed := activationYardFindings(err, "", true, home)
	for _, finding := range findings {
		attributed = attributed && finding.Kind == releasetransition.YardFindingConfig && finding.Yard != "default"
	}
	return attributed
}

// guestAgentProbeTimeout bounds the core's one liveness command in a guest.
const guestAgentProbeTimeout = 10 * time.Second

// guestAgentLost probes a yard after a failed observation: Incus reports the
// instance running, yet a trivial command cannot run in it, or does not finish
// in time. A guest exit status is an answer, so the agent is not lost.
func (cli *CLI) guestAgentLost(ctx context.Context, target configTarget) bool {
	incus, executor := cli.statusPorts()
	if incus == nil || executor == nil || ctx.Err() != nil {
		return false
	}
	instance, err := incus.Instance(ctx, target.Loaded.Context.IncusProject, target.Loaded.Context.YardInstanceName)
	if err != nil || !strings.EqualFold(instance.Status, "running") {
		return false
	}
	probe, cancel := context.WithTimeout(ctx, guestAgentProbeTimeout)
	defer cancel()
	_, err = executor.Exec(probe, target.Loaded.Context.IncusProject, target.Loaded.Context.YardInstanceName, ports.InstanceExecRequest{Command: []string{"true"}})
	var exit *ports.GuestExitError
	return err != nil && ctx.Err() == nil && !errors.As(err, &exit)
}

// guestAgentLostError keeps an owner's own public diagnostic and adds that the
// guest agent of a running yard does not answer, with its repair sequence.
type guestAgentLostError struct {
	yard  string
	cause error
}

func (err guestAgentLostError) Error() string {
	return fmt.Sprintf("yard %s: the guest agent does not answer while Incus reports the yard running: %v", err.yard, err.cause)
}

func (err guestAgentLostError) Unwrap() error { return err.cause }

func (err guestAgentLostError) ActivationDiagnostic() (string, string) {
	message := fmt.Sprintf("yard %s: the guest agent does not answer while Incus reports the yard running", err.yard)
	var diagnostic interface{ ActivationDiagnostic() (string, string) }
	if errors.As(err.cause, &diagnostic) {
		if own, _ := diagnostic.ActivationDiagnostic(); own != "" {
			message += " (" + own + ")"
		}
	}
	message += fmt.Sprintf("; retry yard migrate, and if it persists run yard -Y %s stop --force, then yard -Y %s start, then yard migrate", err.yard, err.yard)
	return message, "run yard migrate"
}

// observedYardsError is an owner's failure in some yards together with what
// it observed in the others, so a failure in one yard never hides another
// yard's drift. Without attribution of the failure itself it attributes
// nothing, which keeps the release gate host-wide.
type observedYardsError struct {
	error
	home     string
	observed []releasetransition.V2YardAttribution
}

func (err observedYardsError) Unwrap() error { return err.error }

func (err observedYardsError) ActivationYardFindings() []releasetransition.V2YardAttribution {
	own, ok := activationYardFindings(err.error, "", true, err.home)
	if !ok {
		return nil
	}
	return append(own, err.observed...)
}
