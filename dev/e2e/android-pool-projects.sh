#!/usr/bin/env bash
# Run inside the allocated Android E2E VM while its existing yard is retained.
set -euo pipefail

fail() { printf 'android-pool-projects: %s\n' "$*" >&2; exit 1; }
[ "$#" -eq 5 ] || fail 'usage: script ROOT STATE YARD PROJECT INSTANCE'
root="$1" state="$2" yard_name="$3" project="$4" instance="$5"
[[ "$root" = /* && "$state" = /* ]] || fail 'root and state must be absolute paths'
[[ "$yard_name" =~ ^[a-zA-Z0-9][a-zA-Z0-9-]*$ ]] || fail 'invalid yard name'
[[ "$project" =~ ^[a-zA-Z0-9][a-zA-Z0-9-]*$ ]] || fail 'invalid Incus project'
[[ "$instance" =~ ^[a-zA-Z0-9][a-zA-Z0-9-]*$ ]] || fail 'invalid Incus instance'
[ "$(cat "$state/.marker" 2>/dev/null)" = subyard-android-pool-runtime-v1 ] \
  || fail 'state is not the retained Android fixture'
[ "${SUBYARD_E2E_VM:-}" = 1 ] && [ -r /run/subyard-e2e-lease.json ] \
  || fail 'requires the allocated Android E2E VM'
[ -x "$root/.build/yard" ] || fail 'candidate yard binary is missing'
for command in curl git incus sg sudo timeout; do
  command -v "$command" >/dev/null || fail "missing $command"
done

export SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_HOME="$state/config" SUBYARD_HOME="$state/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
budget="${ANDROID_L2_TIMEOUT_SECONDS:-7200}"
[[ "$budget" =~ ^[1-9][0-9]*$ ]] && [ "$budget" -le 7200 ] || fail 'invalid time budget'
uptime_seconds() { local up rest; read -r up rest < /proc/uptime; : "$rest"; printf '%s\n' "${up%%.*}"; }
deadline=$(( $(uptime_seconds) + budget ))
remaining() {
  local left=$((deadline - $(uptime_seconds)))
  [ "$left" -gt 0 ] || fail 'total work deadline expired'
  if [ "$left" -lt "$1" ]; then printf '%s\n' "$left"; else printf '%s\n' "$1"; fi
}
yard_command() {
  local seconds="$1" command
  shift
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    printf -v command '%q ' "$root/.build/yard" -Y "$yard_name" "$@"
    timeout --foreground "$seconds" sg incus-admin -c "exec $command"
  else
    timeout --foreground "$seconds" "$root/.build/yard" -Y "$yard_name" "$@"
  fi
}
bounded_yard() { local seconds="$1"; shift; yard_command "$(remaining "$seconds")" "$@"; }
cleanup_yard() {
  yard_command 30 "$@"
}
bounded_incus_exec() {
  local seconds="$1"
  shift
  local -a binary=(/usr/bin/incus)
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    binary=(sudo -n /usr/bin/incus)
  fi
  timeout --foreground "$(remaining "$seconds")" "${binary[@]}" \
    --project "$project" exec "$instance" -- "$@" </dev/null
}
[ "$(bounded_yard 30 config show INCUS_PROJECT | sed -n 's/^effective: //p')" = "$project" ] \
  || fail 'yard Incus project does not match the retained fixture'
[ "$(bounded_yard 30 config show YARD_INSTANCE_NAME | sed -n 's/^effective: //p')" = "$instance" ] \
  || fail 'yard instance does not match the retained fixture'

work="$(mktemp -d "$state/android-l2-parallel.XXXXXX")"
printf '%s\n' subyard-android-l2-parallel-v1 > "$work/.marker"
token="${work##*.}"
name_a="AndroidParallelA$token"
name_b="AndroidParallelB$token"
name_legacy="AndroidLegacy$token"
registered_a=0 registered_b=0 registered_legacy=0 up_a=0 up_b=0 up_legacy=0
legacy_fixture=0
legacy_sdk="/srv/cache/subyard-legacy-sdk-$token"
legacy_gradle="/srv/cache/subyard-legacy-gradle-$token"
diagnostic_logs=()
show_failure_logs() {
  local log
  for log in "${diagnostic_logs[@]}"; do
    [ -s "$log" ] || continue
    printf 'android-pool-projects: failure log %s (last 25 lines)\n' "$log" >&2
    tail -n 25 "$log" | sed -E \
      's/(token|credential|secret|password|authorization|api[_-]?key)[=: ][^[:space:]]+/[redacted]/Ig' >&2
  done
}
cleanup() {
  local result=$? cleanup_failed=0
  trap - EXIT INT TERM
  if [ "$result" -ne 0 ]; then show_failure_logs; fi
  if [ "$registered_b" = 1 ]; then
    if [ "$up_b" = 1 ]; then
      cleanup_yard down "$name_b" --yes \
        >> "$work/cleanup.log" 2>&1 || cleanup_failed=1
    fi
    cleanup_yard remove "$name_b" --yes \
      >> "$work/cleanup.log" 2>&1 || cleanup_failed=1
  fi
  if [ "$registered_a" = 1 ]; then
    if [ "$up_a" = 1 ]; then
      cleanup_yard down "$name_a" --yes \
        >> "$work/cleanup.log" 2>&1 || cleanup_failed=1
    fi
    cleanup_yard remove "$name_a" --yes \
      >> "$work/cleanup.log" 2>&1 || cleanup_failed=1
  fi
  if [ "$registered_legacy" = 1 ]; then
    if [ "$up_legacy" = 1 ]; then
      cleanup_yard down "$name_legacy" --yes \
        >> "$work/cleanup.log" 2>&1 || cleanup_failed=1
    fi
    cleanup_yard remove "$name_legacy" --yes \
      >> "$work/cleanup.log" 2>&1 || cleanup_failed=1
  fi
  if [ "$legacy_fixture" = 1 ]; then
    if ! bounded_incus_exec 30 sh -ceu '
      for path in "$@"; do
        [ -f "$path/.subyard-legacy-l2-marker" ] || exit 2
        find "$path" -depth -delete
      done
    ' _ "$legacy_sdk" "$legacy_gradle" >> "$work/cleanup.log" 2>&1; then
      cleanup_failed=1
    fi
  fi
  if [ "$cleanup_failed" -ne 0 ]; then
    printf 'android-pool-projects: project cleanup failed; see %s/cleanup.log\n' "$work" >&2
    [ "$result" -ne 0 ] || result=3
  fi
  printf 'android-pool-projects: evidence=%s\n' "$work" >&2
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# The checked source and wrapper versions match android-build-smoke-tail.sh.
seed="$work/seed"
install -d -m 0700 "$seed/gradle/wrapper"
fetch() {
  curl -fLsS --retry 1 --connect-timeout 15 --max-time "$(remaining 90)" "$1" -o "$2"
}
fetch https://raw.githubusercontent.com/gradle/gradle/v8.13.0/gradlew "$seed/gradlew"
fetch https://raw.githubusercontent.com/gradle/gradle/v8.13.0/gradle/wrapper/gradle-wrapper.jar \
  "$seed/gradle/wrapper/gradle-wrapper.jar"
fetch https://services.gradle.org/distributions/gradle-8.13-wrapper.jar.sha256 "$work/wrapper.sha256"
fetch https://services.gradle.org/distributions/gradle-8.13-bin.zip.sha256 "$work/distribution.sha256"
wrapper_hash="$(cat "$work/wrapper.sha256")"
distribution_hash="$(cat "$work/distribution.sha256")"
[[ "$wrapper_hash" =~ ^[0-9a-f]{64}$ && "$distribution_hash" =~ ^[0-9a-f]{64}$ ]] \
  || fail 'Gradle checksum response is invalid'
printf '%s  %s\n' "$wrapper_hash" "$seed/gradle/wrapper/gradle-wrapper.jar" | sha256sum -c - \
  > "$work/wrapper-check.log"
cat > "$seed/gradle/wrapper/gradle-wrapper.properties" <<EOF
distributionBase=GRADLE_USER_HOME
distributionPath=wrapper/dists
distributionUrl=https\://services.gradle.org/distributions/gradle-8.13-bin.zip
distributionSha256Sum=$distribution_hash
networkTimeout=120000
zipStoreBase=GRADLE_USER_HOME
zipStorePath=wrapper/dists
EOF
chmod 0755 "$seed/gradlew"

make_project() {
  local name="$1" lane="$2" directory
  directory="$work/$name"
  install -d -m 0700 "$directory/gradle/wrapper" \
    "$directory/app/src/main/java/org/subyard/parallel" \
    "$directory/app/src/main/res/values"
  cp "$seed/gradlew" "$directory/gradlew"
  cp "$seed/gradle/wrapper/gradle-wrapper.jar" \
    "$seed/gradle/wrapper/gradle-wrapper.properties" "$directory/gradle/wrapper/"
  printf '%s\n' "$name" > "$directory/project.marker"
  cat > "$directory/settings.gradle" <<EOF
pluginManagement { repositories { google(); mavenCentral(); gradlePluginPortal() } }
dependencyResolutionManagement { repositories { google(); mavenCentral() } }
rootProject.name = '$name'
include ':app'
EOF
  printf "%s\n" "plugins { id 'com.android.application' version '8.12.2' apply false }" \
    > "$directory/build.gradle"
  cat > "$directory/app/build.gradle" <<EOF
plugins { id 'com.android.application' }
android {
    namespace 'org.subyard.parallel'
    compileSdk 36
    buildToolsVersion '36.0.0'
    defaultConfig {
        applicationId 'org.subyard.parallel.${lane,,}'
        minSdk 26
        targetSdk 36
        versionCode 1
        versionName '1.0'
    }
}
EOF
  cat > "$directory/gradle.properties" <<'EOF'
org.gradle.jvmargs=-Xmx768m -XX:MaxMetaspaceSize=384m
org.gradle.workers.max=1
org.gradle.daemon=false
EOF
  printf '%s\n' '<manifest xmlns:android="http://schemas.android.com/apk/res/android"><application android:label="@string/app_name" /></manifest>' \
    > "$directory/app/src/main/AndroidManifest.xml"
  printf '<resources><string name="app_name">Parallel %s</string></resources>\n' "$lane" \
    > "$directory/app/src/main/res/values/strings.xml"
  printf '%s\n' 'package org.subyard.parallel; public final class Probe { public static int api() { return android.os.Build.VERSION.SDK_INT; } }' \
    > "$directory/app/src/main/java/org/subyard/parallel/Probe.java"
  printf '%s\n' 'sdk.dir=/srv/cache/android-sdk' > "$directory/local.properties"
  git -C "$directory" init -q
  git -C "$directory" add .
}
make_project "$name_a" A
make_project "$name_b" B

printf 'android-pool-projects phase=sync-up\n'
diagnostic_logs=("$work/sync-a.log")
bounded_yard 120 sync "$work/$name_a" --name "$name_a" --target android --yes \
  > "$work/sync-a.log" 2>&1
registered_a=1
diagnostic_logs=("$work/sync-b.log")
bounded_yard 120 sync "$work/$name_b" --name "$name_b" --target android --yes \
  > "$work/sync-b.log" 2>&1
registered_b=1
diagnostic_logs=("$work/up-a.log")
bounded_yard 120 up "$name_a" --yes > "$work/up-a.log" 2>&1
up_a=1
diagnostic_logs=("$work/up-b.log")
bounded_yard 120 up "$name_b" --yes > "$work/up-b.log" 2>&1
up_b=1

find_box() {
  local name="$1"
  local -a boxes
  mapfile -t boxes < <(bounded_incus_exec 30 docker ps -q \
    --filter label=subyard.env=1 --filter "label=subyard.project=$name" \
    --filter label=subyard.profile=android)
  [ "${#boxes[@]}" -eq 1 ] && [ -n "${boxes[0]}" ] \
    || fail "expected one labeled Android L2 container for $name"
  printf '%s\n' "${boxes[0]}"
}
box_a="$(find_box "$name_a")"
box_b="$(find_box "$name_b")"
[ "$box_a" != "$box_b" ] || fail 'both projects resolved to the same L2 container'

check_box() {
  local box="$1" own="$2" other="$3"
  bounded_incus_exec 45 docker exec -u 1000:1000 "$box" bash -ceu '
    [ "$(id -u)" = 1000 ] && [ "$(id -g)" = 1000 ]
    [ "$(cat project.marker)" = "$1" ]
    [ "$GRADLE_USER_HOME" = /home/dev/.gradle ]
    [ "$ANDROID_SDK_ROOT" = /srv/cache/android-sdk ]
    [ ! -w "$ANDROID_SDK_ROOT" ]
    [ ! -e /dev/kvm ] && [ ! -e /dev/dri ]
    [ -z "${DISPLAY:-}" ] && [ -z "${WAYLAND_DISPLAY:-}" ]
    if [ -d /run/user/1000 ]; then
      [ -r /run/user/1000 ]
      [ -z "$(find /run/user/1000 -maxdepth 1 -type s -name "wayland-*" -print -quit)" ]
    fi
    if [ -d /tmp/.X11-unix ]; then
      [ -r /tmp/.X11-unix ]
      [ -z "$(find /tmp/.X11-unix -maxdepth 1 -type s -print -quit)" ]
    fi
    mkdir -p "$GRADLE_USER_HOME"
    [ ! -e "$GRADLE_USER_HOME/$2" ]
    printf "%s\n" "$1" > "$GRADLE_USER_HOME/$3"
    android-broker status >/dev/null
    android-broker catalog >/dev/null
  ' _ "$own" ".subyard-parallel-$other" ".subyard-parallel-$own" \
    > "$work/check-$own.log" 2>&1
}
printf 'android-pool-projects phase=isolation\n'
diagnostic_logs=("$work/check-$name_a.log")
check_box "$box_a" "$name_a" "$name_b"
diagnostic_logs=("$work/check-$name_b.log")
check_box "$box_b" "$name_b" "$name_a"
bounded_incus_exec 30 docker exec -u 1000:1000 "$box_a" \
  test ! -e "/home/dev/.gradle/.subyard-parallel-$name_b"
bounded_incus_exec 30 docker exec -u 1000:1000 "$box_b" \
  test ! -e "/home/dev/.gradle/.subyard-parallel-$name_a"
printf 'android-pool-projects isolation=PASS sdk=readonly gradle=private devices=absent facade=PASS\n'

build_box() {
  local box="$1" name="$2"
  bounded_incus_exec 600 docker exec -u 1000:1000 "$box" bash -ceu '
    [ "$(cat project.marker)" = "$1" ]
    [ -f "$GRADLE_USER_HOME/.subyard-parallel-$1" ]
    [ ! -e "$GRADLE_USER_HOME/.subyard-parallel-$2" ]
    ./gradlew --no-daemon --max-workers=1 clean :app:assembleDebug
    apk=app/build/outputs/apk/debug/app-debug.apk
    [ -s "$apk" ]
    "$JAVA_HOME/bin/jar" tf "$apk" > apk.contents
    grep -Fxq AndroidManifest.xml apk.contents
    grep -Fxq classes.dex apk.contents
    printf "apk project=%s bytes=%s manifest=PASS dex=PASS\n" "$1" "$(stat -c %s "$apk")"
  ' _ "$name" "${3}"
}
printf 'android-pool-projects phase=build\n'
diagnostic_logs=("$work/build-a.log" "$work/build-b.log")
build_box "$box_a" "$name_a" "$name_b" > "$work/build-a.log" 2>&1 &
pid_a=$!
build_box "$box_b" "$name_b" "$name_a" > "$work/build-b.log" 2>&1 &
pid_b=$!
result_a=0 result_b=0
wait "$pid_a" || result_a=$?
wait "$pid_b" || result_b=$?
[ "$result_a" -eq 0 ] && [ "$result_b" -eq 0 ] \
  || fail "parallel builds failed (A=$result_a B=$result_b); see $work/build-*.log"
grep -h '^apk project=' "$work/build-a.log" "$work/build-b.log"
printf 'android-pool-projects parallel-build=PASS\n'

printf 'android-pool-projects phase=l2-lifecycle\n'
diagnostic_logs=("$work/l2-lifecycle.log")
lifecycle="$(cat "$root/dev/e2e/android-pool-lifecycle.py")"
bounded_incus_exec 6600 docker exec -u 1000:1000 "$box_a" \
  python3 -c "$lifecycle" 2>&1 | tee "$work/l2-lifecycle.log"
grep -Fxq 'android pool lifecycle: PASS' "$work/l2-lifecycle.log" \
  || fail 'L2 Android lease lifecycle did not report PASS'
printf 'android-pool-projects l2-lifecycle=PASS\n'
printf 'android-pool-projects l2-emulator-api=35 install=PASS fresh-userdata=PASS\n'

printf 'android-pool-projects phase=rebuild\n'
diagnostic_logs=("$work/down-a.log")
bounded_yard 120 down "$name_a" --yes > "$work/down-a.log" 2>&1
up_a=0
diagnostic_logs=("$work/rebuild-a.log")
bounded_yard 180 up "$name_a" --rebuild --yes > "$work/rebuild-a.log" 2>&1
up_a=1
rebuilt_a="$(find_box "$name_a")"
[ "$rebuilt_a" != "$box_a" ] || fail 'project rebuild reused the previous L2 container'
[ "$(find_box "$name_b")" = "$box_b" ] || fail 'rebuilding A replaced project B'
diagnostic_logs=("$work/rebuild-check.log")
bounded_incus_exec 45 docker exec -u 1000:1000 "$rebuilt_a" bash -ceu '
  [ "$(cat project.marker)" = "$1" ]
  [ ! -e "$GRADLE_USER_HOME/.subyard-parallel-$1" ]
  [ ! -e "$GRADLE_USER_HOME/.subyard-parallel-$2" ]
' _ "$name_a" "$name_b" > "$work/rebuild-check.log" 2>&1
bounded_incus_exec 45 docker exec -u 1000:1000 "$box_b" bash -ceu '
  [ "$(cat project.marker)" = "$1" ]
  [ -f "$GRADLE_USER_HOME/.subyard-parallel-$1" ]
  [ ! -e "$GRADLE_USER_HOME/.subyard-parallel-$2" ]
' _ "$name_b" "$name_a" >> "$work/rebuild-check.log" 2>&1
printf 'android-pool-projects rebuild=PASS source=preserved other-gradle=preserved rebuilt-gradle=fresh\n'

# Reconstruct only the previous mount/device shape with the current engine. This is not a
# published legacy release: its dedicated writable SDK/Gradle directories stay outside the
# managed SDK and are removed by this marker-guarded fixture cleanup.
legacy_source="$work/legacy-source"
install -d -m 0700 "$legacy_source/bin"
cp -a "$root/config" "$legacy_source/config"
ln -s "$root/bin/yard-engine" "$legacy_source/bin/yard-engine"
ln -s "$root/scripts" "$legacy_source/scripts"
cat > "$legacy_source/config/profiles/android/profile.conf" <<EOF
PROFILE_NAME=android
PROJECT_ENV_BASE_IMAGE=subyard-android-env:1
ANDROID_HOME=/srv/cache/android-sdk
ANDROID_SDK_ROOT=/srv/cache/android-sdk
GRADLE_USER_HOME=/home/dev/.gradle
JAVA_HOME=/opt/jdk-17
ANDROID_PUBLIC_ROOT=/srv/cache/android-sdk/.subyard
PATH=/srv/cache/android-sdk/platform-tools:/opt/jdk-17/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ENV_MOUNTS="$legacy_sdk:/srv/cache/android-sdk $legacy_gradle:/home/dev/.gradle"
DEVICES="kvm"
EOF
bounded_incus_exec 30 sh -ceu '
  install -d -o dev -g dev -m 0755 "$1" "$2"
  printf reconstructed-legacy-l2-v1 > "$1/.subyard-legacy-l2-marker"
  printf reconstructed-legacy-l2-v1 > "$2/.subyard-legacy-l2-marker"
' _ "$legacy_sdk" "$legacy_gradle"
legacy_fixture=1
legacy_yard() {
  local seconds="$1" command
  shift
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    printf -v command '%q ' env "SUBYARD_REPOSITORY_ROOT=$legacy_source" "$root/.build/yard" -Y "$yard_name" "$@"
    timeout --foreground "$(remaining "$seconds")" sg incus-admin -c "exec $command"
  else
    timeout --foreground "$(remaining "$seconds")" env "SUBYARD_REPOSITORY_ROOT=$legacy_source" \
      "$root/.build/yard" -Y "$yard_name" "$@"
  fi
}
printf 'android-pool-projects phase=legacy-l2-rebuild\n'
diagnostic_logs=("$work/legacy-sync.log")
legacy_yard 120 sync "$work/$name_a" --name "$name_legacy" --target android --yes \
  > "$work/legacy-sync.log" 2>&1
registered_legacy=1
diagnostic_logs=("$work/legacy-up.log")
legacy_yard 120 up "$name_legacy" --yes > "$work/legacy-up.log" 2>&1
up_legacy=1
legacy_box="$(find_box "$name_legacy")"
diagnostic_logs=("$work/legacy-check.log")
bounded_incus_exec 45 docker exec -u 1000:1000 "$legacy_box" bash -ceu '
  [ "$(cat project.marker)" = "$1" ]
  [ -w /srv/cache/android-sdk ] && [ -w /home/dev/.gradle ] && [ -e /dev/kvm ]
  printf workspace-preserved > legacy-workspace-marker
  printf legacy-gradle > /home/dev/.gradle/legacy-gradle-marker
' _ "$name_a" > "$work/legacy-check.log" 2>&1
diagnostic_logs=("$work/legacy-drift.log")
if bounded_yard 60 up "$name_legacy" --yes > "$work/legacy-drift.log" 2>&1; then
  fail 'legacy L2 accepted changed Android manifest without --rebuild'
fi
grep -Fq -- '--rebuild' "$work/legacy-drift.log" \
  || fail 'legacy L2 drift refusal did not require --rebuild'
diagnostic_logs=("$work/legacy-down.log")
bounded_yard 120 down "$name_legacy" --yes > "$work/legacy-down.log" 2>&1
up_legacy=0
diagnostic_logs=("$work/legacy-rebuild.log")
bounded_yard 180 up "$name_legacy" --rebuild --yes > "$work/legacy-rebuild.log" 2>&1
up_legacy=1
legacy_box_rebuilt="$(find_box "$name_legacy")"
[ "$legacy_box_rebuilt" != "$legacy_box" ] || fail 'legacy rebuild reused the old L2 container'
diagnostic_logs=("$work/legacy-rebuild-check.log")
bounded_incus_exec 45 docker exec -u 1000:1000 "$legacy_box_rebuilt" bash -ceu '
  [ "$(cat project.marker)" = "$1" ]
  [ "$(cat legacy-workspace-marker)" = workspace-preserved ]
  [ "$ANDROID_SDK_ROOT" = /srv/cache/android-sdk ] && [ ! -w "$ANDROID_SDK_ROOT" ]
  [ "$GRADLE_USER_HOME" = /home/dev/.gradle ]
  [ ! -e /dev/kvm ] && [ ! -e /dev/dri ]
  [ -z "${DISPLAY:-}" ] && [ -z "${WAYLAND_DISPLAY:-}" ]
  [ ! -e "$GRADLE_USER_HOME/legacy-gradle-marker" ]
  android-broker status >/dev/null
' _ "$name_a" > "$work/legacy-rebuild-check.log" 2>&1
printf 'android-pool-projects legacy-rebuild=PASS workspace=preserved sdk=readonly devices=absent gradle=fresh\n'
printf 'android-pool-projects=PASS\n'
