package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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
	if ports.LocalInstallationAbsent(incus) {
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
	// Every aggregate attributes each of its causes; errors.As below would
	// see only the first and leave the other yards unattributed.
	if refusals, ok := err.(interface{ Unwrap() []error }); ok {
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

// onlyNamedYardRefusals reports a refusal made only of named yards' own
// findings: their invalid configuration sources, or with anyFinding also a
// yard whose installed context cannot be read. The default yard's
// configuration underlies every yard and the local yard set, so its refusal
// is never tolerated here.
func onlyNamedYardRefusals(err error, home string, anyFinding bool) bool {
	findings, attributed := activationYardFindings(err, "", true, home)
	for _, finding := range findings {
		attributed = attributed && finding.Yard != "default" &&
			(finding.Kind == releasetransition.YardFindingConfig || anyFinding)
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
	if ports.LocalInstallationAbsent(incus) || ctx.Err() != nil {
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

// tolerantActivationTargets loads the local yard set for release observation.
// Yards whose own configuration cannot be loaded are returned as yardRefusals
// beside the other targets, so they are reported without hiding the others;
// any other refusal, including the default yard's, is err with no targets.
// observing also tolerates a named yard whose installed context cannot be
// read, as a lost guest agent leaves it: yard-scoped owners report that yard
// and still observe the others. Host owners never serve a set reduced so.
func (cli *CLI) tolerantActivationTargets(ctx context.Context, yard, home string, allLocal bool, files *config.ProspectiveFiles, observing bool) (config.Loaded, []configTarget, error, error) {
	loaded, targets, err := cli.releaseActivationTargets(ctx, yard, home, allLocal, files)
	if err == nil {
		return loaded, targets, nil, nil
	}
	if len(targets) == 0 || !onlyNamedYardRefusals(err, home, observing) {
		return loaded, nil, nil, err
	}
	if observing {
		err = cli.probedLoadRefusals(ctx, loaded, err)
	}
	return loaded, targets, err, nil
}

// probedLoadRefusals probes each yard whose context could not be read: a lost
// guest agent becomes that yard's unreachable finding.
func (cli *CLI) probedLoadRefusals(ctx context.Context, loaded config.Loaded, err error) error {
	aggregate, ok := err.(interface{ Unwrap() []error })
	causes := []error{err}
	if ok {
		causes = aggregate.Unwrap()
	}
	probed := make([]error, 0, len(causes))
	for _, cause := range causes {
		var failure materializedConfigActivationError
		if errors.As(cause, &failure) && failure.phase == "configuration" && domain.SafeName(failure.yard) {
			var source *config.SourceError
			if target, loadErr := cli.discoverYardTarget(failure.yard, loaded.Context.Paths.ConfigHome); loadErr == nil && !errors.As(cause, &source) &&
				cli.guestAgentLost(ctx, target) {
				failure.cause = guestAgentLostError{failure.yard, failure.cause}
				cause = failure
			}
		}
		probed = append(probed, cause)
	}
	return domain.CollectRefusals(probed...)
}

// discoverYardTarget locates a yard's instance without any guest read.
func (cli *CLI) discoverYardTarget(yard, home string) (configTarget, error) {
	loaded, identity, sources, err := cli.discoverReleaseContext(yard, home)
	if err == nil {
		return configTarget{Name: yard, Loaded: loaded}, nil
	}
	if sources == nil {
		return configTarget{}, err
	}
	return configTarget{Name: yard, Loaded: identity}, nil
}
