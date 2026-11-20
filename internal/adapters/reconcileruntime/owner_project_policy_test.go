package reconcileruntime

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func ownerProjectRendererFixture(t *testing.T, root string, create bool, reporting, nested string) Runtime {
	t.Helper()
	runtime, _ := materializedIntegrationFixture(t, root, create)
	if create {
		helper, err := os.ReadFile("../../../scripts/lib/project-config.sh")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "scripts/lib"), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, ownerProjectRenderer), helper, 0644)
	}
	runtime.Yard.YardName = "test-yard"
	runtime.Yard.Paths.RepositoryRoot = root
	runtime.Yard.Paths.ConfigDir = filepath.Join(root, "config")
	runtime.Yard.Paths.ConfigHome = filepath.Join(root, "operator-config")
	runtime.Yard.Paths.DataHome = filepath.Join(root, "operator-data")
	runtime.Yard.Paths.HostBase = filepath.Join(root, "host-\xff-data")
	contextRaw, err := json.Marshal(runtime.Yard)
	if err != nil {
		t.Fatal(err)
	}
	runtime.Environment = append(runtime.Environment,
		"SUBYARD_HOME="+runtime.Yard.Paths.DataHome,
		"SUBYARD_CONFIG_HOME="+runtime.Yard.Paths.ConfigHome,
		"HOST_BASE="+runtime.Yard.Paths.HostBase,
		"SUBYARD_INSTALLATION_CONTEXT="+string(contextRaw),
		"YARD_NAME="+runtime.Yard.YardName,
		"YARD_INSTANCE_NAME="+runtime.Yard.YardInstanceName,
		"SRV_POOL=original-pool", "SRV_VOLUME=original-volume", "STORAGE_PATH="+filepath.Join(root, "storage"),
		"INCUS_NETWORK=original-bridge", "YARD_KIND=vm", "VM_FREE_PAGE_REPORTING="+reporting, "NESTED_E2E_VMS="+nested)
	return runtime
}

func ownerRenderedMap(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	if len(raw) == 0 || raw[len(raw)-1] != 0 {
		t.Fatal("renderer output is not bounded byte framing")
	}
	result := map[string]string{}
	for _, row := range bytes.Split(raw[:len(raw)-1], []byte{0}) {
		key, value, ok := strings.Cut(string(row), "=")
		if !ok {
			t.Fatal("renderer row is malformed")
		}
		if _, duplicate := result[key]; duplicate {
			t.Fatal("renderer row is repeated")
		}
		result[key] = value
	}
	return result
}

func TestOwnerBlockVMPreInstancePortionPreservesOriginalRootDiskOrder(t *testing.T) {
	repository, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := testkit.TempDir(t)
	script := `set -euo pipefail
source "$1/tests/helpers/test-context.sh"
setup_test_context "$2"
export YARD_KIND=vm ROOT_DISK_SIZE=20GiB SRV_VOLUME_TYPE=block SRV_VOLUME_SIZE=4GiB
export YARD_IMAGE=images:debian/13 YARD_IMAGE_FALLBACK=images:ubuntu/24.04 VM_FREE_PAGE_REPORTING=0
export SRV_POOL=default SRV_VOLUME=yard-srv HOME="$2/home"
export ASSUME_YES=1 SUBYARD_NO_AUDIT=1 SUBYARD_POWER_DESIRED=stopped
export MOCK_ROOT="$2" MOCK_TRACE="$2/trace"
install -d -m 0700 "$2/bin"
export PATH="$2/bin:$PATH"
cat >"$2/bin/incus" <<'INCUS'
#!/bin/bash
set -eu
printf '%s\n' "$*" >>"$MOCK_TRACE"
case "$*" in
 'info'|'project show subyard') exit 0 ;;
 'info yard --project subyard') exit 1 ;;
 'profile get default raw.qemu'*' --project subyard') exit 0 ;;
 'profile device get default root size --project subyard') cat "$MOCK_ROOT/root-size" ;;
 'profile device set default root size=20GiB --project subyard') printf '20GiB\n' >"$MOCK_ROOT/root-size" ;;
 'config get yard volatile.uuid --project subyard') printf 'original-uuid\n' ;;
 'config get yard user.subyard.owner_create --project subyard') printf 'original-token\n' ;;
 'storage volume get default yard-srv user.subyard.owner_create --project subyard') printf 'foreign-volume-token\n' ;;
 'storage volume show default yard-srv --project subyard') exit 1 ;;
 'image list images: --project subyard --format=json') printf '[{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","type":"virtual-machine","aliases":[{"name":"debian/13"}]}]\n' ;;
 init*) exit 17 ;;
 *) exit 90 ;;
esac
INCUS
printf '#!/bin/sh\nexit 0\n' >"$2/bin/dpkg"
printf '#!/bin/sh\nprintf "inactive\\n"\nexit 3\n' >"$2/bin/systemctl"
chmod 0700 "$2/bin/"{incus,dpkg,systemctl}
: >"$2/root-size";: >"$2/trace"
# Stop the ordinary path at its first create request, after all preconditions.
if bash "$1/scripts/03-create-subyard.sh" --yes >"$2/ordinary-output" 2>&1;then exit 61;fi
# The first info yard occurs inside root-size preparation. Retain through the
# root write instead of inferring order from its resulting desired state.
sed -n '1,/^profile device set default root size=20GiB --project subyard$/p' "$2/trace" >"$2/ordinary-prefix"
grep -q '^init ' "$2/trace" || { cat "$2/ordinary-output" "$2/trace" >&2; exit 62; }
: >"$2/root-size";: >"$2/trace"
SUBYARD_PREPARED_OWNER_PHASE=1 bash "$1/scripts/03-create-subyard.sh" --owner-before-instance --yes >"$2/native-output" 2>&1
cmp "$2/ordinary-prefix" "$2/trace"
[ "$(cat "$2/root-size")" = 20GiB ]
! grep -Eq '^(init |image |storage volume )' "$2/trace"
# Native portions cannot be reached by an unbound ordinary selector.
if bash "$1/scripts/03-create-subyard.sh" --owner-before-instance --yes >"$2/refused-output" 2>&1;then exit 63;fi
# Even a retained volume has no fallback create authority in a native half.
: >"$2/trace"
if SUBYARD_PREPARED_OWNER_PHASE=1 SUBYARD_PREPARED_OWNER_INSTANCE_UUID=original-uuid \
 SRV_VOLUME_TYPE=filesystem bash "$1/scripts/03-create-subyard.sh" --owner-after-srv --yes >"$2/refused-output" 2>&1;then exit 64;fi
! grep -q '^storage volume create ' "$2/trace"
# An explicit create result's token cannot accept a foreign same-name volume.
: >"$2/trace"
if SUBYARD_PREPARED_OWNER_PHASE=1 SUBYARD_PREPARED_OWNER_INSTANCE_UUID=original-uuid \
 SUBYARD_PREPARED_OWNER_VOLUME_TOKEN=original-volume-token \
 bash "$1/scripts/03-create-subyard.sh" --owner-after-srv --yes >"$2/refused-output" 2>&1;then exit 65;fi
! grep -Eq '^(storage volume create |config device (add|remove) )' "$2/trace"
`
	command := exec.Command("bash", "-eu", "-s", "--", repository, root)
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("original block-VM pre-instance trace changed: %v (%d fixture diagnostic bytes): %s", err, len(output), output)
	}
}

func TestOwnerProjectNativePortionsShareOriginalPolicyOrder(t *testing.T) {
	repository, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := testkit.TempDir(t)
	script := `set -euo pipefail
source "$1/tests/helpers/test-context.sh"
setup_test_context "$2"
export HOME="$2/home" ASSUME_YES=1 SRV_POOL=default SRV_VOLUME=yard-srv STORAGE_POOL=default
export MOCK_LOG="$2/trace" MOCK_EXISTS="$2/project" MOCK_TOKEN=original-project-token
install -d -m 0700 "$2/bin"
export PATH="$2/bin:$PATH"
cat >"$2/bin/incus" <<'INCUS'
#!/bin/bash
set -eu
printf '%s\n' "$*" >>"$MOCK_LOG"
case "${1:-} ${2:-}" in
 'info ') exit 0 ;;
 'project show') test -f "$MOCK_EXISTS" ;;
 'project create') exit 91 ;;
 'project get')
  test -f "$MOCK_EXISTS"
  case "$4" in
   user.subyard.installation) printf '1\n' ;;
   user.subyard.operator_uid) id -u ;;
   user.subyard.data_home) printf '%s\n' "$SUBYARD_HOME" ;;
   user.subyard.config_home) printf '%s\n' "$SUBYARD_CONFIG_HOME" ;;
   user.subyard.owner_create) printf '%s\n' "$MOCK_TOKEN" ;;
  esac ;;
 'project set'|'project unset') exit 0 ;;
 'profile device') [ "$3" = list ] || [ "$3" = add ] ;;
 *) exit 92 ;;
esac
INCUS
chmod 0700 "$2/bin/incus"
: >"$MOCK_LOG"
SUBYARD_PREPARED_OWNER_PHASE=1 bash "$1/scripts/02-create-project.sh" --owner-before-project --yes >"$2/before-output" 2>&1
! grep -Eq '^(project (create|set|unset)|profile device add)' "$MOCK_LOG"
# A missing SDK result can never fall back to CLI project creation.
: >"$MOCK_LOG"
if SUBYARD_PREPARED_OWNER_PHASE=1 bash "$1/scripts/02-create-project.sh" --owner-after-project --yes >"$2/missing-output" 2>&1; then exit 61;fi
! grep -Eq '^(project (create|set|unset)|profile device add)' "$MOCK_LOG"
touch "$MOCK_EXISTS"
: >"$MOCK_LOG"
bash "$1/scripts/02-create-project.sh" --yes >"$2/ordinary-output" 2>&1
grep -E '^(project (set|unset)|profile device add)' "$MOCK_LOG" >"$2/ordinary-mutations"
: >"$MOCK_LOG"
SUBYARD_PREPARED_OWNER_PHASE=1 SUBYARD_PREPARED_OWNER_PROJECT_TOKEN=original-project-token \
 bash "$1/scripts/02-create-project.sh" --owner-after-project --yes >"$2/after-output" 2>&1
grep -E '^(project (set|unset)|profile device add)' "$MOCK_LOG" >"$2/native-mutations"
cmp "$2/ordinary-mutations" "$2/native-mutations"
! grep -q '^project create ' "$MOCK_LOG"
# Root disk and NIC ordering comes from the same original body.
grep '^profile device add ' "$MOCK_LOG" >"$2/device-order"
sed -n '1p' "$2/device-order" | grep -q 'default root disk'
sed -n '2p' "$2/device-order" | grep -q 'default eth0 nic'
: >"$MOCK_LOG"
if MOCK_TOKEN=foreign SUBYARD_PREPARED_OWNER_PHASE=1 SUBYARD_PREPARED_OWNER_PROJECT_TOKEN=original-project-token \
 bash "$1/scripts/02-create-project.sh" --owner-after-project --yes >"$2/foreign-output" 2>&1;then exit 62;fi
! grep -Eq '^(project (create|set|unset)|profile device add)' "$MOCK_LOG"
`
	command := exec.Command("bash", "-eu", "-s", "--", repository, root)
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native02 order/refusal changed: %v (%d diagnostic bytes)", err, len(output))
	}
}
