package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestTeardownSessionGuardAllowsUnfinishedAndUnavailableGuestsProtectsActive(t *testing.T) {
	helper, err := filepath.Abs("../../scripts/lib/teardown-session.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"absent", "unavailable", "uninstalled", "idle", "active", "unsupported", "malformed", "fence-failed", "pause-failed", "probe-unavailable", "paused-active", "paused-cleared", "late-failure", "vanished", "stopped"} {
		t.Run(state, func(t *testing.T) {
			root := testkit.TempDir(t)
			if err := os.Mkdir(filepath.Join(root, "lib"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"lib/ssh-listener.sh", "vscode-remote-maintenance.sh"} {
				content, err := os.ReadFile(filepath.Join("../../scripts", name))
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, name), content, 0600)
			}
			profiles := filepath.Join(root, "profile-services.sh")
			testkit.WriteFile(t, profiles, []byte(`#!/bin/sh
printf '%s\n' "$1" >> "$GUARD_LOG"
[ "$GUARD_STATE:$1" != pause-failed:--pause ] || exit 9
case "$GUARD_STATE:$1" in paused-*:--pause|late-failure:--pause|vanished:--pause|stopped:--pause) printf 'fixture\n' ;; esac
`), 0700)
			log := filepath.Join(root, "events")
			script := `set -euo pipefail
have_incus=1; PROJ=(--project selected); SCRIPT_DIR="$1"; DEV_USER=missing-user
die() { printf '%s\n' "$*" >&2; exit 1; }
timeout() { [ "$1" = 5s ] || exit 9; shift; "$@"; }
sleep() { :; }
incus() {
  [ "$2" = selected ] && [ "$4" = selected ] || exit 9
  if [ "$1" = list ]; then
    if [ ! -e "$GUARD_LOG.power" ] && [ "$GUARD_STATE" != absent ]; then printf 'RUNNING\n'; fi
    if [ -e "$GUARD_LOG.power" ] && [ "$GUARD_STATE" = stopped ]; then printf 'STOPPED\n'; fi
    return 0
  fi
  [ "$GUARD_STATE" != unavailable ] || return 1
  case "$*" in
    *'systemctl show'*)
      case "$GUARD_STATE" in
        unsupported) printf 'unsupported\n' ;;
        malformed) printf 'invalid\n' ;;
        uninstalled) printf 'snapshot:0:0\n' ;;
        fence-failed) printf 'snapshot:1:1\n' ;;
        *) printf 'snapshot:1:0\n' ;;
      esac ;;
    *'systemctl start'*)
      if [ "$GUARD_STATE" = fence-failed ]; then
        case "$*" in *'--env SERVICE=1 --env SOCKET=1'*) ;; *) exit 9 ;; esac
      fi
      printf 'restore\n' >> "$GUARD_LOG"
      ;;
    *'systemctl stop'*) printf 'fence\n' >> "$GUARD_LOG"; [ "$GUARD_STATE" != fence-failed ] ;;
    *'check-ssh'*)
      [ "$GUARD_STATE" != probe-unavailable ] || return 1
      if [ "$GUARD_STATE" = paused-cleared ] && [ -e "$GUARD_LOG.probed" ]; then
        printf 'idle\n'
      else
        case "$GUARD_STATE" in active|paused-*) printf 'active\n' ;; *) printf 'idle\n' ;; esac
      fi
      : > "$GUARD_LOG.probed"
      printf 'probe\n' >> "$GUARD_LOG"
      ;;
  esac
}
. "$2"
teardown_session_guard selected
case "$GUARD_STATE" in
  late-failure|vanished|stopped)
    PROJ=(--project replacement); YARD_INSTANCE_NAME=replacement
    [ "$GUARD_STATE" = late-failure ] || : > "$GUARD_LOG.power"
    die 'later cleanup failed'
    ;;
esac
printf 'delete\n' >> "$GUARD_LOG"
trap - EXIT
`
			child := exec.Command("bash", "-c", script, "guard", root, helper)
			child.Env = append(os.Environ(), "GUARD_STATE="+state, "GUARD_LOG="+log)
			output, err := child.CombinedOutput()
			blocked := state == "active" || state == "unsupported" || state == "malformed" || state == "fence-failed" || state == "pause-failed" || state == "paused-active" || state == "late-failure" || state == "vanished" || state == "stopped"
			if blocked && err == nil || !blocked && err != nil {
				t.Fatalf("state=%s err=%v output=%s", state, err, output)
			}
			content, _ := os.ReadFile(log)
			if strings.Contains(string(content), "delete") == blocked {
				t.Fatalf("wrong deletion decision for %s: %s", state, content)
			}
			if (state == "active" || state == "fence-failed" || state == "pause-failed" || state == "paused-active" || state == "late-failure") && !strings.Contains(string(content), "restore") {
				t.Fatalf("active refusal did not restore SSH: %s", content)
			}
			if !blocked && strings.Contains(string(content), "--resume") {
				t.Fatal("success resumed removed profile service")
			}
			if (state == "vanished" || state == "stopped" || state == "uninstalled") && (strings.Contains(string(content), "restore") || strings.Contains(string(content), "--resume")) {
				t.Fatalf("restored unavailable target or originally inactive listeners: %s", content)
			}
			if (state == "paused-active" || state == "late-failure") && !strings.Contains(string(content), "--resume") {
				t.Fatalf("failed teardown stranded paused owner service: %s", content)
			}
			if state == "paused-cleared" && strings.Count(string(content), "probe\n") != 2 {
				t.Fatalf("retry did not use the same SSH probe: %s", content)
			}
		})
	}
}
