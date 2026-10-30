package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestTeardownScopeSelectionPrecedesConsent(t *testing.T) {
	for _, test := range []struct {
		name, input    string
		arguments      []string
		terminal       bool
		assume         string
		full, declined bool
	}{
		{name: "default", input: "\nyes\n", terminal: true},
		{name: "full", input: "2\nyes\n", terminal: true, full: true},
		{name: "cancel", input: "q\n", terminal: true, declined: true},
		{name: "EOF", terminal: true, declined: true},
		{name: "non terminal", input: "2\n"},
		{name: "automation", input: "2\n", terminal: true, arguments: []string{"--yes"}},
		{name: "environment automation", input: "2\n", terminal: true, assume: "1"},
		{name: "explicit full", arguments: []string{"--full"}, full: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := strings.NewReader(test.input)
			output := &bytes.Buffer{}
			program := &CLI{env: map[string]string{"ASSUME_YES": test.assume}, options: Options{Stdin: input, Stdout: output}, promptInputTerminal: func() bool { return test.terminal }}
			arguments, err := program.chooseTeardownScope(domain.Context{YardName: "selected", AccessKind: domain.AccessLocal}, test.arguments)
			if test.declined {
				if !errors.Is(err, domain.ErrOperationDeclined) {
					t.Fatalf("decline=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			execution, err := prepareTeardownExecution(arguments)
			if err != nil || execution.full != test.full {
				t.Fatalf("execution=%#v err=%v", execution, err)
			}
			if test.input == "\nyes\n" || test.input == "2\nyes\n" {
				remaining, _ := io.ReadAll(input)
				if string(remaining) != "yes\n" {
					t.Fatalf("scope consumed consent: %q", remaining)
				}
				if !strings.Contains(output.String(), "selected on this local owner host") {
					t.Fatal(output.String())
				}
			}
			if strings.Contains(output.String(), "Proceed?") {
				t.Fatal("scope asked for deletion consent")
			}
		})
	}
}

func TestFullTeardownSnapshotCanonicalizesNativePartialPeers(t *testing.T) {
	program, loaded, incus, _ := teardownResetFixture(t)
	execution := &fullTeardown{}
	order := []string{"default", "neighbor", "stopped", "partial"}
	for _, name := range order {
		execution.Yards = append(execution.Yards, uninstall.RecoveryYard{Context: domain.Context{YardName: name}})
	}
	fresh, err := program.captureTeardownSnapshot(context.Background(), loaded, incus.Reconcile, nil, execution.contexts())
	if err != nil {
		t.Fatal(err)
	}
	approved := *fresh
	approved.Registered = execution.contexts()
	if err := checkTeardownSnapshot(&approved, fresh); err != nil {
		t.Fatal("unchanged native-only partial peer rejected", err)
	}
	for index, yard := range execution.Yards {
		if yard.Context.YardName != order[index] {
			t.Fatal("canonical peer projection changed execution order")
		}
	}
	fresh.Registered[1].SSHPort++
	if err := checkTeardownSnapshot(&approved, fresh); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("changed peer fact was admitted", err)
	}
}

func TestTeardownPendingRecoveryRejectsOriginalInvalidFlags(t *testing.T) {
	for _, flags := range [][]string{{"--full", "--keep-data", "--yes"}, {"--full", "--unknown", "--yes"}, {"--full", "--help", "--yes"}} {
		root, environment, _ := nativeFixture(t)
		home := filepath.Join(root, "data")
		directory := home + ".uninstall"
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		original := uninstall.RecoveryPlan{Schema: 1, Installation: uninstall.Installation{Schema: 1, OperatorUID: uint32(os.Getuid()), DataHome: home, ConfigHome: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "runtime"), CacheRoot: filepath.Join(root, "cache")}, Stage: uninstall.RecoveryPrepared}
		body, _ := json.Marshal(original)
		path := filepath.Join(directory, ".plan-next")
		testkit.WriteFile(t, path, body, 0600)
		prompt := &testkit.Prompt{Answers: []bool{true}}
		program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: append([]string{"teardown"}, flags...), Environment: environment, WorkingDir: root, Prompt: prompt, Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if slices.Contains(flags, "--help") {
			want = 0
		}
		if code := program.Run(context.Background()); code != want || len(prompt.Requests) != 0 {
			t.Fatal("invalid original flags entered recovery", flags, code)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, body) {
			t.Fatal("invalid flags changed original recovery", err)
		}
		if entries, err := os.ReadDir(directory); err != nil || len(entries) != 1 {
			t.Fatal("invalid flags mutated pending inventory", err)
		}
	}
}

func TestFullTeardownRecoveryAssessmentBindsReleaseRepairPermit(t *testing.T) {
	program, loaded, _, _ := teardownResetFixture(t)
	directory := loaded.Context.Paths.DataHome + ".uninstall"
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	original := uninstall.RecoveryPlan{Schema: 1, Installation: uninstall.Installation{Schema: 1, OperatorUID: uint32(os.Getuid()), DataHome: loaded.Context.Paths.DataHome, ConfigHome: loaded.Context.Paths.ConfigHome, RuntimeRoot: filepath.Join(program.options.RepositoryRoot, "runtime"), CacheRoot: filepath.Join(program.options.RepositoryRoot, "cache")}, Stage: uninstall.RecoveryPrepared}
	store := uninstall.RecoveryStore{Directory: directory, UID: uint32(os.Getuid())}
	if _, err := store.Save(original, nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(directory, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	program.fullTeardownRecovery = directory
	program.teardownRepair = &configApplyRepairPermit{factsFingerprint: "approved-history"}
	definition, _ := program.manifest.Lookup("teardown")
	prepared := &preparedCommand{lifecycle: &preparedLifecycle{}, CLI: program, Loaded: loaded, Definition: definition, Arguments: []string{"--full"}}
	if err := prepared.prepareTeardown(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	action, delta, err := prepared.assess(context.Background())
	if err != nil || action != "yard.teardown.full" || !delta.Changed {
		t.Fatalf("full recovery assessment unavailable: %s, %#v, %v", action, delta, err)
	}
	approved := prepared.exactState
	program.teardownRepair.factsFingerprint = "changed-history"
	if _, _, err := prepared.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if prepared.exactState == approved {
		t.Fatal("full consent did not bind the original release repair facts")
	}
	after, err := os.ReadFile(filepath.Join(directory, "plan.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("assessment replaced the original retry plan", err)
	}
}

type fullOwnershipFixture struct {
	*testkit.Incus
	owner       ports.InstallationYard
	inventories int
	shared      []ports.TeardownResource
}

func (fake *fullOwnershipFixture) InstallationYards(context.Context) ([]ports.InstallationYard, error) {
	if fake.owner.Project == "" {
		return nil, nil
	}
	return []ports.InstallationYard{fake.owner}, nil
}

func (fake *fullOwnershipFixture) InstallationSharedInventory(context.Context, string, string) ([]ports.TeardownResource, error) {
	return fake.shared, nil
}

func (fake *fullOwnershipFixture) TeardownInventory(ctx context.Context, project string) ([]ports.TeardownResource, error) {
	fake.inventories++
	return fake.Incus.TeardownInventory(ctx, project)
}

func (fake *fullOwnershipFixture) ReconcileState(ctx context.Context, project, instance, pool, volume, network string) (ports.ReconcileState, error) {
	state, err := fake.Incus.ReconcileState(ctx, project, instance, pool, volume, network)
	if fake.owner.Context != nil {
		state.ProjectFound = project == fake.owner.Project
		state.ProfileFound, state.InstanceFound = state.ProjectFound, false
	}
	return state, err
}

func TestFullTeardownClassifiesNativeOwnershipBeforeDestructiveInventory(t *testing.T) {
	for _, kind := range []string{"same-uid-other-data", "same-uid-other-config", "other-operator", "unproven-legacy", "owned-partial", "owned-unregistered", "foreign-missing-roots", "adopted"} {
		t.Run(kind, func(t *testing.T) {
			program, loaded, incus, _ := teardownResetFixture(t)
			incus.Reconcile.ProjectFound = true
			for _, name := range []string{"subyard-power-reconcile.service.in", "subyard-keys-sync.service.in", "subyard-keys-sync.timer.in"} {
				body, err := os.ReadFile(filepath.Join("../../config/systemd", name))
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(program.options.RepositoryRoot, "config/systemd", name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, body, 0600)
			}
			installation := uninstall.Installation{Schema: 1, OperatorUID: uint32(os.Getuid()), DataHome: loaded.Context.Paths.DataHome, ConfigHome: loaded.Context.Paths.ConfigHome, RuntimeRoot: filepath.Join(program.options.RepositoryRoot, "runtime"), CacheRoot: filepath.Join(program.options.RepositoryRoot, "cache")}
			if err := os.MkdirAll(installation.DataHome, 0700); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(installation)
			testkit.WriteFile(t, filepath.Join(installation.DataHome, uninstall.ManifestName), body, 0600)
			owner := ports.InstallationYard{Project: loaded.Context.IncusProject, OperatorUID: strconv.Itoa(os.Getuid()), DataHome: installation.DataHome, ConfigHome: installation.ConfigHome, HostBase: loaded.Context.Paths.HostBase}
			partialProfile, _ := json.Marshal(map[string]any{"name": "default", "config": map[string]string{}, "devices": map[string]any{}, "description": "partial profile"})
			switch kind {
			case "adopted":
				owner.Adopted = true
				program.options.DispatcherPath = filepath.Join(program.options.RepositoryRoot, "bin", "yard")
				for _, relative := range []string{"scripts/teardown-physical.sh", "scripts/lib/teardown-services.sh", "bin/yard"} {
					body := []byte("fixture retained executable\n")
					if relative != "bin/yard" {
						var err error
						body, err = os.ReadFile(filepath.Join("../..", relative))
						if err != nil {
							t.Fatal(err)
						}
					}
					path := filepath.Join(program.options.RepositoryRoot, relative)
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					testkit.WriteFile(t, path, body, 0700)
				}
				if err := os.MkdirAll(filepath.Join(loaded.Context.Paths.HostBase, "backups"), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(loaded.Context.Paths.HostBase, "backups", "preserved"), []byte("preserve"), 0600)
			case "same-uid-other-data":
				owner.DataHome += "-other"
			case "same-uid-other-config":
				owner.ConfigHome += "-other"
			case "other-operator":
				owner.OperatorUID = strconv.Itoa(os.Getuid() + 1)
			case "unproven-legacy":
				owner.Project = ""
			case "owned-unregistered":
				partial := loaded.Context
				partial.YardName, partial.IncusProject, partial.YardInstanceName = "partial", "native-partial", "yard-partial"
				partial.Paths.HostBase += "-partial"
				owner.Context, owner.Project = &partial, partial.IncusProject
				owner.Pool, owner.Volume, owner.StoragePool = "partial-pool", "partial-volume", "partial-storage"
				incus.Reconcile.HostPoolFound, incus.Reconcile.HostNetworkFound = false, false
				incus.TeardownResources = []ports.TeardownResource{{Kind: "profile", Name: "default", Binding: fmt.Sprintf("%x", sha256.Sum256(partialProfile))}}
			case "foreign-missing-roots":
				owner.DataHome += "-other"
				owner.ConfigHome, owner.HostBase = "", ""
				publicRoot, absErr := filepath.Abs("../..")
				if absErr != nil {
					t.Fatal(absErr)
				}
				program.options.RepositoryRoot = publicRoot
				catalog, catalogErr := config.LoadCatalog(publicRoot)
				if catalogErr != nil {
					t.Fatal(catalogErr)
				}
				program.catalog = catalog
			}
			fake := &fullOwnershipFixture{Incus: incus, owner: owner}
			if kind == "adopted" {
				fake.shared = []ports.TeardownResource{{Kind: "pool", Name: "default", Binding: "pool", Path: loaded.Context.Paths.StoragePath}, {Kind: "network", Name: loaded.Context.IncusBridge, Binding: "network"}, {Kind: "host-profile", Name: "default", Binding: "profile"}}
			}
			program.options.Incus = fake
			execution := &fullTeardown{}
			err := program.observeFullTeardown(context.Background(), loaded, execution)
			if kind == "adopted" {
				if err != nil {
					t.Fatal(err)
				}
				if len(execution.Shared) != 0 || !slices.Contains(execution.PreservedRoots, loaded.Context.Paths.HostBase) {
					t.Fatal("adoption inferred shared or host creation ownership")
				}
				for _, tree := range execution.Trees {
					if tree.Path == loaded.Context.Paths.HostBase {
						t.Fatal("unrecorded adopted host namespace scheduled for deletion")
					}
				}
			} else if kind == "owned-partial" || kind == "owned-unregistered" {
				if fake.inventories == 0 || len(execution.Yards) != 1 {
					t.Fatal("owned partial project was not admitted", err)
				}
				if kind == "owned-unregistered" {
					yard := execution.Yards[0]
					if yard.Context.IncusProject != owner.Project {
						t.Fatal("capture lost native partial ownership")
					}
					bin := testkit.TempDir(t)
					testkit.WriteFile(t, filepath.Join(bin, "incus"), []byte("#!/bin/sh\ncase \"$2\" in *'?project=native-partial') printf '%s' '"+string(partialProfile)+"';; *) printf '%s' '{\"name\":\"default\",\"config\":{},\"devices\":{},\"description\":\"default yard profile\"}';; esac\n"), 0700)
					values := structuredCommandContext(program.teardownLoaded(yard))
					payload, _ := json.Marshal(yard.Snapshot.Resources)
					command := exec.Command("python3", "../../scripts/lib/teardown-plan.py", "guard-all", owner.Project)
					command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SUBYARD_TEARDOWN_INVENTORY="+string(payload))
					if output, err := command.CombinedOutput(); err != nil {
						t.Fatalf("original native partial facts were not accepted: %v: %s", err, output)
					}
					command = exec.Command("python3", "../../scripts/lib/teardown-plan.py", "guard-all", values["INCUS_PROJECT"])
					command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SUBYARD_TEARDOWN_INVENTORY="+string(payload))
					if output, err := command.CombinedOutput(); err != nil {
						t.Fatalf("captured native partial profile routed to another project: %v: %s", err, output)
					}
					for key, expected := range structuredAdapterContext(*owner.Context) {
						if values[key] != expected {
							t.Fatalf("native partial context lost %s", key)
						}
					}
				}
			} else {
				if fake.inventories != 0 || len(execution.Yards) != 0 {
					t.Fatal("foreign project entered destructive inventory", kind, fake.inventories, len(execution.Yards))
				}
				if kind == "unproven-legacy" && (err == nil || !strings.Contains(err.Error(), "no exact installation ownership")) {
					t.Fatal("unprovable legacy ownership not diagnosed", err)
				}
				if kind == "foreign-missing-roots" {
					approved := false
					for _, tree := range execution.Trees {
						approved = approved || tree.Path == installation.DataHome
					}
					if !approved {
						t.Fatal("absent foreign roots suppressed this installation's data cleanup", err)
					}
				}
			}
			if len(fake.ConfigUpdates) != 0 || len(fake.PowerUpdates) != 0 {
				t.Fatal("assessment changed foreign resources")
			}
		})
	}
}

func TestQueuedLocalMutationRefusesCompletedEpochInRetainedRoot(t *testing.T) {
	home := testkit.TempDir(t)
	generation, err := uninstall.CaptureRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = uninstall.AdvanceOwnerEpoch(home, 0, 1); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	called := false
	prepared := &preparedCommand{lifecycle: &preparedLifecycle{}, CLI: &CLI{}, Loaded: config.Loaded{Context: domain.Context{AccessKind: domain.AccessLocal, Paths: domain.RuntimePaths{DataHome: home, ConfigHome: filepath.Join(home, "config")}}}, Plan: domain.OperationPlan{Confirmed: true}, ownerAdmission: ownerWriteAdmission{home: home, generation: []uninstall.Tree{generation}}, execute: func(context.Context, *application.Orchestrator, io.Writer) (domain.AdapterResult, error) {
		called = true
		return domain.AdapterResult{}, nil
	}}
	authorizePreparedFixture(prepared)
	_, err = prepared.Execute(context.Background(), nil, io.Discard)
	if !errors.Is(err, domain.ErrPlanStale) || called {
		t.Fatal("queued mutation entered removed generation", err, called)
	}
}

func TestTeardownFullRejectsRemoteAndKeepDataBeforeConsent(t *testing.T) {
	program := &CLI{env: map[string]string{}, options: Options{Stdin: strings.NewReader(""), Stdout: io.Discard}}
	if _, err := program.chooseTeardownScope(domain.Context{AccessKind: domain.AccessRemote}, []string{"--full", "--yes"}); err == nil {
		t.Fatal("remote full permitted")
	}
	if _, err := prepareTeardownExecution([]string{"--full", "--keep-data"}); err == nil {
		t.Fatal("full retained data permitted")
	}
	if _, err := prepareTeardownExecution([]string{"--full", "--reset-config"}); err != nil {
		t.Fatal(err)
	}
}

func TestTeardownFirstPublicationAdmissionPrecedesContextAndConsent(t *testing.T) {
	for _, kind := range []string{"complete", "truncated", "empty"} {
		t.Run(kind, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			home := filepath.Join(root, "data")
			directory := home + ".uninstall"
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			installation := uninstall.Installation{Schema: 1, OperatorUID: uint32(os.Getuid()), DataHome: home, ConfigHome: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "runtime"), CacheRoot: filepath.Join(root, "cache")}
			original := uninstall.RecoveryPlan{Schema: 1, Installation: installation, Stage: uninstall.RecoveryPrepared}
			body, _ := json.Marshal(original)
			if kind == "truncated" {
				body = body[:len(body)/2]
			}
			if kind != "empty" {
				testkit.WriteFile(t, filepath.Join(directory, ".plan-next"), body, 0600)
			}
			// Ordinary context loading must not replace the original recovery facts.
			testkit.WriteFile(t, filepath.Join(root, "config", "subyard.env"), []byte("HOST_BASE='unterminated\n"), 0600)
			prompt := &testkit.Prompt{Answers: []bool{false}}
			stderr := &bytes.Buffer{}
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"teardown", "--full"}, Environment: environment, WorkingDir: root, Prompt: prompt, Stdout: io.Discard, Stderr: stderr})
			if err != nil {
				t.Fatal(err)
			}
			code := program.Run(context.Background())
			if kind == "complete" {
				if len(prompt.Requests) != 1 || prompt.Requests[0].Default != domain.ConfirmationDefaultNo {
					t.Fatal("original first plan did not reach its single default-no confirmation", code, stderr.String())
				}
			} else if code == 0 || len(prompt.Requests) != 0 || !strings.Contains(stderr.String(), "ORIGINAL approved inventory") {
				t.Fatal("incomplete protected store bypassed admission", code, stderr.String())
			}
			if _, err := os.Stat(filepath.Join(directory, "plan.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("assessment published recovery", err)
			}
			if _, err := os.Stat(filepath.Join(directory, "runtime")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("assessment retained runtime before consent", err)
			}
		})
	}
}

func TestTeardownLegacySourceInventoryRetainsExactShellPaths(t *testing.T) {
	for _, kind := range []string{"same", "different", "foreign-record"} {
		t.Run(kind, func(t *testing.T) {
			root := testkit.TempDir(t)
			data := filepath.Join(root, "data")
			configHome := filepath.Join(root, "config")
			runtime := filepath.Join(root, "custom-runtime")
			rc := filepath.Join(root, "profile")
			login := rc
			if kind == "different" {
				login = filepath.Join(root, "login")
			}
			values := map[string]string{"data-home": data, "config-home": configHome, "bin-dir": filepath.Join(root, "bin"), "runtime-launcher": filepath.Join(runtime, "current/bin/yard"), "rc.path": rc, "login-rc.path": login}
			if kind == "foreign-record" {
				values["config-home"] = filepath.Join(root, "another-installation")
			}
			directory := filepath.Join(data, "recovery/pre-go-source")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			for name, value := range values {
				testkit.WriteFile(t, filepath.Join(directory, name), []byte(value+"\n"), 0600)
			}
			program := &CLI{options: Options{RepositoryRoot: filepath.Join(root, "source")}, env: map[string]string{}}
			saved, err := program.discoverLegacyInstallation(config.Loaded{Context: domain.Context{Paths: domain.RuntimePaths{DataHome: data, ConfigHome: configHome, OperatorHome: root}}})
			if kind == "foreign-record" {
				if err == nil {
					t.Fatal("foreign recorded configuration root adopted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			expected := []string{rc}
			if kind == "different" {
				expected = append(expected, login)
			}
			if saved.RuntimeRoot != runtime || operationStateDigest(saved.ShellFiles) != operationStateDigest(expected) {
				t.Fatal("legacy source inventory changed exact paths", saved.RuntimeRoot, saved.ShellFiles)
			}
		})
	}
}

type teardownBatchLock struct {
	held         bool
	acquisitions int
}

func (lock *teardownBatchLock) Acquire(context.Context) (func(), error) {
	if lock.held {
		return nil, errors.New("recursive network lock")
	}
	lock.held = true
	lock.acquisitions++
	return func() { lock.held = false }, nil
}
func TestTeardownBatchUsesOneApprovedYardSetAndOneLock(t *testing.T) {
	ctx := context.Background()
	program, first, _, _ := teardownResetFixture(t)
	path := filepath.Join(first.Context.Paths.ConfigHome, "yards", "neighbor.env")
	testkit.WriteFile(t, path, []byte("SSH_PORT=3334\nINCUS_PROJECT=subyard-neighbor\nYARD_INSTANCE_NAME=yard-neighbor\n"), 0600)
	second, err := program.loadInventoryLoaded("neighbor", first)
	if err != nil {
		t.Fatal(err)
	}
	execution := &teardownExecution{full: true, revokeAgent: true}
	for _, loaded := range []config.Loaded{first, second} {
		if err := os.MkdirAll(loaded.Context.Paths.StateDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(loaded.Context.Paths.StateDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := program.observeTeardownExecution(ctx, loaded, execution); err != nil {
			t.Fatal(err)
		}
	}
	execution.Assessed = true
	for _, yard := range execution.Yards {
		yard.Snapshot.Registered = execution.contexts()
	}
	original := operationStateDigest(execution.RecoveryPlan)
	lock := &teardownBatchLock{}
	program.options.NetworkPolicy.Lock = lock
	sessions := 0
	before := false
	runner := &testkit.ScriptedAdapter{}
	for range execution.Yards {
		runner.Steps = append(runner.Steps, testkit.AdapterStep{Result: domain.AdapterResult{Schema: 1, Status: "ok"}, Apply: func(request domain.AdapterRequest) {
			if !lock.held || !before || sessions != 2 {
				t.Fatal("yard applied before aggregate guards or outside the real lock")
			}
			if request.Context["SUBYARD_TEARDOWN_KEEP_SHARED"] != "1" || request.Context["SUBYARD_TEARDOWN_FULL"] != "1" {
				t.Fatal("full yard leaf lost exact aggregate scope")
			}
			if err := os.Remove(request.Context["SUBYARD_STATE_DIR"]); err != nil {
				t.Fatal(err)
			}
		}})
	}
	program.options.AdapterRunner = runner
	leaf := func(action string, _ map[string]string) error {
		if action != "check-sessions" || !lock.held || len(runner.Requests) != 0 {
			t.Fatal("unexpected or late session preflight")
		}
		sessions++
		return nil
	}
	orchestrator := &application.Orchestrator{Runner: runner, Clock: wallClock{}}
	plan := confirmedAdapterFixture(t, orchestrator, domain.OperationPlan{OperationID: "batch"})
	_, err = program.executeTeardownYards(ctx, orchestrator, first, plan, execution, io.Discard, func() error { before = true; return nil }, nil, leaf)
	if err != nil {
		t.Fatal(err)
	}
	if lock.held || lock.acquisitions != 1 || len(runner.Requests) != 2 || operationStateDigest(execution.RecoveryPlan) != original {
		t.Fatal("batch duplicated locking, skipped a target, or changed original approved facts")
	}
}

func TestLifecycleOnlyManagedPathsWaitForInstallationCleanup(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) {
			program, loaded, _, _ := teardownResetFixture(t)
			declaration := filepath.Join(program.options.RepositoryRoot, "config/profiles/synthetic")
			if err := os.MkdirAll(declaration, 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(declaration, "profile.json"), []byte(`{"schema_version":1,"lifecycle_only":true,"full_cleanup":"cleanup.py","managed_paths":[{"root":"data","path":"routes/{yard}"}]}`), 0600)
			testkit.WriteFile(t, filepath.Join(declaration, "cleanup.py"), []byte("#!/usr/bin/env python3\n"), 0700)
			route := filepath.Join(loaded.Context.Paths.DataHome, "routes/default/owned")
			peer := filepath.Join(loaded.Context.Paths.DataHome, "routes/foreign/keep")
			for _, path := range []string{route, peer} {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte("preserve until owning phase"), 0600)
			}
			if err := os.MkdirAll(loaded.Context.Paths.StateDir, 0700); err != nil {
				t.Fatal(err)
			}
			program.options.ObserveCoreProducers = func(config.Loaded, uninstall.RecoveryPlan, []string, int) ([]uninstall.RecoveryService, error) {
				return nil, nil
			}
			execution := &teardownExecution{full: full, revokeAgent: true}
			ctx := context.Background()
			if err := program.observeTeardownExecution(ctx, loaded, execution); err != nil {
				t.Fatal(err)
			}
			execution.Assessed = true
			definitions, err := profile.Load(program.options.RepositoryRoot)
			if err != nil {
				t.Fatal(err)
			}
			namespaces, scoped, err := fullRootNamespaces(loaded.Context.Paths.DataHome, uninstall.Installation{DataHome: loaded.Context.Paths.DataHome}, execution, definitions)
			if err != nil || !scoped {
				t.Fatal("installation namespace assessment", err)
			}
			tree, err := uninstall.CaptureScoped(loaded.Context.Paths.DataHome, namespaces, nil)
			if err != nil {
				t.Fatal(err)
			}
			runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{Schema: 1, Status: "ok"}, Apply: func(request domain.AdapterRequest) {
				if err := os.Remove(request.Context["SUBYARD_STATE_DIR"]); err != nil {
					t.Fatal(err)
				}
			}}}}
			program.options.AdapterRunner = runner
			leaf := func(string, map[string]string) error { return nil }
			orchestrator := &application.Orchestrator{Runner: runner, Clock: wallClock{}}
			plan := confirmedAdapterFixture(t, orchestrator, domain.OperationPlan{OperationID: "lifecycle-path"})
			_, err = program.executeTeardownYards(ctx, orchestrator, loaded, plan, execution, io.Discard, nil, nil, leaf)
			if err != nil {
				t.Fatal("common yard executor must finish before installation file cleanup", err)
			}
			if _, err := os.Stat(route); err != nil {
				t.Fatal("yard phase removed installation-only path", err)
			}
			if full {
				// The full executor runs this owned file phase only after common yard verification.
				if err := tree.Remove(); err != nil {
					t.Fatal(err)
				}
				if err := tree.VerifyRemoved(); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(route); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("installation-owned path remains", err)
				}
			}
			if body, err := os.ReadFile(peer); err != nil || string(body) != "preserve until owning phase" {
				t.Fatal("foreign sibling changed", err)
			}
		})
	}
}
