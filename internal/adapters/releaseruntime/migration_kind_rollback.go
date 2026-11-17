package releaseruntime

import (
	"context"
	"fmt"
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

func migrationKindRollbackFindings(target *verifiedPublishedCandidate, source, retained releasetransition.RegistryV2, ledger *releasetransition.LedgerV2) []string {
	published := publishedRollbackCatalogs[target.manifestDigest] == target.version && target.version != ""
	var publishedCatalog releasetransition.CapabilityCatalog
	retiredThrough := make(map[string]int, len(retained.RetiredPrefixes))
	if published {
		publishedCatalog = releasetransition.PublishedSixKindCapabilityCatalog()
	} else {
		// As with a checkpoint, the exact prefix proof binds the retired
		// definitions through the stated epoch.
		for domain, retired := range retained.RetiredPrefixes {
			if digest, err := source.MigrationPrefixDigest(domain, retired.ThroughEpoch); err == nil && digest == retired.Digest {
				retiredThrough[domain] = retired.ThroughEpoch
			}
		}
	}
	migrations := slices.Clone(source.Migrations)
	slices.SortFunc(migrations, func(a, b releasetransition.MigrationDefinitionV2) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	var findings []string
	// Epoch 4 remains an irreversible reader boundary after its migration is
	// retired from the registry. The target's registry bounds what it can read.
	crossesNM := ledger != nil && ledger.Domains["power-metadata"].Epoch >= 4 && retained.CurrentEpochs["power-metadata"] < 4
	if crossesNM {
		findings = append(findings, fmt.Sprintf("retained release %s cannot read power-metadata epoch %d across the NetworkManager device guard boundary at epoch 4; older runtimes can replace the guard during init", target.version, ledger.Domains["power-metadata"].Epoch))
	}
	for _, migration := range migrations {
		nmBoundary := migration.Kind == "nm-device-guard-v1" && migration.Domain == "power-metadata"
		if nmBoundary && crossesNM {
			continue
		}
		if target.registryDigest == "" && !published && !nmBoundary {
			// The active owner keeps responsibility for legacy rollback policies.
			continue
		}
		supported := false
		if published {
			supported = publishedCatalog.Supports(migration.Kind, migration.Domain)
		} else {
			// A registered kind is a lower bound, never the complete compiled catalog.
			through, retired := retiredThrough[migration.Domain]
			supported = (retired && migration.ToEpoch <= through) ||
				slices.ContainsFunc(retained.Migrations, func(known releasetransition.MigrationDefinitionV2) bool {
					return known.Kind == migration.Kind && known.Domain == migration.Domain
				})
		}
		if supported {
			continue
		}
		state := "applied state could not be established"
		if ledger != nil {
			state = "the registration has not been applied"
			if ledger.Domains[migration.Domain].Epoch >= migration.ToEpoch {
				state = "the migration is applied"
			}
		}
		if published {
			findings = append(findings, fmt.Sprintf("release %s does not know migration %s (kind %s); %s; rollback is incompatible", target.version, migration.ID, migration.Kind, state))
		} else if nmBoundary {
			findings = append(findings, fmt.Sprintf("retained release %s cannot verify support for migration %s (kind %s); %s; rollback is incompatible", target.version, migration.ID, migration.Kind, state))
		} else {
			findings = append(findings, fmt.Sprintf("migration %s (kind %s): rollback compatibility with retained release %s cannot be established from its verified artifact; its registry is not a complete capability declaration", migration.ID, migration.Kind, target.version))
		}
		if nmBoundary {
			findings[len(findings)-1] += "; older runtimes can replace the NetworkManager device guard during init"
		}
	}
	return findings
}

func (runtime *Runtime) inspectMigrationKindRollback(ctx context.Context, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest, links runtimeLinkSnapshot) ([]string, error) {
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		return nil, err
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return nil, err
	}
	if snapshot.Exists {
		journal, err := releasetransition.ParseJournal(snapshot.Payload)
		if err != nil {
			return nil, err
		}
		// Accepted journals keep their exact recovery contract.
		if journal.Goal.Target == request.Target && journal.Goal.Direction == request.Direction && journal.ArtifactDigest == request.ArtifactDigest {
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
		return nil, nil
	}
	registry, _, err := readVerifiedTransitionRegistry(source)
	if err != nil {
		return []string{sourceRefusal}, nil
	}
	var retained releasetransition.RegistryV2
	if target.registryDigest != "" {
		retained, _, err = readVerifiedTransitionRegistry(target)
		if err != nil {
			return []string{"the retained release's sealed migration registry cannot be verified"}, nil
		}
	}

	findings := migrationKindRollbackFindings(target, registry, retained, nil)
	checkNMEpoch := registry.CurrentEpochs["power-metadata"] >= 4 && retained.CurrentEpochs["power-metadata"] < 4
	if len(findings) == 0 && !checkNMEpoch {
		return nil, nil
	}
	state, historyErr := store.ReadMigrationLedger(registry, source.registryDigest)
	if historyErr != nil {
		if checkNMEpoch && len(findings) == 0 {
			findings = append(findings, "the NetworkManager device guard epoch cannot be verified from active migration history")
		}
		return findings, historyErr
	}
	return migrationKindRollbackFindings(target, registry, retained, &state.Ledger), nil
}
