package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func writeColdFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte(data), mode)
}

const coldTestRelease = releasetransition.ReleaseID("1.0.0-cold-0123456789ab")

func writeColdTestJournal(t *testing.T, configHome string, releases releasetransition.ReleasePair, direction releasetransition.Direction) {
	t.Helper()
	fingerprint := releasetransition.Fingerprint(strings.Repeat("a", 64))
	journal := releasetransition.JournalRecord{
		SchemaVersion: releasetransition.JournalSchemaV2, Transaction: releasetransition.TransactionID("tx-" + strings.Repeat("b", 32)),
		Goal: releasetransition.Goal{Target: releases.Target, Direction: direction}, Releases: releases,
		AuthorizationPlan: releasetransition.PlanToken("plan-v1-" + string(fingerprint)), ResumePlan: releasetransition.PlanToken("resume-v1-" + string(fingerprint)),
		ArtifactDigest: fingerprint, RegistryDigest: fingerprint, CatalogDigest: fingerprint,
		ObservationScope: fingerprint, AuthorizationDigest: fingerprint,
		Checkpoint: releasetransition.JournalComplete, Steps: []releasetransition.JournalStep{},
	}
	intent, err := json.Marshal(struct {
		AuthorizationPlan releasetransition.PlanToken   `json:"authorizationPlan"`
		ResumePlan        releasetransition.PlanToken   `json:"resumePlan"`
		ObservationScope  releasetransition.Fingerprint `json:"observationScope"`
		Steps             []struct{}                    `json:"steps"`
	}{journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, []struct{}{}})
	if err != nil {
		t.Fatal(err)
	}
	journal.IntentDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(intent)))
	payload, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	// Warm verdicts below must come from the history, not from a rejected record.
	if _, err := releasetransition.ParseJournal(payload); err != nil {
		t.Fatal(err)
	}
	writeColdFile(t, filepath.Join(configHome, "release-transition", "v2", "journal.json"), string(payload), 0o600)
}

func TestColdInstallationRequiresNoYardStateAndFirstInstallHistory(t *testing.T) {
	older := releasetransition.ReleaseID("0.9.0-old-0123456789ab")
	firstInstall := releasetransition.ReleasePair{From: coldTestRelease, Target: coldTestRelease}
	for _, test := range []struct {
		name string
		seed func(t *testing.T, configHome, runtimeRoot string)
		cold bool
	}{
		{name: "absent config home", cold: true, seed: func(t *testing.T, configHome, _ string) {
			if err := os.Remove(configHome); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "empty config home", cold: true, seed: func(*testing.T, string, string) {}},
		{name: "first-install bookkeeping", cold: true, seed: func(t *testing.T, configHome, _ string) {
			writeColdFile(t, filepath.Join(configHome, "migrations", "update.lock"), "", 0o600)
			writeColdTestJournal(t, configHome, firstInstall, releasetransition.DirectionActivateTarget)
			writeColdFile(t, filepath.Join(configHome, "release-transition", "v2", "ledger.json"), "{}", 0o600)
		}},
		{name: "host-wide settings", cold: true, seed: func(t *testing.T, configHome, _ string) {
			writeColdFile(t, filepath.Join(configHome, "config.env"), "SSH_PORT=2301\n", 0o600)
			writeColdFile(t, filepath.Join(configHome, "overrides", "shared", "config.env"), "GIT_USER_NAME=a\n", 0o600)
			if err := os.MkdirAll(filepath.Join(configHome, "overrides", "host"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "yard file overrides", seed: func(t *testing.T, configHome, _ string) {
			writeColdFile(t, filepath.Join(configHome, "overrides", "alpha", "file"), "", 0o600)
		}},
		{name: "keys", seed: func(t *testing.T, configHome, _ string) {
			writeColdFile(t, filepath.Join(configHome, "keys", "ledger"), "", 0o600)
		}},
		{name: "owner host ID", seed: func(t *testing.T, configHome, _ string) {
			writeColdFile(t, filepath.Join(configHome, "host-id"), "host-a\n", 0o600)
		}},
		{name: "default yard config file", seed: func(t *testing.T, configHome, _ string) {
			writeColdFile(t, filepath.Join(configHome, "yards", "default", "config.env"), "SSH_PORT=2222\n", 0o600)
		}},
		{name: "named yard registration", seed: func(t *testing.T, configHome, _ string) {
			writeColdFile(t, filepath.Join(configHome, "yards", "alpha.env"), "SSH_PORT=2301\n", 0o600)
		}},
		{name: "Git settings yard", seed: func(t *testing.T, configHome, _ string) {
			writeColdFile(t, filepath.Join(configHome, ".sync", "settings", "yards", "alpha", "config.env"), "SSH_PORT=2301\n", 0o600)
		}},
		{name: "applied-state record", seed: func(t *testing.T, configHome, _ string) {
			writeColdFile(t, filepath.Join(configHome, "reconcile", "applied-identities", "default.json"), "{}", 0o600)
		}},
		{name: "unknown migration history", seed: func(t *testing.T, configHome, _ string) {
			writeColdFile(t, filepath.Join(configHome, "migrations", "journal.json"), "{}", 0o600)
		}},
		// A default-only installation from before owner HostIDs has no yard
		// files at all; its upgraded release history alone keeps it warm.
		{name: "pre-HostID default-only upgrade", seed: func(t *testing.T, configHome, _ string) {
			writeColdTestJournal(t, configHome, releasetransition.ReleasePair{From: older, Target: coldTestRelease}, releasetransition.DirectionActivateTarget)
		}},
		{name: "pre-HostID default-only repair after upgrade", seed: func(t *testing.T, configHome, _ string) {
			writeColdTestJournal(t, configHome, releasetransition.ReleasePair{From: coldTestRelease, Previous: &older, Target: coldTestRelease}, releasetransition.DirectionActivateTarget)
		}},
		{name: "retained previous release", seed: func(t *testing.T, configHome, runtimeRoot string) {
			writeColdTestJournal(t, configHome, firstInstall, releasetransition.DirectionActivateTarget)
			if err := os.Symlink("releases/"+string(older), filepath.Join(runtimeRoot, "previous")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "invalid journal", seed: func(t *testing.T, configHome, _ string) {
			writeColdFile(t, filepath.Join(configHome, "release-transition", "v2", "journal.json"), "{", 0o600)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			configHome, runtimeRoot := filepath.Join(root, "config"), filepath.Join(root, "runtime")
			for _, directory := range []string{configHome, runtimeRoot} {
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			test.seed(t, configHome, runtimeRoot)
			if got := coldInstallation(configHome, runtimeRoot); got != test.cold {
				t.Fatalf("coldInstallation = %t, want %t", got, test.cold)
			}
		})
	}
}

func TestColdInstallationReportsUnreadableStateAsWarm(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := testkit.TempDir(t)
	configHome := filepath.Join(root, "config")
	writeColdFile(t, filepath.Join(configHome, "migrations", "update.lock"), "", 0o600)
	if err := os.Chmod(filepath.Join(configHome, "migrations"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(configHome, "migrations"), 0o700) })
	if coldInstallation(configHome, "") {
		t.Fatal("unreadable configuration must not prove a cold installation")
	}
	if coldInstallation("relative/config", root) {
		t.Fatal("a relative configuration home must not prove a cold installation")
	}
	empty := filepath.Join(root, "empty-config")
	if coldInstallation(empty, "") || coldInstallation(empty, "relative/runtime") {
		t.Fatal("an unknown runtime root cannot exclude a retained previous release")
	}
	if !coldInstallation(empty, filepath.Join(root, "runtime")) {
		t.Fatal("an empty installation with a known runtime root is cold")
	}
}

type deniedIncus struct {
	reconcileruntime.IncusReader
	ports.InstanceExecutor
	reads int
}

func (native *deniedIncus) Instance(context.Context, string, string) (ports.InstanceInfo, error) {
	native.reads++
	return ports.InstanceInfo{}, errors.Join(domain.ErrIncusAccessDenied, os.ErrPermission)
}

// A cold activation never reads the injected or default native port.
func TestColdReleaseActivationObservesAbsentIncus(t *testing.T) {
	root := testkit.TempDir(t)
	configHome := filepath.Join(root, "config")
	native := &deniedIncus{}
	cli := &CLI{options: Options{Incus: native, Executor: native}}
	request := releasetransition.ProcessRequest{ConfigHome: configHome, RuntimeRoot: filepath.Join(root, "runtime")}
	operation := cli.releaseActivationOperation(request)
	if !operation.coldNative {
		t.Fatal("an installation without yard state is cold")
	}
	port, _ := operation.statusPorts()
	if !ports.LocalInstallationAbsent(port) {
		t.Fatal("a cold activation must observe an absent native installation")
	}
	if _, err := port.Instance(context.Background(), "subyard", "yard"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cold instance read = %v, want not-exist", err)
	}
	if native.reads != 0 {
		t.Fatalf("cold activation read the native port %d times", native.reads)
	}

	writeColdFile(t, filepath.Join(configHome, "host-id"), "host-a\n", 0o600)
	warm := cli.releaseActivationOperation(request)
	if warm.coldNative {
		t.Fatal("owner HostID makes the installation warm")
	}
	port, _ = warm.statusPorts()
	if _, err := port.Instance(context.Background(), "subyard", "yard"); !errors.Is(err, domain.ErrIncusAccessDenied) || native.reads != 1 {
		t.Fatalf("warm activation must read the native port: err=%v reads=%d", err, native.reads)
	}
}

func TestActivationAccessDenialNamesYardAndRepair(t *testing.T) {
	cause := fmt.Errorf("yard alpha: get server extensions: %w: dial unix /private: %w", domain.ErrIncusAccessDenied, os.ErrPermission)
	message, retry := materializedConfigActivationError{cause, "alpha", "materialized config", "config status", "inspect", "/config"}.ActivationDiagnostic()
	if message != "yard alpha: native state cannot be inspected: this account cannot access the Incus socket" || retry != domain.IncusAccessNextStep {
		t.Fatalf("diagnostic = %q / %q", message, retry)
	}
}

// seedOwnerHostID makes a fixture warm: only an installation that ran init can
// own the native yards a fake Incus port reports. It records the identity init
// itself would derive on this host, so owner-bound fixtures stay consistent.
func seedOwnerHostID(t *testing.T, configHome string) {
	t.Helper()
	hostID, _, err := configsync.ResolveHostID(configHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeColdFile(t, filepath.Join(configHome, "host-id"), hostID+"\n", 0o600)
}

// grantedIncus denies the socket until the approved Incus stage grants access.
type grantedIncus struct {
	*testkit.Incus
	granted bool
}

func (native *grantedIncus) Instance(ctx context.Context, project, name string) (ports.InstanceInfo, error) {
	if !native.granted {
		return ports.InstanceInfo{}, fmt.Errorf("get instance: %w: %w", domain.ErrIncusAccessDenied, os.ErrPermission)
	}
	return native.Incus.Instance(ctx, project, name)
}

type grantingInitPlatform struct {
	*initPlatformFixture
	native *grantedIncus
}

func (platform *grantingInitPlatform) ApplyStage(ctx context.Context, stage ports.ReconcileStageID) error {
	if err := platform.initPlatformFixture.ApplyStage(ctx, stage); err != nil {
		return err
	}
	if stage == ports.ReconcileStageIncus {
		platform.native.granted = true
	}
	return nil
}

// A cold first init may plan before socket access exists, but it repeats the
// native owner check once its Incus stage grants access: another yard's
// instance left behind by a deleted configuration is never taken over.
func TestColdFirstInitRechecksNativeOwnerAfterAccessGrant(t *testing.T) {
	cliFixture, root, environment, _ := newMigrateCLIFixture(t, false)
	configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	writeColdFile(t, filepath.Join(configHome, "config.env"), "INCUS_PROJECT=subyard-beta\nYARD_INSTANCE_NAME=yard-beta\n", 0o600)
	if !coldInstallation(configHome, environmentValue(environment, "YARD_RUNTIME_ROOT")) {
		t.Fatal("fixture precondition: host-wide settings keep the installation cold")
	}
	native := &grantedIncus{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard-beta/yard-beta": {
		Name: "yard-beta", Project: "subyard-beta", Status: "Stopped",
		Config: map[string]string{"user.subyard.name": "beta", "user.subyard.managed": "true"},
	}}}}
	platform := &grantingInitPlatform{initPlatformFixture: newInitPlatformFixture(), native: native}
	platform.converged[ports.ReconcileStageIncus] = false
	platform.converged[ports.ReconcileStageInstance] = false
	var output bytes.Buffer
	program := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--yes"}, Environment: environment,
		Stdout: &output, Stderr: &output, InitPlatform: platform, Incus: native, Executor: native})
	if code := program.Run(context.Background()); code == 0 {
		t.Fatalf("init took over another yard's instance: %s", output.String())
	}
	t.Logf("applied=%v granted=%t", platform.applied, native.granted)
	if !native.granted || !strings.Contains(output.String(), "belongs to yard beta") {
		t.Fatalf("owner refusal missing after the access grant: granted=%t\n%s", native.granted, output.String())
	}
	for _, stage := range platform.applied {
		if stage != ports.ReconcileStageIncus {
			t.Fatalf("init applied %s before refusing the foreign instance", stage)
		}
	}
}

// deniedServer denies every socket read, as an inaccessible installed Incus does.
type deniedServer struct{ *testkit.Incus }

func (deniedServer) Server(context.Context) (ports.ServerInfo, error) {
	return ports.ServerInfo{}, fmt.Errorf("get server: %w: %w", domain.ErrIncusAccessDenied, os.ErrPermission)
}

func (deniedServer) Instance(context.Context, string, string) (ports.InstanceInfo, error) {
	return ports.InstanceInfo{}, fmt.Errorf("get instance: %w: %w", domain.ErrIncusAccessDenied, os.ErrPermission)
}

// Init's compatibility probe reports every local yard it cannot read, in
// stable order, not only the selected one.
func TestPlainInitMigrationProbeReportsEveryDeniedYard(t *testing.T) {
	cliFixture, root, environment, _ := newMigrateCLIFixture(t, true)
	program := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, Stdout: io.Discard, Stderr: io.Discard})
	configHome := program.env["SUBYARD_CONFIG_HOME"]
	seedOwnerHostID(t, configHome)
	writeColdFile(t, filepath.Join(configHome, "yards", "beta", "config.env"), "SSH_PORT=2302\n", 0o600)
	writeColdFile(t, filepath.Join(configHome, "yards", "alpha", "config.env"), "SSH_PORT=2301\n", 0o600)
	native := deniedServer{&testkit.Incus{}}
	program.options.Incus, program.options.Executor = native, native
	code, err := program.finishRegisteredInitMigrations(context.Background(), "default", nil, true)
	if code != -1 || !errors.Is(err, domain.ErrIncusAccessDenied) {
		t.Fatalf("probe = %d %v", code, err)
	}
	text := err.Error()
	positions := []int{}
	for _, yard := range []string{"default", "alpha", "beta"} {
		index := strings.Index(text, "yard "+yard+": native state cannot be inspected")
		if index < 0 {
			t.Fatalf("probe omitted yard %s: %s", yard, text)
		}
		positions = append(positions, index)
	}
	if positions[0] > positions[1] || positions[1] > positions[2] {
		t.Fatalf("refusals are not in YardNames order: %s", text)
	}
}

// denyFirstRead denies only the selected yard's probe, as for a selected yard
// that no local listing covers.
type denyFirstRead struct {
	*testkit.Incus
	reads int
}

func (native *denyFirstRead) Instance(context.Context, string, string) (ports.InstanceInfo, error) {
	native.reads++
	if native.reads == 1 {
		return ports.InstanceInfo{}, fmt.Errorf("get instance: %w: %w", domain.ErrIncusAccessDenied, os.ErrPermission)
	}
	return ports.InstanceInfo{}, ports.ErrInstanceNotFound
}

// A denied selected yard fails closed even when no local yard reports a denial.
func TestPlainInitMigrationProbeFailsClosedForTheSelectedYard(t *testing.T) {
	cliFixture, root, environment, _ := newMigrateCLIFixture(t, true)
	program := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, Stdout: io.Discard, Stderr: io.Discard})
	seedOwnerHostID(t, program.env["SUBYARD_CONFIG_HOME"])
	native := &denyFirstRead{Incus: &testkit.Incus{}}
	program.options.Incus, program.options.Executor = native, native
	code, err := program.finishRegisteredInitMigrations(context.Background(), "default", nil, true)
	if code != -1 || !errors.Is(err, domain.ErrIncusAccessDenied) ||
		!strings.Contains(err.Error(), "yard default: native state cannot be inspected") {
		t.Fatalf("selected yard denial did not fail closed: %d %v", code, err)
	}
}
