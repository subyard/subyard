package reconcileruntime

import (
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

const reportingOnlyQEMUConfig = "[device \"qemu_balloon\"]\nfree-page-reporting = \"on\""

func rawQEMUInstance(status, raw string, desired domain.DesiredPower) ports.InstanceInfo {
	config := map[string]string{"user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.desired_power": string(desired)}
	if raw != "" {
		config["raw.qemu.conf"] = raw
	}
	return ports.InstanceInfo{Status: status, Config: config, LocalConfig: config}
}

// A running VM keeps its previous owner value until its next start: that is
// converged with a diagnostic, never drift or a restart. Only an explicit
// reporting opt-in still restarts, and foreign values refuse with every finding.
func TestVMRawQEMUPolicyDefersThePreviousOwnerValueToTheNextStart(t *testing.T) {
	runtime := Runtime{Yard: domain.Context{YardKind: domain.YardVM, IncusProject: "subyard"}}
	for _, test := range []struct {
		name                string
		reporting, preserve bool
		instance            ports.InstanceInfo
		profile             map[string]string
		converged, deferred bool
		refusals            []string
	}{
		{name: "current headless", instance: rawQEMUInstance("Running", domain.VMRawQEMUConfig(false), domain.PowerRunning), converged: true},
		{name: "running VM that keeps its display", instance: rawQEMUInstance("Running", "", domain.PowerRunning), converged: true, deferred: true},
		{name: "running reporting VM that keeps its display", instance: rawQEMUInstance("Running", reportingOnlyQEMUConfig, domain.PowerRunning), converged: true, deferred: true},
		{name: "same value with reporting requested", reporting: true, instance: rawQEMUInstance("Running", reportingOnlyQEMUConfig, domain.PowerRunning), converged: true, deferred: true},
		{name: "disabling reporting keeps the installed override", instance: rawQEMUInstance("Running", domain.VMRawQEMUConfig(true), domain.PowerRunning), converged: true},
		{name: "intentionally stopped VM waits for its start", instance: rawQEMUInstance("Stopped", "", domain.PowerStopped), converged: true, deferred: true},
		{name: "hard cleanup accepts reporting-only before its start", reporting: true, preserve: true, instance: rawQEMUInstance("Stopped", reportingOnlyQEMUConfig, domain.PowerRunning), converged: true, deferred: true},
		{name: "stopped VM that init starts", instance: rawQEMUInstance("Stopped", "", domain.PowerRunning)},
		{name: "reporting opt-in restarts as before", reporting: true, instance: rawQEMUInstance("Running", domain.VMRawQEMUConfig(false), domain.PowerRunning)},
		{name: "reporting opt-in without any override", reporting: true, instance: rawQEMUInstance("Running", "", domain.PowerRunning)},
		{name: "foreign override", instance: rawQEMUInstance("Running", "[device \"qemu_gpu\"]\ndriver = \"virtio-vga\"", domain.PowerRunning),
			refusals: []string{"unexpected effective raw.qemu.conf on VM", "unexpected local raw.qemu.conf on VM"}},
		{name: "inherited owner value", instance: ports.InstanceInfo{Status: "Running", Config: map[string]string{"raw.qemu.conf": domain.VMRawQEMUConfig(false)}, LocalConfig: map[string]string{}},
			refusals: []string{"VM raw.qemu.conf must be owner-managed locally, not inherited"}},
		{name: "every independent finding", instance: ports.InstanceInfo{Status: "Running",
			Config:      map[string]string{"raw.qemu": "-foreign", "raw.qemu.scriptlet": "foreign"},
			LocalConfig: map[string]string{"raw.qemu": "-foreign"}},
			profile: map[string]string{"raw.qemu.conf": domain.VMRawQEMUConfig(false), "raw.qemu.qmp.early": "[]"},
			refusals: []string{"unexpected effective raw.qemu on VM", "unexpected effective raw.qemu.scriptlet on VM", "unexpected local raw.qemu on VM",
				"unexpected inherited raw.qemu.conf in VM project subyard", "unexpected inherited raw.qemu.qmp.early in VM project subyard"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime.Environment = nil
			if test.preserve {
				runtime.Environment = []string{"SUBYARD_PRESERVE_VM_REPORTING=1"}
			}
			converged, deferred, err := runtime.vmRawQEMUPolicy(ports.ReconcileState{Instance: test.instance, ProfileConfig: test.profile}, test.reporting)
			if len(test.refusals) == 0 && (err != nil || converged != test.converged || deferred != test.deferred) {
				t.Fatalf("converged=%v deferred=%v err=%v, want %v %v", converged, deferred, err, test.converged, test.deferred)
			}
			for _, refusal := range test.refusals {
				if err == nil || converged || !strings.Contains(err.Error(), refusal) {
					t.Errorf("missing refusal %q in %v", refusal, err)
				}
			}
		})
	}
}

func TestVMAdoptionTakesOwnerOverridesAsFound(t *testing.T) {
	for _, test := range []struct {
		name, reporting, raw string
		refused              bool
	}{
		{name: "VM without override", reporting: "0"},
		{name: "headless override", reporting: "0", raw: domain.VMRawQEMUConfig(false)},
		{name: "headless reporting override", reporting: "1", raw: domain.VMRawQEMUConfig(true)},
		{name: "reporting override that keeps the display", reporting: "1", raw: reportingOnlyQEMUConfig},
		{name: "reporting VM without its balloon property", reporting: "1", raw: domain.VMRawQEMUConfig(false), refused: true},
		{name: "foreign override", reporting: "0", raw: "[device \"qemu_balloon\"]\ndeflate-on-oom = \"on\"", refused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := Runtime{Yard: domain.Context{YardName: "vm", YardKind: domain.YardVM},
				Environment: []string{"VM_FREE_PAGE_REPORTING=" + test.reporting}}
			config := map[string]string{}
			if test.raw != "" {
				config["raw.qemu.conf"] = test.raw
			}
			state := ports.ReconcileState{Instance: ports.InstanceInfo{Config: config, LocalConfig: config}}
			configuration := map[string]string{}
			err := runtime.adoptionKindPolicy(state, configuration)
			if test.refused != (err != nil && strings.Contains(err.Error(), "converged VM raw policy")) {
				t.Fatalf("refused=%v: %v", test.refused, err)
			}
			if !test.refused && configuration["raw.qemu.conf"] != test.raw {
				t.Fatalf("adopted %q, want %q as found", configuration["raw.qemu.conf"], test.raw)
			}
		})
	}
}

// An adoption refusal names every offending raw QEMU key and scope, in a stable
// order, with the finding that refused it.
func TestVMAdoptionRefusalReportsEveryRawQEMUFinding(t *testing.T) {
	runtime := Runtime{Yard: domain.Context{YardName: "vm", YardKind: domain.YardVM}, Environment: []string{"VM_FREE_PAGE_REPORTING=0"}}
	state := ports.ReconcileState{Instance: ports.InstanceInfo{
		Config:      map[string]string{"raw.qemu": "-foreign", "raw.qemu.conf": domain.VMRawQEMUConfig(false), "raw.qemu.scriptlet": "foreign"},
		LocalConfig: map[string]string{"raw.qemu": "-foreign"},
	}}
	err := runtime.adoptionKindPolicy(state, map[string]string{})
	if err == nil {
		t.Fatal("foreign raw QEMU configuration was adopted")
	}
	want := []string{
		"unexpected effective raw.qemu on VM; unexpected effective raw.qemu.scriptlet on VM; unexpected local raw.qemu on VM; VM raw.qemu.conf must be owner-managed locally, not inherited",
		"raw.qemu  differs -> (unset)", "raw.qemu.conf  differs -> owner-policy", "raw.qemu.conf.local  (unset) -> owner-policy",
		"raw.qemu.local  differs -> (unset)", "raw.qemu.scriptlet  foreign -> (unset)",
	}
	last := -1
	for _, row := range want {
		index := strings.Index(err.Error(), row)
		if index <= last {
			t.Fatalf("refusal %q lacks %q in stable order", err, row)
		}
		last = index
	}
	runtime.Environment = []string{"VM_FREE_PAGE_REPORTING=1"}
	state = ports.ReconcileState{Instance: rawQEMUInstance("Running", domain.VMRawQEMUConfig(false), domain.PowerRunning)}
	if err := runtime.adoptionKindPolicy(state, map[string]string{}); err == nil ||
		!strings.Contains(err.Error(), "VM Free Page Reporting requires its owner raw.qemu.conf") ||
		!strings.Contains(err.Error(), "raw.qemu.conf.local  differs -> owner-policy") {
		t.Fatalf("reporting refusal = %v", err)
	}
}

// Plans that span an approved start bind an owner raw.qemu.conf as the value
// the product start writes, so that exact change is not drift. A foreign
// value and a changed reporting choice stay bound.
func TestLifecycleOutputsBindTheStartWrittenRawQEMUConfig(t *testing.T) {
	bind := func(raw string, virtualMachine bool) string {
		values := map[string]string{}
		if raw != "" {
			values["raw.qemu.conf"] = raw
		}
		NormalizeInstanceLifecycleConfig(values, nil, virtualMachine)
		return values["raw.qemu.conf"]
	}
	for _, pair := range [][2]string{{"", domain.VMRawQEMUConfig(false)}, {reportingOnlyQEMUConfig, domain.VMRawQEMUConfig(true)}} {
		if bind(pair[0], true) != bind(pair[1], true) {
			t.Errorf("start write %q -> %q is bound as drift", pair[0], pair[1])
		}
	}
	foreign := "[device \"qemu_gpu\"]\ndriver = \"virtio-vga\""
	for _, pair := range [][2]string{{"", domain.VMRawQEMUConfig(true)}, {foreign, domain.VMRawQEMUConfig(false)}} {
		if bind(pair[0], true) == bind(pair[1], true) {
			t.Errorf("%q -> %q is not bound", pair[0], pair[1])
		}
	}
	if bind("", false) != "" {
		t.Error("a container acquired a raw QEMU binding")
	}
}
