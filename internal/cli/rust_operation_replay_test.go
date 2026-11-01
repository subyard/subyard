package cli

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

type operationReplayRow struct {
	negative                 string
	pending                  string
	Name                     string                                   `json:"name"`
	Registry                 []byte                                   `json:"registry"`
	Catalog                  []releasetransition.CapabilityDescriptor `json:"catalog"`
	Initial                  releasetransition.PlanFacts              `json:"initial"`
	Files                    map[string][]byte                        `json:"files"`
	FinalFiles               map[string][]byte                        `json:"finalFiles"`
	Candidate                map[string][]byte                        `json:"candidate"`
	Native                   json.RawMessage                          `json:"native"`
	Calls                    []string                                 `json:"calls"`
	Journals                 []releasetransition.JournalRecord        `json:"journals"`
	Outcome                  releasetransition.Outcome                `json:"outcome"`
	OwnerReads               []operationOwnerRead                     `json:"ownerReads"`
	PostEffectPrepareRefused bool                                     `json:"postEffectPrepareRefused"`
}
type operationOwnerRead struct {
	Method      string                   `json:"method"`
	Before      releaseowner.OwnerBefore `json:"before"`
	Token       string                   `json:"token"`
	Progress    string                   `json:"progress"`
	AfterEffect bool                     `json:"afterEffect"`
	Yards       []string                 `json:"yards,omitempty"`
}
type operationReplayCapture struct {
	Journals    []releasetransition.JournalRecord `json:"journals"`
	RawJournals [][]byte                          `json:"rawJournals"`
	Files       map[string][]byte                 `json:"files"`
	FinalFiles  map[string][]byte                 `json:"finalFiles"`
	Plan        releasetransition.PlanFacts       `json:"plan"`
	Calls       []string                          `json:"calls"`
	Events      []string                          `json:"events"`
	Outcome     *releasetransition.Outcome        `json:"outcome"`
	Error       string                            `json:"error"`
	ExitCode    int                               `json:"exitCode"`
	OwnerReads  []operationOwnerRead              `json:"ownerReads"`
}

func operationReplayDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func operationReplayWrite(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, append(raw, '\n'), 0o600)
}
func operationReplayFiles(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "migrations/update.lock" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err == nil {
			files[filepath.ToSlash(relative)] = raw
		}
		return err
	})
	return files, err
}
func operationReplayBundle(home, transaction string) (releasetransition.PlanFacts, error) {
	directory := filepath.Join(home, "migrations/native-custody/v1", transaction)
	raw, err := os.ReadFile(filepath.Join(directory, "bundle.json"))
	if err != nil {
		return releasetransition.PlanFacts{}, err
	}
	var index struct {
		Length int      `json:"length"`
		Digest string   `json:"digest"`
		Pieces []string `json:"pieces"`
	}
	if err = json.Unmarshal(raw, &index); err != nil {
		return releasetransition.PlanFacts{}, err
	}
	whole := []byte{}
	for n, digest := range index.Pieces {
		raw, err := os.ReadFile(filepath.Join(directory, fmt.Sprintf("bundle.%03d.json", n)))
		if err != nil {
			return releasetransition.PlanFacts{}, err
		}
		if operationReplayDigest(raw) != digest {
			return releasetransition.PlanFacts{}, fmt.Errorf("piece digest")
		}
		var piece struct {
			Data string `json:"data"`
		}
		if err = json.Unmarshal(raw, &piece); err != nil {
			return releasetransition.PlanFacts{}, err
		}
		data, err := base64.StdEncoding.DecodeString(piece.Data)
		if err != nil {
			return releasetransition.PlanFacts{}, err
		}
		whole = append(whole, data...)
	}
	if len(whole) != index.Length || operationReplayDigest(whole) != index.Digest {
		return releasetransition.PlanFacts{}, fmt.Errorf("bundle digest")
	}
	var bundle struct {
		Plan releasetransition.PlanFacts `json:"plan"`
	}
	err = json.Unmarshal(whole, &bundle)
	return bundle.Plan, err
}

// The source corpus comes from the actual Go V2Transition.Converge. The peer
// supplies captured component facts through the normal scoped protocol; it
// receives no Go plan, inspection, journal or outcome. This fixture proves
// owner policy and durable ordering, not physical SSH/profile effect fidelity.
func TestRustOwnerWholeOperationReplay(t *testing.T) {
	testRustOwnerWholeOperationReplay(t, false)
}
func TestRustOwnerPendingOwnerObservation(t *testing.T) {
	testRustOwnerWholeOperationReplay(t, true)
}
func testRustOwnerWholeOperationReplay(t *testing.T, pending bool) {
	owner, peer, corpus, out := os.Getenv("SUBYARD_RUST_FIXTURE_OWNER"), os.Getenv("SUBYARD_RUST_OPERATION_PEER"), os.Getenv("SUBYARD_RUST_OPERATION_TRACE_CORPUS"), os.Getenv("SUBYARD_RUST_OPERATION_REPLAY_OUTPUT")
	if owner == "" || peer == "" || corpus == "" || out == "" {
		t.Skip("opt-in actual Go/Rust whole-operation replay")
	}
	raw, err := os.ReadFile(corpus)
	if err != nil {
		t.Fatal(err)
	}
	var rows []operationReplayRow
	if err = json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	required := []string{"success", "apply-error-after-effect", "failed-reobserve", "changed-final-observation", "cleanup-failure", "owner-cleanup-success", "owner-cleanup-failure", "owner-original-progress"}
	names := []string{}
	for _, row := range rows {
		names = append(names, row.Name)
	}
	operationReplayEqual(t, "required actual operation vectors", required, names)
	// These component capture variants preserve the frozen semantic before and
	// public trajectory, while exercising distinct Rust prepared programs.
	if !pending {
		for _, instance := range []string{"absent", "stopped"} {
			row := rows[7]
			var facts map[string]json.RawMessage
			if err := json.Unmarshal(row.Native, &facts); err != nil {
				t.Fatal(err)
			}
			var before releaseowner.OwnerBefore
			if err := json.Unmarshal(facts["owner"], &before); err != nil {
				t.Fatal(err)
			}
			before.Capture = &releaseowner.OwnerCapture{Instance: instance}
			facts["owner"], err = json.Marshal(before)
			if err != nil {
				t.Fatal(err)
			}
			row.Native, err = json.Marshal(facts)
			if err != nil {
				t.Fatal(err)
			}
			row.Name = "owner-original-" + instance
			rows = append(rows, row)
		}
	}
	for _, mode := range []string{"changed-source-bytes", "changed-source-identity"} {
		negative := rows[0]
		negative.Name = mode
		negative.negative = mode
		rows = append(rows, negative)
	}
	if pending {
		original := rows
		rows = nil
		for _, fault := range []string{"absent", "current", "legacy-drift", "ambiguous", "foreign-pending", "corrupt-bundle", "source-drift", "original-not-started"} {
			row := original[1]
			if fault == "original-not-started" {
				row = original[7]
			}
			row.Name = "pending-" + fault
			row.pending = fault
			var facts map[string]any
			if err = json.Unmarshal(row.Native, &facts); err != nil {
				t.Fatal(err)
			}
			facts["name"] = "apply-error-after-effect"
			facts["recoveryFault"] = fault
			row.Native, err = json.Marshal(facts)
			if err != nil {
				t.Fatal(err)
			}
			rows = append(rows, row)
		}
	}
	if err = os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		t.Run(row.Name, func(t *testing.T) {
			if len(row.Native) == 0 {
				t.Fatal("corpus lacks immutable native component facts")
			}
			root := testkit.TempDir(t)
			home, runtime, data, repository := filepath.Join(root, "state"), filepath.Join(root, "runtime"), filepath.Join(root, "data"), filepath.Join(root, "candidate")
			for _, path := range []string{home, data, repository, filepath.Join(runtime, "releases", string(row.Initial.Releases.From)), filepath.Join(runtime, "releases", string(row.Initial.Releases.Target))} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for relative, raw := range row.Files {
				path := filepath.Join(home, relative)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, raw, 0o600)
			}
			for relative, raw := range row.Candidate {
				path := filepath.Join(repository, "config", relative)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, raw, 0o600)
			}
			if err := os.Symlink("releases/"+string(row.Initial.Links.Active), filepath.Join(runtime, "current")); err != nil {
				t.Fatal(err)
			}
			if row.Initial.Links.Previous != nil {
				if err := os.Symlink("releases/"+string(*row.Initial.Links.Previous), filepath.Join(runtime, "previous")); err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, filepath.Join(root, "operation-peer-input.json"), row.Native, 0o600)
			if row.negative == "changed-source-identity" {
				testkit.WriteFile(t, filepath.Join(home, "config.env"), []byte("UNRELATED=stable\n"), 0o600)
			}
			process := startRustOwnerWithBudget(t, owner, peer, os.Environ(), repository, true, 30*time.Second)
			defer process.close()
			// This captured-policy fixture uses only the compile-time fixture owner:
			// release artifact tuples and source-checkpoint assets are omitted.
			// Normal sealed helper-image/root/OFD custody checks still execute.
			options := map[string]any{"dataHome": data, "configHome": home, "runtimeRoot": runtime, "repositoryRoot": repository, "yard": "default", "releases": row.Initial.Releases, "artifactDigest": row.Initial.ArtifactDigest, "registryRaw": row.Registry, "catalog": row.Catalog, "inheritedSettingIds": []string{}, "rollbackTarget": nil, "migrationCheckpoint": false, "sourceIngress": nil}
			if reply := process.exchange(map[string]any{"op": "open", "options": options}); reply.Error != "" {
				t.Fatalf("open: %s", reply.Error)
			}
			inspection := process.exchange(map[string]any{"op": "inspect", "goal": row.Initial.Goal})
			if inspection.Error != "" || inspection.Inspection == nil {
				t.Fatalf("inspect: %#v", inspection)
			}
			if len(inspection.Inspection.Blockers) != 0 {
				t.Fatalf("blocked: %#v", inspection.Inspection.Blockers)
			}
			if row.negative != "" {
				path := filepath.Join(home, "config.env")
				contents := []byte("UNRELATED=changed\n")
				if row.negative == "changed-source-identity" {
					contents = []byte("UNRELATED=stable\n")
					if err := os.Rename(path, path+".original"); err != nil {
						t.Fatal(err)
					}
				}
				testkit.WriteFile(t, path, contents, 0o600)
			}
			capture := operationReplayCapture{Files: map[string][]byte{}}
			controlDone := make(chan error, 1)
			callCursor := 0
			drainEffects := func() error {
				raw, err := os.ReadFile(filepath.Join(root, "component-calls.jsonl"))
				if os.IsNotExist(err) {
					return nil
				}
				if err != nil {
					return err
				}
				decoder := json.NewDecoder(bytes.NewReader(raw))
				calls := []string{}
				for {
					var call string
					err := decoder.Decode(&call)
					if err == io.EOF {
						break
					}
					if err != nil {
						return err
					}
					calls = append(calls, call)
				}
				for _, call := range calls[callCursor:] {
					if operationReplayEffect(call) {
						capture.Events = append(capture.Events, call)
					}
				}
				callCursor = len(calls)
				return nil
			}
			go func() {
				reader := bufio.NewReader(process.control)
				for {
					label, err := reader.ReadString('\n')
					if err != nil {
						if err == io.EOF {
							controlDone <- nil
						} else {
							controlDone <- err
						}
						return
					}
					label = strings.TrimSpace(label)
					if label == "after-journal-publication" || label == "after-authorized-journal" || label == "after-complete-journal" {
						raw, err := os.ReadFile(filepath.Join(home, "release-transition/v2/journal.json"))
						if err != nil {
							controlDone <- err
							return
						}
						if len(capture.RawJournals) == 0 || !bytes.Equal(raw, capture.RawJournals[len(capture.RawJournals)-1]) {
							journal, err := releasetransition.ParseJournal(raw)
							if err != nil {
								controlDone <- err
								return
							}
							capture.Journals = append(capture.Journals, journal)
							if err := drainEffects(); err != nil {
								controlDone <- err
								return
							}
							capture.Events = append(capture.Events, "journal."+string(journal.Checkpoint))
							capture.RawJournals = append(capture.RawJournals, raw)
							files, err := operationReplayFiles(home)
							if err != nil {
								controlDone <- err
								return
							}
							for path, raw := range files {
								capture.Files[path] = raw
							}
							if capture.Plan.ArtifactDigest == "" {
								capture.Plan, err = operationReplayBundle(home, string(journal.Transaction))
								if err != nil {
									controlDone <- err
									return
								}
							}
						}
					}
					if label == "after-complete-journal" && row.Name == "changed-final-observation" {
						if err := os.Remove(filepath.Join(runtime, "current")); err != nil {
							controlDone <- err
							return
						}
						if err := os.Symlink("releases/"+string(row.Initial.Releases.From), filepath.Join(runtime, "current")); err != nil {
							controlDone <- err
							return
						}
						if err := os.Remove(filepath.Join(runtime, "previous")); err != nil {
							controlDone <- err
							return
						}
					}
					response := []byte("R")
					if label == "before-ready-cleanup" && row.Name == "cleanup-failure" {
						response = []byte("E")
					}
					if _, err := process.control.Write(response); err != nil {
						controlDone <- err
						return
					}
				}
			}()
			if err := process.encoder.Encode(map[string]any{"op": "execute", "plan": inspection.Inspection.Plan, "authorization": "fixture-private-consent"}); err != nil {
				t.Fatal(err)
			}
			process.admit(inspection.Inspection.Plan, "")
			var reply rustOwnerReply
			decodeErr := process.decoder.Decode(&reply)
			capture.Outcome = reply.Outcome
			capture.Error = reply.Error
			process.socket.Close()
			waitErr := process.cmd.Wait()
			if process.cmd.ProcessState != nil {
				capture.ExitCode = process.cmd.ProcessState.ExitCode()
			}
			process.control.Close()
			controlErr := <-controlDone
			if err := drainEffects(); err != nil {
				t.Error(err)
			}
			capture.FinalFiles, err = operationReplayFiles(home)
			if err != nil {
				t.Fatal(err)
			}
			if controlErr != nil && !strings.Contains(controlErr.Error(), "closed network") {
				t.Error(controlErr)
			}
			callsRaw, err := os.ReadFile(filepath.Join(root, "component-calls.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(bytes.NewReader(callsRaw))
			for {
				var call string
				if err := decoder.Decode(&call); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				capture.Calls = append(capture.Calls, call)
			}
			readsRaw, err := os.ReadFile(filepath.Join(root, "owner-reads.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			readsDecoder := json.NewDecoder(bytes.NewReader(readsRaw))
			for {
				var read operationOwnerRead
				err := readsDecoder.Decode(&read)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				capture.OwnerReads = append(capture.OwnerReads, read)
			}
			operationReplayWrite(t, filepath.Join(out, row.Name+"-actual.json"), capture)
			if decodeErr != nil {
				t.Fatalf("owner reply: %v (%s)", decodeErr, process.stderr.String())
			}
			if row.pending != "" {
				if waitErr != nil || reply.Error != "" || reply.Outcome == nil {
					t.Fatalf("pending observation failed: error=%s exit=%v", reply.Error, waitErr)
				}
				safe := row.pending == "absent" || row.pending == "current" || row.pending == "original-not-started" || row.pending == "source-drift"
				want := releasetransition.StatusOperatorActionRequired
				if safe {
					want = releasetransition.StatusRecovering
				}
				if reply.Outcome.Status != want {
					t.Fatalf("pending %s: status=%s want=%s reason=%s", row.pending, reply.Outcome.Status, want, reply.Outcome.Code)
				}
				after := false
				effects := 0
				for _, call := range capture.Calls {
					if call == "settings.apply" {
						after = true
						effects++
					}
					if after && (call == "owner.prepare" || call == "owner.apply" || call == "owner.cleanup" || strings.HasPrefix(call, "component.apply.")) {
						t.Fatal("pending observation prepared or applied new authority", call)
					}
				}
				if effects != 1 || len(capture.Journals) == 0 {
					t.Fatal("counterexample missed original settings effect")
				}
				path := "yards/hermes/config.env"
				if bytes.Equal(capture.FinalFiles[path], row.Files[path]) {
					t.Fatal("counterexample did not apply original settings CAS")
				}
				if row.pending == "foreign-pending" {
					if _, err := releasetransition.ParseJournal(capture.FinalFiles["release-transition/v2/journal.json"]); err != nil {
						t.Fatal("foreign journal was not well formed", err)
					}
				}
				if row.pending == "source-drift" {
					// Recovery reports the durable checkpoint; a fresh Resume must
					// still use the original recipe and refuse its changed readset.
					originalFiles := capture.FinalFiles
					originalModes := map[string]os.FileMode{}
					for path := range originalFiles {
						info, err := os.Stat(filepath.Join(home, path))
						if err != nil {
							t.Fatal(err)
						}
						originalModes[path] = info.Mode()
					}
					resumed := startRustOwnerWithBudget(t, owner, peer, os.Environ(), repository, false, 30*time.Second)
					defer resumed.close()
					if opened := resumed.exchange(map[string]any{"op": "open", "options": options}); opened.Error != "" {
						t.Fatal("source guard resume open", opened.Error)
					}
					result := resumed.exchange(map[string]any{"op": "resume"})
					resumed.socket.Close()
					resumeExit := resumed.cmd.Wait()
					if result.Error == "" && (result.Outcome == nil || result.Outcome.Status == releasetransition.StatusReady || result.Outcome.ReachedGoal) {
						t.Fatal("stale original source reached ready", result.Outcome)
					}
					if resumeExit != nil {
						t.Fatal("source guard resume process", resumeExit)
					}
					afterFiles, err := operationReplayFiles(home)
					if err != nil {
						t.Fatal(err)
					}
					operationReplayEqual(t, "source refusal retained exact journal/recipe/evidence/operator bytes", originalFiles, afterFiles)
					for path, mode := range originalModes {
						info, err := os.Stat(filepath.Join(home, path))
						if err != nil || info.Mode() != mode {
							t.Fatal("source refusal changed protected file mode", path, err)
						}
					}
					raw, err := os.ReadFile(filepath.Join(root, "component-calls.jsonl"))
					if err != nil {
						t.Fatal(err)
					}
					decoder := json.NewDecoder(bytes.NewReader(raw))
					calls := []string{}
					for {
						var call string
						err := decoder.Decode(&call)
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
						calls = append(calls, call)
					}
					guard := false
					for _, call := range calls[len(capture.Calls):] {
						if call == "settings.source-refused" {
							guard = true
						}
						if operationReplayEffect(call) {
							t.Fatal("stale source reached additional effect", call)
						}
					}
					if !guard {
						t.Fatal("fresh Resume did not exercise the original recipe source guard")
					}
					operationReplayWrite(t, filepath.Join(out, row.Name+"-resume.json"), map[string]any{"reply": result, "calls": calls[len(capture.Calls):], "files": afterFiles, "modes": originalModes, "exitCode": resumed.cmd.ProcessState.ExitCode()})
				}
				return
			}
			if row.negative != "" {
				exit, exited := waitErr.(*exec.ExitError)
				if !exited || exit.ExitCode() != 1 || reply.Error != "stale" || reply.Outcome != nil || len(capture.Journals) != 0 {
					t.Fatalf("changed native source was admitted: error=%s outcome=%#v journals=%d exit=%v", reply.Error, reply.Outcome, len(capture.Journals), waitErr)
				}
				for _, call := range capture.Calls {
					if operationReplayEffect(call) {
						t.Fatalf("changed source reached effect %s", call)
					}
				}
				if _, err := os.Stat(filepath.Join(home, "release-transition/v2/journal.json")); !os.IsNotExist(err) {
					t.Fatal("changed source published durable journal")
				}
				return
			}
			if waitErr != nil || reply.Error != "" || reply.Outcome == nil {
				t.Fatalf("execute error=%s exit=%v stderr=%s", reply.Error, waitErr, process.stderr.String())
			}
			if len(capture.Journals) == 0 {
				t.Fatal("no durable journal publications")
			}
			bound, err := releasetransition.BindPlan(capture.Plan)
			if err != nil {
				t.Fatal(err)
			}
			if bound != inspection.Inspection.Plan || capture.Journals[0].AuthorizationPlan != bound {
				t.Fatal("Go BindPlan differs over exact Rust native-bound PlanFacts")
			}
			first := capture.Journals[0]
			resume, err := releasetransition.BindResumePlan(releasetransition.ResumePlanFacts{Goal: capture.Plan.Goal, Releases: capture.Plan.Releases, ArtifactDigest: capture.Plan.ArtifactDigest, RegistryDigest: capture.Plan.RegistryDigest, CatalogDigest: capture.Plan.CatalogDigest, RollbackTarget: capture.Plan.RollbackTarget, ObservationScope: capture.Plan.ObservationScope, Assessment: capture.Plan.Assessment, Decisions: capture.Plan.Decisions, Intents: capture.Plan.Intents, Blockers: []releasetransition.Blocker{}, Transaction: first.Transaction, AuthorizationPlan: bound})
			if err != nil || resume != first.ResumePlan {
				t.Fatalf("Go BindResumePlan differs: %v", err)
			}
			if string(first.AuthorizationDigest) != operationReplayDigest([]byte("fixture-private-consent")) {
				t.Fatal("grant digest does not bind inherited private pipe")
			}
			operationReplayEqual(t, "assessment", row.Initial.Assessment, capture.Plan.Assessment)
			operationReplayEqual(t, "decisions", row.Initial.Decisions, capture.Plan.Decisions)
			operationReplayEqual(t, "work", row.Initial.Intents, capture.Plan.Intents)
			operationReplayEqual(t, "observation scope", row.Initial.ObservationScope, capture.Plan.ObservationScope)
			authority, present := row.Files["release-transition/v2/history-checkpoint.json"]
			if !present {
				authority, present = row.Files["release-transition/v2/ledger.json"]
			}
			if !present {
				authority = []byte("protected-record-absent-v1")
			}
			authorityProofs := 0
			observations := []releasetransition.ResourceObservation{}
			for _, observation := range capture.Plan.Observations {
				if observation.Resource == "migration-history.authority" {
					authorityProofs++
					if observation.Class != "validated-migration-authority-v1" || string(observation.Fingerprint) != operationReplayDigest(authority) {
						t.Fatal("Rust authority observation does not bind the original seeded snapshot")
					}
				} else if observation.Resource != "native.yard-set" && observation.Resource != "native.prepared-recipes" {
					observations = append(observations, observation)
				}
			}
			if authorityProofs != 1 {
				t.Fatal("Rust authority observation is missing or duplicated")
			}
			operationReplayEqual(t, "all semantic/source observations", row.Initial.Observations, observations)
			if len(capture.Plan.Observations) != len(observations)+3 {
				t.Error("missing distinct native source bindings")
			}
			wantOutcome, gotOutcome := row.Outcome, *reply.Outcome
			if wantOutcome.Transaction == nil || gotOutcome.Transaction == nil || *gotOutcome.Transaction != first.Transaction {
				t.Fatal("frozen outcome lost its original durable transaction")
			}
			// Transaction IDs are independently generated entropy. No public outcome
			// field besides this correlation ID is canonicalized.
			wantOutcome.Transaction = nil
			gotOutcome.Transaction = nil
			operationReplayEqual(t, "full frozen outcome", wantOutcome, gotOutcome)
			for path, want := range row.FinalFiles {
				if strings.HasPrefix(path, "yards/") || path == "config.env" || path == "overrides/shared/config.env" {
					if !bytes.Equal(want, capture.FinalFiles[path]) {
						t.Errorf("actual persistent configuration differs at %s", path)
					}
				}
			}
			if row.Name == "apply-error-after-effect" || row.Name == "failed-reobserve" {
				path := "yards/hermes/config.env"
				if bytes.Equal(capture.FinalFiles[path], row.Files[path]) {
					t.Fatal("fault did not occur after the actual settings effect")
				}
				failed := false
				for path, raw := range capture.FinalFiles {
					if !strings.HasSuffix(path, ".terminal.json") {
						continue
					}
					var terminal releaseowner.Terminal
					if json.Unmarshal(raw, &terminal) != nil {
						continue
					}
					if terminal.Owner == "settings" && terminal.State == "failed" && len(terminal.Operations) == 1 && terminal.Operations[0].Status == 400 {
						failed = true
					}
				}
				if !failed {
					t.Fatal("apply failure lacks an actual durable closed failure witness")
				}
			}
			operationReplayJournals(t, row, capture)
			wantEvents := []string{}
			for _, call := range row.Calls {
				if strings.HasPrefix(call, "journal.") || operationReplayEffect(call) {
					wantEvents = append(wantEvents, call)
				}
				if call == "checkpoint.after-settings-cas" {
					wantEvents = append(wantEvents, "settings.apply")
				}
			}
			operationReplayEqual(t, "durable publication/component effect order", wantEvents, capture.Events)
			if strings.HasPrefix(row.Name, "owner-cleanup-") || row.Name == "owner-original-progress" {
				operationReplayOwnerReads(t, row, capture)
				found := false
				for path, raw := range capture.FinalFiles {
					if !strings.HasPrefix(path, "migrations/native-custody/v1/"+string(first.Transaction)+"/") || !strings.HasSuffix(path, ".terminal.json") {
						continue
					}
					var terminal releaseowner.Terminal
					if err := json.Unmarshal(raw, &terminal); err != nil {
						t.Fatal(err)
					}
					if terminal.Owner != "owner-registration-cleanup" {
						continue
					}
					found = true
					state := "complete"
					if row.Name == "owner-cleanup-failure" {
						state = "failed"
					}
					if terminal.State != state || terminal.Resource != "test-yard" || len(terminal.Operations) != 0 || terminal.Binding.Transaction != string(first.Transaction) || terminal.Binding.Plan != string(first.AuthorizationPlan) || terminal.Binding.Journal != string(first.IntentDigest) {
						t.Fatal("terminal owner cleanup lost its exact independent proof")
					}
				}
				if !found {
					t.Fatal("distinct terminal owner cleanup invocation did not execute")
				}
				if row.Name == "owner-cleanup-failure" {
					for _, step := range first.Steps {
						if step.Resource != "test-yard-owner" {
							continue
						}
						path := "release-transition/v2/transactions/" + string(first.Transaction) + "/recovery/" + step.ID + ".json"
						if !bytes.Equal(capture.Files[path], capture.FinalFiles[path]) || len(capture.FinalFiles[path]) == 0 {
							t.Fatal("pending cleanup lost its original immutable recovery proof")
						}
					}
				}
			}
		})
	}
}

func operationReplayOwnerReads(t *testing.T, row operationReplayRow, capture operationReplayCapture) {
	t.Helper()
	first := capture.Journals[0]
	var goRecovery, rustRecovery struct {
		Token  string                   `json:"token"`
		Before releaseowner.OwnerBefore `json:"before"`
	}
	for _, step := range first.Steps {
		if step.Resource != "test-yard-owner" {
			continue
		}
		path := "release-transition/v2/transactions/" + string(first.Transaction) + "/recovery/" + step.ID + ".json"
		goPath := "release-transition/v2/transactions/" + string(row.Journals[0].Transaction) + "/recovery/" + step.ID + ".json"
		if err := json.Unmarshal(capture.Files[path], &rustRecovery); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(row.FinalFiles[goPath], &goRecovery); err != nil {
			t.Fatal(err)
		}
	}
	if len(goRecovery.Token) != 32 || len(rustRecovery.Token) != 32 {
		t.Fatal("missing original owner recovery token")
	}
	operationReplayEqual(t, "original owner observation preparation", goRecovery.Before, rustRecovery.Before)
	var native struct {
		ActivationYards []string `json:"activationYards"`
		Yards           []string `json:"yards"`
	}
	if err := json.Unmarshal(row.Native, &native); err != nil {
		t.Fatal(err)
	}
	projected := 0
	for _, read := range capture.OwnerReads {
		if read.Method != "project-activation" {
			continue
		}
		projected++
		read.Before.Capture = nil // Frozen V1 recovery contains component semantics only.
		if read.Before != rustRecovery.Before {
			t.Error("activation projection replaced original owner preparation")
		}
		operationReplayEqual(t, "captured activation-only projection", native.ActivationYards, read.Yards)
	}
	if projected < 2 || len(native.ActivationYards) == 0 || slices.Equal(native.ActivationYards, native.Yards) {
		t.Fatal("distinct captured activation projection was not reconstructed")
	}
	progress := func(reads []operationOwnerRead, token string, before releaseowner.OwnerBefore) []string {
		result := []string{}
		for _, read := range reads {
			if read.Method != "observe" {
				continue
			}
			read.Before.Capture = nil
			if read.Token != token || read.Before != before {
				t.Error("owner progress replaced the original preparation/token")
			}
			if read.Progress != "expected" && read.Progress != "in-progress" && read.Progress != "desired" {
				t.Error("unknown owner progress")
			}
			if read.AfterEffect && read.Progress != "desired" {
				t.Error("owner effect was not observed through original progress")
			}
			if len(result) == 0 || result[len(result)-1] != read.Progress {
				result = append(result, read.Progress)
			}
		}
		return result
	}
	operationReplayEqual(t, "original owner finite progress", progress(row.OwnerReads, goRecovery.Token, goRecovery.Before), progress(capture.OwnerReads, rustRecovery.Token, rustRecovery.Before))
	if row.Name == "owner-original-progress" {
		if !row.PostEffectPrepareRefused {
			t.Fatal("Go owning vector did not prove post-effect fresh-Prepare refusal")
		}
		after := false
		for _, read := range capture.OwnerReads {
			if read.AfterEffect && read.Progress == "desired" {
				after = true
			}
		}
		if !after {
			t.Fatal("Rust operation bypassed original owner progress after effect")
		}
	}
}

func operationReplayEffect(call string) bool {
	return call == "settings.apply" || call == "owner.apply" || call == "owner.cleanup" || strings.HasPrefix(call, "component.apply.")
}

func operationReplayEqual(t *testing.T, label string, want, got any) {
	t.Helper()
	a, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("%s expected JSON: %v", label, err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("%s actual JSON: %v", label, err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("%s mismatch\nGo: %s\nRust: %s", label, a, b)
	}
}
func operationReplayJournals(t *testing.T, row operationReplayRow, actual operationReplayCapture) {
	t.Helper()
	if len(row.Journals) != len(actual.Journals) {
		t.Errorf("durable trajectory length Go=%d Rust=%d", len(row.Journals), len(actual.Journals))
		return
	}
	for i, want := range row.Journals {
		got := actual.Journals[i]
		// Exact native proof is intentionally stronger. Tokens and intent digests
		// bind that unmodified proof and are checked independently with Go's binder.
		// The semantic journal fields, source/owner fingerprints and step order stay.
		want.Transaction = got.Transaction
		want.AuthorizationPlan = got.AuthorizationPlan
		want.ResumePlan = got.ResumePlan
		want.AuthorizationDigest = got.AuthorizationDigest
		want.IntentDigest = got.IntentDigest
		want.Steps = slices.Clone(want.Steps)
		got.Steps = slices.Clone(got.Steps)
		for n := range want.Steps {
			if want.Steps[n].Evidence == nil || got.Steps[n].Evidence == nil {
				continue
			}
			a, b := *want.Steps[n].Evidence, *got.Steps[n].Evidence
			a.Transaction = b.Transaction
			if a.Recovery != "" && b.Recovery != "" {
				path := "release-transition/v2/transactions/" + string(row.Journals[i].Transaction) + "/recovery/" + want.Steps[n].ID + ".json"
				actualPath := "release-transition/v2/transactions/" + string(got.Transaction) + "/recovery/" + got.Steps[n].ID + ".json"
				wantRaw, wantOK := row.FinalFiles[path]
				gotRaw, gotOK := actual.Files[actualPath]
				if !wantOK || !gotOK {
					t.Errorf("missing recovery payload for %s", want.Steps[n].ID)
				} else {
					if operationReplayDigest(wantRaw) != string(a.Recovery) || operationReplayDigest(gotRaw) != string(b.Recovery) {
						t.Error("recovery digest does not bind raw payload")
					}
					var wa, ga map[string]any
					_ = json.Unmarshal(wantRaw, &wa)
					_ = json.Unmarshal(gotRaw, &ga)
					wa["transaction"] = ga["transaction"]
					// Owner archive tokens are also private generated correlation values.
					if _, ok := wa["token"]; ok {
						wa["token"] = ga["token"]
					}
					operationReplayEqual(t, "recovery payload", wa, ga)
				}
				a.Recovery = b.Recovery
			}
			want.Steps[n].Evidence = &a
			got.Steps[n].Evidence = &b
		}
		type comparisonJournal releasetransition.JournalRecord
		operationReplayEqual(t, fmt.Sprintf("durable journal %d", i), comparisonJournal(want), comparisonJournal(got))
	}
}

// The actual owner publishes consent for ledger-only work, then a fresh process
// resumes the same transaction. Component facts are the existing scoped peer;
// no synthetic journal, grant, bundle or native outcome is installed.
func TestRustOwnerResumeAdmissionBeforeLedger(t *testing.T) {
	testRustOwnerResumeAdmission(t, false, "after-authorized-journal")
}
func TestRustOwnerResumeAdmissionBeforeOwner(t *testing.T) {
	testRustOwnerResumeAdmission(t, true, "after-authorized-journal")
}
func TestRustOwnerResumeAdmissionAtOwnerIntent(t *testing.T) {
	testRustOwnerResumeAdmission(t, true, "after-native-intent")
}
func TestRustOwnerResumeAfterCompensatedCheck(t *testing.T) {
	testRustOwnerResumeAdmission(t, true, "compensated-check")
}
func testRustOwnerResumeAdmission(t *testing.T, ownerPending bool, cut string) {
	owner, peer, corpus := os.Getenv("SUBYARD_RUST_FIXTURE_OWNER"), os.Getenv("SUBYARD_RUST_OPERATION_PEER"), os.Getenv("SUBYARD_RUST_OPERATION_TRACE_CORPUS")
	if owner == "" || peer == "" || corpus == "" {
		t.Skip("opt-in actual owner admission fixture")
	}
	raw, err := os.ReadFile(corpus)
	if err != nil {
		t.Fatal(err)
	}
	var rows []operationReplayRow
	if err = json.Unmarshal(raw, &rows); err != nil || len(rows) == 0 {
		t.Fatal("owning corpus", err)
	}
	row := rows[0]
	if ownerPending {
		row = rows[7]
		var facts map[string]json.RawMessage
		if err := json.Unmarshal(row.Native, &facts); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(ownerChildrenWire{Version: 1, Owner: "owner-registration", Source: operationReplayDigest([]byte("original owner source")), Runtime: operationReplayDigest([]byte("original owner runtime")), Resources: operationReplayDigest([]byte("original owner resources"))})
		var before releaseowner.OwnerBefore
		if err := json.Unmarshal(facts["owner"], &before); err != nil {
			t.Fatal(err)
		}
		before.PreparedRecipe = operationReplayDigest(raw)
		facts["owner"], _ = json.Marshal(before)
		facts["ownerRecipe"], _ = json.Marshal(raw)
		facts["requireOwnerProjection"] = json.RawMessage("true")
		if cut == "compensated-check" {
			facts["recoveryFault"], _ = json.Marshal("owner-check-failed")
		}
		row.Native, _ = json.Marshal(facts)
	}
	faults := []string{"unchanged", "source", "desired", "inherited", "extra-sidecar"}
	if ownerPending {
		faults = append(faults, "recipe")
	}
	if cut == "compensated-check" {
		faults = []string{"unchanged"}
	}
	for _, fault := range faults {
		t.Run(fault, func(t *testing.T) {
			root := testkit.TempDir(t)
			home, data, runtime, repository := filepath.Join(root, "state"), filepath.Join(root, "data"), filepath.Join(root, "runtime"), filepath.Join(root, "candidate")
			for _, path := range []string{home, data, repository, filepath.Join(runtime, "releases", string(row.Initial.Releases.From)), filepath.Join(runtime, "releases", string(row.Initial.Releases.Target))} {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for name := range row.Files {
				path := filepath.Join(home, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				value, ok := row.FinalFiles[name]
				if !ok {
					t.Fatal("current fixture configuration absent", name)
				}
				testkit.WriteFile(t, path, value, 0600)
			}
			for name, value := range row.Candidate {
				path := filepath.Join(repository, "config", name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, value, 0600)
			}
			if err := os.Symlink("releases/"+string(row.Initial.Links.Active), filepath.Join(runtime, "current")); err != nil {
				t.Fatal(err)
			}
			if row.Initial.Links.Previous != nil {
				if err := os.Symlink("releases/"+string(*row.Initial.Links.Previous), filepath.Join(runtime, "previous")); err != nil {
					t.Fatal(err)
				}
			}
			input := filepath.Join(root, "operation-peer-input.json")
			testkit.WriteFile(t, input, row.Native, 0600)
			options := map[string]any{"dataHome": data, "configHome": home, "runtimeRoot": runtime, "repositoryRoot": repository, "yard": "default", "releases": row.Initial.Releases, "artifactDigest": row.Initial.ArtifactDigest, "registryRaw": row.Registry, "catalog": row.Catalog, "inheritedSettingIds": []string{}, "rollbackTarget": nil, "migrationCheckpoint": false, "sourceIngress": nil}
			process := startRustOwnerWithBudget(t, owner, peer, os.Environ(), repository, true, 30*time.Second)
			defer process.close()
			if reply := process.exchange(map[string]any{"op": "open", "options": options}); reply.Error != "" {
				t.Fatal("open", reply.Error)
			}
			inspected := process.exchange(map[string]any{"op": "inspect", "goal": row.Initial.Goal})
			if inspected.Error != "" || inspected.Inspection == nil || len(inspected.Inspection.Blockers) != 0 {
				t.Fatal("valid initial inspection", inspected)
			}

			if ownerPending {
				calls, err := os.ReadFile(filepath.Join(root, "component-calls.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"component.reserved.sample-runtime", "component.reserved.test-yard-route-consumers"} {
					if !bytes.Contains(calls, []byte(name)) {
						t.Fatal("initial original reserved view was not captured", name)
					}
				}
			}
			stopped := make(chan bool, 1)
			go func() {
				reader := bufio.NewReader(process.control)
				for {
					label, err := reader.ReadString('\n')
					if err != nil {
						stopped <- false
						return
					}
					if strings.TrimSpace(label) == cut {
						_, err = process.control.Write([]byte("E"))
						stopped <- err == nil
						return
					}
					if _, err = process.control.Write([]byte("R")); err != nil {
						stopped <- false
						return
					}
				}
			}()
			first := process.exchange(map[string]any{"op": "execute", "plan": inspected.Inspection.Plan, "authorization": "fixture-private-consent"})
			process.close()
			cutReached := <-stopped
			if cut == "compensated-check" {
				if first.Error != "" || first.Outcome == nil || first.Outcome.Transaction == nil || first.Outcome.ReachedGoal {
					t.Fatalf("missing interrupted owner transaction: error=%s outcome=%t", first.Error, first.Outcome != nil)
				}
			} else if !cutReached || (cut == "after-authorized-journal" && first.Error == "") || (cut != "after-authorized-journal" && first.Error == "" && (first.Outcome == nil || first.Outcome.ReachedGoal)) {
				t.Fatal("original publication cut not reached", first)
			}
			journalRaw, err := os.ReadFile(filepath.Join(home, "release-transition/v2/journal.json"))
			if err != nil {
				t.Fatal(err)
			}
			journal, err := releasetransition.ParseJournal(journalRaw)
			if err != nil {
				t.Fatal(err)
			}
			if len(journal.Steps) == 0 {
				t.Fatal("no pending ledger work")
			}
			hasOwner := false
			for _, step := range journal.Steps {
				if step.Resource == "test-yard-owner" {
					hasOwner = true
				}
				if (!ownerPending && !strings.HasSuffix(step.ID, ".ledger")) || (cut == "after-authorized-journal" && step.Checkpoint != "intent") {
					t.Fatal("unexpected pending preparation", step.ID)
				}
			}
			if ownerPending && !hasOwner {
				t.Fatal("original owner work not prepared")
			}
			if cut == "compensated-check" {
				files, err := operationReplayFiles(home)
				if err != nil {
					t.Fatal(err)
				}
				failed, edges := 0, 0
				for name, raw := range files {
					if !strings.HasPrefix(name, "migrations/native-custody/v1/"+string(journal.Transaction)+"/") {
						continue
					}
					if strings.HasSuffix(name, ".terminal.json") {
						var terminal releaseowner.Terminal
						if json.Unmarshal(raw, &terminal) == nil && terminal.Owner == "owner-registration" && terminal.State == "failed" && terminal.Binding.Transaction == string(journal.Transaction) {
							failed++
						}
					}
					if strings.HasSuffix(name, ".recovery.json") {
						var edge struct {
							Version     int
							Binding     releaseowner.Binding
							Compensated *struct{ Group releaseowner.Binding }
						}
						if json.Unmarshal(raw, &edge) == nil && edge.Version == 2 && edge.Binding.Transaction == string(journal.Transaction) && edge.Compensated != nil && edge.Compensated.Group.Transaction == string(journal.Transaction) {
							edges++
						}
					}
				}
				calls, err := os.ReadFile(filepath.Join(root, "component-calls.jsonl"))
				if err != nil || failed != 1 || edges != 3 || !bytes.Contains(calls, []byte(`"owner.action.verify-compensated.1"`)) || journal.Transaction != *first.Outcome.Transaction {
					t.Fatalf("missing real compensated attempt: failed=%d retry_edges=%d", failed, edges)
				}
				if first.Outcome.Status != releasetransition.StatusRecovering || first.Outcome.Code != releasetransition.CodeVerificationFailed {
					t.Fatalf("closed compensated check is not recoverable: status=%s code=%s", first.Outcome.Status, first.Outcome.Code)
				}
				var object map[string]json.RawMessage
				if err := json.Unmarshal(row.Native, &object); err != nil {
					t.Fatal(err)
				}
				delete(object, "recoveryFault")
				raw, _ := json.Marshal(object)
				testkit.WriteFile(t, input, raw, 0600)
			}
			if cut == "after-native-intent" {
				for index, step := range journal.Steps {
					if step.Resource != "test-yard-owner" {
						continue
					}
					dir := filepath.Join(home, "migrations/native-custody/v1", string(journal.Transaction))
					prefix := fmt.Sprintf("%03d.", index+1)
					raw, err := os.ReadFile(filepath.Join(dir, prefix+"intent.json"))
					if err != nil {
						t.Fatal("original owner intent", err)
					}
					var intent struct {
						Owner string `json:"owner"`
					}
					if err := json.Unmarshal(raw, &intent); err != nil || intent.Owner != "owner-registration" {
						t.Fatal("wrong native intent", err)
					}
					// The nonempty installed capsule must reach native dispatch only
					// after the original owner recovery token was materialized.
					recoveryRaw, err := os.ReadFile(filepath.Join(home, "release-transition/v2/transactions", string(journal.Transaction), "recovery", step.ID+".json"))
					var recovery struct {
						Token  string                   `json:"token"`
						Before releaseowner.OwnerBefore `json:"before"`
					}
					if err != nil || json.Unmarshal(recoveryRaw, &recovery) != nil || len(recovery.Token) != 32 || strings.Trim(recovery.Token, "0123456789abcdef") != "" || recovery.Before.PreparedRecipe != "" || recovery.Before.Capture != nil || step.Checkpoint != "evidence" {
						t.Fatal("native owner dispatch lacks original prepared recovery authority", err)
					}
					var facts struct {
						Owner       releaseowner.OwnerBefore `json:"owner"`
						OwnerRecipe []byte                   `json:"ownerRecipe"`
					}
					if err := json.Unmarshal(row.Native, &facts); err != nil || facts.Owner.PreparedRecipe == "" || facts.Owner.PreparedRecipe != operationReplayDigest(facts.OwnerRecipe) {
						t.Fatal("missing original installed owner payload", err)
					}
					facts.Owner.PreparedRecipe, facts.Owner.Capture = "", nil
					operationReplayEqual(t, "frozen semantic owner recovery", facts.Owner, recovery.Before)
					for _, kind := range []string{"accepted", "terminal"} {
						if _, err := os.Stat(filepath.Join(dir, prefix+kind+".json")); !os.IsNotExist(err) {
							t.Fatal("pending intent acquired a producer", kind, err)
						}
					}
				}
			}
			if fault == "recipe" {
				var object map[string]json.RawMessage
				if err := json.Unmarshal(row.Native, &object); err != nil {
					t.Fatal(err)
				}
				object["ownerRecipe"], _ = json.Marshal([]byte("changed opaque source"))
				raw, _ := json.Marshal(object)
				testkit.WriteFile(t, input, raw, 0600)
			}
			if fault == "source" || fault == "desired" {
				var facts struct {
					Targets map[string][]releaseowner.Target `json:"targets"`
				}
				if err := json.Unmarshal(row.Native, &facts); err != nil {
					t.Fatal(err)
				}
				values := facts.Targets["sample-runtime"]
				if len(values) == 0 {
					t.Fatal("missing original target")
				}
				if fault == "source" {
					changed := operationReplayDigest([]byte("changed source"))
					if values[0].Source == changed {
						t.Fatal("source mutation is a no-op")
					}
					values[0].Source = changed
				} else {
					changed := operationReplayDigest([]byte("changed desired"))
					if values[0].Desired == changed {
						t.Fatal("desired mutation is a no-op")
					}
					values[0].Desired = changed
				}
				var object map[string]json.RawMessage
				if err := json.Unmarshal(row.Native, &object); err != nil {
					t.Fatal(err)
				}
				object["targets"], err = json.Marshal(facts.Targets)
				if err != nil {
					t.Fatal(err)
				}
				payload, err := json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, input, payload, 0600)
			}
			if fault == "inherited" {
				options["inheritedSettingIds"] = []string{"SSH_PORT"}
			}
			if fault == "extra-sidecar" {
				e := releasetransition.EvidenceRecord{SchemaVersion: 2, Transaction: journal.Transaction, Releases: journal.Releases, Step: "unlisted.ledger", Expected: journal.Steps[0].Expected, Desired: journal.Steps[0].Desired, Observed: journal.Steps[0].Expected, Checkpoint: "captured"}
				path := filepath.Join(home, "release-transition/v2/transactions", string(journal.Transaction), "evidence/unlisted.ledger.captured.json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				operationReplayWrite(t, path, e)
			}
			before, err := operationReplayFiles(home)
			if err != nil {
				t.Fatal(err)
			}
			callsBefore, err := os.ReadFile(filepath.Join(root, "component-calls.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			resumed := startRustOwnerWithBudget(t, owner, peer, os.Environ(), repository, false, 30*time.Second)
			defer resumed.close()
			if opened := resumed.exchange(map[string]any{"op": "open", "options": options}); opened.Error != "" {
				t.Fatal("resume open", opened.Error)
			}
			result := resumed.exchange(map[string]any{"op": "resume"})
			if fault == "recipe" {
				if result.Error != "" || result.Outcome == nil || result.Outcome.Status != releasetransition.StatusOperatorActionRequired || result.Outcome.Code == "" || result.Outcome.ReachedGoal || result.Outcome.Transaction == nil || *result.Outcome.Transaction != journal.Transaction {
					t.Fatal("changed original recipe did not return a structured refusal")
				}
				if read := resumed.exchange(map[string]any{"op": "capabilities", "kind": "capabilities"}); read.Error != "" {
					t.Fatal("recipe refusal lost the healthy readonly peer", read.Error)
				}
			}
			resumed.close()
			after, err := operationReplayFiles(home)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "unchanged" {
				if result.Error != "" || result.Outcome == nil || result.Outcome.Status != "ready" {
					if result.Outcome == nil {
						t.Fatal("unchanged original transaction failed without outcome", result.Error)
					}
					t.Fatalf("unchanged original transaction failed: error=%s status=%s code=%s", result.Error, result.Outcome.Status, result.Outcome.Code)
				}
				if ownerPending && cut == "after-authorized-journal" {
					completedCalls, err := os.ReadFile(filepath.Join(root, "component-calls.jsonl"))
					if err != nil {
						t.Fatal(err)
					}
					for _, mode := range []string{"inspect", "check"} {
						fresh := startRustOwnerWithBudget(t, owner, peer, os.Environ(), repository, false, 30*time.Second)
						defer fresh.close()
						if opened := fresh.exchange(map[string]any{"op": "open", "options": options}); opened.Error != "" {
							t.Fatal("completed fresh open", opened.Error)
						}
						observed := fresh.exchange(map[string]any{"op": mode, "goal": row.Initial.Goal})
						// This finite peer restarts with stale runtime facts; it does
						// not model persisted physical convergence across processes.
						if observed.Error != "" || observed.Inspection == nil || len(observed.Inspection.Blockers) != 0 || observed.Inspection.Resume != nil || observed.Inspection.Outcome == nil || observed.Inspection.Outcome.Status != "recovering" || observed.Inspection.Outcome.Code != "recovery-pending" {
							t.Fatal("completed current inspection refused", mode, observed.Error)
						}
						fresh.close()
					}
					calls, err := os.ReadFile(filepath.Join(root, "component-calls.jsonl"))
					if err != nil || !bytes.HasPrefix(calls, completedCalls) {
						t.Fatal("completed trace changed", err)
					}
					decoder := json.NewDecoder(bytes.NewReader(calls[len(completedCalls):]))
					for {
						var call string
						err := decoder.Decode(&call)
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
						if operationReplayEffect(call) || call == "owner.bind-original-recipe" || call == "owner.bind-empty-original-results" || call == "owner.project-activation" {
							t.Fatal("completed current inspection replayed retired work", call)
						}
					}
				}
				return
			}
			if result.Outcome != nil && result.Outcome.ReachedGoal {
				t.Fatal("changed authority reached goal", result)
			}
			callsAfter, err := os.ReadFile(filepath.Join(root, "component-calls.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(callsAfter, callsBefore) {
				t.Fatal("component call prefix changed")
			}
			decoder := json.NewDecoder(bytes.NewReader(callsAfter[len(callsBefore):]))
			recipeRefused := false
			for {
				var call string
				err := decoder.Decode(&call)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if call == "owner.original-recipe-refused" {
					recipeRefused = true
				}
				if operationReplayEffect(call) {
					t.Fatal("admission reached native effect", call)
				}
			}
			if fault == "recipe" && !recipeRefused {
				t.Fatal("recipe refusal did not reach original recipe validation")
			}
			if len(before) != len(after) {
				t.Fatal("admission changed protected file count", len(before), len(after))
			}
			for name, value := range before {
				if !bytes.Equal(value, after[name]) {
					t.Fatal("admission changed protected input", name, operationReplayDigest(value), operationReplayDigest(after[name]))
				}
			}
		})
	}
}

// The existing captured-facts peer distinguishes report captures from retained
// preparation. It supplies no Go plan/outcome to the actual Rust process.
func TestRustOwnerTerminalCheckUsesOnlyPendingProfileShortcut(t *testing.T) {
	owner, peer, corpus := os.Getenv("SUBYARD_RUST_FIXTURE_OWNER"), os.Getenv("SUBYARD_RUST_OPERATION_PEER"), os.Getenv("SUBYARD_RUST_OPERATION_TRACE_CORPUS")
	if owner == "" || peer == "" || corpus == "" {
		t.Skip("opt-in actual owner/peer")
	}
	raw, err := os.ReadFile(corpus)
	if err != nil {
		t.Fatal(err)
	}
	var rows []operationReplayRow
	if err = json.Unmarshal(raw, &rows); err != nil || len(rows) == 0 {
		t.Fatal("owning corpus", err)
	}
	row := rows[0]
	var native struct {
		Owners []string `json:"owners"`
	}
	if err = json.Unmarshal(row.Native, &native); err != nil {
		t.Fatal(err)
	}
	skipped := append(slices.Clone(native.Owners), "core-guest-runtime", "key-tools-runtime")
	var full *releasetransition.Inspection
	for _, mode := range []string{"inspect", "check", "check-complete", "check-checkpoint"} {
		op := strings.SplitN(mode, "-", 2)[0]
		t.Run(mode, func(t *testing.T) {
			root := testkit.TempDir(t)
			home, runtime, data, repository := filepath.Join(root, "state"), filepath.Join(root, "runtime"), filepath.Join(root, "data"), filepath.Join(root, "candidate")
			for _, path := range []string{home, data, repository, filepath.Join(runtime, "releases", string(row.Initial.Releases.From)), filepath.Join(runtime, "releases", string(row.Initial.Releases.Target))} {
				if err = os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for relative, raw := range row.Files {
				path := filepath.Join(home, relative)
				if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, raw, 0600)
			}
			if mode == "check-complete" {
				ledger := row.FinalFiles["release-transition/v2/ledger.json"]
				if len(ledger) == 0 {
					t.Fatal("owning complete ledger missing")
				}
				path := filepath.Join(home, "release-transition/v2/ledger.json")
				if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, ledger, 0600)
			}
			for relative, raw := range row.Candidate {
				path := filepath.Join(repository, "config", relative)
				if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, raw, 0600)
			}
			if err = os.Symlink("releases/"+string(row.Initial.Links.Active), filepath.Join(runtime, "current")); err != nil {
				t.Fatal(err)
			}
			if row.Initial.Links.Previous != nil {
				if err = os.Symlink("releases/"+string(*row.Initial.Links.Previous), filepath.Join(runtime, "previous")); err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, filepath.Join(root, "operation-peer-input.json"), row.Native, 0600)
			process := startRustOwnerWithBudget(t, owner, peer, os.Environ(), repository, false, 20*time.Second)
			defer process.close()
			options := map[string]any{"dataHome": data, "configHome": home, "runtimeRoot": runtime, "repositoryRoot": repository, "yard": "default", "releases": row.Initial.Releases, "artifactDigest": row.Initial.ArtifactDigest, "registryRaw": row.Registry, "catalog": row.Catalog, "inheritedSettingIds": []string{}, "rollbackTarget": nil, "migrationCheckpoint": mode == "check-checkpoint", "sourceIngress": nil}
			if reply := process.exchange(map[string]any{"op": "open", "options": options}); reply.Error != "" {
				t.Fatal("open", reply.Error)
			}
			before, err := operationReplayFiles(home)
			if err != nil {
				t.Fatal(err)
			}
			reply := process.exchange(map[string]any{"op": op, "goal": row.Initial.Goal})
			if reply.Error != "" || reply.Inspection == nil || len(reply.Inspection.Blockers) != 0 {
				t.Fatal("inspection refused", reply.Error)
			}
			if op == "check" {
				if err = process.cmd.Wait(); err != nil {
					t.Fatal("Check is not terminal", err)
				}
			}
			after, err := operationReplayFiles(home)
			if err != nil {
				t.Fatal(err)
			}
			operationReplayEqual(t, "report makes no protected writes", before, after)
			if _, err := os.Stat(filepath.Join(home, "migrations/update.lock")); !os.IsNotExist(err) {
				t.Fatal("report acquired an update lease", err)
			}
			calls, err := os.ReadFile(filepath.Join(root, "component-calls.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			var observed []string
			decoder := json.NewDecoder(bytes.NewReader(calls))
			for {
				var call string
				err = decoder.Decode(&call)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if operationReplayEffect(call) {
					t.Fatal("report dispatched effect")
				}
				if strings.HasPrefix(call, "component.observe.") {
					observed = append(observed, strings.TrimPrefix(call, "component.observe."))
				}
			}
			for _, name := range skipped {
				if slices.Contains(observed, name) != (mode != "check") {
					t.Fatalf("%s capture for %s: %v", op, name, observed)
				}
			}
			required := []string{"materialized-config", "test-vm-broker", "test-yard-route-consumers", "host-power"}
			var remaining []string
			for _, name := range observed {
				if !slices.Contains(skipped, name) {
					remaining = append(remaining, name)
				}
			}
			operationReplayEqual(t, "required capture order", required, remaining)
			if op == "inspect" {
				full = reply.Inspection
			} else if mode == "check" {
				operationReplayEqual(t, "check outcome", full.Outcome, reply.Inspection.Outcome)
				operationReplayEqual(t, "check consequences", full.Assessment, reply.Inspection.Assessment)
				operationReplayEqual(t, "check decisions", full.Decisions, reply.Inspection.Decisions)
				if reply.Inspection.Outcome.ReachedGoal || reply.Inspection.Resume != nil || reply.Admission != nil {
					t.Fatal("pending report acquired Ready or continuation authority")
				}
			}
		})
	}
}
