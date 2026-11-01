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
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/uninstall"
)

const vmCPUWeightKey = "user.subyard.vm_cpu_weight"

// VMCPUConverged trusts local owned-instance metadata, including at host boot.
// Unconfigured instances keep their existing scheduler placement.
func (client *Client) VMCPUConverged(ctx context.Context, project, name string, apply bool) (bool, error) {
	return client.VMCPUConvergedIf(ctx, project, name, apply, "")
}

func (client *Client) checkVMUUID(ctx context.Context, project, name, expectedUUID string) error {
	if expectedUUID == "" {
		return nil
	}
	instance, err := client.Instance(ctx, project, name)
	if err != nil {
		return err
	}
	return checkVMCPUUUID(instance, project, name, expectedUUID)
}
func checkVMCPUUUID(instance ports.InstanceInfo, project, name, expectedUUID string) error {
	if expectedUUID != "" && (instance.Name != name || instance.Project != project || instance.Type != domain.YardVM || instance.Config["volatile.uuid"] != expectedUUID || instance.LocalConfig["volatile.uuid"] != expectedUUID) {
		return domain.ErrPlanStale
	}
	return nil
}

// VMCPUConvergedNative is the one fixed privileged CLI entry. Its expected
// UUID and owner scope arrive through an admitted retained-self command.
func (client *Client) VMCPUConvergedNative(ctx context.Context, project, name, uuid string, uid uint32, home string) (bool, error) {
	if uuid == "" {
		return false, domain.ErrPlanStale
	}
	if err := uninstall.ValidateOwnerLockDescriptor(home, uid, os.Stdin); err != nil {
		return false, err
	}
	ctx = hostruntime.WithVMCPUOwnerStdin(ctx, os.Stdin)
	return client.VMCPUConvergedIf(ctx, project, name, true, uuid)
}

func (client *Client) VMCPUConvergedIf(ctx context.Context, project, name string, apply bool, expectedUUID string) (bool, error) {
	instance, err := client.Instance(ctx, project, name)
	if err != nil {
		return false, err
	}
	if err := checkVMCPUUUID(instance, project, name, expectedUUID); err != nil {
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
	if apply && (os.Geteuid() != 0 || ports.NativeOperationObserverFrom(ctx) != nil) {
		if ready, err := client.VMCPUConvergedIf(ctx, project, name, false, expectedUUID); err != nil || ready {
			return ready, err
		}
		// Re-read Incus and the process inside the privileged helper rather than
		// accepting a caller-supplied PID, UUID or scheduling value.
		socket := client.socket
		if ports.NativeOperationObserverFrom(ctx) != nil {
			// Convey the connected SDK endpoint, never resolve a fresh ambient
			// helper environment after the privileged boundary.
			server, err := client.connect(ctx, true)
			if err != nil {
				return false, err
			}
			info, err := server.GetConnectionInfo()
			if err != nil {
				return false, err
			}
			if info == nil || info.Protocol != "incus" || info.SocketPath == "" {
				return false, errors.New("native VM CPU requires its bound local SDK socket")
			}
			socket = info.SocketPath
		} else if socket == "" {
			socket = os.Getenv("INCUS_SOCKET")
		}
		var diagnostic bytes.Buffer
		var runErr error
		if ports.NativeOperationObserverFrom(ctx) != nil {
			if expectedUUID == "" {
				return false, domain.ErrPlanStale
			}
			runErr = shelladapter.RunNativeVMCPU(ctx, shelladapter.NativeVMCPU{Project: project, Instance: name, Socket: socket, UUID: expectedUUID}, &diagnostic)
		} else {
			if expectedUUID != "" {
				return false, errors.New("native VM CPU helper requires admitted executable custody")
			}
			engine, err := os.Executable()
			if err != nil {
				return false, err
			}
			command := exec.CommandContext(ctx, "sudo", "-n", "--", engine, "_vm-cpu", "apply", project, name, socket)
			command.Stderr = &diagnostic
			runErr = command.Run()
		}
		if err := runErr; err != nil {
			if message := strings.TrimSpace(diagnostic.String()); strings.HasPrefix(message, "VM CPU scheduling: ") {
				return false, fmt.Errorf("%s: %w", message, err)
			}
			return false, fmt.Errorf("apply VM CPU weight requires noninteractive root authorization: %w", err)
		}
		return client.VMCPUConvergedIf(ctx, project, name, false, expectedUUID)
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

	ready, err := hostruntime.VMCPUWeight(ctx, state.Pid, instance.Config["volatile.uuid"], weight, apply)
	if err != nil {
		return false, err
	}
	if err := client.checkVMUUID(ctx, project, name, expectedUUID); err != nil {
		return false, err
	}
	return ready, nil
}

func (client *Client) applyVMCPU(ctx context.Context, project, name string) error {
	return client.applyVMCPUIf(ctx, project, name, "")
}
func (client *Client) applyVMCPUIf(ctx context.Context, project, name, expectedUUID string) error {
	ready, err := client.VMCPUConvergedIf(ctx, project, name, true, expectedUUID)
	if err == nil && !ready {
		return errors.New("VM host CPU weight did not converge")
	}
	return err
}
