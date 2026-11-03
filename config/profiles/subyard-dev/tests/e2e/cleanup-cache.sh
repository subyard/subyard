#!/usr/bin/env bash
# Sourced by the generic cleanup fixture inside its marker-owned disposable yard.
# fixture_dev_user comes from that fixture; quoted scripts run inside the guest.
# shellcheck disable=SC2154,SC2016
yard provision subyard-dev --yes
# Provision selects this profile after init; reconcile installed guest metadata
# through the ordinary release boundary before the next guarded mutation.
native_owner "$launcher" migrate --yes
guest sh -eu -c '
 install -d -o "$1" -g "$1" -m 0750 /home/"$1"/cleanup-cache-fixture
 printf "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"cache rebuild passed\") }\n" >/home/"$1"/cleanup-cache-fixture/main.go
 chown "$1:$1" /home/"$1"/cleanup-cache-fixture/main.go
' sh "$fixture_dev_user"
cleanup_cache_build() {
  # Incus exec starts as root; runuser alone retains its inaccessible cwd.
  guest runuser -u "$fixture_dev_user" -- sh -eu -c '
    cd "$1"
    exec env GO111MODULE=off GOTOOLCHAIN=local \
      GOCACHE=/srv/cache/go-build GOMODCACHE=/srv/cache/go-mod \
      go build -o program main.go
  ' sh "/home/$fixture_dev_user/cleanup-cache-fixture"
  [ "$(guest "/home/$fixture_dev_user/cleanup-cache-fixture/program")" = 'cache rebuild passed' ] || die 'Go cache rebuild failed'
}
printf 'subyard-dev_cleanup_stage=build-before\n'
cleanup_cache_build
go_identity="$(guest sha256sum /usr/bin/go)"
guest sh -eu -c '
 install -d -m 0750 /srv/cache/go-build /srv/cache/cargo-build/subyard /srv/cache/npm-public /srv/cache/go-mod
 printf disposable >/srv/cache/go-build/fixture
 printf disposable >/srv/cache/cargo-build/subyard/fixture
 printf disposable >/srv/cache/npm-public/fixture
 printf installed-toolchain >/srv/cache/go-mod/fixture
'
cache_identity="$(guest stat -c '%i:%a:%u:%g' /srv/cache/go-build)"
printf 'subyard-dev_cleanup_stage=soft-cleanup\n'
yard cleanup --soft --yes
guest sh -eu -c '
 test ! -e /srv/cache/go-build/fixture
 test ! -e /srv/cache/cargo-build/subyard/fixture
 test ! -e /srv/cache/npm-public/fixture
 test "$(cat /srv/cache/go-mod/fixture)" = installed-toolchain
'
[ "$(guest stat -c '%i:%a:%u:%g' /srv/cache/go-build)" = "$cache_identity" ] || die 'cache root metadata changed'
[ "$(guest sha256sum /usr/bin/go)" = "$go_identity" ] || die 'installed Go toolchain changed'
printf 'subyard-dev_cleanup_stage=build-after\n'
cleanup_cache_build
yard config set ENVIRONMENT_PROFILES '' --scope yard --yes
native_owner "$launcher" migrate --yes
printf 'evidence: subyard_dev_explicit_cache_cleanup_toolchain_retention_and_build_retry=PASS\n'
