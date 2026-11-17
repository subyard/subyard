package releaseruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
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

func publishedRollbackSupports(kind, domain string) bool {
	switch kind {
	case "test-vms-settings-v1-to-v2", "local-ssh-port-settings-v2-to-v3":
		return domain == "settings"
	case "test-yard-owner-v1-to-v2", "registered-yards-v2-to-v3":
		return domain == "owner-registration"
	case "owner-publication-state-v1", "registered-guest-disk-tmp-v2-to-v3":
		return domain == "power-metadata"
	}
	return false
}

func migrationKindRollbackFindings(target *verifiedPublishedCandidate, source, retained releasetransition.RegistryV2, ledger *releasetransition.LedgerV2) []string {
	published := publishedRollbackCatalogs[target.manifestDigest] == target.version && target.version != ""
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
	for _, migration := range migrations {
		nmBoundary := migration.Kind == "nm-device-guard-v1" && migration.Domain == "power-metadata"
		if target.registryDigest == "" && !published && !nmBoundary {
			// The active owner keeps responsibility for legacy rollback policies.
			continue
		}
		supported := false
		if published {
			supported = publishedRollbackSupports(migration.Kind, migration.Domain)
		} else {
			// A registered kind is a lower bound, never the complete compiled catalog.
			for _, known := range retained.Migrations {
				supported = supported || known.Kind == migration.Kind && known.Domain == migration.Domain
			}
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

func (runtime *Runtime) inspectMigrationKindRollback(ctx context.Context, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest) ([]string, error) {
	if request.Direction != releasetransition.DirectionActivatePrevious {
		return nil, nil
	}
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
		// Accepted journals keep their exact recovery contract. This guard is for
		// fresh delegation, never a new admission floor for an authorized owner.
		if journal.Goal.Target == request.Target && journal.Goal.Direction == request.Direction && journal.ArtifactDigest == request.ArtifactDigest {
			return nil, nil
		}
	}
	links, err := runtime.inspectRuntimeLinks(request.RuntimeRoot)
	if err != nil {
		return nil, err
	}
	if !links.current.present {
		return nil, nil
	}
	sourceRoot := filepath.Join(request.RuntimeRoot, links.current.target)
	// Registryless sources retain their existing admission path. This check adds
	// diagnostics for registered kinds; it cannot authorize missing provenance.
	if _, err := os.Lstat(filepath.Join(sourceRoot, "config", "release-transition.json")); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	source, err := runtime.verifyPublishedCandidate(ctx, publishedCandidate{release: releasetransition.ReleaseID(filepath.Base(links.current.target)), root: sourceRoot}, request.RuntimeRoot, nil)
	if err != nil {
		return nil, fmt.Errorf("verify migration rollback source: %w", err)
	}
	defer source.Close()
	registry, _, err := readVerifiedTransitionRegistry(source)
	if err != nil {
		return nil, err
	}
	var retained releasetransition.RegistryV2
	if target.registryDigest != "" {
		retained, _, err = readVerifiedTransitionRegistry(target)
		if err != nil {
			return nil, err
		}
	}
	if findings := migrationKindRollbackFindings(target, registry, retained, nil); len(findings) == 0 {
		return nil, nil
	}
	state, historyErr := store.ReadMigrationLedger(registry, source.registryDigest)
	var ledger *releasetransition.LedgerV2
	if historyErr == nil {
		ledger = &state.Ledger
	}
	return migrationKindRollbackFindings(target, registry, retained, ledger), historyErr
}
