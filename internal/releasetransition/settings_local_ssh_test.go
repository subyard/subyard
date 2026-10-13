package releasetransition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestLocalSSHMigrationPinsAllPersistentYardsAndResumesCAS(t *testing.T) {
	for _, boundary := range []string{"", "after-recovery", "after-settings-cas", "after-step-verified", "after-ledger-cas"} {
		t.Run(boundary, func(t *testing.T) {
			home := testkit.TempDir(t)
			configHome := filepath.Join(home, "settings")
			files := map[string]string{
				".sync/settings/config.env":             "SSH_PORT=2240\n",
				".sync/settings/yards/named/config.env": "SSH_PORT=2241\n",
				"yards/named/config.env":                "# unrelated local records\nDEV_SUDO=0\n",
				"yards/override/config.env":             "# exact winning override\nSSH_PORT=2242\n",
			}
			for name, content := range files {
				path := filepath.Join(configHome, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte(content), 0o640)
			}
			root, err := filepath.Abs(filepath.Join("..", ".."))
			if err != nil {
				t.Fatal(err)
			}
			registry, err := os.ReadFile(filepath.Join(root, "config/release-transition.json"))
			if err != nil {
				t.Fatal(err)
			}
			options := V2Options{ConfigHome: configHome, OperatorHome: home, CandidateConfigDir: filepath.Join(root, "config"), Releases: ReleasePair{From: "release-ssh", Target: "release-ssh"},
				ObserveLinks: func(context.Context) (ReleaseLinks, error) { return ReleaseLinks{Active: "release-ssh"}, nil }, RegistryPayload: registry, ArtifactDigest: digestA,
				OwnerRegistration: &v2TestOwnerRegistration{state: OwnerRegistrationAbsent}, VerifyAuthorization: func(plan PlanToken, a Authorization) bool { return a == v2TestAuthorization(plan) }}
			transition, err := NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			activeBoundary := boundary
			transition.options.fault = func(point string) error {
				if point == activeBoundary && point != "" {
					activeBoundary = ""
					return errors.New("synthetic interruption")
				}
				return nil
			}
			goal := Goal{Target: "release-ssh", Direction: DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(inspection.Blockers) != 0 {
				t.Fatalf("inspect: %#v %v", inspection.Blockers, err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
			if err != nil {
				t.Fatal(err)
			}
			if boundary != "" {
				if outcome.Status != StatusRecovering {
					t.Fatalf("interruption was not retained: %#v", outcome)
				}
				transition, err = NewV2Transition(options)
				if err != nil {
					t.Fatal(err)
				}
				resume, err := transition.Inspect(context.Background(), goal)
				if err != nil || len(resume.Blockers) != 0 {
					t.Fatalf("resume: %#v %v", resume.Blockers, err)
				}
				outcome, err = transition.Converge(context.Background(), Execution{Plan: resume.Plan})
			}
			if err != nil || outcome.Status != StatusReady {
				t.Fatalf("converge: %#v %v", outcome, err)
			}
			for yard, port := range map[string]int{"default": 2240, "named": 2241, "override": 2242} {
				loaded, err := config.Load(config.LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: yard, DisablePrivate: true, Environment: map[string]string{"SUBYARD_CONFIG_HOME": configHome}})
				if err != nil || loaded.Context.SSHPort != port {
					t.Fatalf("yard %s port changed: %v", yard, err)
				}
			}
			for _, name := range []string{".sync/settings/config.env", ".sync/settings/yards/named/config.env", "yards/override/config.env"} {
				content, err := os.ReadFile(filepath.Join(configHome, name))
				if err != nil || string(content) != files[name] {
					t.Fatalf("protected source or winning local file changed: %s", name)
				}
			}
			info, err := os.Stat(filepath.Join(configHome, "yards/named/config.env"))
			if err != nil || info.Mode().Perm() != 0o640 {
				t.Fatal("exact local mode changed")
			}
		})
	}
}

func TestLocalSSHCheckpointResumeRetainsWideAndHistoricalNarrowGrants(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(map[bool]string{false: "wide", true: "historical-narrow"}[historical], func(t *testing.T) {
			home := testkit.TempDir(t)
			configHome := filepath.Join(home, "settings")
			write := func(relative, content string) {
				t.Helper()
				path := filepath.Join(configHome, relative)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte(content), 0o600)
			}
			write(".sync/settings/config.env", "SSH_PORT=2240\n")
			write(".sync/settings/yards/named/config.env", "SSH_PORT=2241\n")
			root, err := filepath.Abs(filepath.Join("..", ".."))
			if err != nil {
				t.Fatal(err)
			}
			registryPayload, err := os.ReadFile(filepath.Join(root, "config/release-transition.json"))
			if err != nil {
				t.Fatal(err)
			}
			registry, _, err := ParseRegistryV2(registryPayload, BuiltinCapabilityCatalog())
			if err != nil {
				t.Fatal(err)
			}
			ledger := BaselineLedgerV2(registry)
			for _, migration := range registry.Migrations {
				if migration.Kind == localSSHPortMigration {
					break
				}
				ledger, err = ledger.Advance(registry, migration)
				if err != nil {
					t.Fatal(err)
				}
			}
			encoded, _, err := MarshalLedgerV2(ledger, registry)
			if err != nil {
				t.Fatal(err)
			}
			write("release-transition/v2/ledger.json", string(encoded))
			owner := &sourceScopeReconciler{}
			var reconciler V2ActivationReconciler = owner
			if historical {
				reconciler = historicalSourceScopeReconciler{owner}
			}
			links := ReleaseLinks{Active: "release-a"}
			options := V2Options{ConfigHome: configHome, OperatorHome: home, CandidateConfigDir: filepath.Join(root, "config"),
				Releases: ReleasePair{From: "release-a", Target: "release-b"}, RegistryPayload: registryPayload, ArtifactDigest: digestA,
				MigrationCheckpoint: true, SourceMigrationCheckpoint: true, Reconcilers: []V2ActivationReconciler{reconciler},
				OwnerRegistration: &v2TestOwnerRegistration{state: OwnerRegistrationAbsent},
				ObserveLinks:      func(context.Context) (ReleaseLinks, error) { return links, nil },
				ActivateLinks: func(_ context.Context, pair ReleasePair) (ReleaseLinks, error) {
					links = ReleaseLinks{Active: pair.Target, Previous: releaseIDPointer(pair.From)}
					return links, nil
				}, VerifyAuthorization: func(plan PlanToken, grant Authorization) bool { return grant == v2TestAuthorization(plan) },
				fault: func(point string) error {
					if point == "before-checkpoint-publication" {
						return errors.New("interrupt SSH pin checkpoint publication")
					}
					return nil
				}}
			transition, err := NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			readJournal := func(active *V2Transition) JournalRecord {
				t.Helper()
				snapshot, err := active.store.ReadCurrentJournal()
				if err != nil || !snapshot.Exists {
					t.Fatal("SSH checkpoint journal unavailable")
				}
				journal, err := ParseJournal(snapshot.Payload)
				if err != nil {
					t.Fatal(err)
				}
				return journal
			}
			goal := Goal{Target: "release-b", Direction: DirectionActivateTarget}
			initial, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(initial.Blockers) != 0 || owner.wide == historical {
				t.Fatalf("SSH original scope: wide=%t err=%v", owner.wide, err)
			}
			outcome, err := transition.Converge(context.Background(), Execution{Plan: initial.Plan, Authorization: v2TestAuthorization(initial.Plan)})
			if err != nil || outcome.Status != StatusRecovering {
				t.Fatalf("SSH checkpoint interruption missing: %#v %v", outcome, err)
			}
			original := readJournal(transition)
			if original.Checkpoint != JournalComplete || len(original.Steps) != 3 {
				t.Fatal("checkpoint fixture did not retain completed two-yard SSH CAS work")
			}
			for yard, port := range map[string]string{"default": "2240", "named": "2241"} {
				path := filepath.Join(configHome, "yards", yard, "config.env")
				values, err := config.ReadAssignments(path)
				info, modeErr := os.Stat(path)
				if err != nil || modeErr != nil || values["SSH_PORT"] != port || info.Mode().Perm() != 0o600 {
					t.Fatalf("actual SSH pin CAS missing for %s", yard)
				}
			}
			freshOwner := &sourceScopeReconciler{converged: true}
			fresh := freshSourceScopeOwner(t, transition, freshOwner)
			resume, err := fresh.Inspect(context.Background(), goal)
			if err != nil || len(resume.Blockers) != 0 || resume.Resume == nil || freshOwner.wide == historical {
				t.Fatalf("SSH checkpoint scope changed: wide=%t inspection=%#v err=%v", freshOwner.wide, resume, err)
			}
			outcome, err = fresh.Converge(context.Background(), Execution{Plan: resume.Plan})
			if err != nil || outcome.Status != StatusReady || freshOwner.applies != 0 {
				t.Fatalf("SSH checkpoint required new activation or consent: %#v %v", outcome, err)
			}
			completed := readJournal(fresh)
			if completed.Transaction != original.Transaction || completed.AuthorizationPlan != original.AuthorizationPlan || completed.ObservationScope != original.ObservationScope {
				t.Fatal("SSH checkpoint publication rewrote historical authorization")
			}
		})
	}
}

func TestLocalSSHMigrationCatalogPreservesKnownRetainedHistoryAndBlocksUnknown(t *testing.T) {
	transition, _, _ := v2TransitionFixture(t, nil)
	transition.catalog = PreviousBuiltinCapabilityCatalog()
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)}); err != nil || outcome.Status != StatusReady {
		t.Fatalf("old owner seed: %#v %v", outcome, err)
	}
	transition, store := interruptSourceMetadataActivation(t, transition, transition.store)
	before, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	old, err := ParseJournal(before.Payload)
	if err != nil || old.CatalogDigest != PreviousBuiltinCapabilityCatalog().Digest() {
		t.Fatal("fixture lost original compiled identity")
	}
	options := transition.options
	options.fault = nil
	options.Reconcilers = []V2ActivationReconciler{&v2TestReconciler{converged: true}}
	current, err := NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	if current.catalog.Digest() != old.CatalogDigest {
		t.Fatal("new engine rebaselined the original catalog")
	}
	if _, err := store.InspectSourceMetadata(current.registry, current.registryDigest); err != nil {
		t.Fatalf("known owner source history refused: %v", err)
	}
	after, _ := store.ReadCurrentJournal()
	if string(before.Payload) != string(after.Payload) {
		t.Fatal("read-only admission rewrote original journal")
	}
	if _, known := SupportedBuiltinCatalog(digestA); known {
		t.Fatal("arbitrary historical catalog admitted")
	}
}

func TestLocalSSHMigrationHasNoLegacyRollbackCompatibility(t *testing.T) {
	registryPayload, err := os.ReadFile(filepath.Join("..", "..", "config/release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, applied := range []bool{false, true} {
		fixture := newV2RollbackBoundaryFixtureWithLedger(t, "0.18.6", "", registryPayload, "absent")
		if applied {
			registry := fixture.transition.registry
			ledger := BaselineLedgerV2(registry)
			for _, migration := range registry.Migrations {
				ledger, err = ledger.Advance(registry, migration)
				if err != nil {
					t.Fatal(err)
				}
			}
			payload, _, err := MarshalLedgerV2(ledger, registry)
			if err != nil {
				t.Fatal(err)
			}
			before, err := fixture.transition.store.ReadLedger()
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.transition.store.CompareAndSwapLedger(before, payload); err != nil {
				t.Fatal(err)
			}
		}
		inspection, err := fixture.transition.Inspect(context.Background(), Goal{Target: "release-a", Direction: DirectionActivatePrevious})
		if err != nil || inspection.Outcome == nil || inspection.Outcome.Code != CodeRollbackIncompatible || inspection.Assessment.Changed {
			t.Fatalf("legacy rollback admitted (applied=%t): %#v %v", applied, inspection, err)
		}
		if *fixture.authorizes != 0 || *fixture.activations != 0 {
			t.Fatal("incompatible rollback authorized or activated")
		}
	}
}

func TestLocalSSHMigrationOverlappingSettingsResumeAndRejectDrift(t *testing.T) {
	for _, newline := range []string{"\n", ""} {
		name := "terminated"
		if newline == "" {
			name = "unterminated"
		}
		t.Run(name, func(t *testing.T) {
			for _, boundary := range []string{"", "after-first-cas", "after-second-cas", "after-second-verified", "after-target-active"} {
				for _, drift := range []string{"", "wrong-port", "unrelated-local", "unrelated-final-lf", "inherited-input"} {
					if boundary == "" && drift != "" {
						continue
					}
					t.Run(boundary+"/"+drift, func(t *testing.T) {
						home := testkit.TempDir(t)
						configHome := filepath.Join(home, "settings")
						for name, content := range map[string]string{
							".sync/settings/yards/named/config.env": "SSH_PORT=2241\n",
							"yards/named/config.env":                "# retained local definition\nYARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=0\n# retained tail" + newline,
						} {
							path := filepath.Join(configHome, name)
							if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
								t.Fatal(err)
							}
							testkit.WriteFile(t, path, []byte(content), 0o640)
						}
						root, err := filepath.Abs(filepath.Join("..", ".."))
						if err != nil {
							t.Fatal(err)
						}
						registry, err := os.ReadFile(filepath.Join(root, "config/release-transition.json"))
						if err != nil {
							t.Fatal(err)
						}
						options := V2Options{ConfigHome: configHome, OperatorHome: home, CandidateConfigDir: filepath.Join(root, "config"),
							Releases:        ReleasePair{From: "release-overlap", Target: "release-overlap"},
							ObserveLinks:    func(context.Context) (ReleaseLinks, error) { return ReleaseLinks{Active: "release-overlap"}, nil },
							RegistryPayload: registry, ArtifactDigest: digestA,
							OwnerRegistration:   &v2TestOwnerRegistration{state: OwnerRegistrationAbsent},
							VerifyAuthorization: func(plan PlanToken, auth Authorization) bool { return auth == v2TestAuthorization(plan) },
						}

						transition, err := NewV2Transition(options)
						if err != nil {
							t.Fatal(err)
						}
						casCount, verifiedCount := 0, 0
						interrupted := false
						transition.options.fault = func(point string) error {
							if point == "after-settings-cas" {
								casCount++
							}
							if point == "after-step-verified" {
								verifiedCount++
							}
							stop := boundary == "after-first-cas" && point == "after-settings-cas" && casCount == 1 ||
								boundary == "after-second-cas" && point == "after-settings-cas" && casCount == 2 ||
								boundary == "after-second-verified" && point == "after-step-verified" && verifiedCount == 2 ||
								boundary == "after-target-active" && point == boundary
							if stop {
								interrupted = true
								return errors.New("synthetic overlapping settings interruption")
							}
							return nil
						}
						goal := Goal{Target: "release-overlap", Direction: DirectionActivateTarget}
						inspection, err := transition.Inspect(context.Background(), goal)
						if err != nil || len(inspection.Blockers) != 0 {
							t.Fatalf("initial: %#v %v", inspection.Blockers, err)
						}
						outcome, err := transition.Converge(context.Background(), Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
						if err != nil {
							t.Fatal(err)
						}
						if boundary == "" {
							if outcome.Status != StatusReady || casCount != 2 {
								t.Fatalf("complete: %#v CAS=%d", outcome, casCount)
							}
						} else if !interrupted || outcome.Status != StatusRecovering {
							t.Fatalf("interruption: %#v CAS=%d verified=%d", outcome, casCount, verifiedCount)
						}
						localPath := filepath.Join(configHome, "yards/named/config.env")
						before, err := os.ReadFile(localPath)
						if err != nil {
							t.Fatal(err)
						}
						switch drift {
						case "wrong-port":
							value := "2249"
							changed, err := config.EditPersistentAssignmentContent(localPath, before, "SSH_PORT", &value)
							if err != nil {
								t.Fatal(err)
							}
							testkit.WriteFile(t, localPath, changed, 0o640)
						case "unrelated-local":
							testkit.WriteFile(t, localPath, append(before, []byte("# unapproved record\n")...), 0o640)
						case "unrelated-final-lf":
							testkit.WriteFile(t, localPath, append(before, '\n'), 0o640)
						case "inherited-input":
							testkit.WriteFile(t, filepath.Join(configHome, ".sync/settings/yards/named/config.env"), []byte("SSH_PORT=2249\n"), 0o640)
						}
						transition, err = NewV2Transition(options)
						if err != nil {
							t.Fatal(err)
						}
						resumed, err := transition.Inspect(context.Background(), goal)
						if err != nil {
							t.Fatal(err)
						}
						if drift != "" {
							if boundary == "after-first-cas" && drift == "inherited-input" {
								if resumed.Plan == inspection.Plan || resumed.Resume != nil {
									t.Fatal("changed inherited input did not require fresh consent")
								}
								journalBefore, err := transition.store.ReadCurrentJournal()
								if err != nil {
									t.Fatal(err)
								}
								blocked, err := transition.Converge(context.Background(), Execution{Plan: resumed.Plan})
								if err != nil || blocked.Status != StatusOperatorActionRequired {
									t.Fatalf("changed input executed without new consent: %#v %v", blocked, err)
								}
								journalAfter, err := transition.store.ReadCurrentJournal()
								if err != nil || journalBefore.Fingerprint != journalAfter.Fingerprint {
									t.Fatal("unapproved inherited input rewrote original journal")
								}
								return
							}
							if len(resumed.Blockers) == 0 {
								t.Fatal("unapproved settings drift admitted")
							}
							blocked := false
							for _, blocker := range resumed.Blockers {
								if blocker.Code == CodeMigrationStale || blocker.Code == CodePlanStale {
									blocked = true
								}
							}
							if !blocked {
								t.Fatalf("drift lost migration guard: %#v", resumed.Blockers)
							}
							return
						}
						if len(resumed.Blockers) != 0 {
							t.Fatalf("authorized overlap resume blocked: %#v", resumed.Blockers)
						}
						if boundary != "" {
							outcome, err = transition.Converge(context.Background(), Execution{Plan: resumed.Plan})
							if err != nil || outcome.Status != StatusReady {
								t.Fatalf("resume: %#v %v", outcome, err)
							}
						}
						content, err := os.ReadFile(localPath)
						if err != nil || string(content) != "# retained local definition\nYARD_TEMPLATE='test-vms'\n# retained tail\nSSH_PORT='2241'\n" {
							t.Fatalf("authorized file differs: %q %v", content, err)
						}
						info, err := os.Stat(localPath)
						if err != nil || info.Mode().Perm() != 0o600 {
							t.Fatal("SSH pin did not preserve the canonical predecessor mode")
						}
						git, err := os.ReadFile(filepath.Join(configHome, ".sync/settings/yards/named/config.env"))
						if err != nil || string(git) != "SSH_PORT=2241\n" {
							t.Fatal("overlapping migration changed Git input")
						}
					})
				}
			}
		})
	}
}
