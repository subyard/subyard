package reconcileruntime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// provisionReadinessError names every unmet provision condition observed after apply.
type provisionReadinessError struct {
	yard  string
	cause error
}

func (failure *provisionReadinessError) Error() string {
	message, _ := failure.ActivationDiagnostic()
	return message
}

func (failure *provisionReadinessError) Unwrap() error { return failure.cause }

func (failure *provisionReadinessError) ActivationDiagnostic() (string, string) {
	return fmt.Sprintf("yard %s: provision did not converge: %s", failure.yard, failure.cause),
		"correct the reported yard state, then run yard -Y " + failure.yard + " init"
}

func (runtime Runtime) provisionConverged(ctx context.Context) (bool, error) {
	return runtime.provisionReadiness(ctx, false)
}

// provisionReadiness returns expected drift as a state and stops at the first
// unmet condition or inspection failure. At the post-apply boundary
// (diagnostic) it evaluates every condition that does not depend on an unmet
// or unreadable one and names each, including inspection failures.
func (runtime Runtime) provisionReadiness(ctx context.Context, diagnostic bool) (bool, error) {
	findings := provisionFindings{ctx: ctx, collect: diagnostic}
	if err := runtime.observeProvision(ctx, &findings); err != nil {
		return false, err
	}
	if findings.stop != nil {
		return false, findings.stop
	}
	if len(findings.pending) == 0 && len(findings.failures) == 0 {
		return true, nil
	}
	if !diagnostic {
		return false, nil
	}
	var pending error
	if len(findings.pending) == 1 {
		var reboot diskTmpRebootError
		if errors.As(findings.pending[0], &reboot) {
			pending = reboot
		}
	}
	if pending == nil && len(findings.pending) != 0 {
		pending = &provisionReadinessError{yard: runtime.Yard.YardName, cause: domain.CollectOrderedRefusals(findings.pending...)}
	}
	return false, domain.CollectOrderedRefusals(append(findings.failures, pending)...)
}

// provisionFindings keeps unmet conditions and inspection failures in their
// stable check order.
type provisionFindings struct {
	ctx      context.Context
	collect  bool
	pending  []error
	failures []error
	stop     error
}

func (findings *provisionFindings) stopped() bool {
	return findings.stop != nil || (!findings.collect && len(findings.pending) != 0)
}

// note records an inspection failure or an unmet condition and reports whether
// the condition holds. After a stop nothing is recorded and nothing holds.
func (findings *provisionFindings) note(ok bool, err error, reason string) bool {
	if findings.stopped() {
		return false
	}
	switch {
	case findings.ctx.Err() != nil:
		findings.stop = findings.ctx.Err()
	case err != nil && findings.collect:
		findings.failures = append(findings.failures, err)
	case err != nil:
		findings.stop = err
	case !ok:
		findings.pending = append(findings.pending, errors.New(reason))
	default:
		return true
	}
	return false
}

// probe runs one check unless the observation already stopped.
func (findings *provisionFindings) probe(reason string, check func() (bool, error)) bool {
	if findings.stopped() {
		return false
	}
	ok, err := check()
	return findings.note(ok, err, reason)
}

// observeProvision returns a refusal that replaces the observation.
func (runtime Runtime) observeProvision(ctx context.Context, findings *provisionFindings) error {
	if !findings.probe("Incus is not reachable", func() (bool, error) { return runtime.incusReady(ctx) }) {
		return nil
	}
	selectionErr := ValidateBubblewrapSelection(runtime.environmentValue("BWRAP_APPARMOR"), runtime.environmentValue("CODING_TOOL_INTEGRATIONS"))
	codingTools := runtime.environmentValue("ALLOWS_CODING_TOOLS") != "false"
	version := runtime.environmentValue("CCUSAGE_VERSION")
	versionResolved := version != "" && version != "latest"
	if selectionErr == nil && codingTools {
		findings.note(versionResolved, nil, "the ccusage version is not resolved")
	}
	var state ports.ReconcileState
	if !findings.probe("the yard instance does not exist", func() (bool, error) {
		var err error
		state, err = runtime.reconcileState(ctx)
		return err == nil && state.InstanceFound, err
	}) {
		return nil
	}
	instance := state.Instance
	// An incompatible selection blocks provisioning. Observe service ownership
	// before returning that refusal, even when other provision work is pending.
	if selectionErr != nil {
		if !strings.EqualFold(instance.Status, "running") || runtime.Executor == nil {
			return selectionErr
		}
		if _, _, err := runtime.integrationServices(ctx, "observe"); err != nil {
			return err
		}
		return selectionErr
	}
	marker, _ := instance.EffectiveConfig("user.subyard.ccusage_version")
	findings.probe("the static preview route differs", func() (bool, error) {
		return runtime.previewRouteConverged(ctx, instance), nil
	})
	if strings.EqualFold(instance.Status, "stopped") {
		policy, _ := instance.EffectiveConfig("user.subyard.disk_tmp")
		findings.note(policy == "v1", nil, "the guest /tmp policy marker is not installed")
		findings.probe("the installed static preview helper differs", func() (bool, error) {
			hash, err := runtime.previewSourceHash()
			installed, _ := instance.EffectiveConfig("user.subyard.preview_sha256")
			return installed == hash, err
		})
		findings.probe("the installed static preview endpoint differs", func() (bool, error) {
			hash, err := runtime.previewEndpointHash(ctx)
			installed, _ := instance.EffectiveConfig("user.subyard.preview_endpoint_sha256")
			return installed == hash, err
		})
		findings.note(instanceIntentionallyStopped(instance), nil, "the yard is stopped without an intentional stop")
		if !codingTools {
			findings.note(marker == "", nil, "a ccusage version is recorded while coding tools are disabled")
		} else if versionResolved {
			findings.note(marker == version, nil, "the recorded ccusage version is pending or stale")
		}
		return nil
	}
	if !findings.note(strings.EqualFold(instance.Status, "running"), nil, "the yard instance is "+instanceStatusText(instance.Status)) {
		return nil
	}
	if runtime.Executor == nil {
		return errors.New("Incus executor is required")
	}
	runtime.observeRunningProvision(ctx, findings, codingTools && versionResolved, version, marker)
	return nil
}

func (runtime Runtime) observeRunningProvision(ctx context.Context, findings *provisionFindings, checkVersion bool, version, marker string) {
	codingTools := runtime.environmentValue("ALLOWS_CODING_TOOLS") != "false"
	// Integration ownership probes require the generic Docker substrate first.
	dockerReady := findings.probe("Docker in the yard is not ready", func() (bool, error) {
		return runtime.guestCheck(ctx, []string{"sh", "-c", "timeout --kill-after=1 10 docker container ls -a --format '{{.Names}}' >/dev/null 2>&1"})
	})
	findings.probe("agent commands are missing or Docker forwarding is not allowed", func() (bool, error) {
		commands, err := runtime.provisionAgentCommands()
		if err != nil {
			return false, err
		}
		commandCheck := []string{"sh", "-c", `
for command do command -v "$command" >/dev/null || exit 1; done
jq -e '."ip-forward-no-drop" == true' /etc/docker/daemon.json >/dev/null \
  && ! iptables -S FORWARD 2>/dev/null | grep -qx -- '-P FORWARD DROP'
`, "subyard"}
		return runtime.guestCheck(ctx, append(commandCheck, commands...))
	})
	if !findings.stopped() {
		checks, err := runtime.provisionAgentChecks()
		if findings.note(true, err, "") {
			for _, check := range checks {
				findings.probe("agent check failed: "+check, func() (bool, error) { return runtime.guestCheck(ctx, []string{check}) })
			}
		}
	}
	user := runtime.Yard.DevUser
	if user == "" {
		user = runtime.environmentDefault("DEV_USER", "dev")
	}
	home := ""
	if findings.probe("the yard user "+user+" does not exist", func() (bool, error) {
		passwd, ok, err := runtime.guestObserve(ctx, provisionPasswdProbe(user))
		if fields := strings.Split(strings.TrimSpace(string(passwd.Stdout)), ":"); ok && len(fields) >= 6 {
			home = fields[5]
		}
		return ok, err
	}) {
		findings.note(home != "", nil, "the yard user "+user+" has no home directory")
	}
	if checkVersion {
		findings.probe("installed ccusage is not version "+version, func() (bool, error) {
			ccusagePath := runtime.environmentDefault("CCUSAGE_INSTALL_PATH", "/usr/local/bin/ccusage")
			reported, ok, err := runtime.guestObserve(ctx, provisionUsageProbe(ccusagePath, runtime.environmentDefault("CCUSAGE_EXPECTED_OWNER", "0:0")))
			return ok && strings.TrimSpace(string(reported.Stdout)) == "ccusage "+version, err
		})
	}
	findings.probe("guest configuration files differ", func() (bool, error) {
		files, err := runtime.guestConfigFiles()
		if err != nil {
			return false, err
		}
		return runtime.guestConfigsConverged(ctx, files)
	})
	// The stage refreshes static skills, so their drift is its work: a provider
	// deselected back to the recorded selection leaves the facts check below clean.
	findings.probe("agent skills differ", func() (bool, error) {
		paths, err := runtime.agentSkillSources()
		if err != nil || len(paths) == 0 {
			return err == nil, err
		}
		return runtime.agentSkillsConverged(ctx, paths)
	})
	sudoers := "/etc/sudoers.d/90-subyard-" + user
	sudoCommand, sudoReason := []string{"test", "-f", sudoers}, "the yard user sudo rule is missing"
	if !runtime.Yard.DevSudo {
		sudoCommand, sudoReason = []string{"sh", "-c", `[ ! -e "$1" ]`, "subyard", sudoers}, "the yard user sudo rule is present"
	}
	findings.probe(sudoReason, func() (bool, error) { return runtime.guestCheck(ctx, sudoCommand) })
	if home != "" {
		findings.probe("host links in the yard user home are missing", func() (bool, error) {
			return runtime.provisionLinksConverged(ctx, home)
		})
		findings.probe("the legacy OpenCode storage link is present", func() (bool, error) {
			link, exists, err := runtime.guestObserve(ctx, []string{"readlink", filepath.Join(home, ".local", "share", "opencode", "storage")})
			return !exists || strings.TrimSpace(string(link.Stdout)) != "/mnt/host/agent-sessions/opencode/storage", err
		})
	}
	findings.probe("project hooks differ", func() (bool, error) { return runtime.projectHooksConverged(ctx) })
	findings.probe("the static preview differs", func() (bool, error) { return runtime.previewConverged(ctx) })
	if runtime.guestFactsAvailable() {
		// Init applies the selection snapshot even when profiles-only drift
		// does not require a release transition. Provision writes begin/commit.
		findings.probe("the guest facts differ from the applied selection", func() (bool, error) {
			exit, err := runtime.guestFactsCheck(ctx)
			return exit == 0, err
		})
	}
	// Resolved contexts opt into the shared integration ownership contract.
	integrations := !codingTools
	for _, entry := range runtime.Environment {
		integrations = integrations || strings.HasPrefix(entry, "INTEGRATION_HOST_LINKS=")
	}
	if integrations && dockerReady {
		findings.probe("integration services have pending work", func() (bool, error) {
			return integrationPlanConverged(runtime.IntegrationPlan(ctx))
		})
	}
	if checkVersion {
		findings.note(marker == version, nil, "the recorded ccusage version is pending or stale")
	}
	if !findings.stopped() {
		pending, err := runtime.diskTmpPending(ctx)
		if findings.note(true, err, "") && pending != nil {
			findings.pending = append(findings.pending, pending)
		}
	}
}

func instanceStatusText(status string) string {
	if status == "" || len(status) > 32 || !domain.SafeName(strings.ToLower(status)) {
		return "not running"
	}
	return strings.ToLower(status)
}
