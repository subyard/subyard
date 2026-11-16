package releaseruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
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
		`{"schemaVersion":1,"inspection":{"plan":"plan-v1-%s","assessment":{"action":"release.transition.v2","effect":"mutation","changed":false,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible"},"outcome":{"status":"ready","reachedGoal":true,"active":%q,"previous":"release-a","target":%q,"code":"ready","message":"the target release is active and the transition is verified","transaction":%q}}}`,
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
		"check":             {journal: true, arguments: []string{"--check"}, status: "ready"},
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
			if !strings.Contains(output, "installed version 1.2.3 is newer than the latest published release 1.2.2; nothing to update") ||
				!strings.Contains(output, test.status) || !strings.Contains(output, fixture.target) {
				t.Fatalf("installed-newer report = %q", output)
			}
			if strings.Contains(test.arguments[0], "--yes") &&
				!strings.Contains(output, "yard update --version 1.2.2 --runtime-root "+fixture.runtimeRoot) {
				t.Fatalf("installed-newer report has no explicit downgrade command: %q", output)
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
		latest    string
		arguments []string
		lookups   bool
	}{
		"equal latest":       {latest: "v1.2.3", lookups: true},
		"newer latest":       {latest: "v1.3.0", lookups: true},
		"explicit downgrade": {latest: "v1.2.3", arguments: []string{"--version", "1.2.2"}},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := readyCompletedFixture(t)
			calls := 0
			var stdout bytes.Buffer
			runtime := New(Config{
				Inputs: fixtureCommandInputs(fixture.environment()), Installer: fixture.installer,
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
