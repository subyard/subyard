package reconcileruntime

import (
	"context"
	"errors"
	"strings"

	"github.com/Subyard/Subyard/internal/ports"
)

// Service retirement always proves ownership before stopping anything. Binaries,
// auth and history remain installed; only managed startup is disabled.
func (runtime Runtime) integrationServices(ctx context.Context, mode string) (string, bool, error) {
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"sh", "-eu", "-c", `
mode=$1
selected=" $2 "
allowed=$3
changed=0
case "$selected" in *' paseo '*) ;; *)
  unit=/etc/systemd/system/paseo.service
  receipt=/var/lib/subyard/paseo-ownership
  if [ -e "$unit" ] || [ -L "$unit" ]; then
    [ -f "$unit" ] && [ ! -L "$unit" ]
    [ -f "$receipt" ] && [ ! -L "$receipt" ]
    [ "$(stat -c '%u:%g:%a' "$receipt")" = 0:0:600 ]
    [ "$(head -n 1 "$receipt")" = "# subyard-paseo-ownership-v1" ]
    [ "$(wc -l < "$receipt")" = 2 ]
    [ "$(tail -n 1 "$receipt" | cut -c67-)" = "$unit" ]
    sha256sum --status -c "$receipt"
    sha256sum "$unit"
    if systemctl is-active --quiet paseo.service || systemctl is-enabled --quiet paseo.service; then
      changed=1
      [ "$mode" != apply ] || systemctl disable --now paseo.service
    fi
  fi
;; esac
case "$selected" in *' aiobserver '*) ;; *)
  if [ -e /etc/subyard/ai-observer/managed ] || [ -e /etc/systemd/system/subyard-ai-observer.service ]; then
    /usr/local/bin/ai-observer assess-disable >/dev/null
    sha256sum /etc/subyard/ai-observer/managed /etc/systemd/system/subyard-ai-observer.service
    if systemctl is-active --quiet subyard-ai-observer.service || systemctl is-enabled --quiet subyard-ai-observer.service; then
      changed=1
      [ "$mode" != apply ] || /usr/local/bin/ai-observer disable
    fi
  fi
;; esac
if [ "$allowed" = false ] && { [ -e /usr/local/bin/ccusage ] || [ -L /usr/local/bin/ccusage ]; }; then
  receipt=/var/lib/subyard/ccusage-ownership
  [ -f "$receipt" ] && [ ! -L "$receipt" ]
  [ "$(stat -c '%u:%g:%a' "$receipt")" = 0:0:600 ]
  [ "$(head -n 1 "$receipt")" = "# subyard-ccusage-ownership-v1" ]
  [ "$(wc -l < "$receipt")" = 2 ]
  [ "$(tail -n 1 "$receipt" | cut -c67-)" = /usr/local/bin/ccusage ]
  [ -f /usr/local/bin/ccusage ] && [ ! -L /usr/local/bin/ccusage ]
  sha256sum --status -c "$receipt"
  sha256sum /usr/local/bin/ccusage
  changed=1
  if [ "$mode" = apply ]; then rm -- /usr/local/bin/ccusage "$receipt"; fi
fi
printf 'changed=%s\n' "$changed"
`, "subyard", mode, runtime.environmentValue("CODING_TOOL_INTEGRATIONS"), runtime.environmentDefault("ALLOWS_CODING_TOOLS", "true")}})
	if err != nil || result.ExitCode != 0 {
		return "", false, errors.New("integration service or utility ownership is unknown or changed")
	}
	return string(result.Stdout), strings.Contains(string(result.Stdout), "changed=1"), nil
}
func (runtime Runtime) retireIntegrationServices(ctx context.Context) error {
	_, _, err := runtime.integrationServices(ctx, "apply")
	return err
}
