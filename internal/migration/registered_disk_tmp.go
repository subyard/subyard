package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func assessRegisteredDiskTmp(ctx context.Context, runtime reconcileruntime.Runtime) error {
	if runtime.Executor == nil {
		return errors.New("Incus executor is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	script, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "scripts", "guest-disk-tmp.sh"))
	if err != nil {
		return err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"sh", "-s", "--", "assess"}, Stdin: script})
	if result.ExitCode == 0 && err == nil {
		return nil
	}
	var exit *ports.GuestExitError
	guestExit := errors.As(err, &exit) && exit.Code == result.ExitCode && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	if result.ExitCode == 10 && guestExit {
		return nil
	}
	if guestExit && result.ExitCode != 0 {
		return &guestDiskTmpRefusal{yard: runtime.Yard.YardName, exit: result.ExitCode, findings: guestDiskTmpFindings(result.Stderr)}
	}
	return errors.Join(fmt.Errorf("yard %s: assess guest disk /tmp safety: exit %d", runtime.Yard.YardName, result.ExitCode), err)
}

// The guest is outside the host's trust boundary: only this leaf's own prefixed
// findings in a closed alphabet reach the operator, and only a bounded number.
const (
	guestDiskTmpFindingPrefix = "guest-disk-tmp: "
	guestDiskTmpFindingLimit  = 8
)

var guestDiskTmpFinding = regexp.MustCompile(`^[A-Za-z0-9 _./:'-]{1,120}$`)

func guestDiskTmpFindings(stderr []byte) []string {
	findings := []string{}
	for line := range strings.SplitSeq(string(stderr), "\n") {
		if finding, ok := strings.CutPrefix(line, guestDiskTmpFindingPrefix); ok && guestDiskTmpFinding.MatchString(finding) && !slices.Contains(findings, finding) {
			findings = append(findings, finding)
		}
	}
	return findings
}

// guestDiskTmpRefusal is a guest's safety verdict, not an unavailable dependency.
type guestDiskTmpRefusal struct {
	yard     string
	exit     int
	findings []string
}

func (refusal *guestDiskTmpRefusal) Error() string {
	message, _ := refusal.ActivationDiagnostic()
	return message
}

func (refusal *guestDiskTmpRefusal) ActivationDiagnostic() (string, string) {
	reason := fmt.Sprintf("guest exit %d without a recognized reason", refusal.exit)
	if count := len(refusal.findings); count > guestDiskTmpFindingLimit {
		reason = fmt.Sprintf("%s; and %d more", strings.Join(refusal.findings[:guestDiskTmpFindingLimit], "; "), count-guestDiskTmpFindingLimit)
	} else if count != 0 {
		reason = strings.Join(refusal.findings, "; ")
	}
	return fmt.Sprintf("yard %s: guest disk /tmp policy refused: %s", refusal.yard, reason),
		"correct the reported guest state, then run yard update --check"
}

func (*guestDiskTmpRefusal) MigrationRefusal() bool { return true }

// DiskTmpRetry names the operation that can apply the pending policy at this power state.
func DiskTmpRetry(yard, power string) string {
	if strings.EqualFold(power, "stopped") {
		return "yard -Y " + yard + " start; then run yard migrate"
	}
	if strings.EqualFold(power, "running") {
		return "yard update --yes"
	}
	return "yard -Y " + yard + " status"
}

// CompleteRegisteredDiskTmp applies the policy and restarts a running yard.
// Its caller owns the upfront plan/confirmation; a stopped yard is never booted.
func CompleteRegisteredDiskTmp(ctx context.Context, runtime reconcileruntime.Runtime, guards ...func() error) error {
	before, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if err != nil {
		return err
	}
	if before.Config["user.subyard.disk_tmp"] == "v1" || strings.EqualFold(before.Status, "stopped") {
		return nil
	}
	if !strings.EqualFold(before.Status, "running") {
		return errors.New("yard is not in a stable power state")
	}
	uuid, power := before.Config["volatile.uuid"], before.Config["user.subyard.desired_power"]
	if uuid == "" {
		return domain.ErrPlanStale
	}
	guard := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, check := range guards {
			if err := check(); err != nil {
				return err
			}
		}
		actual, err := runtime.Incus.Instance(ctx, before.Project, before.Name)
		if err != nil {
			return err
		}
		if actual.Config["volatile.uuid"] != uuid || actual.Config["user.subyard.desired_power"] != power {
			return domain.ErrPlanStale
		}
		return nil
	}
	if err := guard(); err != nil {
		return err
	}
	if err := assessRegisteredDiskTmp(ctx, runtime); err != nil {
		return err
	}
	// Prepare the existing host guard before any stop. Root refusal leaves the
	// running yard untouched; physical start retains its normal fail-closed checks.
	ready, err := runtime.CheckStage(ctx, ports.ReconcileStageNetwork)
	if err != nil {
		return err
	}
	if !ready {
		if err := runtime.ApplyStage(ctx, ports.ReconcileStageNetwork); err != nil {
			return err
		}
	}
	if runtime.SourceReader != nil {
		return errors.New("guest policy requires the verified physical runtime source")
	}
	script, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "scripts/guest-disk-tmp.sh"))
	if err != nil {
		return err
	}
	run := func(call context.Context, action string) error {
		if err := guard(); err != nil {
			return err
		}
		result, err := runtime.Executor.Exec(call, before.Project, before.Name, ports.InstanceExecRequest{Command: []string{"sh", "-s", "--", action}, Stdin: script})
		if err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("guest /tmp %s: exit %d", action, result.ExitCode)
		}
		return nil
	}
	if err := run(ctx, "apply"); err != nil {
		return err
	}
	for _, action := range []string{"stop", "start"} {
		if err := guard(); err != nil {
			return err
		}
		if action == "stop" && runtime.Stderr != nil {
			fmt.Fprintln(runtime.Stderr, DiskTmpDisclosure(runtime.Yard.YardName, true))
		}
		// The caller's plan disclosed this restart and its session loss before consent.
		power := runtime.RunApprovedMigrationStop
		if action == "start" {
			power = func(ctx context.Context) error { return runtime.RunMigrationPhysicalLifecycle(ctx, action) }
		}
		if err := power(ctx); err != nil {
			return fmt.Errorf("%s: %w", action, err)
		}
	}
	wait, cancel := context.WithTimeout(ctx, 150*time.Second)
	err = run(wait, "wait-boot")
	cancel()
	if err != nil {
		return err
	}
	if err := run(ctx, "check"); err != nil {
		return err
	}
	if err := guard(); err != nil {
		return err
	}
	actual, err := runtime.Incus.Instance(ctx, before.Project, before.Name)
	if err != nil {
		return err
	}
	writer, ok := runtime.Incus.(ports.RegisteredInstanceAdopter)
	if !ok {
		return errors.New("registered guest metadata CAS is unavailable")
	}
	return writer.AdoptRegisteredInstance(ctx, actual, map[string]string{"user.subyard.disk_tmp": "v1"})
}
