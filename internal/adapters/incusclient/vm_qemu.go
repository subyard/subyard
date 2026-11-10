package incusclient

import (
	"context"
	"errors"
	"maps"
	"os"
	"strconv"
	"strings"

	incus "github.com/lxc/incus/v6/client"

	"github.com/Subyard/Subyard/internal/domain"
)

const vmLowLevelProjectKey = "restricted.virtual-machines.lowlevel"

// PrepareVMStart writes the owner start value of raw.qemu.conf on a stopped VM
// just before a product start. Incus applies raw.qemu.conf only when QEMU
// starts and refuses to change it on a running VM, so an existing yard becomes
// headless at its next product start instead of being restarted for it.
func (client *Client) PrepareVMStart(ctx context.Context, project, name string) error {
	server, err := client.reconcileServer(ctx)
	if err != nil {
		return err
	}
	return client.prepareVMStart(ctx, server, project, name)
}

// prepareVMStart changes nothing for a container, a VM that is not stopped,
// or a value that is already current. A foreign raw QEMU override is left
// as found: init refuses it. A restricted project needs the low-level VM grant
// for the owner override; the start adds only that key, and only to a project
// this installation provably owns for this yard. Any other restricted project
// is left as found. Both writes use the ETag of the read that checked them, so
// a concurrent edit or start is never overwritten.
func (client *Client) prepareVMStart(ctx context.Context, server incus.InstanceServer, project, name string) error {
	native := server.UseProject(project)
	actual, etag, err := native.GetInstance(name)
	if err != nil {
		return normalizeError("read VM before start", err)
	}
	if actual == nil || instanceInfo(project, actual).Type != domain.YardVM || !strings.EqualFold(actual.Status, "stopped") {
		return nil
	}
	if len(domain.VMRawQEMURefusals(actual.ExpandedConfig, actual.Config)) != 0 {
		return nil
	}
	current := actual.Config["raw.qemu.conf"]
	target, _ := domain.VMRawQEMUTarget(current, false)
	if target == current || client.PreserveReportingVMConfig && target == domain.VMRawQEMUConfig(true) {
		return nil
	}
	policy, projectETag, err := server.GetProject(project)
	if err != nil {
		return normalizeError("read VM project policy before start", err)
	}
	if policy == nil {
		return nil
	}
	if policy.Config["restricted"] == "true" && policy.Config[vmLowLevelProjectKey] != "allow" {
		if !ownedVMProject(project, policy.Config, name, actual.Config) {
			return nil
		}
		if projectETag == "" {
			return errors.New("VM start policy requires a project ETag")
		}
		request := policy.Writable()
		request.Config = maps.Clone(policy.Config)
		request.Config[vmLowLevelProjectKey] = "allow"
		if err := server.UpdateProject(project, request, projectETag); err != nil {
			return normalizeError("grant the VM project low-level policy before start", err)
		}
	}
	if etag == "" {
		return errors.New("VM start policy requires an instance ETag")
	}
	request := actual.Writable()
	request.Config = maps.Clone(actual.Config)
	request.Config["raw.qemu.conf"] = target
	if err := beforeNative(ctx, native, "instance-config", project, name); err != nil {
		return err
	}
	operation, err := native.UpdateInstance(name, request, etag)
	if err != nil {
		return normalizeError("write VM QEMU device policy before start", err)
	}
	return waitNative(ctx, native, operation, "instance-config", project, name)
}

// ownedVMProject proves that a dedicated project carries this installation's
// complete ownership for the managed yard instance being started. The host
// boot reconciler runs as root on behalf of the recorded operator.
func ownedVMProject(project string, config map[string]string, instance string, instanceConfig map[string]string) bool {
	if !domain.SafeName(project) || project == "default" || instanceConfig["user.subyard.managed"] != "true" ||
		config["user.subyard.instance_name"] != instance || config["user.subyard.data_home"] == "" || config["user.subyard.config_home"] == "" {
		return false
	}
	_, adoptedUUID := config["user.subyard.adopted_uuid"]
	switch config["user.subyard.installation"] {
	case "1":
		if adoptedUUID {
			return false
		}
	case domain.AdoptedInstallation:
		if config["user.subyard.adopted_uuid"] == "" {
			return false
		}
	default:
		return false
	}
	uid := os.Getuid()
	return uid == 0 || config["user.subyard.operator_uid"] == strconv.Itoa(uid)
}
