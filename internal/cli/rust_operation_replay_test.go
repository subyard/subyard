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
	for _, mode := range []string{"changed-source-bytes", "changed-source-identity"} {
		negative := rows[0]
		negative.Name = mode
		negative.negative = mode
		rows = append(rows, negative)
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
			reply := process.exchange(map[string]any{"op": "execute", "plan": inspection.Inspection.Plan, "authorization": "fixture-private-consent"})
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
			observations := []releasetransition.ResourceObservation{}
			for _, observation := range capture.Plan.Observations {
				if observation.Resource != "native.yard-set" && observation.Resource != "native.prepared-recipes" {
					observations = append(observations, observation)
				}
			}
			operationReplayEqual(t, "all semantic/source observations", row.Initial.Observations, observations)
			if len(capture.Plan.Observations) != len(observations)+2 {
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
