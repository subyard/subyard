package releaseruntime

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

// These exact published Linux runtime manifests share the six-kind catalog.
// The pins were captured from the v0.28.0, v0.28.1 and v0.29.0 release archives.
// A version or registry digest alone does not identify compiled capabilities.
var publishedRollbackCatalogs = map[releasetransition.Fingerprint]string{
	"61a748799b79c37cdb4b93d9edad7796951dd1d26683358548fd1591deba790b": "0.28.0",
	"09af91ee3bf49ba2d49627797fe2582669571bea55cffcef5833e8f72acdf226": "0.28.0",
	"580274b5a5607dd0a523ad7bf5f9d8c5e17094eacbb8b27bf7b3577c6b635423": "0.28.1",
	"732e8c77497f31b64e13864c6c7fbf68999c45f276dff0dde1e3e04ca797da54": "0.28.1",
	"4591220831f78d5893a50bdc2ff59e4f21f09a2b5a4f99a15390cb1107eb6cdb": "0.29.0",
	"46f7d72290c6dea090f944728d1a5e7bd72a792e7d1c458498d73fa1819116eb": "0.29.0",
}

func migrationKindRollbackFindings(target *verifiedPublishedCandidate, source, retained releasetransition.RegistryV2, ledger releasetransition.LedgerV2) []string {
	published := publishedRollbackCatalogs[target.manifestDigest] == target.version && target.version != ""
	var publishedCatalog releasetransition.CapabilityCatalog
	if published {
		publishedCatalog = releasetransition.PublishedSixKindCapabilityCatalog()
	}

	domains := slices.AppendSeq(slices.Collect(maps.Keys(ledger.Domains)), maps.Keys(retained.MinimumEpochs))
	slices.Sort(domains)
	var findings []string
	for _, domain := range slices.Compact(domains) {
		state, applied := ledger.Domains[domain]
		minimum, hasMinimum := retained.MinimumEpochs[domain]
		maximum, hasMaximum := retained.CurrentEpochs[domain]
		if !applied || !hasMinimum || !hasMaximum {
			findings = append(findings, fmt.Sprintf("cannot verify migration domain %s in both the active history and retained registry", domain))
		} else if state.Epoch < minimum || state.Epoch > maximum {
			message := fmt.Sprintf("retained release %s cannot read %s epoch %d (readable epochs %d to %d)", target.version, domain, state.Epoch, minimum, maximum)
			if domain == "power-metadata" && state.Epoch >= 4 && maximum < 4 {
				message += "; this crosses the NetworkManager device guard boundary at epoch 4 and older runtimes can replace the guard during init"
			}
			findings = append(findings, message)
		}

		_, sourceRetired := source.RetiredPrefixes[domain]
		targetPrefix, targetRetired := retained.RetiredPrefixes[domain]
		prefixMatches := false
		if (sourceRetired || targetRetired) && applied && hasMinimum && hasMaximum && state.Epoch >= minimum && state.Epoch <= maximum {
			sourceDigest, sourceErr := source.MigrationPrefixDigest(domain, state.Epoch)
			targetDigest, targetErr := retained.MigrationPrefixDigest(domain, state.Epoch)
			prefixMatches = sourceErr == nil && targetErr == nil && sourceDigest == targetDigest
			if !prefixMatches {
				findings = append(findings, fmt.Sprintf("retired migration history for %s does not match through active epoch %d", domain, state.Epoch))
			}
		}

		// Keep refusing unsupported pending registrations as well. Selecting
		// a retained runtime can run init before those migrations are applied.
		for _, migration := range source.Migrations {
			if migration.Domain != domain || (prefixMatches && targetRetired && migration.ToEpoch <= targetPrefix.ThroughEpoch) {
				continue
			}
			supported := publishedCatalog.Supports(migration.Kind, domain)
			if !published {
				supported = slices.ContainsFunc(retained.Migrations, func(known releasetransition.MigrationDefinitionV2) bool {
					return known.Kind == migration.Kind && known.Domain == domain
				})
			}
			if !supported {
				findings = append(findings, fmt.Sprintf("retained release %s has no verified support for migration %s (kind %s) in %s", target.version, migration.ID, migration.Kind, domain))
			}
		}
	}
	return findings
}

func (runtime *Runtime) inspectMigrationKindRollback(ctx context.Context, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest, links runtimeLinkSnapshot) ([]string, error) {
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		return []string{"the active migration history store cannot be verified"}, err
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return []string{"the active release transition journal cannot be read"}, err
	}
	if snapshot.Exists {
		journal, err := releasetransition.ParseJournal(snapshot.Payload)
		if err != nil {
			return []string{"the active release transition journal cannot be verified"}, err
		}
		// Accepted journals keep their exact recovery contract.
		if journal.Checkpoint != releasetransition.JournalComplete && journal.Goal.Target == request.Target && journal.Goal.Direction == request.Direction && journal.ArtifactDigest == request.ArtifactDigest {
			return nil, nil
		}
	}
	const sourceRefusal = "the active release's sealed migration inventory cannot be verified; restore the exact active release files"
	if !links.current.present {
		return []string{sourceRefusal}, nil
	}
	sourceRoot := filepath.Join(request.RuntimeRoot, links.current.target)
	source, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: releaseLinksFromRuntimeSnapshot(links).Active, root: sourceRoot}, request.RuntimeRoot, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return []string{sourceRefusal}, err
	}
	defer source.Close()
	if source.registryDigest == "" {
		return []string{"the active release has no sealed migration registry to verify its history"}, nil
	}
	registry, _, err := readVerifiedTransitionRegistry(source)
	if err != nil {
		return []string{sourceRefusal}, nil
	}
	if target.registryDigest == "" {
		return []string{"the retained release has no sealed migration registry to verify its readable epochs"}, nil
	}
	retained, retainedDigest, err := readVerifiedTransitionRegistry(target)
	if err != nil {
		return []string{"the retained release's sealed migration registry cannot be verified"}, nil
	}

	state, err := store.ReadMigrationLedger(registry, source.registryDigest)
	if err != nil {
		return []string{"the active migration ledger or checkpoint cannot be verified"}, err
	}
	if !state.AuthoritySnapshot.Exists {
		return []string{"the authoritative active migration history is missing"}, nil
	}
	findings := migrationKindRollbackFindings(target, registry, retained, state.Ledger)
	if !state.Checkpointed && (len(registry.RetiredPrefixes) != 0 || len(retained.RetiredPrefixes) != 0) {
		findings = append(findings, "retired migration history requires an authoritative active checkpoint")
	}
	if len(findings) != 0 {
		return findings, nil
	}
	if !state.Checkpointed {
		if _, _, err := releasetransition.ParseLedgerV2(state.ProjectionSnapshot.Payload, retained); err != nil {
			return []string{"the retained registry cannot read the authoritative active migration ledger"}, err
		}
		return nil, nil
	}
	retainedState, err := releasetransition.ParseMigrationLedger(state.ProjectionSnapshot, state.AuthoritySnapshot, retained, retainedDigest)
	if err != nil {
		return []string{"the retained registry cannot read the authoritative active migration checkpoint"}, err
	}
	checkpointReader, err := verifiedMigrationCheckpointReader(target)
	if err != nil {
		return []string{"the retained release's migration checkpoint capability cannot be verified"}, err
	}
	if !checkpointReader && retainedState.DiffersFromProjection(retained) {
		return []string{"the retained release requires a legacy ledger that matches the authoritative migration checkpoint"}, nil
	}
	return nil, nil
}
