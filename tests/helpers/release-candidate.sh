#!/usr/bin/env bash
# Optional packaged-candidate transport for release acceptance fixtures.
release_candidate_prepare() {
  local root="$1" acceptance metadata runtime_file runtime_sha256 archive runtime asset
  acceptance="$root/.subyard-acceptance"
  metadata="$acceptance/candidate.json"
  [ -e "$metadata" ] || [ -L "$metadata" ] || return 1
  [ -d "$acceptance" ] && [ ! -L "$acceptance" ] && [ -d "$acceptance/release" ] && [ ! -L "$acceptance/release" ] \
    || { printf 'release candidate directories must be real directories\n' >&2; return 2; }
  [ -f "$metadata" ] && [ ! -L "$metadata" ] || { printf 'release candidate metadata must be a regular file\n' >&2; return 2; }
  read -r runtime_file runtime_sha256 < <(jq -er '[.runtime_file, .runtime_sha256] | select(length == 2 and all(.[]; type == "string")) | @tsv' "$metadata") || { printf 'release candidate metadata is invalid\n' >&2; return 2; }
  case "$runtime_file" in ''|*/*|.|..) printf 'release candidate runtime path is unsafe\n' >&2; return 2 ;; esac
  [[ "$runtime_sha256" =~ ^[0-9a-f]{64}$ ]] || { printf 'release candidate runtime checksum is invalid\n' >&2; return 2; }
  archive="$acceptance/release/$runtime_file"
  [ -f "$archive" ] && [ ! -L "$archive" ] || { printf 'release candidate runtime archive must be a regular file\n' >&2; return 2; }
  [ "$(sha256sum "$archive" | awk '{print $1}')" = "$runtime_sha256" ] || { printf 'release candidate runtime checksum mismatch\n' >&2; return 2; }
  runtime="$acceptance/runtime"
  python3 - "$archive" "$runtime" <<'PY' || { printf 'release candidate runtime archive is unsafe\n' >&2; return 2; }
import os, shutil, sys, tarfile
archive, runtime = sys.argv[1:]
with tarfile.open(archive, "r:gz") as bundle:
    for member in bundle.getmembers():
        name = member.name
        if name.startswith("/") or ".." in name.split("/") or not (member.isdir() or member.isfile()):
            raise SystemExit(1)
    stage = runtime + ".next"
    shutil.rmtree(stage, ignore_errors=True)
    os.mkdir(stage)
    bundle.extractall(stage, filter="data")
    shutil.rmtree(runtime, ignore_errors=True)
    os.rename(stage, runtime)
PY
  [ -f "$runtime/runtime-files.sha256" ] && [ ! -L "$runtime/runtime-files.sha256" ] \
    || { printf 'release candidate runtime manifest is missing\n' >&2; return 2; }
  (cd "$runtime" && sha256sum -c runtime-files.sha256 >/dev/null) \
    || { printf 'release candidate runtime manifest mismatch\n' >&2; return 2; }
  [ -x "$runtime/bin/yard" ] && [ -x "$runtime/bin/yard-engine" ] || { printf 'release candidate yard executable is missing\n' >&2; return 2; }
  if [ -e "$runtime/config/release-owner.json" ] || [ -L "$runtime/config/release-owner.json" ]; then
    jq -e '. == {schemaVersion:1,kind:"rust-release-owner",protocolVersion:1,
      owner:"bin/subyard-release-owner",helper:"bin/yard-engine",
      schema:"config/release-owner.schema.json",buildInputs:"config/release-owner-build.json"}' \
      "$runtime/config/release-owner.json" >/dev/null \
      && [ -x "$runtime/bin/subyard-release-owner" ] \
      && [ -f "$runtime/config/release-owner.schema.json" ] \
      && [ -f "$runtime/config/release-owner-build.json" ] \
      || { printf 'release candidate Rust owner assets are incomplete\n' >&2; return 2; }
    for asset in config/release-owner.json config/release-owner.schema.json config/release-owner-build.json bin/subyard-release-owner; do
      awk -v path="./$asset" '$2 == path && length($1) == 64 && $1 !~ /[^0-9a-f]/ { found++ } END { exit(found != 1) }' "$runtime/runtime-files.sha256" \
        || { printf 'release candidate Rust owner asset is not manifest-bound\n' >&2; return 2; }
    done
    jq -e --arg image "$(sha256sum "$runtime/bin/subyard-release-owner" | awk '{print $1}')" \
      '.schemaVersion == 1 and .kind == "rust-release-owner-build" and .features == [] and .locked == true and .imageSHA256 == $image' \
      "$runtime/config/release-owner-build.json" >/dev/null \
      || { printf 'release candidate Rust owner build declaration is invalid\n' >&2; return 2; }
  elif [ -e "$runtime/bin/subyard-release-owner" ] || [ -e "$runtime/config/release-owner.schema.json" ] || [ -e "$runtime/config/release-owner-build.json" ]; then
    printf 'release candidate Rust owner declaration is missing\n' >&2; return 2
  fi
  printf '%s\n' "$runtime/bin/yard-engine"
}

# Bootstrap a fresh fixture through the public installer before any yard effects.
# The caller supplies its isolated SUBYARD_CONFIG_HOME and SUBYARD_HOME.
release_candidate_install() {
  local root="$1" state="$2" release version runtime_file arch candidate_rc launcher
  if release_candidate_prepare "$root" >/dev/null; then
    release="$root/.subyard-acceptance/release"
    read -r version runtime_file < <(jq -er '[.version, .runtime_file] |
      select(all(.[]; type == "string")) | @tsv' "$root/.subyard-acceptance/candidate.json") \
      || { printf 'release candidate bootstrap identity is invalid\n' >&2; return 2; }
    case "$version" in ''|*[!A-Za-z0-9._+-]*)
      printf 'release candidate bootstrap version is invalid\n' >&2; return 2 ;;
    esac
    case "$(uname -s)/$(uname -m)" in
      Linux/x86_64) arch=amd64 ;;
      Linux/aarch64|Linux/arm64) arch=arm64 ;;
      *) printf 'release candidate bootstrap platform is unsupported\n' >&2; return 2 ;;
    esac
    [ "$runtime_file" = "subyard-$version-linux-$arch.tar.gz" ] \
      || { printf 'release candidate archive differs from the bootstrap selection\n' >&2; return 2; }
  else
    candidate_rc=$?
    [ "$candidate_rc" = 1 ] || return "$candidate_rc"
    release="$state/release"
    version=1.0.0-candidate.e2e
    "$root/dev/package-engine.sh" --version "$version" --output-dir "$release" >&2 || return
  fi
  env YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$state/releases" \
    YARD_RUNTIME_ROOT="$state/runtime" YARD_BIN_DIR="$state/bin" \
    YARD_SHELL_RC="$state/bashrc" YARD_LOGIN_RC="$state/profile" \
    bash "$release/subyard-install.sh" --version "$version" --yes >&2 || return
  launcher="$state/bin/yard"
  env YARD_RUNTIME_ROOT="$state/runtime" "$launcher" migrate --check --json \
    | jq -e '.outcome.status == "ready"' >/dev/null || return
  printf '%s\n' "$launcher"
}

# Copy authenticated historical inputs; never rebuild current source under an old
# version when a fixture depends on the historical Go owner's behavior.
release_candidate_historical() { # <source-root> <cache-root> <version> <output>
  python3 - "$@" <<'PYHISTORY'
import hashlib, pathlib, runpy, shutil, sys
source, cache, version, output = map(pathlib.Path, sys.argv[1:])
version = str(version)
verifier = runpy.run_path(str(source / "dev/verify-release-upgrades.py"))
pins = {"0.9.1":verifier["LEGACY_BASELINE_SHA256"],
        "0.11.0":verifier["MINIMUM_BASELINE_SHA256"],
        "0.11.1":{"amd64":"900a64625eef382f855e4d126ff9f868bdffeac008da2c9df195476dcad86c2f"}}
if version not in pins:
    raise SystemExit("unsupported historical fixture version")
cached = cache / ("baseline-" + version)
output.mkdir(parents=True, exist_ok=True)
# The existing verifier checks the pinned actual bundle, including cached input.
assets = verifier["baseline_assets"](output, cached if cached.is_dir() else None,
                                     version, pins[version], "amd64")
archive = "subyard-" + version + "-linux-amd64.tar.gz"
for suffix in ("", ".sha256", ".manifest.json", ".provenance.json"):
    shutil.copy2(assets / (archive + suffix), output / (archive + suffix))
if version in verifier["RETIRED_BASELINES"]:
    shutil.copy2(assets / "subyard-install-runtime-release.sh",
                 output / ("subyard-install-runtime-release-" + version + ".sh"))
PYHISTORY
}

# Obtain the journal from the actual historical owner, then stop at its durable
# reconciliation boundary using the existing release verifier's process-cut tool.
release_candidate_seed_historical() { # <source-root> <runtime> <config> <source-release> <yard>
  python3 - "$@" <<'PYSEED'
import hashlib, json, os, pathlib, runpy, shutil, subprocess, sys, tempfile
source, runtime, config, release, yard = sys.argv[1:]
source, runtime, config = map(pathlib.Path, (source, runtime, config))
owner = runtime / "releases" / release
verifier = runpy.run_path(str(source / "dev/verify-release-upgrades.py"))
fixture = pathlib.Path(tempfile.mkdtemp(prefix="subyard-historical-cut-", dir="/tmp"))
try:
    tools = fixture / "tools"
    tools.mkdir(mode=0o700)
    control = fixture / "observations.json"
    control.write_text('{"instances":[]}')
    verifier["write_observation_incus"](tools, control, fixture / "unexpected-command", (owner,))
    for name in "bash dirname uname jq sha256sum tar gzip cp chmod mv mktemp cut install readlink basename ln sed awk sort find env date id getent python3 cat head stat tr grep wc realpath flock rm cmp comm mkdir rmdir sync".split():
        path = shutil.which(name)
        if path and not (tools / name).exists():
            (tools / name).symlink_to(path)
    env = dict(os.environ, PATH=str(tools), SUBYARD_REPOSITORY_ROOT=str(owner),
               YARD_RUNTIME_ROOT=str(runtime), SUBYARD_CONFIG_HOME=str(config), SUBYARD_NO_AUDIT="1")
    env.pop("SUBYARD_RELEASE_TRANSITION_GRANT_FD", None)
    request = dict(schemaVersion=1, mode="inspect", runtimeRoot=str(runtime),
                   configHome=str(config), yard=yard, target=release, direction="activate-target",
                   artifactDigest=hashlib.sha256((owner / "runtime-files.sha256").read_bytes()).hexdigest(),
                   registryDigest=hashlib.sha256((owner / "config/release-transition.json").read_bytes()).hexdigest())
    engine = owner / "bin/yard-engine"
    receipts = []
    receipt_path = runtime.parent / "historical-owner-cut-native.json"
    def record_native(phase, result):
        receipts.append(dict(phase=phase, exitCode=result.returncode))
        receipt_path.write_text(json.dumps(receipts, indent=2) + "\n")
        receipt_path.chmod(0o600)
    inspected = verifier["run_process"]([str(engine), "_release-transition"], env, timeout=60,
                                        input_text=json.dumps(request) + "\n")
    record_native("inspect", inspected)
    if inspected.returncode:
        raise RuntimeError("historical owner refused its original inspection")
    inspection = json.loads(inspected.stdout)["inspection"]
    if inspection.get("blockers"):
        raise RuntimeError("historical owner has blockers before its original transition")
    grant = "historical-fixture-consumed-private-grant"
    request.update(mode="converge", execution=dict(plan=inspection["plan"], authorization=grant))
    payload = fixture / "converge.json"
    payload.write_text(json.dumps(request) + "\n")
    payload.chmod(0o600)
    journal = config / "release-transition/v2/journal.json"
    journal.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
    marker = fixture / "reconciling.json"
    command = ["bash", "-c", 'exec 3< <(printf "%s\\n" "$1"); export SUBYARD_RELEASE_TRANSITION_GRANT_FD=3; exec "$2" _release-transition < "$3"',
               "historical-owner", grant, str(engine), str(payload)]
    converged = verifier["run_process"](command, env, timeout=60,
                            interrupt_checkpoint=(journal, "reconciling", marker), allow_failed_checkpoint=True)
    record_native("converge-cut", converged)
    record = json.loads(journal.read_text())
    if record["checkpoint"] != "reconciling" or not all(step["checkpoint"] == "verified" for step in record["steps"]):
        raise RuntimeError("historical owner did not retain its verified reconciliation journal")
    print(record["transaction"])
finally:
    shutil.rmtree(fixture)
PYSEED
}
