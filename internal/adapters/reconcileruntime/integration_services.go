package reconcileruntime

import (
	"context"
	_ "embed"
	"errors"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/shellquote"
)

//go:embed bwrap_policy.sh
var bubblewrapPolicy string

// Integration services are reconciled even when the yard selects no coding tools.
func (runtime Runtime) integrationServices(ctx context.Context, mode string) (string, bool, error) {
	setting := runtime.environmentDefault("BWRAP_APPARMOR", "on")
	selected := strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS"))
	claude, bubblewrap := "0", "0"
	if slices.Contains(selected, "claude") {
		claude = "1"
	}
	if claude == "1" || slices.Contains(selected, "codex") {
		bubblewrap = "1"
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"sh", "-eu", "-c", `
mode=$1
allowed=$2
conflict() { printf 'subyard-integration-conflict:%s\n' "$1" >&2; exit 1; }
ccusage_status=0
(
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
  activation_receipt=/usr/local/bin/ccusage.subyard-runtime.json
  if [ -e "$activation_receipt" ] || [ -L "$activation_receipt" ]; then
    [ -f "$activation_receipt" ] && [ ! -L "$activation_receipt" ] &&
      [ "$(stat -c '%u:%g:%a' "$activation_receipt")" = 0:0:600 ] &&
      jq -e --arg binary "$(sha256sum /usr/local/bin/ccusage | cut -d' ' -f1)" \
        '.schema == 1 and .binary == $binary and (.version | type == "string") and (.archive | test("^[0-9a-f]{64}$"))' "$activation_receipt" >/dev/null || conflict ccusage
    sha256sum "$activation_receipt" || exit 1
  fi
  sha256sum /usr/local/bin/ccusage || exit 1
  if [ "$mode" = apply ]; then
    rm -- /usr/local/bin/ccusage "$receipt" && rm -f -- "$activation_receipt" || exit 1
  fi
  printf 'changed=1\n'
else
  printf 'changed=0\n'
fi
) || ccusage_status=$?
if [ "$ccusage_status" != 0 ]; then mode=observe; fi
bubblewrap_status=0
bash -c "$3" subyard "$mode" "$4" "$5" "$6" "$7" "$8" || bubblewrap_status=$?
[ "$ccusage_status" = 0 ] && [ "$bubblewrap_status" = 0 ]
`, "subyard", mode, runtime.environmentDefault("ALLOWS_CODING_TOOLS", "true"), bubblewrapPolicy, setting, runtime.devUser(), claude, string(runtime.Yard.YardKind), bubblewrap}})
	if err != nil || result.ExitCode != 0 {
		yard := runtime.Yard.YardName
		if yard == "" {
			yard = "default"
		}
		retry := "run " + shellquote.Command([]string{"yard", "-Y", yard, "integration", "status"})
		var refusals []error
		for _, line := range strings.Split(string(result.Stderr), "\n") {
			if line == "subyard-integration-conflict:ccusage" {
				refusals = append(refusals, integrationOwnershipError{message: "this yard has ALLOWS_CODING_TOOLS=false, but /usr/local/bin/ccusage remains and ownership cannot be verified using /var/lib/subyard/ccusage-ownership; utility preserved. Inspect its installation before explicitly retiring the unwanted utility", retry: retry})
			}
			if strings.HasPrefix(line, "Bubblewrap AppArmor: ") {
				refusals = append(refusals, errors.New(line))
			}
		}
		if refusal := domain.CollectRefusals(refusals...); refusal != nil {
			return "", false, refusal
		}
		return "", false, errors.New("integration service or utility ownership is unknown or changed")
	}
	output := string(result.Stdout)
	lines := strings.Split(output, "\n")
	changed := slices.Contains(lines, "changed=1") || slices.Contains(lines, "bwrap=install") ||
		slices.Contains(lines, "bwrap=retire") || slices.Contains(lines, "bwrap=repair") ||
		slices.Contains(lines, "bwrap=enable") || slices.Contains(lines, "bwrap=cleanup")
	return output, changed, nil
}

func (runtime Runtime) reconcileIntegrationServices(ctx context.Context) error {
	_, _, err := runtime.integrationServices(ctx, "apply")
	return err
}
