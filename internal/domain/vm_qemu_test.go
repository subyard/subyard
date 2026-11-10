package domain

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestVMRawQEMUShellParity(t *testing.T) {
	script := filepath.Join("..", "..", "scripts", "lib-vm-page-reporting.sh")
	for _, raw := range []string{"", "[device \"qemu_balloon\"]\nfree-page-reporting = \"on\"", VMRawQEMUConfig(false), VMRawQEMUConfig(true), "foreign"} {
		for _, requested := range []bool{false, true} {
			flag := "0"
			if requested {
				flag = "1"
			}
			command := exec.Command("bash", "-c", `source "$1"
printf '%s\0%s\0' "$VM_NO_DISPLAY_CONF" "$VM_NO_DISPLAY_CONF"$'\n'"$VM_PAGE_REPORTING_CONF"
VM_FREE_PAGE_REPORTING="$3"
if vm_raw_qemu_owner_conf "$2"; then printf 'true\0'; vm_raw_qemu_target "$2"; else printf 'false\0'; fi`, "parity", script, raw, flag)
			output, err := command.Output()
			if err != nil {
				t.Fatal(err)
			}
			want, owner := VMRawQEMUTarget(raw, requested)
			if got := strings.Split(string(output), "\x00"); !slices.Equal(got, []string{VMRawQEMUConfig(false), VMRawQEMUConfig(true), strconv.FormatBool(owner), want}) {
				t.Fatalf("shell policy(%q, %v) = %q, want owner=%v target=%q", raw, requested, got, owner, want)
			}
		}
	}
}

func TestVMRawQEMUTargetKeepsEveryOwnerValueHeadlessAndRefusesForeignOnes(t *testing.T) {
	headless, reporting := VMRawQEMUConfig(false), VMRawQEMUConfig(true)
	if headless != "[device \"qemu_gpu\"]" || reporting != "[device \"qemu_gpu\"]\n[device \"qemu_balloon\"]\nfree-page-reporting = \"on\"" {
		t.Fatalf("owner values changed: %q %q", headless, reporting)
	}
	previous := "[device \"qemu_balloon\"]\nfree-page-reporting = \"on\""
	for _, test := range []struct {
		value     string
		requested bool
		want      string
		owner     bool
	}{
		{value: "", want: headless, owner: true},
		{value: "", requested: true, want: reporting, owner: true},
		{value: previous, want: reporting, owner: true},
		{value: headless, want: headless, owner: true},
		{value: headless, requested: true, want: reporting, owner: true},
		{value: reporting, want: reporting, owner: true},
		{value: "[device \"qemu_gpu\"]\ndriver = \"virtio-vga\""},
		{value: headless + "\n"},
	} {
		got, owner := VMRawQEMUTarget(test.value, test.requested)
		if got != test.want || owner != test.owner {
			t.Errorf("target(%q, %v) = %q %v, want %q %v", test.value, test.requested, got, owner, test.want, test.owner)
		}
	}
}

func TestVMRawQEMURefusalsReportEveryFindingInStableOrder(t *testing.T) {
	got := VMRawQEMURefusals(
		map[string]string{"raw.qemu.scriptlet": "x", "raw.qemu": "-x", "raw.qemu.conf": VMRawQEMUConfig(false), "limits.cpu": "2"},
		map[string]string{"raw.qemu": "-x"})
	want := []string{"unexpected effective raw.qemu on VM", "unexpected effective raw.qemu.scriptlet on VM", "unexpected local raw.qemu on VM",
		"VM raw.qemu.conf must be owner-managed locally, not inherited"}
	if !slices.Equal(got, want) {
		t.Fatalf("refusals = %q, want %q", got, want)
	}
	if got := VMRawQEMURefusals(map[string]string{"raw.qemu.conf": VMRawQEMUConfig(true)}, map[string]string{"raw.qemu.conf": VMRawQEMUConfig(true)}); len(got) != 0 {
		t.Fatalf("owner value refused: %q", got)
	}
}
