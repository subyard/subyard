package incusclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/domain"
)

const vmCPUWeightKey = "user.subyard.vm_cpu_weight"

// VMCPUConverged trusts local owned-instance metadata, including at host boot.
// Unconfigured instances keep their existing scheduler placement.
func (client *Client) VMCPUConverged(ctx context.Context, project, name string, apply bool) (bool, error) {
	instance, err := client.Instance(ctx, project, name)
	if err != nil {
		return false, err
	}
	value := instance.Config[vmCPUWeightKey]
	if value == "" {
		return true, nil
	}
	weight, err := strconv.Atoi(value)
	if err != nil || weight < 1 || weight > 10000 || strconv.Itoa(weight) != value {
		return false, errors.New("invalid persisted VM CPU weight")
	}
	if instance.Type != domain.YardVM || instance.LocalConfig[vmCPUWeightKey] != value ||
		instance.LocalConfig["user.subyard.managed"] != "true" ||
		!domain.SafeName(instance.LocalConfig["user.subyard.name"]) {
		return false, errors.New("VM CPU weight requires local owned VM metadata")
	}
	if strings.EqualFold(instance.Status, "stopped") {
		return true, nil
	}
	if !strings.EqualFold(instance.Status, "running") {
		return false, errors.New("cannot inspect VM CPU scheduling from unknown power state")
	}
	if apply && os.Geteuid() != 0 {
		if ready, err := client.VMCPUConverged(ctx, project, name, false); err != nil || ready {
			return ready, err
		}
		socket := client.socket
		if socket == "" {
			socket = os.Getenv("INCUS_SOCKET")
		}
		if err := runVMCPUHelper(ctx, project, name, socket); err != nil {
			return false, err
		}
		return client.VMCPUConverged(ctx, project, name, false)
	}
	server, err := client.connect(ctx, true)
	if err != nil {
		return false, err
	}
	state, _, err := server.UseProject(project).GetInstanceState(name)
	if err != nil {
		return false, normalizeError("inspect VM host process", err)
	}
	if !strings.EqualFold(state.Status, "running") {
		return false, errors.New("VM power changed before host CPU scheduling")
	}
	return hostruntime.VMCPUWeight(ctx, state.Pid, instance.Config["volatile.uuid"], weight, apply)
}

// runVMCPUHelper re-reads Incus and the process inside the privileged helper
// rather than accepting a caller-supplied PID, UUID or scheduling value.
func runVMCPUHelper(ctx context.Context, project, name, socket string) error {
	engine, err := vmCPUHelperPath()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "sudo", "-n", "--", engine, "_vm-cpu", "apply", project, name, socket)
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	err = command.Run()
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(diagnostic.String())
	if strings.HasPrefix(message, "VM CPU scheduling: ") {
		return fmt.Errorf("%s: %w", message, err)
	}
	// The helper reports an unconverged weight only through its exit status;
	// sudo always explains its own refusals, so do not guess at their cause.
	if message == "" {
		return fmt.Errorf("VM host CPU weight did not converge: %w", err)
	}
	if len(message) > 2048 {
		message = message[:2048]
	}
	return fmt.Errorf("apply VM CPU weight as root: %s: %w", message, err)
}

// vmCPUHelperPath names an image sudo can resolve. A release transition runs a
// sealed memfd, whose reported path does not exist; this waiting process keeps
// the same verified image reachable through its process-qualified path.
func vmCPUHelperPath() (string, error) {
	engine, err := os.Executable()
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(engine, "/memfd:") {
		return fmt.Sprintf("/proc/%d/exe", os.Getpid()), nil
	}
	return engine, nil
}

func (client *Client) applyVMCPU(ctx context.Context, project, name string) error {
	ready, err := client.VMCPUConverged(ctx, project, name, true)
	if err == nil && !ready {
		return errors.New("VM host CPU weight did not converge")
	}
	return err
}
