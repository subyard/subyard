package incusclient

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Exercise the original format producer at its synthetic CLI/guest boundary.
// The guest mock returns an observed filesystem UUID; the original function
// must publish both marker fields and reject a changed already-ready identity.
func ownerBlockFormatCommand(t *testing.T, marker, recorded, observed string) ([]byte, error) {
	t.Helper()
	helper, err := os.ReadFile("../../../scripts/lib-vm-storage.sh")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "--noprofile", "--norc", "-peu", "-s")
	command.Env = append(os.Environ(), "SRV_POOL=default", "SRV_VOLUME=yard-srv", "INCUS_PROJECT=original-project", "YARD_INSTANCE_NAME=yard", "SRV_VOLUME_SIZE=4GiB", "FORMAT_MARKER="+marker, "FORMAT_RECORDED="+recorded, "FORMAT_OBSERVED="+observed)
	command.Stdin = io.MultiReader(bytes.NewReader(helper), strings.NewReader(`
incus_wait_yard_agent() { :; }
die() { printf '%s\n' "$1" >&2; exit 1; }
incus() {
 case "$1 $2 $3" in
  'storage volume get') case "$6" in user.subyard.format) printf '%s\n' "$FORMAT_MARKER";; user.subyard.uuid) printf '%s\n' "$FORMAT_RECORDED";; *) return 2;; esac;;
  'storage volume set') printf '%s\n' "$6" "$7" >&3;;
  *) [ "$1" = exec ] || return 2; cat >/dev/null; printf '%s\n' "$FORMAT_OBSERVED";;
 esac
}
exec 3>&1
vm_storage_mount
`))
	return command.Output()
}
func ownerActualBlockFormat(t *testing.T, marker, recorded, observed string) map[string]string {
	t.Helper()
	raw, err := ownerBlockFormatCommand(t, marker, recorded, observed)
	if err != nil {
		t.Fatal("original format producer failed", err)
	}
	fields := map[string]string{}
	for _, row := range strings.Fields(string(raw)) {
		key, value, ok := strings.Cut(row, "=")
		if !ok {
			t.Fatal("unexpected format output")
		}
		fields[key] = value
	}
	if len(fields) != 2 || fields["user.subyard.format"] != "ready" || fields["user.subyard.uuid"] != observed {
		t.Fatal("original format did not publish both fields")
	}
	return fields
}
func TestOwnerBlockFormatPreservesOriginallyKnownFilesystemUUID(t *testing.T) {
	const uuid = "11111111-2222-3333-4444-555555555555"
	ownerActualBlockFormat(t, "pending", "", uuid)
	if raw, err := ownerBlockFormatCommand(t, "ready", uuid, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"); err == nil || len(raw) != 0 {
		t.Fatal("original producer accepted changed ready filesystem UUID")
	}
	ownerActualBlockFormat(t, "ready", uuid, uuid)
}

func TestOwnerBlockReadinessComparesMarkerWithMountedFilesystem(t *testing.T) {
	const uuid = "11111111-2222-3333-4444-555555555555"
	helper, err := os.ReadFile("../../../scripts/lib-vm-storage.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, actual := range []string{uuid, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"} {
		command := exec.Command("bash", "--noprofile", "--norc", "-peu", "-s")
		command.Env = append(os.Environ(), "SRV_VOLUME_TYPE=block", "SRV_VOLUME_SIZE=4GiB", "SRV_POOL=default", "SRV_VOLUME=yard-srv", "INCUS_PROJECT=original-project", "YARD_INSTANCE_NAME=yard", "ROOT_DISK_SIZE=", "FILESYSTEM_UUID="+actual)
		// Isolate the filesystem identity predicate; the device boundary has its
		// own original checks, while this fixture supplies their converged result.
		command.Stdin = io.MultiReader(bytes.NewReader(helper), strings.NewReader(`
vm_storage_check_device_boundary() { :; }
power_state() { printf 'RUNNING\n'; }
device_get() { case "$2" in type) printf disk;; pool) printf default;; source) printf yard-srv;; path) :;; io.bus) printf virtio-scsi;; *) return 2;; esac; }
incus() { case "$1" in query) printf '%s\n' '{"content_type":"block","config":{"size":"4GiB","user.subyard.storage":"v1","user.subyard.format":"ready","user.subyard.uuid":"11111111-2222-3333-4444-555555555555"}}';; exec) printf '%s\n' "$FILESYSTEM_UUID";; *) return 2;; esac; }
vm_storage_check
`))
		_, err := command.Output()
		if (err == nil) != (actual == uuid) {
			t.Fatal("original readiness lost mounted UUID predicate", err)
		}
	}
}
