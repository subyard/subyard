package releaseruntime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func latestReleaseClient(t *testing.T, tag string, calls *int) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		*calls++
		if request.URL.String() != "https://api.github.com/repos/Subyard/Subyard/releases/latest" {
			t.Fatalf("unexpected lookup %s", request.URL)
		}
		if tag == "" {
			return nil, errors.New("network unreachable")
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK",
			Body: io.NopCloser(strings.NewReader(`{"tag_name":"` + tag + `"}`))}, nil
	})}
}

func pinnedFixtureVersion(t *testing.T, configHome string) string {
	t.Helper()
	store, err := releasetransition.NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := releasetransition.ReleaseIDVersion(journal.Goal.Target)
	if err != nil {
		t.Fatal(err)
	}
	return pinned.String()
}

func TestNewerReleaseProbeNamesOnlyANewerPublishedRelease(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
	pinned := pinnedFixtureVersion(t, fixture.configHome)
	plain := options{repository: "Subyard/Subyard", root: "/home/operator/.subyard/runtime"}
	for name, test := range map[string]struct {
		tag     string
		parsed  options
		version string
		command string
	}{
		"newer":         {tag: "v99.0.0", parsed: plain, version: "99.0.0", command: "yard update --version 99.0.0"},
		"same":          {tag: "v" + pinned, parsed: plain},
		"older":         {tag: "v0.0.1", parsed: plain},
		"unreachable":   {parsed: plain},
		"malformed tag": {tag: "99.0.0", parsed: plain},
		"explicit root": {
			tag:     "v99.0.0",
			parsed:  options{repository: "Subyard/Subyard", root: "/srv/my runtime", rootExplicit: true},
			version: "99.0.0", command: "yard update --version 99.0.0 --runtime-root '/srv/my runtime'",
		},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			runtime := New(Config{HTTPClient: latestReleaseClient(t, test.tag, &calls)})
			probe := runtime.newerReleaseProbe(test.parsed, fixture.configHome)
			if probe == nil {
				t.Fatal("an ordinary resume has no newer-release probe")
			}
			got := Prepared{newerRelease: probe}.NewerRelease(context.Background())
			if got.Version != test.version || got.Command != test.command || calls != 1 {
				t.Fatalf("NewerRelease() = %#v after %d lookups", got, calls)
			}
		})
	}
}

func TestNewerReleaseProbeLeavesExplicitSelectionsAlone(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
	calls := 0
	runtime := New(Config{HTTPClient: latestReleaseClient(t, "v99.0.0", &calls)})
	for name, parsed := range map[string]options{
		"offline":  {offline: true},
		"check":    {check: true},
		"rollback": {rollback: true},
		"force":    {force: true},
		"version":  {version: "1.2.3"},
		"tag":      {tag: "v1.2.3"},
	} {
		if probe := runtime.newerReleaseProbe(parsed, fixture.configHome); probe != nil {
			t.Fatalf("%s preparation received a newer-release probe", name)
		}
	}
	if got := (Prepared{}).NewerRelease(context.Background()); got != (NewerRelease{}) {
		t.Fatalf("an ordinary preparation named %#v", got)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	probe := runtime.newerReleaseProbe(options{repository: "Subyard/Subyard"}, fixture.configHome)
	if got := (Prepared{newerRelease: probe}).NewerRelease(cancelled); got != (NewerRelease{}) {
		t.Fatalf("a cancelled update named %#v", got)
	}
	complete := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalComplete)
	probe = runtime.newerReleaseProbe(options{repository: "Subyard/Subyard"}, complete.configHome)
	if got := (Prepared{newerRelease: probe}).NewerRelease(context.Background()); got != (NewerRelease{}) {
		t.Fatalf("a completed journal named %#v", got)
	}
	if calls != 1 {
		t.Fatalf("explicit selections or cancellation looked up the channel: %d lookups", calls)
	}
}

func TestOrdinaryResumeCarriesNewerReleaseProbe(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
	for name, test := range map[string]struct {
		arguments []string
		command   string
	}{
		"resume":  {arguments: []string{"--runtime-root", fixture.runtimeRoot}, command: "yard update --version 99.0.0 --runtime-root " + fixture.runtimeRoot},
		"offline": {arguments: []string{"--offline", "--runtime-root", fixture.runtimeRoot}},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			runtime := New(Config{
				Inputs: fixtureCommandInputs(fixture.environment()), Installer: fixture.installer,
				Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
				HTTPClient: latestReleaseClient(t, "v99.0.0", &calls),
			})
			prepared, err := runtime.PrepareTransition(context.Background(), test.arguments, fixture.configHome, "default")
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Close != nil {
				defer prepared.Close()
			}
			if calls != 0 {
				t.Fatalf("preparation looked up the channel %d times", calls)
			}
			if got := prepared.NewerRelease(context.Background()); got.Command != test.command {
				t.Fatalf("NewerRelease() = %#v", got)
			}
		})
	}
}
