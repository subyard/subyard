package amnezia_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

const (
	adoptionPublicIPv4 = "203.0.113.10"
	adoptionGuestIPv4  = "10.20.30.40"
)

type adoptionNetwork struct{}

func (adoptionNetwork) Check(context.Context, yardnetwork.Yard) error { return nil }
func (adoptionNetwork) Ensure(context.Context, yardnetwork.Yard) error {
	panic("assessment wrote network policy")
}
func (adoptionNetwork) WithStart(_ context.Context, _ yardnetwork.Yard, start func() error) error {
	return start()
}
func (adoptionNetwork) Prepare(context.Context, []yardnetwork.Yard, yardnetwork.Change) (yardnetwork.Plan, error) {
	return yardnetwork.Plan{}, nil
}

type adoptionNative struct {
	*testkit.Incus
	projectWrites, receiptWrites int
}

func (native *adoptionNative) AdoptProject(_ context.Context, _ string, before, metadata map[string]string) error {
	if !maps.Equal(before, native.Reconcile.ProjectConfig) {
		return domain.ErrPlanStale
	}
	maps.Copy(native.Reconcile.ProjectConfig, metadata)
	native.projectWrites++
	return nil
}

func (native *adoptionNative) AdoptRegisteredInstance(_ context.Context, before ports.InstanceInfo, values map[string]string) error {
	current := native.Reconcile.Instance
	if !maps.Equal(current.LocalConfig, before.LocalConfig) {
		return domain.ErrPlanStale
	}
	for key, value := range values {
		device := current.LocalDevices[strings.TrimPrefix(key, "user.subyard.resource.")]
		if device["type"] != "proxy" || value != (resource.ProxyContract{}).OwnershipValue(device) || current.LocalConfig[key] != "" && current.LocalConfig[key] != value {
			return domain.ErrPlanStale
		}
		if current.LocalConfig[key] != value {
			current.LocalConfig[key], current.Config[key] = value, value
			native.receiptWrites++
		}
	}
	return nil
}

func (native *adoptionNative) Exec(context.Context, string, string, ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	return ports.InstanceExecResult{}, nil
}

// A VM yard as an earlier release left it: a pinned profile NIC and a block /srv
// disk, with the shipped declarations and preset deciding what is accepted.
func amneziaAdoptionFixture(t *testing.T, registrationSettings func(preset string) string) (*migration.RegisteredYardMigration, *migration.RegisteredYardMigration, *adoptionNative, string) {
	t.Helper()
	root := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	preset, err := os.ReadFile(filepath.Join(root, "config/profiles/amnezia/yard.env"))
	if err != nil {
		t.Fatal(err)
	}
	home := testkit.TempDir(t)
	configHome := filepath.Join(home, "config")
	for _, directory := range []string{"overrides/host/agents", "overrides/shared/agents", "yards/default/overrides/agents", ".sync/settings/overrides/host/agents"} {
		if err := os.MkdirAll(filepath.Join(configHome, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	registration := filepath.Join(configHome, "yards", "vpn", "config.env")
	writeFixture(t, registration, registrationSettings(string(preset)))
	options := config.LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: "vpn", DisablePrivate: true, Environment: map[string]string{"SUBYARD_CONFIG_HOME": configHome}}
	loaded, err := config.Load(options)
	if err != nil {
		t.Fatal(err)
	}
	yard := loaded.Context
	if yard.YardKind != domain.YardVM || loaded.Environment["VM_PIN_IPV4"] != "1" || loaded.Environment["SRV_VOLUME_TYPE"] != "block" {
		t.Fatalf("shipped preset is not the pinned block VM this fixture describes: %+v", yard)
	}
	values := map[string]string{
		"volatile.uuid": "fixture-uuid", "volatile.uuid.generation": "fixture-uuid", "volatile.vsock_id": "1234", "volatile.vm.definition": "pc-q35",
		"volatile.vm.rtc_adjustment": "0", "volatile.vm.rtc_offset": "0", "volatile.cloud-init.instance-id": "fixture-instance", "volatile.base_image": strings.Repeat("a", 64),
		"volatile.apply_template": "start", "volatile.last_state.power": "RUNNING", "volatile.last_state.ready": "false", "volatile.eth0.name": "eth0", "volatile.eth0.hwaddr": "00:16:3e:00:00:01", "volatile.eth0.host_name": "tapfixture", "volatile.root.io.bus": "virtio-scsi", "volatile.srv.io.bus": "virtio-scsi",
		"image.os": "fixture", "boot.autostart": "false", "limits.cpu": "2", "limits.memory": "2GiB", "raw.qemu.conf": "[device \"qemu_balloon\"]\nfree-page-reporting = \"on\"",
		"user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.name": "vpn", "user.subyard.bridge": yard.IncusBridge, "user.subyard.desired_power": "running", "user.subyard.disk_tmp": "v1", "user.subyard.vm_cpu_weight": "1000",
		previewroute.Key: "fixture-route", "user.subyard.startup.amnezia-vpn": "enabled",
	}
	pool, volume := loaded.Environment["SRV_POOL"], loaded.Environment["SRV_VOLUME"]
	if pool == "" {
		pool = "default"
	}
	profile := map[string]map[string]string{
		"eth0": {"type": "nic", "network": yard.IncusBridge, "ipv4.address": adoptionGuestIPv4},
		"root": {"type": "disk", "path": "/", "pool": pool, "size": "10GiB"},
	}
	local := map[string]map[string]string{
		"amnezia-admin":   {"type": "proxy", "bind": "host", "nat": "true", "listen": "tcp:" + adoptionPublicIPv4 + ":2226", "connect": "tcp:" + adoptionGuestIPv4 + ":22"},
		"amnezia-vpn":     {"type": "proxy", "bind": "host", "nat": "true", "listen": "udp:" + adoptionPublicIPv4 + ":51820", "connect": "udp:" + adoptionGuestIPv4 + ":51820"},
		"srv":             {"type": "disk", "pool": pool, "source": volume, "io.bus": "virtio-scsi"},
		"subyard-preview": {"type": "proxy", "bind": "host", "nat": "true", "listen": "tcp:100.64.0.10:32225", "connect": "tcp:" + adoptionGuestIPv4 + ":8765"},
	}
	registry, err := resource.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, contract := range registry.ProxyContracts([]string{"amnezia"}) {
		values[contract.OwnershipKey()] = contract.OwnershipValue(local[contract.Device])
	}
	devices := map[string]map[string]string{}
	for _, group := range []map[string]map[string]string{profile, local} {
		for name, device := range group {
			devices[name] = maps.Clone(device)
		}
	}
	instance := ports.InstanceInfo{Project: yard.IncusProject, Name: yard.YardInstanceName, Type: domain.YardVM, Status: "Running", Config: values, LocalConfig: maps.Clone(values), Devices: devices, LocalDevices: local}
	native := &adoptionNative{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{yard.IncusProject + "/" + yard.YardInstanceName: instance}, Reconcile: ports.ReconcileState{
		ProjectFound: true, ProfileFound: true, InstanceFound: true, VolumeFound: true, Instance: instance, ProfileDevices: profile,
		ProjectConfig: map[string]string{"features.profiles": "true", "features.storage.volumes": "true", "features.storage.buckets": "true", "features.images": "true", "restricted": "true", "restricted.containers.nesting": "allow", "restricted.containers.privilege": "unprivileged", "restricted.containers.interception": "block", "restricted.devices.disk": "allow", "restricted.devices.unix-char": "allow", "restricted.devices.proxy": "allow", "restricted.virtual-machines.lowlevel": "allow"},
	}}}
	binding := strings.Repeat("1", 64)
	native.TeardownResources = []ports.TeardownResource{{Kind: "project", Name: yard.IncusProject, Binding: binding}, {Kind: "profile", Name: "default", Binding: binding}, {Kind: "instance", Name: yard.YardInstanceName, Binding: binding}, {Kind: "volume", Name: volume, Pool: pool, Binding: binding}, {Kind: "image", Name: values["volatile.base_image"], Binding: binding}}
	environment := []string{"SUBYARD_HOME=" + yard.Paths.DataHome, "SUBYARD_CONFIG_HOME=" + yard.Paths.ConfigHome}
	for name, value := range loaded.Environment {
		environment = append(environment, name+"="+value)
	}
	runtime := reconcileruntime.Runtime{Incus: native, Executor: native, Yard: yard, RepositoryRoot: root, Environment: environment, NetworkPolicy: adoptionNetwork{}, SRVPool: pool, SRVVolume: volume}
	load := func(context.Context, releasetransition.V2SettingsSnapshotView) ([]migration.RegisteredYardTarget, error) {
		current, err := config.Load(options)
		return []migration.RegisteredYardTarget{{Loaded: current, Runtime: runtime}}, err
	}
	owner := &migration.RegisteredYardMigration{Yard: "vpn", ConfigHome: configHome, Resources: registry, LoadTargets: load}
	guestTmp := &migration.RegisteredYardMigration{Yard: "vpn", ConfigHome: configHome, Resources: registry, GuestTmp: true, LoadTargets: load}
	return owner, guestTmp, native, registration
}

func amneziaOperatorEndpoint(preset string) string {
	return strings.NewReplacer("RESOURCE_VPN_IPV4=\n", "RESOURCE_VPN_IPV4="+adoptionPublicIPv4+"\n", "RESOURCE_VPN_INTERFACE=\n", "RESOURCE_VPN_INTERFACE=eth0\n").Replace(preset)
}

func observeAdoption(t *testing.T, owner *migration.RegisteredYardMigration) releasetransition.V2ActivationObservation {
	t.Helper()
	observed, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	return observed
}

func TestAmneziaYardAdoptsAsFoundThroughRelease(t *testing.T) {
	stop := func(native *adoptionNative, _ *migration.RegisteredYardMigration) {
		instance := native.Reconcile.Instance
		marker := strings.Replace((resource.ProxyContract{}).OwnershipValue(instance.LocalDevices["amnezia-vpn"]), "v1:", "v1:pending:", 1)
		delete(instance.LocalDevices, "amnezia-vpn")
		delete(instance.Devices, "amnezia-vpn")
		instance.LocalConfig["user.subyard.resource.amnezia-vpn"], instance.Config["user.subyard.resource.amnezia-vpn"] = marker, marker
		instance.LocalConfig["user.subyard.startup.amnezia-vpn"], instance.Config["user.subyard.startup.amnezia-vpn"] = "disabled", "disabled"
	}
	for name, test := range map[string]struct {
		mutate  func(*adoptionNative, *migration.RegisteredYardMigration)
		publish int
	}{
		"running-with-receipts": {},
		"resource-stopped":      {mutate: stop},
		"no-receipts-published": {mutate: func(native *adoptionNative, _ *migration.RegisteredYardMigration) {
			for _, values := range []map[string]string{native.Reconcile.Instance.Config, native.Reconcile.Instance.LocalConfig} {
				delete(values, "user.subyard.resource.amnezia-vpn")
				delete(values, "user.subyard.resource.amnezia-admin")
				delete(values, "user.subyard.startup.amnezia-vpn")
			}
		}, publish: 2},
	} {
		t.Run(name, func(t *testing.T) {
			owner, guestTmp, native, registration := amneziaAdoptionFixture(t, amneziaOperatorEndpoint)
			if test.mutate != nil {
				test.mutate(native, owner)
			}
			settings, err := os.ReadFile(registration)
			if err != nil {
				t.Fatal(err)
			}
			devices, err := json.Marshal([]any{native.Reconcile.Instance.LocalDevices, native.Reconcile.Instance.Devices, native.Reconcile.ProfileDevices})
			if err != nil {
				t.Fatal(err)
			}
			initial := observeAdoption(t, owner)
			if initial.Converged {
				t.Fatal("unregistered project reported as converged")
			}
			prepared, _, err := owner.ReadPreparedInput(registration)
			if err != nil || len(prepared.Content) != 0 && string(prepared.Content) != string(settings) {
				t.Fatalf("operator endpoint settings were rewritten: %q %v", prepared.Content, err)
			}
			observeAdoption(t, guestTmp)
			if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
				t.Fatal(err)
			}
			final := observeAdoption(t, owner)
			if !final.Converged || final.Desired != initial.Desired || len(final.Consequences) != 0 {
				t.Fatalf("final: %#v", final)
			}
			if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
				t.Fatal("repeat:", err)
			}
			after, _ := json.Marshal([]any{native.Reconcile.Instance.LocalDevices, native.Reconcile.Instance.Devices, native.Reconcile.ProfileDevices})
			written, _ := os.ReadFile(registration)
			if string(after) != string(devices) || string(written) != string(settings) || native.receiptWrites != test.publish {
				t.Fatalf("metadata-only adoption changed state: devices=%t settings=%t receipts=%d", string(after) != string(devices), string(written) != string(settings), native.receiptWrites)
			}
			if native.Reconcile.ProjectConfig["user.subyard.installation"] != domain.AdoptedInstallation {
				t.Fatal("project ownership was not published")
			}
		})
	}
}

func amneziaWithoutEndpoint(preset string) string {
	var kept []string
	for _, line := range strings.Split(preset, "\n") {
		if !strings.HasPrefix(line, "RESOURCE_VPN_") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// Preset values are the yard's own settings, not shipped defaults, so a working route
// that differs from them is left for ordinary init to report.
func TestAmneziaAdoptionLeavesEndpointSettingsUnwritten(t *testing.T) {
	for name, registration := range map[string]func(string) string{
		"preset-copy-with-empty-address": func(preset string) string { return preset },
		"template-supplies-endpoint":     amneziaWithoutEndpoint,
		"differing-ports": func(preset string) string {
			return strings.NewReplacer("RESOURCE_VPN_PORT=51820\n", "RESOURCE_VPN_PORT=51821\n", "RESOURCE_VPN_ADMIN_PORT=2226\n", "RESOURCE_VPN_ADMIN_PORT=2227\n").Replace(amneziaOperatorEndpoint(preset))
		},
	} {
		t.Run(name, func(t *testing.T) {
			owner, _, _, path := amneziaAdoptionFixture(t, registration)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			observeAdoption(t, owner)
			prepared, present, err := owner.ReadPreparedInput(path)
			if err != nil || !present || string(prepared.Content) != string(before) {
				t.Fatalf("endpoint settings were rewritten: present=%t %v\n%s", present, err, prepared.Content)
			}
		})
	}
}
