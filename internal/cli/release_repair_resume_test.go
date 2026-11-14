package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func repairRequest(t *testing.T, fixture pendingInitFixture) (releasetransition.ProcessRequest, releasetransition.JournalRecord) {
	t.Helper()
	journal, err := releasetransition.ParseJournal(mustReadConfigFixture(t, fixture.journalPath))
	if err != nil {
		t.Fatal(err)
	}
	return releasetransition.ProcessRequest{Mode: releasetransition.ProcessConverge, RuntimeRoot: fixture.cli.env["YARD_RUNTIME_ROOT"], ConfigHome: fixture.cli.env["SUBYARD_CONFIG_HOME"], Target: journal.Goal.Target, Direction: journal.Goal.Direction, ArtifactDigest: journal.ArtifactDigest, RegistryDigest: journal.RegistryDigest}, journal
}

// Native guest observations retain preview drift until the real refresh script
// invokes the configured provisioning hook. Other installed-state reads use the
// same synthetic receipts as the pending-init fixtures.
type installedHookExecutor struct {
	installedSettingsExecutor
	marker string
}

func (fixture installedHookExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := request.Command
	if len(command) >= 4 && command[0] == "sh" && strings.Contains(command[3], "helper=/usr/local/bin/subyard-preview") {
		if _, err := os.Stat(fixture.marker); os.IsNotExist(err) {
			return ports.InstanceExecResult{ExitCode: 1}, &ports.GuestExitError{Code: 1}
		}
		return ports.InstanceExecResult{}, nil
	}
	if len(command) == 4 && command[0] == "sh" && strings.Contains(command[3], "/var/lib/subyard/ccusage-ownership") {
		return ports.InstanceExecResult{Stdout: []byte("installed helper files")}, nil
	}
	if len(command) == 3 && command[0] == "bash" {
		return ports.InstanceExecResult{Stdout: []byte("ccusage-current\n")}, nil
	}
	if len(command) == 5 && command[0] == "python3" && command[4] == "check" {
		return ports.InstanceExecResult{}, nil
	}
	return fixture.installedSettingsExecutor.Exec(ctx, project, instance, request)
}

func TestCoreGuestRepairUsesInstalledProvisionHook(t *testing.T) {
	for _, scenario := range []string{"pending", "unavailable", "unknown installed hook", "missing hook record"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newPendingInitFixture(t, false, nil)
			root := fixture.cli.options.RepositoryRoot
			marker := filepath.Join(root, "hook-ran")
			installed, desired := filepath.Join(root, "hook-a"), filepath.Join(root, "hook-b")
			writeCLIFile(t, installed, "#!/bin/sh\nprintf A > '"+marker+"'\n", 0o700)
			if scenario != "unavailable" {
				writeCLIFile(t, desired, "#!/bin/sh\nprintf B > '"+marker+"'\n", 0o700)
			}
			writeConfigCommandFile(t, filepath.Join(root, "config/preview/subyard-preview"), "preview helper", 0o644)
			writeConfigCommandFile(t, filepath.Join(root, "config/agents/ccusage/provision.sh"), "usage check", 0o644)
			writeConfigCommandFile(t, filepath.Join(root, "scripts/refresh-installed-guest-runtime.sh"), "#!/bin/sh\nset -eu\n[ \"$1\" = --yes ]\nexec /bin/sh \"$CCUSAGE_PROVISION\"\n", 0o700)
			for _, command := range []string{"tailscale", "ip"} {
				writeConfigCommandFile(t, filepath.Join(root, "fake-bin", command), "#!/bin/sh\nexit 1\n", 0o700)
			}
			t.Setenv("PATH", filepath.Join(root, "fake-bin")+":/usr/bin:/bin")
			baseline := fixture.cli.options.Executor.(installedSettingsExecutor)
			baseline.settings["CCUSAGE_PROVISION"] = installed
			if scenario == "unknown installed hook" {
				baseline.settings = nil // Legacy evidence keeps ordinary desired configuration.
			}
			if scenario == "missing hook record" {
				delete(baseline.settings, "CCUSAGE_PROVISION")
			}
			baseline.guest = map[string]string{"ALLOWS_CODING_TOOLS": "true", "DEV_USER": "dev", "ENVIRONMENT_PROFILES": ""}
			fixture.cli.options.Executor = installedHookExecutor{baseline, marker}
			fixture.cli.options.NetworkPolicy = allowTestNetworkPolicy()
			incus := fixture.cli.options.Incus.(*testkit.Incus)
			for key, instance := range incus.Instances {
				instance.Config = map[string]string{"user.subyard.preview_sha256": "owned"}
				incus.Instances[key] = instance
			}
			writeConfigCommandFile(t, filepath.Join(fixture.cli.env["SUBYARD_CONFIG_HOME"], "yards/default/config.env"), "CCUSAGE_PROVISION="+desired+"\n")
			if scenario != "unknown installed hook" && scenario != "missing hook record" {
				plan, err := fixture.cli.inspectPendingInit(context.Background(), fixture.cli.env["SUBYARD_CONFIG_HOME"])
				if err != nil || !strings.Contains(strings.Join(plan.lines(), "\n"), "CCUSAGE_PROVISION") {
					t.Fatalf("installed hook change was not disclosed: %v %v", plan.lines(), err)
				}
			}
			request, journal := repairRequest(t, fixture)
			owner := (fixture.cli.profileActivationReconciler(request, "core-guest-runtime").(*profileRuntimeActivationReconciler))
			observation, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
			if err != nil || observation.Converged {
				t.Fatalf("preview drift: %#v %v", observation, err)
			}
			if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
				t.Fatal(err)
			}
			want := "A"
			if scenario == "unknown installed hook" || scenario == "missing hook record" {
				want = "B"
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != want {
				t.Fatalf("repair hook=%q want=%s err=%v", data, want, err)
			}
			verified, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
			if err != nil || !verified.Converged {
				t.Fatalf("repair did not converge: %#v %v", verified, err)
			}
		})
	}
}

func publishRepairResume(t *testing.T, fixture pendingInitFixture, journal releasetransition.JournalRecord, ordinaryUpdate bool) releasetransition.JournalRecord {
	t.Helper()
	journal.Checkpoint = releasetransition.JournalReconciling
	journal.Releases = normalizedCompletedConfigApplyPair(journal.Releases)
	if ordinaryUpdate {
		journal.Releases.From = "release-source"
	}
	payload, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, fixture.journalPath, payload, 0o600)
	return journal
}

// A new owner process must reproduce the approved installed observation even
// when init-owned desired settings change after journal publication.
func TestActiveReleaseRepairResumeKeepsInstalledView(t *testing.T) {
	fixture := newPendingInitFixture(t, false, map[string]string{"default": pendingRename + "ENVIRONMENT_PROFILES=sample\nCODING_TOOL_INTEGRATIONS=\n"})
	request, journal := repairRequest(t, fixture)
	// A forward request repairs the active target of a completed rollback.
	journal.Goal.Direction = releasetransition.DirectionActivatePrevious
	payload, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, fixture.journalPath, payload, 0o600)
	materialized := fixture.cli.activationReconcilers(request)[len(fixture.cli.profileRuntimeIDs())].(*materializedConfigActivationReconciler)
	if !materialized.cli.activation.repair {
		t.Fatal("forward materialized repair after rollback selected desired configuration")
	}
	if err := materialized.resolveScope(normalizedCompletedConfigApplyPair(journal.Releases)); err != nil || !materialized.allLocal {
		t.Fatalf("forward materialized repair lost completed scope: %v", err)
	}
	marker := filepath.Join(fixture.cli.options.RepositoryRoot, "profile-repaired")
	stale, current := strings.Repeat("a", 64), strings.Repeat("b", 64)
	handler := fmt.Sprintf(`#!/bin/sh
[ "$DEV_USER" = dev ] && [ "$DEV_UID" = 1000 ] && [ -z "$ENVIRONMENT_PROFILES" ] && [ "$CODING_TOOL_INTEGRATIONS" = codex ] || exit 19
case "$1" in
apply) : > '%s' ;;
observe) [ -f '%s' ] || { printf '%%s\n' '{"state":"stale","actual":"%s","desired":"%s"}'; exit 0; } ;;
*) exit 18 ;;
esac
printf '%%s\n' '{"state":"current","actual":"%s","desired":"%s"}'
`, marker, marker, stale, current, current, current)
	testkit.WriteFile(t, filepath.Join(fixture.cli.options.RepositoryRoot, "config/profiles/sample/runtime.sh"), []byte(handler), 0o700)
	first := (fixture.cli.profileActivationReconciler(request, "sample-runtime").(*profileRuntimeActivationReconciler))
	approved, err := first.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
	if err != nil || approved.Converged {
		t.Fatalf("approve installed repair: %#v %v", approved, err)
	}
	journal.Goal.Direction = request.Direction
	journal = publishRepairResume(t, fixture, journal, false)
	path := filepath.Join(request.ConfigHome, "yards/default/config.env")
	testkit.WriteFile(t, path, []byte("DEV_USER=edited\nDEV_UID=1003\nENVIRONMENT_PROFILES=sample\nCODING_TOOL_INTEGRATIONS=\n"), 0o600)
	fresh, err := New(fixture.cli.options)
	if err != nil {
		t.Fatal(err)
	}
	second := (fresh.profileActivationReconciler(request, "sample-runtime").(*profileRuntimeActivationReconciler))
	if !fresh.activationReconcilers(request)[len(fresh.profileRuntimeIDs())].(*materializedConfigActivationReconciler).cli.activation.repair {
		t.Fatal("resumed forward materialized repair selected desired configuration")
	}
	observed, err := second.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
	if err != nil || observed.Actual != approved.Actual || observed.Desired != approved.Desired || observed.Converged {
		t.Fatalf("resumed view differs: got=%#v approved=%#v err=%v", observed, approved, err)
	}
	if !second.cli.activation.repair {
		t.Fatal("unfinished in-place repair lost installed view")
	}
	if _, err := second.PrepareActivationRecovery(context.Background(), journal.Releases, releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal(err)
	}
	if err := second.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal("resume failed", err)
	}
	verified, err := second.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
	if err != nil || !verified.Converged {
		t.Fatalf("repair did not converge: %#v %v", verified, err)
	}
}

func TestOrdinaryUpdateResumeKeepsDesiredView(t *testing.T) {
	fixture := newPendingInitFixture(t, false, map[string]string{"default": pendingRename + "ENVIRONMENT_PROFILES=sample\n"})
	request, journal := repairRequest(t, fixture)
	journal = publishRepairResume(t, fixture, journal, true)
	if activeReleaseRepair(request) {
		t.Fatal("ordinary update resume selected installed view")
	}
	handler := `#!/bin/sh
[ "$DEV_USER" = other ] && [ "$DEV_UID" = 1002 ] && [ "$ENVIRONMENT_PROFILES" = sample ] || exit 19
printf '%s\n' '{"state":"current","actual":"` + strings.Repeat("b", 64) + `","desired":"` + strings.Repeat("b", 64) + `"}'
`
	testkit.WriteFile(t, filepath.Join(fixture.cli.options.RepositoryRoot, "config/profiles/sample/runtime.sh"), []byte(handler), 0o700)
	owner := (fixture.cli.profileActivationReconciler(request, "sample-runtime").(*profileRuntimeActivationReconciler))
	observed, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
	if err != nil || !observed.Converged {
		t.Fatalf("update did not observe desired view: %#v %v", observed, err)
	}
}

// The physical config adapter sees only installed destinations and bytes. This
// keeps the test independent of the number of freshness observations.
type installedConfigRepairExecutor struct {
	destination, hash string
	ready             bool
}

func (fixture *installedConfigRepairExecutor) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) != 3 || request.Command[0] != "sha256sum" || request.Command[2] != fixture.destination {
		return ports.InstanceExecResult{}, fmt.Errorf("unexpected materialization request: %v", request.Command)
	}
	hash := strings.Repeat("0", 64)
	if fixture.ready {
		hash = fixture.hash
	}
	return ports.InstanceExecResult{Stdout: []byte(hash + "  " + fixture.destination + "\n")}, nil
}

type installedConfigRefreshPlatform struct {
	*appliedSelectionPlatformFixture
	executor *installedConfigRepairExecutor
}

func (fixture *installedConfigRefreshPlatform) RefreshConfigs(context.Context) error {
	fixture.configs++
	fixture.executor.ready = true
	return nil
}

func TestMaterializedRepairResumeConvergesInstalledConfiguration(t *testing.T) {
	for _, missingSource := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing-source=%t", missingSource), func(t *testing.T) {
			fixture := newPendingInitFixture(t, false, nil)
			request, journal := repairRequest(t, fixture)
			loaded, err := fixture.cli.resolveReleaseTransitionContext(context.Background(), "default", request.ConfigHome)
			if err != nil {
				t.Fatal(err)
			}
			platform := &installedConfigRefreshPlatform{appliedSelectionPlatformFixture: &appliedSelectionPlatformFixture{initPlatformFixture: newInitPlatformFixture(), selected: []string{"codex"}, settings: loaded.Catalog.IntegrationInitSettings(loaded.Environment)}}
			fixture.cli.options.InitPlatform = platform
			assets, err := effectiveConfigAssets(loaded)
			if err != nil || len(assets) != 1 {
				t.Fatalf("assets=%v err=%v", assets, err)
			}
			hash, err := config.HashRegularFile(assets[0].Source)
			if err != nil {
				t.Fatal(err)
			}
			executor := &installedConfigRepairExecutor{destination: assets[0].Destination, hash: hash}
			platform.executor = executor
			fixture.cli.options.Executor = executor
			newOwner := func(program *CLI) *materializedConfigActivationReconciler {
				return program.activationReconcilers(request)[len(program.profileRuntimeIDs())].(*materializedConfigActivationReconciler)
			}
			first := newOwner(fixture.cli)
			approved, err := first.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
			if err != nil || approved.Converged {
				t.Fatalf("approve: %#v %v", approved, err)
			}
			journal = publishRepairResume(t, fixture, journal, false)
			pendingSource := filepath.Join(fixture.cli.options.RepositoryRoot, "pending.rules")
			setting := "HOST_CODEX_AGENTS_MD"
			if missingSource {
				setting = "AGENT_codex_PROVISION"
			} else {
				testkit.WriteFile(t, pendingSource, []byte("pending rule\n"), 0o600)
			}
			writeConfigCommandFile(t, filepath.Join(request.ConfigHome, "yards/default/config.env"), "DEV_USER=edited\nDEV_UID=1003\n"+setting+"="+pendingSource+"\n")
			if missingSource {
				if _, err := fixture.cli.loadContext("default"); err == nil {
					t.Fatal("ordinary loading accepted a missing provisioning source")
				}
				plan, err := fixture.cli.inspectPendingInit(context.Background(), request.ConfigHome)
				if err != nil || len(plan.Yards) != 1 || plan.Yards[0].State != "unavailable" || plan.Yards[0].Pending {
					t.Fatalf("unreadable desired source blocked repair: %#v %v", plan, err)
				}
				listing := strings.Join(plan.lines(), "\n")
				if !strings.Contains(listing, "not inspected (unavailable: desired integration sources cannot be read)") || strings.Contains(listing, pendingSource) {
					t.Fatalf("unsafe or missing unavailable reason: %s", listing)
				}
				if len(plan.steps(nil)) != 0 {
					t.Fatal("unreadable desired source scheduled init")
				}
				baseline := platform.settings
				installed, err := fixture.cli.resolveInstalledReleaseContext(context.Background(), "default", request.ConfigHome)
				if err != nil {
					t.Fatal(err)
				}
				if _, present := installed.Environment["AGENT_codex_PROVISION"]; present {
					t.Fatal("context discovery introduced an unrecorded consumer input")
				}
				platform.settings = nil
				_, err = fixture.cli.resolveInstalledReleaseContext(context.Background(), "default", request.ConfigHome)
				platform.settings = baseline
				if err == nil {
					t.Fatal("unknown installed sources bypassed ordinary validation")
				}
			}
			fresh, err := New(fixture.cli.options)
			if err != nil {
				t.Fatal(err)
			}
			second := newOwner(fresh)
			if !second.cli.activation.repair {
				t.Fatal("resumed materialized repair uses desired view")
			}
			resumed, err := second.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
			if err != nil || resumed.Actual != approved.Actual || resumed.Desired != approved.Desired {
				t.Fatalf("resumed=%#v approved=%#v err=%v", resumed, approved, err)
			}
			if err := second.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
				t.Fatal("installed repair refused pending settings", err)
			}
			verified, err := second.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
			if err != nil || !verified.Converged || platform.configs != 1 {
				t.Fatalf("verify=%#v err=%v effects=%d", verified, err, platform.configs)
			}
			// The ordinary update materialized reconciler also keeps desired intent.
			journal = publishRepairResume(t, fixture, journal, true)
			if newOwner(fresh).cli.activation.repair {
				t.Fatal("ordinary materialized update selected installed view")
			}
		})
	}
}

func TestRollbackRepairConfigurationView(t *testing.T) {
	for _, scenario := range []struct {
		name                                       string
		complete, sameRelease, rollback, installed bool
	}{
		{"completed rollback", true, false, true, true},
		{"unfinished rollback", false, false, true, false},
		{"resumed rollback repair", false, true, true, true},
		{"ordinary update", false, false, false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newPendingInitFixture(t, false, map[string]string{"default": pendingRename + "ENVIRONMENT_PROFILES=sample\n"})
			request, journal := repairRequest(t, fixture)
			if scenario.rollback {
				journal.Goal.Direction = releasetransition.DirectionActivatePrevious
			}
			if !scenario.complete {
				journal.Checkpoint = releasetransition.JournalReconciling
			}
			if scenario.sameRelease {
				journal.Releases = normalizedCompletedConfigApplyPair(journal.Releases)
			}
			payload, err := releasetransition.MarshalJournal(journal)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, fixture.journalPath, payload, 0o600)
			request.Direction = journal.Goal.Direction
			if activeReleaseRepair(request) != scenario.installed {
				t.Fatal("journal selected the wrong configuration view")
			}
			user, uid, profiles := "other", "1002", "sample"
			if scenario.installed {
				user, uid, profiles = "dev", "1000", ""
			}
			handler := fmt.Sprintf("#!/bin/sh\n[ \"$DEV_USER\" = %q ] && [ \"$DEV_UID\" = %q ] && [ \"$ENVIRONMENT_PROFILES\" = %q ] || exit 19\nprintf '%%s\\n' '{\"state\":\"current\",\"actual\":\"%s\",\"desired\":\"%s\"}'\n", user, uid, profiles, strings.Repeat("b", 64), strings.Repeat("b", 64))
			testkit.WriteFile(t, filepath.Join(fixture.cli.options.RepositoryRoot, "config/profiles/sample/runtime.sh"), []byte(handler), 0o700)
			observed, err := (fixture.cli.profileActivationReconciler(request, "sample-runtime").(*profileRuntimeActivationReconciler)).Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
			if err != nil || !observed.Converged {
				t.Fatalf("rollback/update view: %#v %v", observed, err)
			}
			materialized := fixture.cli.activationReconcilers(request)[len(fixture.cli.profileRuntimeIDs())].(*materializedConfigActivationReconciler)
			if materialized.cli.activation.repair != scenario.installed {
				t.Fatal("materialized view differs from profile view")
			}
		})
	}
}

// pendingRetirementExecutor keeps the native integration observer and answers
// its ownership inventory by view: the installed view (observe-applied) needs a
// repair, while the desired view that pending init applies is answered by
// desired: "" (retirable), "conflict" (guest refusal) or "unavailable" (transport).
type pendingRetirementExecutor struct {
	installedSettingsExecutor
	destination, hash string
	desired           string
	desiredByInstance map[string]string // per-instance override of desired
	unreadable        string            // instance whose materialized config cannot be read
	desiredReads      int
	desiredInstances  []string
}

func (fixture *pendingRetirementExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := request.Command
	if len(command) == 5 && command[0] == "python3" && strings.Contains(command[3], "Version 1 records observed ownership only") {
		switch command[4] {
		case "observe-applied":
			payload, _ := json.Marshal(map[string]any{"fingerprint": strings.Repeat("a", 64), "changed": true})
			return ports.InstanceExecResult{Stdout: payload}, nil
		case "observe":
			fixture.desiredReads++
			fixture.desiredInstances = append(fixture.desiredInstances, instance)
			desired := fixture.desired
			if override, ok := fixture.desiredByInstance[instance]; ok {
				desired = override
			}
			switch desired {
			case "conflict":
				return ports.InstanceExecResult{ExitCode: 1}, &ports.GuestExitError{Code: 1}
			case "unavailable":
				return ports.InstanceExecResult{}, errors.New("synthetic transport failure")
			}
			payload, _ := json.Marshal(map[string]any{"fingerprint": strings.Repeat("b", 64), "changed": true})
			return ports.InstanceExecResult{Stdout: payload}, nil
		case "settings", "selection":
			return fixture.installedSettingsExecutor.Exec(ctx, project, instance, request)
		}
		return ports.InstanceExecResult{}, fmt.Errorf("unexpected inventory mutation: %s", command[4])
	}
	if len(command) == 3 && command[0] == "sha256sum" && command[2] == fixture.destination {
		if instance == fixture.unreadable {
			return ports.InstanceExecResult{}, errors.New("synthetic materialized config read failure")
		}
		return ports.InstanceExecResult{Stdout: []byte(fixture.hash + "  " + fixture.destination + "\n")}, nil
	}
	if len(command) >= 4 && command[0] == "sh" && strings.Contains(strings.Join(command, " "), "changed=0") {
		return ports.InstanceExecResult{Stdout: []byte("changed=0\n")}, nil
	}
	return fixture.installedSettingsExecutor.Exec(ctx, project, instance, request)
}

// An installed-view repair keeps a deselected integration installed until the
// pending yard init retires it. When that retirement would refuse changed owned
// state, or cannot be inspected, the repair refuses instead of first rewriting
// the change. Another yard's independent read failure is reported together.
func TestRepairRefusesToRewriteStateThatPendingInitCannotRetire(t *testing.T) {
	for _, desired := range []string{"conflict", "unavailable", "", "conflict+unreadable-named"} {
		t.Run("desired="+desired, func(t *testing.T) {
			named := desired == "conflict+unreadable-named"
			fixture := newPendingInitFixture(t, named, map[string]string{"default": "CODING_TOOL_INTEGRATIONS=\n"})
			request, journal := repairRequest(t, fixture)
			installed := fixture.cli.options.Executor.(installedSettingsExecutor)
			executor := &pendingRetirementExecutor{installedSettingsExecutor: installed, desired: strings.TrimSuffix(desired, "+unreadable-named")}
			if named {
				executor.unreadable = fixture.instanceName(t, "named")
			}
			fixture.cli.options.Executor = executor
			fixture.cli.options.InitPlatform = nil
			owner := fixture.cli.activationReconcilers(request)[len(fixture.cli.profileRuntimeIDs())].(*materializedConfigActivationReconciler)
			if !owner.cli.activation.repair {
				t.Fatal("fixture does not repair the active release")
			}
			// The installed view still selects codex; its materialized rules are current.
			loaded, err := owner.cli.resolveReleaseTransitionContext(context.Background(), "default", request.ConfigHome)
			if err != nil || !slices.Equal(loaded.Integrations.Effective, []string{"codex"}) {
				t.Fatalf("installed selection=%v err=%v", loaded.Integrations.Effective, err)
			}
			assets, err := effectiveConfigAssets(loaded)
			if err != nil || len(assets) != 1 {
				t.Fatalf("assets=%v err=%v", assets, err)
			}
			fake := fixture.cli.options.Incus.(*testkit.Incus)
			fake.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: fake.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName]}
			executor.destination = assets[0].Destination
			if executor.hash, err = config.HashRegularFile(assets[0].Source); err != nil {
				t.Fatal(err)
			}
			observation, err := owner.Observe(context.Background(), journal.Releases, releasetransition.ReleaseLinks{})
			if desired == "" {
				if err != nil || observation.Converged || !owner.integrationPlans["default"].Changed || executor.desiredReads == 0 {
					t.Fatalf("retirable pending selection blocked the repair: %#v %v", observation, err)
				}
				return
			}
			var refusal pendingInitConflictError
			if !errors.As(err, &refusal) || refusal.yard != "default" || refusal.conflict != (executor.desired == "conflict") {
				t.Fatalf("repair would rewrite state pending init cannot retire: %#v %v", observation, err)
			}
			var diagnostic interface{ ActivationDiagnostic() (string, string) }
			if !errors.As(err, &diagnostic) {
				t.Fatal("refusal has no public diagnostic")
			}
			message, retry := diagnostic.ActivationDiagnostic()
			want := "yard default: integration configuration awaiting yard init has an ownership conflict"
			if executor.desired == "unavailable" {
				want = "yard default: integration configuration awaiting yard init cannot be inspected"
			}
			if !strings.HasPrefix(message, want) || retry != "run yard -Y default integration status" || len(message) > 512 || strings.ContainsAny(retry, ";&|") {
				t.Fatalf("diagnostic=%q retry=%q", message, retry)
			}
			if named {
				var read materializedConfigActivationError
				if !errors.As(err, &read) || read.yard != "named" || !strings.Contains(message, "; yard named: cannot inspect materialized config") {
					t.Fatalf("independent refusal of another yard was not reported: %v %q", err, message)
				}
			}
		})
	}
}

// guestInventoryExecutor runs this release's real inventory program against a
// private fixture guest root. Installed settings and other guest reads keep
// the synthetic pending-init receipts.
type guestInventoryExecutor struct {
	installedSettingsExecutor
	root      string
	installed []map[string]any
}

func (fixture *guestInventoryExecutor) home() string { return fixture.root + "/home/dev" }

// relocate maps source-owned guest destinations into the fixture home without
// altering digests.
func (fixture *guestInventoryExecutor) relocate(value []byte) []byte {
	return []byte(strings.NewReplacer("/home/dev/", fixture.home()+"/",
		"/usr/local/libexec/subyard/", fixture.home()+"/libexec-", "/etc/subyard/", fixture.home()+"/etc-").Replace(string(value)))
}

func (fixture *guestInventoryExecutor) inventory(ctx context.Context, mode string, request map[string]any) (ports.InstanceExecResult, error) {
	request["home"], request["uid"] = fixture.home(), os.Getuid()
	payload, err := json.Marshal(request)
	if err != nil {
		return ports.InstanceExecResult{}, err
	}
	// Model the fixture account's actual primary group, not a guest-only group.
	program := strings.NewReplacer(
		"ROOT = '/var/lib/subyard/integrations'", "ROOT = '"+fixture.root+"/state'",
		"OWNER = int('0')", "OWNER = int('"+fmt.Sprint(os.Getuid())+"')",
		"os.fchown(stream.fileno(), uid, uid)", "os.fchown(stream.fileno(), uid, os.getgid())",
		"os.chown(directory, uid, uid if gid is None else gid)", "os.chown(directory, uid, os.getgid() if gid is None else gid)",
		"group = entry.get('gid', owner) if entry['kind'] == 'link' else owner", "group = entry.get('gid', os.getgid()) if entry['kind'] == 'link' else os.getgid()",
	).Replace(reconcileruntime.IntegrationInventoryReader())
	command := exec.CommandContext(ctx, "python3", "-B", "-c", program, mode)
	command.Stdin = strings.NewReader(string(payload))
	var result ports.InstanceExecResult
	result.Stdout, err = command.Output()
	var failure *exec.ExitError
	if errors.As(err, &failure) {
		result.ExitCode, result.Stderr = failure.ExitCode(), failure.Stderr
		return result, &ports.GuestExitError{Code: failure.ExitCode()}
	}
	return result, err
}

func (fixture *guestInventoryExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := request.Command
	if len(command) == 5 && command[0] == "python3" && command[3] == reconcileruntime.IntegrationInventoryReader() && command[4] != "settings" && command[4] != "selection" {
		var input map[string]any
		if err := json.Unmarshal(fixture.relocate(request.Stdin), &input); err != nil {
			return ports.InstanceExecResult{}, err
		}
		if command[4] == "observe-applied" {
			entries, _ := json.Marshal(input["entries"])
			fixture.installed = nil
			if err := json.Unmarshal(entries, &fixture.installed); err != nil {
				return ports.InstanceExecResult{}, err
			}
		}
		return fixture.inventory(ctx, command[4], input)
	}
	if len(command) == 3 && command[0] == "sha256sum" {
		payload, err := os.ReadFile(string(fixture.relocate([]byte(command[2]))))
		if err != nil {
			return ports.InstanceExecResult{ExitCode: 1}, &ports.GuestExitError{Code: 1}
		}
		return ports.InstanceExecResult{Stdout: []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(payload), command[2]))}, nil
	}
	if len(command) >= 4 && command[0] == "sh" && strings.Contains(strings.Join(command, " "), "changed=0") {
		return ports.InstanceExecResult{Stdout: []byte("changed=0\n")}, nil
	}
	return fixture.installedSettingsExecutor.Exec(ctx, project, instance, request)
}

// A post-repair yard init that deselects an installed integration and then
// fails before its commit leaves the installed assets in place. Release
// readiness stays converged, so neither the check nor the repair has anything
// to restore. A third-party removal of an owned asset remains installed drift.
func TestFailedDeselectingInitKeepsInstalledReleaseReady(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprintf("third-party-removal=%t", removed), func(t *testing.T) {
			ctx := context.Background()
			fixture := newPendingInitFixture(t, false, map[string]string{"default": "CODING_TOOL_INTEGRATIONS=\n"})
			request, journal := repairRequest(t, fixture)
			executor := &guestInventoryExecutor{installedSettingsExecutor: fixture.cli.options.Executor.(installedSettingsExecutor), root: testkit.TempDir(t)}
			if err := os.MkdirAll(executor.home(), 0o700); err != nil {
				t.Fatal(err)
			}
			fixture.cli.options.Executor = executor
			fixture.cli.options.InitPlatform = nil
			owner := fixture.cli.activationReconcilers(request)[len(fixture.cli.profileRuntimeIDs())].(*materializedConfigActivationReconciler)
			if !owner.cli.activation.repair {
				t.Fatal("fixture does not repair the active release")
			}
			loaded, err := owner.cli.resolveReleaseTransitionContext(ctx, "default", request.ConfigHome)
			if err != nil || !slices.Equal(loaded.Integrations.Effective, []string{"codex"}) {
				t.Fatalf("installed selection=%v err=%v", loaded.Integrations.Effective, err)
			}
			assets, err := effectiveConfigAssets(loaded)
			if err != nil || len(assets) != 1 {
				t.Fatalf("assets=%v err=%v", assets, err)
			}
			destination := string(executor.relocate([]byte(assets[0].Destination)))
			fake := fixture.cli.options.Incus.(*testkit.Incus)
			fake.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: fake.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName]}
			// The installed view names the entries the completed init committed.
			if _, err := owner.Observe(ctx, journal.Releases, releasetransition.ReleaseLinks{}); err != nil || len(executor.installed) == 0 {
				t.Fatalf("installed entries were not observed: %v", err)
			}
			installed := executor.installed
			desired := []map[string]any{}
			for _, entry := range installed {
				if entry["id"] != "codex" {
					desired = append(desired, entry)
				}
			}
			for _, step := range []struct {
				mode    string
				entries []map[string]any
			}{{"apply", installed}, {"commit", installed}, {"apply", desired}} {
				if result, err := executor.inventory(ctx, step.mode, map[string]any{"developer": "dev", "entries": step.entries}); err != nil {
					t.Fatalf("seed %s: %v %s", step.mode, err, result.Stderr)
				}
			}
			// The deselecting init's hooks failed: no commit.
			if hash, err := config.HashRegularFile(destination); err != nil || hash != mustHashConfigSource(t, assets[0].Source) {
				t.Fatalf("failed init retired the installed asset before its hook boundary: %v", err)
			}
			if removed {
				if err := os.Remove(destination); err != nil {
					t.Fatal(err)
				}
			}
			inventory := mustReadConfigFixture(t, executor.root+"/state/inventory.json")
			owner.integrationPlans = nil
			observation, err := owner.Observe(ctx, journal.Releases, releasetransition.ReleaseLinks{})
			if err != nil {
				t.Fatalf("readiness refused: %v", err)
			}
			if removed {
				if observation.Converged {
					t.Fatal("readiness hid a third-party removal of an owned asset")
				}
			} else if !observation.Converged || owner.integrationPlans["default"].InstalledChanged() {
				t.Fatalf("failed deselecting init revoked release readiness: %#v %#v", observation, owner.integrationPlans["default"])
			}
			if after := mustReadConfigFixture(t, executor.root+"/state/inventory.json"); string(after) != string(inventory) {
				t.Fatal("readiness observation wrote the inventory")
			}
		})
	}
}

func mustHashConfigSource(t *testing.T, path string) string {
	t.Helper()
	hash, err := config.HashRegularFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestActivationRefusalsStayWithinDiagnosticBound(t *testing.T) {
	causes := []error{}
	for index := range 6 {
		causes = append(causes, pendingInitConflictError{yard: fmt.Sprintf("yard-%02d", index), conflict: index%2 == 0, cause: errors.New("synthetic")})
	}
	if single := collectActivationRefusals(causes[:1]); single != causes[0] {
		t.Fatalf("single refusal was wrapped: %#v", single)
	}
	message, retry := collectActivationRefusals(causes).(interface{ ActivationDiagnostic() (string, string) }).ActivationDiagnostic()
	want := "6 release activation refusals, 5 omitted; complete detail: run yard -Y yard-00 integration status, run yard -Y yard-01 integration status"
	if len(message) > 512 || !strings.HasPrefix(message, want) || !strings.Contains(message, "; first: yard yard-00: ") || retry != "run yard -Y yard-00 integration status" {
		t.Fatalf("diagnostic=%q retry=%q", message, retry)
	}
	// Every omitted yard is named while its status view fits the bound.
	for _, yard := range []string{"yard-01", "yard-05"} {
		if !strings.Contains(message, "run yard -Y "+yard+" integration status") {
			t.Fatalf("omitted %s has no status view: %q", yard, message)
		}
	}
	many := []error{}
	for index := range 40 {
		many = append(many, pendingInitConflictError{yard: fmt.Sprintf("yard-%02d", index), cause: errors.New("synthetic")})
	}
	message, _ = collectActivationRefusals(many).(interface{ ActivationDiagnostic() (string, string) }).ActivationDiagnostic()
	if len(message) > 512 || !strings.HasPrefix(message, "40 release activation refusals, 39 omitted; ") || !strings.Contains(message, " more; first: yard yard-00: ") ||
		len(message)-strings.Index(message, "; first: ")-len("; first: ") < 160 {
		t.Fatalf("overflowing status views not counted or first refusal starved: %q", message)
	}
	// An oversized first retry never starves or breaks the first refusal.
	long := []error{materializedConfigActivationError{yard: strings.Repeat("y", 450), command: "update --check", cause: errors.New("synthetic")}, many[1]}
	message, _ = collectActivationRefusals(long).(interface{ ActivationDiagnostic() (string, string) }).ActivationDiagnostic()
	if len(message) > 512 || !strings.HasPrefix(message, "2 release activation refusals, 1 omitted; first: yard "+strings.Repeat("y", 50)) {
		t.Fatalf("oversized first retry: %q", message)
	}
}

// Prospective planning applies the same guard to the desired files a planned
// source publication would leave: one yard refuses with the desired-view
// conflict while another, retirable yard proceeds in the same observation.
func TestProspectiveRepairGuardsPendingInitRetirement(t *testing.T) {
	fixture := newPendingInitFixture(t, true, map[string]string{"default": "CODING_TOOL_INTEGRATIONS=\n", "named": "CODING_TOOL_INTEGRATIONS=\n"})
	request, journal := repairRequest(t, fixture)
	installed := fixture.cli.options.Executor.(installedSettingsExecutor)
	defaultInstance, namedInstance := fixture.instanceName(t, "default"), fixture.instanceName(t, "named")
	executor := &pendingRetirementExecutor{installedSettingsExecutor: installed, desiredByInstance: map[string]string{defaultInstance: "conflict", namedInstance: ""}}
	fixture.cli.options.Executor = executor
	fixture.cli.options.InitPlatform = nil
	owner := fixture.cli.activationReconcilers(request)[len(fixture.cli.profileRuntimeIDs())].(*materializedConfigActivationReconciler)
	if !owner.cli.activation.repair {
		t.Fatal("fixture does not repair the active release")
	}
	loaded, err := owner.cli.resolveReleaseTransitionContext(context.Background(), "default", request.ConfigHome)
	if err != nil || !slices.Equal(loaded.Integrations.Effective, []string{"codex"}) {
		t.Fatalf("installed selection=%v err=%v", loaded.Integrations.Effective, err)
	}
	assets, err := effectiveConfigAssets(loaded)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%v err=%v", assets, err)
	}
	fake := fixture.cli.options.Incus.(*testkit.Incus)
	fake.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: fake.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName]}
	executor.destination = assets[0].Destination
	if executor.hash, err = config.HashRegularFile(assets[0].Source); err != nil {
		t.Fatal(err)
	}
	observation, err := owner.ObserveProspective(context.Background(), journal.Releases, releasetransition.ReleaseLinks{}, retainedActivationSettingsView{request.ConfigHome})
	if !slices.Contains(executor.desiredInstances, defaultInstance) || !slices.Contains(executor.desiredInstances, namedInstance) {
		t.Fatalf("prospective planning did not inspect every pending desired view: %v %#v %v", executor.desiredInstances, observation, err)
	}
	var refusal pendingInitConflictError
	if !errors.As(err, &refusal) || refusal.yard != "default" || !refusal.conflict {
		t.Fatalf("prospective planning admitted a repair that pending init cannot retire: %#v %v", observation, err)
	}
	var refusals *activationRefusals
	if errors.As(err, &refusals) || strings.Contains(err.Error(), "yard named") || !owner.integrationPlans["named"].Changed {
		t.Fatalf("retirable yard did not proceed independently: %v", err)
	}
}
