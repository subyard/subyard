package releaseruntime

import (
	"context"
	"fmt"
	"io"

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

// prepareInstalledNewer reports a plain update whose discovered latest release
// is older than the active one. Nothing is downloaded or changed; a downgrade
// needs an explicit --version. Readiness comes from the protected inspection
// when there is one, otherwise migrate owns the installed release's readiness.
func (runtime *Runtime) prepareInstalledNewer(
	parsed options,
	links runtimeLinkSnapshot,
	installed semver.Version,
	protected *protectedTransitionInspection,
) Prepared {
	active := releaseLinksFromRuntimeSnapshot(links)
	note := fmt.Sprintf(
		"installed version %s is newer than the latest published release %s; nothing to update",
		installed, parsed.version,
	)
	outcome := releasetransition.Outcome{
		Status: releasetransition.StatusMigrationRequired, Active: active.Active, Previous: active.Previous,
		Target: active.Active, Code: releasetransition.CodeTransitionRequired,
		Retry: "run yard migrate --check",
	}
	if protected != nil && protected.inspection.Outcome != nil {
		outcome = *protected.inspection.Outcome
	}
	outcome.Message = note
	downgrade := "yard update --version " + parsed.version
	if parsed.rootExplicit {
		downgrade += " --runtime-root " + shellWord(parsed.root)
	}
	if parsed.check {
		return runtime.preparePublicInspectionOutcome(parsed, outcome)
	}
	return Prepared{
		Action: "update.check",
		run: func(context.Context) error {
			return writeInstalledNewer(runtime.config.Stdout, outcome, downgrade)
		},
	}
}

func writeInstalledNewer(output io.Writer, outcome releasetransition.Outcome, downgrade string) error {
	_, err := fmt.Fprintf(output, "\n  [ ok ] No update installed: %s\n  Status: %s\n  Active release: %s\n",
		outcome.Message, outcome.Status, outcome.Active)
	if err == nil && outcome.Retry != "" {
		_, err = fmt.Fprintf(output, "  Next: %s\n", outcome.Retry)
	}
	if err == nil {
		_, err = fmt.Fprintf(output, "  To install the older release, review and apply an explicit downgrade:\n    %s --check\n    %s\n",
			downgrade, downgrade)
	}
	return err
}
