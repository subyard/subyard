package reconcileruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/ports"
)

type diskTmpRebootError struct{ yard string }

func (failure diskTmpRebootError) Error() string {
	message, retry := failure.ActivationDiagnostic()
	return message + "; " + retry
}

func (failure diskTmpRebootError) ActivationDiagnostic() (string, string) {
	return fmt.Sprintf("yard %s: disk-backed /tmp policy is installed; the active /tmp is memory-backed; reboot required", failure.yard),
		fmt.Sprintf("close active yard sessions, then run yard -Y %s stop and yard -Y %s start", failure.yard, failure.yard)
}

func (runtime Runtime) diskTmpReadiness(ctx context.Context, diagnostic bool) (bool, error) {
	state, err := runtime.reconcileState(ctx)
	if err != nil {
		return false, err
	}
	if !state.InstanceFound {
		return false, nil
	}
	return runtime.diskTmpInstanceReadiness(ctx, state.Instance, diagnostic)
}

func (runtime Runtime) diskTmpInstanceReadiness(ctx context.Context, instance ports.InstanceInfo, diagnostic bool) (bool, error) {
	if strings.EqualFold(instance.Status, "stopped") {
		policy, _ := instance.EffectiveConfig("user.subyard.disk_tmp")
		return instanceIntentionallyStopped(instance) && policy == "v1", nil
	}
	if !strings.EqualFold(instance.Status, "running") {
		return false, nil
	}
	pending, err := runtime.diskTmpPending(ctx)
	if err != nil || pending == nil {
		return err == nil, err
	}
	var reboot diskTmpRebootError
	if diagnostic && errors.As(pending, &reboot) {
		return false, reboot
	}
	return false, nil
}

// diskTmpPending runs the guest check of a running yard. Unmet conditions are
// returned as pending: the memory-backed reboot state or the guest's findings.
func (runtime Runtime) diskTmpPending(ctx context.Context) (pending error, err error) {
	if runtime.Executor == nil {
		return nil, errors.New("Incus executor is required")
	}
	script, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "scripts", "guest-disk-tmp.sh"))
	if err != nil {
		return nil, err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName,
		ports.InstanceExecRequest{Command: []string{"sh", "-s", "--", "check"}, Stdin: script})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	if err != nil && result.ExitCode == 0 {
		return nil, err
	}
	switch result.ExitCode {
	case 0:
		return nil, nil
	case 10:
		return diskTmpRebootError{yard: runtime.Yard.YardName}, nil
	}
	return diskTmpCheckFindings{exit: result.ExitCode, findings: GuestDiskTmpFindings(result.Stderr)}, nil
}

// diskTmpCheckFindings is the guest check's report of unmet /tmp conditions.
type diskTmpCheckFindings struct {
	exit     int
	findings []string
}

func (pending diskTmpCheckFindings) Error() string {
	return "guest /tmp: " + guestDiskTmpReason(pending.exit, pending.findings)
}

// The guest is outside the host's trust boundary: only this leaf's own prefixed
// findings in a closed alphabet reach the operator, and only a bounded number.
const (
	guestDiskTmpFindingPrefix = "guest-disk-tmp: "
	guestDiskTmpFindingLimit  = 8
)

var guestDiskTmpFinding = regexp.MustCompile(`^[A-Za-z0-9 _./:'-]{1,120}$`)

// GuestDiskTmpFindings returns the guest leaf's distinct recognized findings.
func GuestDiskTmpFindings(stderr []byte) []string {
	findings := []string{}
	for line := range strings.SplitSeq(string(stderr), "\n") {
		if finding, ok := strings.CutPrefix(line, guestDiskTmpFindingPrefix); ok && guestDiskTmpFinding.MatchString(finding) && !slices.Contains(findings, finding) {
			findings = append(findings, finding)
		}
	}
	return findings
}

// GuestDiskTmpReason joins bounded findings, or names the exit without one.
func GuestDiskTmpReason(exit int, findings []string) string {
	return guestDiskTmpReason(exit, findings)
}

func guestDiskTmpReason(exit int, findings []string) string {
	if count := len(findings); count > guestDiskTmpFindingLimit {
		return fmt.Sprintf("%s; and %d more", strings.Join(findings[:guestDiskTmpFindingLimit], "; "), count-guestDiskTmpFindingLimit)
	} else if count != 0 {
		return strings.Join(findings, "; ")
	}
	return fmt.Sprintf("guest exit %d without a recognized reason", exit)
}
