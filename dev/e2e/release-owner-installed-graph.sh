#!/usr/bin/env bash
# One normal installed Rust owner graph on an already allocated disposable VM.
set -euo pipefail

PREFLIGHT=0
CLEANUP_ONLY=0
CURRENT_PARTIAL=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --cleanup) CLEANUP_ONLY=1 ;;
    --preflight) PREFLIGHT=1 ;;
    --current-partial) CURRENT_PARTIAL=1 ;;
    --first-admission|--first-admission-compensation)
      printf 'release-owner-installed-graph: old-source first admission is retired; complete the released Go v0.22.0 bridge before a new Rust goal.\n' >&2
      exit 2 ;;
    *) break ;;
  esac
  shift
done
[ "$#" = 6 ] || { printf 'Usage: %s [--current-partial] [--preflight|--cleanup] TOKEN SOURCE_ASSETS SOURCE_VERSION TARGET_ASSETS TARGET_VERSION TEST_BINARY\n--current-partial uses only TARGET_ASSETS, TARGET_VERSION and TEST_BINARY; source slots are ignored.\n' "$0" >&2; exit 2; }
TOKEN="$1"
GRAPH_SOURCE_ASSETS="$2"
GRAPH_SOURCE_VERSION="$3"
TARGET_ASSETS="$4"
TARGET_VERSION="$5"
TEST_BINARY="$6"
PUBLIC_IMAGE=images:debian/13/cloud
# Reuse existing isolated roots, native environment and bounded marked cleanup.
# Sourcing this file does not run the historical acceptance entrypoint.
# shellcheck source=dev/e2e/release-transition-v0111-recovery.sh
. "$(dirname "${BASH_SOURCE[0]}")/release-transition-v0111-recovery.sh" "$TOKEN"
STATE_ROOT="/var/tmp/subyard-rust-owner-$TOKEN"
SUCCESS_ROOT="$STATE_ROOT/success"
POST_CAS_ROOT="$STATE_ROOT/unused"
MARKER="subyard-rust-owner-$TOKEN"
SUCCESS_YARD=e2e-yard
EVIDENCE="$ROOT/.build/rust-owner-installed-$TOKEN"

die() { printf 'release-owner-installed-graph: %s\n' "$*" >&2; exit 2; }

check_assets() { # directory version role
  local directory="$1" version="$2" role="$3" bundle
  [[ "$directory" = /* ]] && [ -d "$directory" ] && [ ! -L "$directory" ] \
    && [ "$(realpath "$directory")" = "$directory" ] || die 'absolute physical asset directories required'
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+[-A-Za-z0-9.+]*$ ]] || die 'exact package semver required'
  bundle="$directory/subyard-$version-linux-amd64.tar.gz"
  python3 - "$bundle" "$version" "$role" <<'PY'
import hashlib,json,pathlib,sys,tarfile
p=pathlib.Path(sys.argv[1]);version,role=sys.argv[2:]
assert p.is_file() and not p.is_symlink()
h=hashlib.sha256(p.read_bytes()).hexdigest()
assert pathlib.Path(str(p)+'.sha256').read_text().strip()==h+'  '+p.name
m=json.loads(pathlib.Path(str(p)+'.manifest.json').read_bytes())
v=json.loads(pathlib.Path(str(p)+'.provenance.json').read_bytes())
assert m['schemaVersion']==1 and m['kind']=='runtime' and m['version']==version and m['arch']=='amd64' and m['os']=='linux'
assert v['artifact']==p.name and v['sha256']==h and v['version']==version
with tarfile.open(p,'r:gz') as a:
 members={m.name.removeprefix('./'):m for m in a.getmembers()}
 raw=a.extractfile(members['config/release-transition.json']).read()
 r=json.loads(raw)
 if role == 'source':
  assert version=='0.22.0' and h=='11e9174b7cac129ad41167b60864eb30e7b8540d1a57ad10481f7f0fcf19bbe0'
  assert hashlib.sha256(raw).hexdigest()=='e4d70df389a0eaac35bd309f864f69c5609a9e3b395e69334441187a96d49d20'
  assert r['currentEpochs']=={'owner-registration':2,'power-metadata':2,'project-state':1,'settings':3}
  assert 'config/release-owner.json' not in members and 'bin/subyard-release-owner' not in members
 else:
  assert r['currentEpochs']['owner-registration']==2 and r['currentEpochs']['power-metadata']==2
  d=json.load(a.extractfile(members['config/release-owner.json']))
  assert d['kind']=='rust-release-owner' and d['owner']=='bin/subyard-release-owner' and d['helper']=='bin/yard-engine'
PY
}

install_graph_bundle() { # directory version [--publish-only]
  local directory="$1" version="$2" bundle
  bundle="$directory/subyard-$version-linux-amd64.tar.gz"
  fixture_env bash "$TARGET_ASSETS/subyard-install-runtime-release.sh" \
    --runtime-root "$RUNTIME_ROOT" --bundle "$bundle" --checksum "$bundle.sha256" \
    --manifest "$bundle.manifest.json" --provenance "$bundle.provenance.json" "${@:3}"
}

assert_product_project() { # exact known project
  local project="$1" installation uid data config yard uuid
  case "$project" in subyard-e2e-yard|subyard-test-yard|subyard-partial-yard) ;; *) return 1 ;; esac
  installation="$(incus project get "$project" user.subyard.installation)" || return 1
  uid="$(incus project get "$project" user.subyard.operator_uid)" || return 1
  data="$(incus project get "$project" user.subyard.data_home)" || return 1
  config="$(incus project get "$project" user.subyard.config_home)" || return 1
  if [ "$installation" = 1 ] && [ "$uid" = "$(id -u)" ] \
    && [ "$data" = "$DATA_ROOT" ] && [ "$config" = "$CONFIG_HOME" ]; then return 0; fi
  # Authentic legacy releases own fixed instances without installation metadata.
  case "$project" in subyard-e2e-yard) yard=e2e-yard ;; subyard-test-yard) yard=test-yard ;; *) return 1 ;; esac
  assert_fixture_root || return 1
  incus project list --format=json | jq -e --arg project "$project" '
    [.[] | select(.name==$project)] | length==1 and (.[0].config | type=="object" and
      ((has("user.subyard.installation") or has("user.subyard.operator_uid") or
       has("user.subyard.data_home") or has("user.subyard.config_home")) | not))' >/dev/null || return 1
  [ "$(incus config get "yard-$yard" user.subyard.managed --project "$project")" = true ] \
    && [ "$(incus config get "yard-$yard" user.subyard.name --project "$project")" = "$yard" ] \
    && [ "$(incus config get "yard-$yard" user.subyard.initialized --project "$project")" = true ] || return 1
  uuid="$(incus config get "yard-$yard" volatile.uuid --project "$project")" || return 1
  [ -n "$uuid" ]
}

mark_product_project() { # cleanup marker, only after real product ownership
  local project="$1" old
  assert_fixture_root && assert_product_project "$project" || return 1
  old="$(incus project get "$project" user.subyard.p0)" || return 1
  [ -z "$old" ] || [ "$old" = "$FIXTURE_MARKER" ] || return 1
  incus project set "$project" "user.subyard.p0=$FIXTURE_MARKER"
}

graph_project_exists() { # 0 present, 1 absent, 2 unavailable; never infer absence from failed I/O
  local inventory project="$1"
  inventory="$(incus project list --format=json)" || return 2
  jq -e 'type=="array" and all(.[]; (.name|type)=="string")' <<< "$inventory" >/dev/null || return 2
  if jq -e --arg name "$project" 'any(.[]; .name==$name)' <<< "$inventory" >/dev/null; then return 0; fi
  return 1
}

partial_mount_identity() { # fixed marker-owned mount, never a caller-selected path
  assert_fixture_root
  sudo -n python3 - "$FIXTURE_ROOT/partial-host/host-codex-rules" "$TOKEN" <<'PYMOUNT'
import json,os,pathlib,stat,sys
p=pathlib.Path(sys.argv[1]);token=sys.argv[2]
for parent in (p.parent,p):
    s=parent.lstat();assert stat.S_ISDIR(s.st_mode)
s=p.stat();assert stat.S_IMODE(s.st_mode)==0o755 and s.st_uid==s.st_gid==1001
m=p/'.subyard-partial-marker';ms=m.lstat()
assert stat.S_ISREG(ms.st_mode) and ms.st_uid==0 and stat.S_IMODE(ms.st_mode)==0o600 and ms.st_nlink==1
assert m.read_bytes()==('subyard-rust-owner-'+token+':partial\n').encode()
rows=[]
for row in pathlib.Path('/proc/self/mountinfo').read_text().splitlines():
    left,right=row.split(' - ');a,b=left.split(),right.split()
    if a[4]==str(p):
        assert a[3]=='/' and b[0]=='tmpfs' and b[1]=='subyard-installed-partial-'+token
        rows.append(dict(id=a[0],majorMinor=a[2],source=b[1],device=s.st_dev,inode=s.st_ino,uid=s.st_uid,gid=s.st_gid,mode=s.st_mode))
assert len(rows)==1
print(json.dumps(rows[0],sort_keys=True))
PYMOUNT
}

assert_graph_test_log() { # the selected real test must run, with no skip
  local name="$1" log="$2"
  grep -Eq "^--- PASS: $name \([0-9.]+s\)$" "$log" \
    && grep -Fxq PASS "$log" && ! grep -Eq '^--- SKIP:|^SKIP$' "$log" \
    || die 'required installed graph test did not pass completely'
}

prepare_current_partial_yard() {
  # No preceding test process retains helpers or original owner OFDs.
  local launcher="$RUNTIME_ROOT/current/bin/yard" mount="$FIXTURE_ROOT/partial-host/host-codex-rules" key value
  if graph_project_exists subyard-partial-yard; then die 'partial project already exists'; else
    [ "$?" = 1 ] || die 'partial project absence is unknown'
  fi
  assert_fixture_root
  # Register the local fixture once with its coupled path settings coherent.
  # Ordinary config authoring requires this yard to exist before its first set.
  python3 - "$CONFIG_HOME/yards" "$FIXTURE_ROOT/partial-host" <<'PYREGISTER'
import os,pathlib,shlex,stat,sys
parent=pathlib.Path(sys.argv[1]);s=parent.lstat()
assert stat.S_ISDIR(s.st_mode) and s.st_uid==os.getuid()
yard=parent/'partial-yard'
os.mkdir(yard,0o700);yard.chmod(0o700)
fd=os.open(yard/'config.env',os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
os.fchmod(fd,0o600)
with os.fdopen(fd,'w') as out:
    for key in ('HOST_BASE','RESTRICTED_DISK_PATHS'):
        out.write(key+'='+shlex.quote(sys.argv[2])+'\n')
PYREGISTER
  for key in ENVIRONMENT_PROFILES CODING_TOOL_INTEGRATIONS DEV_UID YARD_IMAGE YARD_IMAGE_FALLBACK HOST_MOUNTS SSH_PORT; do
    case "$key" in
      ENVIRONMENT_PROFILES) value='' ;;
      CODING_TOOL_INTEGRATIONS) value=codex ;;
      DEV_UID) value=1001 ;;
      YARD_IMAGE|YARD_IMAGE_FALLBACK) value="$PUBLIC_IMAGE" ;;
      HOST_MOUNTS) value='host-codex-rules:/home/dev/.codex/rules:rw:0755
host-agent-sessions:/mnt/host/agent-sessions:rw:0700' ;;
      SSH_PORT) value=2293 ;;
    esac
    fixture_env "$launcher" -Y partial-yard config --yes set "$key" "$value" --scope yard --local
  done
  [ ! -e "$mount" ] && [ ! -L "$mount" ] || die 'partial mount already exists'
  install -d -m 0700 "$FIXTURE_ROOT/partial-host"
  sudo -n install -d -m 0755 -o 1001 -g 1001 "$mount"
  sudo -n mount -t tmpfs -o size=64k,mode=0755,uid=1001,gid=1001,nosuid,nodev "subyard-installed-partial-$TOKEN" "$mount"
  printf '%s\n' "$MARKER:partial" | sudo -n tee "$mount/.subyard-partial-marker" >/dev/null
  sudo -n chown root:root "$mount/.subyard-partial-marker"
  sudo -n chmod 0600 "$mount/.subyard-partial-marker"
  partial_mount_identity > "$FIXTURE_ROOT/partial-mount.json"
  chmod 0600 "$FIXTURE_ROOT/partial-mount.json"
  # Real native Codex provisioning may download its public release. Do not skip it.
  fixture_env timeout --kill-after=30s 900s "$launcher" -Y partial-yard init --yes > "$FIXTURE_ROOT/partial-init.log" 2>&1
  mark_product_project subyard-partial-yard || die 'partial ordinary init ownership differs'
  fixture_env timeout --kill-after=30s 300s "$launcher" -Y partial-yard start --yes > "$FIXTURE_ROOT/partial-start.log" 2>&1
  fixture_env timeout --kill-after=30s 120s "$launcher" -Y partial-yard check > "$FIXTURE_ROOT/partial-check.log" 2>&1
  [ "$(incus config device get yard-partial-yard host-codex-rules source --project subyard-partial-yard)" = "$mount" ] \
    && [ "$(incus config device get yard-partial-yard host-codex-rules path --project subyard-partial-yard)" = /home/dev/.codex/rules ] \
    && [ "$(incus config device get yard-partial-yard host-codex-rules shift --project subyard-partial-yard)" = true ] \
    && [ "$(incus exec yard-partial-yard --project subyard-partial-yard -- id -u dev)" = 1001 ] \
    && [ "$(incus exec yard-partial-yard --project subyard-partial-yard -- stat -c %u:%g /home/dev/.codex/rules/repo.rules)" = 1001:1001 ] \
    || die 'ordinary declared idmapped rules mount differs'
  [ "$(partial_mount_identity)" = "$(cat "$FIXTURE_ROOT/partial-mount.json")" ] || die 'initial mount authority changed'
  incus config get yard-partial-yard volatile.uuid --project subyard-partial-yard > "$FIXTURE_ROOT/partial-instance.uuid"
  [ -s "$FIXTURE_ROOT/partial-instance.uuid" ] || die 'original partial UUID missing'
  fixture_env timeout --kill-after=30s 180s "$launcher" -Y partial-yard stop --yes > "$FIXTURE_ROOT/partial-stop.log" 2>&1
  python3 - "$RUNTIME_ROOT/current/config/agents/codex/config.toml" "$FIXTURE_ROOT" <<'PYSOURCE'
import os,pathlib,sys,tomllib
root=pathlib.Path(sys.argv[2]);source=pathlib.Path(sys.argv[1]).read_text()
assert source.count('fullscreen_transcript = false')==1
source=source.replace('fullscreen_transcript = false','fullscreen_transcript = true')
assert tomllib.loads(source)['tui']['fullscreen_transcript'] is True
for name,data in [('partial-config-source.toml',source.encode()),('partial-rules-source.rules',b'# public partial recovery rules\n'+b'# ordinary public fixture comment\n'*8192)]:
    if name.endswith('.rules'):data=(data+b'#'* (256*1024))[:256*1024-1]+b'\n'
    f=os.open(root/name,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(f,'wb') as out:out.write(data)
PYSOURCE
  {
    fixture_env "$launcher" -Y partial-yard config --yes import AGENT_codex_CONFIG "$FIXTURE_ROOT/partial-config-source.toml" --scope yard --local
    fixture_env "$launcher" -Y partial-yard config --yes import AGENT_codex_RULES "$FIXTURE_ROOT/partial-rules-source.rules" --scope yard --local
  } > "$FIXTURE_ROOT/partial-import.log" 2>&1
  # Ordinary start does not refresh configs; original before-state remains live.
  fixture_env timeout --kill-after=30s 300s "$launcher" -Y partial-yard start --yes > "$FIXTURE_ROOT/partial-restart.log" 2>&1
}

cleanup_installed_graph() {
  local scenario_rc=$? capture_rc=0 native_rc=0 fallback_rc=0 launcher yard project name names rc mount_identity cleanup_target cleanup_launcher
  trap - EXIT INT TERM
  set +e
  if [ "$CLEANUP_ONLY" != 1 ]; then
    # The controller must verify and fsync original evidence before its separate
    # --cleanup invocation acknowledges permission to destroy fixture state.
    python3 "$ROOT/dev/e2e/publication-capture.py" installed-collect "$TOKEN" || capture_rc=$?
    printf 'INSTALLED_GRAPH_OUTCOME scenario_exit=%s capture_exit=%s fixture_cleanup=deferred\n' \
      "$scenario_rc" "$capture_rc" >&2
    [ "$scenario_rc" != 0 ] || [ "$capture_rc" = 0 ] || scenario_rc=3
    exit "$scenario_rc"
  fi
  install -d -m 0700 "$EVIDENCE" || capture_rc=1
  for name in native-teardown.log fallback-cleanup.log cleanup-summary.json; do
    install -m 0600 /dev/null "$EVIDENCE/$name" || capture_rc=1
  done
  if [ "$CLEANUP_ARMED" = 1 ]; then
    assert_state_root && assert_fixture_root || { printf 'unsafe fixture cleanup root\n' >&2; exit 3; }
    launcher="$(readlink -f "$RUNTIME_ROOT/current/bin/yard")"
    case "$launcher" in "$RUNTIME_ROOT"/releases/*/bin/yard) ;; *) launcher= ;; esac
    for yard in partial-yard test-yard e2e-yard; do
      project="subyard-$yard"
      if graph_project_exists "$project"; then :; else
        rc=$?
        [ "$rc" = 1 ] || { printf 'cleanup phase=project-presence exit=%s\n' "$rc" >> "$EVIDENCE/fallback-cleanup.log"; fallback_rc=3; }
        continue
      fi
      # A failed init may leave an unowned partial project. Preserve it for the
      # independent disposable-VM release; never fabricate ownership to delete it.
      if [ "$yard" = partial-yard ] && [ "$(incus project get "$project" user.subyard.p0)" != "$FIXTURE_MARKER" ]; then
        native_rc=3; fallback_rc=3; continue
      fi
      if ! mark_product_project "$project"; then native_rc=3; fallback_rc=3; continue; fi
      if [ -n "$launcher" ] && [ -x "$launcher" ]; then
        fixture_env timeout --kill-after=15s 180s "$launcher" -Y "$yard" teardown --yes \
          >> "$EVIDENCE/native-teardown.log" 2>&1
        rc=$?
        [ "$rc" = 0 ] || native_rc="$rc"
      else
        native_rc=3
      fi
      if graph_project_exists "$project"; then :; else
        rc=$?
        [ "$rc" = 1 ] || { printf 'cleanup phase=project-presence exit=%s\n' "$rc" >> "$EVIDENCE/fallback-cleanup.log"; fallback_rc=3; }
        continue
      fi
      if ! assert_product_project "$project" \
        || [ "$(incus project get "$project" user.subyard.p0)" != "$FIXTURE_MARKER" ]; then
        fallback_rc=3
        continue
      fi
      names="$(incus list --project "$project" --format csv -c n)" || { fallback_rc=3; continue; }
      rc=0
      while IFS= read -r name; do
        [ -n "$name" ] || continue
        if [ "$yard" = partial-yard ] && { [ "$name" != yard-partial-yard ]           || [ ! -f "$FIXTURE_ROOT/partial-instance.uuid" ]           || [ "$(incus config get "$name" volatile.uuid --project "$project")" != "$(cat "$FIXTURE_ROOT/partial-instance.uuid")" ]; }; then rc=3; break; fi
        if [ "$(incus config get "$name" user.subyard.managed --project "$project")" != true ]; then rc=3; break; fi
        incus delete "$name" --project "$project" --force >> "$EVIDENCE/fallback-cleanup.log" 2>&1 || { rc=3; break; }
      done <<< "$names"
      if [ "$rc" != 0 ] || ! incus project delete "$project" >> "$EVIDENCE/fallback-cleanup.log" 2>&1; then fallback_rc=3; fi
    done
    # Reuse the owning unit guards only after all exact projects are observed
    # absent. Unknown/partial cleanup preserves all roots for VM release.
    for project in subyard-e2e-yard subyard-test-yard subyard-partial-yard; do
      if graph_project_exists "$project"; then
        printf 'cleanup phase=project-remains exit=0\n' >> "$EVIDENCE/fallback-cleanup.log"; fallback_rc=3
      else
        rc=$?
        [ "$rc" = 1 ] || { printf 'cleanup phase=project-presence exit=%s\n' "$rc" >> "$EVIDENCE/fallback-cleanup.log"; fallback_rc=3; }
      fi
    done
    if [ -e "$FIXTURE_ROOT/partial-mount.json" ]; then
      if [ "$capture_rc" = 0 ] && [ "$native_rc" = 0 ] && [ "$fallback_rc" = 0 ] \
        && mount_identity="$(partial_mount_identity)" && [ "$mount_identity" = "$(cat "$FIXTURE_ROOT/partial-mount.json")" ]; then
        # shellcheck disable=SC2024 # The fixture user owns the log; only unmount needs privilege.
        sudo -n umount "$FIXTURE_ROOT/partial-host/host-codex-rules" >> "$EVIDENCE/fallback-cleanup.log" 2>&1 \
          || { rc=$?; printf 'cleanup phase=unmount exit=%s\n' "$rc" >> "$EVIDENCE/fallback-cleanup.log"; fallback_rc=3; }
      else
        printf 'cleanup phase=mount-identity-or-prerequisite exit=1\n' >> "$EVIDENCE/fallback-cleanup.log"; fallback_rc=3
      fi
    elif mountpoint -q "$FIXTURE_ROOT/partial-host/host-codex-rules"; then
      printf 'cleanup phase=unrecorded-mount exit=0\n' >> "$EVIDENCE/fallback-cleanup.log"
      fallback_rc=3
    fi
    if [ "$capture_rc" = 0 ] && [ "$native_rc" = 0 ] && [ "$fallback_rc" = 0 ]; then
      # Selected teardown preserves shared services for registered, projectless
      # yards. Remove this isolated installation through its ordinary owner only
      # after the original tmpfs is unmounted; full removal may delete its roots.
      if cleanup_target="$(install_graph_bundle "$TARGET_ASSETS" "$TARGET_VERSION" --publish-only 2>> "$EVIDENCE/fallback-cleanup.log")"; then
        case "$cleanup_target" in
          releases/"$TARGET_VERSION"-*) cleanup_launcher="$RUNTIME_ROOT/$cleanup_target/bin/yard" ;;
          *) printf 'cleanup phase=full-target exit=1\n' >> "$EVIDENCE/fallback-cleanup.log"; fallback_rc=3 ;;
        esac
      else
        rc=$?; printf 'cleanup phase=full-publish exit=%s\n' "$rc" >> "$EVIDENCE/fallback-cleanup.log"; fallback_rc=3
      fi
      if [ "$fallback_rc" = 0 ]; then
        ( prepare_operator_user_bus ) >> "$EVIDENCE/fallback-cleanup.log" 2>&1 \
          || { rc=$?; printf 'cleanup phase=operator-user-bus exit=%s\n' "$rc" >> "$EVIDENCE/fallback-cleanup.log"; fallback_rc=3; }
      fi
      if [ "$fallback_rc" = 0 ] && [ -x "$cleanup_launcher" ]; then
        fixture_env timeout --kill-after=15s 180s "$cleanup_launcher" -Y default teardown --full --yes \
          >> "$EVIDENCE/fallback-cleanup.log" 2>&1 \
          || { rc=$?; printf 'cleanup phase=full-teardown exit=%s\n' "$rc" >> "$EVIDENCE/fallback-cleanup.log"; fallback_rc=3; }
      elif [ "$fallback_rc" = 0 ]; then
        printf 'cleanup phase=full-launcher exit=1\n' >> "$EVIDENCE/fallback-cleanup.log"; fallback_rc=3
      fi
    fi
    if [ "$capture_rc" = 0 ] && [ "$native_rc" = 0 ] && [ "$fallback_rc" = 0 ]; then
      cleanup_test_vms_sink >> "$EVIDENCE/fallback-cleanup.log" 2>&1 \
        || { rc=$?; printf 'cleanup phase=test-vms-sink exit=%s\n' "$rc" >> "$EVIDENCE/fallback-cleanup.log"; fallback_rc=3; }
      cleanup_power_runtime >> "$EVIDENCE/fallback-cleanup.log" 2>&1 \
        || { rc=$?; printf 'cleanup phase=power-runtime exit=%s\n' "$rc" >> "$EVIDENCE/fallback-cleanup.log"; fallback_rc=3; }
    fi
    if [ "$capture_rc" = 0 ] && [ "$native_rc" = 0 ] && [ "$fallback_rc" = 0 ]; then
      # shellcheck disable=SC2024 # The fixture user owns the log; only find needs privilege.
      sudo -n find "$FIXTURE_ROOT" -depth -delete >> "$EVIDENCE/fallback-cleanup.log" 2>&1 || fallback_rc=3
      [ "$fallback_rc" != 0 ] || find "$STATE_ROOT" -depth -delete >> "$EVIDENCE/fallback-cleanup.log" 2>&1 || fallback_rc=3
    fi
  fi
  find "$EVIDENCE" -type d -exec chmod 0700 {} + || capture_rc=1
  find "$EVIDENCE" -type f -exec chmod 0600 {} + || capture_rc=1
  printf '{"scenarioExit":%d,"captureExit":%d,"nativeCleanupExit":%d,"fallbackCleanupExit":%d,"leaseRelease":"controller-owned-not-observed"}\n' \
    "$scenario_rc" "$capture_rc" "$native_rc" "$fallback_rc" > "$EVIDENCE/cleanup-summary.json" || capture_rc=1
  printf 'INSTALLED_GRAPH_CLEANUP capture_exit=%s native_exit=%s fallback_exit=%s\n' \
    "$capture_rc" "$native_rc" "$fallback_rc" >&2
  [ "$capture_rc" = 0 ] && [ "$native_rc" = 0 ] && [ "$fallback_rc" = 0 ] || exit 3
  exit "$scenario_rc"
}

run_current_partial_graph() { # exact already-active target ID
  local target_id="$1"
  ( prepare_current_partial_yard ) >> "$FIXTURE_ROOT/partial-setup.log" 2>&1
  fixture_env env SUBYARD_RUST_INSTALLED_GRAPH_TOKEN="$TOKEN" SUBYARD_RUST_INSTALLED_GRAPH_TARGET="$target_id" \
    SUBYARD_RUST_INSTALLED_GRAPH_CAPTURE_HELPER="$ROOT/dev/e2e/publication-capture.py" \
    "$TEST_BINARY" -test.run '^TestRustOwnerInstalledCurrentPartialRecovery$' -test.v -test.timeout 15m \
    > "$FIXTURE_ROOT/partial-rust-graph.log" 2>&1
  assert_graph_test_log TestRustOwnerInstalledCurrentPartialRecovery "$FIXTURE_ROOT/partial-rust-graph.log"
}

assert_source_ready() { # exact source release link, after ordinary Go migration
  local source_relative="$1" ready
  [ "$(readlink "$RUNTIME_ROOT/current")" = "$source_relative" ] \
    && [ ! -e "$RUNTIME_ROOT/previous" ] && [ ! -L "$RUNTIME_ROOT/previous" ] || die 'clean Go bridge links differ'
  ready="$(fixture_env timeout --kill-after=30s 120s "$RUNTIME_ROOT/current/bin/yard" migrate --check --json)"
  printf '%s\n' "$ready" > "$FIXTURE_ROOT/ready-a.json"
  python3 - "$FIXTURE_ROOT/ready-a.json" "$JOURNAL" "$RUNTIME_ROOT/current" "${source_relative#releases/}" <<'PYSOURCEREADY'
import hashlib,json,pathlib,sys
ready,journal=map(lambda p:json.loads(pathlib.Path(p).read_bytes()),sys.argv[1:3])
root=pathlib.Path(sys.argv[3]);source=sys.argv[4]
raw=(root/'config/release-transition.json').read_bytes();registry=json.loads(raw)
assert ready['current']==ready['owner']==source
assert ready['outcome']['status']=='ready' and ready['outcome']['reachedGoal'] is True
assert ready['outcome']['active']==ready['outcome']['target']==source and not ready.get('blockers')
assert journal['checkpoint']=='complete' and not journal.get('sourceIngress')
assert journal['goal']=={'target':source,'direction':'activate-target'}
assert journal['releases']['from']==journal['releases']['target']==source and not journal['releases'].get('previous')
assert journal['artifactDigest']==hashlib.sha256((root/'runtime-files.sha256').read_bytes()).hexdigest()
assert journal['registryDigest']==hashlib.sha256(raw).hexdigest()
assert all(step['checkpoint']=='verified' for step in journal['steps'])
assert ready['journal']['checkpoint']=='complete' and ready['journal']['transaction']==journal['transaction']
assert {d['domain']:d['epoch'] for d in ready['domains']}==registry['currentEpochs']
assert all(d['epoch']==d['requiredEpoch'] and not d['pending'] for d in ready['domains'])
PYSOURCEREADY
}

main_installed_graph() {
  valid_token "$TOKEN" || die 'numeric allocation token required'
  if [ "$CURRENT_PARTIAL" != 1 ]; then
    [ "$TARGET_VERSION" != "$GRAPH_SOURCE_VERSION" ] || die 'distinct honest package versions required'
    check_assets "$GRAPH_SOURCE_ASSETS" "$GRAPH_SOURCE_VERSION" source
  fi
  check_assets "$TARGET_ASSETS" "$TARGET_VERSION" target
  [[ "$TEST_BINARY" = /* ]] && [ -x "$TEST_BINARY" ] && [ -f "$TEST_BINARY" ] \
    && [ ! -L "$TEST_BINARY" ] && [ "$(realpath "$TEST_BINARY")" = "$TEST_BINARY" ] || die 'absolute normal compiled package test required'
  [ "$(cat "$TEST_BINARY.sha256")" = "$(sha256sum "$TEST_BINARY" | awk '{print $1}')  $(basename "$TEST_BINARY")" ] || die 'compiled test checksum differs'
  [ "$(cat "$TARGET_ASSETS/subyard-install-runtime-release.sh.sha256")" = "$(sha256sum "$TARGET_ASSETS/subyard-install-runtime-release.sh" | awk '{print $1}')  subyard-install-runtime-release.sh" ] || die 'runtime installer checksum differs'
  if [ "$PREFLIGHT" = 1 ]; then
    printf 'ok: installed owner asset and test prerequisites verified; VM producer not run\n'
    return
  fi
  [ "${SUBYARD_E2E_VM:-}" = 1 ] && [ -n "${SUBYARD_E2E_RUN_ID:-}" ] || die 'run only through the allocated VM1 transport'
  [ "$(id -u)" != 0 ] || die 'run as the allocated dev user'
  incus info >/dev/null
  [ ! -e "$STATE_ROOT" ] && [ ! -L "$STATE_ROOT" ] || die 'fixture root already exists'
  local project rc
  for project in subyard-e2e-yard subyard-test-yard subyard-partial-yard; do
    if graph_project_exists "$project"; then die 'fixture project already exists'; else
      rc=$?
      [ "$rc" = 1 ] || die 'fixture project availability is unknown'
    fi
  done
  trap cleanup_installed_graph EXIT
  trap 'handle_signal 130' INT
  trap 'handle_signal 143' TERM
  install -d -m 0700 "$STATE_ROOT" "$SUCCESS_ROOT"
  printf '%s\n' "$MARKER" > "$STATE_ROOT/.subyard-p0-marker"
  if [ "$CURRENT_PARTIAL" != 1 ]; then SUCCESS_YARD=test-yard; fi
  select_fixture success
  printf '%s\n' "$FIXTURE_MARKER" > "$FIXTURE_ROOT/.subyard-p0-marker"
  chmod 0600 "$STATE_ROOT/.subyard-p0-marker" "$FIXTURE_ROOT/.subyard-p0-marker"
  CLEANUP_ARMED=1
  SUCCESS_ARMED=1
  install -d -m 0700 "$HOME_ROOT" "$DATA_ROOT" "$CONFIG_HOME"
  python3 - "$FIXTURE_ROOT/fixture-assets.json" "$GRAPH_SOURCE_ASSETS" "$GRAPH_SOURCE_VERSION" "$TARGET_ASSETS" "$TARGET_VERSION" "$CURRENT_PARTIAL" <<'PY'
import hashlib,json,os,pathlib,sys
assets={}
for role,offset in ((("target",4),) if sys.argv[6]=="1" else (("source",2),("target",4))):
    directory,version=sys.argv[offset:offset+2]
    archive=pathlib.Path(directory)/("subyard-"+version+"-linux-amd64.tar.gz")
    with archive.open('rb') as source:
        assets[role]={"version":version,"archiveSHA256":hashlib.file_digest(source,'sha256').hexdigest()}
with open(sys.argv[1],'x') as output:
    os.fchmod(output.fileno(),0o600)
    json.dump(assets,output)
PY
  local launcher="$RUNTIME_ROOT/current/bin/yard" source_relative target_relative target_id ready source_uuid
  if [ "$CURRENT_PARTIAL" = 1 ]; then
    # Bootstrap the real current owner independently of the released-Go source graph.
    (
      install_graph_bundle "$TARGET_ASSETS" "$TARGET_VERSION"
      [ "$(fixture_env "$launcher" --version)" = "yard $TARGET_VERSION" ] || die 'installed target engine version differs'
      target_relative="$(readlink "$RUNTIME_ROOT/current")"
      case "$target_relative" in releases/"$TARGET_VERSION"-*) ;; *) die 'active target identity differs' ;; esac
      fixture_env timeout --kill-after=30s 300s "$launcher" migrate --yes
      ready="$(fixture_env timeout --kill-after=30s 120s "$launcher" migrate --check --json)"
      printf '%s\n' "$ready"
      jq -e '.outcome.status=="ready" and .outcome.reachedGoal and all(.domains[]; .epoch==.requiredEpoch)' <<< "$ready" >/dev/null || die 'current target bootstrap is not Ready'
      [ "$(readlink "$RUNTIME_ROOT/current")" = "$target_relative" ] || die 'bootstrap changed target identity'
      jq -e '.checkpoint=="complete"' "$JOURNAL" >/dev/null || die 'current target bootstrap did not produce genuine COMPLETE'
      install -d -m 0700 "$CONFIG_HOME/yards"
    ) > "$FIXTURE_ROOT/partial-setup.log" 2>&1
    target_relative="$(readlink "$RUNTIME_ROOT/current")"
    case "$target_relative" in releases/"$TARGET_VERSION"-*) ;; *) die 'active target identity differs' ;; esac
    target_id="${target_relative#releases/}"
    run_current_partial_graph "$target_id"
    printf 'ok: current normal Rust bootstrap and genuine current partial Resume Ready; released-Go source not run\n'
    return
  fi
  install_graph_bundle "$GRAPH_SOURCE_ASSETS" "$GRAPH_SOURCE_VERSION" > "$FIXTURE_ROOT/install-a.log" 2>&1
  [ "$(fixture_env "$launcher" --version)" = "yard $GRAPH_SOURCE_VERSION" ] || die 'installed Go bridge engine version differs'
  source_relative="$(readlink "$RUNTIME_ROOT/current")"
  [ "$source_relative" = releases/0.22.0-11e9174b7cac ] || die 'authenticated Go bridge identity differs'
  fixture_env timeout --kill-after=30s 300s "$launcher" migrate --yes > "$FIXTURE_ROOT/migrate-a.log" 2>&1
  assert_source_ready "$source_relative"
  install -d -m 0700 "$CONFIG_HOME/yards/$YARD_NAME"
  printf '%s\n' 'ENVIRONMENT_PROFILES=' 'CODING_TOOL_INTEGRATIONS=' > "$CONFIG_HOME/config.env"
  printf '%s\n' 'YARD_KIND=container' 'SSH_PORT=2291' 'DEV_UID=1001' \
    "YARD_IMAGE=$PUBLIC_IMAGE" "YARD_IMAGE_FALLBACK=$PUBLIC_IMAGE" \
    'ENVIRONMENT_PROFILES=test-vms' 'CODING_TOOL_INTEGRATIONS=' 'NESTED_E2E_VMS=1' \
    > "$CONFIG_HOME/yards/$YARD_NAME/config.env"
  chmod 0600 "$CONFIG_HOME/config.env" "$CONFIG_HOME/yards/$YARD_NAME/config.env"
  # Resolve the declared public image in the product project's own namespace.
  fixture_env timeout --signal=TERM --kill-after=5s 60s incus image info "$PUBLIC_IMAGE" > "$FIXTURE_ROOT/image-info.txt"
  fixture_env timeout --signal=TERM --kill-after=5s 60s incus image list images: --format=json > "$FIXTURE_ROOT/image-catalog.json"
  jq -e 'any(.[]; (.type=="container" or .type=="") and .architecture=="x86_64" and any((.aliases // [])[]; .name=="debian/13/cloud"))' \
    "$FIXTURE_ROOT/image-catalog.json" >/dev/null || die 'declared public container source is unavailable for amd64'
  fixture_env timeout --kill-after=30s 900s "$launcher" -Y "$YARD_NAME" init --yes > "$FIXTURE_ROOT/init-a.log" 2>&1
  mark_product_project "$PROJECT" || die 'ordinary Go bridge init did not publish exact product ownership'
  fixture_env timeout --kill-after=30s 300s "$launcher" -Y "$YARD_NAME" start --yes > "$FIXTURE_ROOT/start-a.log" 2>&1
  fixture_env timeout --kill-after=30s 120s "$launcher" -Y "$YARD_NAME" check > "$FIXTURE_ROOT/check-a.log" 2>&1
  fixture_env timeout --kill-after=30s 300s "$launcher" migrate --yes >> "$FIXTURE_ROOT/migrate-a.log" 2>&1
  assert_source_ready "$source_relative"
  grep -Fxq 'NESTED_E2E_VMS=1' "$CONFIG_HOME/yards/$YARD_NAME/config.env" || die 'Go bridge lost the enabled test-VM profile'
  source_uuid="$(incus config get yard-test-yard volatile.uuid --project subyard-test-yard)"
  [ -n "$source_uuid" ] || die 'Go bridge native instance UUID is missing'
  printf '%s\n' "$source_uuid" > "$FIXTURE_ROOT/source-instance.uuid"
  chmod 0600 "$FIXTURE_ROOT/source-instance.uuid"
  cp "$RUNTIME_ROOT/current/runtime-files.sha256" "$FIXTURE_ROOT/source-a-manifest.before"
  target_relative="$(install_graph_bundle "$TARGET_ASSETS" "$TARGET_VERSION" --publish-only)"
  case "$target_relative" in releases/"$TARGET_VERSION"-*) ;; *) die 'published target identity differs' ;; esac
  target_id="${target_relative#releases/}"
  fixture_env env SUBYARD_RUST_INSTALLED_GRAPH_TOKEN="$TOKEN" SUBYARD_RUST_INSTALLED_GRAPH_TARGET="$target_id" \
    SUBYARD_RUST_INSTALLED_GRAPH_SOURCE="${source_relative#releases/}" \
    SUBYARD_RUST_INSTALLED_GRAPH_CAPTURE_HELPER="$ROOT/dev/e2e/publication-capture.py" \
    "$TEST_BINARY" -test.run '^TestRustOwnerInstalledNormalGraph$' -test.v -test.timeout 15m \
    > "$FIXTURE_ROOT/normal-rust-graph.log" 2>&1
  assert_graph_test_log TestRustOwnerInstalledNormalGraph "$FIXTURE_ROOT/normal-rust-graph.log"
  mark_product_project subyard-test-yard || die 'normal target lost the canonical product project'
  fixture_env timeout --kill-after=30s 120s "$launcher" -Y test-yard check > "$FIXTURE_ROOT/check-target.log" 2>&1
  [ -n "$(incus config get yard-test-yard volatile.uuid --project subyard-test-yard)" ] || die 'actual canonical instance UUID is missing'
  [ "$(incus config get yard-test-yard volatile.uuid --project subyard-test-yard)" = "$source_uuid" ] || die 'normal target replaced the Go bridge instance'
  cmp "$RUNTIME_ROOT/$source_relative/runtime-files.sha256" "$FIXTURE_ROOT/source-a-manifest.before"
  [ "$(sha256sum "$GRAPH_SOURCE_ASSETS/subyard-$GRAPH_SOURCE_VERSION-linux-amd64.tar.gz" | awk '{print $1}')" = 11e9174b7cac129ad41167b60864eb30e7b8540d1a57ad10481f7f0fcf19bbe0 ] || die 'original Go bridge archive changed'
  run_current_partial_graph "$target_id"
  printf 'ok: authentic Go0.22 COMPLETE source, normal Rust Ready and genuine current partial Resume Ready\n'

}

if [ "$CLEANUP_ONLY" = 1 ]; then
  valid_token "$TOKEN" && [ "${SUBYARD_E2E_VM:-}" = 1 ] \
    && [ -n "${SUBYARD_E2E_RUN_ID:-}" ] && [ "$(id -u)" != 0 ] \
    || die 'cleanup requires the allocated fixture user'
  select_fixture success
  CLEANUP_ARMED=1
  cleanup_installed_graph
else
  main_installed_graph
fi
