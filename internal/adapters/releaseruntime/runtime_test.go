package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestSourceIngressRequestFromEnvironmentIsAllOrNothing(t *testing.T) {
	values := map[string]string{
		"SUBYARD_SOURCE_INGRESS_V1_ROOT":     "/home/operator/source",
		"SUBYARD_SOURCE_INGRESS_V1_DATA":     "/home/operator/.subyard",
		"SUBYARD_SOURCE_INGRESS_V1_BIN":      "/home/operator/.local/bin",
		"SUBYARD_SOURCE_INGRESS_V1_RC":       "/home/operator/.bashrc",
		"SUBYARD_SOURCE_INGRESS_V1_LOGIN_RC": "/home/operator/.profile",
	}
	request, err := sourceIngressRequestFromEnvironment(values)
	if err != nil || request == nil || request.SourceRoot != values["SUBYARD_SOURCE_INGRESS_V1_ROOT"] {
		t.Fatalf("sourceIngressRequestFromEnvironment() = %#v, %v", request, err)
	}
	delete(values, "SUBYARD_SOURCE_INGRESS_V1_RC")
	if _, err := sourceIngressRequestFromEnvironment(values); err == nil {
		t.Fatal("partial source ingress environment was accepted")
	}
	if request, err := sourceIngressRequestFromEnvironment(nil); err != nil || request != nil {
		t.Fatalf("empty source ingress environment = %#v, %v", request, err)
	}
}

func TestLatestTagReportsGitHubHTTPStatus(t *testing.T) {
	release := New(Config{
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Status:     "403 Forbidden",
				Body:       io.NopCloser(strings.NewReader(`{"message":"rate limit exceeded"}`)),
			}, nil
		})},
	})

	_, err := release.latestTag(context.Background(), "Subyard/Subyard")
	if err == nil || err.Error() != "GitHub latest release request returned 403 Forbidden" {
		t.Fatalf("latest release error = %v", err)
	}
}

func TestProductionTransitionPathDoesNotDelegateToLegacyMutation(t *testing.T) {
	source, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(source, []byte("func (runtime *Runtime) PrepareTransition("))
	end := bytes.Index(source, []byte("func (runtime *Runtime) prepareRetainedTransition("))
	if start < 0 || end <= start {
		t.Fatal("could not isolate production release transition entry point")
	}
	entrypoint := source[start:end]
	for _, forbidden := range [][]byte{
		[]byte("runtime.Prepare(ctx, arguments)"),
		[]byte("_migrate\", \"apply"),
		[]byte("_migrate\", \"finalize"),
		[]byte("_migrate\", \"rollback"),
		[]byte("_migrate\", \"cleanup"),
	} {
		if bytes.Contains(entrypoint, forbidden) {
			t.Fatalf("production transition entry point contains superseded delegation %q", forbidden)
		}
	}

	installer, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "install-runtime-release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"apply", "finalize", "rollback", "cleanup"} {
		if bytes.Contains(installer, []byte("_migrate "+verb)) {
			t.Fatalf("current installer still invokes mutating _migrate %s", verb)
		}
	}
}

func TestCandidateBlockedInspectionCheckReturnsStructuredPublicOutcome(t *testing.T) {
	root := t.TempDir()
	response := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["inspect the blocked transition"]},"blockers":[{"code":"migration-stale","resource":"yard.fixture","message":"the resource changed","retry":"run yard update --check"}],"outcome":{"status":"operator-action-required","reachedGoal":false,"active":"release-a","previous":"release-z","target":"release-b","code":"migration-stale","message":"the resource changed","retry":"run yard update --check","transaction":"tx-0123456789abcdef"}}}`
	candidate := writeRuntimeCandidateFixture(t, root, response)
	request := releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Mode:          releasetransition.ProcessInspect,
		RuntimeRoot:   root,
		ConfigHome:    root,
		Target:        "release-b",
		Direction:     releasetransition.DirectionActivateTarget,
	}

	t.Run("check", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		runtime := New(Config{Stdout: &stdout, Stderr: &stderr})
		prepared, err := prepareCandidateTransitionForTest(runtime,
			context.Background(), options{check: true, root: root}, candidate, request,
		)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.Action != "update.check" || prepared.Changed || prepared.RefreshConfigs {
			t.Fatalf("prepared check = %#v", prepared)
		}
		if err := prepared.Execute(context.Background()); err != nil {
			t.Fatal(err)
		}
		var output struct {
			Outcome *releasetransition.Outcome `json:"outcome"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &output); err != nil {
			t.Fatalf("check output = %q: %v", stdout.String(), err)
		}
		if output.Outcome == nil ||
			output.Outcome.Status != releasetransition.StatusOperatorActionRequired ||
			output.Outcome.Code != releasetransition.CodeMigrationStale ||
			output.Outcome.Active != "release-a" || output.Outcome.Previous == nil ||
			*output.Outcome.Previous != "release-z" || output.Outcome.Target != "release-b" ||
			output.Outcome.Transaction == nil || output.Outcome.Retry != "run yard update --check" {
			t.Fatalf("check public outcome = %#v", output.Outcome)
		}
	})

	t.Run("mutation", func(t *testing.T) {
		runtime := New(Config{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
		_, err := prepareCandidateTransitionForTest(runtime,
			context.Background(), options{root: root}, candidate, request,
		)
		if err == nil || !strings.Contains(err.Error(),
			"release transition operator-action-required: code=migration-stale active=release-a previous=release-z target=release-b transaction=tx-0123456789abcdef") ||
			!strings.Contains(err.Error(), "next: run yard update --check") {
			t.Fatalf("mutating blocked preparation error = %v", err)
		}
	})
}

func TestCandidateInvalidRegistryOutcomeStaysStructured(t *testing.T) {
	root := t.TempDir()
	response := `{"schemaVersion":1,"outcome":{"status":"operator-action-required","reachedGoal":false,"active":"release-a","previous":"release-z","target":"release-b","code":"registry-invalid","message":"the candidate release transition registry is invalid","retry":"install a release with a valid transition registry, then run yard update --check"}}`
	candidate := writeRuntimeCandidateFixture(t, root, response)
	request := releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Mode:          releasetransition.ProcessInspect,
		RuntimeRoot:   root, ConfigHome: root,
		Target: "release-b", Direction: releasetransition.DirectionActivateTarget,
	}

	t.Run("check", func(t *testing.T) {
		var stdout bytes.Buffer
		runtime := New(Config{Stdout: &stdout, Stderr: &bytes.Buffer{}})
		prepared, err := prepareCandidateTransitionForTest(runtime,
			context.Background(), options{check: true, root: root}, candidate, request,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := prepared.Execute(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{
			`"code":"registry-invalid"`, `"active":"release-a"`,
			`"previous":"release-z"`, `"target":"release-b"`,
		} {
			if !strings.Contains(stdout.String(), field) {
				t.Fatalf("invalid registry check output = %q", stdout.String())
			}
		}
	})

	t.Run("mutation", func(t *testing.T) {
		runtime := New(Config{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
		_, err := prepareCandidateTransitionForTest(runtime,
			context.Background(), options{root: root}, candidate, request,
		)
		if err == nil || !strings.Contains(err.Error(),
			"code=registry-invalid active=release-a previous=release-z target=release-b") ||
			!strings.Contains(err.Error(), "next: install a release with a valid transition registry") {
			t.Fatalf("invalid registry mutation error = %v", err)
		}
	})
}

func TestRollbackFailuresHaveStructuredPublicOutcomes(t *testing.T) {
	for _, test := range []struct {
		name             string
		withPrevious     bool
		code             releasetransition.OutcomeCode
		target, previous string
	}{
		{
			name: "retention expired", code: releasetransition.CodeRollbackExpired,
			target: "unknown", previous: "none",
		},
		{
			name: "retained runtime incompatible", withPrevious: true,
			code:   releasetransition.CodeRollbackIncompatible,
			target: "release-b", previous: "release-b",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			runtimeRoot := filepath.Join(root, "runtime")
			cache := filepath.Join(root, "cache")
			for _, directory := range []string{
				filepath.Join(runtimeRoot, "releases", "release-a", "bin"), cache,
			} {
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(
				filepath.Join(runtimeRoot, "releases", "release-a", "bin", "yard-engine"),
				[]byte("#!/bin/sh\nexit 0\n"), 0o700,
			); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("releases/release-a", filepath.Join(runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			if test.withPrevious {
				if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", "release-b"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("releases/release-b", filepath.Join(runtimeRoot, "previous")); err != nil {
					t.Fatal(err)
				}
			}
			runtime := New(Config{
				Environment: map[string]string{"HOME": root, "YARD_RELEASE_CACHE": cache},
				Stdout:      &bytes.Buffer{}, Stderr: &bytes.Buffer{},
			})

			_, err := runtime.PrepareTransition(
				context.Background(), []string{"--rollback", "--runtime-root", runtimeRoot},
				filepath.Join(root, "config"), "default", nil,
			)
			if err == nil || !strings.Contains(err.Error(),
				"release transition operator-action-required: code="+string(test.code)+
					" active=release-a previous="+test.previous+" target="+test.target) ||
				!strings.Contains(err.Error(), "next: ") {
				t.Fatalf("rollback failure = %v", err)
			}
		})
	}
}

func TestRollbackToRetainedPreV2ReleaseUsesVerifiedActiveTransitionOwner(t *testing.T) {
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "runtime")
	configHome := filepath.Join(root, "config")
	capture := filepath.Join(root, "transition-requests.jsonl")
	legacyUnexpected := filepath.Join(root, "legacy-transition-called")
	inspection := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["roll back through the verified active transition owner"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"release-b","previous":"release-a","target":"release-a","code":"transition-required","message":"the rollback transition has not started","retry":"run yard update --rollback"}}}`
	ready := `{"schemaVersion":1,"outcome":{"status":"ready","reachedGoal":true,"active":"release-a","previous":"release-b","target":"release-a","code":"ready","message":"verified","transaction":"tx-0123456789abcdef"}}`
	ownerPayload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition)
    request=$(cat)
    printf '%%s\n' "$request" >> %q
    case "$request" in
      *'"mode":"inspect"'*) printf '%%s\n' %q ;;
      *'"mode":"converge"'*) printf '%%s\n' %q ;;
      *) exit 64 ;;
    esac
    ;;
  *) exit 64 ;;
esac
`, capture, inspection, ready)
	owner := writeRuntimeCandidatePayload(t, runtimeRoot, ownerPayload)
	legacyRoot := filepath.Join(runtimeRoot, "releases", "release-a")
	if err := os.MkdirAll(filepath.Join(legacyRoot, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	legacyPayload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 0.8.0\n' ;;
  _release-transition) : > %q; exit 93 ;;
  *) exit 64 ;;
esac
`, legacyUnexpected)
	legacyEngine := filepath.Join(legacyRoot, "bin", "yard-engine")
	if err := os.WriteFile(legacyEngine, []byte(legacyPayload), 0o700); err != nil {
		t.Fatal(err)
	}
	legacyDigest := sha256.Sum256([]byte(legacyPayload))
	legacyManifest := fmt.Sprintf("%x  ./bin/yard-engine\n", legacyDigest)
	if err := os.WriteFile(
		filepath.Join(legacyRoot, "runtime-files.sha256"), []byte(legacyManifest), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/"+string(owner.release), filepath.Join(runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/release-a", filepath.Join(runtimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}

	release := New(Config{
		Environment: map[string]string{"HOME": root},
		Stdout:      &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	prepared, err := release.PrepareTransition(
		context.Background(), []string{"--rollback", "--runtime-root", runtimeRoot},
		configHome, "default", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Action != "update.rollback" || !prepared.Changed {
		t.Fatalf("legacy rollback preparation = %#v", prepared)
	}
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyUnexpected); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retained pre-v2 engine owned the transition: %v", err)
	}
	payload, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(payload), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("transition request count=%d payload=%q", len(lines), payload)
	}
	wantArtifact := sha256.Sum256([]byte(legacyManifest))
	for index, line := range lines {
		var request releasetransition.ProcessRequest
		if err := json.Unmarshal(line, &request); err != nil {
			t.Fatalf("request %d: %v", index, err)
		}
		if request.Target != "release-a" ||
			request.Direction != releasetransition.DirectionActivatePrevious ||
			request.ArtifactDigest != releasetransition.Fingerprint(fmt.Sprintf("%x", wantArtifact)) ||
			request.RegistryDigest == "" {
			t.Fatalf("request %d = %#v", index, request)
		}
	}
}

func TestProtectedPreV2RollbackIsInspectedByRetainedTransitionOwner(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalComplete)
	ownerRelease := fixture.target
	oldRelease := releasetransition.ReleaseID("0.8.0-aaaaaaaaaaaa")
	oldRoot := filepath.Join(fixture.runtimeRoot, "releases", string(oldRelease))
	if err := os.MkdirAll(filepath.Join(oldRoot, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldTransitionCalled := filepath.Join(filepath.Dir(fixture.runtimeRoot), "old-transition-called")
	oldPayload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 0.8.0\n' ;;
  _release-transition) : > %q; exit 93 ;;
  *) exit 64 ;;
esac
`, oldTransitionCalled)
	oldEngineDigest := sha256.Sum256([]byte(oldPayload))
	oldManifest := fmt.Sprintf("%x  ./bin/yard-engine\n", oldEngineDigest)
	if err := os.WriteFile(filepath.Join(oldRoot, "bin", "yard-engine"), []byte(oldPayload), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(oldRoot, "runtime-files.sha256"), []byte(oldManifest), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	requestCapture := filepath.Join(filepath.Dir(fixture.runtimeRoot), "rollback-inspection.json")
	readyInspection := fmt.Sprintf(
		`{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":false,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible"},"outcome":{"status":"ready","reachedGoal":true,"active":%q,"previous":%q,"target":%q,"code":"ready","message":"verified","transaction":%q}}}`,
		oldRelease, ownerRelease, oldRelease, fixture.transaction,
	)
	ownerPayload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition) cat > %q; printf '%%s\n' %q ;;
  *) exit 64 ;;
esac
`, requestCapture, readyInspection)
	if err := os.WriteFile(fixture.engine, []byte(ownerPayload), 0o700); err != nil {
		t.Fatal(err)
	}
	ownerEngineDigest := sha256.Sum256([]byte(ownerPayload))
	registryPath := filepath.Join(
		fixture.runtimeRoot, "releases", ownerRelease, "config", "release-transition.json",
	)
	registry, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	registryDigest := sha256.Sum256(registry)
	ownerManifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n",
		ownerEngineDigest, registryDigest)
	if err := os.WriteFile(
		filepath.Join(fixture.runtimeRoot, "releases", ownerRelease, "runtime-files.sha256"),
		[]byte(ownerManifest), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(fixture.runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.runtimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/"+string(oldRelease), filepath.Join(fixture.runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/"+ownerRelease, filepath.Join(fixture.runtimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}

	journalPayload, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(journalPayload)
	if err != nil {
		t.Fatal(err)
	}
	previous := oldRelease
	journal.Goal = releasetransition.Goal{
		Target: oldRelease, Direction: releasetransition.DirectionActivatePrevious,
	}
	journal.Releases = releasetransition.ReleasePair{
		From: releasetransition.ReleaseID(ownerRelease), Previous: &previous, Target: oldRelease,
	}
	oldManifestDigest := sha256.Sum256([]byte(oldManifest))
	journal.ArtifactDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", oldManifestDigest))
	journal.RegistryDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", registryDigest))
	journalPayload, err = releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.journalPath, journalPayload, 0o600); err != nil {
		t.Fatal(err)
	}

	runtime := New(Config{
		Environment: fixture.environment(), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	outcome, err := runtime.InspectMutationGate(
		context.Background(), fixture.runtimeRoot, fixture.configHome, "default", nil,
	)
	if err != nil || outcome != nil {
		t.Fatalf("protected rollback inspection outcome=%#v err=%v", outcome, err)
	}
	if _, err := os.Stat(oldTransitionCalled); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-v2 target owned protected rollback inspection: %v", err)
	}
	requestPayload, err := os.ReadFile(requestCapture)
	if err != nil {
		t.Fatal(err)
	}
	var request releasetransition.ProcessRequest
	if err := json.Unmarshal(requestPayload, &request); err != nil {
		t.Fatal(err)
	}
	if request.Target != oldRelease || request.RegistryDigest != journal.RegistryDigest ||
		request.ArtifactDigest != journal.ArtifactDigest {
		t.Fatalf("protected rollback request = %#v", request)
	}
}

func TestCandidateTransitionRejectsTrailingResponseJSON(t *testing.T) {
	root := t.TempDir()
	candidate := writeRuntimeCandidateFixture(t, root, `{"schemaVersion":1}{}`)
	runtime := New(Config{Stderr: &bytes.Buffer{}})
	verified, err := runtime.verifyPublishedCandidate(
		context.Background(), candidate, root, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	_, err = runtime.invokeVerifiedCandidateTransition(
		context.Background(), verified,
		releasetransition.ProcessRequest{SchemaVersion: 1},
		"",
	)
	if err == nil || !strings.Contains(err.Error(), "invalid release transition response") {
		t.Fatalf("candidate trailing response error = %v", err)
	}
}

func TestVerifiedCandidateRootDescriptorSurvivesAncestorReplacement(t *testing.T) {
	parent := t.TempDir()
	runtimeRoot := filepath.Join(parent, "runtime")
	candidateRoot := filepath.Join(runtimeRoot, "releases", "release-b")
	if err := os.MkdirAll(candidateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidateRoot, "marker"), []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := publishedCandidate{release: "release-b", root: candidateRoot}
	root, err := openVerifiedCandidateRoot(candidate, runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := os.Rename(runtimeRoot, filepath.Join(parent, "displaced-runtime")); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(runtimeRoot, "releases", "release-b")
	if err := os.MkdirAll(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replacement, "marker"), []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	marker, err := openCandidateFile(int(root.Fd()), "marker")
	if err != nil {
		t.Fatal(err)
	}
	defer marker.Close()
	payload, err := io.ReadAll(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "original\n" {
		t.Fatalf("pinned candidate marker = %q", payload)
	}
}

func TestVerifiedCandidateInvocationBindsRegistryDigest(t *testing.T) {
	root := t.TempDir()
	capture := filepath.Join(filepath.Dir(root), "candidate-request.json")
	response := `{"schemaVersion":1}`
	payload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition) cat > %q; printf '%%s\n' %q ;;
  *) exit 64 ;;
esac
`, capture, response)
	candidate := writeRuntimeCandidatePayload(t, root, payload)
	registry := []byte("{}\n")
	runtime := New(Config{Stderr: &bytes.Buffer{}})
	verified, err := runtime.verifyPublishedCandidate(
		context.Background(), candidate, root, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	if _, err := runtime.invokeVerifiedCandidateTransition(
		context.Background(), verified,
		releasetransition.ProcessRequest{
			SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
			RuntimeRoot:   root,
		},
		"",
	); err != nil {
		t.Fatal(err)
	}
	captured, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var request releasetransition.ProcessRequest
	if err := json.Unmarshal(captured, &request); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(registry)
	if request.RegistryDigest != releasetransition.Fingerprint(fmt.Sprintf("%x", want)) {
		t.Fatalf("registry digest = %q", request.RegistryDigest)
	}
}

func TestCandidateProtocolRejectsLegacyManifestWithoutV2Registry(t *testing.T) {
	root := t.TempDir()
	candidate := writeRuntimeCandidateFixture(t, root, `{"schemaVersion":1}`)
	if err := os.RemoveAll(filepath.Join(candidate.root, "config")); err != nil {
		t.Fatal(err)
	}
	engine, err := os.ReadFile(filepath.Join(candidate.root, "bin", "yard-engine"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(engine)
	if err := os.WriteFile(
		filepath.Join(candidate.root, "runtime-files.sha256"),
		[]byte(fmt.Sprintf("%x  ./bin/yard-engine\n", digest)), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	runtime := New(Config{Stderr: &bytes.Buffer{}})
	verified, err := runtime.verifyPublishedCandidate(context.Background(), candidate, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	_, err = runtime.invokeVerifiedCandidateTransition(
		context.Background(), verified,
		releasetransition.ProcessRequest{SchemaVersion: 1}, "",
	)
	if err == nil || !strings.Contains(err.Error(), "does not bind release transition registry") {
		t.Fatalf("legacy candidate protocol boundary error = %v", err)
	}
}

func TestPrepareTransitionUsesExactPublishedRecoveryWithoutLatestOrPublication(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
	transport := &countingErrorTransport{}
	runtime := New(Config{
		Environment: fixture.environment(), Installer: fixture.installer,
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		HTTPClient: &http.Client{Transport: transport},
	})

	prepared, err := runtime.PrepareTransition(
		context.Background(), []string{"--runtime-root", fixture.runtimeRoot},
		fixture.configHome, "default", nil,
	)
	if err != nil {
		cause := err
		for errors.Unwrap(cause) != nil {
			cause = errors.Unwrap(cause)
		}
		t.Fatalf("prepare exact recovery: %v (cause: %v)", err, cause)
	}
	if prepared.Action != "update.activate" || prepared.Changed {
		t.Fatalf("exact recovery preparation = %#v", prepared)
	}
	if transport.calls != 0 {
		t.Fatalf("exact recovery resolved or downloaded a release: calls=%d", transport.calls)
	}
	if _, err := os.Lstat(fixture.publicationCapture); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact recovery invoked publication: %v", err)
	}
	if _, err := os.Lstat(fixture.cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact recovery created the release cache: %v", err)
	}
}

func TestPrepareTransitionCheckOfProtectedRecoveryIsReadOnly(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
	before, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Lstat(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	transport := &countingErrorTransport{}
	var stdout bytes.Buffer
	runtime := New(Config{
		Environment: fixture.environment(), Installer: fixture.installer,
		Stdout: &stdout, Stderr: &bytes.Buffer{},
		HTTPClient: &http.Client{Transport: transport},
	})

	prepared, err := runtime.PrepareTransition(
		context.Background(), []string{"--check", "--runtime-root", fixture.runtimeRoot},
		fixture.configHome, "default", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Action != "update.check" || prepared.Changed || prepared.RefreshConfigs {
		t.Fatalf("protected recovery check preparation = %#v", prepared)
	}
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"status":"recovering"`) ||
		!strings.Contains(stdout.String(), `"target":"1.2.3-aaaaaaaaaaaa"`) {
		t.Fatalf("protected recovery check output = %q", stdout.String())
	}
	after, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Lstat(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !os.SameFile(beforeInfo, afterInfo) ||
		beforeInfo.Mode() != afterInfo.Mode() {
		t.Fatal("protected recovery check changed its journal")
	}
	if transport.calls != 0 {
		t.Fatalf("protected recovery check used the network: calls=%d", transport.calls)
	}
	if _, err := os.Lstat(fixture.publicationCapture); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("protected recovery check invoked publication: %v", err)
	}
	if _, err := os.Lstat(fixture.cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("protected recovery check created the release cache: %v", err)
	}
}

func TestPrepareTransitionRejectsArbitraryV0111RecoveryCandidate(t *testing.T) {
	tests := []struct {
		name               string
		prepare            func(*testing.T, *v0111CandidateRoutingFixture) (string, []string)
		tamperRoot         bool
		environmentVersion bool
		wantRetry          string
	}{
		{
			name: "ordinary active runtime update",
			prepare: func(_ *testing.T, fixture *v0111CandidateRoutingFixture) (string, []string) {
				return fixture.source.root, []string{"--offline", "--version", fixture.candidateVersion}
			},
		},
		{
			name: "same target",
			prepare: func(_ *testing.T, fixture *v0111CandidateRoutingFixture) (string, []string) {
				return fixture.source.root, []string{"--offline", "--version", fixture.sourceVersion}
			},
		},
		{
			name: "rollback",
			prepare: func(_ *testing.T, fixture *v0111CandidateRoutingFixture) (string, []string) {
				return fixture.candidate.root, []string{"--rollback"}
			},
		},
		{
			name: "force",
			prepare: func(_ *testing.T, fixture *v0111CandidateRoutingFixture) (string, []string) {
				return fixture.candidate.root, []string{
					"--offline", "--version", fixture.candidateVersion, "--force",
				}
			},
		},
		{
			name:       "unverified root",
			tamperRoot: true,
			prepare: func(_ *testing.T, fixture *v0111CandidateRoutingFixture) (string, []string) {
				return fixture.candidate.root, []string{"--offline", "--version", fixture.candidateVersion}
			},
		},
		{
			name: "ambiguous version match",
			prepare: func(t *testing.T, fixture *v0111CandidateRoutingFixture) (string, []string) {
				writeVersionedRuntimeCandidate(t, fixture.runtimeRoot,
					"0.11.2-cccccccccccc", fixture.candidateVersion,
					"#!/bin/sh\ncase \"${1:-}\" in --version) printf 'yard-engine 0.11.2\\n' ;; *) exit 64 ;; esac\n",
				)
				return fixture.runtimeRoot, []string{"--offline", "--version", fixture.candidateVersion}
			},
		},
		{
			name: "requested version mismatch",
			prepare: func(_ *testing.T, fixture *v0111CandidateRoutingFixture) (string, []string) {
				return fixture.candidate.root, []string{"--offline", "--version", "0.11.3"}
			},
		},
		{
			name:               "environment version without explicit version option",
			environmentVersion: true,
			wantRetry:          "run the verified standalone release with --offline and its exact --version",
			prepare: func(_ *testing.T, fixture *v0111CandidateRoutingFixture) (string, []string) {
				return fixture.candidate.root, []string{"--offline"}
			},
		},
		{
			name:      "candidate registry missing",
			wantRetry: "install a verified standalone release newer than v0.11.1, then run it with --offline and its exact --version",
			prepare: func(t *testing.T, fixture *v0111CandidateRoutingFixture) (string, []string) {
				unbindCandidateRegistry(t, fixture.candidate, true)
				return fixture.candidate.root, []string{"--offline", "--version", fixture.candidateVersion}
			},
		},
		{
			name:      "candidate registry not manifest bound",
			wantRetry: "install a verified standalone release newer than v0.11.1, then run it with --offline and its exact --version",
			prepare: func(t *testing.T, fixture *v0111CandidateRoutingFixture) (string, []string) {
				unbindCandidateRegistry(t, fixture.candidate, false)
				return fixture.candidate.root, []string{"--offline", "--version", fixture.candidateVersion}
			},
		},
		{
			name: "target not later than v0.11.1",
			prepare: func(t *testing.T, fixture *v0111CandidateRoutingFixture) (string, []string) {
				candidate := writeVersionedRuntimeCandidate(t, fixture.runtimeRoot,
					"0.11.0-dddddddddddd", "0.11.0",
					"#!/bin/sh\ncase \"${1:-}\" in --version) printf 'yard-engine 0.11.0\\n' ;; *) exit 64 ;; esac\n",
				)
				return candidate.root, []string{"--offline", "--version", "0.11.0"}
			},
		},
		{
			name: "different sole blocker code",
			prepare: func(t *testing.T, fixture *v0111CandidateRoutingFixture) (string, []string) {
				fixture.writeSourceBlocker(t, releasetransition.Blocker{
					Code:     releasetransition.CodeDependencyUnavailable,
					Resource: "activation.unavailable",
					Message:  "the activation state is temporarily unavailable",
					Retry:    "run yard update --check",
				})
				return fixture.candidate.root, []string{
					"--offline", "--version", fixture.candidateVersion,
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newV0111CandidateRoutingFixture(t)
			repositoryRoot, arguments := test.prepare(t, &fixture)
			if test.tamperRoot {
				if err := os.WriteFile(
					filepath.Join(fixture.candidate.root, "bin", "yard-engine"),
					[]byte("#!/bin/sh\nexit 91\n"), 0o700,
				); err != nil {
					t.Fatal(err)
				}
			}
			before := fixture.snapshot(t)
			environment := fixture.environment()
			if test.environmentVersion {
				environment["YARD_RELEASE_VERSION"] = fixture.candidateVersion
			}
			runtime := New(Config{
				RepositoryRoot: repositoryRoot,
				Environment:    environment, Installer: fixture.installer,
				Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
			})
			arguments = append(arguments, "--runtime-root", fixture.runtimeRoot)

			_, err := runtime.PrepareTransition(
				context.Background(), arguments, fixture.configHome, "default", nil,
			)
			if err == nil {
				t.Fatal("arbitrary recovery candidate was accepted")
			}
			if test.wantRetry != "" && !strings.Contains(err.Error(), "next: "+test.wantRetry) {
				t.Fatalf("blocked recovery diagnostic = %v, want retry %q", err, test.wantRetry)
			}
			if after := fixture.snapshot(t); !bytes.Equal(before, after) {
				t.Fatalf("blocked recovery changed protected state:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			if payload, readErr := os.ReadFile(fixture.candidateCalls); readErr == nil &&
				strings.Contains(string(payload), `"mode":"converge"`) {
				t.Fatalf("blocked recovery invoked candidate Converge: %s", payload)
			} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				t.Fatal(readErr)
			}
			if _, statErr := os.Lstat(fixture.publicationCapture); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("blocked recovery invoked publication: %v", statErr)
			}
		})
	}
}

func TestPrepareTransitionUsesExactDirectCandidateForV0111RecoveryPlan(t *testing.T) {
	const recoveryConsequence = "supersede the verified v0.11.1 recovery journal with the standalone candidate plan"
	fixture := newV0111CandidateRoutingFixture(t)
	before := fixture.snapshot(t)
	directRoot := filepath.Join(filepath.Dir(fixture.runtimeRoot), "direct-candidate-root")
	if err := os.Symlink(fixture.candidate.root, directRoot); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	runtime := New(Config{
		RepositoryRoot: directRoot,
		Environment:    fixture.environment(), Installer: fixture.installer,
		Stdout: &stdout, Stderr: &bytes.Buffer{},
	})

	prepared, err := runtime.PrepareTransition(
		context.Background(), []string{
			"--check", "--offline", "--version", fixture.candidateVersion,
			"--runtime-root", fixture.runtimeRoot,
		}, fixture.configHome, "default", nil,
	)
	if err != nil {
		t.Fatalf("prepare exact standalone recovery check: %v", err)
	}
	if prepared.Action != "update.check" || prepared.Changed || prepared.RefreshConfigs {
		t.Fatalf("standalone recovery check preparation = %#v", prepared)
	}
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	var inspection releasetransition.Inspection
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &inspection); err != nil {
		t.Fatalf("decode standalone recovery check output %q: %v", stdout.String(), err)
	}
	if inspection.Plan != releasetransition.PlanToken("plan-v1-"+strings.Repeat("c", 64)) ||
		inspection.Outcome == nil ||
		inspection.Outcome.Target != releasetransition.ReleaseID(fixture.candidateID) ||
		countString(inspection.Assessment.Consequences, recoveryConsequence) != 1 {
		t.Fatalf("standalone recovery check inspection = %#v", inspection)
	}
	if after := fixture.snapshot(t); !bytes.Equal(before, after) {
		t.Fatalf("standalone recovery check changed protected state:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	requests := readCandidateRoutingRequests(t, fixture.candidateCalls)
	if len(requests) != 1 || requests[0].Mode != releasetransition.ProcessInspect ||
		requests[0].Target != releasetransition.ReleaseID(fixture.candidateID) ||
		requests[0].Replacement == nil ||
		requests[0].Replacement.Transaction != releasetransition.TransactionID(fixture.transaction) ||
		requests[0].Replacement.Fingerprint != fixture.journalFingerprint ||
		requests[0].Replacement.Reason != releasetransition.JournalReplacementPostActivationScopeV0111 ||
		requests[0].Replacement.SourceVersion != fixture.sourceVersion {
		t.Fatalf("standalone recovery request = %#v", requests)
	}
}

func TestV0111RecoveryValidatesAugmentedConsequenceBoundaryBeforeConverge(t *testing.T) {
	const recoveryConsequence = "supersede the verified v0.11.1 recovery journal with the standalone candidate plan"
	tests := []struct {
		name                 string
		candidateCount       int
		includeRecovery      bool
		wantPreparationError bool
		wantCount            int
	}{
		{
			name:           "64 generic consequences have no recovery capacity",
			candidateCount: 64, wantPreparationError: true,
		},
		{
			name:           "64 consequences including recovery stay bounded",
			candidateCount: 64, includeRecovery: true, wantCount: 64,
		},
		{
			name:           "63 generic consequences admit recovery",
			candidateCount: 63, wantCount: 64,
		},
	}
	for _, test := range tests {
		for _, check := range []bool{true, false} {
			mode := "interactive"
			if check {
				mode = "check"
			}
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				fixture := newV0111CandidateRoutingFixture(t)
				consequences := make([]string, test.candidateCount)
				for index := range consequences {
					consequences[index] = fmt.Sprintf("candidate consequence %02d", index)
				}
				if test.includeRecovery {
					consequences[31] = recoveryConsequence
				}
				fixture.writeCandidateConsequences(t, consequences)
				before := fixture.snapshot(t)
				var stdout bytes.Buffer
				runtime := New(Config{
					RepositoryRoot: fixture.candidate.root,
					Environment:    fixture.environment(), Installer: fixture.installer,
					Stdout: &stdout, Stderr: &bytes.Buffer{},
				})
				defer runtime.Close()
				arguments := []string{
					"--offline", "--version", fixture.candidateVersion,
					"--runtime-root", fixture.runtimeRoot,
				}
				if check {
					arguments = append(arguments, "--check")
				}
				prepared, err := runtime.PrepareTransition(
					context.Background(), arguments, fixture.configHome, "default", nil,
				)
				if test.wantPreparationError {
					if err == nil {
						t.Fatal("recovery preparation accepted an invalid augmented assessment")
					}
					requests := readCandidateRoutingRequests(t, fixture.candidateCalls)
					if len(requests) != 1 || requests[0].Mode != releasetransition.ProcessInspect {
						t.Fatalf("invalid recovery reached Converge: %#v", requests)
					}
					if after := fixture.snapshot(t); !bytes.Equal(before, after) {
						t.Fatalf("invalid recovery changed protected state:\nbefore:\n%s\nafter:\n%s", before, after)
					}
					if _, statErr := os.Lstat(fixture.publicationCapture); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("invalid recovery invoked publication: %v", statErr)
					}
					return
				}
				if err != nil {
					t.Fatalf("prepare bounded recovery: %v", err)
				}
				if check {
					if err := prepared.Execute(context.Background()); err != nil {
						t.Fatal(err)
					}
					var inspection releasetransition.Inspection
					if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &inspection); err != nil {
						t.Fatal(err)
					}
					if err := inspection.ValidateOutcome(releasetransition.Goal{
						Target:    fixture.candidate.release,
						Direction: releasetransition.DirectionActivateTarget,
					}); err != nil {
						t.Fatalf("augmented check inspection is invalid: %v", err)
					}
					if inspection.Plan != releasetransition.PlanToken("plan-v1-"+strings.Repeat("c", 64)) ||
						len(inspection.Assessment.Consequences) != test.wantCount ||
						countString(inspection.Assessment.Consequences, recoveryConsequence) != 1 {
						t.Fatalf("bounded check inspection = %#v", inspection)
					}
				} else {
					if len(prepared.Consequences) != test.wantCount ||
						countString(prepared.Consequences, recoveryConsequence) != 1 {
						t.Fatalf("bounded interactive preparation = %#v", prepared)
					}
					if err := prepared.Execute(context.Background()); err != nil {
						t.Fatalf("execute bounded recovery: %v", err)
					}
				}
				requests := readCandidateRoutingRequests(t, fixture.candidateCalls)
				wantRequests := 1
				if !check {
					wantRequests = 2
				}
				if len(requests) != wantRequests || requests[0].Mode != releasetransition.ProcessInspect {
					t.Fatalf("bounded recovery requests = %#v", requests)
				}
				if !check && (requests[1].Mode != releasetransition.ProcessConverge ||
					requests[1].Execution == nil ||
					requests[1].Execution.Plan != releasetransition.PlanToken("plan-v1-"+strings.Repeat("c", 64))) {
					t.Fatalf("bounded recovery changed candidate Plan: %#v", requests[1])
				}
			})
		}
	}
}

func TestPreparedV0111RecoveryAddsOneRedactedConsequenceAndConverges(t *testing.T) {
	const recoveryConsequence = "supersede the verified v0.11.1 recovery journal with the standalone candidate plan"
	for _, arguments := range [][]string{nil, {"--yes"}} {
		name := "interactive"
		if len(arguments) != 0 {
			name = "assume yes"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newV0111CandidateRoutingFixture(t)
			runtime := New(Config{
				RepositoryRoot: fixture.candidate.root,
				Environment:    fixture.environment(), Installer: fixture.installer,
				Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
			})
			defer runtime.Close()
			arguments = append(append([]string(nil), arguments...),
				"--offline", "--version", fixture.candidateVersion,
				"--runtime-root", fixture.runtimeRoot,
			)
			prepared, err := runtime.PrepareTransition(
				context.Background(), arguments, fixture.configHome, "default", nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Action != "update.activate" || !prepared.Changed ||
				countString(prepared.Consequences, recoveryConsequence) != 1 ||
				countString(prepared.Consequences,
					"apply the exact typed migration and release activation plan") != 1 {
				t.Fatalf("recovery preparation = %#v", prepared)
			}
			for _, private := range []string{
				fixture.runtimeRoot, fixture.configHome, fixture.transaction, "pid", "fd", "environment",
			} {
				if strings.Contains(strings.ToLower(strings.Join(prepared.Consequences, "\n")), strings.ToLower(private)) {
					t.Fatalf("recovery consequences expose %q: %q", private, prepared.Consequences)
				}
			}
			if err := prepared.Execute(context.Background()); err != nil {
				t.Fatal(err)
			}
			requests := readCandidateRoutingRequests(t, fixture.candidateCalls)
			if len(requests) != 2 || requests[0].Mode != releasetransition.ProcessInspect ||
				requests[1].Mode != releasetransition.ProcessConverge ||
				requests[1].Replacement == nil ||
				*requests[1].Replacement != *requests[0].Replacement {
				t.Fatalf("recovery process sequence = %#v", requests)
			}
		})
	}
}

func TestPreparedV0111RecoveryRevalidatesArtifactsAndJournalBeforeConverge(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, v0111CandidateRoutingFixture)
	}{
		{
			name: "candidate artifact",
			mutate: func(t *testing.T, fixture v0111CandidateRoutingFixture) {
				t.Helper()
				if err := os.WriteFile(
					filepath.Join(fixture.candidate.root, "bin", "yard-engine"),
					[]byte("#!/bin/sh\nexit 91\n"), 0o700,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "source artifact",
			mutate: func(t *testing.T, fixture v0111CandidateRoutingFixture) {
				t.Helper()
				if err := os.WriteFile(
					filepath.Join(fixture.source.root, "bin", "yard-engine"),
					[]byte("#!/bin/sh\nexit 92\n"), 0o700,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "source journal snapshot",
			mutate: func(t *testing.T, fixture v0111CandidateRoutingFixture) {
				t.Helper()
				path := filepath.Join(fixture.configHome, "release-transition", "v2", "journal.json")
				payload, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(payload, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newV0111CandidateRoutingFixture(t)
			runtime := New(Config{
				RepositoryRoot: fixture.candidate.root,
				Environment:    fixture.environment(), Installer: fixture.installer,
				Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
			})
			defer runtime.Close()
			prepared, err := runtime.PrepareTransition(
				context.Background(), []string{
					"--offline", "--version", fixture.candidateVersion,
					"--runtime-root", fixture.runtimeRoot,
				}, fixture.configHome, "default", nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, fixture)
			if err := prepared.Execute(context.Background()); err == nil {
				t.Fatal("recovery execution accepted facts changed after confirmation")
			}
			requests := readCandidateRoutingRequests(t, fixture.candidateCalls)
			if len(requests) != 1 || requests[0].Mode != releasetransition.ProcessInspect {
				t.Fatalf("stale recovery invoked candidate Converge: %#v", requests)
			}
		})
	}
}

func TestV0111ScopeBlockerReportsTruthfulStandaloneRetry(t *testing.T) {
	tests := []struct {
		name           string
		repositoryRoot func(v0111CandidateRoutingFixture) string
		prepare        func(*testing.T, v0111CandidateRoutingFixture)
		wantRetry      string
	}{
		{
			name: "verified candidate available",
			repositoryRoot: func(fixture v0111CandidateRoutingFixture) string {
				return fixture.candidate.root
			},
			wantRetry: "run the verified standalone release with --offline and its exact --version",
		},
		{
			name: "candidate absent",
			repositoryRoot: func(fixture v0111CandidateRoutingFixture) string {
				return fixture.source.root
			},
			wantRetry: "install a verified standalone release newer than v0.11.1, then run it with --offline and its exact --version",
		},
		{
			name: "candidate registry missing",
			repositoryRoot: func(fixture v0111CandidateRoutingFixture) string {
				return fixture.candidate.root
			},
			prepare: func(t *testing.T, fixture v0111CandidateRoutingFixture) {
				unbindCandidateRegistry(t, fixture.candidate, true)
			},
			wantRetry: "install a verified standalone release newer than v0.11.1, then run it with --offline and its exact --version",
		},
		{
			name: "candidate registry not manifest bound",
			repositoryRoot: func(fixture v0111CandidateRoutingFixture) string {
				return fixture.candidate.root
			},
			prepare: func(t *testing.T, fixture v0111CandidateRoutingFixture) {
				unbindCandidateRegistry(t, fixture.candidate, false)
			},
			wantRetry: "install a verified standalone release newer than v0.11.1, then run it with --offline and its exact --version",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newV0111CandidateRoutingFixture(t)
			if test.prepare != nil {
				test.prepare(t, fixture)
			}
			var stdout bytes.Buffer
			runtime := New(Config{
				RepositoryRoot: test.repositoryRoot(fixture),
				Environment:    fixture.environment(), Installer: fixture.installer,
				Stdout: &stdout, Stderr: &bytes.Buffer{},
			})
			defer runtime.Close()
			prepared, err := runtime.PrepareTransition(
				context.Background(), []string{
					"--check", "--runtime-root", fixture.runtimeRoot,
				}, fixture.configHome, "default", nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := prepared.Execute(context.Background()); err != nil {
				t.Fatal(err)
			}
			var inspection releasetransition.Inspection
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &inspection); err != nil ||
				inspection.Outcome == nil || inspection.Outcome.Retry != test.wantRetry ||
				len(inspection.Blockers) != 1 || inspection.Blockers[0].Retry != test.wantRetry {
				t.Fatalf("scope retry inspection=%#v output=%q err=%v", inspection, stdout.String(), err)
			}
			for _, private := range []string{
				fixture.runtimeRoot, fixture.configHome, fixture.candidate.root,
				"pid", " fd ", "environment",
			} {
				if strings.Contains(strings.ToLower(stdout.String()), strings.ToLower(private)) {
					t.Fatalf("scope retry exposes %q: %q", private, stdout.String())
				}
			}
			if strings.Contains(inspection.Outcome.Retry, "yard update --check") {
				t.Fatalf("scope retry makes a false check promise: %q", inspection.Outcome.Retry)
			}
			_, err = runtime.PrepareTransition(
				context.Background(), []string{
					"--version", fixture.candidateVersion,
					"--runtime-root", fixture.runtimeRoot,
				}, fixture.configHome, "default", nil,
			)
			if err == nil || !strings.Contains(err.Error(), "next: "+test.wantRetry) ||
				strings.Contains(err.Error(), "yard update --check") {
				t.Fatalf("scope retry error = %v", err)
			}
		})
	}
}

func TestPrepareTransitionReportsProtectedInspectionFailuresAsPublicOutcomes(t *testing.T) {
	for _, test := range []struct {
		name                string
		tamper              func(*testing.T, protectedRuntimeTransitionFixture)
		code                releasetransition.OutcomeCode
		target, transaction string
		retry               string
	}{
		{
			name: "corrupt journal",
			tamper: func(t *testing.T, fixture protectedRuntimeTransitionFixture) {
				t.Helper()
				if err := os.WriteFile(fixture.journalPath, []byte("{\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			code: releasetransition.CodeJournalInvalid, target: "unknown",
			retry: "restore protected release metadata from backup, then run yard update --check",
		},
		{
			name: "journal selected candidate unavailable",
			tamper: func(t *testing.T, fixture protectedRuntimeTransitionFixture) {
				t.Helper()
				if err := os.Remove(fixture.engine); err != nil {
					t.Fatal(err)
				}
			},
			code:   releasetransition.CodeDependencyUnavailable,
			target: "1.2.3-aaaaaaaaaaaa", transaction: "tx-0123456789abcdef",
			retry: "restore the journal-selected release, then run yard update --check",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
			test.tamper(t, fixture)
			var stdout bytes.Buffer
			runtime := New(Config{
				Environment: fixture.environment(), Installer: fixture.installer,
				Stdout: &stdout, Stderr: &bytes.Buffer{},
			})
			prepared, err := runtime.PrepareTransition(
				context.Background(), []string{"--check", "--runtime-root", fixture.runtimeRoot},
				fixture.configHome, "default", nil,
			)
			if err != nil {
				t.Fatalf("prepare public check: %v", err)
			}
			if prepared.Action != "update.check" || prepared.Changed {
				t.Fatalf("public check preparation = %#v", prepared)
			}
			if err := prepared.Execute(context.Background()); err != nil {
				t.Fatal(err)
			}
			var response struct {
				Outcome *releasetransition.Outcome `json:"outcome"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &response); err != nil ||
				response.Outcome == nil ||
				response.Outcome.Status != releasetransition.StatusOperatorActionRequired ||
				response.Outcome.Code != test.code || string(response.Outcome.Target) != test.target ||
				response.Outcome.Retry != test.retry ||
				(response.Outcome.Transaction == nil) != (test.transaction == "") ||
				(response.Outcome.Transaction != nil && string(*response.Outcome.Transaction) != test.transaction) ||
				strings.Contains(stdout.String(), fixture.runtimeRoot) ||
				strings.Contains(stdout.String(), fixture.configHome) {
				t.Fatalf("public check outcome=%#v output=%q err=%v", response.Outcome, stdout.String(), err)
			}

			_, err = runtime.PrepareTransition(
				context.Background(), []string{"--runtime-root", fixture.runtimeRoot},
				fixture.configHome, "default", nil,
			)
			if err == nil || !strings.Contains(err.Error(),
				"release transition operator-action-required: code="+string(test.code)) ||
				!strings.Contains(err.Error(), "target="+test.target) ||
				!strings.Contains(err.Error(), "next: "+test.retry) ||
				strings.Contains(err.Error(), fixture.runtimeRoot) ||
				strings.Contains(err.Error(), fixture.configHome) {
				t.Fatalf("public mutation error = %v", err)
			}
		})
	}
}

func TestPrepareTransitionRejectsDifferentTargetBeforePublication(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
	runtime := New(Config{
		Environment: fixture.environment(), Installer: fixture.installer,
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})

	_, err := runtime.PrepareTransition(
		context.Background(), []string{
			"--version", "9.9.9", "--runtime-root", fixture.runtimeRoot,
		},
		fixture.configHome, "default", nil,
	)
	if err == nil || !strings.Contains(err.Error(), "exact") {
		t.Fatalf("different-target update error = %v", err)
	}
	if _, err := os.Lstat(fixture.publicationCapture); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("different-target update invoked publication: %v", err)
	}
	if _, err := os.Lstat(fixture.cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("different-target update created the release cache: %v", err)
	}
}

func TestMutationGateVerifiesJournalSelectedEngineBeforeExecution(t *testing.T) {
	for _, test := range []struct {
		name   string
		tamper func(*testing.T, protectedRuntimeTransitionFixture, string)
	}{
		{
			name: "modified engine",
			tamper: func(t *testing.T, fixture protectedRuntimeTransitionFixture, payload string) {
				t.Helper()
				if err := os.WriteFile(fixture.engine, []byte(payload), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlinked engine",
			tamper: func(t *testing.T, fixture protectedRuntimeTransitionFixture, payload string) {
				t.Helper()
				external := filepath.Join(filepath.Dir(fixture.runtimeRoot), "replacement-engine")
				if err := os.WriteFile(external, []byte(payload), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(fixture.engine); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, fixture.engine); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "non-executable engine",
			tamper: func(t *testing.T, fixture protectedRuntimeTransitionFixture, payload string) {
				t.Helper()
				if err := os.WriteFile(fixture.engine, []byte(payload), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(fixture.engine, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "modified engine and replaced manifest",
			tamper: func(t *testing.T, fixture protectedRuntimeTransitionFixture, payload string) {
				t.Helper()
				if err := os.WriteFile(fixture.engine, []byte(payload), 0o700); err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256([]byte(payload))
				manifest := fmt.Sprintf("%x  ./bin/yard-engine\n", digest)
				if err := os.WriteFile(
					filepath.Join(filepath.Dir(filepath.Dir(fixture.engine)), "runtime-files.sha256"),
					[]byte(manifest), 0o600,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
			capture := filepath.Join(filepath.Dir(fixture.runtimeRoot), "unverified-engine-called")
			payload := fmt.Sprintf("#!/bin/sh\nprintf called > %q\ncat >/dev/null\nprintf '%%s\\n' %q\n",
				capture, fixture.recoveringResponse())
			test.tamper(t, fixture, payload)
			runtime := New(Config{
				Environment: fixture.environment(), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
			})

			outcome, err := runtime.InspectMutationGate(
				context.Background(), fixture.runtimeRoot, fixture.configHome, "default", nil,
			)
			if err != nil || outcome == nil ||
				outcome.Status != releasetransition.StatusOperatorActionRequired ||
				outcome.Code != releasetransition.CodeDependencyUnavailable ||
				outcome.Active != "release-a" || outcome.Target != releasetransition.ReleaseID(fixture.target) ||
				outcome.Transaction == nil || string(*outcome.Transaction) != fixture.transaction ||
				outcome.Retry != "restore the journal-selected release, then run yard update --check" {
				t.Fatalf("unverified candidate gate outcome=%#v err=%v", outcome, err)
			}
			if _, err := os.Lstat(capture); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unverified candidate engine executed: %v", err)
			}
		})
	}
}

func TestMutationGateExecutesTheVerifiedEngineAcrossPathReplacement(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
	started := filepath.Join(filepath.Dir(fixture.runtimeRoot), "verified-engine-started")
	proceed := filepath.Join(filepath.Dir(fixture.runtimeRoot), "verified-engine-proceed")
	replacementCalled := filepath.Join(filepath.Dir(fixture.runtimeRoot), "replacement-engine-called")
	original := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version)
    : > %q
    while [ ! -e %q ]; do sleep 0.01; done
    printf 'yard-engine 1.2.3\n'
    ;;
  _release-transition) cat >/dev/null; printf '%%s\n' %q ;;
  *) exit 64 ;;
esac
`, started, proceed, fixture.recoveringResponse())
	writeProtectedRuntimeFixtureEngine(t, fixture, original)
	replacement := filepath.Join(filepath.Dir(fixture.engine), "replacement-yard-engine")
	replacementPayload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition)
    : > %q
    cat >/dev/null
    printf '%%s\n' %q
    ;;
  *) exit 64 ;;
esac
`, replacementCalled, fixture.recoveringResponse())
	if err := os.WriteFile(replacement, []byte(replacementPayload), 0o700); err != nil {
		t.Fatal(err)
	}
	runtime := New(Config{
		Environment: fixture.environment(), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	type gateResult struct {
		outcome *releasetransition.Outcome
		err     error
	}
	result := make(chan gateResult, 1)
	go func() {
		outcome, err := runtime.InspectMutationGate(
			context.Background(), fixture.runtimeRoot, fixture.configHome, "default", nil,
		)
		result <- gateResult{outcome: outcome, err: err}
	}()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-deadline:
			t.Fatal("verified engine did not reach its version boundary")
		case <-time.After(time.Millisecond):
		}
	}
	if err := os.Rename(replacement, fixture.engine); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proceed, []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case observed := <-result:
		if observed.err != nil || observed.outcome == nil ||
			observed.outcome.Status != releasetransition.StatusRecovering {
			cause := observed.err
			for cause != nil && errors.Unwrap(cause) != nil {
				cause = errors.Unwrap(cause)
			}
			t.Fatalf("replacement race outcome=%#v err=%v cause=%v",
				observed.outcome, observed.err, cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mutation gate did not finish after releasing the verified engine")
	}
	if _, err := os.Stat(replacementCalled); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement engine executed after verification: %v", err)
	}
}

func TestCandidateTransitionRejectsContradictoryInspectionOutcome(t *testing.T) {
	root := t.TempDir()
	response := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata"],"recovery":"reversible"},"blockers":[{"code":"migration-stale","resource":"yard.fixture","message":"the resource changed","retry":"run yard update --check"}],"outcome":{"status":"ready","reachedGoal":true,"active":"release-b","target":"release-b","code":"ready","message":"verified"}}}`
	candidate := writeRuntimeCandidateFixture(t, root, response)
	request := releasetransition.ProcessRequest{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Mode:          releasetransition.ProcessInspect, RuntimeRoot: root, ConfigHome: root,
		Target: "release-b", Direction: releasetransition.DirectionActivateTarget,
	}
	for _, check := range []bool{true, false} {
		runtime := New(Config{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
		_, err := prepareCandidateTransitionForTest(
			runtime, context.Background(), options{check: check, root: root}, candidate, request,
		)
		if err == nil || !strings.Contains(err.Error(), "inconsistent") {
			t.Fatalf("contradictory candidate check=%v error=%v", check, err)
		}
	}
}

func TestCandidateTransitionRejectsSemanticallyInvalidReadyOutcome(t *testing.T) {
	root := t.TempDir()
	inspection := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the exact typed migration and release activation plan"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"release-a","target":"release-b","code":"transition-required","message":"the release transition has not started","retry":"run yard update"}}}`
	invalid := `{"schemaVersion":1,"outcome":{"status":"ready","reachedGoal":true,"active":"release-a","target":"release-b","code":"ready","message":"verified","transaction":"tx-0123456789abcdef"}}`
	payload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition)
    request=$(cat)
    case "$request" in
      *'"mode":"inspect"'*) printf '%%s\n' %q ;;
      *'"mode":"converge"'*) printf '%%s\n' %q ;;
      *) exit 64 ;;
    esac
    ;;
  *) exit 64 ;;
esac
`, inspection, invalid)
	candidate := writeRuntimeCandidatePayload(t, root, payload)
	runtime := New(Config{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	prepared, err := prepareCandidateTransitionForTest(runtime,
		context.Background(), options{root: root}, candidate,
		releasetransition.ProcessRequest{
			SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
			Mode:          releasetransition.ProcessInspect, RuntimeRoot: root, ConfigHome: root,
			Target: "release-b", Direction: releasetransition.DirectionActivateTarget,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Execute(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "inconsistent release transition outcome") {
		t.Fatalf("invalid ready convergence error = %v", err)
	}
}

func TestCandidateTransitionPrintsValidatedReadyWarnings(t *testing.T) {
	root := t.TempDir()
	inspection := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the exact typed migration and release activation plan"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"release-a","target":"release-b","code":"transition-required","message":"the release transition has not started","retry":"run yard update"}}}`
	ready := `{"schemaVersion":1,"outcome":{"status":"ready","reachedGoal":true,"active":"release-b","previous":"release-a","target":"release-b","code":"ready","message":"verified","transaction":"tx-0123456789abcdef","warnings":["recovery cleanup is pending"]}}`
	payload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition)
    request=$(cat)
    case "$request" in
      *'"mode":"inspect"'*) printf '%%s\n' %q ;;
      *'"mode":"converge"'*) printf '%%s\n' %q ;;
      *) exit 64 ;;
    esac
    ;;
  *) exit 64 ;;
esac
`, inspection, ready)
	candidate := writeRuntimeCandidatePayload(t, root, payload)
	var stderr bytes.Buffer
	runtime := New(Config{Stdout: &bytes.Buffer{}, Stderr: &stderr})
	prepared, err := prepareCandidateTransitionForTest(runtime,
		context.Background(), options{root: root}, candidate,
		releasetransition.ProcessRequest{
			SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
			Mode:          releasetransition.ProcessInspect, RuntimeRoot: root, ConfigHome: root,
			Target: "release-b", Direction: releasetransition.DirectionActivateTarget,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := stderr.String(); got != "warning: release transition: recovery cleanup is pending\n" {
		t.Fatalf("ready warnings = %q", got)
	}
}

func TestCandidateTransitionReinspectsPlanStaleOutcomeBeforeReturning(t *testing.T) {
	initial := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the exact typed migration and release activation plan"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"release-a","target":"release-b","code":"transition-required","message":"the release transition has not started","retry":"run yard update"}}}`
	stale := `{"schemaVersion":1,"outcome":{"status":"operator-action-required","reachedGoal":false,"active":"release-b","previous":"release-a","target":"release-b","code":"plan-stale","message":"the inspected release transition changed before convergence","retry":"run yard update --check"}}`
	ready := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","assessment":{"action":"release.transition.v2","effect":"mutation","changed":false,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible"},"outcome":{"status":"ready","reachedGoal":true,"active":"release-b","previous":"release-a","target":"release-b","code":"ready","message":"verified"}}}`
	wrongReady := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","assessment":{"action":"release.transition.v2","effect":"mutation","changed":false,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible"},"outcome":{"status":"ready","reachedGoal":true,"active":"release-a","target":"release-b","code":"ready","message":"verified"}}}`
	pending := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the exact typed migration and release activation plan"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"release-a","target":"release-b","code":"transition-required","message":"the release transition is still pending","retry":"run yard update"}}}`

	for _, test := range []struct {
		name         string
		reinspection string
		wantError    string
	}{
		{name: "externally reached fixed point", reinspection: ready},
		{name: "state is still pending", reinspection: pending, wantError: "code=transition-required"},
		{name: "false ready active release", reinspection: wrongReady, wantError: "inconsistent release reinspection"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			settled := filepath.Join(t.TempDir(), "settled")
			payload := fmt.Sprintf(`#!/bin/sh
marker=%q
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition)
    request=$(cat)
    case "$request" in
      *'"mode":"inspect"'*)
        if [ -e "$marker" ]; then printf '%%s\n' %q; else printf '%%s\n' %q; fi
        ;;
      *'"mode":"converge"'*)
        : > "$marker"
        printf '%%s\n' %q
        ;;
      *) exit 64 ;;
    esac
    ;;
  *) exit 64 ;;
esac
`, settled, test.reinspection, initial, stale)
			candidate := writeRuntimeCandidatePayload(t, root, payload)
			runtime := New(Config{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
			prepared, err := prepareCandidateTransitionForTest(runtime,
				context.Background(), options{root: root}, candidate,
				releasetransition.ProcessRequest{
					SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
					Mode:          releasetransition.ProcessInspect,
					RuntimeRoot:   root,
					ConfigHome:    root,
					Target:        "release-b",
					Direction:     releasetransition.DirectionActivateTarget,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			err = prepared.Execute(context.Background())
			if test.wantError == "" && err != nil {
				t.Fatalf("externally settled transition error = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("pending transition error = %v", err)
			}
		})
	}
}

func TestCandidateTransitionValidatesNonReadyOutcomeAtProcessBoundary(t *testing.T) {
	execute := func(t *testing.T, inspection, convergence string) error {
		t.Helper()
		root := t.TempDir()
		payload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition)
    request=$(cat)
    case "$request" in
      *'"mode":"inspect"'*) printf '%%s\n' %q ;;
      *'"mode":"converge"'*) printf '%%s\n' %q ;;
      *) exit 64 ;;
    esac
    ;;
  *) exit 64 ;;
esac
`, inspection, convergence)
		candidate := writeRuntimeCandidatePayload(t, root, payload)
		runtime := New(Config{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
		prepared, err := prepareCandidateTransitionForTest(runtime,
			context.Background(), options{root: root}, candidate,
			releasetransition.ProcessRequest{
				SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
				Mode:          releasetransition.ProcessInspect, RuntimeRoot: root, ConfigHome: root,
				Target: "release-b", Direction: releasetransition.DirectionActivateTarget,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		return prepared.Execute(context.Background())
	}

	t.Run("unknown post-mutation links remain structured", func(t *testing.T) {
		inspection := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the exact typed migration and release activation plan"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"release-a","target":"release-b","code":"transition-required","message":"the release transition has not started","retry":"run yard update"}}}`
		convergence := `{"schemaVersion":1,"outcome":{"status":"operator-action-required","reachedGoal":false,"active":"","target":"release-b","code":"recovery-ambiguous","message":"release facts cannot be observed","retry":"run yard update --check","transaction":"tx-0123456789abcdef"}}`
		err := execute(t, inspection, convergence)
		if err == nil || !strings.Contains(err.Error(),
			"release transition operator-action-required: code=recovery-ambiguous active=") ||
			strings.Contains(err.Error(), "inconsistent release transition outcome") {
			t.Fatalf("unknown-link convergence error = %v", err)
		}
	})

	t.Run("foreign resume transaction is rejected", func(t *testing.T) {
		inspection := `{"schemaVersion":1,"inspection":{"plan":"resume-v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["resume the protected transition"]},"resume":"tx-0123456789abcdef","outcome":{"status":"recovering","reachedGoal":false,"active":"release-a","target":"release-b","code":"recovery-pending","message":"the transition can resume","retry":"run yard update","transaction":"tx-0123456789abcdef"}}}`
		convergence := `{"schemaVersion":1,"outcome":{"status":"recovering","reachedGoal":false,"active":"release-a","target":"release-b","code":"recovery-pending","message":"the transition can resume","retry":"run yard update","transaction":"tx-fedcba9876543210"}}`
		err := execute(t, inspection, convergence)
		if err == nil || !strings.Contains(err.Error(), "inconsistent release transition outcome") {
			t.Fatalf("foreign recovery convergence error = %v", err)
		}
	})

	for _, test := range []struct {
		name        string
		convergence string
		code        string
	}{
		{
			name:        "interrupted staged checkpoint remains structured",
			convergence: `{"schemaVersion":1,"outcome":{"status":"recovering","reachedGoal":false,"active":"release-a","previous":"release-b","target":"release-b","code":"verification-failed","message":"the release transition was interrupted after a durable mutation checkpoint","retry":"run yard update","transaction":"tx-0123456789abcdef"}}`,
			code:        "verification-failed",
		},
		{
			name:        "retryable dependency remains structured",
			convergence: `{"schemaVersion":1,"outcome":{"status":"recovering","reachedGoal":false,"active":"release-a","target":"release-b","code":"dependency-unavailable","message":"activation reconciliation is temporarily unavailable","retry":"run yard update","transaction":"tx-0123456789abcdef"}}`,
			code:        "dependency-unavailable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspection := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the exact typed migration and release activation plan"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"release-a","target":"release-b","code":"transition-required","message":"the release transition has not started","retry":"run yard update"}}}`
			err := execute(t, inspection, test.convergence)
			if err == nil || !strings.Contains(err.Error(), "code="+test.code) ||
				strings.Contains(err.Error(), "inconsistent release transition outcome") {
				t.Fatalf("intermediate convergence error = %v", err)
			}
		})
	}
}

func TestMutationGateInspectsSemanticallyUnsafeCompleteJournal(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalComplete)
	runtime := New(Config{
		Environment: fixture.environment(), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})

	outcome, err := runtime.InspectMutationGate(
		context.Background(), fixture.runtimeRoot, fixture.configHome, "default", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.Status != releasetransition.StatusOperatorActionRequired ||
		outcome.Code != releasetransition.CodePlanStale {
		t.Fatalf("unsafe complete journal gate outcome = %#v", outcome)
	}
}

func TestMutationGateRestoresSourceIngressFromTheProtectedJournal(t *testing.T) {
	fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
	payload, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(payload)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Dir(fixture.runtimeRoot)
	descriptor := releasetransition.SourceIngressRequest{
		SchemaVersion: releasetransition.SourceIngressRequestSchemaV1,
		Kind:          releasetransition.SourceIngressPreGoV1,
		SourceRoot:    filepath.Join(home, "source"),
		DataHome:      filepath.Join(home, "data"),
		BinDir:        filepath.Join(home, "bin"),
		RC:            filepath.Join(home, ".bashrc"),
		LoginRC:       filepath.Join(home, ".profile"),
	}
	journal.SourceIngress = &descriptor
	encoded, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.journalPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(home, "source-ingress-request.json")
	engine := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition) cat > %q; printf '%%s\n' %q ;;
  *) exit 64 ;;
esac
`, capture, fixture.recoveringResponse())
	writeProtectedRuntimeFixtureEngine(t, fixture, engine)

	runtime := New(Config{
		Environment: fixture.environment(), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	if _, err := runtime.InspectMutationGate(
		context.Background(), fixture.runtimeRoot, fixture.configHome, "default", nil,
	); err != nil {
		t.Fatal(err)
	}
	requestPayload, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var request releasetransition.ProcessRequest
	if err := json.Unmarshal(requestPayload, &request); err != nil {
		t.Fatal(err)
	}
	if request.SourceIngress == nil || *request.SourceIngress != descriptor {
		t.Fatalf("restored source ingress = %#v", request.SourceIngress)
	}
}

type countingErrorTransport struct{ calls int }

func (transport *countingErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.calls++
	return nil, errors.New("unexpected network request")
}

type protectedRuntimeTransitionFixture struct {
	runtimeRoot, configHome, cache, installer, publicationCapture string
	target, transaction, resumePlan                               string
	artifactDigest                                                string
	journalPath, engine                                           string
}

func newProtectedRuntimeTransitionFixture(
	t *testing.T,
	checkpoint releasetransition.JournalCheckpoint,
) protectedRuntimeTransitionFixture {
	t.Helper()
	root := t.TempDir()
	fixture := protectedRuntimeTransitionFixture{
		runtimeRoot: filepath.Join(root, "runtime"), configHome: filepath.Join(root, "config"),
		cache: filepath.Join(root, "cache"), installer: filepath.Join(root, "installer"),
		publicationCapture: filepath.Join(root, "publication-called"),
		target:             "1.2.3-aaaaaaaaaaaa", transaction: "tx-0123456789abcdef",
		resumePlan: "resume-v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	for _, directory := range []string{
		fixture.runtimeRoot, filepath.Join(fixture.runtimeRoot, "releases", "release-a"),
		filepath.Join(fixture.runtimeRoot, "releases", fixture.target, "bin"), fixture.configHome,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	current := "release-a"
	if checkpoint == releasetransition.JournalComplete {
		current = fixture.target
	}
	if err := os.Symlink(filepath.Join("releases", current), filepath.Join(fixture.runtimeRoot, "current")); err != nil {
		t.Fatal(err)
	}
	if checkpoint == releasetransition.JournalComplete {
		if err := os.Symlink("releases/release-a", filepath.Join(fixture.runtimeRoot, "previous")); err != nil {
			t.Fatal(err)
		}
	}
	fixture.engine = filepath.Join(fixture.runtimeRoot, "releases", fixture.target, "bin", "yard-engine")
	response := fixture.recoveringResponse()
	if checkpoint == releasetransition.JournalComplete {
		response = fixture.unsafeCompleteResponse()
	}
	engine := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition) cat >/dev/null; printf '%%s\n' %q ;;
  *) exit 64 ;;
esac
`, response)
	if err := os.WriteFile(fixture.engine, []byte(engine), 0o700); err != nil {
		t.Fatal(err)
	}
	registry := []byte("{}\n")
	registryPath := filepath.Join(
		fixture.runtimeRoot, "releases", fixture.target, "config", "release-transition.json",
	)
	if err := os.MkdirAll(filepath.Dir(registryPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, registry, 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(fixture.engine)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	registryDigest := sha256.Sum256(registry)
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n",
		digest, registryDigest)
	manifestDigest := sha256.Sum256([]byte(manifest))
	fixture.artifactDigest = fmt.Sprintf("%x", manifestDigest)
	if err := os.WriteFile(
		filepath.Join(fixture.runtimeRoot, "releases", fixture.target, "runtime-files.sha256"),
		[]byte(manifest), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.installer, []byte(fmt.Sprintf(
		"#!/bin/sh\n# --publish-only\nprintf called > %q\nexit 91\n", fixture.publicationCapture,
	)), 0o700); err != nil {
		t.Fatal(err)
	}

	authorizationPlan := releasetransition.PlanToken("plan-v1-" + strings.Repeat("a", 64))
	resumePlan := releasetransition.PlanToken(fixture.resumePlan)
	observationScope := releasetransition.Fingerprint(strings.Repeat("d", 64))
	intentPayload, err := json.Marshal(struct {
		AuthorizationPlan releasetransition.PlanToken     `json:"authorizationPlan"`
		ResumePlan        releasetransition.PlanToken     `json:"resumePlan"`
		ObservationScope  releasetransition.Fingerprint   `json:"observationScope"`
		Steps             []releasetransition.JournalStep `json:"steps"`
	}{authorizationPlan, resumePlan, observationScope, []releasetransition.JournalStep{}})
	if err != nil {
		t.Fatal(err)
	}
	intentDigest := sha256.Sum256(intentPayload)
	journal := releasetransition.JournalRecord{
		SchemaVersion: releasetransition.JournalSchemaV2,
		Transaction:   releasetransition.TransactionID(fixture.transaction),
		Goal: releasetransition.Goal{
			Target:    releasetransition.ReleaseID(fixture.target),
			Direction: releasetransition.DirectionActivateTarget,
		},
		Releases: releasetransition.ReleasePair{
			From: "release-a", Target: releasetransition.ReleaseID(fixture.target),
		},
		AuthorizationPlan: authorizationPlan, ResumePlan: resumePlan,
		ArtifactDigest:      releasetransition.Fingerprint(fixture.artifactDigest),
		RegistryDigest:      releasetransition.Fingerprint(fmt.Sprintf("%x", registryDigest)),
		CatalogDigest:       releasetransition.Fingerprint(strings.Repeat("c", 64)),
		ObservationScope:    observationScope,
		AuthorizationDigest: releasetransition.Fingerprint(strings.Repeat("e", 64)),
		IntentDigest:        releasetransition.Fingerprint(fmt.Sprintf("%x", intentDigest)),
		Checkpoint:          checkpoint, Steps: []releasetransition.JournalStep{},
	}
	journalPayload, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	fixture.journalPath = filepath.Join(fixture.configHome, "release-transition", "v2", "journal.json")
	if err := os.MkdirAll(filepath.Dir(fixture.journalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.journalPath, journalPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture protectedRuntimeTransitionFixture) environment() map[string]string {
	return map[string]string{
		"HOME": filepath.Dir(fixture.runtimeRoot), "YARD_RELEASE_CACHE": fixture.cache,
		"YARD_RELEASE_BASE_URL": "file://" + filepath.Join(filepath.Dir(fixture.runtimeRoot), "missing-assets"),
	}
}

func (fixture protectedRuntimeTransitionFixture) recoveringResponse() string {
	return fmt.Sprintf(
		`{"schemaVersion":1,"inspection":{"plan":%q,"assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["resume the protected transition"]},"resume":%q,"outcome":{"status":"recovering","reachedGoal":false,"active":"release-a","target":%q,"code":"recovery-pending","message":"the authorized release transition can resume from observed facts","retry":"run yard update","transaction":%q}}}`,
		fixture.resumePlan, fixture.transaction, fixture.target, fixture.transaction,
	)
}

func (fixture protectedRuntimeTransitionFixture) unsafeCompleteResponse() string {
	return fmt.Sprintf(
		`{"schemaVersion":1,"inspection":{"plan":"plan-v1-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","assessment":{"action":"release.transition.v2","effect":"mutation","changed":false,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible"},"outcome":{"status":"operator-action-required","reachedGoal":false,"active":%q,"previous":"release-a","target":%q,"code":"plan-stale","message":"the complete journal bindings do not match the verified release","retry":"run yard update --check","transaction":%q}}}`,
		fixture.target, fixture.target, fixture.transaction,
	)
}

func writeProtectedRuntimeFixtureEngine(
	t *testing.T,
	fixture protectedRuntimeTransitionFixture,
	payload string,
) {
	t.Helper()
	if err := os.WriteFile(fixture.engine, []byte(payload), 0o700); err != nil {
		t.Fatal(err)
	}
	engineDigest := sha256.Sum256([]byte(payload))
	registry, err := os.ReadFile(filepath.Join(
		filepath.Dir(filepath.Dir(fixture.engine)), "config", "release-transition.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	registryDigest := sha256.Sum256(registry)
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n",
		engineDigest, registryDigest)
	if err := os.WriteFile(
		filepath.Join(filepath.Dir(filepath.Dir(fixture.engine)), "runtime-files.sha256"),
		[]byte(manifest), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256([]byte(manifest))
	snapshot, err := os.ReadFile(fixture.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	journal.ArtifactDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", manifestDigest))
	encoded, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.journalPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeRuntimeCandidateFixture(
	t *testing.T,
	root string,
	transitionResponse string,
) publishedCandidate {
	t.Helper()
	payload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine 1.2.3\n' ;;
  _release-transition) cat >/dev/null; printf '%%s\n' %q ;;
  *) exit 64 ;;
esac
`, transitionResponse)
	return writeRuntimeCandidatePayload(t, root, payload)
}

func prepareCandidateTransitionForTest(
	runtime *Runtime,
	ctx context.Context,
	parsed options,
	candidate publishedCandidate,
	request releasetransition.ProcessRequest,
) (Prepared, error) {
	var expected *releasetransition.Fingerprint
	if request.ArtifactDigest != "" {
		expected = &request.ArtifactDigest
	}
	verified, err := runtime.verifyPublishedCandidate(ctx, candidate, request.RuntimeRoot, expected)
	if err != nil {
		return Prepared{}, err
	}
	defer verified.Close()
	if request.ArtifactDigest == "" {
		request.ArtifactDigest = verified.manifestDigest
	}
	return runtime.prepareVerifiedCandidateTransition(ctx, parsed, verified, request)
}

func writeRuntimeCandidatePayload(
	t *testing.T,
	root string,
	payload string,
) publishedCandidate {
	t.Helper()
	candidateRoot := filepath.Join(root, "releases", "release-b")
	if err := os.MkdirAll(filepath.Join(candidateRoot, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := validateReleaseRoot(candidateRoot, "candidate fixture"); err != nil {
		t.Fatalf("candidate fixture root: %v", err)
	}
	engine := filepath.Join(candidateRoot, "bin", "yard-engine")
	if err := os.WriteFile(engine, []byte(payload), 0o700); err != nil {
		t.Fatal(err)
	}
	registry := []byte("{}\n")
	registryPath := filepath.Join(candidateRoot, "config", "release-transition.json")
	if err := os.MkdirAll(filepath.Dir(registryPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, registry, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(payload))
	registryDigest := sha256.Sum256(registry)
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n",
		digest, registryDigest)
	if err := os.WriteFile(filepath.Join(candidateRoot, "runtime-files.sha256"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return publishedCandidate{release: "release-b", root: candidateRoot}
}

type v0111CandidateRoutingFixture struct {
	runtimeRoot, configHome, installer, publicationCapture string
	source, candidate                                      publishedCandidate
	sourceID, candidateID                                  string
	sourceVersion, candidateVersion                        string
	transaction                                            string
	journalFingerprint                                     releasetransition.Fingerprint
	candidateCalls                                         string
}

func newV0111CandidateRoutingFixture(t *testing.T) v0111CandidateRoutingFixture {
	t.Helper()
	root := t.TempDir()
	fixture := v0111CandidateRoutingFixture{
		runtimeRoot:        filepath.Join(root, "runtime"),
		configHome:         filepath.Join(root, "config"),
		installer:          filepath.Join(root, "installer"),
		publicationCapture: filepath.Join(root, "publication-called"),
		sourceID:           "0.11.1-aaaaaaaaaaaa",
		candidateID:        "0.11.2-bbbbbbbbbbbb",
		sourceVersion:      "0.11.1",
		candidateVersion:   "0.11.2",
		transaction:        "tx-source-v0111",
		candidateCalls:     filepath.Join(root, "candidate-requests.jsonl"),
	}
	if err := os.MkdirAll(filepath.Join(fixture.runtimeRoot, "releases", "0.9.1-old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		filepath.Join("releases", fixture.sourceID), filepath.Join(fixture.runtimeRoot, "current"),
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/0.9.1-old", filepath.Join(fixture.runtimeRoot, "previous")); err != nil {
		t.Fatal(err)
	}

	resumePlan := "resume-v1-" + strings.Repeat("b", 64)
	blockerMessage := "the authorized transition observation scope differs from this engine"
	blockerRetry := "restore or repair the verified source release before retrying"
	sourceResponse := fmt.Sprintf(
		`{"schemaVersion":1,"inspection":{"plan":%q,"assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["resume the protected transition"]},"blockers":[{"code":"plan-stale","resource":"transition.observation-scope","message":%q,"retry":%q}],"resume":%q,"outcome":{"status":"operator-action-required","reachedGoal":false,"active":%q,"previous":"0.9.1-old","target":%q,"code":"plan-stale","message":%q,"retry":%q,"transaction":%q}}}`,
		resumePlan, blockerMessage, blockerRetry, fixture.transaction,
		fixture.sourceID, fixture.sourceID, blockerMessage, blockerRetry, fixture.transaction,
	)
	sourcePayload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine %s\n' ;;
  _release-transition) cat >/dev/null; printf '%%s\n' %q ;;
  *) exit 64 ;;
esac
`, fixture.sourceVersion, sourceResponse)
	fixture.source = writeVersionedRuntimeCandidate(
		t, fixture.runtimeRoot, fixture.sourceID, fixture.sourceVersion, sourcePayload,
	)

	inspection := fmt.Sprintf(
		`{"schemaVersion":1,"inspection":{"plan":"plan-v1-%s","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the exact typed migration and release activation plan"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":%q,"previous":"0.9.1-old","target":%q,"code":"transition-required","message":"the inspected release transition requires confirmation","retry":"confirm the inspected update plan"}}}`,
		strings.Repeat("c", 64), fixture.sourceID, fixture.candidateID,
	)
	converged := fmt.Sprintf(
		`{"schemaVersion":1,"outcome":{"status":"ready","reachedGoal":true,"active":%q,"previous":%q,"target":%q,"code":"ready","message":"the release transition reached the requested goal","transaction":"tx-recovery-v0112"}}`,
		fixture.candidateID, fixture.sourceID, fixture.candidateID,
	)
	candidatePayload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine %s\n' ;;
  _release-transition)
    request="$(cat)"
    printf '%%s\n' "$request" >> %q
    case "$request" in
      *'"mode":"converge"'*) printf '%%s\n' %q ;;
      *) printf '%%s\n' %q ;;
    esac
    ;;
  *) exit 64 ;;
esac
`, fixture.candidateVersion, fixture.candidateCalls, converged, inspection)
	fixture.candidate = writeVersionedRuntimeCandidate(
		t, fixture.runtimeRoot, fixture.candidateID, fixture.candidateVersion, candidatePayload,
	)

	sourceManifest, err := os.ReadFile(filepath.Join(fixture.source.root, "runtime-files.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	artifactDigest := sha256.Sum256(sourceManifest)
	registry, err := os.ReadFile(filepath.Join(fixture.source.root, "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	registryDigest := sha256.Sum256(registry)
	authorizationPlan := releasetransition.PlanToken("plan-v1-" + strings.Repeat("a", 64))
	observationScope := releasetransition.Fingerprint(strings.Repeat("d", 64))
	intentPayload, err := json.Marshal(struct {
		AuthorizationPlan releasetransition.PlanToken     `json:"authorizationPlan"`
		ResumePlan        releasetransition.PlanToken     `json:"resumePlan"`
		ObservationScope  releasetransition.Fingerprint   `json:"observationScope"`
		Steps             []releasetransition.JournalStep `json:"steps"`
	}{authorizationPlan, releasetransition.PlanToken(resumePlan), observationScope, []releasetransition.JournalStep{}})
	if err != nil {
		t.Fatal(err)
	}
	intentDigest := sha256.Sum256(intentPayload)
	previous := releasetransition.ReleaseID("0.9.1-old")
	journalPayload, err := releasetransition.MarshalJournal(releasetransition.JournalRecord{
		SchemaVersion: releasetransition.JournalSchemaV2,
		Transaction:   releasetransition.TransactionID(fixture.transaction),
		Goal: releasetransition.Goal{
			Target:    releasetransition.ReleaseID(fixture.sourceID),
			Direction: releasetransition.DirectionActivateTarget,
		},
		Releases: releasetransition.ReleasePair{
			From: previous, Previous: nil, Target: releasetransition.ReleaseID(fixture.sourceID),
		},
		AuthorizationPlan:   authorizationPlan,
		ResumePlan:          releasetransition.PlanToken(resumePlan),
		ArtifactDigest:      releasetransition.Fingerprint(fmt.Sprintf("%x", artifactDigest)),
		RegistryDigest:      releasetransition.Fingerprint(fmt.Sprintf("%x", registryDigest)),
		CatalogDigest:       releasetransition.Fingerprint(strings.Repeat("e", 64)),
		ObservationScope:    observationScope,
		AuthorizationDigest: releasetransition.Fingerprint(strings.Repeat("f", 64)),
		IntentDigest:        releasetransition.Fingerprint(fmt.Sprintf("%x", intentDigest)),
		Checkpoint:          releasetransition.JournalReconciling,
		Steps:               []releasetransition.JournalStep{},
	})
	if err != nil {
		t.Fatal(err)
	}
	journalDigest := sha256.Sum256(journalPayload)
	fixture.journalFingerprint = releasetransition.Fingerprint(fmt.Sprintf("%x", journalDigest))
	journalPath := filepath.Join(fixture.configHome, "release-transition", "v2", "journal.json")
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath, journalPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.installer, []byte(fmt.Sprintf(
		"#!/bin/sh\n# --publish-only\nprintf called > %q\nexit 91\n", fixture.publicationCapture,
	)), 0o700); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func writeVersionedRuntimeCandidate(
	t *testing.T,
	runtimeRoot string,
	releaseID string,
	version string,
	payload string,
) publishedCandidate {
	t.Helper()
	candidateRoot := filepath.Join(runtimeRoot, "releases", releaseID)
	if err := os.MkdirAll(filepath.Join(candidateRoot, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(candidateRoot, "bin", "yard-engine")
	if err := os.WriteFile(engine, []byte(payload), 0o700); err != nil {
		t.Fatal(err)
	}
	registry := []byte("{}\n")
	registryPath := filepath.Join(candidateRoot, "config", "release-transition.json")
	if err := os.MkdirAll(filepath.Dir(registryPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, registry, 0o600); err != nil {
		t.Fatal(err)
	}
	engineDigest := sha256.Sum256([]byte(payload))
	registryDigest := sha256.Sum256(registry)
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n",
		engineDigest, registryDigest)
	if err := os.WriteFile(
		filepath.Join(candidateRoot, "runtime-files.sha256"), []byte(manifest), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if version == "" {
		t.Fatal("candidate fixture version is required")
	}
	return publishedCandidate{release: releasetransition.ReleaseID(releaseID), root: candidateRoot}
}

func unbindCandidateRegistry(
	t *testing.T,
	candidate publishedCandidate,
	removeRegistry bool,
) {
	t.Helper()
	enginePayload, err := os.ReadFile(filepath.Join(candidate.root, "bin", "yard-engine"))
	if err != nil {
		t.Fatal(err)
	}
	engineDigest := sha256.Sum256(enginePayload)
	manifest := fmt.Sprintf("%x  ./bin/yard-engine\n", engineDigest)
	if err := os.WriteFile(
		filepath.Join(candidate.root, "runtime-files.sha256"), []byte(manifest), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if removeRegistry {
		if err := os.Remove(filepath.Join(candidate.root, "config", "release-transition.json")); err != nil {
			t.Fatal(err)
		}
	}
}

func (fixture v0111CandidateRoutingFixture) writeCandidateConsequences(
	t *testing.T,
	consequences []string,
) {
	t.Helper()
	previous := releasetransition.ReleaseID("0.9.1-old")
	inspection := releasetransition.ProcessResponse{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Inspection: &releasetransition.Inspection{
			Plan: releasetransition.PlanToken("plan-v1-" + strings.Repeat("c", 64)),
			Assessment: domain.ActionAssessment{
				Action: "release.transition.v2", Effect: domain.ActionMutation, Changed: true,
				Impacts: []domain.ActionImpact{
					domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactYardRuntime,
				},
				Recovery: domain.RecoveryReversible, Consequences: append([]string(nil), consequences...),
			},
			Outcome: &releasetransition.Outcome{
				Status: releasetransition.StatusMigrationRequired,
				Active: releasetransition.ReleaseID(fixture.sourceID), Previous: &previous,
				Target:  releasetransition.ReleaseID(fixture.candidateID),
				Code:    releasetransition.CodeTransitionRequired,
				Message: "the inspected release transition requires confirmation",
				Retry:   "confirm the inspected update plan",
			},
		},
	}
	transaction := releasetransition.TransactionID("tx-recovery-v0112")
	converged := releasetransition.ProcessResponse{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Outcome: &releasetransition.Outcome{
			Status: releasetransition.StatusReady, ReachedGoal: true,
			Active:   releasetransition.ReleaseID(fixture.candidateID),
			Previous: &fixture.source.release, Target: releasetransition.ReleaseID(fixture.candidateID),
			Code:    releasetransition.CodeReady,
			Message: "the release transition reached the requested goal", Transaction: &transaction,
		},
	}
	inspectionPayload, err := json.Marshal(inspection)
	if err != nil {
		t.Fatal(err)
	}
	convergedPayload, err := json.Marshal(converged)
	if err != nil {
		t.Fatal(err)
	}
	candidatePayload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine %s\n' ;;
  _release-transition)
    request="$(cat)"
    printf '%%s\n' "$request" >> %q
    case "$request" in
      *'"mode":"converge"'*) printf '%%s\n' %q ;;
      *) printf '%%s\n' %q ;;
    esac
    ;;
  *) exit 64 ;;
esac
`, fixture.candidateVersion, fixture.candidateCalls, convergedPayload, inspectionPayload)
	writeVersionedRuntimeCandidate(
		t, fixture.runtimeRoot, fixture.candidateID, fixture.candidateVersion, candidatePayload,
	)
}

func (fixture *v0111CandidateRoutingFixture) writeSourceBlocker(
	t *testing.T,
	blocker releasetransition.Blocker,
) {
	t.Helper()
	previous := releasetransition.ReleaseID("0.9.1-old")
	transaction := releasetransition.TransactionID(fixture.transaction)
	journalPath := filepath.Join(fixture.configHome, "release-transition", "v2", "journal.json")
	journalPayload, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(journalPayload)
	if err != nil {
		t.Fatal(err)
	}
	response := releasetransition.ProcessResponse{
		SchemaVersion: releasetransition.ProcessProtocolSchemaV1,
		Inspection: &releasetransition.Inspection{
			Plan: journal.ResumePlan,
			Assessment: domain.ActionAssessment{
				Action: "release.transition.v2", Effect: domain.ActionMutation, Changed: true,
				Impacts: []domain.ActionImpact{
					domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactYardRuntime,
				},
				Recovery:     domain.RecoveryReversible,
				Consequences: []string{"resume the protected transition"},
			},
			Blockers: []releasetransition.Blocker{blocker},
			Resume:   &transaction,
			Outcome: &releasetransition.Outcome{
				Status: releasetransition.StatusOperatorActionRequired,
				Active: fixture.source.release, Previous: &previous, Target: fixture.source.release,
				Code: blocker.Code, Message: blocker.Message, Retry: blocker.Retry,
				Transaction: &transaction,
			},
		},
	}
	responsePayload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	sourcePayload := fmt.Sprintf(`#!/bin/sh
case "${1:-}" in
  --version) printf 'yard-engine %s\n' ;;
  _release-transition) cat >/dev/null; printf '%%s\n' %q ;;
  *) exit 64 ;;
esac
`, fixture.sourceVersion, string(responsePayload))
	fixture.source = writeVersionedRuntimeCandidate(
		t, fixture.runtimeRoot, fixture.sourceID, fixture.sourceVersion, sourcePayload,
	)
	manifest, err := os.ReadFile(filepath.Join(fixture.source.root, "runtime-files.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	journal.ArtifactDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", digest))
	journalPayload, err = releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath, journalPayload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture v0111CandidateRoutingFixture) environment() map[string]string {
	return map[string]string{
		"HOME":               filepath.Dir(fixture.runtimeRoot),
		"YARD_RELEASE_CACHE": filepath.Join(filepath.Dir(fixture.runtimeRoot), "cache"),
	}
}

func (fixture v0111CandidateRoutingFixture) snapshot(t *testing.T) []byte {
	t.Helper()
	var snapshot bytes.Buffer
	for _, path := range []string{
		filepath.Join(fixture.runtimeRoot, "current"),
		filepath.Join(fixture.runtimeRoot, "previous"),
	} {
		target, err := os.Readlink(path)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&snapshot, "%s\t%s\n", filepath.Base(path), target)
	}
	root := filepath.Join(fixture.configHome, "release-transition", "v2")
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&snapshot, "%s\t%s\n", relative, info.Mode())
		if info.Mode().IsRegular() {
			payload, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			snapshot.Write(payload)
			snapshot.WriteByte('\n')
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot.Bytes()
}

func readCandidateRoutingRequests(
	t *testing.T,
	path string,
) []releasetransition.ProcessRequest {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(payload), []byte{'\n'})
	requests := make([]releasetransition.ProcessRequest, 0, len(lines))
	for _, line := range lines {
		var request releasetransition.ProcessRequest
		if err := json.Unmarshal(line, &request); err != nil {
			t.Fatalf("candidate request %q: %v", line, err)
		}
		requests = append(requests, request)
	}
	return requests
}

func countString(values []string, target string) int {
	count := 0
	for _, value := range values {
		if value == target {
			count++
		}
	}
	return count
}

func releaseIDPointer(value releasetransition.ReleaseID) *releasetransition.ReleaseID {
	return &value
}
