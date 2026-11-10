package releaseruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/blang/semver/v4"
)

func TestSourcePolicyUsesSealedVersionAndV2Provenance(t *testing.T) {
	aboveAnchor := semver.MustParse(UpdateAnchorVersion)
	aboveAnchor.Patch++
	for _, scenario := range []struct {
		name, version, history       string
		registry, bootstrap, allowed bool
	}{
		{"last V1", "0.10.0", "", false, false, false},
		{"low version with V2 registry", "0.10.0", "", true, false, false},
		{"retired direct floor", "0.11.0", "", true, false, false},
		{"below anchor", "0.26.0", "", true, false, false},
		{"pending identity and old source", "0.26.0", "", true, false, false},
		{"inclusive anchor", UpdateAnchorVersion, "", true, false, true},
		{"anchor hotfix", aboveAnchor.String(), "", true, false, true},
		{"unfinished before activation", "0.26.0", "", true, false, false},
		{"unfinished after activation", UpdateAnchorVersion, "", true, false, false},
		{"pending identity and unfinished anchor", UpdateAnchorVersion, "", true, false, false},
		{"unfinished modern source", aboveAnchor.String(), "", true, false, true},
		{"complete old-source journal", UpdateAnchorVersion, "", true, false, true},
		{"exact unfinished anchor owner", UpdateAnchorVersion, "", true, false, true},
		{"missing provenance", "0.11.0", "", false, false, false},
		{"unparseable source", "development", "", true, false, false},
		{"invalid fixture version", "0.26.0.next", "", true, false, false},
		{"invalid without registry", "0.26.0.next", "", false, false, false},
		{"fixture successor", aboveAnchor.String() + "-p0.next", "", true, false, true},
		{"prerelease fixture", aboveAnchor.String() + "-p0.15.next", "", true, false, true},
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
			direction := releasetransition.DirectionActivateTarget
			artifact := releasetransition.Fingerprint("")
			if strings.Contains(scenario.name, "unfinished") || scenario.name == "complete old-source journal" {
				original := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
				payload, err := os.ReadFile(original.journalPath)
				if err != nil {
					t.Fatal(err)
				}
				journal, err := releasetransition.ParseJournal(payload)
				if err != nil {
					t.Fatal(err)
				}
				journal.Releases.From = "0.26.0-aaaaaaaaaaaa"
				journal.Goal.Target = releasetransition.ReleaseID(UpdateAnchorVersion + "-bbbbbbbbbbbb")
				journal.Releases.Target = journal.Goal.Target
				if scenario.name == "unfinished modern source" {
					journal.Releases.From = releasetransition.ReleaseID(UpdateAnchorVersion + "-aaaaaaaaaaaa")
					journal.Goal.Target = releasetransition.ReleaseID(aboveAnchor.String() + "-bbbbbbbbbbbb")
					journal.Releases.Target = journal.Goal.Target
				}
				if scenario.name != "unfinished before activation" {
					journal.Checkpoint = releasetransition.JournalReconciling
				}
				if scenario.name == "complete old-source journal" {
					journal.Checkpoint = releasetransition.JournalComplete
				}
				if scenario.name == "exact unfinished anchor owner" {
					target, artifact = journal.Goal.Target, journal.ArtifactDigest
				}
				if scenario.name != "unfinished before activation" {
					journal.Goal.Target = releasetransition.ReleaseID(release)
					journal.Releases.Target = journal.Goal.Target
					if scenario.name == "exact unfinished anchor owner" {
						target = journal.Goal.Target
					}
				}
				payload, err = releasetransition.MarshalJournal(journal)
				if err != nil {
					t.Fatal(err)
				}
				write(filepath.Join(configHome, "release-transition/v2/journal.json"), payload, 0o600)
				if scenario.name != "unfinished before activation" {
					if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", string(journal.Releases.From)), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("releases/"+string(journal.Releases.From), filepath.Join(runtimeRoot, "previous")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if strings.HasPrefix(scenario.name, "pending identity") {
				digest := strings.Repeat("a", 64)
				marker := config.DeveloperIdentityMarker{Schema: 1, Yard: "default", Inputs: digest, Identity: digest, Release: digest}
				payload, err := json.Marshal(marker)
				if err != nil {
					t.Fatal(err)
				}
				write(config.DeveloperIdentityMarkerPath(configHome, "default"), payload, 0o600)
			}
			err := CheckSourcePolicy(context.Background(), releasetransition.ProcessRequest{RuntimeRoot: runtimeRoot, ConfigHome: configHome, Target: target, Direction: direction, ArtifactDigest: artifact, Mode: releasetransition.ProcessConverge})
			if (err == nil) != scenario.allowed {
				t.Fatalf("source policy allowed=%t: %v", scenario.allowed, err)
			}
			if strings.Contains(scenario.name, "unfinished") && !scenario.allowed {
				if !strings.Contains(err.Error(), "must be completed by its original owner") || !strings.Contains(err.Error(), "yard update --version "+UpdateAnchorVersion) {
					t.Fatalf("lost owner continuation: %v", err)
				}
				if strings.HasPrefix(scenario.name, "pending identity") && !strings.Contains(err.Error(), "developer identity transition is pending") {
					t.Fatalf("lost independent identity refusal: %v", err)
				}
			}
			if !scenario.allowed {
				if _, parseErr := semver.Parse(scenario.version); parseErr != nil {
					if !strings.Contains(err.Error(), "source release version is invalid") || strings.Contains(err.Error(), "uninstall") {
						t.Fatalf("invalid version became destructive pre-floor guidance: %v", err)
					}
				} else if semver.MustParse(scenario.version).GTE(semver.MustParse(MinimumSourceVersion)) {
					if !strings.Contains(err.Error(), "source-anchor-required") || !strings.Contains(err.Error(), "yard update --version "+UpdateAnchorVersion) || strings.Contains(err.Error(), "uninstall") {
						t.Fatalf("missing anchor guidance: %v", err)
					}
				} else if !strings.Contains(err.Error(), "uninstall --full") {
					t.Fatalf("missing removal guidance: %v", err)
				}
				if scenario.name == "pending identity and old source" && !strings.Contains(err.Error(), "developer identity transition is pending") {
					t.Fatalf("lost independent identity refusal: %v", err)
				}
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
				templatePath := filepath.Join("..", "..", "..", "tests", "fixtures", "systemd", strings.TrimSuffix(name, ".in")+"-v0.19.0.in")
				if name == "subyard-keys-sync.service.in" {
					// Historical producers retain the public recipe, independently of current unit policy.
					templatePath = filepath.Join("..", "..", "..", "tests", "fixtures", "systemd", "subyard-keys-sync-v0.21.2.service.in")
				}
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
			if semver.MustParse(scenario.version).GTE(semver.MustParse(MinimumSourceVersion)) {
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

func TestSourceVersionRefusalsDistinguishInvalidFromUnsupported(t *testing.T) {
	aboveAnchor := semver.MustParse(UpdateAnchorVersion)
	aboveAnchor.Patch++
	for _, version := range []string{"0.10.0", "development", "0.26.0.next", "0.11.0", "0.26.0", "0.26.0-p0.15", UpdateAnchorVersion + "-rc.1", UpdateAnchorVersion, aboveAnchor.String()} {
		t.Run(version, func(t *testing.T) {
			err := sourceVersionError(version, "")
			switch version {
			case "0.10.0":
				if err == nil || err.Error() != LegacyReinstallGuidance {
					t.Fatal("lost supported floor refusal", err)
				}
			case "development", "0.26.0.next":
				if err == nil || !strings.Contains(err.Error(), "source release version is invalid") || strings.Contains(err.Error(), "uninstall") {
					t.Fatal("invalid version became uninstall guidance", err)
				}
			case "0.11.0", "0.26.0", "0.26.0-p0.15", UpdateAnchorVersion + "-rc.1":
				want := fmt.Sprintf("source-anchor-required: installed v%s. Direct update requires installed v%s or newer; next: yard update --version %s && yard update", version, UpdateAnchorVersion, UpdateAnchorVersion)
				if err == nil || err.Error() != want {
					t.Fatalf("anchor guidance = %v, want %s", err, want)
				}
			default:
				if err != nil {
					t.Fatal("supported version refused", err)
				}
			}
		})
	}
}

func TestSourcePolicyRetainsOnlyExactUnfinishedOwnerBypass(t *testing.T) {
	for _, mismatch := range []string{"none", "target", "direction", "artifact", "pending identity"} {
		t.Run(mismatch, func(t *testing.T) {
			fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalAuthorized)
			request := releasetransition.ProcessRequest{
				RuntimeRoot: fixture.runtimeRoot, ConfigHome: fixture.configHome,
				Target: releasetransition.ReleaseID(fixture.target), Direction: releasetransition.DirectionActivateTarget,
				ArtifactDigest: releasetransition.Fingerprint(fixture.artifactDigest),
			}
			switch mismatch {
			case "target":
				request.Target = "another-release"
			case "direction":
				request.Direction = releasetransition.DirectionActivatePrevious
			case "artifact":
				request.ArtifactDigest = releasetransition.Fingerprint(strings.Repeat("f", 64))
			case "pending identity":
				digest := strings.Repeat("a", 64)
				payload, err := json.Marshal(config.DeveloperIdentityMarker{Schema: 1, Yard: "default", Inputs: digest, Identity: digest, Release: digest})
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, config.DeveloperIdentityMarkerPath(fixture.configHome, "default"), payload, 0o600)
				request.Mode = releasetransition.ProcessConverge
			}
			// The fixture's source is unavailable: only the exact original owner
			// may bypass fresh source admission and validate recovery itself.
			err := CheckSourcePolicy(context.Background(), request)
			if (err == nil) != (mismatch == "none") {
				t.Fatalf("recovery bypass %s: %v", mismatch, err)
			}
			if mismatch == "pending identity" && !strings.Contains(err.Error(), "developer identity transition is pending") {
				t.Fatalf("exact recovery lost the independent identity fence: %v", err)
			}
		})
	}
}

// Internal update/replacement callers share the same fence as direct CLI and RPC.
func TestSourcePolicyPendingIdentityPrecedesReleaseRecovery(t *testing.T) {
	root := testkit.TempDir(t)
	digest := strings.Repeat("a", 64)
	marker := config.DeveloperIdentityMarker{Schema: 1, Yard: "default", Inputs: digest, Identity: digest, Release: digest}
	payload, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	path := config.DeveloperIdentityMarkerPath(root, "default")
	testkit.WriteFile(t, path, payload, 0600)
	for _, mode := range []releasetransition.ProcessMode{releasetransition.ProcessConverge, releasetransition.ProcessMode(releasetransition.RecoveryProcessConverge)} {
		t.Run(string(mode), func(t *testing.T) {
			err := CheckSourcePolicy(context.Background(), releasetransition.ProcessRequest{ConfigHome: root, Mode: mode, RuntimeRoot: filepath.Join(root, "unavailable-runtime")})
			if err == nil || !strings.Contains(err.Error(), "developer identity transition is pending") {
				t.Fatalf("release mutation escaped identity fence: %v", err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || string(after) != string(payload) {
				t.Fatal("release admission changed pending authority", readErr)
			}
		})
	}
}

func TestSourceAnchorCommandsPreserveRuntimeScope(t *testing.T) {
	t.Setenv("HOME", "/operator")
	t.Setenv("SUBYARD_HOME", "/operator/data")
	for _, root := range []string{"", "/operator/data/runtime", "/custom/runtime", "/custom/owner's runtime"} {
		err := sourceVersionError("0.26.0", root)
		scope := ""
		if root != "" && root != "/operator/data/runtime" {
			scope = " --runtime-root " + shellWord(root)
		}
		expected := "next: yard update --version " + UpdateAnchorVersion + scope + " && yard update" + scope
		if err == nil || !strings.Contains(err.Error(), expected) {
			t.Fatalf("scope %q: %v, want %s", root, err, expected)
		}
	}
}
