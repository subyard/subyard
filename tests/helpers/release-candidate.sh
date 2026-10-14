#!/usr/bin/env bash
# Optional packaged-candidate transport for release acceptance fixtures.
release_candidate_prepare() {
  local root="$1" acceptance metadata runtime_file runtime_sha256 archive runtime
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
    version=0.1.0-candidate.e2e
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
