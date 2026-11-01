package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

type ownerContinuationProcessInput struct {
	Repository, Retained, Pool, Volume string
	Environment, RuntimeEnvironment    []string
	Raw                                []byte
	State                              ports.ReconcileState
	Resources                          []ports.TeardownResource
}

// The COMPLETE fact/history and Incus observations are synthetic. The helper,
// production init callback, saved grant assessment and inode guards are real.
func TestOwnerContinuationFreshProcessOriginalPending(t *testing.T) {
	if path := os.Getenv("SUBYARD_TEST_OWNER_CONTINUATION"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var input ownerContinuationProcessInput
		if err = json.Unmarshal(raw, &input); err != nil {
			t.Fatal(err)
		}
		program, err := New(Options{RepositoryRoot: input.Repository, Environment: input.Environment, Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		var record ownerInitRecord
		if err = nativejson.DecodeLosslessNativeJSON(input.Raw, &record, 32<<20); err != nil {
			t.Fatal(err)
		}
		loaded, err := record.Input.restore(program.catalog)
		if err != nil {
			t.Fatal(err)
		}
		host := &testkit.Incus{Reconcile: input.State, TeardownResources: input.Resources}
		runtime := reconcileruntime.Runtime{RepositoryRoot: input.Repository, Yard: loaded.Context, Incus: host, NetworkPolicy: legacyContinuationNetworkFixture{}, Environment: input.RuntimeEnvironment, SRVPool: input.Pool, SRVVolume: input.Volume}
		for _, fault := range []string{"original", "uuid", "epoch", "security", "source", "roots", "recreate", "settings", "permissions", "inventory", "runtime-root"} {
			t.Run(fault, func(t *testing.T) {
				execution, err := restoreOwnerInitRecord(input.Raw, loaded, runtime)
				if err != nil {
					t.Fatal(err)
				}
				again, err := captureOwnerInitRecord(execution)
				if err != nil || !bytes.Equal(again, input.Raw) {
					t.Fatal("original continuation changed", err)
				}
				changed := runtime
				copyHost := *host
				changed.Incus = &copyHost
				originalState := copyHost.Reconcile
				switch fault {
				case "uuid":
					copyHost.Reconcile.Instance.Config = maps.Clone(copyHost.Reconcile.Instance.Config)
					copyHost.Reconcile.Instance.Config["volatile.uuid"] = "replacement"
					copyHost.Reconcile.Instance.LocalConfig = maps.Clone(copyHost.Reconcile.Instance.LocalConfig)
					copyHost.Reconcile.Instance.LocalConfig["volatile.uuid"] = "replacement"
				case "security":
					copyHost.Reconcile.ProjectConfig = maps.Clone(copyHost.Reconcile.ProjectConfig)
					copyHost.Reconcile.ProjectConfig["restricted"] = "false"
				case "source":
					execution.legacyContinuation.ownerSource.Reference.MetadataBinding = strings.Repeat("b", 64)
				case "roots":
					execution.legacyContinuation.ownerSource.RuntimeRoot = filepath.Join(input.Repository, "foreign")
				case "epoch":
					execution.legacyContinuation.grant.OwnerEpoch++
				case "settings":
					file := execution.legacyContinuation.registrationPath
					before, err := os.ReadFile(file)
					if err != nil {
						t.Fatal(err)
					}
					testkit.WriteFile(t, file, append(before, []byte("\nSSH_PORT=2255\n")...), 0600)
					defer testkit.WriteFile(t, file, before, 0600)
				case "permissions":
					file := execution.legacyContinuation.registrationPath
					if err = os.Chmod(file, 0644); err != nil {
						t.Fatal(err)
					}
					defer os.Chmod(file, 0600)
				case "inventory":
					copyHost.TeardownResources = append([]ports.TeardownResource{}, copyHost.TeardownResources...)
					copyHost.TeardownResources[0].Binding = "replacement inventory"
				case "runtime-root":
					root := execution.legacyContinuation.ownerSource.RuntimeRoot
					backup := root + ".original-runtime"
					info, err := os.Lstat(root)
					if err != nil {
						t.Fatal(err)
					}
					if err = os.Rename(root, backup); err != nil {
						t.Fatal(err)
					}
					if err = os.Mkdir(root, info.Mode().Perm()); err != nil {
						t.Fatal(err)
					}
					defer func() {
						entries, err := os.ReadDir(root)
						if err != nil {
							t.Error(err)
							return
						}
						for _, entry := range entries {
							if err = os.Rename(filepath.Join(root, entry.Name()), filepath.Join(backup, entry.Name())); err != nil {
								t.Error(err)
								return
							}
						}
						if err = os.Remove(root); err != nil {
							t.Error(err)
							return
						}
						if err = os.Rename(backup, root); err != nil {
							t.Error(err)
						}
					}()
					entries, err := os.ReadDir(backup)
					if err != nil {
						t.Fatal(err)
					}
					for _, entry := range entries {
						if err = os.Rename(filepath.Join(backup, entry.Name()), filepath.Join(root, entry.Name())); err != nil {
							t.Fatal(err)
						}
					}
				case "recreate":
					copyHost.Reconcile.ProjectFound = false
					copyHost.Reconcile.ProfileFound = false
					copyHost.Reconcile.InstanceFound = false
				}
				execution.platform = changed
				retained := changed
				retained.RepositoryRoot = input.Retained
				program.options.InitPlatform = changed
				err = runOriginalOwnerInit(context.Background(), program, execution, changed, retained, io.Discard)
				if fault == "original" && err != nil {
					t.Fatal("production init after enclosing pending", err)
				}
				if fault != "original" && err == nil {
					t.Fatal("foreign original continuation admitted")
				}
				if len(copyHost.ConfigUpdates) != 0 {
					t.Fatal("continuation check wrote native state")
				}
				host.Reconcile = originalState
			})
		}
		return
	}
	program, execution, host, _ := legacyContinuationInstalledFixture(t)
	// A stopped original identity permits a legitimately empty init/hook plan.
	host.Reconcile.Instance.Status = "stopped"
	for _, values := range []map[string]string{host.Reconcile.Instance.Config, host.Reconcile.Instance.LocalConfig} {
		values["user.subyard.desired_power"] = "stopped"
	}
	ctx := context.Background()
	if err := program.prepareLegacyContinuation(ctx, execution, initArguments{continueLegacy: true}); err != nil {
		t.Fatal(err)
	}
	if err := execution.legacyContinuation.publish(execution); err != nil {
		t.Fatal(err)
	}
	execution.legacyContinuation = nil
	journalPath := filepath.Join(execution.loaded.Context.Paths.ConfigHome, "release-transition/v2/journal.json")
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := releasetransition.ParseJournal(raw)
	if err != nil {
		t.Fatal(err)
	}
	program.ownerCompleteSource = &releaseowner.CompleteSourceReference{Transaction: string(journal.Transaction), JournalDigest: nativeDigest(raw), MetadataBinding: strings.Repeat("a", 64)}
	if err = program.prepareLegacyContinuation(ctx, execution, initArguments{}); err != nil {
		t.Fatal("saved original capture", err)
	}
	execution.integrationBaseline, err = captureInitIntegrationBaseline(execution.loaded)
	if err != nil {
		t.Fatal(err)
	}
	original, err := captureOwnerInitRecord(execution, program)
	if err != nil {
		t.Fatal(err)
	}
	runtime := execution.platform.(reconcileruntime.Runtime)
	for _, fault := range []string{"reference", "missing-reference", "foreign-source", "installation", "tooling"} {
		var record ownerInitRecord
		if err = nativejson.DecodeLosslessNativeJSON(original, &record, 32<<20); err != nil {
			t.Fatal(err)
		}
		switch fault {
		case "reference":
			record.Continuation.SourceFacts.Reference.MetadataBinding = strings.Repeat("b", 64)
		case "missing-reference":
			record.Continuation.SourceFacts.Reference = releaseowner.CompleteSourceReference{}
		case "foreign-source":
			record.Continuation.SourceFacts.Repository = filepath.Join(record.Continuation.SourceFacts.RuntimeRoot, "releases/foreign")
			record.Continuation.SourceFacts.ConfigDir = filepath.Join(record.Continuation.SourceFacts.Repository, "config")
		case "installation":
			record.Continuation.SourceFacts.Installation.Identity.Inode++
		case "tooling":
			record.Continuation.SourceFacts.Tooling = strings.Repeat("b", 64)
		}
		tampered, err := nativejson.EncodeLosslessNativeJSON(record, 32<<20)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = restoreOwnerInitRecord(tampered, execution.loaded, runtime); err == nil {
			t.Fatal("tampered source facts admitted", fault)
		}
	}
	missing := *program
	missing.ownerCompleteSource = nil
	missing.ownerLegacySource = nil
	component := &releaseOwnerComponents{}
	if _, _, err = component.prepareOwnerInitView(ctx, &missing, execution.loaded, nil, nil); err == nil {
		t.Fatal("saved grant without COMPLETE reference admitted")
	}
	// Ordinary Go must independently refuse the same absent recreation identity.
	absent := *host
	absent.Reconcile.ProjectFound = false
	absent.Reconcile.ProfileFound = false
	absent.Reconcile.InstanceFound = false
	plain := *execution
	plain.legacyContinuation = nil
	absentRuntime := runtime
	absentRuntime.Incus = &absent
	plain.platform = absentRuntime
	if err = missing.prepareLegacyContinuation(ctx, &plain, initArguments{}); err == nil {
		t.Fatal("ordinary Go adopted absent saved identity")
	}
	retained := testkit.TempDir(t)
	if err = os.MkdirAll(filepath.Join(retained, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	// The transport marker is test control, not part of the admitted product env.
	input := ownerContinuationProcessInput{program.options.RepositoryRoot, retained, runtime.SRVPool, runtime.SRVVolume, program.options.Environment, runtime.Environment, original, host.Reconcile, host.TeardownResources}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(testkit.TempDir(t), "input.json")
	testkit.WriteFile(t, path, body, 0600)
	testkit.WriteFile(t, journalPath, []byte("{\"checkpoint\":\"authorized\"}\n"), 0600)
	processContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(processContext, os.Args[0], "-test.run=^TestOwnerContinuationFreshProcessOriginalPending$")
	child.WaitDelay = time.Second
	child.Env = append(os.Environ(), "SUBYARD_TEST_OWNER_CONTINUATION="+path)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("fresh original continuation: %v\n%s", err, output)
	}
}

// The bounded init callback retains its original guards. A retained tree
// deliberately has different file identities; original guards must still run
// against the original operation before the retained orphan child executes.
func TestOwnerInitCallbackOriginalBaselinesAndRetainedResource(t *testing.T) {
	for _, withOrphan := range []bool{false, true} {
		t.Run(fmt.Sprintf("orphan=%t", withOrphan), func(t *testing.T) {
			program, loaded, base := originalOrphanProcessCLI(t)
			host := &orphanOriginalHost{startupRecordHost: base}
			program.options.Incus = host
			runtime := reconcileruntime.Runtime{RepositoryRoot: program.options.RepositoryRoot, LogicalRepositoryRoot: program.options.RepositoryRoot, Environment: program.options.Environment, Yard: loaded.Context}
			platform := newInitPlatformFixture()
			for id := range platform.converged {
				platform.converged[id] = true
			}
			platform.converged[ports.ReconcileStageFinalize] = true
			program.options.InitPlatform = platform
			execution := &initExecution{loaded: loaded, mode: initReconcile, platform: runtime, operationID: "original-init"}
			var err error
			execution.hostID, err = configsync.EnsureHostID(loaded.Context.Paths.ConfigHome, loaded.Environment)
			if err != nil {
				t.Fatal(err)
			}
			execution.inputBaseline, err = program.captureOwnerInputs(loaded, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			execution.integrationBaseline, err = captureInitIntegrationBaseline(loaded)
			if err != nil {
				t.Fatal(err)
			}
			if withOrphan {
				execution.orphanIngress, err = program.prepareOrphanIngress(context.Background(), loaded)
				if err != nil {
					t.Fatal(err)
				}
			}
			if withOrphan {
				execution.plan, err = (application.Reconciler{Stages: application.InitStages(loaded.Context), Runner: platform}).Plan(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				execution.approvedPlan = execution.plan
				execution.approvedFinalize, err = (application.Reconciler{Stages: []application.ReconcileStage{application.FinalizeStage()}, Runner: platform}).Plan(context.Background())
				if err != nil {
					t.Fatal(err)
				}
			}
			raw, err := captureOwnerInitRecord(execution, program)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := restoreOwnerInitRecord(raw, loaded, runtime)
			if err != nil {
				t.Fatal(err)
			}
			restored.inputBaseline = execution.inputBaseline
			restored.platform = platform
			restored.operationID = "original-init"
			retained := runtime
			retained.RepositoryRoot = testkit.TempDir(t)
			if err = os.MkdirAll(filepath.Join(retained.RepositoryRoot, "config"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, relative := range []string{"config/profiles/sample/resources/relay.res", "config/profiles/sample/resources/relay/handler.sh"} {
				body, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, relative))
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(retained.RepositoryRoot, relative)
				if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, body, 0700)
			}
			_, release, err := uninstall.OwnerLock(context.Background(), loaded.Context.Paths.DataHome, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			observer := &orphanProcessObserver{startupProcessObserver: startupProcessObserver{root: loaded.Context.Paths.DataHome}}
			ctx := ports.WithNativeOwnerHome(ports.WithNativeOperationObserver(context.Background(), observer), loaded.Context.Paths.DataHome)
			if err = runOriginalOwnerInit(ctx, program, restored, runtime, retained, io.Discard); err != nil {
				t.Fatal("actual original init callback", err)
			}
			if platform.projectHooks != 1 {
				t.Fatal("full init did not reach original hooks")
			}
			if withOrphan && observer.commands != 1 {
				t.Fatal("retained resource child omitted", observer.commands)
			}
			if !withOrphan && observer.commands != 0 {
				t.Fatal("nil orphan expanded effects")
			}
		})
	}
}

func TestOwnerInitCallbackRejectsOriginalInputDrift(t *testing.T) {
	for _, fault := range []string{"config", "source", "inode"} {
		t.Run(fault, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			program, err := New(Options{RepositoryRoot: root, Environment: environment, Stdout: io.Discard, Stderr: io.Discard})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			runtime := reconcileruntime.Runtime{RepositoryRoot: root, LogicalRepositoryRoot: root, Environment: environment, Yard: loaded.Context}
			execution := &initExecution{loaded: loaded, mode: initReconcile, platform: runtime}
			execution.inputBaseline, err = program.captureOwnerInputs(loaded, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			execution.integrationBaseline, err = captureInitIntegrationBaseline(loaded)
			if err != nil {
				t.Fatal(err)
			}
			platform := newInitPlatformFixture()
			execution.platform = platform
			program.options.InitPlatform = platform
			retained := runtime
			retained.RepositoryRoot = testkit.TempDir(t)
			if err = os.Mkdir(filepath.Join(retained.RepositoryRoot, "config"), 0700); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "config":
				program.baseEnv["SSH_PORT"] = "2255"
			case "source":
				testkit.WriteFile(t, filepath.Join(root, "config/changed-source"), []byte("new scope"), 0600)
			case "inode":
				file := filepath.Join(root, "config/commands.registry")
				body, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				replacement := file + ".replacement"
				testkit.WriteFile(t, replacement, body, 0600)
				if err = os.Rename(replacement, file); err != nil {
					t.Fatal(err)
				}
			}
			if err = runOriginalOwnerInit(context.Background(), program, execution, runtime, retained, io.Discard); err == nil {
				t.Fatal("original input drift admitted")
			}
			if platform.projectHooks != 0 || len(platform.applied) != 0 {
				t.Fatal("drift reached init mutation")
			}
		})
	}
}
