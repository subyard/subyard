#!/usr/bin/env bash
# Verify and atomically activate a self-contained Subyard runtime bundle.
set -euo pipefail
original_arguments=("$@")

RUNTIME_ROOT="${YARD_RUNTIME_ROOT:-${SUBYARD_HOME:-$HOME/.subyard}/runtime}"
BUNDLE=''; CHECKSUM=''; MANIFEST=''; PROVENANCE=''; ROLLBACK=0; CHECK_ONLY=0; PUBLISH_ONLY=0

while [ $# -gt 0 ]; do
  case "$1" in
    --bundle) [ $# -ge 2 ] || { printf 'install-runtime-release: --bundle needs a path\n' >&2; exit 2; }; BUNDLE="$2"; shift 2 ;;
    --checksum) [ $# -ge 2 ] || { printf 'install-runtime-release: --checksum needs a path\n' >&2; exit 2; }; CHECKSUM="$2"; shift 2 ;;
    --manifest) [ $# -ge 2 ] || { printf 'install-runtime-release: --manifest needs a path\n' >&2; exit 2; }; MANIFEST="$2"; shift 2 ;;
    --provenance) [ $# -ge 2 ] || { printf 'install-runtime-release: --provenance needs a path\n' >&2; exit 2; }; PROVENANCE="$2"; shift 2 ;;
    --runtime-root) [ $# -ge 2 ] || { printf 'install-runtime-release: --runtime-root needs a path\n' >&2; exit 2; }; RUNTIME_ROOT="$2"; shift 2 ;;
    --check) CHECK_ONLY=1; shift ;;
    --publish-only) PUBLISH_ONLY=1; shift ;;
    --rollback) ROLLBACK=1; shift ;;
    *) printf 'install-runtime-release: unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done

case "$RUNTIME_ROOT" in /*) ;; *) printf 'install-runtime-release: runtime root must be absolute\n' >&2; exit 2 ;; esac
[ "$RUNTIME_ROOT" != / ] || { printf 'install-runtime-release: refusing filesystem root\n' >&2; exit 2; }
export YARD_RUNTIME_ROOT="$RUNTIME_ROOT"
if [ "$CHECK_ONLY" = 0 ]; then
  command -v python3 >/dev/null 2>&1 \
    || { printf 'install-runtime-release: Python 3 is required for exact installation ownership\n' >&2; exit 2; }
  # Share the exact account/data-home barrier with CLI/source entrypoints. A
  # Delegated installers receive the exact held descriptor; independent calls
  # wait and recheck their original epoch and target generation.
  owner_barrier=(python3)
  # An independent invocation transfers the whole shell to the barrier. An
  # inherited invocation only validates the descriptor held by its own shell.
  [ -n "${SUBYARD_INSTALL_OWNER_FD:-}" ] || owner_barrier=(exec python3)
  "${owner_barrier[@]}" - "${SUBYARD_HOME:-$HOME/.subyard}" "$RUNTIME_ROOT" "$0" "${original_arguments[@]}" <<'PY_BARRIER'
import fcntl,hashlib,os,pathlib,stat,sys,time
home,runtime,script=sys.argv[1:4];uid=os.getuid()
directory=pathlib.Path("/tmp")/("subyard-owner-locks-"+str(uid))
path=directory/(hashlib.sha256(os.path.normpath(home).encode()).hexdigest()+".lock")
epoch_path=pathlib.Path(str(path)+".epoch")
def epoch():
 try:
  fd=os.open(epoch_path,os.O_RDONLY|os.O_NOFOLLOW)
 except FileNotFoundError:return "0"
 try:
  info=os.fstat(fd);body=os.read(fd,32)
  if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o600 or info.st_uid!=uid or not body.endswith(b"\n") or not body[:-1].isdigit():raise ValueError()
  return body[:-1].decode("ascii")
 finally:os.close(fd)
def identity(target):
 try:info=os.lstat(target)
 except FileNotFoundError:return None
 return (info.st_dev,info.st_ino,info.st_uid,info.st_gid,info.st_mode,info.st_size if not stat.S_ISDIR(info.st_mode) else 0,info.st_mtime_ns if not stat.S_ISDIR(info.st_mode) else 0,info.st_ctime_ns if not stat.S_ISDIR(info.st_mode) else 0)

try:
 original=epoch();targets=[runtime,os.path.join(runtime,"current"),os.path.join(runtime,"installation.json")]
 before=[identity(target) for target in targets]
 try:directory.mkdir(mode=0o700);directory.chmod(0o700)
 except FileExistsError:pass
 directory_info=directory.lstat()
 if not stat.S_ISDIR(directory_info.st_mode) or stat.S_IMODE(directory_info.st_mode)!=0o700 or directory_info.st_uid!=uid:raise ValueError()
 inherited=os.environ.get("SUBYARD_INSTALL_OWNER_FD")
 if inherited:
  fd=int(inherited);info=os.fstat(fd);current=os.lstat(path)
  if not os.path.samestat(info,current) or os.environ.get("SUBYARD_INSTALL_OWNER_EPOCH")!=original:raise ValueError()
 else:
  parent=os.open(directory,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
  try:
   try:fd=os.open(path.name,os.O_RDWR|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600,dir_fd=parent);os.fchmod(fd,0o600)
   except FileExistsError:fd=os.open(path.name,os.O_RDWR|os.O_NOFOLLOW,dir_fd=parent)
  finally:os.close(parent)
  info=os.fstat(fd)
 if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o600 or info.st_uid!=uid:raise ValueError()
 deadline=time.monotonic()+600
 while True:
  try:fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB);break
  except BlockingIOError:
   if inherited or time.monotonic()>=deadline:raise ValueError()
   time.sleep(0.1)
 if epoch()!=original or before!=[identity(target) for target in targets] or os.path.lexists(home+".uninstall"):raise ValueError()
 if inherited:raise SystemExit(0)
 safe_fd=fcntl.fcntl(fd,fcntl.F_DUPFD,10);os.close(fd);os.set_inheritable(safe_fd,True)
 environment=dict(os.environ,SUBYARD_INSTALL_OWNER_FD=str(safe_fd),SUBYARD_INSTALL_OWNER_EPOCH=original)
 os.execvpe("bash",["bash",script]+sys.argv[4:],environment)
except (ValueError,OSError,KeyError):
 print("install-runtime-release: installation ownership changed or full uninstall is pending; rerun the installer after completing recovery",file=sys.stderr)
 raise SystemExit(75)
PY_BARRIER
fi
releases="$RUNTIME_ROOT/releases"; current="$RUNTIME_ROOT/current"; previous="$RUNTIME_ROOT/previous"
install -d -m 0700 "$releases"
if [ "$CHECK_ONLY" = 0 ]; then
  command -v flock >/dev/null 2>&1 \
    || { printf 'install-runtime-release: flock is required\n' >&2; exit 2; }
  exec 9>"$RUNTIME_ROOT/update.lock"
  chmod 0600 "$RUNTIME_ROOT/update.lock"
  flock 9
fi
command -v jq >/dev/null 2>&1 \
  || { printf 'install-runtime-release: jq is required\n' >&2; exit 2; }

safe_link_target() { # <link>
  local value
  [ -L "$1" ] || return 1
  value="$(readlink "$1")"
  case "$value" in releases/*) [ -d "$RUNTIME_ROOT/$value" ] ;; *) return 1 ;; esac
}

activate_link() { # <link> <relative target>
  local link="$1" target="$2" temporary
  temporary="$(mktemp "$RUNTIME_ROOT/.link.XXXXXX")"
  rm -f -- "$temporary"
  ln -s "$target" "$temporary"
  mv -Tf "$temporary" "$link"
}

if [ "$ROLLBACK" = 1 ]; then
  printf 'install-runtime-release: rollback is owned by yard update --rollback\n' >&2
  exit 2
fi
[ "$PUBLISH_ONLY" = 0 ] || [ "$CHECK_ONLY$ROLLBACK" = 00 ] \
  || { printf 'install-runtime-release: publish-only cannot be combined with check or rollback\n' >&2; exit 2; }

[ -n "$BUNDLE" ] && [ -n "$CHECKSUM" ] && [ -n "$MANIFEST" ] && [ -n "$PROVENANCE" ] \
  || { printf 'install-runtime-release: bundle, checksum, manifest and provenance are required\n' >&2; exit 2; }
for release_file in "$BUNDLE" "$CHECKSUM" "$MANIFEST" "$PROVENANCE"; do
  [ -f "$release_file" ] && [ ! -L "$release_file" ] \
    || { printf 'install-runtime-release: release inputs must be regular non-symlink files\n' >&2; exit 2; }
done
command -v sha256sum >/dev/null 2>&1 || { printf 'install-runtime-release: sha256sum is required\n' >&2; exit 2; }

read -r expected _ < "$CHECKSUM" || true
case "$expected" in
  ????????????????????????????????????????????????????????????????) ;;
  *) printf 'install-runtime-release: invalid SHA-256 file\n' >&2; exit 2 ;;
esac
case "$expected" in *[!0-9a-fA-F]*) printf 'install-runtime-release: invalid SHA-256 value\n' >&2; exit 2 ;; esac
actual="$(sha256sum "$BUNDLE" | cut -d' ' -f1)"
[ "${actual,,}" = "${expected,,}" ] \
  || { printf 'install-runtime-release: checksum mismatch\n' >&2; exit 1; }
case "$(uname -m)" in x86_64) host_arch=amd64 ;; aarch64|arm64) host_arch=arm64 ;; *) host_arch=unsupported ;; esac
read -r version < <(jq -er --arg arch "$host_arch" '
  select(.schemaVersion == 1 and .kind == "runtime" and .os == "linux" and .arch == $arch and
    .rpc.min <= 1 and .rpc.max >= 1 and .projectStateSchema == 1 and .credentialSchema == 1 and
    (.migrationSchema // 1) == 1 and
    ((.configLayout // 1) | type == "number" and . >= 1 and floor == .)) |
  .version | select(type == "string" and length > 0)
  ' "$MANIFEST") \
  || { printf 'install-runtime-release: incompatible release manifest\n' >&2; exit 1; }
# The first condition is the v0.1 wire identity; canonicalRepository records the current home.
jq -e --arg artifact "$(basename "$BUNDLE")" --arg sha "${actual,,}" --arg version "$version" '
  .schemaVersion == 1 and .artifact == $artifact and (.sha256 | ascii_downcase) == $sha and
  .version == $version and .sourceRepository == "github.com/Dmitry-Borodin/Subyard" and
  (.canonicalRepository // "github.com/Subyard/Subyard") == "github.com/Subyard/Subyard" and
  (.sourceRevision | type == "string" and length > 0)' "$PROVENANCE" >/dev/null \
  || { printf 'install-runtime-release: provenance does not match the bundle\n' >&2; exit 1; }

if tar -tzf "$BUNDLE" | grep -Eq '(^/|(^|/)\.\.(/|$))'; then
  printf 'install-runtime-release: bundle contains an unsafe path\n' >&2
  exit 1
fi
if tar -tvzf "$BUNDLE" | awk '
  substr($1, 1, 1) != "-" && substr($1, 1, 1) != "d" { invalid=1 }
  END { exit invalid ? 0 : 1 }
'; then
  printf 'install-runtime-release: bundle contains a non-regular entry\n' >&2
  exit 1
fi
release_id="$version-${actual:0:12}"
destination="$releases/$release_id"
candidate="$(mktemp -d "$releases/.candidate.XXXXXX")"
published=0
cleanup_candidate() { [ "$published" = 1 ] || rm -rf -- "$candidate"; }
actual_files=''; listed_files=''
cleanup() {
  [ -z "$actual_files" ] || rm -f -- "$actual_files"
  [ -z "$listed_files" ] || rm -f -- "$listed_files"
  cleanup_candidate
}
trap cleanup EXIT
tar -xzf "$BUNDLE" -C "$candidate" --no-same-owner --no-same-permissions
if find "$candidate" -type l -print -quit | grep -q .; then
  printf 'install-runtime-release: bundle contains a symbolic link\n' >&2
  exit 1
fi
for required in bin/yard bin/yard-engine config/commands.registry config/migrations.json config/release-transition.json scripts/install-runtime-release.sh; do
  [ -f "$candidate/$required" ] && [ ! -L "$candidate/$required" ] \
    || { printf 'install-runtime-release: bundle is missing %s\n' "$required" >&2; exit 1; }
done
[ -f "$candidate/runtime-files.sha256" ] && [ ! -L "$candidate/runtime-files.sha256" ] \
  || { printf 'install-runtime-release: runtime file manifest is missing\n' >&2; exit 1; }
(
  cd "$candidate" &&
  sha256sum -c runtime-files.sha256 >/dev/null
) || { printf 'install-runtime-release: runtime file manifest verification failed\n' >&2; exit 1; }
actual_files="$(mktemp "$releases/.actual-files.XXXXXX")"
listed_files="$(mktemp "$releases/.listed-files.XXXXXX")"
(
  cd "$candidate"
  find . -type f ! -path './runtime-files.sha256' -print | sort > "$actual_files"
  sed -E 's/^[0-9a-fA-F]{64}  //' runtime-files.sha256 | sort > "$listed_files"
)
cmp -s "$actual_files" "$listed_files" \
  || { printf 'install-runtime-release: runtime file manifest is not exact\n' >&2; exit 1; }
rm -f -- "$actual_files" "$listed_files"
chmod 0755 "$candidate/bin/yard" "$candidate/bin/yard-engine"
candidate_version="$(SUBYARD_REPOSITORY_ROOT="$candidate" "$candidate/bin/yard-engine" --version 2>/dev/null | awk '{print $2}')" \
  || { printf 'install-runtime-release: candidate self-check failed\n' >&2; exit 1; }
[ "$candidate_version" = "$version" ] \
  || { printf 'install-runtime-release: candidate version does not match manifest\n' >&2; exit 1; }
if [ "$CHECK_ONLY" = 1 ]; then
  printf 'verified runtime yard %s\n' "$version"
  exit 0
fi

if [ ! -e "$destination" ] && [ ! -L "$destination" ]; then
  if mv -T "$candidate" "$destination"; then
    published=1
  fi
fi
if [ "$published" = 0 ]; then
  [ -d "$destination" ] && [ ! -L "$destination" ] \
    && cmp -s "$candidate/runtime-files.sha256" "$destination/runtime-files.sha256" \
    && ! find "$destination" -type l -print -quit | grep -q . \
    && (
      cd "$destination" &&
      sha256sum -c runtime-files.sha256 >/dev/null &&
      find . -type f ! -path './runtime-files.sha256' -print | sort > "$actual_files" &&
      sed -E 's/^[0-9a-fA-F]{64}  //' runtime-files.sha256 | sort > "$listed_files" &&
      cmp -s "$actual_files" "$listed_files"
    ) \
    || { printf 'install-runtime-release: published release identity conflicts with verified candidate\n' >&2; exit 1; }
  rm -rf -- "$candidate"
  published=1
fi

# Keep an installation boundary even before bootstrap creates any yard or
# external launcher. Only the verified runtime store is mutated here.
installation_inventory="$RUNTIME_ROOT/installation.json"
if [ -e "$installation_inventory" ] || [ -L "$installation_inventory" ]; then
  [ -f "$installation_inventory" ] && [ ! -L "$installation_inventory" ] \
    && [ "$(stat -c '%u' "$installation_inventory")" = "$(id -u)" ] \
    && [ "$(stat -c '%a' "$installation_inventory")" = 600 ] \
    || { printf 'install-runtime-release: unsafe installation ownership inventory\n' >&2; exit 1; }
  jq -e --arg data "${SUBYARD_HOME:-$HOME/.subyard}" \
    --arg config "${SUBYARD_CONFIG_HOME:-${XDG_CONFIG_HOME:-$HOME/.config}/subyard}" \
    --arg runtime "$RUNTIME_ROOT" --argjson uid "$(id -u)" \
    '.schema == 1 and .operatorUid == $uid and .dataHome == $data and .configHome == $config and .runtimeRoot == $runtime' \
    "$installation_inventory" >/dev/null \
    || { printf 'install-runtime-release: runtime ownership conflicts with this installation\n' >&2; exit 1; }

else
  installation_candidate="$(mktemp "$RUNTIME_ROOT/.installation.XXXXXX")"
  jq -n --arg data "${SUBYARD_HOME:-$HOME/.subyard}" \
    --arg config "${SUBYARD_CONFIG_HOME:-${XDG_CONFIG_HOME:-$HOME/.config}/subyard}" \
    --arg runtime "$RUNTIME_ROOT" --arg cache "${YARD_RELEASE_CACHE:-${SUBYARD_HOME:-$HOME/.subyard}/releases}" \
    --argjson uid "$(id -u)" \
    '{schema:1,operatorUid:$uid,dataHome:$data,configHome:$config,runtimeRoot:$runtime,cacheRoot:$cache,binDir:"",shellFiles:[]}' \
    > "$installation_candidate"
  chmod 0600 "$installation_candidate"
  mv -fT -- "$installation_candidate" "$installation_inventory"
  sync -d -- "$installation_inventory"
fi

if [ "$CHECK_ONLY" = 0 ]; then
  # These verified inputs belong to this cache only when the owning installer
  # actually publishes from it. Record native file identity, never infer ownership
  # later from a release-like filename or approve the surrounding directory.
  python3 - "$installation_inventory" "$BUNDLE" "$CHECKSUM" "$MANIFEST" "$PROVENANCE" <<'PY_CACHE'
import json,os,pathlib,stat,sys,tempfile
metadata=pathlib.Path(sys.argv[1]);value=json.loads(metadata.read_bytes())
root=pathlib.Path(value["cacheRoot"])
records={entry["Path"]:entry for entry in value.get("cacheFiles",[])}
for argument in sys.argv[2:]:
 path=pathlib.Path(argument).absolute()
 try:relative=path.relative_to(root).as_posix()
 except ValueError:continue
 info=path.lstat()
 if not stat.S_ISREG(info.st_mode) or info.st_uid!=value["operatorUid"]:raise SystemExit("install-runtime-release: unsafe cache artifact ownership")
 records[relative]={"Path":relative,"Device":info.st_dev,"Inode":info.st_ino,"Mode":info.st_mode,"UID":info.st_uid,"GID":info.st_gid,"Size":info.st_size,"Modified":info.st_mtime_ns,"Changed":info.st_ctime_ns,"Link":""}
if len(records)>4096:raise SystemExit("install-runtime-release: cache ownership inventory is oversized")
value["cacheFiles"]=[records[path] for path in sorted(records)]
fd,pending=tempfile.mkstemp(prefix=".cache-ownership.",dir=metadata.parent)
with os.fdopen(fd,"w") as output:
 os.fchmod(output.fileno(),0o600);json.dump(value,output);output.write("\n");output.flush();os.fsync(output.fileno())
os.replace(pending,metadata)
directory=os.open(metadata.parent,os.O_RDONLY|os.O_DIRECTORY);os.fsync(directory);os.close(directory)
PY_CACHE
fi

if [ "$PUBLISH_ONLY" = 1 ]; then
  printf 'releases/%s\n' "$release_id"
  exit 0
fi

# Initial installation has no release transition source. It may create the
# first current link, but an existing installation is activated only by the Go
# ReleaseTransition module.
if [ -e "$previous" ] || [ -L "$previous" ]; then
  safe_link_target "$previous" \
    || { printf 'install-runtime-release: previous runtime link is unsafe\n' >&2; exit 1; }
fi
if [ -e "$current" ] || [ -L "$current" ]; then
  safe_link_target "$current" \
    || { printf 'install-runtime-release: current runtime link is unsafe\n' >&2; exit 1; }
  [ "$(readlink "$current")" = "releases/$release_id" ] \
    || { printf 'install-runtime-release: existing runtime activation is owned by yard update\n' >&2; exit 2; }
  printf 'runtime yard %s is already installed\n' "$version"
  exit 0
fi
[ ! -e "$previous" ] && [ ! -L "$previous" ] \
  || { printf 'install-runtime-release: previous runtime exists without current\n' >&2; exit 1; }
activate_link "$current" "releases/$release_id" \
  || { printf 'install-runtime-release: could not activate initial runtime\n' >&2; exit 1; }
printf 'installed runtime %s\n' \
  "$(SUBYARD_REPOSITORY_ROOT="$destination" "$destination/bin/yard-engine" --version)"
