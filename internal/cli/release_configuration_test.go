package cli

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestActiveReleaseRepairDecision(t *testing.T) {
	for _, scenario := range []string{"completed rollback", "resume", "ordinary update resume", "ordinary rollback resume", "other artifact", "other registry", "other request target", "journal target no longer active"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newPendingInitFixture(t, false, nil)
			request, journal := repairRequest(t, fixture)
			want := scenario == "completed rollback" || scenario == "resume"
			if scenario == "completed rollback" {
				journal.Goal.Direction = releasetransition.DirectionActivatePrevious
			} else {
				journal.Checkpoint = releasetransition.JournalReconciling
				journal.Releases = normalizedCompletedConfigApplyPair(journal.Releases)
			}
			switch scenario {
			case "ordinary update resume", "ordinary rollback resume":
				journal.Releases.From = "release-a"
				journal.Releases.Previous = nil
				if scenario == "ordinary rollback resume" {
					journal.Goal.Direction = releasetransition.DirectionActivatePrevious
				}
			case "other artifact":
				request.ArtifactDigest = releasetransition.Fingerprint(strings.Repeat("d", 64))
			case "other registry":
				request.RegistryDigest = releasetransition.Fingerprint(strings.Repeat("d", 64))
			case "other request target":
				request.Target = "release-a"
			case "journal target no longer active":
				current := filepath.Join(request.RuntimeRoot, "current")
				if err := os.Remove(current); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("releases/release-a", current); err != nil {
					t.Fatal(err)
				}
				request.Target = "release-a"
			}
			payload, err := releasetransition.MarshalJournal(journal)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, fixture.journalPath, payload, 0o600)
			if got := activeReleaseRepair(request); got != want {
				t.Fatalf("repair=%t want=%t", got, want)
			}
		})
	}
}

func TestRepairUnrecordedProfileInitSettingUsesDesiredAcrossResume(t *testing.T) {
	fixture := newPendingInitFixture(t, false, nil)
	root, home := fixture.cli.options.RepositoryRoot, fixture.cli.env["SUBYARD_CONFIG_HOME"]
	profileRoot := filepath.Join(root, "config/profiles/sample")
	testkit.WriteFile(t, filepath.Join(profileRoot, "profile.json"), []byte(`{"schema_version":1,"settings":[{"name":"SAMPLE_INIT_MODE","type":"string","scopes":["shipped","yard"],"application":"yard-init","default":"installed"}],"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh"}}`), 0o600)
	writeConfigCommandFile(t, filepath.Join(home, "yards/default/config.env"), "SAMPLE_INIT_MODE=desired\n")
	capture := filepath.Join(root, "observed-profile-setting")
	handler := `#!/bin/sh
printf '%s' "$SAMPLE_INIT_MODE" > '` + capture + `'
printf '%s\n' '{"state":"stale","actual":"` + strings.Repeat("a", 64) + `","desired":"` + strings.Repeat("b", 64) + `","consequence":"repair desired profile setting"}'
`
	testkit.WriteFile(t, filepath.Join(profileRoot, "runtime.sh"), []byte(handler), 0o700)
	request, journal := repairRequest(t, fixture)
	request.Mode = releasetransition.ProcessInspect
	for process := 1; process <= 2; process++ {
		program, err := New(fixture.cli.options)
		if err != nil {
			t.Fatal(err)
		}
		definition, known := program.catalog.LookupSetting("SAMPLE_INIT_MODE")
		if !known || definition.Owner != "profile:sample" || definition.Application != config.SettingYardInit || program.catalog.IntegrationInitSetting(definition.Name) {
			t.Fatalf("fixture must declare an unrecorded profile-owned init setting: %#v", definition)
		}
		owner := program.profileActivationReconciler(request, "sample-runtime").(*profileRuntimeActivationReconciler)
		if owner.cli.activation == nil || !owner.cli.activation.repair {
			t.Fatalf("process %d did not use the shared repair context", process)
		}
		observation, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
		if err != nil || observation.Converged || !slices.Equal(observation.Consequences, []string{"yard default: repair desired profile setting"}) {
			t.Fatalf("process %d lost ordinary profile repair: %#v err=%v", process, observation, err)
		}
		if value := string(mustReadConfigFixture(t, capture)); value != "desired" {
			t.Fatalf("process %d owning activation received %q, want desired", process, value)
		}
		pending, err := program.inspectPendingInit(context.Background(), home)
		if err != nil || strings.Contains(strings.Join(pending.lines(), "\n"), "SAMPLE_INIT_MODE") {
			t.Fatalf("process %d listed an unrecorded profile setting as pending init: %v err=%v", process, pending.lines(), err)
		}
		if process == 1 {
			if err := os.Remove(capture); err != nil {
				t.Fatal(err)
			}
			journal = publishRepairResume(t, fixture, journal, false)
			request.Mode = releasetransition.ProcessConverge
		}
	}
}

// This is a constructor contract over every registered owner, not a list of
// currently known consumers. All contexts and native hook environments must
// carry the same installed inputs before and after journal publication.
func TestActivationConfigurationBoundary(t *testing.T) {
	t.Run("non-repair observations", testNonRepairActivationConfiguration)
	fixture := newPendingInitFixture(t, false, nil)
	root, home := fixture.cli.options.RepositoryRoot, fixture.cli.env["SUBYARD_CONFIG_HOME"]
	installedHook := filepath.Join(root, "installed-hook")
	testkit.WriteFile(t, installedHook, []byte("#!/bin/sh\nexit 0\n"), 0o700)
	baseline := fixture.cli.options.Executor.(installedSettingsExecutor)
	baseline.settings["CCUSAGE_PROVISION"] = installedHook
	baseline.settings["AGENT_codex_PROVISION"] = installedHook
	archive := strings.Repeat("a", 64)
	baseline.guest = map[string]string{"ENVIRONMENT_PROFILES": "", "ALLOWS_CODING_TOOLS": "true", "DEV_USER": "dev", "WEB_PREVIEW_HOST_PORT": "8080", "CCUSAGE_VERSION": "1.2.3", "CCUSAGE_SHA256_AMD64": archive, "CCUSAGE_SHA256_ARM64": archive}
	fixture.cli.options.Executor = baseline
	incus := fixture.cli.options.Incus.(*testkit.Incus)
	for key, instance := range incus.Instances {
		instance.Config = map[string]string{"user.subyard.preview_sha256": "owned"}
		incus.Instances[key] = instance
	}
	request, journal := repairRequest(t, fixture)
	journal.Goal.Direction = releasetransition.DirectionActivatePrevious
	payload, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, fixture.journalPath, payload, 0o600)
	edits := func(user string) {
		writeConfigCommandFile(t, filepath.Join(home, "yards/default/config.env"), "DEV_USER="+user+"\nDEV_UID=1042\nENVIRONMENT_PROFILES=sample\nCODING_TOOL_INTEGRATIONS=\nCCUSAGE_VERSION=9.9.9\nWEB_PREVIEW_HOST_PORT=9090\nCCUSAGE_PROVISION="+filepath.Join(root, "desired-hook")+"\nAGENT_codex_PROVISION="+filepath.Join(root, "desired-hook")+"\n")
	}
	testkit.WriteFile(t, filepath.Join(root, "desired-hook"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
	edits("pending")
	want := map[string]string{"DEV_USER": "dev", "DEV_UID": "1000", "ENVIRONMENT_PROFILES": "", "CCUSAGE_PROVISION": installedHook, "AGENT_codex_PROVISION": installedHook, "CCUSAGE_VERSION": "1.2.3", "WEB_PREVIEW_HOST_PORT": "8080", "CCUSAGE_SHA256_AMD64": archive, "CCUSAGE_SHA256_ARM64": archive}
	program := fixture.cli
	fingerprints := map[string]string{}
	for process := 1; process <= 2; process++ {
		owners := program.activationReconcilers(request)
		var shared *releaseConfiguration
		for _, owner := range owners {
			provider, ok := owner.(interface{ activationConfiguration() *CLI })
			if !ok {
				t.Fatalf("owner %s bypasses the activation context constructor", owner.ID())
			}
			operation := provider.activationConfiguration()
			if operation == nil || operation.activation == nil {
				t.Fatalf("owner %s has no bound activation source", owner.ID())
			}
			if shared == nil {
				shared = operation.activation
			}
			if operation.activation != shared {
				t.Fatalf("owner %s constructed another context source", owner.ID())
			}
			check := func() string {
				t.Helper()
				loaded, err := operation.resolveReleaseTransitionContext(context.Background(), "default", home)
				if err != nil {
					t.Fatalf("process %d owner %s: %v", process, owner.ID(), err)
				}
				for name, value := range want {
					if loaded.Environment[name] != value {
						t.Fatalf("process %d owner %s obtained desired %s=%q", process, owner.ID(), name, loaded.Environment[name])
					}
					if loaded.Settings[name].EffectiveValue != value {
						t.Fatalf("owner %s obtained a desired effective trace for %s", owner.ID(), name)
					}
				}
				if loaded.Context.DevUser != "dev" || loaded.Context.DevUID != 1000 || !slices.Equal(loaded.Integrations.Effective, []string{"codex"}) {
					t.Fatalf("owner %s obtained desired identity or effective selection", owner.ID())
				}
				runtime := operation.initPlatform(loaded, nil).(reconcileruntime.Runtime)
				for _, pair := range runtime.Environment {
					name, value, _ := strings.Cut(pair, "=")
					if expected, check := want[name]; check && value != expected {
						t.Fatalf("owner %s native hook received desired %s", owner.ID(), name)
					}
				}
				return integrationConfigurationFingerprint(loaded)
			}
			fingerprint := check()
			if process == 1 {
				fingerprints[owner.ID()] = fingerprint
			} else if fingerprint != fingerprints[owner.ID()] {
				t.Fatalf("owner %s desired fingerprint changed across processes", owner.ID())
			}
			edits("edited-after-observation")
			if check() != fingerprint {
				t.Fatalf("owner %s rebuilt its repair context from pending inputs", owner.ID())
			}
		}
		if len(fingerprints) != len(owners) {
			t.Fatal("registered owner coverage changed on resume")
		}
		if process == 1 {
			journal.Goal.Direction = request.Direction
			publishRepairResume(t, fixture, journal, false)
			program, err = New(fixture.cli.options)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func testNonRepairActivationConfiguration(t *testing.T) {
	for _, scenario := range []string{"completed readiness", "ordinary inspection", "teardown"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newConfigApplyRepairFixture(t, true)
			program := fixture.cli
			home := program.env["SUBYARD_CONFIG_HOME"]
			unit := filepath.Join(testkit.TempDir(t), "power.service")
			testkit.WriteFile(t, unit, []byte("installed power owner\n"), 0o600)
			program.env["SUBYARD_POWER_UNIT_PATH"] = unit
			program.env["SUBYARD_POWER_RECONCILER_PATH"] = unit + ".absent"
			platform := &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture()}
			platform.converged[ports.ReconcileStagePower] = false
			program.options.InitPlatform = platform
			if scenario == "teardown" {
				program.options.Arguments = []string{"teardown"}
			}
			for _, name := range []string{"default", "named"} {
				path := filepath.Join(home, "yards", name, "config.env")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				previous, _ := os.ReadFile(path)
				testkit.WriteFile(t, path, append(previous, []byte("DEV_USER=pending_"+name+"\n")...), 0o600)
			}
			request, journal := repairRequest(t, pendingInitFixture{configApplyRepairFixture: fixture})
			request.Mode = "" // Ordinary readiness/admission does not request release repair.
			pair := normalizedCompletedConfigApplyPair(journal.Releases)
			if scenario == "ordinary inspection" {
				request.Mode, request.Target = releasetransition.ProcessInspect, "release-a"
				pair.From, pair.Target = pair.Target, request.Target
			}
			owners, ordinary := program.activationReconcilers(request), program.activationReconcilers(request)
			// The ordinary reference uses the pre-binding caller environment and
			// strict yard loader. Restore every constructor-provided context
			// generically, including those captured by owner closures.
			for _, owner := range ordinary {
				operation := owner.(interface{ activationConfiguration() *CLI }).activationConfiguration()
				operation.activation = nil
				operation.env, operation.baseEnv = maps.Clone(program.env), maps.Clone(program.baseEnv)
			}
			incus := program.options.Incus.(*testkit.Incus)
			observe := func(owner releasetransition.V2ActivationReconciler) (releasetransition.V2ActivationObservation, error) {
				incus.ExecSteps = nil
				for _, name := range []string{"default", "named"} {
					loaded, err := program.loadReleaseTransitionContext(name, home, nil)
					if err != nil {
						t.Fatal(err)
					}
					appendMismatchedHashSteps(t, incus, loaded, "0")
				}
				return owner.Observe(context.Background(), pair, releasetransition.ReleaseLinks{})
			}
			for index, owner := range owners {
				operation := owner.(interface{ activationConfiguration() *CLI }).activationConfiguration()
				if operation.activation == nil || operation.activation.repair {
					t.Fatalf("owner %s changed the non-repair view", owner.ID())
				}
				if !maps.Equal(operation.env, program.env) {
					t.Fatalf("owner %s changed ordinary applicability inputs", owner.ID())
				}
				for _, name := range []string{"default", "named"} {
					got, err := operation.resolveReleaseTransitionContext(context.Background(), name, home)
					if err != nil {
						t.Fatal(err)
					}
					want, err := program.loadReleaseTransitionContext(name, home, nil)
					if err != nil {
						t.Fatal(err)
					}
					if got.Context.DevUser != "pending_"+name || !reflect.DeepEqual(got.Context, want.Context) || !maps.Equal(got.Environment, want.Environment) ||
						integrationConfigurationFingerprint(got) != integrationConfigurationFingerprint(want) {
						t.Fatalf("owner %s changed ordinary yard %s configuration", owner.ID(), name)
					}
				}
				got, gotErr := observe(owner)
				want, wantErr := observe(ordinary[index])
				if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotErr, wantErr) {
					t.Fatalf("owner %s changed ordinary observation: got=%#v err=%v want=%#v err=%v", owner.ID(), got, gotErr, want, wantErr)
				}
				if owner.ID() == "host-power" && (gotErr != nil || got.Converged) {
					t.Fatalf("ordinary observation lost installed global drift: %#v err=%v", got, gotErr)
				}
				if materialized, ok := owner.(*materializedConfigActivationReconciler); ok && scenario != "ordinary inspection" && !materialized.allLocal {
					t.Fatal("completed readiness narrowed to the selected yard")
				}
			}
		})
	}
}
