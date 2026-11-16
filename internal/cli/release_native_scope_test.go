package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
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
	if coldInstallation("relative/config", "") {
		t.Fatal("a relative configuration home must not prove a cold installation")
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
// own the native yards a fake Incus port reports.
func seedOwnerHostID(t *testing.T, configHome string) {
	t.Helper()
	writeColdFile(t, filepath.Join(configHome, "host-id"), "yard\n", 0o600)
}
