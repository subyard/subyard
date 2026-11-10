package domain

import (
	"maps"
	"slices"
	"strings"
)

// The owner raw.qemu.conf of a VM yard is fixed text, never operator or
// profile input. An override section without keys removes the Incus default
// emulated GPU (virtio-vga): it slows guest boot and shutdown and never
// renders with host hardware.
const (
	vmNoDisplayQEMUConfig     = "[device \"qemu_gpu\"]"
	vmPageReportingQEMUConfig = "[device \"qemu_balloon\"]\nfree-page-reporting = \"on\""
)

// VMRawQEMUConfig is the owner raw.qemu.conf of a VM yard: headless, plus the
// balloon reporting property when the yard opted in.
func VMRawQEMUConfig(reporting bool) string {
	if reporting {
		return vmNoDisplayQEMUConfig + "\n" + vmPageReportingQEMUConfig
	}
	return vmNoDisplayQEMUConfig
}

// VMRawQEMUOwnerValue reports whether a local raw.qemu.conf is one a release
// wrote for a VM yard: none, the reporting-only value of releases that kept
// the emulated display, or a current headless value.
func VMRawQEMUOwnerValue(value string) bool {
	return value == "" || value == vmPageReportingQEMUConfig || value == VMRawQEMUConfig(false) || value == VMRawQEMUConfig(true)
}

// VMRawQEMUReporting reports whether an owner value enables Free Page Reporting.
func VMRawQEMUReporting(value string) bool {
	return value == vmPageReportingQEMUConfig || value == VMRawQEMUConfig(true)
}

// VMRawQEMUTarget is the owner value that replaces an owner value: headless,
// with reporting when the yard requests it or the value already enables it.
// Disabling reporting never removes an installed override. A product start
// uses reporting=false because it does not read yard settings. ok is false for
// a foreign value, which is never replaced.
func VMRawQEMUTarget(value string, reporting bool) (string, bool) {
	if !VMRawQEMUOwnerValue(value) {
		return "", false
	}
	return VMRawQEMUConfig(reporting || VMRawQEMUReporting(value)), true
}

// VMRawQEMURefusals returns every raw QEMU finding that is not an owner value:
// any other raw.qemu key, a foreign raw.qemu.conf, or one that is inherited
// instead of local. Expanded and local configuration are both inspected.
func VMRawQEMURefusals(expanded, local map[string]string) []string {
	var findings []string
	for _, scope := range []struct {
		name   string
		values map[string]string
	}{{"effective", expanded}, {"local", local}} {
		for _, key := range slices.Sorted(maps.Keys(scope.values)) {
			value := scope.values[key]
			if !strings.HasPrefix(key, "raw.qemu") || value == "" {
				continue
			}
			if key != "raw.qemu.conf" || !VMRawQEMUOwnerValue(value) {
				findings = append(findings, "unexpected "+scope.name+" "+key+" on VM")
			}
		}
	}
	if expanded["raw.qemu.conf"] != local["raw.qemu.conf"] {
		findings = append(findings, "VM raw.qemu.conf must be owner-managed locally, not inherited")
	}
	return findings
}
