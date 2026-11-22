package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const preparedFor = "user.subyard.prepared-for"

// One never-booted root per slot lives outside the allocation project. It
// contains the immutable base, with no lease identity, cloud data or SSH keys.
func (runtime *Runtime) preparedRuntime() *Runtime {
	ready := *runtime
	ready.Config.Project += "-prepared"
	// A stopped NIC still reserves its instance name in Incus bridge DNS.
	ready.Config.Network = ""
	ready.Config = ready.Config.withEnvironment(EnvironmentSpec{Name: EnvironmentContainer, Count: 1, CPU: runtime.Config.CPU})
	ready.Config.AgentPublicKey = ""
	ready.allocation = nil
	ready.agentLogOwner = ""
	return &ready
}

// Return nil when the project is absent, or an empty fingerprint when only
// the project exists. Never infer absence from an unsuccessful native read.
func (runtime *Runtime) preparedContainer(ctx context.Context) (*Runtime, string, error) {
	ready := runtime.preparedRuntime()
	exists, err := ready.projectPresence(ctx)
	if err != nil || !exists {
		return nil, "", err
	}
	if err := ready.requireProjectMarker(ctx); err != nil {
		return nil, "", err
	}
	marker, err := ready.incus(ctx, "project", "get", ready.Config.Project, preparedFor)
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(marker) != runtime.Config.Project {
		return nil, "", errors.New("foreign prepared container project")
	}
	if err := ready.requireAllocationIdle(ctx); err != nil {
		return nil, "", err
	}
	names, err := ready.projectInstances(ctx)
	if err != nil || len(names) == 0 {
		return ready, "", err
	}
	vm := ready.Config.vm(1)
	if len(names) != 1 || names[0] != vm {
		return nil, "", errors.New("unexpected instances in prepared container project")
	}
	body, err := ready.incus(ctx, "query", "/1.0/instances/"+url.PathEscape(vm)+"?project="+url.QueryEscape(ready.Config.Project))
	if err != nil {
		return nil, "", err
	}
	var instance struct {
		Type       string            `json:"type"`
		Status     string            `json:"status"`
		LastUsedAt *time.Time        `json:"last_used_at"`
		Config     map[string]string `json:"config"`
	}
	if err := json.Unmarshal([]byte(body), &instance); err != nil {
		return nil, "", err
	}
	cfg := instance.Config
	if instance.Type != "container" || instance.Status != "Stopped" ||
		(instance.LastUsedAt == nil || (!instance.LastUsedAt.IsZero() && !instance.LastUsedAt.Equal(time.Unix(0, 0)))) ||
		cfg["boot.autostart"] != "false" || cfg["user.subyard.managed"] != managedMarker ||
		cfg[preparedFor] != runtime.Config.Project || !imageFingerprint.MatchString(cfg["volatile.base_image"]) ||
		cfg["user.subyard.generation"] != "" || cfg["user.subyard.lease-epoch"] != "" || cfg["cloud-init.user-data"] != "" {
		return nil, "", errors.New("prepared container ownership or pristine state mismatch")
	}
	return ready, cfg["volatile.base_image"], nil
}

func (runtime *Runtime) initPreparedGuest(ctx context.Context, vm string, settings []string) error {
	fingerprint := runtime.Config.Image
	if !imageFingerprint.MatchString(fingerprint) {
		return errors.New("no pinned base for prepared container")
	}
	ready, current, err := runtime.preparedContainer(ctx)
	if err != nil {
		return err
	}
	if ready == nil {
		ready = runtime.preparedRuntime()
		if _, err := ready.incus(ctx, "project", "create", ready.Config.Project,
			"-c", "features.images=false", "-c", "restricted=true", "-c", "user.subyard.managed="+managedMarker,
			"-c", preparedFor+"="+runtime.Config.Project); err != nil {
			return err
		}
	}
	template := ready.Config.vm(1)
	if current != fingerprint {
		if current != "" {
			if _, err := ready.incus(ctx, "delete", template, "--project", ready.Config.Project); err != nil {
				return err
			}
		}
		if err := ready.ensureProject(ctx); err != nil {
			return err
		}
		ready.Config.Image = fingerprint
		if err := ready.initFromImage(ctx, template, []string{preparedFor + "=" + runtime.Config.Project}); err != nil {
			return err
		}
	}
	// Apply the allocation identity on native creation. A client interrupted
	// mid-copy leaves either no target or one the guarded drain recognizes.
	args := []string{"copy", template, vm, "--project", ready.Config.Project, "--target-project", runtime.Config.Project,
		"--instance-only", "-c", preparedFor + "="}
	for _, setting := range append(runtime.Config.instanceSettings(), settings...) {
		args = append(args, "-c", setting)
	}
	_, err = ready.incus(ctx, args...)
	return err
}

func (runtime *Runtime) retirePreparedContainer(ctx context.Context) error {
	ready, fingerprint, err := runtime.preparedContainer(ctx)
	if err != nil || ready == nil {
		return err
	}
	if fingerprint != "" {
		if _, err := ready.incus(ctx, "delete", ready.Config.vm(1), "--project", ready.Config.Project); err != nil {
			return err
		}
	}
	if _, err := ready.incus(ctx, "project", "delete", ready.Config.Project); err != nil {
		return fmt.Errorf("delete retired prepared container project: %w", err)
	}
	return nil
}
