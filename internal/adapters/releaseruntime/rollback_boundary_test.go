package releaseruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestDeclaredRollbackBoundaryRefusesBeforePreparation(t *testing.T) {
	for _, test := range []struct {
		name, through, component               string
		target, tamper, unsealed, format, long bool
		refused                                bool
	}{
		{name: "active release declares boundary", through: "0.30.0", refused: true},
		{name: "registry boundary still refuses", through: "0.30.0", component: "registry", refused: true},
		{name: "bounded outcome retains boundary diagnostic", through: "0.30.0", long: true, refused: true},
		{name: "older targets also refused", through: "0.30.1", refused: true},
		{name: "target beyond boundary", through: "0.27.0"},
		{name: "target declaration cannot forbid upgrade", through: "0.30.0", target: true},
		{name: "tampered active declaration", through: "0.30.0", tamper: true, refused: true},
		{name: "unsealed declaration", through: "0.30.0", unsealed: true, refused: true},
		{name: "independent format refusal", through: "0.30.0", format: true, refused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := preparedRollbackOptions{seed: func(active, target string) {
				root := active
				if test.target {
					root = target
				}
				declaration := releasetransition.RollbackDeclaration{SchemaVersion: 1,
					PredecessorRegistryDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
					Migrations:                []json.RawMessage{}, ProfileFormats: []json.RawMessage{},
					Boundaries: []releasetransition.RollbackBoundary{{Component: "profile:sample-runtime", ThroughVersion: test.through,
						Reason: "sample state cannot be read by the previous release", Recovery: "keep the current release active and restore a saved sample database before manual downgrade"}}}
				if test.component != "" {
					declaration.Boundaries[0].Component = test.component
				}
				if test.long {
					declaration.Boundaries[0].Reason = strings.Repeat("sample state ", 32) + "is unreadable"
				}
				data, err := json.Marshal(declaration)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, "config/release-rollback.json"), data, 0o600)
			}}
			if test.format {
				options.active = `,"format":2`
			}
			fixture := newPreparedRollbackFixture(t, options)
			root := filepath.Join(fixture.runtimeRoot, "releases", fixture.activeID)
			if test.tamper {
				testkit.WriteFile(t, filepath.Join(root, "config/release-rollback.json"), []byte("{}"), 0o600)
			}
			if test.unsealed {
				path := filepath.Join(root, "runtime-files.sha256")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var kept []string
				for _, line := range strings.Split(string(data), "\n") {
					if !strings.Contains(line, "./config/release-rollback.json") {
						kept = append(kept, line)
					}
				}
				testkit.WriteFile(t, path, []byte(strings.Join(kept, "\n")), 0o600)
			}
			_, err := fixture.prepare(t)
			if test.tamper || test.unsealed {
				if err == nil {
					t.Fatal("unverified declaration accepted")
				}
			} else if test.refused {
				outcome := outcomeOf(t, err)
				if outcome.Code != releasetransition.CodeRollbackIncompatible ||
					!strings.Contains(err.Error(), "declared rollback boundary") || !strings.Contains(err.Error(), "restore a saved sample database") ||
					!strings.Contains(outcome.Retry, "keep the current release active") {
					t.Fatalf("refusal lost boundary or next step: %#v, %v", outcome, err)
				}
				if test.long && !strings.Contains(outcome.Message, "declared release boundary") {
					t.Fatalf("bounded diagnostic lost boundary: %#v", outcome)
				}
				if test.format && !strings.Contains(err.Error(), "sample-runtime 1 instead of 2") {
					t.Fatalf("independent format finding lost: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			fixture.assertUnchanged(t)
			if len(fixture.applies) != 0 {
				t.Fatal("refusal mutated profile state")
			}
		})
	}
}

// The introducing 0.31 candidate's shipped agent declaration must not obstruct
// the existing profile preparation path to the published 0.30 predecessor.
func TestShippedAgentCleanupAllowsRollbackToV030(t *testing.T) {
	data, err := os.ReadFile("../../../config/release-rollback.json")
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := releasetransition.ParseRollbackDeclaration(data)
	if err != nil {
		t.Fatal(err)
	}
	var notices []releasetransition.RollbackBoundary
	for _, entry := range declaration.Boundaries {
		if entry.Component == "agents" {
			notices = append(notices, entry)
		}
	}
	if len(notices) == 0 {
		t.Fatal("shipped agent cleanup declaration is missing")
	}
	for _, preparing := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary rollback", true: "profile preparation then rollback"}[preparing], func(t *testing.T) {
			options := preparedRollbackOptions{targetVersion: "0.30.0", seed: func(active, target string) {
				testkit.WriteFile(t, filepath.Join(active, "config/release-rollback.json"), data, 0o600)
			}}
			if preparing {
				options.active = preparedSample.active
			}
			fixture := newPreparedRollbackFixture(t, options)
			active, target := fixture.verified(t)
			identity := active.candidate.release
			active.candidate.release = "unversioned-source"
			message, _, cleanup, compareErr := declaredRollbackBoundary(active, target)
			if compareErr != nil || message != "" || len(cleanup) != len(notices) {
				t.Fatalf("agent notice refused an unversioned source: %s, %v", message, compareErr)
			}
			active.candidate.release = identity
			prepared, err := fixture.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			checkNotices := func(prepared Prepared) {
				t.Helper()
				if len(prepared.AgentCleanup) != len(notices) {
					t.Fatalf("missing cleanup notices: %#v", prepared.AgentCleanup)
				}
				rendered := strings.Join(prepared.Consequences, "\n")
				for _, note := range prepared.AgentCleanupConsequences() {
					if !strings.Contains(rendered, note) {
						t.Fatalf("plan lost cleanup: %s", rendered)
					}
				}
				for i, notice := range notices {
					if prepared.AgentCleanup[i].Recovery != notice.Recovery || !slices.Equal(prepared.AgentCleanup[i].Files, notice.Files) {
						t.Fatalf("cleanup changed: %#v", prepared.AgentCleanup)
					}
				}
			}
			checkNotices(prepared)
			if preparing {
				if prepared.Action != PrepareRollbackAction {
					t.Fatalf("no preparation offered: %s", prepared.Action)
				}
				if err := prepared.Execute(context.Background()); err != nil {
					t.Fatal(err)
				}
				prepared, err = fixture.prepare(t)
				if err != nil {
					t.Fatal(err)
				}
				checkNotices(prepared)
			}
			if prepared.Action != "update.rollback" {
				t.Fatalf("no rollback offered: %s", prepared.Action)
			}
			// This fixture's retained engine deliberately exits at converge. Dispatch,
			// rather than an agent refusal, proves admission reached that engine.
			_ = prepared.Execute(context.Background())
			if !strings.HasSuffix(fixture.engineLog(t), "converge\n") {
				t.Fatalf("rollback not dispatched: %s", fixture.engineLog(t))
			}
		})
	}
}

func TestProtectedRollbackRetainsAgentCleanup(t *testing.T) {
	data, err := os.ReadFile("../../../config/release-rollback.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, direction := range []releasetransition.Direction{releasetransition.DirectionActivatePrevious, releasetransition.DirectionActivateTarget} {
		for _, switched := range []bool{false, true} {
			t.Run(string(direction)+"/"+map[bool]string{false: "before activation", true: "after activation"}[switched], func(t *testing.T) {
				fixture := newPreparedRollbackFixture(t, preparedRollbackOptions{targetVersion: "0.30.0", seed: func(active, _ string) {
					testkit.WriteFile(t, filepath.Join(active, "config/release-rollback.json"), data, 0o600)
				}})
				prepared, err := fixture.prepare(t)
				if err != nil {
					t.Fatal(err)
				}
				active, target := fixture.verified(t)
				request := prepared.Request
				request.Mode = releasetransition.ProcessInspect
				response, err := fixture.runtime.invokeVerifiedCandidateTransition(context.Background(), active, request, "")
				if err != nil {
					t.Fatal(err)
				}
				request.Direction = direction
				if switched {
					next := filepath.Join(fixture.runtimeRoot, "next-current")
					if err := os.Symlink("releases/"+fixture.targetID, next); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(next, filepath.Join(fixture.runtimeRoot, "current")); err != nil {
						t.Fatal(err)
					}
					response.Inspection.Outcome.Active = releasetransition.ReleaseID(fixture.targetID)
					previous := releasetransition.ReleaseID(fixture.activeID)
					response.Inspection.Outcome.Previous = &previous
				}
				resumed, err := fixture.runtime.prepareProtectedTransition(options{root: fixture.runtimeRoot}, &protectedTransitionInspection{
					owner: verificationOf(active), target: verificationOf(target), request: request, inspection: *response.Inspection,
					activationReconciliationOwned: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				if len(resumed.AgentCleanup) == 0 || !slices.Equal(resumed.AgentCleanupConsequences(), prepared.AgentCleanupConsequences()) {
					t.Fatalf("resume lost cleanup: %#v", resumed.AgentCleanup)
				}
				for _, consequence := range prepared.AgentCleanupConsequences() {
					if !slices.Contains(resumed.Consequences, consequence) {
						t.Fatalf("resumed plan lost %s", consequence)
					}
				}
			})
		}
	}
}

func TestRollbackBoundaryUsesVerifiedCandidateVersions(t *testing.T) {
	fixture := newPreparedRollbackFixture(t, preparedRollbackOptions{targetVersion: "0.30.0", seed: func(active, _ string) {
		declaration := releasetransition.RollbackDeclaration{SchemaVersion: 1, PredecessorRegistryDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
			Migrations: []json.RawMessage{}, ProfileFormats: []json.RawMessage{}, Boundaries: []releasetransition.RollbackBoundary{{Component: "registry", ThroughVersion: "0.30.0", Reason: "unreadable state", Recovery: "restore a complete pre-upgrade backup into a clean installation"}}}
		data, err := json.Marshal(declaration)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(active, "config/release-rollback.json"), data, 0o600)
	}})
	active, target := fixture.verified(t)
	// IDs suggest an upgrade; the verified engine versions establish a downgrade.
	active.candidate.release, target.candidate.release = "0.9.0-owner", "0.10.0-target"
	message, _, _, err := declaredRollbackBoundary(active, target)
	if err == nil || !strings.Contains(message, "declared rollback boundary") {
		t.Fatalf("reported versions ignored: %s, %v", message, err)
	}
	target.version = "0.32.0"
	if message, _, _, err := declaredRollbackBoundary(active, target); err != nil || message != "" {
		t.Fatalf("upgrade refused: %s, %v", message, err)
	}
}

func TestBlockingBoundaryCannotBeBypassedByOlderVersionOrBootstrap(t *testing.T) {
	for _, bootstrap := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit version", true: "bootstrap"}[bootstrap], func(t *testing.T) {
			fixture := retainedVersionFixture
			if bootstrap {
				fixture = bootstrapDraftFixture
			}
			runtime, candidate, request, called, args := fixture(t)
			active := publishedCandidate{release: "release-a", root: filepath.Join(request.RuntimeRoot, "releases/release-a")}
			writeMigrationRollbackSource(t, candidate, request, []byte(sourceAdmissionRegistry))
			rewriteReleaseOwnerManifest(t, candidate)
			declaration := releasetransition.RollbackDeclaration{SchemaVersion: 1, PredecessorRegistryDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)), Migrations: []json.RawMessage{}, ProfileFormats: []json.RawMessage{}, Boundaries: []releasetransition.RollbackBoundary{{Component: "registry", ThroughVersion: "0.30.0", Reason: "unreadable state", Recovery: "restore a complete pre-upgrade backup into a clean older installation"}}}
			data, err := json.Marshal(declaration)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(active.root, "config/release-rollback.json"), data, 0o600)
			if err := os.MkdirAll(filepath.Join(active.root, "bin"), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(active.root, "bin/yard-engine"), []byte("#!/bin/sh\n[ \"$1\" = --version ] || exit 64\nprintf 'yard-engine 0.31.0\\n'\n"), 0o700)
			rewriteReleaseOwnerManifest(t, active)
			newerRoot := filepath.Join(request.RuntimeRoot, "releases/0.31.0-active")
			if err := os.Rename(active.root, newerRoot); err != nil {
				t.Fatal(err)
			}
			next := filepath.Join(request.RuntimeRoot, "new-current")
			if err := os.Symlink("releases/0.31.0-active", next); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(next, filepath.Join(request.RuntimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			_, err = runtime.PrepareTransition(context.Background(), args, request.ConfigHome, request.Yard)
			if err == nil || !strings.Contains(err.Error(), "declared rollback boundary") || !strings.Contains(err.Error(), "complete pre-upgrade backup") {
				t.Fatalf("boundary bypassed: %v", err)
			}
			for _, path := range []string{called, filepath.Join(request.ConfigHome, "release-transition/v2/journal.json")} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("refusal changed %s: %v", path, err)
				}
			}
			if current, err := os.Readlink(filepath.Join(request.RuntimeRoot, "current")); err != nil || current != "releases/0.31.0-active" {
				t.Fatalf("current changed: %s %v", current, err)
			}
			previous, err := os.Readlink(filepath.Join(request.RuntimeRoot, "previous"))
			if bootstrap {
				if !os.IsNotExist(err) {
					t.Fatalf("previous created: %s %v", previous, err)
				}
			} else if err != nil || previous != "releases/"+string(request.Target) {
				t.Fatalf("previous changed: %s %v", previous, err)
			}
		})
	}
}
