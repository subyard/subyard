package reconcileruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func appArmorRuntime(t *testing.T, output string, mask bool) (Runtime, *testkit.Incus) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "systemctl"), []byte(`#!/bin/sh
[ "$*" = 'show incus.service -p Environment --value' ] || exit 90
[ -z "${PROBE_LOG:-}" ] || printf 'probe\n' >> "$PROBE_LOG"
[ "${PROBE_SLEEP:-0}" = 0 ] || exec /bin/sleep 30
printf '%s\n' "$PROBE_OUTPUT"
exit "${PROBE_EXIT:-0}"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	instance := ports.InstanceInfo{
		Name: "yard", Project: "subyard", Status: "Running",
		Config: map[string]string{
			"user.subyard.managed": "true", "user.subyard.initialized": "true",
			"user.subyard.desired_power": "running", "user.subyard.name": "default",
			"user.subyard.bridge": "incusbr0", "boot.autostart": "false",
		},
		LocalConfig: map[string]string{"security.nesting": "true"},
		LocalDevices: map[string]map[string]string{
			"srv": {"source": "yard-srv", "path": "/srv", "pool": "default"},
			"subyard-e2e-routes": {
				"type": "disk", "source": filepath.Join(root, "e2e", "routes"),
				"path": "/var/lib/subyard/e2e-routes", "readonly": "true",
			},
		},
	}
	if mask {
		instance.LocalDevices["subyard-docker-apparmor"] = map[string]string{
			"type": "disk", "source": "/dev/null",
			"path": "/sys/module/apparmor/parameters/enabled", "readonly": "true",
		}
	}
	incus := &testkit.Incus{
		ServerInfo: ports.ServerInfo{Environment: "incus"},
		Instances:  map[string]ports.InstanceInfo{"subyard/yard": instance},
		Reconcile:  ports.ReconcileState{InstanceFound: true, VolumeFound: true, Instance: instance},
	}
	return Runtime{
		RepositoryRoot: root, Incus: incus, ConfigWriter: incus, HostDeviceRoot: root,
		NetworkPolicy: &networkPolicyFixture{},
		Environment:   []string{"PATH=" + root, "PROBE_OUTPUT=" + output},
		Yard: domain.Context{
			YardName: "default", IncusProject: "subyard", YardInstanceName: "yard",
			IncusBridge: "incusbr0", YardKind: domain.YardContainer,
			Paths: domain.RuntimePaths{DataHome: root},
		},
	}, incus
}

func TestAppArmorProbeConvergence(t *testing.T) {
	for _, test := range []struct {
		name, output      string
		disabled, unknown bool
	}{
		{name: "empty"},
		{name: "enabled", output: "INCUS_SECURITY_APPARMOR=true"},
		{name: "disabled", output: "INCUS_SECURITY_APPARMOR=false", disabled: true},
		{name: "quoted", output: `"INCUS_SECURITY_APPARMOR=false"`, disabled: true},
		{name: "unrelated quoted value", output: `"NOTE=before INCUS_SECURITY_APPARMOR=false after"`},
		{name: "escaped quotes", output: `"NOTE=\" INCUS_SECURITY_APPARMOR=false \""`},
		{name: "escaped space", output: `NOTE=before\ INCUS_SECURITY_APPARMOR=false`},
		{name: "single quoted", output: `'NOTE=before INCUS_SECURITY_APPARMOR=false after'`},
		{name: "unrelated flag", output: "OTHER_INCUS_SECURITY_APPARMOR=false"},
		{name: "conflict", output: "INCUS_SECURITY_APPARMOR=false INCUS_SECURITY_APPARMOR=true", unknown: true},
		{name: "duplicate", output: "INCUS_SECURITY_APPARMOR=false INCUS_SECURITY_APPARMOR=false", unknown: true},
		{name: "unsupported value", output: "INCUS_SECURITY_APPARMOR=maybe", unknown: true},
		{name: "invalid quoted escape", output: `"INCUS_SECURITY_APPARMOR=fa\lse"`, unknown: true},
		{name: "unclosed quote", output: `"INCUS_SECURITY_APPARMOR=false`, unknown: true},
		{name: "trailing escape", output: "NOTE=private-sentinel\\", unknown: true},
		{name: "not assignment", output: "private-sentinel", unknown: true},
		{name: "invalid name", output: "BAD-NAME=value", unknown: true},
		{name: "multiline", output: "INCUS_SECURITY_APPARMOR=false\nNOTE=x", unknown: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, mask := range []bool{false, true} {
				runtime, _ := appArmorRuntime(t, test.output, mask)
				converged, err := runtime.CheckStage(context.Background(), ports.ReconcileStageInstance)
				want := !test.unknown && mask == test.disabled
				if converged != want || (err != nil) != test.unknown {
					t.Fatalf("mask=%v: converged=%v err=%v; want converged=%v unknown=%v", mask, converged, err, want, test.unknown)
				}
				if err != nil && (!strings.Contains(err.Error(), "unknown") || strings.Contains(err.Error(), "private-sentinel")) {
					t.Fatalf("expected sanitized unknown diagnostic: %v", err)
				}
			}
		})
	}
}

func TestAppArmorFailurePrecedesInstanceDrift(t *testing.T) {
	for _, drift := range []string{"none", "instance", "volume", "status", "srv", "routes", "nesting"} {
		t.Run(drift, func(t *testing.T) {
			runtime, incus := appArmorRuntime(t, "", true)
			runtime.Environment = append(runtime.Environment, "PROBE_EXIT=1")
			switch drift {
			case "instance":
				incus.Reconcile.InstanceFound = false
			case "volume":
				incus.Reconcile.VolumeFound = false
			case "status":
				incus.Reconcile.Instance.Status = "Stopped"
			case "srv":
				delete(incus.Reconcile.Instance.LocalDevices, "srv")
			case "routes":
				delete(incus.Reconcile.Instance.LocalDevices, "subyard-e2e-routes")
			case "nesting":
				delete(incus.Reconcile.Instance.LocalConfig, "security.nesting")
			}
			if converged, err := runtime.CheckStage(context.Background(), ports.ReconcileStageInstance); converged || err == nil {
				t.Fatalf("failed capability probe became ordinary drift: converged=%v err=%v", converged, err)
			}
		})
	}
}

func TestAppArmorProbeUnavailable(t *testing.T) {
	for _, mode := range []string{"missing", "failed", "cancelled", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			runtime, _ := appArmorRuntime(t, "", false)
			ctx := context.Background()
			switch mode {
			case "missing":
				runtime.Environment[0] = "PATH=" + t.TempDir()
				t.Setenv("PATH", t.TempDir())
			case "failed":
				runtime.Environment = append(runtime.Environment, "PROBE_EXIT=1")
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			case "timeout":
				deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
				ctx = deadline
				runtime.Environment = append(runtime.Environment, "PROBE_SLEEP=1")
			}
			if converged, err := runtime.CheckStage(ctx, ports.ReconcileStageInstance); converged || err == nil {
				t.Fatalf("unavailable capability accepted: converged=%v err=%v", converged, err)
			}
		})
	}
}

func TestAppArmorApplyRechecksBeforePowerMutation(t *testing.T) {
	runtime, incus := appArmorRuntime(t, "INCUS_SECURITY_APPARMOR=false", false)
	if converged, err := runtime.CheckStage(context.Background(), ports.ReconcileStageInstance); converged || err != nil {
		t.Fatalf("expected known mask drift: converged=%v err=%v", converged, err)
	}
	runtime.Environment = append(runtime.Environment, "PROBE_EXIT=1")
	err := runtime.ApplyStage(context.Background(), ports.ReconcileStageInstance)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("expected capability error before invoking script: %v", err)
	}
	if len(incus.ConfigUpdates) != 0 {
		t.Fatalf("failed Apply probe changed power metadata: %#v", incus.ConfigUpdates)
	}
}

func TestAppArmorVMDoesNotProbe(t *testing.T) {
	for _, mask := range []bool{false, true} {
		runtime, _ := appArmorRuntime(t, "", mask)
		runtime.Yard.YardKind = domain.YardVM
		runtime.Environment = append(runtime.Environment, "PROBE_EXIT=90")
		converged, err := runtime.CheckStage(context.Background(), ports.ReconcileStageInstance)
		if err != nil || converged == mask {
			t.Fatalf("VM depends on container capability: mask=%v converged=%v err=%v", mask, converged, err)
		}
	}
}

func TestAppArmorApplyPassesFreshObservationToScript(t *testing.T) {
	for _, kind := range []domain.YardKind{domain.YardContainer, domain.YardVM} {
		runtime, incus := appArmorRuntime(t, "INCUS_SECURITY_APPARMOR=false", false)
		runtime.Yard.YardKind = kind
		log := filepath.Join(runtime.RepositoryRoot, "probes")
		capture := filepath.Join(runtime.RepositoryRoot, "prepared")
		runtime.Environment = append(runtime.Environment, "PROBE_LOG="+log, "CAPTURE="+capture,
			"SUBYARD_PREPARED_INCUS_APPARMOR=invalid-ambient-value")
		if kind == domain.YardVM {
			runtime.Environment = append(runtime.Environment, "PROBE_EXIT=90")
		}
		scripts := filepath.Join(runtime.RepositoryRoot, "scripts")
		if err := os.Mkdir(scripts, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scripts, "03-create-subyard.sh"), []byte(`#!/bin/sh
printf '%s\n' "$SUBYARD_PREPARED_INCUS_APPARMOR" > "$CAPTURE"
`), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := runtime.ApplyStage(context.Background(), ports.ReconcileStageInstance); err != nil {
			t.Fatal(err)
		}
		policy := runtime.NetworkPolicy.(*networkPolicyFixture)
		if len(policy.started) != 1 {
			t.Fatalf("instance start was not network-gated: %#v", policy.started)
		}
		got, err := os.ReadFile(capture)
		want := "disabled\n"
		if kind == domain.YardVM {
			want = "restored\n"
		}
		if err != nil || string(got) != want {
			t.Fatalf("prepared state=%q err=%v, want %q", got, err, want)
		}
		probes, _ := os.ReadFile(log)
		wantProbes := "probe\n"
		if kind == domain.YardVM {
			wantProbes = ""
		}
		if string(probes) != wantProbes {
			t.Fatalf("probe calls=%q, want %q", probes, wantProbes)
		}
		if len(incus.ConfigUpdates) == 0 {
			t.Fatal("successful Apply skipped power metadata")
		}
	}
}
