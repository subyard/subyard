#!/usr/bin/env bash
# Install and reconcile the pinned Paseo headless bundle inside one enabled yard.
set -euo pipefail

PASEO_VERSION=0.2.1
PASEO_HOME="${PASEO_HOME:-/srv/agents/paseo}"
PASEO_ROOT="${PASEO_ROOT:-/opt/subyard/paseo}"
PASEO_RELEASE_VERSION="${PASEO_RELEASE_VERSION:-${YARD_VERSION:-}}"
PASEO_RELEASE_REPOSITORY="${PASEO_RELEASE_REPOSITORY:-Subyard/Subyard}"
DEV_USER="${DEV_USER:-dev}"
DEV_HOME="$(getent passwd "$DEV_USER" | cut -d: -f6)"
: "${DEV_HOME:=/home/$DEV_USER}"

die() { printf 'Paseo provision: %s\n' "$*" >&2; exit 1; }
case "$DEV_USER" in ''|*[!A-Za-z0-9._-]*|-*|.|..) die "invalid developer user" ;; esac
# A foreign or changed service must never be adopted by provisioning.
ownership=/var/lib/subyard/paseo-ownership
if [ -e /etc/systemd/system/paseo.service ] || [ -L /etc/systemd/system/paseo.service ]; then
  [ -f "$ownership" ] && [ ! -L "$ownership" ] \
    && [ "$(stat -c '%u:%g:%a' "$ownership")" = 0:0:600 ] \
    && [ "$(head -n 1 "$ownership")" = "# subyard-paseo-ownership-v1" ] \
    && sha256sum --status -c "$ownership" \
    || die "Paseo service ownership is unknown or changed"
fi
case "$(dpkg --print-architecture)" in
  amd64) artifact_arch=amd64 ;;
  arm64) artifact_arch=arm64 ;;
  *) die "unsupported architecture" ;;
esac

for command in cmp curl find flock jq runuser sed sha256sum systemctl tar; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done

temporary="$(mktemp -d /tmp/subyard-paseo.XXXXXX)"
cleanup() { rm -rf -- "$temporary"; }
trap cleanup EXIT HUP INT TERM
bundle="$temporary/paseo-headless-$PASEO_VERSION-linux-$artifact_arch.tar.gz"
asset_name="$(basename "$bundle")"

if [ -n "${PASEO_BUNDLE_FILE:-}" ]; then
  case "$PASEO_BUNDLE_FILE" in /*) ;; *) die "PASEO_BUNDLE_FILE must be absolute" ;; esac
  [ -f "$PASEO_BUNDLE_FILE" ] && [ ! -L "$PASEO_BUNDLE_FILE" ] \
    || die "PASEO_BUNDLE_FILE must be a regular non-symlink file"
  cp -- "$PASEO_BUNDLE_FILE" "$bundle"
else
  case "$PASEO_RELEASE_VERSION" in
    [0-9]*.[0-9]*.[0-9]*) ;;
    *) die "a stable Subyard release or explicit PASEO_BUNDLE_FILE is required" ;;
  esac
  base="https://github.com/$PASEO_RELEASE_REPOSITORY/releases/download/v$PASEO_RELEASE_VERSION"
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
    "$base/$asset_name" -o "$bundle"
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
    "$base/$asset_name.sha256" -o "$bundle.sha256"
  read -r expected expected_name extra <"$bundle.sha256" || true
  [ -z "${extra:-}" ] && [ "$expected_name" = "$asset_name" ] \
    || die "invalid bundle checksum file"
  case "$expected" in
    ????????????????????????????????????????????????????????????????) ;;
    *) die "invalid bundle SHA-256" ;;
  esac
  case "$expected" in *[!0-9a-fA-F]*) die "invalid bundle SHA-256" ;; esac
  [ "$(sha256sum "$bundle" | cut -d' ' -f1)" = "${expected,,}" ] \
    || die "bundle checksum mismatch"
fi

if tar -tzf "$bundle" | awk '
  /(^\/|(^|\/)\.\.(\/|$))/ { unsafe=1 }
  END { exit unsafe ? 0 : 1 }
'; then
  die "bundle contains an unsafe path"
fi
if tar -tvzf "$bundle" | awk '
  substr($1, 1, 1) != "-" && substr($1, 1, 1) != "d" { invalid=1 }
  END { exit invalid ? 0 : 1 }
'; then
  die "bundle contains a non-regular entry"
fi

candidate="$temporary/candidate"
mkdir "$candidate"
tar -xzf "$bundle" -C "$candidate" --no-same-owner --no-same-permissions
for required in \
  manifest.json files.sha256 node/bin/node \
  app/node_modules/@getpaseo/cli/bin/paseo app/libexec/paseo-sync-projects.mjs \
  package/config.json package/paseo.service.in \
  package/bin/paseo package/bin/paseo-check package/bin/paseo-pair \
  package/bin/paseo-sync-projects package/bin/paseo-rotate-identity; do
  [ -f "$candidate/$required" ] && [ ! -L "$candidate/$required" ] \
    || die "bundle is missing $required"
done
jq -e --arg version "$PASEO_VERSION" --arg arch "$artifact_arch" '
  .schemaVersion == 1 and .kind == "paseo-headless" and
  .paseoVersion == $version and .upstreamRevision ==
    "36f38245cab51bbe0b43b6ac42fd41aa757064d9" and
  .nodeVersion == "22.20.0" and .os == "linux" and .arch == $arch and
  .dependencies.nodePty == "1.2.0-beta.11" and
  .dependencies.sherpaOnnxNode == "1.12.28" and
  .dependencies.claudeAgentSdk == "0.3.214"
' "$candidate/manifest.json" >/dev/null || die "bundle manifest mismatch"
(cd "$candidate" && sha256sum -c files.sha256 >/dev/null) \
  || die "bundle file checksum mismatch"

actual_files="$temporary/actual-files"
listed_files="$temporary/listed-files"
(cd "$candidate" && find . -type f ! -name files.sha256 -print | sort >"$actual_files")
sed -E 's/^[0-9a-fA-F]{64}  //' "$candidate/files.sha256" | sort >"$listed_files"
cmp -s "$actual_files" "$listed_files" || die "bundle file manifest is not exact"

bundle_hash="$(sha256sum "$bundle" | cut -d' ' -f1)"
release="$PASEO_ROOT/releases/$PASEO_VERSION-${bundle_hash:0:16}"
install -d -m 0755 "$PASEO_ROOT/releases"
release_matches=0
if [ -d "$release" ] && [ ! -L "$release" ] &&
  cmp -s "$candidate/files.sha256" "$release/files.sha256" &&
  (cd "$release" && sha256sum -c files.sha256 >/dev/null); then
  release_files="$temporary/release-files"
  (cd "$release" && find . -type f ! -name files.sha256 -print | sort >"$release_files")
  if cmp -s "$actual_files" "$release_files"; then
    release_matches=1
  fi
fi
displaced_release=''
if [ "$release_matches" = 0 ]; then
  staged="$(mktemp -d "$PASEO_ROOT/releases/.candidate.XXXXXX")"
  cp -a -- "$candidate/." "$staged/"
  find "$staged" -type d -exec chmod go-w {} +
  find "$staged" -type f -exec chmod go-w {} +
  if [ -e "$release" ] || [ -L "$release" ]; then
    displaced_release="$(mktemp -d "$PASEO_ROOT/releases/.replaced.XXXXXX")"
    rmdir "$displaced_release"
    mv -- "$release" "$displaced_release"
  fi
  if ! mv -- "$staged" "$release"; then
    [ -z "$displaced_release" ] || mv -- "$displaced_release" "$release"
    die "could not publish verified runtime"
  fi
fi
previous=''
[ ! -L "$PASEO_ROOT/current" ] || previous="$(readlink "$PASEO_ROOT/current")"
link="$(mktemp "$PASEO_ROOT/.current.XXXXXX")"
rm -f -- "$link"
ln -s "releases/$(basename "$release")" "$link"
mv -Tf -- "$link" "$PASEO_ROOT/current"

state_preexisting=0
[ ! -e "$PASEO_HOME" ] || state_preexisting=1
install -d -m 0700 -o "$DEV_USER" -g "$DEV_USER" "$PASEO_HOME"

rollback_files="$temporary/rollback-files"
install -d -m 0700 "$rollback_files/bin"
config_preexisting=0
unit_preexisting=0
[ ! -e "$PASEO_HOME/config.json" ] && [ ! -L "$PASEO_HOME/config.json" ] || {
  cp -a -- "$PASEO_HOME/config.json" "$rollback_files/config.json"
  config_preexisting=1
}
[ ! -e /etc/systemd/system/paseo.service ] && [ ! -L /etc/systemd/system/paseo.service ] || {
  cp -a -- /etc/systemd/system/paseo.service "$rollback_files/paseo.service"
  unit_preexisting=1
}
declare -A helper_preexisting=()
for helper in paseo paseo-check paseo-pair paseo-sync-projects paseo-rotate-identity; do
  helper_preexisting["$helper"]=0
  if [ -e "/usr/local/bin/$helper" ] || [ -L "/usr/local/bin/$helper" ]; then
    cp -a -- "/usr/local/bin/$helper" "$rollback_files/bin/$helper"
    helper_preexisting["$helper"]=1
  fi
done

install -m 0644 -o root -g root "$release/package/config.json" "$PASEO_HOME/config.json"

install -d -m 0755 /usr/local/bin
for helper in paseo paseo-check paseo-pair paseo-sync-projects paseo-rotate-identity; do
  sed -e "s|@DEV_USER@|$DEV_USER|g" -e "s|@DEV_HOME@|$DEV_HOME|g" \
    "$release/package/bin/$helper" >"$temporary/$helper"
  install -m 0755 -o root -g root "$temporary/$helper" "/usr/local/bin/$helper"
done
sed -e "s|@DEV_USER@|$DEV_USER|g" -e "s|@DEV_HOME@|$DEV_HOME|g" \
  "$release/package/paseo.service.in" >"$temporary/paseo.service"
install -m 0644 -o root -g root "$temporary/paseo.service" \
  /etc/systemd/system/paseo.service

rollback() {
  systemctl disable --now paseo.service >/dev/null 2>&1 || true
  if [ -n "$displaced_release" ] && [ -e "$displaced_release" ]; then
    rm -rf -- "$release"
    mv -- "$displaced_release" "$release"
  fi
  if [ "$config_preexisting" = 1 ]; then
    rm -f -- "$PASEO_HOME/config.json"
    cp -a -- "$rollback_files/config.json" "$PASEO_HOME/config.json"
  else
    rm -f -- "$PASEO_HOME/config.json"
  fi
  if [ "$unit_preexisting" = 1 ]; then
    rm -f -- /etc/systemd/system/paseo.service
    cp -a -- "$rollback_files/paseo.service" /etc/systemd/system/paseo.service
  else
    rm -f -- /etc/systemd/system/paseo.service
  fi
  for helper in paseo paseo-check paseo-pair paseo-sync-projects paseo-rotate-identity; do
    rm -f -- "/usr/local/bin/$helper"
    if [ "${helper_preexisting[$helper]}" = 1 ]; then
      cp -a -- "$rollback_files/bin/$helper" "/usr/local/bin/$helper"
    fi
  done
  systemctl daemon-reload >/dev/null 2>&1 || true
  if [ -n "$previous" ] && [ -d "$PASEO_ROOT/$previous" ]; then
    fallback="$(mktemp "$PASEO_ROOT/.current.XXXXXX")"
    rm -f -- "$fallback"
    ln -s "$previous" "$fallback"
    mv -Tf -- "$fallback" "$PASEO_ROOT/current"
    systemctl enable --now paseo.service >/dev/null 2>&1 || true
  else
    rm -f -- "$PASEO_ROOT/current"
    if [ "$state_preexisting" = 0 ]; then
      rm -rf -- "$PASEO_HOME"
    fi
  fi
}

systemctl daemon-reload
if ! systemctl enable --now paseo.service || ! systemctl restart paseo.service; then
  rollback
  die "unit failed to start; previous runtime restored"
fi
if ! PASEO_DEV_USER="$DEV_USER" PASEO_DEV_HOME="$DEV_HOME" /usr/local/bin/paseo-check; then
  rollback
  die "readiness failed; previous runtime restored"
fi

# Evidence contains only identities and content digests, never pairing/auth state.
install -d -m 0755 /var/lib/subyard
receipt="$(mktemp /var/lib/subyard/.paseo-ownership.XXXXXX)"
printf '%s\n' '# subyard-paseo-ownership-v1' >"$receipt"
sha256sum /etc/systemd/system/paseo.service >>"$receipt"
chmod 0600 "$receipt"
chown root:root "$receipt"
mv -f -- "$receipt" "$ownership"
[ -z "$displaced_release" ] || rm -rf -- "$displaced_release"
printf 'Paseo %s installed. Retrieve the pairing offer explicitly with paseo-pair.\n' "$PASEO_VERSION"
