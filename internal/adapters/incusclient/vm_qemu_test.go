package incusclient

import (
	"context"
	"maps"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

const reportingOnlyQEMUConfig = "[device \"qemu_balloon\"]\nfree-page-reporting = \"on\""

// ownedVMProjectConfig is a restricted project of this installation for the
// yard instance, before any VM low-level grant.
func ownedVMProjectConfig(changes map[string]string) map[string]string {
	config := map[string]string{
		"restricted": "true", "restricted.containers.nesting": "allow",
		"user.subyard.installation": "1", "user.subyard.operator_uid": strconv.Itoa(os.Getuid()),
		"user.subyard.data_home": "/data", "user.subyard.config_home": "/config", "user.subyard.instance_name": "yard",
	}
	for key, value := range changes {
		if value == "" {
			delete(config, key)
		} else {
			config[key] = value
		}
	}
	return config
}

func startPolicyServer(t *testing.T, kind, status string, project, local, expanded map[string]string) (*testkit.IncusServer, *Client) {
	t.Helper()
	server, err := testkit.NewIncusServer(testkit.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	server.SetProject("subyard", map[string]any{"name": "subyard", "config": project})
	server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "type": kind, "status": status, "config": local, "expanded_config": expanded})
	return server, New(server.SocketPath)
}

func startThrough(client *Client, path string) error {
	switch path {
	case "product":
		return client.PrepareVMStart(context.Background(), "subyard", "yard")
	case "boot":
		client.PreserveReportingVMConfig = true
		return client.SetInstancePower(context.Background(), "subyard", "yard", "start", false)
	default:
		return client.NetworkPower(context.Background(), yardnetwork.Yard{Project: "subyard", Instance: "yard"}, "start")
	}
}

// Normal product starts write the headless owner value first; boot preserves
// reporting-only values so rollback can resume. A project this installation owns
// receives only the low-level grant that value needs. Nothing else is written.
func TestProductStartWritesTheHeadlessOwnerValueOnlyOnAStoppedVM(t *testing.T) {
	grant := map[string]string{"restricted": "true", "restricted.virtual-machines.lowlevel": "allow"}
	otherOperator := "4294967294"
	for _, test := range []struct {
		name, kind, status, raw, inherited, want string
		project                                  map[string]string
		managed                                  string
		write, granted, wiring                   bool
	}{
		{name: "VM without an override", raw: "", want: domain.VMRawQEMUConfig(false), write: true, wiring: true},
		{name: "reporting VM that kept its display", raw: reportingOnlyQEMUConfig, want: domain.VMRawQEMUConfig(true), write: true, wiring: true},
		{name: "headless VM", raw: domain.VMRawQEMUConfig(false), want: domain.VMRawQEMUConfig(false)},
		{name: "headless reporting VM", raw: domain.VMRawQEMUConfig(true), want: domain.VMRawQEMUConfig(true)},
		{name: "running VM", status: "Running", raw: "", want: ""},
		{name: "container", kind: "container", raw: "", want: ""},
		{name: "foreign override is left as found", raw: "[device \"qemu_gpu\"]\ndriver = \"virtio-vga\"", want: "[device \"qemu_gpu\"]\ndriver = \"virtio-vga\"", wiring: true},
		{name: "inherited override is left as found", inherited: domain.VMRawQEMUConfig(false), want: ""},
		{name: "unrestricted project needs no grant", project: map[string]string{}, want: domain.VMRawQEMUConfig(false), write: true},
		{name: "owned project receives its low-level grant", project: ownedVMProjectConfig(nil), want: domain.VMRawQEMUConfig(false), write: true, granted: true},
		{name: "adopted project receives its low-level grant",
			project: ownedVMProjectConfig(map[string]string{"user.subyard.installation": domain.AdoptedInstallation, "user.subyard.adopted_uuid": "uuid"}),
			want:    domain.VMRawQEMUConfig(false), write: true, granted: true},
		{name: "project of another yard is left as found", project: ownedVMProjectConfig(map[string]string{"user.subyard.instance_name": "other"})},
		{name: "project without ownership is left as found", project: ownedVMProjectConfig(map[string]string{"user.subyard.installation": ""})},
		{name: "project with conflicting ownership is left as found", project: ownedVMProjectConfig(map[string]string{"user.subyard.adopted_uuid": "uuid"})},
		{name: "project of another operator is left as found", project: ownedVMProjectConfig(map[string]string{"user.subyard.operator_uid": otherOperator})},
		{name: "unmanaged instance does not grant its project", project: ownedVMProjectConfig(nil), managed: "false"},
	} {
		paths := []string{"product"}
		if test.wiring {
			paths = append(paths, "boot", "network")
		}
		for _, path := range paths {
			t.Run(test.name+"/"+path, func(t *testing.T) {
				write, want := test.write, test.want
				if path == "boot" && test.raw == reportingOnlyQEMUConfig {
					write, want = false, test.raw
				}
				if os.Getuid() == 0 && test.project["user.subyard.operator_uid"] == otherOperator {
					t.Skip("root starts on behalf of the recorded operator")
				}
				kind, status, project, managed := test.kind, test.status, test.project, test.managed
				if kind == "" {
					kind = "virtual-machine"
				}
				if status == "" {
					status = "Stopped"
				}
				if project == nil {
					project = grant
				}
				if managed == "" {
					managed = "true"
				}
				local, expanded := map[string]string{"user.subyard.managed": managed}, map[string]string{"user.subyard.managed": managed}
				if test.raw != "" {
					local["raw.qemu.conf"], expanded["raw.qemu.conf"] = test.raw, test.raw
				}
				if test.inherited != "" {
					expanded["raw.qemu.conf"] = test.inherited
				}
				server, client := startPolicyServer(t, kind, status, project, local, expanded)
				if err := startThrough(client, path); err != nil {
					t.Fatal(err)
				}
				updates := server.ConfigUpdates()
				if write != (len(updates) == 1) || len(updates) > 1 {
					t.Fatalf("writes = %+v, want write %v", updates, write)
				}
				if write && (updates[0].Config["raw.qemu.conf"] != want || updates[0].Config["user.subyard.managed"] != "true" || len(updates[0].Config) != 2) {
					t.Fatalf("start wrote %+v, want only raw.qemu.conf %q", updates[0].Config, want)
				}
				instance, err := client.Instance(context.Background(), "subyard", "yard")
				if err != nil || instance.LocalConfig["raw.qemu.conf"] != want {
					t.Fatalf("start value = %q, want %q: %v", instance.LocalConfig["raw.qemu.conf"], want, err)
				}
				grants := server.ProjectUpdates()
				if test.granted {
					want := maps.Clone(project)
					want["restricted.virtual-machines.lowlevel"] = "allow"
					if len(grants) != 1 || !maps.Equal(grants[0].Config, want) {
						t.Fatalf("project grant = %+v, want only the low-level key added to %v", grants, project)
					}
				} else if len(grants) != 0 {
					t.Fatalf("start changed project policy: %+v", grants)
				}
				if path != "product" {
					calls := server.PowerCalls()
					if len(calls) != 1 || calls[0].Action != "start" {
						t.Fatalf("start was not requested once after the policy: %+v", calls)
					}
				}
			})
		}
	}
}

// A concurrent edit between the policy read and its write, or a refused write,
// fails the start: Incus never starts a VM whose policy was not applied.
func TestProductStartRefusesToStartAfterAFailedPolicyWrite(t *testing.T) {
	for _, test := range []struct {
		name, refusal string
		hook          func(map[string]any) (int, string)
	}{
		{name: "concurrent edit", refusal: "ETag", hook: func(instance map[string]any) (int, string) {
			instance["description"] = "edited"
			return 0, ""
		}},
		{name: "refused write", refusal: "storage unavailable", hook: func(map[string]any) (int, string) {
			return http.StatusInternalServerError, "storage unavailable"
		}},
	} {
		for _, path := range []string{"product", "boot", "network"} {
			t.Run(test.name+"/"+path, func(t *testing.T) {
				config := map[string]string{"user.subyard.managed": "true"}
				server, client := startPolicyServer(t, "virtual-machine", "Stopped", ownedVMProjectConfig(nil), config, maps.Clone(config))
				server.OnInstanceUpdate(test.hook)
				err := startThrough(client, path)
				if err == nil || !strings.Contains(err.Error(), test.refusal) {
					t.Fatalf("start error = %v, want %q", err, test.refusal)
				}
				if calls := server.PowerCalls(); len(calls) != 0 {
					t.Fatalf("VM was started after a failed policy write: %+v", calls)
				}
				if updates := server.ConfigUpdates(); len(updates) != 0 {
					t.Fatalf("failed policy write was recorded: %+v", updates)
				}
			})
		}
	}
}

func TestVMStartLowLevelGrantRefusesAProjectETagConflict(t *testing.T) {
	config := map[string]string{"user.subyard.managed": "true"}
	server, client := startPolicyServer(t, "virtual-machine", "Stopped", ownedVMProjectConfig(nil), config, maps.Clone(config))
	server.OnProjectUpdate(func(project map[string]any) { project["description"] = "concurrent edit" })
	err := client.SetInstancePower(context.Background(), "subyard", "yard", "start", false)
	if err == nil || !strings.Contains(err.Error(), "ETag") {
		t.Fatalf("project conflict = %v", err)
	}
	if len(server.PowerCalls()) != 0 || len(server.ConfigUpdates()) != 0 || len(server.ProjectUpdates()) != 0 {
		t.Fatal("project conflict allowed a grant, QEMU write or start")
	}
}
