package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestLegacyFullRegistrationUsesProtectedDefaultRoot(t *testing.T) {
	for _, scenario := range []struct {
		name, yard string
		mode       os.FileMode
		allowed    bool
	}{
		{"default root", "default", 0o600, true},
		{"named yard cannot use root", "demo", 0o600, false},
		{"unprotected default root", "default", 0o666, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			configHome := testkit.TempDir(t)
			testkit.WriteFile(t, filepath.Join(configHome, "config.env"), []byte("INCUS_PROJECT=old-project\n"), scenario.mode)
			if err := readLegacyFullRegistration(configHome, scenario.yard); (err == nil) != scenario.allowed {
				t.Fatalf("registration allowed=%t: %v", scenario.allowed, err)
			}
			// Even a protected default registration cannot authorize an unknown
			// producer: provenance is checked before registration is considered.
			loaded := config.Loaded{Context: domain.Context{YardName: scenario.yard}}
			installation := uninstall.Installation{ConfigHome: configHome, RuntimeRoot: filepath.Join(configHome, "unknown-runtime")}
			if err := (&CLI{}).inspectLegacyFullYardProvenance(context.Background(), loaded, installation); err == nil {
				t.Fatal("unknown producer accepted a root registration")
			}
		})
	}
}

func TestLegacyFullYardRequiresEntireExactNativeScope(t *testing.T) {
	loaded := config.Loaded{Context: domain.Context{YardName: "demo", YardKind: "container", SSHPort: 24230, IncusProject: "old-project", YardInstanceName: "old-yard", IncusBridge: "old-bridge"},
		Environment: map[string]string{"SRV_POOL": "old-pool", "SRV_VOLUME": "old-srv", "HOST_MOUNTS": "host-secrets:/mnt/host/secrets:ro:0700 host-memory:/mnt/host/memory:rw:0770 host-devcontainers:/mnt/host/devcontainers:ro:0755 host-agent-sessions:/mnt/host/agent-sessions:rw:0700"}}
	loaded.Context.Paths.HostBase = "/owner/host"
	installation := uninstall.Installation{DataHome: "/owner/data", ConfigHome: "/owner/config"}
	profile := map[string]map[string]string{"root": {"type": "disk", "path": "/", "pool": "old-pool"}, "eth0": {"type": "nic", "network": "old-bridge"}}
	devices := map[string]map[string]string{"root": profile["root"], "eth0": profile["eth0"],
		"ssh":                 {"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:24230", "connect": "tcp:127.0.0.1:22"},
		"kvm":                 {"type": "unix-char", "source": "/dev/kvm", "path": "/dev/kvm", "mode": "0660", "gid": "992"},
		"host-secrets":        {"type": "disk", "source": "/owner/host/host-secrets", "path": "/mnt/host/secrets", "readonly": "true"},
		"host-memory":         {"type": "disk", "source": "/owner/host/host-memory", "path": "/mnt/host/memory"},
		"host-devcontainers":  {"type": "disk", "source": "/owner/host/host-devcontainers", "path": "/mnt/host/devcontainers", "readonly": "true"},
		"host-agent-sessions": {"type": "disk", "source": "/owner/host/host-agent-sessions", "path": "/mnt/host/agent-sessions"},
		"srv":                 {"type": "disk", "path": "/srv", "pool": "old-pool", "source": "old-srv"},
		"subyard-e2e-routes":  {"type": "disk", "source": "/owner/data/e2e/routes", "path": "/var/lib/subyard/e2e-routes", "readonly": "true"}}
	base := teardownSnapshot{State: ports.ReconcileState{ProjectFound: true, InstanceFound: true, ProfileDevices: profile,
		ProjectConfig: map[string]string{}, Instance: ports.InstanceInfo{Project: "old-project", Name: "old-yard", Devices: devices,
			Config: map[string]string{"user.subyard.managed": "true", "user.subyard.name": "demo", "user.subyard.bridge": "old-bridge", "volatile.base_image": "base-image"}}},
		Resources: []ports.TeardownResource{{Kind: "project", Name: "old-project"}, {Kind: "instance", Name: "old-yard"},
			{Kind: "profile", Name: "default"}, {Kind: "volume", Name: "old-srv", Pool: "old-pool"}, {Kind: "image", Name: "base-image"}}}
	for _, scenario := range []struct {
		name   string
		mutate func(*teardownSnapshot)
	}{
		{"exact", nil},
		{"changed proxy binding", func(s *teardownSnapshot) { s.State.Instance.Devices["ssh"]["bind"] = "instance" }},
		{"changed KVM target", func(s *teardownSnapshot) { s.State.Instance.Devices["kvm"]["path"] = "/foreign/device" }},
		{"foreign proxy endpoint", func(s *teardownSnapshot) { s.State.Instance.Devices["ssh"]["connect"] = "tcp:192.0.2.1:22" }},
		{"public proxy listener", func(s *teardownSnapshot) { s.State.Instance.Devices["ssh"]["listen"] = "tcp:0.0.0.0:24230" }},
		{"another proxy port", func(s *teardownSnapshot) { s.State.Instance.Devices["ssh"]["listen"] = "tcp:127.0.0.1:24231" }},
		{"changed KVM source", func(s *teardownSnapshot) { s.State.Instance.Devices["kvm"]["source"] = "/foreign/device" }},
		{"noncanonical KVM group", func(s *teardownSnapshot) { s.State.Instance.Devices["kvm"]["gid"] = "0992" }},
		{"overflow KVM group", func(s *teardownSnapshot) { s.State.Instance.Devices["kvm"]["gid"] = "4294967296" }},
		{"unknown KVM property", func(s *teardownSnapshot) { s.State.Instance.Devices["kvm"]["uid"] = "1000" }},
		{"another host mount root", func(s *teardownSnapshot) { s.State.Instance.Devices["host-memory"]["source"] = "/foreign/host-memory" }},
		{"another data root", func(s *teardownSnapshot) {
			s.State.Instance.Devices["subyard-e2e-routes"]["source"] = "/another/data/e2e/routes"
		}},
		{"writable route", func(s *teardownSnapshot) { s.State.Instance.Devices["subyard-e2e-routes"]["readonly"] = "false" }},
		{"unmanaged instance", func(s *teardownSnapshot) { delete(s.State.Instance.Config, "user.subyard.managed") }},
		{"partial modern ownership", func(s *teardownSnapshot) { s.State.ProjectConfig["user.subyard.operator_uid"] = "1" }},
		{"partial instance installation", func(s *teardownSnapshot) { s.State.Instance.Config["user.subyard.installation"] = "" }},
		{"foreign instance operator UID", func(s *teardownSnapshot) { s.State.Instance.Config["user.subyard.operator_uid"] = "12345" }},
		{"partial instance context", func(s *teardownSnapshot) { s.State.Instance.Config["user.subyard.context"] = "" }},
		{"foreign instance data home", func(s *teardownSnapshot) { s.State.Instance.Config["user.subyard.data_home"] = "/foreign/data" }},
		{"foreign instance config home", func(s *teardownSnapshot) { s.State.Instance.Config["user.subyard.config_home"] = "/foreign/config" }},
		{"shared storage", func(s *teardownSnapshot) { s.State.ProjectConfig["features.storage.volumes"] = "false" }},
		{"foreign instance", func(s *teardownSnapshot) {
			s.Resources = append(s.Resources, ports.TeardownResource{Kind: "instance", Name: "foreign"})
		}},
		{"foreign volume", func(s *teardownSnapshot) {
			s.Resources = append(s.Resources, ports.TeardownResource{Kind: "volume", Name: "foreign", Pool: "old-pool"})
		}},
		{"foreign profile", func(s *teardownSnapshot) {
			s.Resources = append(s.Resources, ports.TeardownResource{Kind: "profile", Name: "foreign"})
		}},
		{"foreign image", func(s *teardownSnapshot) {
			s.Resources = append(s.Resources, ports.TeardownResource{Kind: "image", Name: "foreign"})
		}},
		{"snapshot", func(s *teardownSnapshot) { s.Resources[1].SnapshotCount = 1 }},
		{"unknown device", func(s *teardownSnapshot) {
			s.State.Instance.Devices["foreign"] = map[string]string{"type": "disk", "source": "/foreign", "path": "/foreign"}
		}},
		{"profile configuration", func(s *teardownSnapshot) { s.State.ProfileConfig = map[string]string{"user.foreign": "true"} }},
		{"missing inventory", func(s *teardownSnapshot) { s.Resources = nil }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			payload, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot teardownSnapshot
			if err := json.Unmarshal(payload, &snapshot); err != nil {
				t.Fatal(err)
			}
			if scenario.mutate != nil {
				scenario.mutate(&snapshot)
			}
			err = validateLegacyFullYard(loaded, installation, &snapshot)
			if (err == nil) != (scenario.mutate == nil) {
				t.Fatalf("native scope admission: %v", err)
			}
		})
	}
}
