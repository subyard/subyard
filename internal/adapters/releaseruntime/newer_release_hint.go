package releaseruntime

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/blang/semver/v4"
)

// newerReleaseProbeTimeout bounds the channel lookup that runs only after a
// failed resume, so an unreachable channel cannot delay the failure report.
const newerReleaseProbeTimeout = 10 * time.Second

// NewerRelease names a published release newer than the target of an
// unfinished update, with the explicit command that asks it to replace the
// journal. Only that release's own inspection decides whether it qualifies.
type NewerRelease struct {
	Version string
	Command string
}

// NewerRelease looks up the stable channel after an ordinary resume failed.
// It returns nothing for other preparations, an unreachable channel, or a
// journal that the newer-release replacement contract excludes.
func (prepared Prepared) NewerRelease(ctx context.Context) NewerRelease {
	if prepared.newerRelease == nil || ctx.Err() != nil {
		return NewerRelease{}
	}
	return prepared.newerRelease(ctx)
}

// newerReleaseProbe leaves explicit selections alone: they already chose
// their release, or deliberately stay offline.
func (runtime *Runtime) newerReleaseProbe(parsed options, configHome string) func(context.Context) NewerRelease {
	if parsed.offline || parsed.check || parsed.rollback || parsed.force || parsed.version != "" || parsed.tag != "" {
		return nil
	}
	return func(ctx context.Context) NewerRelease {
		ctx, cancel := context.WithTimeout(ctx, newerReleaseProbeTimeout)
		defer cancel()
		tag, err := runtime.latestTag(ctx, parsed.repository)
		if err != nil {
			return NewerRelease{}
		}
		version := strings.TrimPrefix(tag, "v")
		requested, err := semver.Parse(version)
		if err != nil || !safeVersion(version) || "v"+version != tag {
			return NewerRelease{}
		}
		journal, _, ok := replaceableJournal(configHome, requested)
		if !ok || journal.Goal.Direction != releasetransition.DirectionActivateTarget ||
			releasetransition.IsRecoveryTransaction(journal.Transaction) ||
			strings.HasPrefix(string(journal.Transaction), releasetransition.MigrationOwnerTransactionPrefixV1) {
			return NewerRelease{}
		}
		command := "yard update --version " + version
		if parsed.rootExplicit {
			command += " --runtime-root " + shellWord(parsed.root)
		}
		return NewerRelease{Version: version, Command: command}
	}
}

// replaceableJournal reads the current journal when it is an unfinished
// update whose pinned target is older than requested and that no exact owner
// contract or source migration keeps.
func replaceableJournal(
	configHome string,
	requested semver.Version,
) (releasetransition.JournalRecord, releasetransition.ProtectedSnapshot, bool) {
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		return releasetransition.JournalRecord{}, releasetransition.ProtectedSnapshot{}, false
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return releasetransition.JournalRecord{}, releasetransition.ProtectedSnapshot{}, false
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || journal.Checkpoint == releasetransition.JournalComplete {
		return releasetransition.JournalRecord{}, releasetransition.ProtectedSnapshot{}, false
	}
	pinned, err := releasetransition.ReleaseIDVersion(journal.Goal.Target)
	if err != nil || !requested.GT(pinned) || negotiatedRecoveryRelease(journal.Goal.Target) ||
		journal.SourceIngress != nil {
		return releasetransition.JournalRecord{}, releasetransition.ProtectedSnapshot{}, false
	}
	return journal, snapshot, true
}

var plainShellWord = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=-]+$`)

func shellWord(value string) string {
	if plainShellWord.MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
