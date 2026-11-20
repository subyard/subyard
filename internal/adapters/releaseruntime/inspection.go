package releaseruntime

import (
	"context"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

type observationKey struct{}

// observing marks an installation observation: its unauthorized release
// launches try the owner lock once instead of waiting for it.
func observing(ctx context.Context) bool {
	observed, _ := ctx.Value(observationKey{}).(bool)
	return observed
}

// InspectInstalled shares migrate --check's read-only inspection of the
// installed release, including the activation owners of every local yard. It
// never selects, downloads or activates a release, and never waits for the
// owner lock.
func (runtime *Runtime) InspectInstalled(ctx context.Context, configHome string) []domain.Inspected {
	prepared, err := runtime.PrepareCurrentTransition(context.WithValue(ctx, observationKey{}, true), []string{"--check"}, configHome, "default")
	if err != nil {
		return []domain.Inspected{{Key: "read", State: domain.InspectedUnknown, Resource: "release", Message: err.Error(), Next: "yard migrate --check"}}
	}
	return prepared.currentReport.inspect()
}

func (report currentReport) inspect() []domain.Inspected {
	var result []domain.Inspected
	seen := map[string]int{}
	for _, blocker := range report.Blockers {
		state := domain.InspectedBlocked
		if blocker.PendingConfiguration() {
			state = domain.InspectedPending
		}
		key := string(blocker.Code) + "." + domain.InspectionName(blocker.Resource)
		if position, present := seen[key]; present {
			result[position].Message += "; " + blocker.Message
			result[position].Next = first(result[position].Next, blocker.Retry)
			continue
		}
		seen[key] = len(result)
		result = append(result, domain.Inspected{Key: key, State: state, Resource: blocker.Resource, Message: blocker.Message, Next: blocker.Retry})
	}
	if report.Outcome.Status != releasetransition.StatusReady && len(report.Blockers) == 0 {
		state := domain.InspectedDrift
		if report.Outcome.Status == releasetransition.StatusOperatorActionRequired {
			state = domain.InspectedBlocked
		}
		result = append(result, domain.Inspected{Key: "readiness", State: state, Resource: "release", Installed: string(report.Current),
			Desired: string(report.Outcome.Target), Message: first(report.Outcome.Message, string(report.Outcome.Status)),
			Next: first(report.Next, first(report.Outcome.Retry, "yard migrate"))})
	}
	// Release warnings carry no resource identity: one notice keeps them all
	// without inventing message-derived keys.
	if len(report.Outcome.Warnings) > 0 {
		result = append(result, domain.Inspected{Key: "notices", State: domain.InspectedNotice, Resource: "release", Message: strings.Join(report.Outcome.Warnings, "; ")})
	}
	return result
}
