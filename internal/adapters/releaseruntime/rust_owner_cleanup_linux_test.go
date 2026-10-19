//go:build linux

package releaseruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/uninstall"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRustOriginalSourceFactsAreOptionalAndSessionPinned(t *testing.T) {
	for _, available := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired-clean-history", true: "retained-original"}[available], func(t *testing.T) {
			runtime, verified, request := transportRustSessionFixture(t)
			fixture := newProtectedRuntimeTransitionFixture(t, releasetransition.JournalComplete)
			raw, err := os.ReadFile(fixture.journalPath)
			if err != nil {
				t.Fatal(err)
			}
			journal, err := releasetransition.ParseJournal(raw)
			if err != nil {
				t.Fatal(err)
			}
			journal.Releases.From = "1.2.3-original"
			previous := releasetransition.ReleaseID("1.2.3-expired-previous")
			journal.Releases.Previous = &previous
			if available {
				writeDelegateRuntimeFixture(t, request.RuntimeRoot, string(journal.Releases.From), "original", []byte("{}\n"), "#!/bin/sh\nprintf 'yard-engine 1.2.3\\n'\n")
			}
			raw, err = releasetransition.MarshalJournal(journal)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(request.ConfigHome, "release-transition/v2/journal.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, path, raw, 0o600)
			// This transport fixture checks optional authenticated fact ingress,
			// not Rust history admission or cleanup eligibility.
			session, err := runtime.startRustOwner(context.Background(), verified, verified, verified, request, "")
			if err != nil {
				t.Fatal("missing optional old assets prevented Open", err)
			}
			defer session.Close()
			if available {
				facts := session.options.OriginalSources
				if facts == nil || facts.Source.Release != journal.Releases.From || facts.Previous != nil || len(session.options.originalRoots) != 1 {
					t.Fatal("original source facts lost their exact root or treated expired previous as authority")
				}
				root := session.options.originalRoots[0]
				if _, err := root.Stat(); err != nil {
					t.Fatal("original verified root was not retained", err)
				}
				if err := session.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := root.Stat(); err == nil {
					t.Fatal("closed session leaked original source root")
				}
			} else if session.options.OriginalSources != nil || len(session.options.originalRoots) != 0 {
				t.Fatal("unavailable original assets acquired a proof")
			}
		})
	}
}

func testRustOwnerRealCleanupRetry(t *testing.T, runtime *Runtime, owner, target *verifiedPublishedCandidate, request releasetransition.ProcessRequest, session *rustOwnerSession, inspection releasetransition.Inspection, root, scenario string, releaseDonor func()) {
	t.Helper()
	barrier, err := uninstall.BorrowOwnerLock(session.options.DataHome)
	if err != nil || barrier == nil {
		t.Fatal("original held account OFD unavailable", err)
	}
	defer barrier.Close()
	identity, err := releaseowner.IdentityOf(barrier)
	if err != nil {
		t.Fatal(err)
	}
	inheritedBarrier := strconv.Itoa(int(barrier.Fd()))
	checkContender := func() {
		t.Helper()
		file, err := os.OpenFile(uninstall.OwnerLockPath(session.options.DataHome), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Fatal("donor close released surviving original OFD", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	releaseQuery := func() { testkit.WriteFile(t, filepath.Join(root, "release-cleanup-query"), []byte("release\n"), 0o600) }
	defer releaseQuery()
	type result struct {
		outcome releasetransition.Outcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := session.Execute(ctx, inspection.Plan, "")
		done <- result{outcome, err}
	}()
	for {
		if _, err := os.Stat(filepath.Join(root, "cleanup-query-ready")); err == nil {
			break
		}
		select {
		case early := <-done:
			t.Fatal("actual owner did not reach the readonly terminal cleanup barrier", early.outcome, early.err)
		case <-ctx.Done():
			t.Fatal("actual owner cleanup barrier timed out")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// The duplicate and actual accepted helper retain the same OFD after
	// closing its original donor. Fresh Resume must use this inherited FD.
	releaseDonor()
	checkContender()
	archives, err := filepath.Glob(filepath.Join(request.ConfigHome, "yards/.owner-migration-registration-archive.*"))
	if err != nil || len(archives) != 1 {
		t.Fatal("actual owning Commit did not retain exactly one original registration archive", err)
	}
	archive := archives[0]
	if info, err := os.Stat(archive); err == nil && info.IsDir() {
		archive = filepath.Join(archive, "config.env")
	}
	original, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(request.ConfigHome, "release-transition/v2/journal.json")
	completed, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(completed)
	if err != nil || journal.Checkpoint != releasetransition.JournalComplete {
		t.Fatal("cleanup did not follow actual durable completion", err)
	}
	if scenario == "cleanup-helper-loss" {
		pid := session.command.Process.Pid
		children, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", strconv.Itoa(pid), "children"))
		if err != nil || len(strings.Fields(string(children))) != 1 {
			t.Fatal("exact Rust owner helper child unavailable", err)
		}
		helperPID, err := strconv.Atoi(strings.Fields(string(children))[0])
		if err != nil {
			t.Fatal(err)
		}
		process, err := os.FindProcess(helperPID)
		if err != nil || process.Kill() != nil {
			t.Fatal("cannot kill owned native helper at readonly cleanup query", err)
		}
	} else {
		// The real synchronous archive removal fails on its original parent.
		// Read-only observation remains possible; no fake terminal is supplied.
		if os.Geteuid() == 0 {
			t.Fatal("local archive permission failure requires the ordinary execution UID")
		}
		if err := os.Chmod(filepath.Join(request.ConfigHome, "yards"), 0o500); err != nil {
			t.Fatal(err)
		}
	}
	releaseQuery()
	first := <-done
	if scenario == "cleanup-helper-loss" {
		if first.err == nil && first.outcome.ReachedGoal {
			t.Fatal("lost helper manufactured cleanup completion")
		}
	} else if first.err != nil || !first.outcome.ReachedGoal || len(session.retainedReleases) != 3 {
		t.Fatal("known local cleanup failure lost Ready/retention semantics", first.outcome, first.err)
	}
	_ = session.Close()
	checkContender()
	if err := os.Chmod(filepath.Join(request.ConfigHome, "yards"), 0o700); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(archive); err != nil || string(after) != string(original) {
		t.Fatal("failed or lost cleanup changed the retained original archive", err)
	}
	older := filepath.Join(request.RuntimeRoot, "releases", string(journal.Releases.From)+"-previous")
	if scenario == "cleanup-missing-previous" {
		expired := filepath.Join(root, "temporarily-unavailable-previous")
		if err := os.Rename(older, expired); err != nil {
			t.Fatal(err)
		}
		_, err := runtime.invokeRustOwnerTransition(context.Background(), owner, owner, target, request, nil, "")
		var selected selectedRustOwnerError
		if err == nil || !errors.As(err, &selected) || allowsCompletedMetadataAdmission(nil, err) {
			t.Fatal("pending cleanup lost selected owner on missing original previous proof", err)
		}
		if err := os.Rename(expired, older); err != nil {
			t.Fatal(err)
		}
	}
	wrongGoal, err := runtime.startRustOwner(context.Background(), owner, owner, target, request, "")
	if err != nil {
		t.Fatal(err)
	}
	_, wrongErr := wrongGoal.exchange(context.Background(), map[string]any{"op": "inspect", "goal": releasetransition.Goal{Target: journal.Releases.From, Direction: request.Direction}}, rustOwnerStartupLimit)
	wrongGoal.Close()
	if wrongErr == nil {
		t.Fatal("pending cleanup silently retargeted the requested operation")
	}
	if raw, err := os.ReadFile(journalPath); err != nil || string(raw) != string(completed) {
		t.Fatal("wrong goal changed original completed journal", err)
	}
	if after, err := os.ReadFile(archive); err != nil || string(after) != string(original) {
		t.Fatal("wrong goal changed original owner archive", err)
	}
	resume, err := runtime.startRustOwner(context.Background(), owner, owner, target, request, "")
	if err != nil {
		t.Fatal("pending cleanup fresh Open", err)
	}
	defer resume.Close()
	pending, err := resume.Inspect(context.Background(), nil)
	if err != nil || !resume.matchesRustAdmission(pending) || pending.Resume == nil || *pending.Resume != journal.Transaction || pending.Plan != journal.ResumePlan || len(resume.retainedReleases) != 3 {
		t.Fatal("pending cleanup did not retain original Rust admission", err)
	}
	if scenario == "cleanup-source-drift" {
		path := filepath.Join(older, "config/yards/README.md")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, append(append([]byte{}, body...), '\n'), 0o644)
		resume.grant = ""
		if outcome, err := resume.Execute(context.Background(), pending.Plan, inheritedBarrier); err == nil && outcome.ReachedGoal {
			t.Fatal("changed original source bytes admitted terminal cleanup")
		}
		resume.Close()
		if after, err := os.ReadFile(archive); err != nil || string(after) != string(original) {
			t.Fatal("source drift refusal changed owner archive", err)
		}
		testkit.WriteFile(t, path, body, 0o644)
		resume, err = runtime.startRustOwner(context.Background(), owner, owner, target, request, "")
		if err != nil {
			t.Fatal(err)
		}
		defer resume.Close()
		pending, err = resume.Inspect(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	// No fresh authorization is available. The actual private Resume consumes
	// the Rust-issued original ticket and duplicates the still-held account OFD.
	resume.grant = ""
	outcome, err := resume.Execute(context.Background(), pending.Plan, inheritedBarrier)
	if err != nil || !outcome.ReachedGoal {
		t.Fatal("actual owning cleanup retry did not reach Ready", outcome, err)
	}
	if _, err := os.Stat(archives[0]); !os.IsNotExist(err) {
		t.Fatal("actual owning cleanup retry retained original archive", err)
	}
	if raw, err := os.ReadFile(journalPath); err != nil || string(raw) != string(completed) {
		t.Fatal("terminal cleanup rewrote frozen completed journal", err)
	}
	if directory := os.Getenv("SUBYARD_RUST_DELIVERY_EVIDENCE_DIR"); directory != "" {
		evidence := struct {
			Scenario              string
			OriginalJournalSHA256 string
			ArchiveSHA256         string
			OriginalLock          releaseowner.Identity
			DonorClosed           bool
			FreshGrant            bool
			ResumePlan            releasetransition.PlanToken
			Transaction           string
			ArchiveRemoved        bool
		}{scenario, fmt.Sprintf("%x", sha256.Sum256(completed)), fmt.Sprintf("%x", sha256.Sum256(original)), identity, true, false, pending.Plan, string(journal.Transaction), true}
		raw, err := json.MarshalIndent(evidence, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(directory, "cleanup-"+scenario+"-observations.json"), append(raw, '\n'), 0600)
	}

}

func TestRustRetainedReferencesRequireExplicitClosedList(t *testing.T) {
	for _, item := range []struct {
		name  string
		refs  []releasetransition.ReleaseID
		valid bool
	}{
		{"missing", nil, false}, {"empty", []releasetransition.ReleaseID{}, true},
		{"exact", []releasetransition.ReleaseID{"a", "b", "c"}, true},
		{"duplicate", []releasetransition.ReleaseID{"a", "a"}, false},
		{"unsorted", []releasetransition.ReleaseID{"b", "a"}, false},
		{"path", []releasetransition.ReleaseID{"../a"}, false},
		{"oversized", []releasetransition.ReleaseID{"a", "b", "c", "d"}, false},
	} {
		t.Run(item.name, func(t *testing.T) {
			if err := validateRustRetainedReleases(item.refs); (err == nil) != item.valid {
				t.Fatal("retained reference validation", err)
			}
		})
	}
}

func TestRustSelectedOwnerErrorsCannotEnterCompletedMetadataAdmission(t *testing.T) {
	for _, scenario := range []string{"scope-mismatch", "missing-source-proof", "corrupt-owner", "transport"} {
		t.Run(scenario, func(t *testing.T) {
			runtime, verified, request := transportRustSessionFixture(t)
			switch scenario {
			case "scope-mismatch":
				request.Target = "unbound-original"
			case "missing-source-proof":
				if err := os.Rename(filepath.Join(request.RuntimeRoot, "releases/1.2.3-old"), filepath.Join(request.RuntimeRoot, "unavailable-original-source")); err != nil {
					t.Fatal(err)
				}
			case "corrupt-owner":
				verified.releaseOwner.Close()
			case "transport":
				raw := []byte("#!/usr/bin/python3\nraise SystemExit(91)\n")
				path := filepath.Join(request.RuntimeRoot, "broken-owner")
				testkit.WriteFile(t, path, raw, 0700)
				file, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				image, err := sealVerifiedEngine(file, sha256.Sum256(raw))
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
				verified.releaseOwner.Close()
				verified.releaseOwner = image
			}
			_, err := runtime.invokeRustOwnerTransition(context.Background(), verified, verified, verified, request, nil, "")
			var selected selectedRustOwnerError
			if err == nil || !errors.As(err, &selected) {
				t.Fatalf("selected Rust failure lost its contract: %v", err)
			}
			calls := 0
			if allowsCompletedMetadataAdmission(nil, redactReleaseInspectionError(fmt.Errorf("inspection: %w", err))) {
				calls++
			}
			if calls != 0 {
				t.Fatal("Rust failure allowed Go metadata/new-operation fallback")
			}
			if verified.candidate.release != "1.2.3-new" || request.Direction != releasetransition.DirectionActivateTarget {
				t.Fatal("selected context was replaced")
			}
		})
	}
	protected := &protectedTransitionInspection{rustOwner: true, journal: releasetransition.JournalRecord{Checkpoint: releasetransition.JournalComplete}, inspection: releasetransition.Inspection{Outcome: &releasetransition.Outcome{Status: releasetransition.StatusOperatorActionRequired}}}
	if allowsCompletedMetadataAdmission(protected, nil) {
		t.Fatal("blocked Rust owner allowed metadata fallback")
	}
	protected.rustOwner = false
	if !allowsCompletedMetadataAdmission(protected, nil) || !allowsCompletedMetadataAdmission(nil, errors.New("historical owner unavailable")) {
		t.Fatal("historical completed compatibility reader was removed")
	}
}

func TestRustVerifiedDeclarationMarksSubsequentImageFailure(t *testing.T) {
	root := testkit.TempDir(t)
	candidate := writeReleaseOwnerCandidate(t, root, "#!/bin/sh\nprintf 'yard-engine 1.2.3\\n'\n")
	// Leave the exact closed declaration and inventory intact; corrupt its image.
	testkit.WriteFile(t, filepath.Join(candidate.root, releaseOwnerImagePath), []byte("corrupt image\n"), 0755)
	runtime := New(Config{})
	defer runtime.Close()
	_, err := runtime.verifyPublishedCandidate(context.Background(), candidate, root, nil)
	var selected selectedRustOwnerError
	if err == nil || !errors.As(err, &selected) || allowsCompletedMetadataAdmission(nil, err) {
		t.Fatalf("verified Rust declaration did not retain image failure: %v", err)
	}
}
