package reconcileruntime

import (
	"context"
	_ "embed"
	"errors"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/shellquote"
)

//go:embed bwrap_policy.sh
var bubblewrapPolicy string

var errBubblewrapSelection = errors.New("BWRAP_APPARMOR=off requires deselecting Codex; keep BWRAP_APPARMOR=on or remove codex from CODING_TOOL_INTEGRATIONS")

// ValidateBubblewrapSelection rejects a selection incompatible with Codex's
// provisioning check before setup can change the yard.
func ValidateBubblewrapSelection(setting, selection string) error {
	if setting == "off" && slices.Contains(strings.Fields(selection), "codex") {
		return errBubblewrapSelection
	}
	return nil
}

// Integration services are reconciled even when the yard selects no coding tools.
func (runtime Runtime) integrationServices(ctx context.Context, mode string) (string, bool, error) {
	setting := runtime.environmentDefault("BWRAP_APPARMOR", "on")
	selection := runtime.environmentValue("CODING_TOOL_INTEGRATIONS")
	settingsErr := ValidateBubblewrapSelection(setting, selection)
	if mode == "apply" && settingsErr != nil {
		return "", false, settingsErr
	}
	selected := strings.Fields(selection)
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
		refusals := []error{settingsErr}
		for _, line := range guestLines(result.Stderr) {
			if line.Matches("subyard-integration-conflict:ccusage") {
				refusals = append(refusals, integrationOwnershipError{message: "this yard has ALLOWS_CODING_TOOLS=false, but /usr/local/bin/ccusage remains and ownership cannot be verified using /var/lib/subyard/ccusage-ownership; utility preserved. Inspect its installation before explicitly retiring the unwanted utility", retry: retry})
			}
			if line.wraps(bubblewrapRefusalPrefix, "") {
				refusals = append(refusals, bubblewrapPolicyRefusal{line: bubblewrapRefusalLine(line), yard: yard, retry: retry})
			}
		}
		if len(refusals) == 1 {
			refusals = append(refusals, errors.New("integration service or utility ownership is unknown or changed"))
		}
		return "", false, domain.CollectRefusals(refusals...)
	}
	if settingsErr != nil {
		return "", false, settingsErr
	}
	output := string(result.Stdout)
	lines := strings.Split(output, "\n")
	changed := slices.Contains(lines, "changed=1") || slices.Contains(lines, "bwrap=install") ||
		slices.Contains(lines, "bwrap=retire") || slices.Contains(lines, "bwrap=repair") ||
		slices.Contains(lines, "bwrap=enable") || slices.Contains(lines, "bwrap=cleanup")
	return output, changed, nil
}

const (
	bubblewrapRefusalPrefix = "Bubblewrap AppArmor: "
	bubblewrapRefusalSuffix = ". Inspect the yard policy and restore the original Subyard files from backup or resolve the administrator policy before rerunning yard setup."
)

// bubblewrapRefusalReasons lists every fixed refusal of bwrap_policy.sh with
// its paths, which are the program's own constants.
func bubblewrapRefusalReasons() []string {
	const (
		policyParent, policy = "/opt/subyard", "/opt/subyard/bwrap-apparmor"
		recordParent, marker = "/var/lib/subyard", "/var/lib/subyard/bwrap-apparmor-ownership"
		pending              = marker + ".pending"
		unit                 = "/etc/systemd/system/subyard-bwrap-apparmor.service"
		link                 = "/etc/systemd/system/multi-user.target.wants/subyard-bwrap-apparmor.service"
		legacyUnit           = "/etc/systemd/system/subyard-codex-bwrap.service"
	)
	reasons := []string{
		"ownership of " + unit + " cannot be verified", "ownership of " + link + " cannot be verified",
		"the systemd unit lookup paths could not be verified", "AppArmor is not in a yard policy namespace",
		"the yard cannot inspect its AppArmor profiles", "the owned boot loader " + policy + "/load is missing or changed",
		"unexpected entry in the owned policy directory " + policy, "the owned policy directory " + policy + " is missing or changed",
		"the developer's Bubblewrap sandbox could not create user and network namespaces", "the yard cannot load AppArmor profiles",
		"both ownership records " + marker + " and " + pending + " exist", "boot registration exists without the policy directory",
		"existing policy or boot registration has no ownership record at " + marker, "the boot loader has no verified ownership or is disabled",
		"could not disable " + unit, "could not compile the distribution Bubblewrap profiles",
	}
	for _, path := range []string{unit, pending, policy, marker} {
		reasons = append(reasons, path+" appeared during reconciliation")
	}
	for _, parent := range []string{policyParent, recordParent} {
		reasons = append(reasons, "ownership of the parent directory "+parent+" cannot be verified")
	}
	for _, record := range []string{marker, pending} {
		reasons = append(reasons, "the ownership record "+record+" is missing or invalid", "the two profile hashes in "+record+" do not match the files")
	}
	for _, name := range []string{"unpriv_bwrap", "bwrap"} {
		reasons = append(reasons, "the owned file "+policy+"/"+name+" is missing or changed",
			"the loaded AppArmor profile "+name+" cannot be verified as Subyard-owned",
			"could not load AppArmor profile "+name, "could not undo loading AppArmor profile "+name)
	}
	for _, directory := range []string{"", "local/", "disable/", "force-complain/"} {
		for _, name := range []string{"bwrap", "bwrap-userns-restrict", "unpriv_bwrap"} {
			reasons = append(reasons, "administrator AppArmor policy at /etc/apparmor.d/"+directory+name)
		}
	}
	for _, old := range []string{"/opt/subyard/codex/apparmor", legacyUnit, "/etc/systemd/system/multi-user.target.wants/subyard-codex-bwrap.service"} {
		reasons = append(reasons, "legacy policy at "+old+" has no ownership record")
	}
	return reasons
}

// bubblewrapRefusalLine replaces a guest refusal line with the matching fixed
// host text. An override found in a systemd unit path keeps only its class.
func bubblewrapRefusalLine(line guestText) string {
	for _, reason := range bubblewrapRefusalReasons() {
		if text := bubblewrapRefusalPrefix + reason + bubblewrapRefusalSuffix; line.Matches(text) {
			return text
		}
	}
	if line.wraps(bubblewrapRefusalPrefix+"administrator systemd override at /", bubblewrapRefusalSuffix) {
		return bubblewrapRefusalPrefix + "administrator systemd override in a systemd unit path" + bubblewrapRefusalSuffix
	}
	return bubblewrapRefusalPrefix + "the yard policy program refused with an unrecognized reason" + bubblewrapRefusalSuffix
}

// bubblewrapPolicyRefusal is one refusal of the yard's Bubblewrap policy
// program, as fixed host text. Its bounded text is the public release diagnostic.
type bubblewrapPolicyRefusal struct{ line, yard, retry string }

func (err bubblewrapPolicyRefusal) Error() string { return err.line }

func (err bubblewrapPolicyRefusal) ActivationDiagnostic() (string, string) {
	message := "yard " + err.yard + ": " + err.line
	// One yard's invalid text must not invalidate the joined release message.
	if len(err.yard) > 63 || !domain.SafeName(err.yard) || len(message) > 480 || !utf8.ValidString(message) ||
		strings.TrimSpace(message) != message || strings.ContainsFunc(message, unicode.IsControl) {
		return "", err.retry
	}
	return message, err.retry
}

func (runtime Runtime) reconcileIntegrationServices(ctx context.Context) error {
	_, _, err := runtime.integrationServices(ctx, "apply")
	return err
}
