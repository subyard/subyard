package releaseruntime

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestSourcePolicyUsesSealedVersionAndV2Provenance(t *testing.T) {
	for _, scenario := range []struct {
		name, version, history       string
		registry, bootstrap, allowed bool
	}{
		{"last V1", "0.10.0", "", false, false, false},
		{"low version with V2 registry", "0.10.0", "", true, false, false},
		{"inclusive floor", "0.11.0", "", true, false, true},
		{"missing provenance", "0.11.0", "", false, false, false},
		{"unparseable source", "development", "", true, false, false},
		{"fresh development bootstrap", "development", "", true, true, true},
		{"raw V1 is not fresh", "development", "migrations/state.json", true, true, false},
		{"raw journal is not fresh", "development", "migrations/transactions/unknown/transaction.json", true, true, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			runtimeRoot, configHome := filepath.Join(root, "runtime"), filepath.Join(root, "config")
			write := func(path string, payload []byte, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, payload, mode)
			}
			release := scenario.version + "-aaaaaaaaaaaa"
			candidate := filepath.Join(runtimeRoot, "releases", release)
			engine := []byte("#!/bin/sh\nprintf 'yard-engine " + scenario.version + "\\n'\n")
			write(filepath.Join(candidate, "bin", "yard-engine"), engine, 0o700)
			manifest := fmt.Sprintf("%x  ./bin/yard-engine\n", sha256.Sum256(engine))
			if scenario.registry {
				registry, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "release-transition.json"))
				if err != nil {
					t.Fatal(err)
				}
				write(filepath.Join(candidate, "config", "release-transition.json"), registry, 0o600)
				manifest += fmt.Sprintf("%x  ./config/release-transition.json\n", sha256.Sum256(registry))
			}
			write(filepath.Join(candidate, "runtime-files.sha256"), []byte(manifest), 0o600)
			if err := os.Symlink("releases/"+release, filepath.Join(runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			if scenario.history != "" {
				write(filepath.Join(configHome, scenario.history), []byte("{}\n"), 0o600)
			}
			target := releasetransition.ReleaseID("0.19.1-bbbbbbbbbbbb")
			if scenario.bootstrap {
				target = releasetransition.ReleaseID(release)
			}
			err := CheckSourcePolicy(context.Background(), releasetransition.ProcessRequest{RuntimeRoot: runtimeRoot, ConfigHome: configHome, Target: target})
			if (err == nil) != scenario.allowed {
				t.Fatalf("source policy allowed=%t: %v", scenario.allowed, err)
			}
			if !scenario.allowed && !strings.Contains(err.Error(), "uninstall --full") {
				t.Fatalf("missing removal guidance: %v", err)
			}
		})
	}
}

func TestHistoricalUninstallSourceIgnoresMigrationReadiness(t *testing.T) {
	for _, scenario := range []struct {
		name, version, creator      string
		unsealed, tampered, allowed bool
	}{
		{"last V1", "0.10.0", "0.11.0", false, false, true},
		{"inclusive V2", "0.11.0", "0.11.0", false, false, true},
		{"later V2", "0.19.0", "0.19.0", false, false, true},
		{"unknown old claim", "0.10.0", "unknown", false, false, false},
		{"modern producer", "0.21.0", "modern", false, false, false},
		{"unsealed historical contract", "0.11.0", "0.11.0", true, false, false},
		{"changed historical contract", "0.19.0", "0.19.0", false, true, false},
		{"rehashed producer template", "0.11.0", "0.11.0", false, false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			runtimeRoot := filepath.Join(root, "runtime")
			release := scenario.version + "-aaaaaaaaaaaa"
			candidate := filepath.Join(runtimeRoot, "releases", release)
			write := func(path string, payload []byte, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, payload, mode)
			}
			creatorPath := filepath.Join("testdata", "historical-project-creators", scenario.creator+".txt")
			if scenario.creator == "modern" {
				creatorPath = filepath.Join("..", "..", "..", "scripts", "02-create-project.sh")
			}
			creator := []byte("unknown creator\n")
			if scenario.creator != "unknown" {
				var err error
				creator, err = os.ReadFile(creatorPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			engine := []byte("#!/bin/sh\nprintf 'yard-engine " + scenario.version + "\\n'\n")
			write(filepath.Join(candidate, "bin", "yard-engine"), engine, 0o700)
			write(filepath.Join(candidate, "scripts", "02-create-project.sh"), creator, 0o600)
			manifest := fmt.Sprintf("%x  ./bin/yard-engine\n", sha256.Sum256(engine))
			for _, name := range []string{"subyard-power-reconcile.service.in", "subyard-keys-sync.service.in", "subyard-keys-sync.timer.in"} {
				templatePath := filepath.Join("..", "..", "..", "config", "systemd", name)
				if name == "subyard-power-reconcile.service.in" && scenario.creator == "0.11.0" {
					templatePath = filepath.Join("testdata", "historical-project-creators", "power-0.10.0.txt")
				}
				body, err := os.ReadFile(templatePath)
				if err != nil {
					t.Fatal(err)
				}
				if scenario.name == "rehashed producer template" && name == "subyard-power-reconcile.service.in" {
					body = append(body, []byte("ExecStart=/foreign\n")...)
				}
				write(filepath.Join(candidate, "config", "systemd", name), body, 0o600)
				manifest += fmt.Sprintf("%x  ./config/systemd/%s\n", sha256.Sum256(body), name)
			}

			if !scenario.unsealed {
				manifest += fmt.Sprintf("%x  ./scripts/02-create-project.sh\n", sha256.Sum256(creator))
			}
			// A V2 registry does not imply ownership and does not prevent cleanup.
			if supportedSourceVersion(scenario.version) {
				registry := []byte("invalid registry is irrelevant to uninstall\n")
				write(filepath.Join(candidate, "config", "release-transition.json"), registry, 0o600)
				manifest += fmt.Sprintf("%x  ./config/release-transition.json\n", sha256.Sum256(registry))
			}
			write(filepath.Join(candidate, "runtime-files.sha256"), []byte(manifest), 0o600)
			if scenario.tampered {
				write(filepath.Join(candidate, "scripts", "02-create-project.sh"), append(creator, '#'), 0o600)
			}
			if err := os.Symlink("releases/"+release, filepath.Join(runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			journals := map[string][]byte{
				"release-transition/v2/journal.json":          []byte("not valid V2 JSON\n"),
				"migrations/state.json":                       []byte("not valid V1 JSON\n"),
				"release-transition/v2/.journal.json.pending": []byte("{\"schemaVersion\":2,\"checkpoint\":\"reconciling\"}\n"),
			}
			for path, data := range journals {
				write(filepath.Join(root, "config", path), data, 0o600)
			}
			err := VerifyLegacyUninstallSource(context.Background(), runtimeRoot)
			if (err == nil) != scenario.allowed {
				t.Fatalf("historical provenance allowed=%t: %v", scenario.allowed, err)
			}
			producer, producerErr := HistoricalUninstallProducerEvidence(context.Background(), runtimeRoot)
			producerAllowed := scenario.allowed && scenario.name != "rehashed producer template"
			if (producerErr == nil) != producerAllowed {
				t.Fatalf("original producer allowed=%t: %v", producerAllowed, producerErr)
			}
			if producerAllowed && (producer.EngineSHA256 != fmt.Sprintf("%x", sha256.Sum256(engine)) || len(producer.Templates) != 3) {
				t.Fatal("original producer binding incomplete")
			}
			for path, data := range journals {
				got, err := os.ReadFile(filepath.Join(root, "config", path))
				if err != nil || string(got) != string(data) {
					t.Fatalf("migration evidence changed: %s: %v", path, err)
				}
			}
		})
	}
}
