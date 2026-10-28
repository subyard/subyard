# shellcheck shell=bash
set -euo pipefail
root=$1
install -d -m 0700 "$root"
printf 'subyard-git-fetch-loss-v1\n' > "$root/.marker"
printf -v quoted '%q' "$root"
printf 'loss_root=%s\n' "$quoted" > "$root/environment"
cat >> "$root/environment" <<'BASH_ENV'
python3() {
  local argument refresh=0 status
  for argument in "$@"; do [ "$argument" != custody-refresh ] || refresh=1; done
  if [ "$refresh" = 1 ]; then
    printf '%s %s\n' "$$" "$(awk '{print $22}' /proc/$$/stat)" > "$loss_root/paused"
    while [ ! -e "$loss_root/release" ]; do sleep .05; done
    if command python3 "$@"; then status=0; else status=$?; fi
    printf '%s\n' "$status" > "$loss_root/terminal"
    return "$status"
  fi
  command python3 "$@"
}
BASH_ENV
chmod 0600 "$root/environment"
