#!/usr/bin/env bash
# Run as the development user. This is the same destination the official clients update.
set -euo pipefail
minimum="$1" generation="$2" digest="$3" mode="$4" login="$5"
bin="$HOME/.local/bin"
binary="$bin/herdr-core"
release=https://releases.herdr.dev/core

case "$(uname -m)" in
  x86_64|aarch64) machine=$(uname -m) ;;
  *) printf 'Herdr: no release for this architecture\n' >&2; exit 1 ;;
esac

version() {
  local result
  result=$("$1" --version) || return 1
  if [[ "$result" =~ ^hc\ ([0-9]+\.[0-9]+\.[0-9]+)$ ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
  else
    printf 'Herdr: unknown executable version\n' >&2
    return 1
  fi
}

if [ -L "$binary" ] || { [ -e "$binary" ] && [ ! -x "$binary" ]; }; then
  printf 'Herdr: unsafe canonical binary path\n' >&2
  exit 1
fi
if [ -e "$bin/hc" ] || [ -L "$bin/hc" ]; then
  if [ "$(readlink "$bin/hc" || true)" != herdr-core ]; then
    printf 'Herdr: hc already exists outside Subyard ownership\n' >&2
    exit 1
  fi
fi
installed=
if [ -e "$binary" ]; then installed=$(version "$binary"); fi
if [ "$mode" = check ]; then
  [ -n "$installed" ] && [ -L "$bin/hc" ] && dpkg --compare-versions "$installed" ge "$minimum"
  exit
fi

target="$minimum"
curl_https() {
  curl -fsS --proto '=https' --connect-timeout 10 --max-time 120 "$@"
}
if [ "$mode" = latest ]; then
  manifest=$(curl_https --max-filesize 65536 "$release/latest.json")
  target=$(jq -er --argjson generation "$generation" '
    select(.generation == $generation) | .version | select(test("^[0-9]+\\.[0-9]+\\.[0-9]+$"))' <<< "$manifest") || {
      printf 'Herdr: latest release has an unsupported version or protocol generation\n' >&2
      exit 1
    }
  digest=$(jq -er --arg name "linux-$machine" '.targets[$name].sha256 | select(test("^[a-f0-9]{64}$"))' <<< "$manifest")
  if dpkg --compare-versions "$target" lt "$minimum"; then
    printf 'Herdr: latest manifest is behind the minimum verified release\n' >&2
    exit 1
  fi
fi

if [ -z "$installed" ] || dpkg --compare-versions "$installed" lt "$target" || \
   { [ "$mode" = force ] && dpkg --compare-versions "$installed" gt "$target"; }; then
  mkdir -p "$bin"
  stage=$(mktemp -d "$bin/.herdr-release.XXXXXX")
  trap 'rm -rf -- "$stage"' EXIT
  curl_https --max-filesize 67108864 "$release/$target/herdr-core-linux-$machine.tar.gz" -o "$stage/release.tar.gz"
  printf '%s  %s\n' "$digest" "$stage/release.tar.gz" | sha256sum -c --status
  tar -xOzf "$stage/release.tar.gz" -- herdr-core > "$stage/herdr-core"
  chmod 0755 "$stage/herdr-core"
  [ "$(version "$stage/herdr-core")" = "$target" ]
  if [ -z "$installed" ]; then
    mv -T -- "$stage/herdr-core" "$binary"
  elif [ "$mode" = force ]; then
    "$login" update --from "$stage/herdr-core" --force
  else
    "$login" update --from "$stage/herdr-core"
  fi
fi
[ -L "$bin/hc" ] || ln -s herdr-core "$bin/hc"
