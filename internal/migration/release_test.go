package migration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReleaseMigrationPrepareCommitRollbackAndRollForward(t *testing.T) {
	options, source, destination := releaseMigrationFixture(t)

	report, err := CheckRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Layout != 1 || report.TargetLayout != 2 ||
		len(report.RequiredMigrations) != 1 || report.Changed {
		t.Fatalf("unexpected check report: %#v", report)
	}
	if _, err := os.Lstat(migrationRoot(options.ConfigHome)); !os.IsNotExist(err) {
		t.Fatal("migration check changed the config home")
	}

	report, err = ApplyRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Pending || report.Phase != "prepared" {
		t.Fatalf("unexpected apply report: %#v", report)
	}
	assertFileContents(t, source, "TOKEN=legacy\n")
	assertFileContents(t, destination, "TOKEN=legacy\n")
	repeated, err := ApplyRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !repeated.Pending || repeated.Phase != "prepared" {
		t.Fatalf("repeated apply did not resume prepared work: %#v", repeated)
	}
	state, err := readAppliedState(options.ConfigHome, 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Layout != 1 {
		t.Fatalf("prepared state layout = %d, want 1", state.Layout)
	}
	if changed, err := FinalizeActive(context.Background(), options); err != nil || changed {
		t.Fatalf("inactive candidate finalized: changed=%v err=%v", changed, err)
	}

	activateFixtureRelease(t, options)
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("active candidate did not finalize: changed=%v err=%v", changed, err)
	}
	if _, err := os.Lstat(source); !os.IsNotExist(err) {
		t.Fatal("finalize retained the legacy source")
	}
	assertFileContents(t, destination, "TOKEN=legacy\n")
	state, err = readAppliedState(options.ConfigHome, 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Layout != 2 ||
		state.CurrentRelease != filepath.Join("releases", filepath.Base(options.RepositoryRoot)) {
		t.Fatalf("committed state = %#v", state)
	}

	report, err = RollbackRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Changed || report.Layout != 1 {
		t.Fatalf("unexpected rollback report: %#v", report)
	}
	assertFileContents(t, source, "TOKEN=legacy\n")
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("rollback retained the new destination")
	}
	swapFixtureRuntimeLinks(t, options.RuntimeRoot)

	if _, err := ApplyRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	activateFixtureRelease(t, options)
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("roll-forward did not finalize: changed=%v err=%v", changed, err)
	}
	if _, err := os.Lstat(source); !os.IsNotExist(err) {
		t.Fatal("roll-forward retained the legacy source")
	}
	assertFileContents(t, destination, "TOKEN=legacy\n")
}

func TestCleanupRetainsRolledBackTransactionAcrossOlderRuntimePair(t *testing.T) {
	options, _, _ := releaseMigrationFixture(t)
	olderRepository := filepath.Join(
		options.RuntimeRoot,
		"releases",
		"1.0.0-test-release",
	)
	olderRegistryPath := filepath.Join(olderRepository, "config", "migrations.json")
	if err := os.MkdirAll(filepath.Dir(olderRegistryPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRegistryFixture(t, olderRegistryPath, Registry{
		SchemaVersion: 1,
		MinimumLayout: 1,
		CurrentLayout: 1,
	})

	if _, err := ApplyRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	activateFixtureRelease(t, options)
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("finalize: changed=%v err=%v", changed, err)
	}
	if _, err := RollbackRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	swapFixtureRuntimeLinks(t, options.RuntimeRoot)

	olderOptions := options
	olderOptions.RegistryPath = olderRegistryPath
	olderOptions.RepositoryRoot = olderRepository
	olderOptions.Version = "1.0.0-test"
	if removed, err := CleanupRelease(olderOptions); err != nil || removed != 0 {
		t.Fatalf("cleanup retained rollback pair: removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(
		transactionDirectory(options.ConfigHome, options.Version),
		"transaction.json",
	)); err != nil {
		t.Fatalf("cleanup removed the retained rollback transaction: %v", err)
	}
}

func TestReleaseCheckKeepsProjectAndRegistryMigrations(t *testing.T) {
	options, _, _ := releaseMigrationFixture(t)
	projects := filepath.Join(t.TempDir(), "projects")
	writeMigrationFixture(
		t, filepath.Join(projects, "legacy-a.json"),
		`{"schema":1,"projectId":"legacy-a","name":"Demo","hostPath":"/one/Demo","yardPath":"/srv/workspaces/legacy-a/src","mode":"sync","sshHost":"yard"}`+"\n",
	)
	writeMigrationFixture(
		t, filepath.Join(projects, "legacy-b.json"),
		`{"schema":1,"projectId":"legacy-b","name":"demo","hostPath":"/two/Demo","yardPath":"/srv/workspaces/legacy-b/src","mode":"sync","sshHost":"yard"}`+"\n",
	)
	options.ProjectDirectories = []string{projects}
	report, err := CheckRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Pending ||
		!slices.Contains(report.RequiredMigrations, projectNameMigrationID) ||
		!slices.Contains(report.RequiredMigrations, "move-legacy-assignments") ||
		!slices.Contains(report.AffectedResources, projects) ||
		!slices.Contains(report.AffectedResources, "fixture-assignments") {
		t.Fatalf("release check dropped a required migration: %#v", report)
	}
}

func TestReleaseMigrationResumesInterruptedPrepare(t *testing.T) {
	options, source, destination := releaseMigrationFixture(t)
	registry, err := LoadRegistry(options.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	path, err := registry.Path(1)
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction{
		SchemaVersion: migrationStateSchema,
		FromLayout:    1,
		ToLayout:      2,
		ToRelease:     options.Version,
		Phase:         "preparing",
		Migrations:    []string{path[0].ID},
		Entries:       flattenEntries(path),
	}
	if err := writeTransaction(options.ConfigHome, tx); err != nil {
		t.Fatal(err)
	}
	report, err := ApplyRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Phase != "prepared" || !report.Pending {
		t.Fatalf("interrupted preparation did not resume: %#v", report)
	}
	assertFileContents(t, source, "TOKEN=legacy\n")
	assertFileContents(t, destination, "TOKEN=legacy\n")
}

func TestReleaseMigrationRollsBackInterruptedPrepare(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "before-entry"
		if partial {
			name = "after-destination"
		}
		t.Run(name, func(t *testing.T) {
			options, source, destination := releaseMigrationFixture(t)
			registry, err := LoadRegistry(options.RegistryPath)
			if err != nil {
				t.Fatal(err)
			}
			path, err := registry.Path(1)
			if err != nil {
				t.Fatal(err)
			}
			tx := transaction{
				SchemaVersion: migrationStateSchema,
				FromLayout:    1,
				ToLayout:      2,
				ToRelease:     options.Version,
				Phase:         "preparing",
				Migrations:    []string{path[0].ID},
				Entries:       flattenEntries(path),
			}
			if partial {
				info, exists, err := inspectMoveFile(
					options,
					tx.Entries[0].Scope,
					tx.Entries[0].Source,
				)
				if err != nil || !exists {
					t.Fatalf("inspect interrupted source: exists=%v err=%v", exists, err)
				}
				tx.Entries[0].Digest = info.Digest
				tx.Entries[0].Mode = uint32(info.Mode.Perm())
				if err := publishMatchingFile(
					options.ConfigHome,
					source,
					destination,
					info.Mode,
					info.Digest,
				); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeTransaction(options.ConfigHome, tx); err != nil {
				t.Fatal(err)
			}
			report, err := RollbackRelease(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			if !report.Changed || report.Layout != 1 {
				t.Fatalf("interrupted prepare rollback report: %#v", report)
			}
			assertFileContents(t, source, "TOKEN=legacy\n")
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatalf("interrupted prepare rollback retained destination: %v", err)
			}
			rolledBack, exists, err := readTransaction(options.ConfigHome, options.Version)
			if err != nil || !exists || rolledBack.Phase != "rolled-back" {
				t.Fatalf(
					"interrupted prepare transaction = %#v exists=%v err=%v",
					rolledBack,
					exists,
					err,
				)
			}
		})
	}
}

func TestReleaseMigrationResumesInterruptedFinalize(t *testing.T) {
	options, source, destination := releaseMigrationFixture(t)
	if _, err := ApplyRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	activateFixtureRelease(t, options)

	tx, exists, err := readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists {
		t.Fatalf("read prepared transaction: exists=%v err=%v", exists, err)
	}
	tx.Phase = "committing"
	if err := writeTransaction(options.ConfigHome, tx); err != nil {
		t.Fatal(err)
	}
	if err := removeMatchingFile(source, tx.Entries[0].Digest); err != nil {
		t.Fatal(err)
	}
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("interrupted finalize did not resume: changed=%v err=%v", changed, err)
	}
	if _, err := os.Lstat(source); !os.IsNotExist(err) {
		t.Fatal("resumed finalize restored the retired source")
	}
	assertFileContents(t, destination, "TOKEN=legacy\n")
}

func TestReleaseMigrationResumesAfterStateCommitAndDuringRollback(t *testing.T) {
	t.Run("state-committed", func(t *testing.T) {
		options, source, _ := releaseMigrationFixture(t)
		if _, err := ApplyRelease(context.Background(), options); err != nil {
			t.Fatal(err)
		}
		activateFixtureRelease(t, options)
		tx, exists, err := readTransaction(options.ConfigHome, options.Version)
		if err != nil || !exists {
			t.Fatalf("read transaction: exists=%v err=%v", exists, err)
		}
		tx.ToRuntime = currentRuntimeTarget(options.RuntimeRoot)
		tx.Phase = "committing"
		if err := writeTransaction(options.ConfigHome, tx); err != nil {
			t.Fatal(err)
		}
		if err := removeMatchingFile(source, tx.Entries[0].Digest); err != nil {
			t.Fatal(err)
		}
		if err := writeAppliedState(options.ConfigHome, appliedState{
			SchemaVersion:  migrationStateSchema,
			Layout:         2,
			Applied:        tx.Migrations,
			CurrentRelease: tx.ToRuntime,
		}); err != nil {
			t.Fatal(err)
		}
		if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
			t.Fatalf("state-committed finalize did not resume: changed=%v err=%v", changed, err)
		}
	})

	t.Run("rolling-back", func(t *testing.T) {
		options, source, destination := releaseMigrationFixture(t)
		if _, err := ApplyRelease(context.Background(), options); err != nil {
			t.Fatal(err)
		}
		activateFixtureRelease(t, options)
		if _, err := FinalizeActive(context.Background(), options); err != nil {
			t.Fatal(err)
		}
		tx, exists, err := readTransaction(options.ConfigHome, options.Version)
		if err != nil || !exists {
			t.Fatalf("read transaction: exists=%v err=%v", exists, err)
		}
		tx.Phase = "rolling-back"
		if err := writeTransaction(options.ConfigHome, tx); err != nil {
			t.Fatal(err)
		}
		recovery := filepath.Join(
			transactionDirectory(options.ConfigHome, options.Version),
			tx.Entries[0].Recovery,
		)
		if err := publishMatchingFile(
			options.ConfigHome, recovery, source,
			os.FileMode(tx.Entries[0].Mode), tx.Entries[0].Digest,
		); err != nil {
			t.Fatal(err)
		}
		if err := removeMatchingFile(destination, tx.Entries[0].Digest); err != nil {
			t.Fatal(err)
		}
		report, err := RollbackRelease(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		if report.Layout != 1 {
			t.Fatalf("resumed rollback report: %#v", report)
		}
		assertFileContents(t, source, "TOKEN=legacy\n")
	})
}

func TestReleaseMigrationFailedRollbackDoesNotCreateMixedLayout(t *testing.T) {
	options, source, destination := releaseMigrationFixture(t)
	if _, err := ApplyRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	activateFixtureRelease(t, options)
	if _, err := FinalizeActive(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	writeMigrationFixture(t, destination, "TOKEN=changed\n")
	if _, err := RollbackRelease(context.Background(), options); err == nil {
		t.Fatal("rollback accepted a changed destination")
	}
	if _, err := os.Lstat(source); !os.IsNotExist(err) {
		t.Fatal("failed rollback restored the old source before preflight completed")
	}
	assertFileContents(t, destination, "TOKEN=changed\n")
	state, err := readAppliedState(options.ConfigHome, 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Layout != 2 {
		t.Fatalf("failed rollback changed applied layout to %d", state.Layout)
	}
	tx, exists, err := readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists || tx.Phase != "committed" {
		t.Fatalf("failed rollback changed transaction authority: exists=%v phase=%q err=%v", exists, tx.Phase, err)
	}
}

func TestReleaseMigrationRejectsChangedDestinationAndHardLinks(t *testing.T) {
	t.Run("destination-conflict", func(t *testing.T) {
		options, _, destination := releaseMigrationFixture(t)
		writeMigrationFixture(t, destination, "TOKEN=conflict\n")
		if _, err := CheckRelease(context.Background(), options); err == nil {
			t.Fatal("check accepted a conflicting destination")
		}
	})
	t.Run("hard-link", func(t *testing.T) {
		options, source, _ := releaseMigrationFixture(t)
		if err := os.Link(source, source+".link"); err != nil {
			t.Fatal(err)
		}
		if _, err := CheckRelease(context.Background(), options); err == nil {
			t.Fatal("check accepted a multiply linked source")
		}
	})
	t.Run("intermediate-symlink", func(t *testing.T) {
		options, source, _ := releaseMigrationFixture(t)
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.Rename(filepath.Dir(source), outside); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Dir(source)); err != nil {
			t.Fatal(err)
		}
		if _, err := CheckRelease(context.Background(), options); err == nil {
			t.Fatal("check accepted an intermediate symlink")
		}
	})
}

func TestRegistryRejectsNonContiguousAndUnsafeDefinitions(t *testing.T) {
	registry := Registry{
		SchemaVersion: 1,
		MinimumLayout: 1,
		CurrentLayout: 2,
		Migrations: []Definition{{
			ID:             "unsafe",
			FromLayout:     1,
			ToLayout:       2,
			Resources:      []string{"unsafe-resource"},
			FinalizePolicy: "remove-source-after-active-verify",
			RollbackPolicy: "restore-recovery-before-runtime-swap",
			Moves: []Move{{
				Scope: "config-home", Source: "../escape",
				Destination: "current/config.env", Consumer: "assignments",
			}},
		}},
	}
	if err := registry.Validate(); err == nil {
		t.Fatal("registry accepted an escaping source")
	}
	registry.Migrations[0].Moves[0].Source = "legacy/config.env"
	registry.Migrations[0].ToLayout = 3
	if err := registry.Validate(); err == nil {
		t.Fatal("registry accepted a skipped layout")
	}

	registry.Migrations[0] = Definition{
		ID:             "typed",
		FromLayout:     1,
		ToLayout:       2,
		Resources:      []string{"typed-resource"},
		FinalizePolicy: orderedFinalizePolicy,
		RollbackPolicy: orderedRollbackPolicy,
		Operations: []Operation{{
			ID: "arbitrary", Kind: "shell-command",
		}},
	}
	if err := registry.Validate(); err == nil {
		t.Fatal("registry accepted an arbitrary operation kind")
	}
	registry.Migrations[0].Operations[0].Kind = OperationKindTestYardOwnerV1
	registry.Migrations[0].Moves = []Move{{
		Scope: "config-home", Source: "legacy/config.env",
		Destination: "current/config.env", Consumer: "assignments",
	}}
	if err := registry.Validate(); err != nil {
		t.Fatalf("registry rejected ordered moves plus typed operations: %v", err)
	}
}

func TestShippedRegistryOrdersLifecycleRefreshes(t *testing.T) {
	registry, err := LoadRegistry(filepath.Join("..", "..", "config", "migrations.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Migrations) != 4 || registry.CurrentLayout != 5 {
		t.Fatalf(
			"shipped registry layout=%d migrations=%d, want layout 5 with 4 migrations",
			registry.CurrentLayout,
			len(registry.Migrations),
		)
	}
	operations := registry.Migrations[0].Operations
	if len(operations) != 2 ||
		operations[0].Kind != OperationKindTestYardOwnerV1 ||
		operations[1].Kind != OperationKindTestYardRouteConsumersV1 {
		t.Fatalf("shipped operation order = %#v", operations)
	}
	broker := registry.Migrations[1]
	if broker.FromLayout != 2 || broker.ToLayout != 3 ||
		len(broker.Operations) != 1 ||
		broker.Operations[0].Kind != OperationKindTestVMBrokerRuntimeV1 {
		t.Fatalf("shipped broker migration = %#v", broker)
	}
	power := registry.Migrations[2]
	if power.FromLayout != 3 || power.ToLayout != 4 ||
		len(power.Operations) != 1 ||
		power.Operations[0].Kind != OperationKindPowerReconcilerRuntimeV1 {
		t.Fatalf("shipped power reconciler migration = %#v", power)
	}
	compatibility := registry.Migrations[3]
	if compatibility.ID != "repair-power-reconciler-systemd-compat" ||
		compatibility.FromLayout != 4 || compatibility.ToLayout != 5 ||
		!slices.Equal(compatibility.Resources, []string{"power-reconciler-runtime"}) ||
		len(compatibility.Operations) != 1 ||
		compatibility.Operations[0].ID != "power-reconciler-systemd-compat" ||
		compatibility.Operations[0].Kind != OperationKindPowerReconcilerSystemdCompatV1 {
		t.Fatalf("shipped power reconciler compatibility migration = %#v", compatibility)
	}
}

func TestReleaseRollbackFromCatchUpLayoutsUsesCandidateForCompatibility(t *testing.T) {
	for fromLayout := 1; fromLayout <= 3; fromLayout++ {
		t.Run("layout "+string(rune('0'+fromLayout)), func(t *testing.T) {
			fixture := powerReconcilerRollbackFixture(t, false)
			options := fixture.options
			options.ConfigHome = filepath.Join(t.TempDir(), "config-home")
			options.Version = "5.0.0-test"
			registry := Registry{
				SchemaVersion: 1,
				MinimumLayout: 1,
				CurrentLayout: 5,
				Migrations: []Definition{
					powerReconcilerDefinition(1, OperationKindPowerReconcilerRuntimeV1),
					powerReconcilerDefinition(2, OperationKindPowerReconcilerRuntimeV1),
					powerReconcilerDefinition(3, OperationKindPowerReconcilerRuntimeV1),
					powerReconcilerDefinition(4, OperationKindPowerReconcilerSystemdCompatV1),
				},
			}
			if err := os.MkdirAll(
				filepath.Join(options.RepositoryRoot, "config"), 0o700,
			); err != nil {
				t.Fatal(err)
			}
			options.RegistryPath = filepath.Join(
				options.RepositoryRoot, "config", "migrations.json",
			)
			writeRegistryFixture(t, options.RegistryPath, registry)
			candidateRuntime := filepath.Join("releases", "candidate-release")
			if err := os.Symlink(
				candidateRuntime, filepath.Join(options.RuntimeRoot, "current"),
			); err != nil {
				t.Fatal(err)
			}
			previousRuntime := filepath.Join("releases", "previous-release")
			allIDs := []string{
				"refresh-power-reconciler-1",
				"refresh-power-reconciler-2",
				"refresh-power-reconciler-3",
				"refresh-power-reconciler-4",
			}
			if err := writeAppliedState(options.ConfigHome, appliedState{
				Layout:         5,
				Applied:        allIDs,
				CurrentRelease: candidateRuntime,
			}); err != nil {
				t.Fatal(err)
			}
			operations := flattenOperations(registry.Migrations)
			for index := range operations {
				operations[index].Before = powerReconcilerInstalled
				operations[index].Phase = operationCommitted
			}
			if err := writeTransaction(options.ConfigHome, transaction{
				FromLayout:  fromLayout,
				ToLayout:    5,
				FromRuntime: previousRuntime,
				ToRelease:   options.Version,
				ToRuntime:   candidateRuntime,
				Phase:       "committed",
				Migrations:  allIDs[fromLayout-1:],
				Operations:  operations,
			}); err != nil {
				t.Fatal(err)
			}

			report, err := RollbackRelease(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			if !report.Changed || report.Layout != fromLayout {
				t.Fatalf("catch-up rollback report = %#v", report)
			}
			state, err := readAppliedState(options.ConfigHome, registry.MinimumLayout)
			if err != nil {
				t.Fatal(err)
			}
			if state.Layout != fromLayout || state.CurrentRelease != previousRuntime ||
				!slices.Equal(state.Applied, allIDs[:fromLayout-1]) {
				t.Fatalf("catch-up rollback state = %#v", state)
			}
			assertFileContents(t, fixture.runnerCalls,
				strings.Repeat(
					"candidate|"+fixture.candidateRepository+"|"+
						fixture.previousRepository+"|set\n",
					5-fromLayout,
				))
			assertFileContents(t, fixture.reconciler,
				string(mustReadPowerMigrationFile(t, fixture.previousEngine)))
			assertFileContents(t, fixture.unit,
				materializedPowerUnit(t, fixture.previousTemplate, fixture.reconciler))
		})
	}
}

func powerReconcilerDefinition(fromLayout int, kind string) Definition {
	id := "refresh-power-reconciler-" + string(rune('0'+fromLayout))
	return Definition{
		ID:             id,
		FromLayout:     fromLayout,
		ToLayout:       fromLayout + 1,
		Resources:      []string{"power-reconciler-runtime"},
		FinalizePolicy: orderedFinalizePolicy,
		RollbackPolicy: orderedRollbackPolicy,
		Operations: []Operation{{
			ID:   "power-reconciler-" + string(rune('0'+fromLayout)),
			Kind: kind,
		}},
	}
}

func TestSourceUpgradeFixtureExtendsExactShippedRegistry(t *testing.T) {
	shipped, err := LoadRegistry(filepath.Join("..", "..", "config", "migrations.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := LoadRegistry(filepath.Join(
		"..", "..", "tests", "fixtures", "migrations", "layout-6-production-prefix.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	if fixture.CurrentLayout != shipped.CurrentLayout+1 ||
		len(fixture.Migrations) != len(shipped.Migrations)+1 {
		t.Fatalf(
			"source-upgrade fixture layout=%d migrations=%d, want one beyond shipped layout=%d migrations=%d",
			fixture.CurrentLayout,
			len(fixture.Migrations),
			shipped.CurrentLayout,
			len(shipped.Migrations),
		)
	}
	shippedPrefix, err := json.Marshal(shipped.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	fixturePrefix, err := json.Marshal(fixture.Migrations[:len(shipped.Migrations)])
	if err != nil {
		t.Fatal(err)
	}
	if string(fixturePrefix) != string(shippedPrefix) {
		t.Fatal("source-upgrade fixture rewrote the shipped migration prefix")
	}
	synthetic := fixture.Migrations[len(shipped.Migrations)]
	if synthetic.ID != "move-legacy-assignments" ||
		synthetic.FromLayout != shipped.CurrentLayout ||
		synthetic.ToLayout != fixture.CurrentLayout {
		t.Fatalf("source-upgrade synthetic migration = %#v", synthetic)
	}
}

func TestReleaseMigrationRejectsCorruptDurableState(t *testing.T) {
	t.Run("applied-ids", func(t *testing.T) {
		options, _, _ := releaseMigrationFixture(t)
		if err := writeAppliedState(options.ConfigHome, appliedState{
			SchemaVersion: migrationStateSchema,
			Layout:        1,
			Applied:       []string{"unknown-migration"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := CheckRelease(context.Background(), options); err == nil {
			t.Fatal("check accepted applied IDs inconsistent with layout")
		}
	})
	t.Run("transaction-phase", func(t *testing.T) {
		options, _, _ := releaseMigrationFixture(t)
		registry, err := LoadRegistry(options.RegistryPath)
		if err != nil {
			t.Fatal(err)
		}
		path, err := registry.Path(1)
		if err != nil {
			t.Fatal(err)
		}
		tx := transaction{
			SchemaVersion: migrationStateSchema,
			FromLayout:    1,
			ToLayout:      2,
			ToRelease:     options.Version,
			Phase:         "unknown",
			Migrations:    []string{path[0].ID},
			Entries:       flattenEntries(path),
		}
		if err := writeTransaction(options.ConfigHome, tx); err != nil {
			t.Fatal(err)
		}
		if _, err := CheckRelease(context.Background(), options); err == nil {
			t.Fatal("check accepted an unknown transaction phase")
		}
	})
}

func TestReleaseMigrationRequiresAppliedRegistryPrefix(t *testing.T) {
	options, _, destination := releaseMigrationFixture(t)
	registry, err := LoadRegistry(options.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	registry.CurrentLayout = 3
	registry.Migrations = append(registry.Migrations, Definition{
		ID:             "rotate-current-assignments",
		FromLayout:     2,
		ToLayout:       3,
		Resources:      []string{"fixture-assignments"},
		FinalizePolicy: fileFinalizePolicy,
		RollbackPolicy: fileRollbackPolicy,
		Moves: []Move{{
			Scope: "config-home", Source: "current/config.env",
			Destination: "final/config.env", Consumer: "assignments",
		}},
	})
	writeRegistryFixture(t, options.RegistryPath, registry)
	writeMigrationFixture(t, destination, "TOKEN=legacy\n")
	if err := writeAppliedState(options.ConfigHome, appliedState{
		SchemaVersion: migrationStateSchema,
		Layout:        2,
		Applied:       []string{"move-legacy-assignments"},
	}); err != nil {
		t.Fatal(err)
	}

	report, err := CheckRelease(context.Background(), options)
	if err != nil {
		t.Fatalf("extended registry rejected its applied prefix: %v", err)
	}
	if report.Layout != 2 || report.TargetLayout != 3 ||
		!slices.Equal(report.RequiredMigrations, []string{"rotate-current-assignments"}) {
		t.Fatalf("extended registry report = %#v", report)
	}

	registry.Migrations[0].ID = "rewritten-layout-two"
	writeRegistryFixture(t, options.RegistryPath, registry)
	if _, err := CheckRelease(context.Background(), options); err == nil ||
		err.Error() != "migration applied-state IDs do not match its layout" {
		t.Fatalf("rewritten registry history error = %v", err)
	}
}

func TestReleaseMigrationPreparesFileMoveAfterAppliedTypedPrefix(t *testing.T) {
	options, source, destination := releaseMigrationFixture(t)
	registry, err := LoadRegistry(options.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	fileMove := registry.Migrations[0]
	fileMove.FromLayout = 2
	fileMove.ToLayout = 3
	registry.CurrentLayout = 3
	registry.Migrations = []Definition{{
		ID:             "migrate-test-yard-owner",
		FromLayout:     1,
		ToLayout:       2,
		Resources:      []string{"test-yard-owner"},
		FinalizePolicy: orderedFinalizePolicy,
		RollbackPolicy: orderedRollbackPolicy,
		Operations: []Operation{{
			ID: "test-yard-owner", Kind: OperationKindTestYardOwnerV1,
		}},
	}, fileMove}
	writeRegistryFixture(t, options.RegistryPath, registry)
	if err := writeAppliedState(options.ConfigHome, appliedState{
		SchemaVersion: migrationStateSchema,
		Layout:        2,
		Applied:       []string{"migrate-test-yard-owner"},
	}); err != nil {
		t.Fatal(err)
	}

	report, err := ApplyRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Layout != 2 || report.TargetLayout != 3 ||
		!slices.Equal(report.RequiredMigrations, []string{fileMove.ID}) {
		t.Fatalf("extended registry apply report = %#v", report)
	}
	tx, exists, err := readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists {
		t.Fatalf("prepared transaction exists=%v err=%v", exists, err)
	}
	if tx.Phase != "prepared" || tx.FromLayout != 2 || tx.ToLayout != 3 ||
		len(tx.Entries) != 1 || len(tx.Operations) != 1 ||
		tx.Operations[0].MigrationID != "migrate-test-yard-owner" ||
		tx.Operations[0].Before != "absent" ||
		tx.Operations[0].Phase != operationPrepared ||
		tx.Entries[0].MigrationID != fileMove.ID {
		t.Fatalf("prepared transaction = %#v", tx)
	}
	assertFileContents(t, source, "TOKEN=legacy\n")
	assertFileContents(t, destination, "TOKEN=legacy\n")
}

func TestReleaseMigrationReopensAppliedTypedPrefixAfterSourceIngress(t *testing.T) {
	options, legacyRegistration, currentRegistration, _ := typedReleaseMigrationFixture(t, "0")
	if _, err := ApplyRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	activateFixtureRelease(t, options)
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("initial typed migration finalize: changed=%v err=%v", changed, err)
	}

	registry, err := LoadRegistry(options.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	registry.CurrentLayout = 3
	registry.Migrations = append(registry.Migrations, Definition{
		ID:             "move-legacy-assignments",
		FromLayout:     2,
		ToLayout:       3,
		Resources:      []string{"fixture-assignments"},
		FinalizePolicy: fileFinalizePolicy,
		RollbackPolicy: fileRollbackPolicy,
		Moves: []Move{{
			Scope: "config-home", Source: "legacy/config.env",
			Destination: "current/config.env", Consumer: "assignments",
		}},
	})
	nextRepository := filepath.Join(options.RuntimeRoot, "releases", "3.0.0-test-release")
	if err := os.MkdirAll(filepath.Join(nextRepository, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	nextRegistry := filepath.Join(nextRepository, "config", "migrations.json")
	writeRegistryFixture(t, nextRegistry, registry)
	source := filepath.Join(options.ConfigHome, "legacy", "config.env")
	destination := filepath.Join(options.ConfigHome, "current", "config.env")
	writeMigrationFixture(t, source, "TOKEN=legacy\n")
	options.RepositoryRoot = nextRepository
	options.RegistryPath = nextRegistry
	options.Version = "3.0.0-test"

	if _, err := ApplyRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	tx, exists, err := readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists || len(tx.Operations) != 1 ||
		tx.Operations[0].MigrationID != "migrate-test-yard-owner" ||
		tx.Operations[0].Before != "current" {
		t.Fatalf("applied-prefix guard = %#v, exists=%v err=%v", tx.Operations, exists, err)
	}
	activateFixtureRelease(t, options)
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("later migration finalize: changed=%v err=%v", changed, err)
	}

	if err := os.Remove(currentRegistration); err != nil {
		t.Fatal(err)
	}
	writeMigrationFixture(
		t,
		filepath.Join(filepath.Dir(currentRegistration), "projects", ".lock"),
		"",
	)

	prepass, err := ApplyRelease(context.Background(), options)
	if err != nil {
		t.Fatalf("same-target absent pre-pass: %v", err)
	}
	if !prepass.Changed || prepass.Pending || prepass.Phase != "committed" {
		t.Fatalf("same-target absent pre-pass report = %#v", prepass)
	}
	tx, exists, err = readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists || tx.Operations[0].Before != "absent" ||
		tx.Operations[0].Phase != operationCommitted {
		t.Fatalf("same-target absent baseline = %#v, exists=%v err=%v",
			tx.Operations, exists, err)
	}
	if changed, err := FinalizeActive(context.Background(), options); err != nil || changed {
		t.Fatalf("same-target absent finalize: changed=%v err=%v", changed, err)
	}

	writeMigrationFixture(t, legacyRegistration, "YARD_TEMPLATE=test-vms\n")

	check, err := CheckRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !check.Pending || check.Phase != "reconcile" ||
		!slices.Contains(check.RequiredMigrations, "migrate-test-yard-owner") {
		t.Fatalf("applied-prefix ingress check = %#v", check)
	}
	if _, err := os.Stat(legacyRegistration); err != nil {
		t.Fatalf("applied-prefix check changed the legacy registration: %v", err)
	}
	if _, err := os.Lstat(currentRegistration); !os.IsNotExist(err) {
		t.Fatal("applied-prefix check recreated the current registration")
	}

	report, err := ApplyRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Changed || !report.Pending || report.Phase != "prepared" ||
		!slices.Contains(report.RequiredMigrations, "migrate-test-yard-owner") {
		t.Fatalf("applied-prefix ingress report = %#v", report)
	}
	tx, exists, err = readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists || tx.Operations[0].Before != "legacy-directory-adopt-current" ||
		tx.Operations[0].Phase != operationPrepared {
		t.Fatalf("reopened applied-prefix guard = %#v, exists=%v err=%v", tx.Operations, exists, err)
	}
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("applied-prefix ingress finalize: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(currentRegistration); err != nil {
		t.Fatalf("applied-prefix ingress did not restore current registration: %v", err)
	}
	if _, err := os.Lstat(legacyRegistration); !os.IsNotExist(err) {
		t.Fatal("applied-prefix ingress retained legacy registration")
	}

	if _, err := RollbackRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	state, err := readAppliedState(options.ConfigHome, 1)
	if err != nil || state.Layout != 2 {
		t.Fatalf("later rollback state = %#v err=%v", state, err)
	}
	if _, err := os.Stat(currentRegistration); err != nil {
		t.Fatalf("later rollback incorrectly reverted applied-prefix owner: %v", err)
	}
	assertFileContents(t, source, "TOKEN=legacy\n")
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("later rollback retained the file-move destination")
	}
}

func TestReleaseMigrationCreatesSameLayoutMaintenanceTransaction(t *testing.T) {
	options, legacyRegistration, currentRegistration, calls := typedReleaseMigrationFixture(t, "0")
	if _, err := ApplyRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	activateFixtureRelease(t, options)
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("initial typed migration finalize: changed=%v err=%v", changed, err)
	}
	if err := removeTransactionDirectory(
		transactionDirectory(options.ConfigHome, options.Version),
	); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(currentRegistration)); err != nil {
		t.Fatal(err)
	}
	writeMigrationFixture(t, legacyRegistration, "YARD_TEMPLATE=test-vms\n")
	writeMigrationFixture(t, filepath.Join(filepath.Dir(calls), "incus-state"), "legacy\n")

	report, err := ApplyRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Changed || !report.Pending || report.Phase != "prepared" ||
		!slices.Equal(report.RequiredMigrations, []string{"migrate-test-yard-owner"}) {
		t.Fatalf("same-layout maintenance report = %#v", report)
	}
	tx, exists, err := readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists || tx.FromLayout != 2 || tx.ToLayout != 2 ||
		len(tx.Migrations) != 0 || len(tx.Entries) != 0 || len(tx.Operations) != 1 ||
		tx.Operations[0].Before != "legacy-directory" ||
		tx.Operations[0].Phase != operationPrepared {
		t.Fatalf("same-layout maintenance transaction = %#v, exists=%v err=%v", tx, exists, err)
	}
	registry, err := LoadRegistry(options.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	tx.Phase = "committing"
	tx.Operations[0].Phase = operationCommitting
	if err := writeTransaction(options.ConfigHome, tx); err != nil {
		t.Fatal(err)
	}
	if err := commitTypedOperation(
		context.Background(),
		options,
		registry.Migrations[0].Operations[0],
		tx.Operations[0].Before,
	); err != nil {
		t.Fatal(err)
	}
	tx.Operations[0].Phase = operationCommitted
	if err := writeTransaction(options.ConfigHome, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackRelease(context.Background(), options); err != nil {
		t.Fatalf("partial maintenance rollback: %v", err)
	}
	tx, exists, err = readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists || tx.Phase != "rolled-back" || !tx.RollbackOps ||
		tx.Operations[0].Phase != operationRolledBack {
		t.Fatalf("partial maintenance rollback transaction = %#v, exists=%v err=%v",
			tx, exists, err)
	}
	if _, err := os.Stat(legacyRegistration); err != nil {
		t.Fatalf("partial maintenance rollback did not restore legacy registration: %v", err)
	}

	if _, err := ApplyRelease(context.Background(), options); err != nil {
		t.Fatalf("maintenance retry: %v", err)
	}
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("same-layout maintenance finalize: changed=%v err=%v", changed, err)
	}
	state, err := readAppliedState(options.ConfigHome, 1)
	if err != nil || state.Layout != 2 ||
		!slices.Equal(state.Applied, []string{"migrate-test-yard-owner"}) {
		t.Fatalf("same-layout maintenance state = %#v err=%v", state, err)
	}
	if _, err := os.Stat(currentRegistration); err != nil {
		t.Fatalf("same-layout maintenance did not restore current registration: %v", err)
	}
	if _, err := os.Lstat(legacyRegistration); !os.IsNotExist(err) {
		t.Fatal("same-layout maintenance retained legacy registration")
	}
	repeated, err := ApplyRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Changed || repeated.Pending {
		t.Fatalf("repeated same-layout maintenance was not a no-op: %#v", repeated)
	}
	if _, err := RollbackRelease(context.Background(), options); err != nil {
		t.Fatalf("committed maintenance rollback: %v", err)
	}
	if _, err := os.Stat(currentRegistration); err != nil {
		t.Fatalf("committed maintenance rollback reverted the applied prefix: %v", err)
	}
	report, err = ApplyRelease(context.Background(), options)
	if err != nil {
		t.Fatalf("maintenance roll-forward prepare: %v", err)
	}
	if !report.Pending || report.Phase != "prepared" {
		t.Fatalf("maintenance roll-forward report = %#v", report)
	}
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("maintenance roll-forward finalize: changed=%v err=%v", changed, err)
	}

	registry.CurrentLayout = 3
	registry.Migrations = append(registry.Migrations, Definition{
		ID:             "move-legacy-assignments",
		FromLayout:     2,
		ToLayout:       3,
		Resources:      []string{"fixture-assignments"},
		FinalizePolicy: fileFinalizePolicy,
		RollbackPolicy: fileRollbackPolicy,
		Moves: []Move{{
			Scope: "config-home", Source: "legacy/config.env",
			Destination: "current/config.env", Consumer: "assignments",
		}},
	})
	nextRepository := filepath.Join(options.RuntimeRoot, "releases", "3.0.0-test-release")
	if err := os.MkdirAll(filepath.Join(nextRepository, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	nextRegistry := filepath.Join(nextRepository, "config", "migrations.json")
	writeRegistryFixture(t, nextRegistry, registry)
	writeMigrationFixture(
		t,
		filepath.Join(options.ConfigHome, "legacy", "config.env"),
		"TOKEN=legacy\n",
	)
	maintenanceVersion := options.Version
	options.RepositoryRoot = nextRepository
	options.RegistryPath = nextRegistry
	options.Version = "3.0.0-test"
	if _, err := ApplyRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	activateFixtureRelease(t, options)
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("post-maintenance release finalize: changed=%v err=%v", changed, err)
	}
	if removed, err := CleanupRelease(options); err != nil || removed != 1 {
		t.Fatalf("post-maintenance cleanup: removed=%d err=%v", removed, err)
	}
	if _, err := os.Lstat(
		transactionDirectory(options.ConfigHome, maintenanceVersion),
	); !os.IsNotExist(err) {
		t.Fatalf("extended registry retained the old maintenance transaction: %v", err)
	}
}

func TestReleaseMigrationRunsTypedOperationThroughGenericLifecycle(t *testing.T) {
	options, legacyRegistration, currentRegistration, calls := typedReleaseMigrationFixture(t, "0")

	report, err := CheckRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Layout != 1 || report.TargetLayout != 2 ||
		!slices.Equal(report.RequiredMigrations, []string{"migrate-test-yard-owner"}) {
		t.Fatalf("unexpected typed migration check report: %#v", report)
	}
	if _, err := os.Lstat(migrationRoot(options.ConfigHome)); !os.IsNotExist(err) {
		t.Fatal("typed migration check changed durable state")
	}

	report, err = ApplyRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Phase != "prepared" || !report.Pending {
		t.Fatalf("typed migration did not prepare: %#v", report)
	}
	tx, exists, err := readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists || len(tx.Operations) != 1 ||
		tx.Operations[0].Before != "legacy-directory" ||
		tx.Operations[0].Phase != operationPrepared {
		t.Fatalf("typed operation checkpoint = %#v, exists=%v err=%v", tx.Operations, exists, err)
	}
	if _, err := os.Stat(legacyRegistration); err != nil {
		t.Fatal("prepare changed the legacy registration")
	}

	activateFixtureRelease(t, options)
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("typed migration finalize: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(currentRegistration); err != nil {
		t.Fatalf("typed migration did not publish current registration: %v", err)
	}
	if _, err := os.Lstat(legacyRegistration); !os.IsNotExist(err) {
		t.Fatal("typed migration retained the legacy registration")
	}
	state, err := readAppliedState(options.ConfigHome, 1)
	if err != nil || state.Layout != 2 ||
		!slices.Equal(state.Applied, []string{"migrate-test-yard-owner"}) {
		t.Fatalf("typed migration state = %#v, %v", state, err)
	}

	if _, err := RollbackRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyRegistration); err != nil {
		t.Fatalf("typed rollback did not restore legacy registration: %v", err)
	}
	if _, err := os.Lstat(currentRegistration); !os.IsNotExist(err) {
		t.Fatal("typed rollback retained current registration")
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"-Y e2e-yard test-vms status",
		"-Y e2e-yard teardown --yes",
		"-Y test-yard init --yes",
		"-Y test-yard teardown --yes",
		"-Y e2e-yard init --yes",
	} {
		if !strings.Contains(string(log), expected+"\n") {
			t.Fatalf("typed migration calls omitted %q:\n%s", expected, log)
		}
	}
}

func TestReleaseRollbackResumesCanonicalOwner(t *testing.T) {
	options, legacyRegistration, currentRegistration, calls :=
		typedReleaseMigrationFixture(t, "0")
	if err := os.RemoveAll(filepath.Dir(legacyRegistration)); err != nil {
		t.Fatal(err)
	}
	writeMigrationFixture(t, currentRegistration, "YARD_TEMPLATE=test-vms\n")
	writeMigrationFixture(
		t,
		filepath.Join(filepath.Dir(options.Executable), "incus-state"),
		"current\n",
	)
	if _, err := ApplyRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	activateFixtureRelease(t, options)
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("canonical owner finalize: changed=%v err=%v", changed, err)
	}
	tx, exists, err := readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists || len(tx.Operations) != 1 ||
		tx.Operations[0].Before != "current" ||
		tx.Operations[0].Phase != operationCommitted {
		t.Fatalf("canonical owner transaction = %#v, exists=%v err=%v", tx, exists, err)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("canonical owner no-op invoked lifecycle: %v", err)
	}

	// Reproduce an updater whose later operation failed: rollback has already
	// reached the pre-existing canonical owner and must be resumable without
	// undoing its candidate reconciliation.
	tx.ToRuntime = currentRuntimeTarget(options.RuntimeRoot)
	tx.Phase = "rolling-back"
	tx.RollbackOps = true
	tx.Operations[0].Phase = operationRollingBack
	if err := writeTransaction(options.ConfigHome, tx); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		report, err := RollbackRelease(context.Background(), options)
		if err != nil {
			t.Fatalf("canonical owner rollback attempt %d: %v", attempt+1, err)
		}
		if report.Layout != 1 {
			t.Fatalf("canonical owner rollback report = %#v", report)
		}
	}
	tx, exists, err = readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists || tx.Phase != "rolled-back" ||
		tx.Operations[0].Phase != operationRolledBack {
		t.Fatalf("canonical owner rollback transaction = %#v, exists=%v err=%v",
			tx, exists, err)
	}
	if _, err := os.Stat(currentRegistration); err != nil {
		t.Fatalf("canonical owner rollback changed current registration: %v", err)
	}
}

func TestReleaseRollbackResumesPreviousOwnerAfterDeferredLayoutOneBroker(t *testing.T) {
	options, legacyRegistration, currentRegistration, calls :=
		typedReleaseMigrationFixture(t, "0")
	registry, err := LoadRegistry(options.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	registry.CurrentLayout = 3
	registry.Migrations = append(registry.Migrations, Definition{
		ID:             "refresh-test-vm-broker",
		FromLayout:     2,
		ToLayout:       3,
		Resources:      []string{"test-vm-broker-runtime"},
		FinalizePolicy: orderedFinalizePolicy,
		RollbackPolicy: orderedRollbackPolicy,
		Operations: []Operation{{
			ID: "test-vm-broker-runtime", Kind: OperationKindTestVMBrokerRuntimeV1,
		}},
	})
	writeRegistryFixture(t, options.RegistryPath, registry)

	if err := os.RemoveAll(filepath.Dir(legacyRegistration)); err != nil {
		t.Fatal(err)
	}
	writeMigrationFixture(t, currentRegistration, "YARD_TEMPLATE=test-vms\n")
	projectState := filepath.Join(filepath.Dir(options.Executable), "incus-state")
	writeMigrationFixture(t, projectState, "current\n")
	activateFixtureRelease(t, options)
	candidateRuntime := currentRuntimeTarget(options.RuntimeRoot)
	previousRuntime := runtimeLinkTarget(options.RuntimeRoot, "previous")
	if err := writeAppliedState(options.ConfigHome, appliedState{
		Layout:         3,
		Applied:        []string{"migrate-test-yard-owner", "refresh-test-vm-broker"},
		CurrentRelease: candidateRuntime,
	}); err != nil {
		t.Fatal(err)
	}
	tx := transaction{
		FromLayout:  1,
		ToLayout:    3,
		FromRuntime: previousRuntime,
		ToRelease:   options.Version,
		ToRuntime:   candidateRuntime,
		Phase:       "committed",
		Migrations:  []string{"migrate-test-yard-owner", "refresh-test-vm-broker"},
		Operations: []transactionOperation{
			{
				MigrationID: "migrate-test-yard-owner",
				OperationID: "test-yard-owner",
				Kind:        OperationKindTestYardOwnerV1,
				Before:      "legacy-directory",
				Phase:       operationCommitted,
			},
			{
				MigrationID: "refresh-test-vm-broker",
				OperationID: "test-vm-broker-runtime",
				Kind:        OperationKindTestVMBrokerRuntimeV1,
				Before:      "active",
				Phase:       operationCommitted,
			},
		},
	}
	if err := writeTransaction(options.ConfigHome, tx); err != nil {
		t.Fatal(err)
	}

	executablePayload, err := os.ReadFile(options.Executable)
	if err != nil {
		t.Fatal(err)
	}
	hostRuntimeWriter := filepath.Join(filepath.Dir(calls), "host-runtime-writer")
	options.Environment = append(
		options.Environment,
		"MIGRATION_HOST_RUNTIME_WRITER="+hostRuntimeWriter,
	)
	candidatePayload := string(executablePayload) + `
if [ "$*" = "-Y e2e-yard init --yes" ]; then
  printf 'candidate\n' > "$MIGRATION_HOST_RUNTIME_WRITER"
fi
`
	writeMigrationFixture(t, options.Executable, candidatePayload)
	if err := os.Chmod(options.Executable, 0o700); err != nil {
		t.Fatal(err)
	}
	previousExecutable := filepath.Join(
		options.RuntimeRoot,
		previousRuntime,
		"bin",
		"yard-engine",
	)
	previousPayload := strings.Replace(
		string(executablePayload),
		"printf '%s\\n' \"$*\" >> \"$MIGRATION_CALLS\"",
		"printf 'previous|%s\\n' \"$*\" >> \"$MIGRATION_CALLS\"",
		1,
	)
	if previousPayload == string(executablePayload) {
		t.Fatal("previous owner fixture did not mark its runner")
	}
	previousPayload += `
if [ "$*" = "-Y e2e-yard init --yes" ]; then
  printf 'previous\n' > "$MIGRATION_HOST_RUNTIME_WRITER"
fi
`
	writeMigrationFixture(t, previousExecutable, "#!/bin/sh\nexit 99\n")
	if err := os.Chmod(previousExecutable, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackRelease(context.Background(), options); err == nil {
		t.Fatal("previous owner rollback failure did not interrupt the transaction")
	}
	tx, exists, err := readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists || tx.Phase != "rolling-back" ||
		tx.Operations[0].Phase != operationRollingBack ||
		tx.Operations[1].Phase != operationRolledBack {
		t.Fatalf("interrupted deferred broker transaction = %#v, exists=%v err=%v",
			tx, exists, err)
	}
	if payload, err := os.ReadFile(calls); err == nil &&
		strings.Contains(string(payload), "reconcile-test-vm-broker") {
		t.Fatalf("layout-1 deferred rollback invoked broker runtime:\n%s", payload)
	}

	writeMigrationFixture(t, previousExecutable, previousPayload)
	if err := os.Chmod(previousExecutable, 0o700); err != nil {
		t.Fatal(err)
	}
	report, err := RollbackRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Changed || report.Layout != 1 {
		t.Fatalf("resumed deferred broker rollback report = %#v", report)
	}
	state, err := readAppliedState(options.ConfigHome, registry.MinimumLayout)
	if err != nil || state.Layout != 1 || state.CurrentRelease != previousRuntime ||
		len(state.Applied) != 0 {
		t.Fatalf("resumed deferred broker rollback state = %#v, err=%v", state, err)
	}
	tx, exists, err = readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists || tx.Phase != "rolled-back" ||
		tx.Operations[0].Phase != operationRolledBack ||
		tx.Operations[1].Phase != operationRolledBack {
		t.Fatalf("resumed deferred broker transaction = %#v, exists=%v err=%v",
			tx, exists, err)
	}
	if _, err := os.Stat(legacyRegistration); err != nil {
		t.Fatalf("resumed owner rollback did not restore legacy registration: %v", err)
	}
	if _, err := os.Lstat(currentRegistration); !os.IsNotExist(err) {
		t.Fatalf("resumed owner rollback retained current registration: %v", err)
	}
	assertFileContents(t, hostRuntimeWriter, "previous\n")
	payload, err := os.ReadFile(calls)
	callLog := string(payload)
	candidateTeardown := strings.Index(callLog, "-Y test-yard teardown --yes")
	previousInit := strings.Index(callLog, "previous|-Y e2e-yard init --yes")
	desiredPower := strings.Index(callLog, "desired-power|running")
	previousCheck := strings.Index(callLog, "previous|-Y e2e-yard check")
	if err != nil || candidateTeardown < 0 || previousInit < 0 ||
		desiredPower < 0 || previousCheck < 0 || candidateTeardown >= previousInit ||
		previousInit >= desiredPower || desiredPower >= previousCheck ||
		strings.Contains(callLog, "reconcile-test-vm-broker") {
		t.Fatalf("resumed layout-1 rollback calls = %q, err=%v", payload, err)
	}
}

func TestReleaseMigrationReopensCommittedNoopAfterSourceIngress(t *testing.T) {
	options, legacyRegistration, currentRegistration, _ := typedReleaseMigrationFixture(t, "0")
	if err := os.Remove(legacyRegistration); err != nil {
		t.Fatal(err)
	}
	legacyProjectLock := filepath.Join(filepath.Dir(legacyRegistration), "projects", ".lock")
	writeMigrationFixture(t, legacyProjectLock, "")

	if _, err := ApplyRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	activateFixtureRelease(t, options)
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("initial absent migration finalize: changed=%v err=%v", changed, err)
	}
	tx, exists, err := readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists || tx.Operations[0].Before != "absent" ||
		tx.Operations[0].Phase != operationCommitted {
		t.Fatalf("initial no-op transaction = %#v, exists=%v err=%v", tx.Operations, exists, err)
	}

	writeMigrationFixture(t, legacyRegistration, "YARD_TEMPLATE=test-vms\n")
	report, err := ApplyRelease(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Changed || !report.Pending || report.Phase != "prepared" ||
		!slices.Equal(report.RequiredMigrations, []string{"migrate-test-yard-owner"}) {
		t.Fatalf("late source ingress did not reopen the registry transition: %#v", report)
	}
	tx, exists, err = readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists || tx.Operations[0].Before != "legacy-directory+projects" ||
		tx.Operations[0].Phase != operationPrepared {
		t.Fatalf("reopened operation = %#v, exists=%v err=%v", tx.Operations, exists, err)
	}
	if changed, err := FinalizeActive(context.Background(), options); err != nil || !changed {
		t.Fatalf("late source ingress finalize: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(currentRegistration); err != nil {
		t.Fatalf("late source ingress did not converge to test-yard: %v", err)
	}
	if _, err := os.Lstat(legacyRegistration); !os.IsNotExist(err) {
		t.Fatal("late source ingress retained e2e-yard")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(currentRegistration), "projects", ".lock")); err != nil {
		t.Fatalf("late source ingress did not recreate canonical project state: %v", err)
	}
}

func TestReleaseMigrationTypedOperationBlocksActiveLeaseBeforeState(t *testing.T) {
	options, legacyRegistration, _, calls := typedReleaseMigrationFixture(t, "90")
	if _, err := CheckRelease(context.Background(), options); err == nil ||
		!strings.Contains(err.Error(), "active lease") {
		t.Fatalf("typed migration accepted an active lease: %v", err)
	}
	if _, err := ApplyRelease(context.Background(), options); err == nil ||
		!strings.Contains(err.Error(), "active lease") {
		t.Fatalf("typed migration apply accepted an active lease: %v", err)
	}
	if _, err := os.Stat(legacyRegistration); err != nil {
		t.Fatal("active-lease rejection changed legacy registration")
	}
	if _, err := os.Lstat(migrationRoot(options.ConfigHome)); !os.IsNotExist(err) {
		t.Fatal("active-lease rejection created migration state")
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "teardown") {
		t.Fatal("active-lease rejection reached lifecycle mutation")
	}
}

func TestReleaseMigrationTypedOperationRechecksLeaseBeforeCommit(t *testing.T) {
	options, legacyRegistration, _, calls := typedReleaseMigrationFixture(t, "0")
	if _, err := ApplyRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	options.Environment = replaceTestEnvironment(options.Environment, "MIGRATION_TTL", "90")
	if _, err := CheckRelease(context.Background(), options); err == nil ||
		!strings.Contains(err.Error(), "active lease") {
		t.Fatalf("typed check accepted a changed prepared lease precondition: %v", err)
	}
	activateFixtureRelease(t, options)
	if _, err := FinalizeActive(context.Background(), options); err == nil ||
		!strings.Contains(err.Error(), "active lease") {
		t.Fatalf("typed finalize accepted a newly active lease: %v", err)
	}
	if _, err := os.Stat(legacyRegistration); err != nil {
		t.Fatal("failed pre-commit lease check changed the legacy registration")
	}
	tx, exists, err := readTransaction(options.ConfigHome, options.Version)
	if err != nil || !exists || tx.Operations[0].Phase != operationPrepared {
		t.Fatalf("failed pre-commit checkpoint = %#v, exists=%v err=%v", tx.Operations, exists, err)
	}
	if _, err := RollbackRelease(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "teardown") {
		t.Fatal("new active lease reached lifecycle mutation")
	}
}

func replaceTestEnvironment(environment []string, name, value string) []string {
	result := make([]string, 0, len(environment)+1)
	prefix := name + "="
	for _, assignment := range environment {
		if strings.HasPrefix(assignment, prefix) {
			continue
		}
		result = append(result, assignment)
	}
	return append(result, prefix+value)
}

func typedReleaseMigrationFixture(
	t *testing.T,
	ttl string,
) (ReleaseOptions, string, string, string) {
	t.Helper()
	root := t.TempDir()
	configHome := filepath.Join(root, "config-home")
	dataHome := filepath.Join(root, "data-home")
	runtimeRoot := filepath.Join(root, "runtime")
	repositoryRoot := filepath.Join(runtimeRoot, "releases", "2.0.0-test-release")
	for _, directory := range []string{
		configHome,
		dataHome,
		filepath.Join(repositoryRoot, "config"),
		filepath.Join(runtimeRoot, "releases", "1.0.0-test-release"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	registry := Registry{
		SchemaVersion: 1,
		MinimumLayout: 1,
		CurrentLayout: 2,
		Migrations: []Definition{{
			ID:             "migrate-test-yard-owner",
			FromLayout:     1,
			ToLayout:       2,
			Resources:      []string{"test-yard-owner"},
			FinalizePolicy: orderedFinalizePolicy,
			RollbackPolicy: orderedRollbackPolicy,
			Operations: []Operation{{
				ID: "test-yard-owner", Kind: OperationKindTestYardOwnerV1,
			}},
		}},
	}
	payload, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(repositoryRoot, "config", "migrations.json")
	if err := os.WriteFile(registryPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		filepath.Join("releases", "1.0.0-test-release"),
		filepath.Join(runtimeRoot, "current"),
	); err != nil {
		t.Fatal(err)
	}
	legacyRegistration := filepath.Join(configHome, "yards", "e2e-yard", "config.env")
	currentRegistration := filepath.Join(configHome, "yards", "test-yard", "config.env")
	writeMigrationFixture(t, legacyRegistration, "YARD_TEMPLATE=test-vms\n")
	writeMigrationFixture(
		t,
		filepath.Join(dataHome, "e2e", "controllers", "e2e-yard", ".operator-enrollment-v1"),
		"managed\n",
	)
	calls := filepath.Join(root, "calls")
	projectState := filepath.Join(root, "incus-state")
	writeMigrationFixture(t, projectState, "legacy\n")
	executable := filepath.Join(root, "yard")
	writeMigrationFixture(t, executable, `#!/bin/sh
set -eu
[ "$SUBYARD_INTERNAL_MIGRATION_CHILD" = 1 ]
printf '%s\n' "$*" >> "$MIGRATION_CALLS"
update_projects() {
  current="$(cat "$MIGRATION_INCUS_STATE")"
  case "$1:$current" in
    remove-legacy:legacy) printf 'none\n' > "$MIGRATION_INCUS_STATE" ;;
    remove-legacy:both) printf 'current\n' > "$MIGRATION_INCUS_STATE" ;;
    remove-current:current) printf 'none\n' > "$MIGRATION_INCUS_STATE" ;;
    remove-current:both) printf 'legacy\n' > "$MIGRATION_INCUS_STATE" ;;
    add-legacy:none) printf 'legacy\n' > "$MIGRATION_INCUS_STATE" ;;
    add-legacy:current) printf 'both\n' > "$MIGRATION_INCUS_STATE" ;;
    add-current:none) printf 'current\n' > "$MIGRATION_INCUS_STATE" ;;
    add-current:legacy) printf 'both\n' > "$MIGRATION_INCUS_STATE" ;;
  esac
}
case "$*" in
  "-Y e2e-yard teardown --yes")
    update_projects remove-legacy
    find "$MIGRATION_CONFIG_HOME/yards/e2e-yard/projects" -depth -delete 2>/dev/null || :
    ;;
  "-Y test-yard teardown --yes")
    [ "$SUBYARD_REPOSITORY_ROOT" = "$MIGRATION_CANDIDATE_REPOSITORY" ]
    update_projects remove-current
    ;;
  "-Y e2e-yard init --yes")
    update_projects add-legacy
    install -d -m 0700 "$MIGRATION_CONFIG_HOME/yards/e2e-yard/projects"
    : > "$MIGRATION_CONFIG_HOME/yards/e2e-yard/projects/.lock"
    chmod 0600 "$MIGRATION_CONFIG_HOME/yards/e2e-yard/projects/.lock"
    ;;
  "-Y test-yard init --yes")
    update_projects add-current
    install -d -m 0700 "$MIGRATION_CONFIG_HOME/yards/test-yard/projects"
    : > "$MIGRATION_CONFIG_HOME/yards/test-yard/projects/.lock"
    chmod 0600 "$MIGRATION_CONFIG_HOME/yards/test-yard/projects/.lock"
    ;;
esac
if [ "${3:-}" = test-vms ] && [ "${4:-}" = status ]; then
  printf 'ttl_remaining_seconds\t%s\n' "$MIGRATION_TTL"
fi
`)
	if err := os.Chmod(executable, 0o700); err != nil {
		t.Fatal(err)
	}
	previousExecutable := filepath.Join(
		runtimeRoot,
		"releases",
		"1.0.0-test-release",
		"bin",
		"yard-engine",
	)
	executablePayload, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	writeMigrationFixture(t, previousExecutable, string(executablePayload))
	if err := os.Chmod(previousExecutable, 0o700); err != nil {
		t.Fatal(err)
	}
	incus := filepath.Join(root, "incus")
	writeMigrationFixture(t, incus, `#!/bin/sh
set -eu
state="$(cat "$MIGRATION_INCUS_STATE")"
case "$*" in
  "list yard-e2e-yard --project subyard-e2e-yard --format=json")
    printf '[{"name":"yard-e2e-yard","status":"RUNNING"}]\n'
    ;;
  "project list --format=json")
    case "$state" in
      legacy) printf '[{"name":"subyard-e2e-yard"}]\n' ;;
      current) printf '[{"name":"subyard-test-yard"}]\n' ;;
      both) printf '[{"name":"subyard-e2e-yard"},{"name":"subyard-test-yard"}]\n' ;;
      none) printf '[]\n' ;;
    esac
    ;;
  "project get subyard-e2e-yard features.images")
    [ "$state" = legacy ] || [ "$state" = both ]
    printf 'false\n'
    ;;
  "project get subyard-test-yard features.images")
    [ "$state" = current ] || [ "$state" = both ]
    printf 'false\n'
    ;;
  "project create subyard-e2e-yard -c features.images=false")
    case "$state" in
      none) printf 'legacy\n' > "$MIGRATION_INCUS_STATE" ;;
      current) printf 'both\n' > "$MIGRATION_INCUS_STATE" ;;
    esac
    ;;
  "project create subyard-test-yard -c features.images=false")
    case "$state" in
      none) printf 'current\n' > "$MIGRATION_INCUS_STATE" ;;
      legacy) printf 'both\n' > "$MIGRATION_INCUS_STATE" ;;
    esac
    ;;
  "config set yard-e2e-yard user.subyard.desired_power running --project subyard-e2e-yard")
    printf 'desired-power|running\n' >> "$MIGRATION_CALLS"
    ;;
  *) exit 2 ;;
esac
`)
	if err := os.Chmod(incus, 0o700); err != nil {
		t.Fatal(err)
	}
	return ReleaseOptions{
		RegistryPath:   registryPath,
		RepositoryRoot: repositoryRoot,
		RuntimeRoot:    runtimeRoot,
		ConfigHome:     configHome,
		DataHome:       dataHome,
		Version:        "2.0.0-test",
		Executable:     executable,
		Incus:          incus,
		Environment: append(
			os.Environ(),
			"MIGRATION_CALLS="+calls,
			"MIGRATION_CONFIG_HOME="+configHome,
			"MIGRATION_TTL="+ttl,
			"MIGRATION_INCUS_STATE="+projectState,
			"MIGRATION_CANDIDATE_REPOSITORY="+repositoryRoot,
			"SUBYARD_REPOSITORY_ROOT="+repositoryRoot,
			"SUBYARD_CONFIG_DIR="+filepath.Join(repositoryRoot, "config"),
		),
	}, legacyRegistration, currentRegistration, calls
}

func writeRegistryFixture(t *testing.T, path string, registry Registry) {
	t.Helper()
	payload, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func releaseMigrationFixture(t *testing.T) (ReleaseOptions, string, string) {
	t.Helper()
	root := t.TempDir()
	configHome := filepath.Join(root, "config-home")
	dataHome := filepath.Join(root, "data-home")
	runtimeRoot := filepath.Join(root, "runtime")
	repositoryRoot := filepath.Join(runtimeRoot, "releases", "2.0.0-test-release")
	for _, directory := range []string{
		configHome, dataHome, filepath.Join(repositoryRoot, "config"),
		filepath.Join(runtimeRoot, "releases", "1.0.0-test-release"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	registry := Registry{
		SchemaVersion: 1,
		MinimumLayout: 1,
		CurrentLayout: 2,
		Migrations: []Definition{{
			ID:             "move-legacy-assignments",
			FromLayout:     1,
			ToLayout:       2,
			Resources:      []string{"fixture-assignments"},
			FinalizePolicy: "remove-source-after-active-verify",
			RollbackPolicy: "restore-recovery-before-runtime-swap",
			Moves: []Move{{
				Scope: "config-home", Source: "legacy/config.env",
				Destination: "current/config.env", Consumer: "assignments",
			}},
		}},
	}
	payload, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(repositoryRoot, "config", "migrations.json")
	if err := os.WriteFile(registryPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		filepath.Join("releases", "1.0.0-test-release"),
		filepath.Join(runtimeRoot, "current"),
	); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(configHome, "legacy", "config.env")
	destination := filepath.Join(configHome, "current", "config.env")
	writeMigrationFixture(t, source, "TOKEN=legacy\n")
	return ReleaseOptions{
		RegistryPath:   registryPath,
		RepositoryRoot: repositoryRoot,
		RuntimeRoot:    runtimeRoot,
		ConfigHome:     configHome,
		DataHome:       dataHome,
		Version:        "2.0.0-test",
	}, source, destination
}

func activateFixtureRelease(t *testing.T, options ReleaseOptions) {
	t.Helper()
	if err := os.MkdirAll(options.RuntimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	oldTarget, err := os.Readlink(filepath.Join(options.RuntimeRoot, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(options.RuntimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(options.RuntimeRoot, "previous"))
	if err := os.Symlink(
		filepath.Join("releases", filepath.Base(options.RepositoryRoot)),
		filepath.Join(options.RuntimeRoot, "current"),
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldTarget, filepath.Join(options.RuntimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}
}

func swapFixtureRuntimeLinks(t *testing.T, runtimeRoot string) {
	t.Helper()
	current := filepath.Join(runtimeRoot, "current")
	previous := filepath.Join(runtimeRoot, "previous")
	currentTarget, err := os.Readlink(current)
	if err != nil {
		t.Fatal(err)
	}
	previousTarget, err := os.Readlink(previous)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(previous); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(previousTarget, current); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(currentTarget, previous); err != nil {
		t.Fatal(err)
	}
}

func writeMigrationFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != want {
		t.Fatalf("%s = %q, want %q", path, payload, want)
	}
}
