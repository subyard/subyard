#!/usr/bin/env bash
# Runs a periodic E2E VM check on a disposable GitHub-hosted runner. The runner becomes an ordinary
# owner: the candidate runtime installs the test-vms service, and unchanged controllers lease
# from it. A failing check is a result; the probes here only establish that it could run.
set -uo pipefail
umask 022

RESULTS="${RUNNER_TEMP:-/var/tmp}/periodic-e2e-results.tsv"
# Controllers attribute leases from the managed workspace path.
WORKSPACE=/srv/workspaces/Subyard/src
PACKAGES=(build-essential ca-certificates curl expect git jq make openssh-client openssh-server ripgrep shellcheck tar xz-utils zsh)

die() { printf 'periodic-e2e: %s\n' "$*" >&2; exit 2; }

require_runner() {
  [ "${GITHUB_ACTIONS:-}" = true ] && [ "${RUNNER_ENVIRONMENT:-}" = github-hosted ] \
    || die 'refusing to run outside a disposable GitHub-hosted runner'
}

incus_root() { sudo -n /usr/bin/incus "$@" </dev/null; }

# Each probe runs in its own subshell so one failure cannot hide the independent ones after it.
probe() {
  local name="$1" start=$SECONDS rc=0 result=pass
  shift
  printf '::group::%s\n' "$name"
  (set -e; "$@") &
  wait "$!" || rc=$?
  printf '::endgroup::\n'
  [ "$rc" = 0 ] || { result="fail($rc)"; printf '::error title=%s::exit %s\n' "$name" "$rc"; }
  printf '%s\t%s\t%s\n' "$name" "$result" "$((SECONDS - start))" >> "$RESULTS"
  printf '%s: %s in %ss\n' "$name" "$result" "$((SECONDS - start))"
}

passed() { grep -q "^$1"$'\tpass\t' "$RESULTS" 2>/dev/null; }

# A probe whose prerequisite failed is recorded as skipped instead of adding a misleading failure.
probe_after() {
  local needed="$1" name="$2"
  shift 2
  if passed "$needed"; then
    probe "$name" "$@"
  else
    printf '%s\tskip (needs %s)\t0\n' "$name" "$needed" >> "$RESULTS"
  fi
}

write_summary() {
  local failed=0 name result seconds
  {
    printf '| Step | Result | Seconds |\n| --- | --- | ---: |\n'
    while IFS=$'\t' read -r name result seconds; do
      printf '| %s | %s | %s |\n' "$name" "$result" "$seconds"
    done < "$RESULTS"
  } | tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"
  grep -q $'\tfail(' "$RESULTS" && failed=1
  return "$failed"
}

facts() {
  local module
  uname -a
  . /etc/os-release && printf 'os: %s\n' "$PRETTY_NAME"
  printf 'cpus: %s\n' "$(nproc)"
  lscpu | grep -E '^(Model name|Virtualization|Hypervisor vendor)' || true
  free -h
  df -h / /mnt /var/tmp 2>/dev/null || true
  ls -l /dev/kvm 2>&1 || true
  for module in kvm_intel kvm_amd; do
    [ ! -r "/sys/module/$module/parameters/nested" ] \
      || printf '%s nested: %s\n' "$module" "$(cat "/sys/module/$module/parameters/nested")"
  done
  systemctl is-active docker || true
  sudo -n iptables -S FORWARD 2>&1 | head -5 || true
  command -v incus lxc lxd 2>/dev/null || true
  stat -c '%a %U %n' "$HOME" || true
}

enable_kvm() {
  printf 'KERNEL=="kvm", GROUP="kvm", MODE="0666", OPTIONS+="static_node=kvm"\n' \
    | sudo -n tee /etc/udev/rules.d/99-subyard-ci-kvm.rules >/dev/null
  sudo -n udevadm control --reload-rules
  sudo -n udevadm trigger --name-match=kvm
  udevadm settle --timeout=30 || true
  [ -r /dev/kvm ] && [ -w /dev/kvm ]
  ls -l /dev/kvm
}

prepare_host() {
  sudo -n apt-get update -qq
  sudo -n apt-get install -y -qq --no-install-recommends "${PACKAGES[@]}"
  if sysctl -n kernel.apparmor_restrict_unprivileged_userns >/dev/null 2>&1; then
    sudo -n sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
  fi
  # Unprivileged guests must traverse storage paths under this home.
  sudo -n chmod 0711 "$HOME"
  # The test-vms host check requires these nodes; nothing has loaded their modules yet.
  sudo -n modprobe vsock
  sudo -n modprobe vhost_vsock
  [ -c /dev/vsock ] && [ -c /dev/vhost-vsock ]
  free_disk
}

# Hosted runners come with either 82 or 13 GB free, and building a base reserves three VM disks.
# None of these preinstalled tools is used here; Go stays because setup-go installed it there.
free_disk() {
  df -h /
  sudo -n find /opt/hostedtoolcache -mindepth 1 -maxdepth 1 ! -name go -exec rm -rf {} +
  sudo -n rm -rf /usr/local/lib/android /usr/share/dotnet /usr/local/.ghcup /usr/share/swift \
    /usr/lib/jvm /usr/lib/google-cloud-sdk /usr/local/share/powershell /usr/local/share/chromium \
    /usr/local/lib/node_modules /opt/microsoft /opt/google /opt/az /usr/share/miniconda \
    /usr/local/julia* /usr/local/share/vcpkg /usr/share/kotlinc /usr/local/aws-cli \
    /usr/local/aws-sam-cli /usr/share/gradle* /usr/share/apache-maven* /usr/share/sbt /opt/pipx \
    /usr/lib/firefox /usr/share/az_* /usr/share/rust "$HOME/.rustup" "$HOME/.cargo" \
    /usr/local/share/boost /opt/mssql-tools /usr/lib/mono /usr/lib/llvm-16 /usr/lib/llvm-17 \
    /usr/lib/llvm-18 /etc/skel/.rustup /etc/skel/.cargo /home/packer
  df -h /
  # What is left, for the next trim.
  sudo -n du -xh -d 3 / 2>/dev/null | sort -h | tail -n 30
}

drop_docker() {
  sudo -n systemctl stop docker.socket docker.service containerd.service
  sudo -n systemctl disable docker.socket docker.service containerd.service
  sudo -n rm -rf /var/lib/docker /var/lib/containerd
  sudo -n ip link delete docker0 2>/dev/null || true
  sudo -n iptables -P FORWARD ACCEPT
  sudo -n iptables -F DOCKER-USER 2>/dev/null || true
  sudo -n iptables -S FORWARD | head -5
}

install_candidate() {
  local bundle="$1"
  sudo -n install -d -o "$(id -un)" -g "$(id -gn)" -m 0755 /srv/workspaces /srv/workspaces/Subyard "$WORKSPACE"
  tar -xzf "$bundle" -C "$WORKSPACE"
  [ -f "$WORKSPACE/.subyard-acceptance/candidate.json" ]
  jq -er .version "$WORKSPACE/.subyard-acceptance/candidate.json"
}

# Through the public installer, like an operator's install. The published 0.25.5 has no
# `yard service`, so the pool comes from the candidate itself.
install_runtime() {
  local release="$WORKSPACE/.subyard-acceptance/release" version
  version="$(jq -er .version "$WORKSPACE/.subyard-acceptance/candidate.json")"
  env YARD_RELEASE_BASE_URL="file://$release" bash "$release/subyard-install.sh" --version "$version" --yes
  "$HOME/.local/bin/yard" --version
}

# One slot per job. The reserve leaves a 9 GiB pair admissible on a 16 GiB runner.
# The documented path for a host without Incus: init installs it, then the service is registered.
# This process predates the incus-admin group that init creates.
install_pool() {
  local free_gib
  install -d -m 0700 "$HOME/.config/subyard"
  cat >> "$HOME/.config/subyard/config.env" <<'CONFIG'
CODING_TOOL_INTEGRATIONS=''
E2E_VM_SLOT_COUNT=1
E2E_MEMORY_RESERVE=2GiB
E2E_DISK_RESERVE=2GiB
CONFIG
  # The default 20 GiB disk needs 62 GiB to build a base; the small runner disk has less.
  free_gib="$(df --output=avail -BG "$HOME" | tail -n 1 | tr -dc 0-9)"
  if [ "$free_gib" -lt 65 ]; then
    printf 'E2E_VM_DISK=12GiB\n' >> "$HOME/.config/subyard/config.env"
    printf 'only %s GiB free: using 12 GiB VM disks\n' "$free_gib"
  fi
  "$HOME/.local/bin/yard" -Y test-yard init --profile test-vms --yes </dev/null
  incus_root --version
  sg incus-admin -c '"$HOME/.local/bin/yard" service register test-vms --yard test-yard --yes </dev/null'
  sg incus-admin -c '"$HOME/.local/bin/yard" test-vms status </dev/null'
}

controller_env() {
  env SUBYARD_E2E_ROUTE_REGISTRY="$HOME/.subyard/e2e/routes" \
    SUBYARD_E2E_CANDIDATE_BUNDLE="$BUNDLE" \
    SUBYARD_E2E_CANDIDATE_SHA256="$(sha256sum "$BUNDLE" | awk '{print $1}')" \
    SUBYARD_E2E_CONTROLLER_WORKSPACE="$WORKSPACE" \
    PYTHONDONTWRITEBYTECODE=1 "$@"
}

run_check() {
  local status=0
  cd "$WORKSPACE"
  controller_env "$@" </dev/null || status=$?
  # The runner is gone afterwards, and only these events hold a refusal's or quarantine's reason.
  # Checks lease through either the service route or the yard route, with separate histories.
  [ "$status" = 0 ] || controller_env dev/agent-e2e.sh --logs --slot 1 -n 50 </dev/null \
    || controller_env dev/agent-e2e.sh --yard test-yard --logs --slot 1 -n 50 </dev/null || true
  return "$status"
}

check_probes() {
  local id="$1"
  BUNDLE="$2"
  shift 2
  : > "$RESULTS"
  probe facts facts
  probe kvm-udev enable_kvm
  probe prepare-host prepare_host
  probe docker-removed drop_docker
  probe_after prepare-host candidate install_candidate "$BUNDLE"
  probe_after candidate candidate-runtime install_runtime
  probe_after candidate-runtime test-vms-service install_pool
  probe_after test-vms-service "check:$id" run_check "$@"
  probe facts-after facts
  write_summary
}

require_runner
case "${1:-}" in
  check)
    [ "$#" -ge 4 ] || die 'usage: periodic-e2e.sh check ID BUNDLE COMMAND...'
    shift
    check_probes "$@"
    ;;
  *) die 'usage: periodic-e2e.sh check ID BUNDLE COMMAND...' ;;
esac
