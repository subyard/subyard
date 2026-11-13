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
			if strings.HasPrefix(scenario.name, "pending identity") {
				digest := strings.Repeat("a", 64)
				marker := config.DeveloperIdentityMarker{Schema: 1, Yard: "default", Inputs: digest, Identity: digest, Release: digest}
				payload, err := json.Marshal(marker)
				if err != nil {
					t.Fatal(err)
				}
				write(config.DeveloperIdentityMarkerPath(configHome, "default"), payload, 0o600)
			}
			err := CheckSourcePolicy(context.Background(), releasetransition.ProcessRequest{RuntimeRoot: runtimeRoot, ConfigHome: configHome, Target: target, Direction: releasetransition.DirectionActivateTarget, Mode: releasetransition.ProcessConverge})
			if (err == nil) != scenario.allowed {
				t.Fatalf("source policy allowed=%t: %v", scenario.allowed, err)
			}
			if !scenario.allowed {
				if _, parseErr := semver.Parse(scenario.version); parseErr != nil {
					if !strings.Contains(err.Error(), "source release version is invalid") || strings.Contains(err.Error(), "uninstall") {
						t.Fatalf("invalid version became destructive pre-floor guidance: %v", err)
					}
				} else if semver.MustParse(scenario.version).GTE(semver.MustParse(MinimumSourceVersion)) {
					if !strings.Contains(err.Error(), "source-anchor-required") || (!strings.Contains(scenario.name, "rollback") && !strings.Contains(err.Error(), "yard update --version "+UpdateAnchorVersion)) || strings.Contains(err.Error(), "uninstall") {
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

// Admission uses only the sealed active version and whether the journal is the
// request's own: an unfinished foreign journal below the anchor is always finished
// by the installed release with plain yard update, whatever its kind. The engine
// separately validates checkpoint/link consistency, replacement bindings and consent.
func TestSourcePolicyUnfinishedAnchorHop(t *testing.T) {
	// Literal expectations: moving the anchor must revisit this table.
	if UpdateAnchorVersion != "0.28.0" {
		t.Fatalf("update the literal anchor expectations for v%s", UpdateAnchorVersion)
	}
	const (
		replace = "yard update --version 0.28.0 && yard update"
		resume  = "yard update && yard update"
		chain   = "yard update && yard update --version 0.28.0 && yard update"
		legacy  = "legacy"
	)
	modern := semver.MustParse(UpdateAnchorVersion)
	modern.Patch++
	for _, checkpoint := range []releasetransition.JournalCheckpoint{releasetransition.JournalAuthorized, releasetransition.JournalReconciling, releasetransition.JournalComplete} {
		for _, activated := range []bool{false, true} {
			// want keys: "<active side>/<journal kind>"; "" admits, legacy is the
			// full-uninstall route, anything else is the single expected next.
			for _, pair := range []struct {
				name, from, target string
				want               map[string]string
			}{
				{"hop", "0.27.0", "0.28.0", map[string]string{
					"from/complete": replace, "from/target": resume, "from/previous": chain,
				}},
				{"pre-floor", "0.10.0", "0.20.0", map[string]string{
					"from/complete": legacy, "from/target": legacy, "from/previous": legacy,
					"target/complete": replace, "target/target": chain, "target/previous": chain,
				}},
				{"modern", "0.28.0", modern.String(), map[string]string{}},
			} {
				for _, direction := range []releasetransition.Direction{releasetransition.DirectionActivateTarget, releasetransition.DirectionActivatePrevious} {
					name := fmt.Sprintf("%s/activated=%t/%s/%s", checkpoint, activated, pair.name, direction)
					t.Run(name, func(t *testing.T) {
						fixture := newProtectedRuntimeTransitionFixture(t, checkpoint)
						payload := mustReadFile(t, fixture.journalPath)
						journal, err := releasetransition.ParseJournal(payload)
						if err != nil {
							t.Fatal(err)
						}
						journal.Releases = releasetransition.ReleasePair{From: releasetransition.ReleaseID(pair.from + "-aaaaaaaaaaaa"), Target: releasetransition.ReleaseID(pair.target + "-bbbbbbbbbbbb")}
						journal.Goal = releasetransition.Goal{Target: journal.Releases.Target, Direction: direction}
						if direction == releasetransition.DirectionActivatePrevious {
							journal.Releases.Previous = &journal.Releases.Target
						}
						write := func(journal releasetransition.JournalRecord) []byte {
							t.Helper()
							payload, err := releasetransition.MarshalJournal(journal)
							if err != nil {
								t.Fatal(err)
							}
							testkit.WriteFile(t, fixture.journalPath, payload, 0600)
							return payload
						}
						payload = write(journal)
						active, version, previous := journal.Releases.From, pair.from, journal.Releases.Previous
						if activated {
							active, version, previous = journal.Releases.Target, pair.target, &journal.Releases.From
						}
						for _, release := range []releasetransition.ReleaseID{journal.Releases.From, journal.Releases.Target} {
							if err := os.MkdirAll(filepath.Join(fixture.runtimeRoot, "releases", string(release), "bin"), 0700); err != nil {
								t.Fatal(err)
							}
						}
						root := filepath.Join(fixture.runtimeRoot, "releases", string(active))
						testkit.WriteFile(t, filepath.Join(root, "bin/yard-engine"), []byte("#!/bin/sh\nprintf 'yard-engine "+version+"\\n'\n"), 0700)
						if err := os.MkdirAll(filepath.Join(root, "config"), 0700); err != nil {
							t.Fatal(err)
						}
						sealSourceAdmissionCandidate(t, publishedCandidate{release: active, root: root})
						registry, _, err := releasetransition.ParseRegistryV2([]byte(sourceAdmissionRegistry), releasetransition.BuiltinCapabilityCatalog())
						if err != nil {
							t.Fatal(err)
						}
						ledger, _, err := releasetransition.MarshalLedgerV2(releasetransition.BaselineLedgerV2(registry), registry)
						if err != nil {
							t.Fatal(err)
						}
						testkit.WriteFile(t, filepath.Join(fixture.configHome, "release-transition/v2/ledger.json"), ledger, 0600)
						for _, link := range []string{"current", "previous"} {
							if err := os.Remove(filepath.Join(fixture.runtimeRoot, link)); err != nil && !os.IsNotExist(err) {
								t.Fatal(err)
							}
						}
						if err := os.Symlink("releases/"+string(active), filepath.Join(fixture.runtimeRoot, "current")); err != nil {
							t.Fatal(err)
						}
						if previous != nil {
							if err := os.Symlink("releases/"+string(*previous), filepath.Join(fixture.runtimeRoot, "previous")); err != nil {
								t.Fatal(err)
							}
						}
						unfinished := checkpoint != releasetransition.JournalComplete
						key := "from/"
						if activated {
							key = "target/"
						}
						switch {
						case !unfinished:
							key += "complete"
						case direction == releasetransition.DirectionActivateTarget:
							key += "target"
						default:
							key += "previous"
						}
						next := pair.want[key]
						activeBelow := next != ""
						request := releasetransition.ProcessRequest{RuntimeRoot: fixture.runtimeRoot, ConfigHome: fixture.configHome, Target: "1.2.3-cccccccccccc", Direction: releasetransition.DirectionActivateTarget, Mode: releasetransition.ProcessInspect}
						err = CheckSourcePolicy(context.Background(), request)
						if (err == nil) != !activeBelow {
							t.Fatalf("allowed=%t: %v", !activeBelow, err)
						}
						if err != nil && next == legacy {
							if err.Error() != LegacyReinstallGuidance {
								t.Fatalf("below the V2 floor must keep only the full-uninstall route: %v", err)
							}
						} else if err != nil {
							if strings.Count(err.Error(), "source-anchor-required:") != 1 || strings.Count(err.Error(), "next:") != 1 ||
								!strings.Contains(err.Error(), "next: "+next+"\n") && !strings.HasSuffix(err.Error(), "next: "+next) {
								t.Fatalf("lost single continuation %q: %v", next, err)
							}
							if unfinished && (!strings.Contains(err.Error(), "Do not uninstall.") || strings.Contains(err.Error(), "uninstall --full")) {
								t.Fatalf("lost repair guidance: %v", err)
							}
						}
						if after := mustReadFile(t, fixture.journalPath); string(after) != string(payload) {
							t.Fatal("admission changed the original journal")
						}
						if unfinished && next == legacy {
							// Without sealed registry provenance the floor still wins over
							// the provenance repair hint.
							if err := os.Remove(filepath.Join(root, "config", "release-transition.json")); err != nil {
								t.Fatal(err)
							}
							engine := mustReadFile(t, filepath.Join(root, "bin", "yard-engine"))
							testkit.WriteFile(t, filepath.Join(root, "runtime-files.sha256"), []byte(fmt.Sprintf("%x  ./bin/yard-engine\n", sha256.Sum256(engine))), 0o600)
							if err := CheckSourcePolicy(context.Background(), request); err == nil || err.Error() != LegacyReinstallGuidance {
								t.Fatalf("registry-less source below the V2 floor lost the full-uninstall route: %v", err)
							}
						}
						if !unfinished || !activeBelow || next == legacy {
							return
						}
						// Every unfinished kind, including recovery ownership, gets the same
						// continuation: the installed release resumes its own journal.
						journal.Transaction = "recovery-v1-owner"
						write(journal)
						if err := CheckSourcePolicy(context.Background(), request); err == nil || !strings.Contains(err.Error(), "next: "+next) {
							t.Fatalf("recovery journal lost the uniform continuation: %v", err)
						}
						// Pending identity is independent of the anchor finding.
						digest := strings.Repeat("a", 64)
						marker, _ := json.Marshal(config.DeveloperIdentityMarker{Schema: 1, Yard: "default", Inputs: digest, Identity: digest, Release: digest})
						path := config.DeveloperIdentityMarkerPath(fixture.configHome, "default")
						if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
							t.Fatal(err)
						}
						testkit.WriteFile(t, path, marker, 0600)
						request.Mode = releasetransition.ProcessConverge
						err = CheckSourcePolicy(context.Background(), request)
						if err == nil || !strings.Contains(err.Error(), "developer identity transition is pending") || strings.Count(err.Error(), "source-anchor-required:") != 1 {
							t.Fatalf("lost independent refusal: %v", err)
						}
					})
				}
			}
		}
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
			err := sourceVersionError(version)
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

func TestActivatedTakeoverRequiresCompleteOriginalAuthority(t *testing.T) {
	for _, state := range []string{"legacy-complete", "checkpoint-complete", "missing", "incomplete", "contradictory"} {
		t.Run(state, func(t *testing.T) {
			fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalReconciling)
			t.Setenv("SUBYARD_HOME", filepath.Dir(fixture.runtimeRoot))
			active := releasetransition.ReleaseID(UpdateAnchorVersion + "-bbbbbbbbbbbb")
			original := releasetransition.ReleaseID("0.26.0-aaaaaaaaaaaa")
			root := filepath.Join(fixture.runtimeRoot, "releases", string(active))
			if err := os.MkdirAll(filepath.Join(root, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			engine := []byte("#!/bin/sh\nprintf 'yard-engine " + UpdateAnchorVersion + "\\n'\n")
			testkit.WriteFile(t, filepath.Join(root, "bin/yard-engine"), engine, 0700)
			registryBytes, err := os.ReadFile(filepath.Join("..", "..", "..", "config/release-transition.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "config"), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "config/release-transition.json"), registryBytes, 0600)
			manifest := fmt.Sprintf("%x  ./bin/yard-engine\n%x  ./config/release-transition.json\n", sha256.Sum256(engine), sha256.Sum256(registryBytes))
			testkit.WriteFile(t, filepath.Join(root, "runtime-files.sha256"), []byte(manifest), 0600)
			if err := os.Remove(filepath.Join(fixture.runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(fixture.runtimeRoot, "releases", string(original)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("releases/"+string(active), filepath.Join(fixture.runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("releases/"+string(original), filepath.Join(fixture.runtimeRoot, "previous")); err != nil {
				t.Fatal(err)
			}
			journal, err := releasetransition.ParseJournal(mustReadFile(t, fixture.journalPath))
			if err != nil {
				t.Fatal(err)
			}
			journal.Releases = releasetransition.ReleasePair{From: original, Target: active}
			journal.Goal = releasetransition.Goal{Target: active, Direction: releasetransition.DirectionActivateTarget}
			payload, err := releasetransition.MarshalJournal(journal)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, fixture.journalPath, payload, 0600)
			registry, digest, err := releasetransition.ParseRegistryV2(registryBytes, releasetransition.BuiltinCapabilityCatalog())
			if err != nil {
				t.Fatal(err)
			}
			baseline := releasetransition.BaselineLedgerV2(registry)
			complete := baseline
			pending, err := registry.PendingPath(complete)
			if err != nil {
				t.Fatal(err)
			}
			for _, edge := range pending {
				complete, err = complete.Advance(registry, edge)
				if err != nil {
					t.Fatal(err)
				}
			}
			projection := complete
			if state != "legacy-complete" {
				projection = baseline
			}
			ledger, fp, err := releasetransition.MarshalLedgerV2(projection, registry)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(fixture.configHome, "release-transition/v2/ledger.json")
			if state != "missing" {
				testkit.WriteFile(t, path, ledger, 0600)
			}
			checkpointPath := filepath.Join(fixture.configHome, "release-transition/v2/history-checkpoint.json")
			if state == "checkpoint-complete" || state == "contradictory" {
				checkpoint := releasetransition.LedgerCheckpointV1{SchemaVersion: 1, RegistryDigest: digest, LegacyProjection: releasetransition.LedgerProjectionBindingV1{Exists: true, Fingerprint: fp, Epochs: map[string]int{}}, Domains: map[string]releasetransition.DomainCheckpointV1{}}
				for domain, value := range complete.Domains {
					prefix, err := registry.MigrationPrefixDigest(domain, value.Epoch)
					if err != nil {
						t.Fatal(err)
					}
					checkpoint.Domains[domain] = releasetransition.DomainCheckpointV1{Epoch: value.Epoch, CompactedThrough: value.Epoch, PrefixDigest: prefix, AppliedSuffix: []string{}}
					checkpoint.LegacyProjection.Epochs[domain] = baseline.Domains[domain].Epoch
				}
				if state == "contradictory" {
					checkpoint.LegacyProjection.Fingerprint = releasetransition.Fingerprint(strings.Repeat("f", 64))
				}
				payload, err := json.Marshal(checkpoint)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, checkpointPath, payload, 0600)
			}
			before := map[string]string{}
			for _, path := range []string{fixture.journalPath, path, checkpointPath} {
				raw, err := os.ReadFile(path)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				before[path] = string(raw)
			}
			request := releasetransition.ProcessRequest{ConfigHome: fixture.configHome, RuntimeRoot: fixture.runtimeRoot, Target: "1.2.3-cccccccccccc", Direction: releasetransition.DirectionActivateTarget, Mode: releasetransition.ProcessInspect}
			err = CheckSourcePolicy(context.Background(), request)
			allowed := state == "legacy-complete" || state == "checkpoint-complete"
			if (err == nil) != allowed {
				t.Fatalf("authority %s allowed=%t: %v", state, allowed, err)
			}
			if !allowed {
				if strings.Contains(err.Error(), "uninstall --full") || strings.Contains(err.Error(), "next:") ||
					!strings.HasPrefix(err.Error(), "replacement-authority-required: ") || !strings.Contains(err.Error(), "known-good backup") {
					t.Fatalf("unsafe damaged-history guidance: %v", err)
				}
			}
			for path, raw := range before {
				after, _ := os.ReadFile(path)
				if string(after) != raw {
					t.Fatal("admission mutated authority", path)
				}
			}
		})
	}
}
