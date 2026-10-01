package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestConfigLayoutRollbackCompatibility(t *testing.T) {
	for _, scenario := range []struct {
		name, options  string
		schema         int
		files, allowed bool
	}{
		{"legacy cached layout", "--scope", 2, true, false},
		{"supported cached layout", "--scope --local --git", 2, true, true},
		{"partial support", "--scope --git", 2, true, false},
		{"legacy original layout", "--scope", 1, true, true},
		{"legacy empty cache", "--scope", 2, false, true},
		{"legacy no manifest", "--scope", 0, false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			runtime, parsed, candidate, request, called := configLayoutRollbackFixture(t, scenario.options)
			if scenario.schema != 0 {
				writeConfigLayoutManifest(t, request.ConfigHome, scenario.schema, scenario.files)
			}
			_, err := prepareCandidateTransitionForTest(runtime, context.Background(), parsed, candidate, request)
			if scenario.allowed {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(called); err != nil {
					t.Fatalf("compatible target never inspected: %v", err)
				}
			} else {
				var failure publicReleaseInspectionError
				if !errors.As(err, &failure) || failure.outcome.Code != releasetransition.CodeRollbackIncompatible {
					t.Fatalf("rollback failure = %v", err)
				}
				if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("incompatible target planned rollback: %v", err)
				}
			}
		})
	}
}

func TestConfigLayoutRollbackWithoutManifestPreservesExistingRootModes(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o775} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			runtime, parsed, candidate, request, called := configLayoutRollbackFixture(t, "--scope")
			if err := os.Chmod(request.ConfigHome, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := prepareCandidateTransitionForTest(runtime, context.Background(), parsed, candidate, request); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(called); err != nil {
				t.Fatalf("legacy target was blocked: %v", err)
			}
			info, err := os.Stat(request.ConfigHome)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("existing root mode changed: %v", err)
			}
			entries, err := os.ReadDir(request.ConfigHome)
			if err != nil || len(entries) != 0 {
				t.Fatalf("rollback inspection mutated config root: %v", err)
			}
		})
	}
}

func TestConfigLayoutRollbackRejectsUnboundMetadata(t *testing.T) {
	runtime, parsed, candidate, request, called := configLayoutRollbackFixture(t, "--scope --local --git")
	manifestPath := filepath.Join(candidate.root, "runtime-files.sha256")
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(payload), []byte("\n"))
	testkit.WriteFile(t, manifestPath, append(bytes.Join(lines[:len(lines)-1], []byte("\n")), '\n'), 0o600)
	writeConfigLayoutManifest(t, request.ConfigHome, 2, true)
	_, err = prepareCandidateTransitionForTest(runtime, context.Background(), parsed, candidate, request)
	var failure publicReleaseInspectionError
	if !errors.As(err, &failure) || failure.outcome.Code != releasetransition.CodeRollbackIncompatible {
		t.Fatalf("unbound metadata accepted: %v", err)
	}
	if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unbound target planned rollback: %v", err)
	}
}

func TestConfigLayoutRollbackRechecksBeforeExecution(t *testing.T) {
	runtime, parsed, candidate, request, called := configLayoutRollbackFixture(t, "--scope")
	prepared, err := prepareCandidateTransitionForTest(runtime, context.Background(), parsed, candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(called)
	if err != nil {
		t.Fatal(err)
	}
	writeConfigLayoutManifest(t, request.ConfigHome, 2, true)
	err = prepared.Execute(context.Background())
	var failure publicReleaseInspectionError
	if !errors.As(err, &failure) || failure.outcome.Code != releasetransition.CodeRollbackIncompatible {
		t.Fatalf("rollback execution failure = %v", err)
	}
	after, err := os.ReadFile(called)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("incompatible target executed: %v", err)
	}
}

func configLayoutRollbackFixture(t *testing.T, metadata string) (*Runtime, options, publishedCandidate, releasetransition.ProcessRequest, string) {
	t.Helper()
	root := testkit.TempDir(t)
	runtimeRoot := filepath.Join(root, "runtime")
	configHome := filepath.Join(root, "config")
	for _, path := range []string{filepath.Join(runtimeRoot, "releases", "release-a"), configHome} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	called := filepath.Join(root, "transition-called")
	response := `{"schemaVersion":1,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["rollback"]},"outcome":{"status":"migration-required","reachedGoal":false,"active":"release-a","previous":"release-b","target":"release-b","code":"transition-required","message":"pending","retry":"run yard update --rollback"}}}`
	payload := fmt.Sprintf("#!/bin/sh\ncase \"${1:-}\" in\n--version) printf 'yard-engine 1.2.3\\n' ;;\n--command-options) printf '%%s\\n' %q ;;\n_release-transition) cat >/dev/null; printf 'called\\n' >> %q; printf '%%s\\n' %q ;;\n*) exit 64 ;;\nesac\n", metadata, called, response)
	candidate := writeRuntimeCandidatePayload(t, runtimeRoot, payload)
	registry := []byte("config||@config||local|mutate|dynamic|public|lifecycle|config|config <command>|configuration settings|" + metadata + "|set unset import edit\n")
	testkit.WriteFile(t, filepath.Join(candidate.root, "config", "commands.registry"), registry, 0o600)
	manifestPath := filepath.Join(candidate.root, "runtime-files.sha256")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = append(manifest, []byte(fmt.Sprintf("%x  ./config/commands.registry\n", sha256.Sum256(registry)))...)
	testkit.WriteFile(t, manifestPath, manifest, 0o600)

	for link, target := range map[string]string{"current": "release-a", "previous": "release-b"} {
		if err := os.Symlink(filepath.Join("releases", target), filepath.Join(runtimeRoot, link)); err != nil {
			t.Fatal(err)
		}
	}
	runtime := New(Config{Environment: map[string]string{"HOME": root}, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	request := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect, RuntimeRoot: runtimeRoot, ConfigHome: configHome, Target: candidate.release, Direction: releasetransition.DirectionActivatePrevious}
	return runtime, options{root: runtimeRoot}, candidate, request, called
}

func writeConfigLayoutManifest(t *testing.T, root string, schema int, files bool) {
	t.Helper()
	directory := filepath.Join(root, ".sync")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	entries := "[]"
	if files {
		entries = `[{"path":".sync/settings/yards/cached/config.env"}]`
	}
	testkit.WriteFile(t, filepath.Join(directory, "manifest.json"), []byte(fmt.Sprintf(`{"schemaVersion":%d,"files":%s}`, schema, entries)), 0o600)
}
