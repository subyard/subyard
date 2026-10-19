//go:build linux

package releaseruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

type rustArtifactFact struct {
	Release        releasetransition.ReleaseID   `json:"release"`
	Version        string                        `json:"version"`
	ArtifactDigest releasetransition.Fingerprint `json:"artifactDigest"`
	EngineDigest   string                        `json:"engineDigest"`
	Root           releaseowner.Identity         `json:"root"`
}

type rustSourceCheckpoint struct {
	SourceRelease    releasetransition.ReleaseID   `json:"sourceRelease"`
	ArtifactDigest   releasetransition.Fingerprint `json:"artifactDigest"`
	Root             releaseowner.Identity         `json:"root"`
	CapabilityRaw    []byte                        `json:"capabilityRaw"`
	CapabilityDigest string                        `json:"capabilityDigest"`
}

type rustConfigLayout struct {
	ManifestRaw           []byte                         `json:"manifestRaw"`
	ManifestDigest        *string                        `json:"manifestDigest"`
	CommandManifestDigest *releasetransition.Fingerprint `json:"commandManifestDigest"`
	CommandOptionsRaw     []byte                         `json:"commandOptionsRaw"`
	CommandOptionsStatus  string                         `json:"commandOptionsStatus"`
}

type rustOwnerOptions struct {
	Replacement         *releasetransition.JournalReplacement `json:"replacement"`
	MigrationSources    *rustMigrationSources                 `json:"migrationSources,omitempty"`
	OriginalSources     *rustMigrationSources                 `json:"originalSources,omitempty"`
	originalRoots       []*os.File
	DataHome            string                                   `json:"dataHome"`
	ConfigHome          string                                   `json:"configHome"`
	RuntimeRoot         string                                   `json:"runtimeRoot"`
	RepositoryRoot      string                                   `json:"repositoryRoot"`
	Yard                string                                   `json:"yard"`
	Releases            releasetransition.ReleasePair            `json:"releases"`
	ArtifactDigest      releasetransition.Fingerprint            `json:"artifactDigest"`
	RegistryRaw         []byte                                   `json:"registryRaw"`
	Catalog             []releasetransition.CapabilityDescriptor `json:"catalog"`
	InheritedSettingIDs []string                                 `json:"inheritedSettingIds"`
	RollbackTarget      *releasetransition.RollbackTarget        `json:"rollbackTarget"`
	MigrationCheckpoint bool                                     `json:"migrationCheckpoint"`
	SourceIngress       *releasetransition.SourceIngressRequest  `json:"sourceIngress"`
	SourceCheckpoint    *rustSourceCheckpoint                    `json:"sourceCheckpoint"`
	SourceArtifact      *rustArtifactFact                        `json:"sourceArtifact"`
	TargetArtifact      *rustArtifactFact                        `json:"targetArtifact"`
	PreviousArtifact    *rustArtifactFact                        `json:"previousArtifact"`
	CandidateVersion    string                                   `json:"candidateVersion"`
	ConfigLayout        *rustConfigLayout                        `json:"configLayout"`
}

type rustMigrationSources struct {
	Source   rustArtifactFact  `json:"source"`
	Previous *rustArtifactFact `json:"previous"`
}

func verifiedRustArtifactFact(candidate *verifiedPublishedCandidate) (rustArtifactFact, error) {
	identity, err := releaseowner.IdentityOf(candidate.root)
	if err != nil {
		return rustArtifactFact{}, err
	}
	return rustArtifactFact{Release: candidate.candidate.release, Version: candidate.version,
		ArtifactDigest: candidate.manifestDigest, EngineDigest: candidate.producerEvidence.EngineSHA256,
		Root: identity}, nil
}

func (runtime *Runtime) rustOwnerFacts(ctx context.Context, assets, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest) (rustOwnerOptions, *verifiedPublishedCandidate, config.PersistentFileSnapshot, error) {
	var options rustOwnerOptions
	links, err := releasetransition.NewRuntimeLinkStore(request.RuntimeRoot)
	if err != nil {
		return options, nil, config.PersistentFileSnapshot{}, err
	}
	observed, err := links.Observe()
	if err != nil {
		return options, nil, config.PersistentFileSnapshot{}, err
	}
	pair := releasetransition.ReleasePair{From: observed.Active, Previous: observed.Previous, Target: request.Target}
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		return options, nil, config.PersistentFileSnapshot{}, err
	}
	journal, err := store.ReadCurrentJournal()
	if err != nil {
		return options, nil, config.PersistentFileSnapshot{}, err
	}
	var journalRecord *releasetransition.JournalRecord
	if journal.Exists {
		checked, err := releasetransition.ParseJournal(journal.Payload)
		if err != nil {
			return options, nil, config.PersistentFileSnapshot{}, err
		}
		journalRecord = &checked
		if checked.Checkpoint != releasetransition.JournalComplete && request.MigrationOwnerReplacement == nil && request.Replacement == nil {
			pair = checked.Releases
			pair.Target = request.Target
		}
	}
	source, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: pair.From,
		root: filepath.Join(request.RuntimeRoot, "releases", string(pair.From))}, request.RuntimeRoot, nil)
	if err != nil {
		return options, nil, config.PersistentFileSnapshot{}, err
	}
	fail := func(err error) (rustOwnerOptions, *verifiedPublishedCandidate, config.PersistentFileSnapshot, error) {
		source.Close()
		for _, root := range options.originalRoots {
			root.Close()
		}
		return rustOwnerOptions{}, nil, config.PersistentFileSnapshot{}, err
	}
	sourceFact, err := verifiedRustArtifactFact(source)
	if err != nil {
		return fail(err)
	}
	targetFact, err := verifiedRustArtifactFact(target)
	if err != nil {
		return fail(err)
	}
	var previousFact *rustArtifactFact
	if pair.Previous != nil {
		previous, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: *pair.Previous,
			root: filepath.Join(request.RuntimeRoot, "releases", string(*pair.Previous))}, request.RuntimeRoot, nil)
		if err != nil {
			return fail(err)
		}
		fact, err := verifiedRustArtifactFact(previous)
		previous.Close()
		if err != nil {
			return fail(err)
		}
		previousFact = &fact
	}
	registry, err := openCandidateFile(int(assets.root.Fd()), "config/release-transition.json")
	if err != nil {
		return fail(err)
	}
	raw, err := io.ReadAll(io.LimitReader(registry, maximumCandidateResponseBytes+1))
	closeErr := registry.Close()
	if err != nil || closeErr != nil || len(raw) > maximumCandidateResponseBytes ||
		hex.EncodeToString(sha256Sum(raw)) != string(assets.registryDigest) {
		return fail(errors.New("verified Rust owner registry changed"))
	}
	options = rustOwnerOptions{
		Replacement: request.Replacement,
		DataHome:    runtime.config.Inputs.value("SUBYARD_HOME", runtime.config.Context), ConfigHome: request.ConfigHome,
		RuntimeRoot: request.RuntimeRoot, Yard: request.Yard, Releases: pair, ArtifactDigest: request.ArtifactDigest,
		RegistryRaw: slices.Clone(raw), Catalog: releasetransition.BuiltinCapabilityCatalog().Descriptors(),
		InheritedSettingIDs: append([]string{}, request.InheritedSettingIDs...), SourceIngress: request.SourceIngress,
		SourceArtifact: &sourceFact, TargetArtifact: &targetFact, PreviousArtifact: previousFact, CandidateVersion: target.version,
	}
	if journalRecord != nil {
		// Syntax-only original IDs are optional physical facts. Only Rust can
		// require them for a checked pending cleanup; old GC must not prevent
		// an otherwise clean completed no-op from opening.
		capture := func(release releasetransition.ReleaseID) (rustArtifactFact, bool) {
			verified := source
			if release == target.candidate.release {
				verified = target
			} else if release != source.candidate.release {
				var err error
				verified, err = runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: release,
					root: filepath.Join(request.RuntimeRoot, "releases", string(release))}, request.RuntimeRoot, nil)
				if err != nil {
					return rustArtifactFact{}, false
				}
				defer verified.Close()
			}
			fact, err := verifiedRustArtifactFact(verified)
			if err != nil {
				return rustArtifactFact{}, false
			}
			root, err := duplicateReleaseFile(verified.root)
			if err != nil {
				return rustArtifactFact{}, false
			}
			options.originalRoots = append(options.originalRoots, root)
			return fact, true
		}
		if fact, ok := capture(journalRecord.Releases.From); ok {
			options.OriginalSources = &rustMigrationSources{Source: fact}
			if journalRecord.Releases.Previous != nil {
				if previous, ok := capture(*journalRecord.Releases.Previous); ok {
					options.OriginalSources.Previous = &previous
				}
			}
		}
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
	}
	if request.MigrationOwnerReplacement != nil && journalRecord != nil {
		capture := func(release releasetransition.ReleaseID) (rustArtifactFact, error) {
			verified, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: release,
				root: filepath.Join(request.RuntimeRoot, "releases", string(release))}, request.RuntimeRoot, nil)
			if err != nil {
				return rustArtifactFact{}, err
			}
			defer verified.Close()
			return verifiedRustArtifactFact(verified)
		}
		fact, err := capture(journalRecord.Releases.From)
		if err != nil {
			return fail(err)
		}
		options.MigrationSources = &rustMigrationSources{Source: fact}
		if journalRecord.Releases.Previous != nil {
			previous, err := capture(*journalRecord.Releases.Previous)
			if err != nil {
				return fail(err)
			}
			options.MigrationSources.Previous = &previous
		}
	}
	if options.DataHome == "" {
		options.DataHome = filepath.Join(runtime.config.Inputs.value("HOME", runtime.config.Context), ".subyard")
	}
	if !filepath.IsAbs(options.DataHome) {
		return fail(errors.New("Rust release owner data home must be absolute"))
	}
	if source.checkpointRaw != nil {
		if _, err := verifiedMigrationCheckpointReader(source); err != nil {
			return fail(err)
		}
		options.SourceCheckpoint = &rustSourceCheckpoint{SourceRelease: pair.From, ArtifactDigest: source.manifestDigest,
			Root: sourceFact.Root, CapabilityRaw: slices.Clone(source.checkpointRaw), CapabilityDigest: hex.EncodeToString(source.checkpointDigest[:])}
	}
	if assets.checkpointRaw != nil {
		if _, err := verifiedMigrationCheckpointReader(assets); err != nil {
			return fail(err)
		}
		options.MigrationCheckpoint = true
	}
	if request.Direction == releasetransition.DirectionActivatePrevious {
		options.RollbackTarget = &releasetransition.RollbackTarget{Version: target.version, RegistryDigest: target.registryDigest}
		layout, snapshot, err := runtime.rustConfigLayoutFacts(ctx, target, request)
		if err != nil {
			return fail(err)
		}
		options.ConfigLayout = &layout
		return options, source, snapshot, nil
	}
	return options, source, config.PersistentFileSnapshot{}, nil
}

func sha256Sum(raw []byte) []byte {
	digest := sha256.Sum256(raw)
	return digest[:]
}

func (runtime *Runtime) rustConfigLayoutFacts(ctx context.Context, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest) (rustConfigLayout, config.PersistentFileSnapshot, error) {
	facts := rustConfigLayout{CommandOptionsStatus: "unavailable", CommandOptionsRaw: []byte{}}
	snapshot, err := config.ReadPersistentFileSnapshot(request.ConfigHome, filepath.Join(request.ConfigHome, ".sync/manifest.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return facts, snapshot, err
	}
	if snapshot.Exists {
		if len(snapshot.Content) > maximumCandidateResponseBytes {
			return facts, snapshot, errors.New("configuration layout manifest exceeds the inspection bound")
		}
		facts.ManifestRaw = slices.Clone(snapshot.Content)
		digest := hex.EncodeToString(sha256Sum(snapshot.Content))
		facts.ManifestDigest = &digest
	}
	if target.commandManifestBound {
		digest := target.commandManifestDigest
		facts.CommandManifestDigest = &digest
		var output boundedResponseBuffer
		err := runtime.runVerifiedCandidate(ctx, target, request.RuntimeRoot, []string{"--command-options", "config"}, nil, &output, "")
		if ctx.Err() != nil {
			return facts, snapshot, ctx.Err()
		}
		if err == nil && output.Len() <= maximumCandidateResponseBytes {
			facts.CommandOptionsRaw = slices.Clone(output.Bytes())
			facts.CommandOptionsStatus = "ok"
		}
	}
	return facts, snapshot, nil
}
