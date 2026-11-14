package releaseruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/blang/semver/v4"
)

const MinimumSourceVersion = "0.11.0"
const UpdateAnchorVersion = "0.28.0"

const LegacyReinstallGuidance = "Sources before v0.11.0 are unsupported. Stage verified current tooling with subyard-install.sh --version <version> --publish-only, then run the printed candidate's bin/yard uninstall --full. Full uninstall deletes this installation's owned data; review and confirm its removal plan. After verified cleanup, install a new version separately."

func sourceVersionError(version string) error {
	parsed, err := semver.Parse(version)
	if err != nil {
		return errors.New("the sealed source release version is invalid; restore a runtime with a valid semantic version, then run yard update --check")
	}
	if parsed.LT(semver.MustParse(MinimumSourceVersion)) {
		return errors.New(LegacyReinstallGuidance)
	}
	if parsed.LT(semver.MustParse(UpdateAnchorVersion)) {
		return fmt.Errorf("source-anchor-required: installed v%s. Direct update requires installed v%s or newer; next: yard update --version %s && yard update", version, UpdateAnchorVersion, UpdateAnchorVersion)
	}
	return nil
}

// unfinishedBelowAnchorError routes an unfinished transition only through the
// release that started it: plain yard update resumes its own journal on every
// supported release. It never predicts which journals another release replaces.
func unfinishedBelowAnchorError(journal releasetransition.JournalRecord) error {
	anchor := semver.MustParse(UpdateAnchorVersion)
	next := "yard update && yard update --version " + UpdateAnchorVersion + " && yard update"
	if target, err := releasetransition.ReleaseIDVersion(journal.Goal.Target); err == nil &&
		journal.Goal.Direction == releasetransition.DirectionActivateTarget && target.GTE(anchor) {
		next = "yard update && yard update"
	}
	return fmt.Errorf("source-anchor-required: an unfinished release transition must first be completed by the installed release that started it; next: %s\nThis release cannot replace it. If the installed release cannot finish it, a v%d.%d.x fix release is the repair path. Do not uninstall.", next, anchor.Major, anchor.Minor)
}

// VerifyLegacyUninstallSource recognizes the sealed historical project-creator
// contract, independently of migration support or journal readiness. This is
// read-only association evidence for uninstall assessment, never deletion authority.
func VerifyLegacyUninstallSource(ctx context.Context, runtimeRoot string) error {
	verified, err := verifyHistoricalUninstallSource(ctx, runtimeRoot)
	if err == nil {
		verified.Close()
	}
	return err
}

// HistoricalUninstallProducerEvidence reads sealed original producer data only.
// It never invokes historical setup or mutation handlers.
func HistoricalUninstallProducerEvidence(ctx context.Context, runtimeRoot string) (uninstall.ProducerEvidence, error) {
	verified, err := verifyHistoricalUninstallSource(ctx, runtimeRoot)
	if err != nil {
		return uninstall.ProducerEvidence{}, err
	}
	defer verified.Close()
	// The manifest binds the protected installed files, but cannot authenticate
	// an arbitrary replacement template that was locally rehashed. Admit only
	// these exact public producer contracts, also verified in released archives.
	contracts := map[string][]string{
		"subyard-power-reconcile.service.in": {"b26f847617207d9e7d81d3de2213fef89b214a25274b2fccc96723ff5f458ef6", "64f8489b10dc10fb63dc67b858c2cde6d39ed447c6752a50000981daa3afaf21"},
		"subyard-keys-sync.service.in":       {"04f14943eec7e1925a3bc3de241ea0bc738cf18542ae0f705e16b34fef25f194"},
		"subyard-keys-sync.timer.in":         {"b271fe9c185f508d1b388182ce64bd8def7628b92a0d7164a0a919d690e75137"},
	}
	for name, allowed := range contracts {
		digest := sha256.Sum256([]byte(verified.producerEvidence.Templates[name]))
		match := false
		for _, expected := range allowed {
			match = match || hex.EncodeToString(digest[:]) == expected
		}
		if !match {
			return uninstall.ProducerEvidence{}, errors.New("the historical producer template has no known public contract")
		}
	}
	// Bind interrupted replacement to this executing engine image, never to an
	// arbitrary path supplied through the environment.
	image, err := os.Open("/proc/self/exe")
	if err != nil {
		return uninstall.ProducerEvidence{}, err
	}
	defer image.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, image); err != nil {
		return uninstall.ProducerEvidence{}, err
	}
	verified.producerEvidence.ExecutingEngineSHA256 = hex.EncodeToString(digest.Sum(nil))
	return verified.producerEvidence, nil
}

func verifyHistoricalUninstallSource(ctx context.Context, runtimeRoot string) (*verifiedPublishedCandidate, error) {
	links, err := releasetransition.NewRuntimeLinkStore(runtimeRoot)
	if err != nil {
		return nil, err
	}
	observed, err := links.Observe()
	if err != nil {
		return nil, err
	}
	runtime := New(Config{})
	verified, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{
		release: observed.Active, root: filepath.Join(runtimeRoot, "releases", string(observed.Active)),
	}, runtimeRoot, nil)
	if err != nil {
		return nil, err
	}

	version, err := semver.Parse(verified.version)
	if err != nil || version.String() != verified.version ||
		!strings.HasPrefix(string(observed.Active), verified.version+"-") {
		verified.Close()
		return nil, errors.New("the active runtime does not prove legacy installation provenance")
	}
	// These public creator contracts shipped without installation ownership labels:
	// v0.10.0/v0.11.0/v0.11.2 and v0.17.3/v0.18.1/v0.18.6/v0.19.0. The verified
	// manifest binds the entire runtime and this exact script, not a version claim.
	switch hex.EncodeToString(verified.projectCreatorDigest[:]) {
	case "483638e0a3783c1c7aa6dbe38970e4e0099fe142d2710daf3558cb292958b660",
		"68a0b83c8ace53203e78620d8329ba541d53baf84b430b3d90ffe14a6bd2d8ef":
		return verified, nil
	default:
		verified.Close()
		return nil, errors.New("the active runtime has no known unlabelled project-creator contract")
	}
}

// CheckSourcePolicy verifies the sealed active source before a fresh transition.
// Exact accepted V2 owners retain their recovery contract even when their source
// predates the ordinary admission floor; the transition validates that binding.
func CheckSourcePolicy(ctx context.Context, request releasetransition.ProcessRequest) (refusal error) {
	var identityErr, unfinishedErr error
	var unfinished *releasetransition.JournalRecord
	activatedTakeover := false
	defer func() { refusal = domain.CollectRefusals(identityErr, unfinishedErr, refusal) }()
	if pending, err := config.PendingDeveloperIdentities(request.ConfigHome); err != nil {
		identityErr = err
	} else if len(pending) != 0 && request.Mode != releasetransition.ProcessInspect {
		identityErr = errors.New("developer identity transition is pending; finish plain init before replacing the runtime")
	}
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		return err
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return err
	}
	if snapshot.Exists {
		journal, err := releasetransition.ParseJournal(snapshot.Payload)
		if err != nil {
			return err
		}
		if journal.Checkpoint != releasetransition.JournalComplete &&
			journal.Goal.Target == request.Target && journal.Goal.Direction == request.Direction &&
			journal.ArtifactDigest == request.ArtifactDigest {
			return nil
		}
		if journal.Checkpoint != releasetransition.JournalComplete {
			unfinished = &journal
		}
	}
	links, err := releasetransition.NewRuntimeLinkStore(request.RuntimeRoot)
	if err != nil {
		return err
	}
	observed, err := links.Observe()
	if err != nil {
		return err
	}
	runtime := New(Config{})
	verified, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{
		release: observed.Active,
		root:    filepath.Join(request.RuntimeRoot, "releases", string(observed.Active)),
	}, request.RuntimeRoot, nil)
	if err != nil {
		return fmt.Errorf("verify source support: %w", err)
	}
	defer verified.Close()
	if !strings.HasPrefix(string(observed.Active), verified.version+"-") {
		return errors.New("source release identity does not match its sealed version")
	}
	admissionErr := sourceVersionError(verified.version)
	// Sources below the V2 floor cannot resume a V2 journal; they keep the
	// full-uninstall route whatever journal they hold.
	active, activeErr := semver.Parse(verified.version)
	belowFloor := activeErr == nil && active.LT(semver.MustParse(MinimumSourceVersion))
	if unfinished != nil && !belowFloor {
		anchor := semver.MustParse(UpdateAnchorVersion)
		if err := activeErr; err == nil && active.LT(anchor) {
			// This continuation replaces the version-only refusal for the same
			// cause; unrelated identity and provenance findings remain independent.
			unfinishedErr, admissionErr = unfinishedBelowAnchorError(*unfinished), nil
		} else if err == nil && unfinished.Goal.Direction == releasetransition.DirectionActivateTarget {
			// Match activatedReleaseLinks: the switch may have succeeded before
			// activation-intent advanced. The engine still owns replacement checks,
			// fresh consent and preserving the old journal, or its ordinary Retry.
			activatedTakeover = observed.Active == unfinished.Releases.Target &&
				observed.Previous != nil && *observed.Previous == unfinished.Releases.From
		}
	}
	if verified.registryDigest == "" {
		if unfinishedErr != nil {
			return nil
		}
		if admissionErr != nil {
			return admissionErr
		}
		// A supported version without a sealed registry is damaged, not a pre-V2
		// source: never point it, or an interrupted update, at data deletion.
		return errors.New("source release has no verified V2 registry provenance; restore its exact sealed runtime from a known-good backup; do not uninstall")
	}
	registryFile, err := openCandidateFile(int(verified.root.Fd()), "config/release-transition.json")
	if err != nil {
		return err
	}
	registryPayload, err := io.ReadAll(io.LimitReader(registryFile, 1<<20+1))
	registryFile.Close()
	if err != nil {
		return err
	}
	registry, registryDigest, err := releasetransition.ParseRegistryV2(registryPayload, releasetransition.BuiltinCapabilityCatalog())
	if err != nil || registryDigest != verified.registryDigest {
		return errors.New("source release has invalid or foreign V2 registry provenance")
	}
	if activatedTakeover {
		history, historyErr := store.ReadMigrationLedger(registry, registryDigest)
		var pending []releasetransition.MigrationDefinitionV2
		if historyErr == nil {
			pending, historyErr = registry.PendingPath(history.Ledger)
		}
		if historyErr != nil || !history.AuthoritySnapshot.Exists || len(pending) != 0 {
			return errors.New("replacement-authority-required: the activated release's migration history is missing, inconsistent, or incomplete; this release cannot replace its unfinished transition. Restore the exact protected history from a known-good backup, or use a fix release of the activated series; do not uninstall")
		}
	}
	// A just-installed development runtime has no predecessor or accepted state.
	// Its first self-activation is bootstrap, not admission of an older source.
	_, versionErr := semver.Parse(verified.version)
	if versionErr != nil && observed.Active == request.Target && observed.Previous == nil && !snapshot.Exists && verified.registryDigest != "" && request.SourceIngress == nil {
		ledger, err := store.ReadLedger()
		if err != nil {
			return err
		}
		checkpoint, err := store.ReadMigrationCheckpoint()
		if err != nil {
			return err
		}
		absent := !ledger.Exists && !checkpoint.Exists
		paths := []string{
			filepath.Join(request.ConfigHome, "migrations", "state.json"),
			filepath.Join(request.ConfigHome, "migrations", "transactions"),
			filepath.Join(filepath.Dir(request.RuntimeRoot), "recovery", "pre-go-source"),
		}
		if dataHome := os.Getenv("SUBYARD_HOME"); dataHome != "" {
			paths = append(paths, filepath.Join(dataHome, "recovery", "pre-go-source"))
		}
		for _, path := range paths {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				absent = false
			}
		}
		if absent {
			return nil
		}
	}
	return admissionErr
}
