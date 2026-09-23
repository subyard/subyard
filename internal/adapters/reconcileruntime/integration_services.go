package reconcileruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/shellquote"
)

// Service retirement always proves ownership before stopping anything. Binaries,
// auth and history remain installed; only managed startup is disabled.
func (runtime Runtime) integrationServices(ctx context.Context, mode string) (string, bool, error) {
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"sh", "-eu", "-c", `
mode=$1
selected=" $2 "
allowed=$3
changed=0
conflict() { printf 'subyard-integration-conflict:%s\n' "$1" >&2; exit 1; }
case "$selected" in *' paseo '*) ;; *)
  unit=/etc/systemd/system/paseo.service
  receipt=/var/lib/subyard/paseo-ownership
  if [ -e "$unit" ] || [ -L "$unit" ]; then
    if ! { [ -f "$unit" ] && [ ! -L "$unit" ] &&
      [ -f "$receipt" ] && [ ! -L "$receipt" ] &&
      [ "$(stat -c '%u:%g:%a' "$receipt")" = 0:0:600 ] &&
      [ "$(head -n 1 "$receipt")" = "# subyard-paseo-ownership-v1" ] &&
      [ "$(wc -l < "$receipt")" = 2 ] &&
      [ "$(tail -n 1 "$receipt" | cut -c67-)" = "$unit" ] &&
      sha256sum --status -c "$receipt"; }; then
      conflict paseo
    fi
    sha256sum "$unit"
    if systemctl is-active --quiet paseo.service || systemctl is-enabled --quiet paseo.service; then
      changed=1
      [ "$mode" != apply ] || systemctl disable --now paseo.service
    fi
  fi
;; esac
case "$selected" in *' aiobserver '*) ;; *)
  if [ -e /etc/subyard/ai-observer/managed ] || [ -e /etc/systemd/system/subyard-ai-observer.service ]; then
    /usr/local/bin/ai-observer assess-disable >/dev/null || conflict aiobserver
    sha256sum /etc/subyard/ai-observer/managed /etc/systemd/system/subyard-ai-observer.service
    if systemctl is-active --quiet subyard-ai-observer.service || systemctl is-enabled --quiet subyard-ai-observer.service; then
      changed=1
      [ "$mode" != apply ] || /usr/local/bin/ai-observer disable
    fi
  fi
;; esac
if [ "$allowed" = false ] && { [ -e /usr/local/bin/ccusage ] || [ -L /usr/local/bin/ccusage ]; }; then
  receipt=/var/lib/subyard/ccusage-ownership
  if ! { [ -f "$receipt" ] && [ ! -L "$receipt" ] &&
    [ "$(stat -c '%u:%g:%a' "$receipt")" = 0:0:600 ] &&
    [ "$(head -n 1 "$receipt")" = "# subyard-ccusage-ownership-v1" ] &&
    [ "$(wc -l < "$receipt")" = 2 ] &&
    [ "$(tail -n 1 "$receipt" | cut -c67-)" = /usr/local/bin/ccusage ] &&
    [ -f /usr/local/bin/ccusage ] && [ ! -L /usr/local/bin/ccusage ] &&
    sha256sum --status -c "$receipt"; }; then
    conflict ccusage
  fi
  sha256sum /usr/local/bin/ccusage
  changed=1
  if [ "$mode" = apply ]; then rm -- /usr/local/bin/ccusage "$receipt"; fi
fi
printf 'changed=%s\n' "$changed"
`, "subyard", mode, runtime.environmentValue("CODING_TOOL_INTEGRATIONS"), runtime.environmentDefault("ALLOWS_CODING_TOOLS", "true")}})
	if err != nil || result.ExitCode != 0 {
		for _, line := range strings.Split(string(result.Stderr), "\n") {
			switch line {
			case "subyard-integration-conflict:paseo":
				return "", false, fmt.Errorf("integration paseo is not selected in CODING_TOOL_INTEGRATIONS, but /etc/systemd/system/paseo.service remains and ownership cannot be verified using /var/lib/subyard/paseo-ownership; service preserved (releases before 0.15 did not create this receipt)%s", runtime.integrationCleanupHint("paseo"))
			case "subyard-integration-conflict:aiobserver":
				return "", false, fmt.Errorf("integration aiobserver is not selected in CODING_TOOL_INTEGRATIONS, but its managed marker or systemd service remains and ownership cannot be verified; service preserved. Inspect /etc/subyard/ai-observer/managed and /etc/systemd/system/subyard-ai-observer.service in the yard before explicitly retiring the unwanted service%s", runtime.integrationCleanupHint("aiobserver"))
			case "subyard-integration-conflict:ccusage":
				return "", false, errors.New("this yard has ALLOWS_CODING_TOOLS=false, but /usr/local/bin/ccusage remains and ownership cannot be verified using /var/lib/subyard/ccusage-ownership; utility preserved. Inspect its installation before explicitly retiring the unwanted utility")
			}
		}
		return "", false, errors.New("integration service or utility ownership is unknown or changed")
	}
	return string(result.Stdout), strings.Contains(string(result.Stdout), "changed=1"), nil
}

func (runtime Runtime) integrationCleanupHint(id string) string {
	if runtime.environmentValue("AGENT_"+id+"_CLEANUP") == "" {
		return ""
	}
	yard := runtime.Yard.YardName
	if yard == "" {
		yard = "default"
	}
	executable := "yard"
	// An update can fail before the candidate becomes current. Resolve its
	// pinned /proc/.../fd root to the published path, which survives updater exit.
	if runtime.RepositoryRoot != "" {
		if engine, err := filepath.EvalSymlinks(filepath.Join(runtime.RepositoryRoot, "bin", "yard-engine")); err == nil {
			if info, err := os.Stat(engine); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
				executable = engine
			}
		}
	}
	command := []string{executable, "-Y", yard, "integration", "cleanup", id}
	return ". If this integration is no longer needed, inspect its profile-owned cleanup plan on the owner host:\n" +
		shellquote.Command(append(slices.Clone(command), "--check")) + "\nThen run with confirmation:\n" + shellquote.Command(command)
}
func (runtime Runtime) retireIntegrationServices(ctx context.Context) error {
	_, _, err := runtime.integrationServices(ctx, "apply")
	return err
}
