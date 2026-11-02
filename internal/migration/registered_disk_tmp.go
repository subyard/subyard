package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type registeredMaintenanceError struct {
	stage string
	cause error
}

func (failure *registeredMaintenanceError) Error() string {
	message, _ := failure.ActivationDiagnostic()
	return message
}
func (failure *registeredMaintenanceError) Unwrap() error { return failure.cause }
func (failure *registeredMaintenanceError) ActivationDiagnostic() (string, string) {
	category := "unclassified"
	var exit *exec.ExitError
	switch {
	case errors.Is(failure.cause, domain.ErrPlanStale):
		category = "stale-input"
	case errors.Is(failure.cause, context.Canceled):
		category = "cancelled"
	case errors.Is(failure.cause, context.DeadlineExceeded):
		category = "deadline"
	case errors.Is(failure.cause, os.ErrPermission):
		category = "permission-denied"
	case errors.Is(failure.cause, os.ErrNotExist):
		category = "command-not-found"
	case errors.As(failure.cause, &exit):
		category = fmt.Sprintf("child-exit-%d", exit.ExitCode())
	}
	return "registered yard maintenance " + failure.stage + " failed (" + category + ")", "run yard update --check"
}
func maintenanceDiagnostic(stage string, cause error) error {
	if cause == nil {
		return nil
	}
	return &registeredMaintenanceError{stage: stage, cause: cause}
}

func registeredDiskTmpReady(ctx context.Context, runtime reconcileruntime.Runtime) (bool, error) {
	return runtime.RegisteredDiskTmpReady(ctx)
}

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
	if result.ExitCode == 10 && errors.As(err, &exit) && exit.Code == 10 && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return errors.Join(fmt.Errorf("assess guest disk /tmp safety: exit %d", result.ExitCode), err)
}

// MigrateRegisteredDiskTmp is one concrete migration leaf. Installing policy
// does not authorize a reboot: each disruptive cycle needs separate late assent.
func migrateRegisteredDiskTmp(ctx context.Context, runtime reconcileruntime.Runtime, confirm func(domain.ConfirmationRequest) (bool, error), inputGuards ...func() error) (result error) {
	before, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if err != nil {
		return err
	}
	uuid, power := before.Config["volatile.uuid"], before.Config["user.subyard.desired_power"]
	if uuid == "" || power != "running" && power != "stopped" {
		return domain.ErrPlanStale
	}
	identityGuard := func(call context.Context) error {
		actual, err := runtime.Incus.Instance(call, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
		if err != nil {
			return err
		}
		if actual.Config["volatile.uuid"] != uuid || actual.Config["user.subyard.desired_power"] != power {
			return domain.ErrPlanStale
		}
		return nil
	}
	guard := func() error {
		for _, check := range inputGuards {
			if err := check(); err != nil {
				return err
			}
		}
		return identityGuard(ctx)
	}
	restoreStopped := func() error {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		defer cancel()
		if err := identityGuard(cleanup); err != nil {
			return err
		}
		return maintenanceDiagnostic("restore-stop", runtime.RunMigrationPhysicalLifecycle(cleanup, "stop"))
	}
	restorePower := false
	defer func() {
		if restorePower {
			result = errors.Join(result, restoreStopped())
		}
	}()
	physical := func(action string) error {
		if err := guard(); err != nil {
			return maintenanceDiagnostic("pre-"+action+"-check", err)
		}
		return maintenanceDiagnostic(action, runtime.RunMigrationPhysicalLifecycle(ctx, action))
	}
	ask := func(summary string) error {
		if confirm == nil {
			return domain.ErrConfirmationRequired
		}
		consequence := "stop and start this yard; interrupt its active processes and connections; reset /tmp on boot"
		if strings.HasPrefix(summary, "restore stopped power") {
			consequence = "stop this yard, interrupt its active processes and connections, and clear /tmp after workloads stop"
		}
		yes, err := confirm(domain.ConfirmationRequest{Summary: "yard " + runtime.Yard.YardName + ": " + summary,
			Default: domain.ConfirmationDefaultNo, Consequences: []string{
				consequence,
				"retain persistent data and instance UUID; restore desired power " + power}})
		if err != nil {
			return err
		}
		if !yes {
			return domain.ErrOperationDeclined
		}
		return maintenanceDiagnostic("post-consent-check", guard())
	}
	if strings.EqualFold(before.Status, "stopped") && power == "stopped" && before.Config["user.subyard.disk_tmp"] == "v1" {
		return nil
	}
	if runtime.SourceReader != nil {
		return errors.New("guest policy migration requires the verified physical runtime source")
	}
	script, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "scripts", "guest-disk-tmp.sh"))
	if err != nil {
		return err
	}
	if strings.EqualFold(before.Status, "stopped") {
		summary := "resume running power to finish disk-backed /tmp migration"
		if power == "stopped" {
			summary = "temporarily boot to finish disk-backed /tmp migration, then restore stopped state"
		}
		if err := ask(summary); err != nil {
			return err
		}
		restorePower = power == "stopped"
		if err := physical("start"); err != nil {
			return err
		}
	} else if !strings.EqualFold(before.Status, "running") {
		return errors.New("registered yard is not in a stable power state")
	}
	run := func(action string) (int, error) {
		if err := guard(); err != nil {
			return 0, err
		}
		result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName,
			ports.InstanceExecRequest{Command: []string{"sh", "-s", "--", action}, Stdin: script})
		if err != nil {
			var exit *ports.GuestExitError
			if !errors.As(err, &exit) || exit.Code != result.ExitCode || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return 0, err
			}
		}
		return result.ExitCode, nil
	}
	if code, err := run("apply"); err != nil || code != 0 {
		return errors.Join(fmt.Errorf("install guest disk /tmp policy: exit %d", code), err)
	}
	restartApproved := false
	code, err := run("check")
	if err != nil {
		return err
	}
	if code == 10 {
		if err := ask("restart to finish disk-backed /tmp migration"); err != nil {
			return err
		}
		restartApproved = true
		if err := physical("stop"); err != nil {
			return err
		}
		restorePower = power == "stopped"
		if err := physical("start"); err != nil {
			return err
		}
		if code, err := run("wait-boot"); err != nil || code != 0 {
			return errors.Join(fmt.Errorf("wait for guest boot: exit %d", code), err)
		}
		code, err = run("check")
	}
	if err != nil || code != 0 {
		return errors.Join(fmt.Errorf("verify guest disk /tmp policy: exit %d", code), err)
	}
	if power == "stopped" && strings.EqualFold(before.Status, "running") {
		if !restartApproved {
			if err := ask("restore stopped power after disk-backed /tmp migration"); err != nil {
				return err
			}
		}
		restorePower = true
	}
	if restorePower {
		restorePower = false
		if err := restoreStopped(); err != nil {
			return err
		}
	}
	if err := guard(); err != nil {
		return err
	}
	actual, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if err != nil {
		return err
	}
	writer, ok := runtime.Incus.(ports.RegisteredInstanceAdopter)
	if !ok {
		return errors.New("registered guest metadata CAS is unavailable")
	}
	return writer.AdoptRegisteredInstance(ctx, actual, map[string]string{"user.subyard.disk_tmp": "v1"})
}
