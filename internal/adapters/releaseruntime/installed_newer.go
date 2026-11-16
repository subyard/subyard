package releaseruntime

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/blang/semver/v4"
)

// installedVersionNewerThan reports whether the active release is a newer
// semver than a discovered release. Plain update installs only a release that
// is not older; a release ID without a version keeps the ordinary path.
func installedVersionNewerThan(current releasetransition.ReleaseID, discovered string) (semver.Version, bool) {
	installed, err := releasetransition.ReleaseIDVersion(current)
	if err != nil {
		return semver.Version{}, false
	}
	latest, err := semver.Parse(discovered)
	if err != nil {
		return semver.Version{}, false
	}
	return installed, installed.GT(latest)
}

// InstalledNewer is the additive update report for a plain update whose
// discovered latest release is older than the active one.
type InstalledNewer struct {
	InstalledVersion string   `json:"installedVersion"`
	LatestVersion    string   `json:"latestVersion"`
	Message          string   `json:"message"`
	Downgrade        []string `json:"downgrade"`
}

type installedNewerError struct {
	cause  error
	report InstalledNewer
}

func (err installedNewerError) Error() string {
	return err.cause.Error() + "; " + err.report.Message
}

func (err installedNewerError) Unwrap() error { return err.cause }

// prepareInstalledNewer never selects the older discovered release; a
// downgrade needs an explicit --version. Only a ready protected inspection or
// an installation without any journal is a successful no-op. Any other
// inspection result keeps its ordinary failure or check report.
func (runtime *Runtime) prepareInstalledNewer(
	parsed options,
	links runtimeLinkSnapshot,
	installed semver.Version,
	protected *protectedTransitionInspection,
	inspectionErr error,
) (Prepared, error) {
	downgrade := "yard update --version " + parsed.version
	if parsed.rootExplicit {
		downgrade += " --runtime-root " + shellWord(parsed.root)
	}
	report := InstalledNewer{
		InstalledVersion: installed.String(), LatestVersion: parsed.version,
		Message: fmt.Sprintf(
			"installed version %s is newer than the latest published release %s; no release was selected",
			installed, parsed.version,
		),
		Downgrade: []string{downgrade + " --check", downgrade},
	}
	var outcome releasetransition.Outcome
	switch {
	case inspectionErr != nil:
		var public publicReleaseInspectionError
		if parsed.check && errors.As(inspectionErr, &public) {
			return runtime.prepareInstalledNewerCheck(parsed, public.outcome, report), nil
		}
		_, err := runtime.inspectionFailure(parsed, inspectionErr)
		return Prepared{}, installedNewerError{cause: err, report: report}
	case protected != nil:
		if protected.inspection.Outcome == nil {
			return Prepared{}, errors.New("protected release inspection has no outcome")
		}
		outcome = *protected.inspection.Outcome
	default:
		// No journal: the installed release has never completed its protected
		// transition, and migrate owns that same-release inspection.
		active := releaseLinksFromRuntimeSnapshot(links)
		outcome = releasetransition.Outcome{
			Status: releasetransition.StatusMigrationRequired, Active: active.Active, Previous: active.Previous,
			Target: active.Active, Code: releasetransition.CodeTransitionRequired,
			Message: "the installed release has no protected release transition journal",
			Retry:   "run yard migrate --check",
		}
	}
	if parsed.check {
		return runtime.prepareInstalledNewerCheck(parsed, outcome, report), nil
	}
	if protected != nil && outcome.Status != releasetransition.StatusReady {
		return Prepared{}, installedNewerError{cause: transitionOutcomeError(outcome), report: report}
	}
	return Prepared{
		Action: "update.check",
		run: func(context.Context) error {
			return writeInstalledNewer(runtime.config.Stdout, outcome, report)
		},
	}, nil
}

func (runtime *Runtime) prepareInstalledNewerCheck(parsed options, outcome releasetransition.Outcome, report InstalledNewer) Prepared {
	return Prepared{
		Action: "update.check",
		run: func(context.Context) error {
			return runtime.writeInspection(parsed, struct {
				Outcome        releasetransition.Outcome   `json:"outcome"`
				InstalledNewer InstalledNewer              `json:"installedNewer"`
				Configuration  []config.ConfigurationState `json:"configuration,omitempty"`
			}{outcome, report, runtime.configurationState()})
		},
	}
}

func writeInstalledNewer(output io.Writer, outcome releasetransition.Outcome, report InstalledNewer) error {
	text := fmt.Sprintf("\n  [ ok ] No update installed\n  %s\n  Status: %s\n  Active release: %s\n",
		report.Message, outcome.Status, outcome.Active)
	if outcome.Previous != nil {
		text += fmt.Sprintf("  Previous release: %s\n", *outcome.Previous)
	}
	text += fmt.Sprintf("  %s\n", outcome.Message)
	for _, warning := range outcome.Warnings {
		text += fmt.Sprintf("  Warning: %s\n", warning)
	}
	if outcome.Retry != "" {
		text += fmt.Sprintf("  Next: %s\n", outcome.Retry)
	}
	text += "  To install the older release, review and apply an explicit downgrade:\n"
	for _, command := range report.Downgrade {
		text += "    " + command + "\n"
	}
	_, err := io.WriteString(output, text)
	return err
}
