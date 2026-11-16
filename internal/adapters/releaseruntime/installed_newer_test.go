package releaseruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/blang/semver/v4"
)

func TestInstalledVersionNewerThanUsesSemverOrder(t *testing.T) {
	for _, test := range []struct {
		current, latest string
		newer           bool
	}{
		{"0.30.0-bughunt.2-cb4d68cb6c60", "0.29.0", true},
		{"0.30.0-rc.1-aaaaaaaaaaaa", "0.29.0", true},
		{"0.30.0-rc.1-aaaaaaaaaaaa", "0.30.0", false},
		{"0.30.0-aaaaaaaaaaaa", "0.30.0-rc.1", true},
		{"0.29.0-aaaaaaaaaaaa", "0.29.0", false},
		{"0.29.0-aaaaaaaaaaaa", "0.30.0", false},
		{"0.29.10-aaaaaaaaaaaa", "0.29.9", true},
		{"release-a", "0.29.0", false},
		{"0.29.0-aaaaaaaaaaaa", "not-a-version", false},
	} {
		if _, newer := installedVersionNewerThan(releasetransition.ReleaseID(test.current), test.latest); newer != test.newer {
			t.Errorf("installedVersionNewerThan(%q, %q) = %v, want %v", test.current, test.latest, newer, test.newer)
		}
	}
}

// readyCompletedFixture is a completed, verified installation of 1.2.3 whose
// plain update reaches release discovery.
func readyCompletedFixture(t *testing.T) protectedRuntimeTransitionFixture {
	t.Helper()
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalComplete)
	ready := fmt.Sprintf(
		`{"schemaVersion":1,"inspection":{"plan":"plan-v1-%s","assessment":{"action":"release.transition.v2","effect":"mutation","changed":false,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible"},"outcome":{"status":"ready","reachedGoal":true,"active":%q,"previous":"release-a","target":%q,"code":"ready","message":"the target release is active and the transition is verified","transaction":%q,"warnings":["recovery cleanup is pending"]}}}`,
		strings.Repeat("a", 64), fixture.target, fixture.target, fixture.transaction,
	)
	writeProtectedRuntimeFixtureEngine(t, fixture, fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition) cat >/dev/null; printf '%%s\n' %q ;;
  *) exit 64 ;;
esac
`, ready))
	return fixture
}

func TestPlainUpdateNeverSelectsAnOlderLatestRelease(t *testing.T) {
	for name, test := range map[string]struct {
		journal   bool
		arguments []string
		status    string
	}{
		"update":            {journal: true, arguments: []string{"--yes"}, status: "ready"},
		"forced update":     {journal: true, arguments: []string{"--yes", "--force"}, status: "ready"},
		"check":             {journal: true, arguments: []string{"--check"}, status: "ready"},
		"check json":        {journal: true, arguments: []string{"--check", "--json"}, status: "ready"},
		"update no journal": {arguments: []string{"--yes"}, status: "migration-required"},
		"check no journal":  {arguments: []string{"--check", "--json"}, status: "migration-required"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := readyCompletedFixture(t)
			if !test.journal {
				if err := os.Remove(fixture.journalPath); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			var stdout bytes.Buffer
			runtime := New(Config{
				Inputs: fixtureCommandInputs(fixture.environment()), Installer: fixture.installer,
				Stdout: &stdout, Stderr: &bytes.Buffer{},
				HTTPClient: latestReleaseClient(t, "v1.2.2", &calls),
			})
			arguments := append([]string{"--runtime-root", fixture.runtimeRoot}, test.arguments...)
			prepared, err := runtime.PrepareTransition(context.Background(), arguments, fixture.configHome, "default")
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Action != "update.check" || prepared.Changed || len(prepared.Steps) != 0 || prepared.TargetRelease != "" {
				t.Fatalf("older latest release was prepared as %#v", prepared)
			}
			if err := prepared.Execute(context.Background()); err != nil {
				t.Fatal(err)
			}
			output := stdout.String()
			want := []string{
				"installed version 1.2.3 is newer than the latest published release 1.2.2; no release was selected",
				test.status, fixture.target,
				"yard update --version 1.2.2 --runtime-root " + fixture.runtimeRoot + " --check",
			}
			if test.journal {
				want = append(want, "the target release is active and the transition is verified", "recovery cleanup is pending")
			} else {
				want = append(want, "run yard migrate --check")
			}
			for _, expected := range want {
				if !strings.Contains(output, expected) {
					t.Fatalf("installed-newer report lacks %q: %q", expected, output)
				}
			}
			if slices.Contains(test.arguments, "--check") {
				var report struct {
					Outcome        releasetransition.Outcome `json:"outcome"`
					InstalledNewer InstalledNewer            `json:"installedNewer"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &report); err != nil ||
					string(report.Outcome.Status) != test.status || report.InstalledNewer.LatestVersion != "1.2.2" ||
					len(report.InstalledNewer.Downgrade) != 2 {
					t.Fatalf("installed-newer check report = %#v, %v", report, err)
				}
			} else {
				if !strings.Contains(output, "[ ok ] No update installed") {
					t.Fatalf("installed-newer update report = %q", output)
				}
				// The human report renders the inspection's warnings.
				if test.journal && !strings.Contains(output, "\n  Warning: recovery cleanup is pending\n") {
					t.Fatalf("installed-newer update report dropped warnings: %q", output)
				}
			}
			if calls == 0 {
				t.Fatal("plain update did not discover the latest release")
			}
			if _, err := os.Lstat(fixture.publicationCapture); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("older latest release invoked publication: %v", err)
			}
			if _, err := os.Lstat(fixture.cache); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("older latest release created the release cache: %v", err)
			}
			if current, err := os.Readlink(fixture.runtimeRoot + "/current"); err != nil || current != "releases/"+fixture.target {
				t.Fatalf("active runtime changed: %s, %v", current, err)
			}
		})
	}
}

// Equal and newer discovered releases, and an explicit older --version, keep
// the ordinary verified download path; the fixture has no assets to download.
func TestUpdateKeepsOrdinarySelectionUnlessDiscoveryIsOlder(t *testing.T) {
	for name, test := range map[string]struct {
		latest      string
		arguments   []string
		environment map[string]string
		lookups     bool
	}{
		"equal latest":       {latest: "v1.2.3", lookups: true},
		"newer latest":       {latest: "v1.3.0", lookups: true},
		"explicit downgrade": {latest: "v1.2.3", arguments: []string{"--version", "1.2.2"}},
		"environment downgrade": {
			latest: "v1.2.3", environment: map[string]string{"YARD_RELEASE_VERSION": "1.2.2"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := readyCompletedFixture(t)
			environment := fixture.environment()
			maps.Copy(environment, test.environment)
			calls := 0
			var stdout bytes.Buffer
			runtime := New(Config{
				Inputs: fixtureCommandInputs(environment), Installer: fixture.installer,
				Stdout: &stdout, Stderr: &bytes.Buffer{},
				HTTPClient: latestReleaseClient(t, test.latest, &calls),
			})
			arguments := append([]string{"--runtime-root", fixture.runtimeRoot, "--yes"}, test.arguments...)
			prepared, err := runtime.PrepareTransition(context.Background(), arguments, fixture.configHome, "default")
			if prepared.Close != nil {
				defer prepared.Close()
			}
			if err == nil {
				t.Fatalf("selection skipped the release download: %#v", prepared)
			}
			if strings.Contains(stdout.String()+err.Error(), "nothing to update") {
				t.Fatalf("selection reported installed-newer: %v", err)
			}
			if (calls != 0) != test.lookups {
				t.Fatalf("latest lookups = %d", calls)
			}
			if _, statErr := os.Lstat(fixture.cache); statErr != nil {
				t.Fatalf("selection did not reach the release download: %v (prepare: %v)", statErr, err)
			}
		})
	}
}

// Only a ready protected inspection, or no journal at all, is a no-op success.
// Other inspection results keep their own report and never select the older release.
func TestInstalledNewerKeepsUnreadyInspectionResults(t *testing.T) {
	transaction := releasetransition.TransactionID("tx-0123456789abcdef")
	blocked := releasetransition.Outcome{
		Status: releasetransition.StatusOperatorActionRequired, Active: "1.2.3-aaaaaaaaaaaa",
		Target: "1.2.3-aaaaaaaaaaaa", Code: releasetransition.CodePlanStale,
		Message: "the complete journal bindings do not match the verified release",
		Retry:   "run yard update --check", Transaction: &transaction,
	}
	installed := semver.MustParse("1.2.3")
	links := runtimeLinkSnapshot{current: runtimeLinkState{present: true, target: "releases/1.2.3-aaaaaaaaaaaa"}}
	for name, test := range map[string]struct {
		protected *protectedTransitionInspection
		err       error
	}{
		"operator action required": {protected: &protectedTransitionInspection{inspection: releasetransition.Inspection{Outcome: &blocked}}},
		"failed public inspection": {err: publicReleaseInspectionError{cause: errors.New("journal unreadable"), outcome: blocked}},
		"failed inspection":        {err: errors.New("owner crashed")},
	} {
		t.Run(name, func(t *testing.T) {
			for _, check := range []bool{false, true} {
				var stdout bytes.Buffer
				runtime := New(Config{Stdout: &stdout})
				parsed := options{version: "1.2.2", check: check}
				prepared, err := runtime.prepareInstalledNewer(parsed, links, installed, test.protected, test.err)
				if !check || name == "failed inspection" {
					if err == nil || !strings.Contains(err.Error(), "newer than the latest published release 1.2.2") {
						t.Fatalf("check=%v: unready inspection prepared %#v, %v", check, prepared, err)
					}
					if outcome, public := TransitionOutcome(err); name != "failed inspection" && (!public || outcome.Code != blocked.Code) {
						t.Fatalf("check=%v: unready inspection lost its outcome: %v", check, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := prepared.Execute(context.Background()); err != nil {
					t.Fatal(err)
				}
				var report struct {
					Outcome        releasetransition.Outcome `json:"outcome"`
					InstalledNewer InstalledNewer            `json:"installedNewer"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &report); err != nil ||
					report.Outcome.Status != blocked.Status || report.Outcome.Message != blocked.Message ||
					report.InstalledNewer.Downgrade[1] != "yard update --version 1.2.2" {
					t.Fatalf("unready check report = %q, %v", stdout.String(), err)
				}
			}
		})
	}
}
