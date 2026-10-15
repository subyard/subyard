package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/adapters/sshagentruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func (cli *CLI) runFullTeardownRecovery(ctx context.Context, args []string) int {
	assumeYes := false
	if len(args) == 2 && args[1] == "--yes" {
		assumeYes = true
		args = args[:1]
	}
	if len(args) != 1 {
		cli.errorf("usage: yard _uninstall-resume /exact/recovery/directory")
		return 2
	}
	cli.fullTeardownRecovery = args[0]
	store := uninstall.RecoveryStore{Directory: args[0], UID: uint32(os.Getuid())}
	saved, _, err := store.Read()
	if err != nil {
		cli.errorf("full uninstall recovery at %s: %v; retain this protected store and restore its ORIGINAL approved inventory before continuing", store.Directory, err)
		return 1
	}
	loaded := config.Loaded{Catalog: cli.catalog, Environment: map[string]string{}, Context: domain.Context{AccessKind: domain.AccessLocal, YardName: "default", Paths: domain.RuntimePaths{DataHome: saved.Installation.DataHome, ConfigHome: saved.Installation.ConfigHome, RepositoryRoot: cli.options.RepositoryRoot, ConfigDir: filepath.Join(cli.options.RepositoryRoot, "config"), OperatorHome: cli.env["HOME"]}}}
	if len(saved.Yards) > 0 {
		loaded.Context = saved.Yards[0].Context
		loaded.Environment = saved.Yards[0].Environment
	}
	cli.env["SUBYARD_NO_AUDIT"] = "1"
	definition, _ := cli.manifest.Lookup("teardown")
	prepared, err := cli.prepareCommand(ctx, prepareCommandRequest{Definition: definition, Loaded: loaded, Arguments: []string{"--full"}})
	if err != nil {
		return cli.reportPreparationError(definition, err)
	}
	defer prepared.Close()
	return cli.runPreparedCommand(ctx, prepared, assumeYes)
}

func (cli *CLI) restoreFullTeardown(execution *fullTeardown) error {
	saved, binding, err := (uninstall.RecoveryStore{Directory: cli.fullTeardownRecovery, UID: uint32(os.Getuid())}).Read()
	if err != nil {
		return err
	}
	execution.RecoveryPlan, execution.RecoveryBinding, execution.resumed = saved, &binding, true
	cli.ownerMutationLockExpected = saved.OwnerLock

	return nil
}

func (cli *CLI) executeFullTeardown(ctx context.Context, orchestrator *application.Orchestrator, current config.Loaded, operation domain.OperationPlan, execution *fullTeardown, diagnostics io.Writer) (result domain.AdapterResult, err error) {
	result = domain.AdapterResult{Schema: shelladapter.ProtocolSchema, OperationID: operation.OperationID, Status: "ok"}
	if err = cli.prepareTeardownPrivileges(ctx, execution, diagnostics); err != nil {
		return result, err
	}
	directory := cli.fullTeardownRecovery
	if directory == "" {
		directory = execution.Installation.DataHome + ".uninstall"
	}
	for _, tree := range execution.Trees {
		if directory == tree.Path || strings.HasPrefix(directory, tree.Path+"/") {
			return result, errors.New("recovery root overlaps an approved removal root")
		}
	}
	store := uninstall.RecoveryStore{Directory: directory, UID: uint32(os.Getuid())}
	defer func() {
		if err != nil {
			fmt.Fprintf(diagnostics, "Full removal is incomplete. Retain %s and resume with:\n  %s _uninstall-resume %s\n", directory, filepath.Join(directory, "runtime", "bin", "yard"), directory)
		}
	}()
	saved := &execution.RecoveryPlan
	binding := execution.RecoveryBinding
	if !execution.resumed {
		saved.OwnerLock = cli.ownerMutationLock
		// Capture this goal once under the stable owner barrier, before first publication.
		saved.EpochBefore, err = uninstall.ReadOwnerEpoch(saved.Installation.DataHome)
		if err != nil {
			return result, err
		}
		if err = saved.Validate(uint32(os.Getuid())); err != nil {
			return result, err
		}
		if err = os.Mkdir(directory, 0700); err != nil {
			return result, fmt.Errorf("create exact full uninstall recovery root: %w", err)
		}
		if err = os.Chmod(directory, 0700); err != nil {
			return result, err
		}
		captured, saveErr := store.Save(*saved, nil)
		if saveErr != nil {
			return result, saveErr
		}
		binding = &captured
	}
	execution.RecoveryBinding = binding
	if saved.OwnerLock != nil {
		if cli.ownerMutationLock == nil || saved.OwnerLock.Path != cli.ownerMutationLock.Path {
			return result, errors.New("saved owner mutation lock is unavailable")
		}
		if err = uninstall.CheckOwnerLockIdentity(*saved.OwnerLock); err != nil {
			return result, err
		}
	}
	if binding != nil && filepath.Base(binding.Path) == ".plan-next" {
		if err = binding.Check(); err != nil {
			return result, err
		}
		original, publishErr := store.Save(*saved, nil)
		if publishErr != nil {
			return result, publishErr
		}
		*binding = original
	}
	checkpoint := func() error {
		next, saveErr := store.Save(*saved, binding)
		if saveErr == nil {
			*binding = next
		}
		return saveErr
	}
	if !saved.AssetsReady {
		if err = store.PublishRuntime(*saved); err != nil {
			return result, err
		}
		saved.AssetsReady = true
		if err = checkpoint(); err != nil {
			return result, err
		}
	}
	uninstall.UseArtifactGuard(filepath.Join(directory, "runtime"))
	// All following leaves use the retained engine and public assets, never a
	// runtime root that this operation may already have removed.
	clone := *cli
	clone.options.RepositoryRoot = filepath.Join(directory, "runtime")
	clone.options.DispatcherPath = filepath.Join(clone.options.RepositoryRoot, "bin", "yard-engine")
	clone.fullTeardownRecovery = directory
	if execution.resumed && (saved.Stage == uninstall.RecoveryPrepared || saved.Stage == uninstall.RecoveryProducersStopped) {
		for _, yard := range execution.Yards {
			manager := sshagentruntime.Manager{Config: sshagentruntime.Config{Directory: sshagentruntime.Directory(yard.Context.Paths.DataHome, yard.Context.YardName)}}
			agent, agentErr := manager.PrepareLock(ctx)
			if agentErr != nil {
				return result, agentErr
			}
			if agent.State() != "locked" && agent.Binding() != yard.Snapshot.AgentBinding {
				return result, domain.ErrPlanStale
			}
			yard.Snapshot.Agent = agent
		}
	}

	definition, _ := clone.manifest.Lookup("teardown")
	orchestrator.Runner = clone.operationOrchestrator(operation.OperationID, current, nil, &definition).Runner
	leaf := func(action string, values map[string]string) error {
		if bindErr := execution.bindFullPlanContext(&clone, values); bindErr != nil {
			return bindErr
		}
		request := domain.AdapterRequest{Schema: shelladapter.ProtocolSchema, OperationID: operation.OperationID, Adapter: "teardown", Action: action, Arguments: []string{action, "--yes"}, Context: values}
		applied, stderr, runErr := orchestrator.RunAdapter(ctx, operation, request, nil)
		writeAdapterDiagnostics(diagnostics, stderr)
		if runErr != nil {
			return runErr
		}
		if applied.Status != "ok" {
			return errors.New("full uninstall physical leaf did not converge")
		}
		return nil
	}
	values := structuredCommandContext(current)
	values["SUBYARD_DISPATCHER_PATH"] = clone.options.DispatcherPath
	values["SUBYARD_SUDO_PREAUTHORIZED"] = "1"
	processInfo, readProcessErr := os.ReadFile("/proc/self/stat")
	if readProcessErr != nil {
		return result, readProcessErr
	}
	processFields := strings.Fields(string(processInfo)[strings.LastIndex(string(processInfo), ")")+1:])
	if len(processFields) < 20 {
		return result, errors.New("owner process identity unavailable")
	}
	values["SUBYARD_TEARDOWN_PARENT_ID"] = fmt.Sprintf("%d:%s", os.Getpid(), processFields[19])
	if saved.Stage == uninstall.RecoveryPrepared || saved.Stage == uninstall.RecoveryProducersStopped {
		before := func() error {
			if len(saved.ProfileChecks) > 0 {
				if err := leaf("check-profiles", values); err != nil {
					return err
				}
			}
			if saved.Stage == uninstall.RecoveryPrepared {
				if len(saved.Services) > 0 {
					if err := leaf("stop-services", values); err != nil {
						return err
					}
				}
				saved.Stage = uninstall.RecoveryProducersStopped
				return checkpoint()
			}
			return nil
		}
		after := func() error {
			if len(saved.Shared) > 0 {
				payload, _ := json.Marshal(saved.Shared)
				values["SUBYARD_TEARDOWN_INVENTORY"] = string(payload)
				return leaf("remove-shared", values)
			}
			return nil
		}
		if _, err = clone.executeTeardownYards(ctx, orchestrator, current, operation, execution, diagnostics, before, after, leaf); err != nil {
			return result, err
		}
		saved.Stage = uninstall.RecoveryYardsRemoved
		if err = checkpoint(); err != nil {
			return result, err
		}
	}
	if saved.Stage == uninstall.RecoveryYardsRemoved {
		for index, edit := range saved.Startup {
			preparedEdit, editErr := edit.PrepareApply()
			if editErr != nil {
				return result, editErr
			}
			saved.Startup[index] = preparedEdit
			if err = checkpoint(); err != nil {
				return result, err
			}
			updated, editErr := preparedEdit.CommitApply()
			if editErr != nil {
				return result, editErr
			}
			saved.Startup[index].Before = updated
			saved.Startup[index].Blocks = nil
			saved.Startup[index].Pending = nil
			if err = checkpoint(); err != nil {
				return result, err
			}
		}
		for _, launcher := range saved.Launchers {
			if err = launcher.Remove(); err != nil {
				return result, err
			}
		}
		// Privileged exact-tree refinement/removal is bound to this durable plan.
		if err = leaf("remove-files", values); err != nil {
			return result, err
		}
		// The leaf may durably refine conditional private roots. Reload its exact
		// approved metadata rather than retaining a stale publication identity.
		reloaded, newBinding, readErr := store.Read()
		if readErr != nil {
			return result, readErr
		}
		execution.RecoveryPlan = reloaded
		*binding = newBinding
		saved = &execution.RecoveryPlan

		saved.Stage = uninstall.RecoveryFilesRemoved
		if err = checkpoint(); err != nil {
			return result, err
		}
	}
	for _, tree := range saved.Trees {
		if err = tree.VerifyRemoved(); err != nil {
			return result, err
		}
	}
	for _, launcher := range saved.Launchers {
		if _, statErr := os.Lstat(launcher.Path); !errors.Is(statErr, os.ErrNotExist) {
			return result, fmt.Errorf("approved launcher remains: %s", launcher.Path)
		}
	}
	for _, service := range saved.Services {
		for _, artifact := range service.Artifacts {
			if _, statErr := os.Lstat(artifact.Path); !errors.Is(statErr, os.ErrNotExist) {
				return result, fmt.Errorf("approved service artifact remains: %s", artifact.Path)
			}
		}
	}
	for _, message := range saved.Preserved {
		fmt.Fprintln(diagnostics, "Preserved:", message)
	}
	if err = uninstall.AdvanceOwnerEpoch(saved.Installation.DataHome, saved.EpochBefore, saved.EpochBefore+1); err != nil {
		return result, err
	}
	recovery, captureErr := uninstall.Capture(directory)
	if captureErr != nil {
		return result, captureErr
	}
	if err = recovery.Remove(); err != nil {
		return result, err
	}
	fmt.Fprintln(diagnostics, "Fully removed this Subyard installation and its local yards.")
	return result, nil
}

func (execution *teardownExecution) bindFullPlanContext(cli *CLI, values map[string]string) error {
	if cli.fullTeardownRecovery == "" {
		return nil
	}
	if execution.RecoveryBinding == nil {
		return errors.New("full uninstall original plan binding is unavailable")
	}
	body, err := (uninstall.RecoveryStore{Directory: cli.fullTeardownRecovery, UID: uint32(os.Getuid())}).ReadBody(*execution.RecoveryBinding)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	values["SUBYARD_TEARDOWN_PLAN"], values["SUBYARD_TEARDOWN_PLAN_SHA256"] = execution.RecoveryBinding.Path, hex.EncodeToString(digest[:])
	return nil
}
