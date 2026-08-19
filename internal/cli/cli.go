package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/credentialmeta"
	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/adapters/projectruntime"
	"github.com/Subyard/Subyard/internal/adapters/securityruntime"
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/adapters/statusruntime"
	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/audit"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/credential"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/sshidentity"
	"github.com/Subyard/Subyard/internal/state"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

var Version = "0.1.0-dev"
var operationCounter atomic.Uint64

type Options struct {
	RepositoryRoot  string
	DispatcherPath  string
	Program         string
	Arguments       []string
	Environment     []string
	WorkingDir      string
	Stdin           io.Reader
	Stdout          io.Writer
	Stderr          io.Writer
	Incus           ports.Incus
	Executor        ports.InstanceExecutor
	ProjectData     ports.YardExecutor
	ProjectDevices  ports.InstanceDeviceManager
	ProjectArchive  ports.DirectoryArchiver
	ProjectExports  ports.ProjectExportStore
	ProjectVSCode   ports.VSCode
	ProjectObserver ports.ProjectObserver
	StatusFacts     ports.StatusFactsReader
	Credentials     ports.CredentialMetadataReader
	AdapterRunner   ports.AdapterRunner
	InitPlatform    ports.InitPlatform
	RemoteControl   ports.RemoteControl
	Prompt          ports.Prompter
	Config          ports.ConfigApplier
	Clock           ports.Clock
	Audit           ports.AuditSink
}

type CLI struct {
	options                      Options
	env                          map[string]string
	baseEnv                      map[string]string
	manifest                     command.Manifest
	resources                    resource.Registry
	inventoryRoutes              map[string]config.Loaded
	discoveredOwners             map[string]ownerinventory.Connection
	coreActions                  *domain.ActionRegistry
	promptInputTerminal          func() bool
	operatorTerminal             func() bool
	openTerminal                 func() (*os.File, error)
	effectiveUID                 func() int
	retainedAdapterCompatibility bool
}

func New(options Options) (*CLI, error) {
	root, err := filepath.Abs(options.RepositoryRoot)
	if err != nil {
		return nil, err
	}
	options.RepositoryRoot = filepath.Clean(root)
	if options.Program == "" {
		options.Program = "yard"
	}
	if options.DispatcherPath == "" {
		options.DispatcherPath = options.Program
	}
	if options.Stdin == nil {
		options.Stdin = strings.NewReader("")
	}
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if options.Stderr == nil {
		options.Stderr = io.Discard
	}
	manifestFile, err := os.Open(filepath.Join(root, "config", "commands.registry"))
	if err != nil {
		return nil, fmt.Errorf("open command manifest: %w", err)
	}
	manifest, parseErr := command.Parse(manifestFile)
	closeErr := manifestFile.Close()
	if parseErr != nil {
		return nil, parseErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	resources, err := resource.Load(root)
	if err != nil {
		return nil, err
	}
	coreActions, err := application.NewCoreActionRegistry()
	if err != nil {
		return nil, fmt.Errorf("create core action registry: %w", err)
	}
	coreActions, err = coreActions.With(resources.ActionDefinitions())
	if err != nil {
		return nil, fmt.Errorf("compose resource action registry: %w", err)
	}
	for _, definition := range resources.Definitions() {
		if _, conflict := manifest.Lookup(definition.Command); conflict {
			return nil, fmt.Errorf("profile resource command conflicts with core command: %s", definition.Command)
		}
	}
	baseEnvironment := environmentMap(options.Environment)
	delete(baseEnvironment, "SUBYARD_SUDO_PREAUTHORIZED")
	activeEnvironment := make(map[string]string, len(baseEnvironment))
	for name, value := range baseEnvironment {
		activeEnvironment[name] = value
	}
	cli := &CLI{
		options: options, env: activeEnvironment, baseEnv: baseEnvironment,
		manifest: manifest, resources: resources, inventoryRoutes: make(map[string]config.Loaded),
		discoveredOwners: make(map[string]ownerinventory.Connection), coreActions: coreActions,
	}
	cli.promptInputTerminal = func() bool { return terminalStream(options.Stdin) }
	cli.operatorTerminal = func() bool {
		return terminalStream(options.Stdin) &&
			terminalStream(options.Stdout) &&
			terminalStream(options.Stderr)
	}
	cli.openTerminal = func() (*os.File, error) {
		return os.OpenFile("/dev/tty", os.O_RDWR, 0)
	}
	cli.effectiveUID = os.Geteuid
	return cli, nil
}

func (cli *CLI) Run(ctx context.Context) int {
	arguments := append([]string(nil), cli.options.Arguments...)
	yard, explicit, yes, remaining, err := parseGlobals(arguments, cli.env["SUBYARD_YARD"])
	if err != nil {
		cli.errorf("%v", err)
		return 2
	}
	if len(remaining) == 0 {
		cli.usage()
		return 0
	}
	if remaining[0] != "_migrate" && cli.env["SUBYARD_INTERNAL_MIGRATION_CHILD"] != "1" {
		if err := cli.finalizeActiveMigration(ctx); err != nil {
			cli.errorf("finish interrupted runtime migration: %v", err)
			return 1
		}
	}
	if code, handled := cli.globalQuery(remaining); handled {
		return code
	}
	name := remaining[0]
	commandArguments := append([]string(nil), remaining[1:]...)
	if yes {
		commandArguments = append([]string{"--yes"}, commandArguments...)
	}
	definition, core := cli.manifest.Lookup(name)
	resourceDefinition, profileResource := cli.resources.Lookup(name)
	if !core && !profileResource {
		cli.errorf("unknown command %q\nTry %q.", name, cli.options.Program+" --help")
		return 2
	}
	if explicit {
		cli.env["SUBYARD_YARD_EXPLICIT"] = "1"
	}
	cli.env["SUBYARD_YARD"] = yard
	if core && definition.Handler == "@help" {
		if cli.env["SUBYARD_NO_AUDIT"] == "" {
			cli.audit(name, commandArguments, yard, "")
		}
		cli.usage()
		return 0
	}
	if core && definition.Handler == "@rpc" {
		if cli.env["SUBYARD_NO_AUDIT"] == "" {
			cli.audit(name, commandArguments, yard, "")
		}
		return cli.serveRPC(ctx, yard, commandArguments)
	}
	if core && definition.Handler == "@migrate" {
		return cli.runMigration(ctx, yard, commandArguments)
	}
	if core && definition.Handler == "@test-vms" &&
		testVMLogsInvocation(commandArguments) {
		if cli.env["SUBYARD_NO_AUDIT"] == "" {
			cli.audit(name, commandArguments, "", "")
		}
		return cli.runTestVMLogs(ctx, commandArguments)
	}
	ownerDataHome := cli.env["SUBYARD_HOME"]
	if ownerDataHome == "" {
		operatorHome := cli.env["SUBYARD_OPERATOR_HOME"]
		if operatorHome == "" {
			operatorHome = cli.env["HOME"]
		}
		if operatorHome != "" {
			ownerDataHome = filepath.Join(operatorHome, ".subyard")
		}
	}
	if ownerDataHome != "" {
		if err := (ownerinventory.Connections{Root: filepath.Join(ownerDataHome, "owner-inventory")}).Recover(); err != nil {
			cli.errorf("recover owner inventory transaction: %v", err)
			return 1
		}
	}
	configSync, configSyncCheck, configSyncStatus := false, false, false
	configSyncHome := ""
	configSyncPending := false
	if core && definition.Handler == "@config" {
		configSync, configSyncCheck, configSyncStatus = configSyncInvocation(commandArguments)
		if configSync {
			operatorHome := cli.env["SUBYARD_OPERATOR_HOME"]
			if operatorHome == "" {
				operatorHome = cli.env["HOME"]
			}
			configSyncHome, err = config.ResolveConfigHome(operatorHome, cli.env)
			if err != nil {
				cli.errorf("config sync: %v", err)
				return 2
			}
			configSyncPending, err = config.PendingConfigurationTransaction(configSyncHome)
			if err != nil {
				cli.errorf("config sync: %v", err)
				return 1
			}
		}
	}
	remotePlane := command.RemoteForward
	if core {
		remotePlane = definition.Remote
	}
	var loaded config.Loaded
	var bootstrap *initBootstrap
	if core && definition.Handler == "@init" && !commandHelpRequested(commandArguments) {
		loaded, bootstrap, err = cli.loadInitContext(yard, explicit, commandArguments)
	} else if configSyncPending {
		loaded, err = cli.resolveContextAllowPending(yard)
	} else {
		loaded, err = cli.loadContext(yard)
	}
	if core && (definition.Handler == "@list" || definition.Handler == "@status" ||
		definition.Handler == "@yards") && strings.Contains(yard, "/") {
		cli.env["SUBYARD_INVENTORY_SELECTOR"] = yard
		yard = "default"
		cli.env["SUBYARD_YARD"] = yard
		loaded, err = cli.loadContext(yard)
	}
	if strings.Contains(yard, "/") && !(core && (definition.Handler == "@list" ||
		definition.Handler == "@status" || definition.Handler == "@yards")) {
		canonical := yard
		base, baseErr := cli.loadContext("default")
		if baseErr != nil {
			err = baseErr
		} else {
			var results []ownerInventoryResult
			if core && definition.Name == "remove" {
				results = cli.allOwnerInventoriesReadOnly(ctx, base)
			} else {
				results = cli.allOwnerInventories(ctx, base, false)
			}
			selected, _, selectErr := selectOwnerYards(results, canonical)
			if selectErr != nil {
				err = selectErr
			} else {
				hostID := selected[0].inventory.HostID
				yardName := selected[0].inventory.Yards[0].Name
				routeName, _, routeErr := cli.ownerYardRoute(ctx, base, hostID, yardName)
				if routeErr != nil {
					err = routeErr
				} else if dynamic, ok := cli.inventoryRoutes[routeName]; ok {
					loaded, err = dynamic, nil
				} else {
					loaded, err = cli.loadInventoryLoaded(routeName, base)
				}
			}
		}
	}
	if err != nil && core && !explicit &&
		(definition.Handler == "@list" || definition.Handler == "@yards") &&
		config.IsRetiredYardTemplate(err) {
		yard = "default"
		cli.env["SUBYARD_YARD"] = yard
		loaded, err = cli.loadContext(yard)
	}
	if err != nil {
		cli.errorf("%v", err)
		return 2
	}
	if err := configsync.RecoverHostIDRename(loaded.Context.Paths.ConfigHome); err != nil {
		cli.errorf("recover owner HostID rename: %v", err)
		return 1
	}
	loadedContext := loaded.Context
	if cli.env["SUBYARD_OPERATION_ID"] == "" {
		cli.env["SUBYARD_OPERATION_ID"] = newOperationID()
	}
	if core && definition.Handler == "@test-vms" && testVMLogsInvocation(commandArguments) {
		orchestrator := cli.operationOrchestrator(
			cli.env["SUBYARD_OPERATION_ID"], loaded, nil, &definition,
		)
		plan, planErr := orchestrator.PlanAction(
			ctx, loaded.Context, definition.Name, domain.RemotePolicy(definition.Remote),
			"test-vms.logs", domain.ActionDelta{}, yes || cli.env["ASSUME_YES"] == "1",
		)
		if planErr != nil {
			cli.errorf("plan test-vms logs: %v", planErr)
			return 1
		}
		remote := ""
		if loaded.Context.AccessKind == domain.AccessRemote {
			remote = loaded.Context.OwnerEndpoint
		}
		if cli.env["SUBYARD_NO_AUDIT"] == "" {
			cli.audit(name, commandArguments, yard, remote)
		}
		if plan.Target == domain.TargetRemoteOwner {
			return cli.forwardRemote(ctx, loaded.Context, definition.Name, commandArguments)
		}
		return cli.runTestVMLogs(ctx, commandArguments)
	}
	var projectRun *projectExecution
	var remoteRun *domain.RemotePrepared
	if !commandHelpRequested(commandArguments) {
		projectRun, err = cli.prepareProjectExecution(ctx, loaded, definition, commandArguments, explicit)
		if err != nil {
			cli.errorf("prepare %s: %v", name, err)
			return 1
		}
		if definition.Name == "remote" {
			remoteRun, err = cli.prepareRemoteExecution(ctx, loaded, commandArguments)
			if err != nil {
				cli.errorf("prepare remote: %v", err)
				return 1
			}
		}
	}
	if projectRun != nil {
		defer cli.abortProjectExecution(context.Background(), projectRun)
		loaded = projectRun.Loaded
		loadedContext = loaded.Context
		commandArguments = projectRun.Arguments
		for key, value := range projectRun.Environment {
			cli.env[key] = value
		}
	}
	remote := ""
	if loadedContext.AccessKind == domain.AccessRemote {
		remote = loadedContext.OwnerEndpoint
	}
	if name != "_info" && cli.env["SUBYARD_NO_AUDIT"] == "" {
		cli.audit(name, commandArguments, yard, remote)
	}
	if core && structuredCommandSupported(definition.Name) &&
		!commandHelpRequested(commandArguments) {
		return cli.runStructuredCommand(ctx, loaded, definition, commandArguments,
			yes || cli.env["ASSUME_YES"] == "1", projectRun, remoteRun, bootstrap)
	}
	target, routeErr := application.Route(loadedContext, domain.RemotePolicy(remotePlane))
	if routeErr != nil {
		if remotePlane == command.RemoteDeny {
			fmt.Fprintf(cli.options.Stderr, "%s is host-local — use sync or clone\n", name)
		} else {
			cli.errorf("route %s: %v", name, routeErr)
		}
		return 1
	}
	if target == domain.TargetRemoteOwner {
		if core && definition.Name == "keys" {
			return cli.runRemoteKeys(ctx, loaded, definition, commandArguments)
		}
		return cli.forwardRemote(ctx, loadedContext, name, commandArguments)
	}
	if configSync {
		configSyncTarget, configSyncRouteErr := application.Route(
			loadedContext, domain.RemoteOnOwner,
		)
		if configSyncRouteErr != nil {
			cli.errorf("route config sync: %v", configSyncRouteErr)
			return 1
		}
		if configSyncTarget == domain.TargetRemoteOwner {
			return cli.forwardRemote(ctx, loadedContext, name, commandArguments)
		}
	}
	if configSyncPending {
		switch {
		case configSyncCheck:
			cli.errorf(
				"config sync --check: interrupted transaction requires recovery by a normal config sync",
			)
			return 1
		case configSyncStatus:
			return cli.runPendingConfigSyncStatus(
				ctx, configSyncHome, configSyncStatusArguments(commandArguments),
			)
		default:
			if recoveryErr := resumeInterruptedConfigSync(configSyncHome); recoveryErr != nil {
				cli.errorf("config sync recovery: %v", recoveryErr)
				return 1
			}
			loaded, err = cli.loadContext(yard)
			if err != nil {
				cli.errorf("%v", err)
				return 2
			}
			loadedContext = loaded.Context
		}
	}
	switch definition.Handler {
	case "@check":
		return cli.runHostCheck(ctx, loaded, commandArguments)
	case "@security":
		return cli.runSecurity(ctx, loaded, commandArguments)
	case "@keys":
		return cli.runKeys(ctx, loaded, definition, commandArguments)
	case "@update":
		return cli.runUpdate(ctx, loaded, definition, commandArguments)
	case "@config":
		return cli.runConfig(ctx, loaded, commandArguments)
	case "@status":
		return cli.runStatus(ctx, loaded, explicit, commandArguments)
	case "@space":
		return cli.runSpace(ctx, loaded, explicit, commandArguments)
	case "@info":
		return cli.runOwnerInfo(ctx, loaded)
	case "@yards":
		return cli.runYards(ctx, loaded, commandArguments)
	case "@host":
		return cli.runHost(ctx, loaded, commandArguments)
	case "@authorize":
		return cli.runAuthorize(ctx, loaded, commandArguments)
	case "@logs":
		return cli.runLogs(ctx, loaded, commandArguments)
	case "@usage":
		return cli.runUsage(ctx, loaded, commandArguments)
	case "@shell":
		return cli.runShell(ctx, loaded, commandArguments, projectRun)
	case "@list":
		return cli.runProjectList(ctx, loaded, explicit, commandArguments)
	case "@state":
		return cli.runProjectState(ctx, loadedContext, commandArguments, false)
	case "@project-state":
		return cli.runProjectState(ctx, loadedContext, commandArguments, true)
	case "@remote":
		fmt.Fprintf(cli.options.Stdout, "Usage: %s remote add <name> <user@host> [--yard <yard>] | repair-key <name> | remove <name> | list\n", cli.options.Program)
		return 0
	case "@project", "@project-env":
		fmt.Fprintf(cli.options.Stdout, "Usage: %s %s\n", cli.options.Program, definition.Display)
		return 0
	case "@init":
		fmt.Fprintf(cli.options.Stdout, "Usage: %s init [--configs | --reset] [--yes]\n", cli.options.Program)
		return 0
	case "@lifecycle":
		fmt.Fprintf(cli.options.Stdout, "Usage: %s %s\n", cli.options.Program, definition.Display)
		return 0
	case "@provision":
		fmt.Fprintf(cli.options.Stdout, "Usage: %s provision [profile | --list]\n", cli.options.Program)
		return 0
	case "@test-vms":
		fmt.Fprintf(cli.options.Stdout,
			"Usage: %s test-vms <logs [-n N] [-f] [--slot N] | status | revoke --slot N | recover --slot N>\n",
			cli.options.Program)
		return 0
	case "@teardown":
		fmt.Fprintf(cli.options.Stdout, "Usage: %s teardown [--keep-data]\n", cli.options.Program)
		return 0
	}
	if profileResource {
		return cli.runResourceCommand(
			ctx, loaded, resourceDefinition, commandArguments,
			yes || cli.env["ASSUME_YES"] == "1",
		)
	}
	if definition.Handler == "@resource" {
		resourceArguments := commandArguments
		if yes && len(resourceArguments) != 0 && resourceArguments[0] == "--yes" {
			resourceArguments = resourceArguments[1:]
		}
		if len(resourceArguments) == 0 {
			cli.errorf("'svc' needs a resource name or command (see '%s --resources')", cli.options.Program)
			return 2
		}
		selected, ok := cli.resources.Lookup(resourceArguments[0])
		if !ok {
			cli.errorf("unknown resource %q (see '%s --resources')", resourceArguments[0], cli.options.Program)
			return 2
		}
		return cli.runResourceCommand(
			ctx, loaded, selected, resourceArguments[1:],
			yes || cli.env["ASSUME_YES"] == "1",
		)
	}
	path := filepath.Join(cli.options.RepositoryRoot, "scripts", definition.Handler)
	handlerArguments := commandArguments
	if definition.Arg0 != "" {
		handlerArguments = append([]string{definition.Arg0}, handlerArguments...)
	}
	code := cli.runCommand(ctx, path, handlerArguments, cli.handlerEnvironment(definition.Name, definition.Arg0))
	if code == 0 && projectRun != nil {
		if err := cli.commitProjectExecution(ctx, projectRun); err != nil {
			cli.errorf("commit %s: %v", name, err)
			return 1
		}
	}
	return code
}

// resumeInterruptedConfigSync is continuation of a prior apply, not mutation in
// the next command's preflight. configsync.Apply writes the validated journal
// only after its operation was authorized. Recover either finishes cleanup for
// that exact committed manifest or rolls back exact journal entries, refusing
// external drift. It must finish before config.Load can build the next exact
// owner-side action plan.
func resumeInterruptedConfigSync(configHome string) error {
	return configsync.Recover(configHome)
}

func configSyncInvocation(arguments []string) (bool, bool, bool) {
	if len(arguments) != 0 && (arguments[0] == "-y" || arguments[0] == "--yes") {
		arguments = arguments[1:]
	}
	if len(arguments) == 0 {
		return false, false, false
	}
	switch arguments[0] {
	case "sync":
		if len(arguments) >= 2 {
			switch arguments[1] {
			case "status":
				return true, false, true
			case "help", "--help", "-h", "path":
				return false, false, false
			}
		}
		for _, argument := range arguments[1:] {
			if argument == "--check" {
				return true, true, false
			}
		}
		return true, false, false
	default:
		return false, false, false
	}
}

func configSyncStatusArguments(arguments []string) []string {
	if len(arguments) != 0 && (arguments[0] == "-y" || arguments[0] == "--yes") {
		arguments = arguments[1:]
	}
	if len(arguments) >= 2 && arguments[0] == "sync" && arguments[1] == "status" {
		return arguments[2:]
	}
	return nil
}

func (cli *CLI) projectObserver() ports.ProjectObserver {
	if cli.options.ProjectObserver != nil {
		return cli.options.ProjectObserver
	}
	incusPort, executor := cli.statusPorts()
	return projectruntime.Runtime{Incus: incusPort, Executor: executor}
}

func (cli *CLI) projectDataPlane() ports.YardExecutor {
	if cli.options.ProjectData != nil {
		return cli.options.ProjectData
	}
	_, executor := cli.statusPorts()
	streamer, _ := executor.(ports.InstanceStreamExecutor)
	return projectruntime.Runtime{
		Executor: executor, Streamer: streamer,
		Environment: environmentList(cli.env, nil), Timeout: 10 * time.Minute, MaxBytes: 64 << 20,
	}
}

func (cli *CLI) projectArchiver() ports.DirectoryArchiver {
	if cli.options.ProjectArchive != nil {
		return cli.options.ProjectArchive
	}
	return projectruntime.TarArchiver{Environment: environmentList(cli.env, nil)}
}

func (cli *CLI) projectExportStore(loaded config.Loaded) ports.ProjectExportStore {
	if cli.options.ProjectExports != nil {
		return cli.options.ProjectExports
	}
	return projectruntime.PatchStore{Directory: filepath.Join(loaded.Context.Paths.DataHome, "exports")}
}

func (cli *CLI) projectVSCode() ports.VSCode {
	if cli.options.ProjectVSCode != nil {
		return cli.options.ProjectVSCode
	}
	program, err := exec.LookPath("code")
	if err != nil {
		return nil
	}
	return transport.Process{Program: program, Env: environmentList(cli.env, nil), MaxBytes: 4 << 20}
}

func (cli *CLI) projectDeviceManager() ports.InstanceDeviceManager {
	if cli.options.ProjectDevices != nil {
		return cli.options.ProjectDevices
	}
	incusPort, _ := cli.statusPorts()
	manager, _ := incusPort.(ports.InstanceDeviceManager)
	return manager
}

func openProjectStore(ctx context.Context, directory string) (*state.FileStore, error) {
	store, err := state.NewFileStore(directory)
	if err != nil {
		return nil, err
	}
	if _, err := store.RepairLegacyPermissions(ctx); err != nil {
		return nil, err
	}
	if _, err := store.MigrateLegacyNames(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

type readOnlyProjectStore struct{ store *state.FileStore }

func openProjectStoreReadOnly(directory string) (readOnlyProjectStore, error) {
	store, err := state.NewFileStore(directory)
	if err != nil {
		return readOnlyProjectStore{}, err
	}
	return readOnlyProjectStore{store: store}, nil
}

func (store readOnlyProjectStore) List(ctx context.Context) ([]domain.ProjectRecord, error) {
	return store.store.ListReadOnly(ctx)
}

func (store readOnlyProjectStore) Get(ctx context.Context, id string) (domain.ProjectRecord, error) {
	return store.store.GetReadOnly(ctx, id)
}

func (store readOnlyProjectStore) Put(context.Context, domain.ProjectRecord) error {
	return errors.New("read-only project store")
}

func (store readOnlyProjectStore) Delete(context.Context, string) error {
	return errors.New("read-only project store")
}

func (cli *CLI) statusPorts() (ports.Incus, ports.InstanceExecutor) {
	incusPort := cli.options.Incus
	executor := cli.options.Executor
	if incusPort == nil || executor == nil {
		client := incusclient.New(cli.env["SUBYARD_INCUS_SOCKET"], "projects")
		if incusPort == nil {
			incusPort = client
		}
		if executor == nil {
			executor = client
		}
	}
	return incusPort, executor
}

func (cli *CLI) statusFacts(loaded config.Loaded) ports.StatusFactsReader {
	if cli.options.StatusFacts != nil {
		return cli.options.StatusFacts
	}
	environment := make(map[string]string, len(loaded.Environment)+5)
	for key, value := range loaded.Environment {
		environment[key] = value
	}
	environment["SUBYARD_CONFIG_LOADED"] = "1"
	environment["SUBYARD_ENGINE_CONTEXT"] = "1"
	environment["SUBYARD_ENGINE_CONTEXT_SCHEMA"] = "1"
	environment["SUBYARD_REPOSITORY_ROOT"] = cli.options.RepositoryRoot
	environment["PROG"] = cli.options.Program
	definitions := cli.resources.Definitions()
	if profiles := strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]); len(profiles) != 0 {
		definitions = slices.DeleteFunc(definitions, func(definition resource.Definition) bool {
			return !slices.Contains(profiles, definition.Profile)
		})
	}
	incusPort, _ := cli.statusPorts()
	return statusruntime.Runtime{
		Environment: environment, Resources: definitions, Program: cli.options.Program,
		Security: securityruntime.Runtime{
			RepositoryRoot: cli.options.RepositoryRoot, Environment: loaded.Environment,
			Yard: loaded.Context, Incus: incusPort, Stdout: io.Discard, Stderr: io.Discard,
			ProxyContracts: cli.resources.ProxyContracts(strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"])),
		},
	}
}

func (cli *CLI) runStatus(
	ctx context.Context,
	loaded config.Loaded,
	explicit bool,
	arguments []string,
) int {
	all := !cli.yardSelectionExplicit(loaded, explicit)
	for _, argument := range arguments {
		switch argument {
		case "--all":
			all = true
		case "-h", "--help":
			fmt.Fprintf(cli.options.Stdout, `Usage: %s status [--all]

Without a yard selector, show the owner-inventory summary for all known yards.
With -Y/--yard or @<yard>, show detailed status for that one yard.
--all explicitly requests the summary and overrides a yard selector.
`, cli.options.Program)
			return 0
		case "--yes":
		default:
			cli.errorf("unknown option %q", argument)
			return 2
		}
	}
	if !all {
		if selector := cli.env["SUBYARD_INVENTORY_SELECTOR"]; selector != "" {
			results := cli.allOwnerInventories(ctx, loaded, false)
			selected, _, err := selectOwnerYards(results, selector)
			if err != nil {
				cli.errorf("status: %v", err)
				return 1
			}
			hostID := selected[0].inventory.HostID
			yard := selected[0].inventory.Yards[0]
			localHostID := results[0].inventory.HostID
			if hostID == localHostID {
				local, loadErr := cli.loadInventoryLoaded(yard.Name, loaded)
				if loadErr != nil {
					cli.errorf("status: %v", loadErr)
					return 1
				}
				return cli.printYardStatus(ctx, local)
			}
			status, statusErr := cli.remoteOwnerYardStatus(ctx, loaded, hostID, yard.Name)
			if statusErr != nil {
				cli.errorf("remote status: %v", statusErr)
				return 1
			}
			fmt.Fprintf(cli.options.Stdout, "%s/%s  %s\n", hostID, yard.Name, status.State)
			fmt.Fprintf(cli.options.Stdout, "  projects %d\n", status.ProjectCount)
			fmt.Fprintf(cli.options.Stdout, "  ssh      %s:%d\n",
				status.Context.DevUser, status.Context.SSHPort)
			return 0
		}
		if loaded.Context.AccessKind == domain.AccessRemote {
			status, err := cli.remoteYardStatus(ctx, loaded.Context)
			if err != nil {
				cli.errorf("remote status: %v", err)
				return 1
			}
			fmt.Fprintf(cli.options.Stdout, "%s/%s  %s\n",
				loaded.Context.OwnerEndpoint, status.Context.YardName, status.State)
			fmt.Fprintf(cli.options.Stdout, "  projects %d\n", status.ProjectCount)
			fmt.Fprintf(cli.options.Stdout, "  ssh      %s:%d\n",
				status.Context.DevUser, status.Context.SSHPort)
			return 0
		}
		return cli.printYardStatus(ctx, loaded)
	}
	results := cli.allOwnerInventories(ctx, loaded, false)
	code := 0
	first := true
	for _, result := range results {
		if result.err != nil {
			code = 1
			fmt.Fprintf(cli.options.Stderr, "Warning: owner inventory refresh: %v\n", result.err)
		}
		for _, yard := range result.inventory.Yards {
			if !first {
				fmt.Fprintln(cli.options.Stdout)
			}
			first = false
			fmt.Fprintf(cli.options.Stdout, "%s/%s  %s\n",
				result.inventory.HostID, yard.Name, yard.State)
			fmt.Fprintf(cli.options.Stdout, "  instance %s (%s)\n", yard.Instance, yard.Kind)
			fmt.Fprintf(cli.options.Stdout, "  ssh      %s:%d\n", yard.DevUser, yard.SSHPort)
			fmt.Fprintf(cli.options.Stdout, "  projects %d\n", len(yard.Projects))
		}
	}
	return code
}

func (cli *CLI) printYardStatus(ctx context.Context, loaded config.Loaded) int {
	store, err := openProjectStore(ctx, loaded.Context.Paths.StateDir)
	if err != nil {
		cli.errorf("open project state: %v", err)
		return 1
	}
	incusPort, executor := cli.statusPorts()
	service := application.StatusService{
		Incus: incusPort, Executor: executor, Store: store, Facts: cli.statusFacts(loaded),
	}
	status, err := service.Read(ctx, loaded.Context)
	if err != nil {
		cli.errorf("status: %v", err)
		return 1
	}
	label := loaded.Context.YardName
	if label == "default" {
		label = "yard"
	}
	fmt.Fprintf(cli.options.Stdout, "%s  %s\n", label, status.State)
	fmt.Fprintf(cli.options.Stdout, "  desired  %s  (initialized=%s, incus-autostart=%s)\n",
		status.Desired, status.Initialized, status.IncusAutostart)
	resolvedImage := string(status.ResolvedYardImage)
	if resolvedImage == "" {
		resolvedImage = "unknown"
	}
	fmt.Fprintf(cli.options.Stdout, "  image    desired=%s resolved=%s\n",
		status.Context.YardImageRef, resolvedImage)
	if status.State == "RUNNING" {
		ip := status.IP
		if ip == "" {
			ip = "—"
		}
		fmt.Fprintf(cli.options.Stdout, "  ip       %s\n", ip)
	}
	identityState := sshidentity.Classify(
		status.Context.Paths.OperatorHome, status.Context.Paths.DataHome, status.Context.YardName,
	)
	if status.SSHConfigured {
		fmt.Fprintf(cli.options.Stdout, "  ssh      127.0.0.1:%d  (ssh %s)\n",
			status.Context.SSHPort, status.Context.SSHHost)
	} else {
		fmt.Fprintf(cli.options.Stdout, "  ssh      not set up  (run: %s init)\n",
			cli.yardHint(status.Context))
	}
	fmt.Fprintf(cli.options.Stdout, "  ssh-id   %s\n", identityState)
	mounts := "none"
	if len(status.Mounts) != 0 {
		mounts = strings.Join(status.Mounts, " ")
	}
	fmt.Fprintf(cli.options.Stdout, "  mounts   %s\n", mounts)
	if status.State == "RUNNING" {
		fmt.Fprintf(cli.options.Stdout, "  services ssh/docker = %s\n", status.Services)
		forward := "off"
		if status.Context.ForwardSSHAgent {
			forward = "on"
		}
		fmt.Fprintf(cli.options.Stdout, "  vscode   %s agent-fwd=%s  (yard code <project>)\n",
			status.VSCode, forward)
	}
	fmt.Fprintf(cli.options.Stdout, "  projects %d  (%s list)\n", status.ProjectCount, cli.yardHint(status.Context))
	if len(status.Facts.Shared) == 0 {
		fmt.Fprintln(cli.options.Stdout, "  shared   none")
	} else {
		fmt.Fprintln(cli.options.Stdout, "  shared:")
		for _, shared := range status.Facts.Shared {
			if shared.Hint == "" {
				fmt.Fprintf(cli.options.Stdout, "    %-9s %-16s %s\n", shared.Profile, shared.Name, shared.State)
			} else {
				fmt.Fprintf(cli.options.Stdout, "    %-9s %-16s %-5s (%s)\n",
					shared.Profile, shared.Name, shared.State, shared.Hint)
			}
		}
	}
	switch status.Facts.Security {
	case "live":
		fmt.Fprintln(cli.options.Stdout, "  security ok (live)")
	case "static-only":
		fmt.Fprintln(cli.options.Stdout, "  security static-only")
	default:
		fmt.Fprintf(cli.options.Stdout, "  security FAIL  (inspect: %s security)\n", cli.yardHint(status.Context))
	}
	fmt.Fprintf(cli.options.Stdout, "  space    %s\n", status.Facts.Space)
	return 0
}

type ownerInfo = domain.RemoteInfo

func (cli *CLI) runOwnerInfo(ctx context.Context, loaded config.Loaded) int {
	yard := loaded.Context
	info := ownerInfo{
		YardName: yard.YardName, AccessKind: string(domain.AccessLocal), Version: Version,
		YardInstanceName: yard.YardInstanceName, IncusProject: yard.IncusProject, State: "UNKNOWN",
		SSHHost: yard.SSHHost, SSHPort: yard.SSHPort, DevUser: yard.DevUser,
	}
	incusPort, _ := cli.statusPorts()
	if _, err := incusPort.Server(ctx); err == nil {
		info.State = "STOPPED"
		if instance, instanceErr := incusPort.Instance(ctx, yard.IncusProject, yard.YardInstanceName); instanceErr == nil {
			if state := strings.ToUpper(strings.TrimSpace(instance.Status)); state != "" {
				info.State = state
			}
		}
	}
	if info.State == "RUNNING" {
		observation, err := cli.projectObserver().Observe(ctx, yard, nil, true)
		if err == nil && observation.Reached {
			ids := make(map[string]struct{}, len(observation.Live))
			for _, record := range observation.Live {
				ids[record.ProjectID] = struct{}{}
			}
			count := len(ids)
			info.Projects = &count
		}
	}
	if err := json.NewEncoder(cli.options.Stdout).Encode(info); err != nil {
		cli.errorf("write owner info: %v", err)
		return 1
	}
	return 0
}

func (cli *CLI) runYards(ctx context.Context, loaded config.Loaded, arguments []string) int {
	verbose, jsonOutput := false, false
	for _, argument := range arguments {
		switch argument {
		case "-y", "--yes":
		case "-v", "--verbose":
			verbose = true
		case "--json":
			jsonOutput = true
		case "-h", "--help":
			fmt.Fprintf(cli.options.Stdout, "Usage: %s yards [--verbose | --json]\n", cli.options.Program)
			return 0
		default:
			cli.errorf("yards accepts only --verbose or --json")
			return 2
		}
	}
	if verbose && jsonOutput {
		cli.errorf("yards --verbose and --json are mutually exclusive")
		return 2
	}
	results := cli.allOwnerInventories(ctx, loaded, false)
	type yardOutput struct {
		YardRef           domain.YardRef           `json:"yardRef"`
		AccessKind        domain.AccessKind        `json:"accessKind"`
		YardKind          domain.YardKind          `json:"yardKind"`
		YardInstanceName  string                   `json:"yardInstanceName"`
		State             string                   `json:"state"`
		Projects          int                      `json:"projects"`
		YardImageRef      domain.YardImageRef      `json:"yardImageRef,omitempty"`
		ResolvedYardImage domain.ResolvedYardImage `json:"resolvedYardImage,omitempty"`
		OwnerEndpoint     string                   `json:"ownerEndpoint,omitempty"`
	}
	var rows []yardOutput
	code := 0
	localHostID := ""
	if len(results) != 0 {
		localHostID = results[0].inventory.HostID
	}
	endpoints := make(map[string]string)
	connections, connectionErr := (ownerinventory.Connections{
		Root: loaded.Context.Paths.DataHome + "/owner-inventory",
	}).List()
	if connectionErr == nil {
		for _, connection := range connections {
			endpoints[connection.HostID] = connection.Destination
		}
	}
	for _, result := range results {
		if result.err != nil {
			code = 1
			owner := result.inventory.HostID
			if owner == "" {
				owner = "unknown owner"
			}
			marker := "unavailable"
			if result.stale {
				marker = "stale, age " + ageHuman(time.Since(result.fetchedAt))
			}
			fmt.Fprintf(cli.options.Stderr, "Warning: %s inventory is %s: %v\n",
				owner, marker, result.err)
		}
		for _, yard := range result.inventory.Yards {
			accessKind := domain.AccessRemote
			if result.inventory.HostID == localHostID {
				accessKind = domain.AccessLocal
			}
			stateValue := yard.State
			if stateValue == "" {
				stateValue = "?"
			}
			rows = append(rows, yardOutput{
				YardRef:    domain.YardRef{HostID: result.inventory.HostID, YardName: yard.Name},
				AccessKind: accessKind, YardKind: domain.YardKind(yard.Kind),
				YardInstanceName: yard.Instance, State: stateValue, Projects: len(yard.Projects),
				YardImageRef: yard.YardImageRef, ResolvedYardImage: yard.ResolvedYardImage,
				OwnerEndpoint: endpoints[result.inventory.HostID],
			})
		}
	}
	if jsonOutput {
		if err := json.NewEncoder(cli.options.Stdout).Encode(rows); err != nil {
			cli.errorf("write yards JSON: %v", err)
			return 1
		}
		return code
	}
	if verbose {
		fmt.Fprintf(cli.options.Stdout, "%-24s %-6s %-9s %-16s %-9s %-8s %-24s %-24s %s\n",
			"NAME", "ACCESS", "KIND", "INSTANCE", "STATE", "PROJECTS", "DESIRED IMAGE", "RESOLVED IMAGE", "OWNER ENDPOINT")
		for _, row := range rows {
			fmt.Fprintf(cli.options.Stdout, "%-24s %-6s %-9s %-16s %-9s %-8d %-24s %-24s %s\n",
				row.YardRef.String(), row.AccessKind, row.YardKind, row.YardInstanceName, row.State,
				row.Projects, row.YardImageRef, row.ResolvedYardImage, row.OwnerEndpoint)
		}
		return code
	}
	fmt.Fprintf(cli.options.Stdout, "%-24s %-6s %-9s %-16s %-9s %s\n",
		"NAME", "ACCESS", "KIND", "INSTANCE", "STATE", "PROJECTS")
	for _, row := range rows {
		fmt.Fprintf(cli.options.Stdout, "%-24s %-6s %-9s %-16s %-9s %d\n",
			row.YardRef.String(), row.AccessKind, row.YardKind, row.YardInstanceName, row.State, row.Projects)
	}
	return code
}

func (cli *CLI) runAuthorize(ctx context.Context, loaded config.Loaded, arguments []string) int {
	if len(arguments) != 0 {
		cli.errorf("_authorize takes no arguments")
		return 2
	}
	scanner := bufio.NewScanner(cli.options.Stdin)
	publicKey := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			publicKey = line
			break
		}
	}
	if err := scanner.Err(); err != nil {
		cli.errorf("_authorize: read public key: %v", err)
		return 1
	}
	if _, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(publicKey)); err != nil || len(bytes.TrimSpace(rest)) != 0 {
		cli.errorf("_authorize: stdin does not contain a supported SSH public key")
		return 2
	}
	yard := loaded.Context
	incusPort, executor := cli.statusPorts()
	instance, err := incusPort.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
	if err != nil {
		cli.errorf("_authorize: instance %q is unavailable: %v", yard.YardInstanceName, err)
		return 1
	}
	if !strings.EqualFold(instance.Status, "running") {
		cli.errorf("_authorize: yard %q is not running", yard.YardInstanceName)
		return 1
	}
	result, err := executor.Exec(ctx, yard.IncusProject, yard.YardInstanceName, ports.InstanceExecRequest{
		Command: []string{"sh", "-eu", "-c", `
home="$(getent passwd "$DEV_USER" | cut -d: -f6)"
[ -n "$home" ]
install -d -m 700 -o "$DEV_USER" -g "$DEV_USER" "$home/.ssh"
ak="$home/.ssh/authorized_keys"
touch "$ak"
if grep -qxF "$PUBKEY" "$ak"; then printf already; else printf '%s\n' "$PUBKEY" >> "$ak"; printf added; fi
chmod 600 "$ak"
chown "$DEV_USER:$DEV_USER" "$ak"`},
		Environment: map[string]string{"PUBKEY": publicKey, "DEV_USER": yard.DevUser},
	})
	if err != nil || result.ExitCode != 0 {
		cli.errorf("_authorize: could not update authorized_keys")
		return 1
	}
	action := strings.TrimSpace(string(result.Stdout))
	if action != "added" && action != "already" {
		cli.errorf("_authorize: unexpected guest result")
		return 1
	}
	message := "authorized"
	if action == "already" {
		message = "already authorized"
	}
	fmt.Fprintf(cli.options.Stderr, "  [ ok ] controller key %s for %s in %s\n",
		message, yard.DevUser, yard.YardInstanceName)
	return 0
}

func (cli *CLI) runLogs(ctx context.Context, loaded config.Loaded, arguments []string) int {
	journalArguments, help, err := parseLogArguments(arguments)
	if err != nil {
		cli.errorf("logs: %v", err)
		return 2
	}
	if help {
		fmt.Fprintf(cli.options.Stdout, "Usage: %s logs [-f] [-n LINES] [UNIT]\n", cli.options.Program)
		return 0
	}
	yard := loaded.Context
	incusPort, _ := cli.statusPorts()
	instance, err := incusPort.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
	if err != nil {
		cli.errorf("logs: instance %q is unavailable: %v", yard.YardInstanceName, err)
		return 1
	}
	if !strings.EqualFold(instance.Status, "running") {
		cli.errorf("logs: yard is not running")
		return 1
	}
	commandArguments := []string{"exec", yard.YardInstanceName, "--project", yard.IncusProject, "--"}
	commandArguments = append(commandArguments, journalArguments...)
	return cli.runExternal(ctx, "incus", commandArguments)
}

func parseLogArguments(arguments []string) ([]string, bool, error) {
	follow := false
	lines := 200
	unit := ""
	for index := 0; index < len(arguments); index++ {
		switch argument := arguments[index]; argument {
		case "-f":
			follow = true
		case "-n":
			index++
			if index >= len(arguments) {
				return nil, false, errors.New("-n needs a positive number")
			}
			value, err := strconv.Atoi(arguments[index])
			if err != nil || value < 1 {
				return nil, false, errors.New("-n needs a positive number")
			}
			lines = value
		case "-y", "--yes":
		case "-h", "--help":
			return nil, true, nil
		default:
			if strings.HasPrefix(argument, "-") {
				return nil, false, fmt.Errorf("unknown option %q", argument)
			}
			if unit != "" {
				return nil, false, errors.New("logs accepts at most one unit")
			}
			unit = argument
		}
	}
	result := []string{"journalctl", "-n", strconv.Itoa(lines)}
	if unit != "" {
		result = append(result, "-u", unit)
	}
	if follow {
		result = append(result, "-f")
	} else {
		result = append(result, "--no-pager")
	}
	return result, false, nil
}

func (cli *CLI) runUsage(ctx context.Context, loaded config.Loaded, arguments []string) int {
	filtered, help := parseUsageArguments(arguments)
	if help {
		fmt.Fprintf(cli.options.Stdout, "Usage: %s usage [CCUSAGE ARG...]\n", cli.options.Program)
		return 0
	}
	yard := loaded.Context
	if !cli.incusCLIYardRunning(ctx, yard) {
		cli.errorf("usage: yard is not running")
		return 1
	}
	probe := exec.CommandContext(ctx, "incus", "exec", yard.YardInstanceName, "--project", yard.IncusProject,
		"--", "sh", "-eu", "-c", "[ -f /usr/local/bin/ccusage ] && [ ! -L /usr/local/bin/ccusage ] && [ -x /usr/local/bin/ccusage ]")
	probe.Env = environmentList(cli.env, nil)
	if err := probe.Run(); err != nil {
		hint := cli.env["SUBYARD_USAGE_REPAIR_HINT"]
		if hint == "" {
			hint = cli.yardHint(yard) + " init"
		}
		cli.errorf("usage: /usr/local/bin/ccusage is missing or not executable; repair with: %s", hint)
		return 1
	}
	return cli.runExternal(ctx, "incus", usageExecArguments(yard, filtered))
}

func parseUsageArguments(arguments []string) ([]string, bool) {
	filtered := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		switch argument {
		case "-y", "--yes":
		case "-h", "--help":
			return nil, true
		default:
			filtered = append(filtered, argument)
		}
	}
	return filtered, false
}

func usageExecArguments(yard domain.Context, arguments []string) []string {
	home := "/home/" + yard.DevUser
	result := []string{
		"exec", yard.YardInstanceName, "--project", yard.IncusProject,
		"--user", strconv.Itoa(yard.DevUID), "--group", strconv.Itoa(yard.DevUID),
		"--cwd", home, "--env", "HOME=" + home, "--env", "USER=" + yard.DevUser,
		"--", "/usr/local/bin/ccusage",
	}
	return append(result, arguments...)
}

func (cli *CLI) runShell(
	ctx context.Context,
	loaded config.Loaded,
	arguments []string,
	project *projectExecution,
) int {
	root, selector, guestCommand, help, err := parseShellArguments(arguments)
	if err != nil {
		cli.errorf("shell: %v", err)
		return 2
	}
	if help {
		fmt.Fprintf(cli.options.Stdout,
			"Usage: %s shell [--root] [PROJECT] [-- COMMAND...]\n", cli.options.Program)
		return 0
	}
	yard := loaded.Context
	home := "/home/" + yard.DevUser
	cwd := home
	if selector != "" {
		if project == nil || project.Record.YardPath == "" {
			cli.errorf("shell: project %q has no yard path", selector)
			return 1
		}
		cwd = project.Record.YardPath
	}
	if !cli.incusCLIYardRunning(ctx, yard) {
		cli.errorf("shell: yard is not running - start it: %s start", cli.yardHint(yard))
		return 1
	}
	return cli.runExternal(ctx, "incus", shellExecArguments(yard, root, cwd, guestCommand))
}

func shellExecArguments(yard domain.Context, root bool, cwd string, guestCommand []string) []string {
	uid := yard.DevUID
	userArguments := []string{
		"--user", strconv.Itoa(uid), "--group", strconv.Itoa(uid),
		"--env", "HOME=/home/" + yard.DevUser,
	}
	if root {
		userArguments = []string{"--user", "0", "--group", "0", "--env", "HOME=/root"}
	}
	result := []string{"exec", yard.YardInstanceName, "--project", yard.IncusProject}
	result = append(result, userArguments...)
	result = append(result, "--cwd", cwd)
	if len(guestCommand) == 0 {
		return append(result, "-t", "--", "bash", "-l")
	}
	result = append(result, "--")
	return append(result, guestCommand...)
}

func (cli *CLI) incusCLIYardRunning(ctx context.Context, yard domain.Context) bool {
	command := exec.CommandContext(ctx, "incus", "list", yard.YardInstanceName,
		"--project", yard.IncusProject, "-f", "csv", "-c", "s")
	command.Env = environmentList(cli.env, nil)
	state, err := command.Output()
	return err == nil && strings.EqualFold(strings.TrimSpace(string(state)), "running")
}

func parseShellArguments(arguments []string) (root bool, selector string, command []string, help bool, err error) {
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch argument {
		case "-y", "--yes":
		case "-h", "--help":
			return false, "", nil, true, nil
		case "--root":
			root = true
		case "--":
			return root, selector, append([]string(nil), arguments[index+1:]...), false, nil
		default:
			if strings.HasPrefix(argument, "-") {
				return false, "", nil, false, fmt.Errorf("unknown option %q", argument)
			}
			if selector != "" {
				return false, "", nil, false,
					errors.New("only one project may be selected; put commands after '--'")
			}
			selector = argument
		}
	}
	return root, selector, nil, false, nil
}

func (cli *CLI) yardHint(yard domain.Context) string {
	if yard.YardName == "default" {
		return cli.options.Program
	}
	return fmt.Sprintf("%s -Y %s", cli.options.Program, yard.YardName)
}

func ageHuman(age time.Duration) string {
	seconds := int64(age.Seconds())
	if seconds < 0 {
		seconds = 0
	}
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh", seconds/3600)
	default:
		return fmt.Sprintf("%dd", seconds/86400)
	}
}

func (cli *CLI) yardSelectionExplicit(loaded config.Loaded, explicit bool) bool {
	return explicit || cli.env["SUBYARD_YARD_EXPLICIT"] != "" ||
		loaded.Context.YardName != "default"
}

func (cli *CLI) runProjectList(
	ctx context.Context,
	loaded config.Loaded,
	explicit bool,
	arguments []string,
) int {
	live := false
	completion := ""
	for _, argument := range arguments {
		switch argument {
		case "--live":
			live = true
		case "--complete-yards":
			completion = "yards"
		case "--complete-projects":
			completion = "projects"
		case "-h", "--help":
			fmt.Fprintf(cli.options.Stdout, "Usage: %s list [--live]\n", cli.options.Program)
			return 0
		case "--yes":
		default:
			cli.errorf("unknown option %q", argument)
			return 2
		}
	}
	results := cli.allOwnerInventories(ctx, loaded, live)
	if completion != "" {
		printOwnerCompletions(cli.options.Stdout, results, completion)
		return 0
	}
	selector := cli.env["SUBYARD_INVENTORY_SELECTOR"]
	explicit = cli.yardSelectionExplicit(loaded, explicit)
	if selector == "" && explicit {
		selector = loaded.Context.YardName
		if loaded.Context.AccessKind == domain.AccessRemote && loaded.Context.OwnerYardName != "" {
			selector = loaded.Context.OwnerYardName
		}
	}
	selected, _, err := selectOwnerYards(results, selector)
	if err != nil {
		cli.errorf("list projects: %v", err)
		return 1
	}
	type row struct {
		owner, yard string
		project     domain.OwnerProject
	}
	var rows []row
	fatal := false
	for _, result := range selected {
		if result.err != nil {
			if result.inventory.HostID == "" || errors.Is(result.err, ownerinventory.ErrIntegrity) {
				fatal = true
			}
			owner := result.inventory.HostID
			if owner == "" {
				owner = "unknown owner"
			}
			if result.stale {
				fmt.Fprintf(cli.options.Stderr,
					"Warning: %s inventory is stale (age %s): %v\n",
					owner, ageHuman(time.Since(result.fetchedAt)), result.err)
			} else {
				fmt.Fprintf(cli.options.Stderr, "Warning: %s inventory is unavailable: %v\n",
					owner, result.err)
			}
		}
		for _, yard := range result.inventory.Yards {
			for _, project := range yard.Projects {
				rows = append(rows, row{
					owner: result.inventory.HostID, yard: yard.Name, project: project,
				})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].owner != rows[j].owner {
			return rows[i].owner < rows[j].owner
		}
		if rows[i].yard != rows[j].yard {
			return rows[i].yard < rows[j].yard
		}
		if rows[i].project.Name != rows[j].project.Name {
			return rows[i].project.Name < rows[j].project.Name
		}
		return rows[i].project.ProjectID < rows[j].project.ProjectID
	})
	if len(rows) == 0 {
		fmt.Fprintln(cli.options.Stdout, "No projects in the selected owner inventory.")
		if fatal {
			return 1
		}
		return 0
	}
	fmt.Fprintf(cli.options.Stdout, "%-24s %-6s %-10s %-20s %s\n",
		"NAME", "MODE", "TARGET", "OWNER", "YARD")
	for _, row := range rows {
		target := row.project.Target
		if target == "" {
			target = "yard"
		}
		printProjectListRow(cli.options.Stdout,
			row.project.Name, row.project.Mode, target, row.owner, row.yard)
	}
	if fatal {
		return 1
	}
	return 0
}

func (cli *CLI) loadInventoryContext(name string, loaded config.Loaded) (domain.Context, error) {
	contextLoaded, err := cli.loadInventoryLoaded(name, loaded)
	return contextLoaded.Context, err
}

func (cli *CLI) loadInventoryLoaded(name string, loaded config.Loaded) (config.Loaded, error) {
	environment := make(map[string]string, len(cli.baseEnv))
	for key, value := range cli.baseEnv {
		environment[key] = value
	}
	for _, key := range []string{
		"SUBYARD_YARD", "YARD_NAME", "ACCESS_KIND", "OWNER_ENDPOINT", "OWNER_YARD_NAME",
	} {
		delete(environment, key)
	}
	return config.Load(config.LoadOptions{
		RepositoryRoot: cli.options.RepositoryRoot,
		OperatorHome:   loaded.Context.Paths.OperatorHome,
		YardName:       name,
		Environment:    environment,
	})
}

func (cli *CLI) runProjectState(
	ctx context.Context,
	yard domain.Context,
	arguments []string,
	ownerEndpoint bool,
) int {
	store, err := openProjectStore(ctx, yard.Paths.StateDir)
	if err != nil {
		cli.errorf("open project state: %v", err)
		return 1
	}
	service := state.Service{Store: store}
	if ownerEndpoint {
		return cli.runOwnerProjectState(ctx, service, yard, arguments)
	}
	if len(arguments) == 0 {
		cli.errorf("internal: _state needs an action")
		return 2
	}
	action := arguments[0]
	arguments = arguments[1:]
	fail := func(err error) int {
		cli.errorf("project state %s: %v", action, err)
		return 1
	}
	switch action {
	case "validate":
		if len(arguments) != 0 {
			return fail(errors.New("validate takes no arguments"))
		}
		if err := service.Validate(ctx); err != nil {
			return fail(err)
		}
	case "ids":
		if len(arguments) != 0 {
			return fail(errors.New("ids takes no arguments"))
		}
		records, err := store.List(ctx)
		if err != nil {
			return fail(err)
		}
		for _, record := range records {
			fmt.Fprintln(cli.options.Stdout, record.ProjectID)
		}
	case "validate-file":
		if len(arguments) != 2 {
			return fail(errors.New("validate-file needs path and expected ID"))
		}
		if err := store.ValidateFile(arguments[0], arguments[1]); err != nil {
			return fail(err)
		}
	case "project-id":
		if len(arguments) != 1 {
			return fail(errors.New("project-id needs a path"))
		}
		path := arguments[0]
		if !filepath.IsAbs(path) && cli.options.WorkingDir != "" {
			path = filepath.Join(cli.options.WorkingDir, path)
		}
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fail(err)
		}
		realPath, err = filepath.Abs(realPath)
		if err != nil {
			return fail(err)
		}
		records, err := store.List(ctx)
		if err != nil {
			return fail(err)
		}
		matches := recordsBySource(records, realPath)
		if len(matches) != 1 {
			if len(matches) == 0 {
				return fail(fmt.Errorf("project path %q is not registered", arguments[0]))
			}
			return fail(fmt.Errorf("%w: project path %q has %d records",
				state.ErrAmbiguous, arguments[0], len(matches)))
		}
		fmt.Fprintln(cli.options.Stdout, matches[0].ProjectID)
	case "yard-path":
		if len(arguments) != 1 || !domain.SafeID(arguments[0]) {
			return fail(errors.New("yard-path needs a valid project ID"))
		}
		fmt.Fprintln(cli.options.Stdout, state.YardPath(arguments[0]))
	case "device":
		if len(arguments) != 1 || !domain.SafeID(arguments[0]) {
			return fail(errors.New("device needs a valid project ID"))
		}
		record, err := store.Get(ctx, arguments[0])
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(cli.options.Stdout, state.WorkspaceDeviceFor(record))
	case "valid":
		if len(arguments) != 2 || !validStateValue(arguments[0], arguments[1]) {
			return 1
		}
	case "resolve-local", "resolve-local-soft":
		if len(arguments) != 1 {
			return fail(errors.New("resolve-local needs a selector"))
		}
		match, err := cli.resolveLocalProject(ctx, yard, store, arguments[0])
		if err != nil {
			if action == "resolve-local-soft" {
				return 1
			}
			return fail(err)
		}
		fmt.Fprintln(cli.options.Stdout, match.Record.ProjectID)
	case "resolve-global":
		if len(arguments) != 1 {
			return fail(errors.New("resolve-global needs a selector"))
		}
		match, err := cli.resolveGlobalProject(ctx, yard, arguments[0])
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(cli.options.Stdout, "%s\t%s\n", match.Yard, match.Record.ProjectID)
	case "route-sync":
		if len(arguments) != 2 {
			return fail(errors.New("route-sync needs a canonical source and target yard"))
		}
		target, err := cli.routeProjectSource(ctx, yard, arguments[0], arguments[1])
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(cli.options.Stdout, target)
	case "exists":
		if len(arguments) != 1 {
			return 2
		}
		if _, err := store.Get(ctx, arguments[0]); err != nil {
			if errors.Is(err, state.ErrNotFound) {
				return 1
			}
			return fail(err)
		}
	case "get":
		if len(arguments) != 2 {
			return fail(errors.New("get needs ID and field"))
		}
		record, err := store.Get(ctx, arguments[0])
		if err != nil {
			return fail(err)
		}
		value, err := state.Field(record, arguments[1])
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(cli.options.Stdout, value)
	case "remove":
		if len(arguments) != 1 {
			return fail(errors.New("remove needs ID"))
		}
		if err := store.Delete(ctx, arguments[0]); err != nil {
			return fail(err)
		}
	case "write":
		if len(arguments) != 6 {
			return fail(errors.New("write needs ID, name, host path, yard path, mode and SSH host"))
		}
		if err := service.Write(ctx, arguments[0], arguments[1], arguments[2], arguments[3],
			domain.ProjectMode(arguments[4]), arguments[5], time.Now().UTC().Format(time.RFC3339)); err != nil {
			return fail(err)
		}
	case "set":
		if len(arguments) != 3 {
			return fail(errors.New("set needs ID, field and value"))
		}
		if err := service.Set(ctx, arguments[0], arguments[1], arguments[2]); err != nil {
			return fail(err)
		}
	case "upsert-yard":
		if len(arguments) != 5 {
			return fail(errors.New("upsert-yard needs ID, name, mode, target and SSH host"))
		}
		if err := service.UpsertYard(ctx, arguments[0], arguments[1], domain.ProjectMode(arguments[2]),
			arguments[3], arguments[4]); err != nil {
			return fail(err)
		}
	default:
		return fail(fmt.Errorf("unknown action %q", action))
	}
	return 0
}

func (cli *CLI) runMigration(ctx context.Context, yard string, arguments []string) int {
	if len(arguments) == 3 && arguments[0] == "normalize-yard-config" {
		if err := migration.NormalizeLegacyYardConfig(arguments[1], arguments[2]); err != nil {
			cli.errorf("source-install yard config normalization: %v", err)
			return 1
		}
		return 0
	}
	if (len(arguments) == 2 || len(arguments) == 4) && arguments[0] == "overlay-manifest" {
		var manifest migration.SourceInstallManifest
		var err error
		if len(arguments) == 2 {
			manifest, err = migration.DiscoverSourceInstall(arguments[1])
		} else {
			manifest, err = migration.DiscoverSourceInstallWithRoots(
				arguments[1], arguments[2], arguments[3],
			)
		}
		if err != nil {
			cli.errorf("source-install overlay manifest: %v", err)
			return 1
		}
		if err := json.NewEncoder(cli.options.Stdout).Encode(manifest); err != nil {
			cli.errorf("source-install overlay manifest: %v", err)
			return 1
		}
		return 0
	}
	if len(arguments) != 1 ||
		(arguments[0] != "paths" && arguments[0] != "check" &&
			arguments[0] != "apply" && arguments[0] != "finalize" &&
			arguments[0] != "rollback" && arguments[0] != "cleanup" &&
			arguments[0] != "reconcile-test-vm-broker" &&
			arguments[0] != "reconcile-power-reconciler") {
		cli.errorf("internal: invalid _migrate action")
		return 2
	}
	repositoryRoot := cli.options.RepositoryRoot
	if arguments[0] == "reconcile-power-reconciler" {
		payloadRepositoryRoot, payloadErr := powerMigrationRepositoryRoot(
			repositoryRoot,
			cli.baseEnv,
		)
		if payloadErr != nil {
			cli.errorf("state migration power reconciler payload: %v", payloadErr)
			return 1
		}
		repositoryRoot = payloadRepositoryRoot
	}
	migrationEnvironment := freshMigrationEnvironment(
		cli.baseEnv,
		repositoryRoot,
	)
	operatorHome := migrationEnvironment["SUBYARD_OPERATOR_HOME"]
	if operatorHome == "" {
		operatorHome = migrationEnvironment["HOME"]
	}
	loaded, err := config.Load(config.LoadOptions{
		RepositoryRoot: repositoryRoot,
		OperatorHome:   operatorHome,
		YardName:       yard,
		Environment:    migrationEnvironment,
	})
	if err != nil {
		cli.errorf("state migration context: %v", err)
		return 1
	}
	environment := loaded.Environment
	operatorHome = environment["SUBYARD_OPERATOR_HOME"]
	if operatorHome == "" {
		operatorHome = environment["HOME"]
	}
	configHome := environment["SUBYARD_CONFIG_HOME"]
	if configHome == "" {
		configHome = filepath.Join(operatorHome, ".config", "subyard")
	}
	projectDirectories := make([]string, 0, 4)
	seenDirectory := make(map[string]struct{})
	addDirectory := func(directory string) {
		directory = filepath.Clean(directory)
		if _, exists := seenDirectory[directory]; exists {
			return
		}
		seenDirectory[directory] = struct{}{}
		projectDirectories = append(projectDirectories, directory)
	}
	addDirectory(filepath.Join(configHome, "projects"))
	if explicit := environment["SUBYARD_STATE_DIR"]; explicit != "" {
		addDirectory(explicit)
	}
	named, _ := filepath.Glob(filepath.Join(configHome, "yards", "*", "projects"))
	for _, directory := range named {
		addDirectory(directory)
	}
	if arguments[0] == "paths" {
		payload := struct {
			OperatorHome       string   `json:"operatorHome"`
			ConfigHome         string   `json:"configHome"`
			DataHome           string   `json:"dataHome"`
			ProjectDirectories []string `json:"projectDirectories"`
		}{
			OperatorHome: operatorHome, ConfigHome: configHome,
			DataHome: environment["SUBYARD_HOME"], ProjectDirectories: projectDirectories,
		}
		if err := json.NewEncoder(cli.options.Stdout).Encode(payload); err != nil {
			cli.errorf("state migration paths: %v", err)
			return 1
		}
		return 0
	}
	if arguments[0] == "reconcile-test-vm-broker" {
		if migrationEnvironment["SUBYARD_INTERNAL_MIGRATION_CHILD"] != "1" {
			cli.errorf("internal: test VM broker migration child is required")
			return 1
		}
		if !loaded.Context.NestedE2EVMs ||
			loaded.Context.YardKind != domain.YardContainer {
			cli.errorf("state migration test VM broker context is not active")
			return 1
		}
		platform := cli.options.InitPlatform
		if platform == nil {
			if err := cli.prepareSudoPrivileges(
				ctx, cli.options.Stderr, cli.effectiveUID(), "update",
			); err != nil {
				cli.errorf("state migration test VM broker privileges: %v", err)
				return 1
			}
			platform = cli.initPlatform(loaded, []domain.Context{loaded.Context})
		}
		if err := reconcileMigrationTestVMs(ctx, platform); err != nil {
			cli.errorf("state migration test VM broker reconcile: %v", err)
			return 1
		}
		return 0
	}
	if arguments[0] == "reconcile-power-reconciler" {
		if migrationEnvironment["SUBYARD_INTERNAL_MIGRATION_CHILD"] != "1" {
			cli.errorf("internal: power reconciler migration child is required")
			return 1
		}
		platform := cli.options.InitPlatform
		if platform == nil {
			if err := cli.prepareSudoPrivileges(
				ctx, cli.options.Stderr, cli.effectiveUID(), "update",
			); err != nil {
				cli.errorf("state migration power reconciler privileges: %v", err)
				return 1
			}
			// Rollback is dispatched by the active runtime, but the root-owned
			// helper must come from the selected retained release.
			platform = func() ports.InitPlatform {
				activeRepositoryRoot := cli.options.RepositoryRoot
				activeDispatcherPath := cli.options.DispatcherPath
				activeCompatibility := cli.retainedAdapterCompatibility
				defer func() {
					cli.options.RepositoryRoot = activeRepositoryRoot
					cli.options.DispatcherPath = activeDispatcherPath
					cli.retainedAdapterCompatibility = activeCompatibility
				}()
				cli.options.RepositoryRoot = repositoryRoot
				cli.options.DispatcherPath = filepath.Join(repositoryRoot, "bin", "yard-engine")
				cli.retainedAdapterCompatibility = repositoryRoot != activeRepositoryRoot
				return cli.initPlatform(loaded, []domain.Context{loaded.Context})
			}()
		}
		if err := platform.ApplyStage(ctx, ports.ReconcileStagePower); err != nil {
			cli.errorf("state migration power reconciler: %v", err)
			return 1
		}
		return 0
	}
	keysRoot := environment["SUBYARD_KEYS_ROOT"]
	if keysRoot == "" {
		keysRoot = filepath.Join(configHome, "keys")
	}
	runtimeRoot := environment["YARD_RUNTIME_ROOT"]
	if runtimeRoot == "" {
		runtimeRoot = filepath.Join(environment["SUBYARD_HOME"], "runtime")
	}
	runtimeRoot = runtimeRootForRepository(cli.options.RepositoryRoot, runtimeRoot)
	releaseOptions := migration.ReleaseOptions{
		RegistryPath:       filepath.Join(cli.options.RepositoryRoot, "config", "migrations.json"),
		RepositoryRoot:     cli.options.RepositoryRoot,
		RuntimeRoot:        runtimeRoot,
		ConfigHome:         configHome,
		DataHome:           environment["SUBYARD_HOME"],
		Version:            Version,
		ProjectDirectories: projectDirectories,
		Credentials:        credentialmeta.Reader{Root: keysRoot},
		Executable:         cli.options.DispatcherPath,
		Incus:              "incus",
		Environment:        environmentList(migrationEnvironment, nil),
		Diagnostics:        cli.options.Stderr,
		Stderr:             cli.options.Stderr,
	}
	var report migration.Report
	switch arguments[0] {
	case "apply":
		report, err = migration.ApplyRelease(ctx, releaseOptions)
	case "finalize":
		var changed bool
		changed, err = migration.FinalizeActive(ctx, releaseOptions)
		if err == nil {
			report, err = migration.CheckRelease(ctx, releaseOptions)
			report.Changed = changed
		}
	case "rollback":
		report, err = migration.RollbackRelease(ctx, releaseOptions)
	case "cleanup":
		var removed int
		removed, err = migration.CleanupRelease(releaseOptions)
		if err == nil {
			report, err = migration.CheckRelease(ctx, releaseOptions)
			report.Changed = removed > 0
		}
	default:
		report, err = migration.CheckRelease(ctx, releaseOptions)
	}
	if err != nil {
		cli.errorf("state migration %s: %v", arguments[0], err)
		return 1
	}
	if err := json.NewEncoder(cli.options.Stdout).Encode(report); err != nil {
		cli.errorf("state migration report: %v", err)
		return 1
	}
	return 0
}

// A release migration is evaluated by the target runtime. The updater that
// launches it may have exported a fully resolved context from the previous
// runtime, including the previous immutable config directory and settings that
// no longer exist. Keep process/bootstrap inputs, but make every migration
// child load the target runtime's shipped config and the operator's persisted
// configuration from scratch.
func freshMigrationEnvironment(
	inherited map[string]string,
	repositoryRoot string,
) map[string]string {
	environment := make(map[string]string, len(inherited)+1)
	for name, value := range inherited {
		if _, setting := config.LookupSetting(name); setting {
			continue
		}
		switch name {
		case "E2E_VM_TTL_MINUTES",
			"SUBYARD_CONFIG_DIR",
			"SUBYARD_CONFIG_LOADED",
			"SUBYARD_DISPATCH_ARG0",
			"SUBYARD_DISPATCH_COMMAND",
			"SUBYARD_DISPATCH_PATH",
			"SUBYARD_ENGINE_CONTEXT",
			"SUBYARD_ENGINE_CONTEXT_SCHEMA",
			"SUBYARD_ENGINE_CONTEXT_SOURCED",
			"SUBYARD_SUDO_PREAUTHORIZED",
			"SUBYARD_YARD":
			continue
		}
		environment[name] = value
	}
	environment["SUBYARD_REPOSITORY_ROOT"] = repositoryRoot
	return environment
}

func powerMigrationRepositoryRoot(
	activeRepositoryRoot string,
	environment map[string]string,
) (string, error) {
	payloadRoot := environment["SUBYARD_POWER_MIGRATION_PAYLOAD_ROOT"]
	if payloadRoot == "" {
		return activeRepositoryRoot, nil
	}
	return retainedMigrationPayloadRoot(payloadRoot, environment)
}

func retainedMigrationPayloadRoot(
	payloadRoot string,
	environment map[string]string,
) (string, error) {
	runtimeRoot := environment["YARD_RUNTIME_ROOT"]
	if runtimeRoot == "" || !filepath.IsAbs(runtimeRoot) {
		return "", errors.New("absolute runtime root is required")
	}
	resolvedRuntimeRoot, err := filepath.EvalSymlinks(runtimeRoot)
	if err != nil {
		return "", fmt.Errorf("resolve runtime root: %w", err)
	}
	resolvedPrevious, err := filepath.EvalSymlinks(filepath.Join(resolvedRuntimeRoot, "previous"))
	if err != nil {
		return "", fmt.Errorf("resolve retained previous release: %w", err)
	}
	resolvedPayload, err := filepath.EvalSymlinks(payloadRoot)
	if err != nil {
		return "", fmt.Errorf("resolve migration payload: %w", err)
	}
	if resolvedPayload != resolvedPrevious {
		return "", errors.New("migration payload is not the retained previous release")
	}
	relative, err := filepath.Rel(filepath.Join(resolvedRuntimeRoot, "releases"), resolvedPrevious)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("retained previous release escapes the runtime release store")
	}
	return resolvedPrevious, nil
}

func (cli *CLI) finalizeActiveMigration(ctx context.Context) error {
	operatorHome := cli.env["SUBYARD_OPERATOR_HOME"]
	if operatorHome == "" {
		operatorHome = cli.env["HOME"]
	}
	if operatorHome == "" {
		return nil
	}
	configHome := cli.env["SUBYARD_CONFIG_HOME"]
	if configHome == "" {
		configHome = filepath.Join(operatorHome, ".config", "subyard")
	}
	dataHome := cli.env["SUBYARD_HOME"]
	if dataHome == "" {
		dataHome = filepath.Join(operatorHome, ".subyard")
	}
	runtimeRoot := cli.env["YARD_RUNTIME_ROOT"]
	if runtimeRoot == "" {
		runtimeRoot = filepath.Join(dataHome, "runtime")
	}
	runtimeRoot = runtimeRootForRepository(cli.options.RepositoryRoot, runtimeRoot)
	_, err := migration.FinalizeActive(ctx, migration.ReleaseOptions{
		RegistryPath:   filepath.Join(cli.options.RepositoryRoot, "config", "migrations.json"),
		RepositoryRoot: cli.options.RepositoryRoot,
		RuntimeRoot:    runtimeRoot,
		ConfigHome:     filepath.Clean(configHome),
		DataHome:       filepath.Clean(dataHome),
		Version:        Version,
		Executable:     cli.options.DispatcherPath,
		Incus:          "incus",
		Environment:    environmentList(cli.env, nil),
		Diagnostics:    cli.options.Stderr,
		Stderr:         cli.options.Stderr,
	})
	return err
}

func runtimeRootForRepository(repositoryRoot, fallback string) string {
	releases := filepath.Dir(filepath.Clean(repositoryRoot))
	if filepath.Base(releases) == "releases" {
		return filepath.Dir(releases)
	}
	return filepath.Clean(fallback)
}

func validStateValue(kind, value string) bool {
	switch kind {
	case "id":
		return domain.SafeID(value)
	case "mode":
		return value == string(domain.ProjectSync) || value == string(domain.ProjectGit) ||
			value == string(domain.ProjectBind)
	case "target":
		return value == "" || value == "yard" || domain.SafeName(value)
	case "name":
		return domain.SafeProjectName(value)
	default:
		return false
	}
}

func (cli *CLI) resolveLocalProject(
	ctx context.Context,
	yard domain.Context,
	store ports.ProjectStore,
	selector string,
) (state.Match, error) {
	if !isExplicitProjectPath(selector) {
		return (state.Resolver{
			Stores: map[string]ports.ProjectStore{yard.YardName: store},
		}).Resolve(ctx, selector)
	}
	path := cli.projectSelectorPath(selector)
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return state.Match{}, err
	}
	realPath, err = filepath.Abs(realPath)
	if err != nil {
		return state.Match{}, err
	}
	records, err := store.List(ctx)
	if err != nil {
		return state.Match{}, err
	}
	matches := recordsBySource(records, realPath)
	if len(matches) == 0 {
		return state.Match{}, fmt.Errorf("project path %q is not in yard %s", selector, yard.YardName)
	}
	if len(matches) > 1 {
		return state.Match{}, fmt.Errorf("%w: project path %q has %d records in yard %s",
			state.ErrAmbiguous, selector, len(matches), yard.YardName)
	}
	return state.Match{Yard: yard.YardName, Record: matches[0]}, nil
}

func (cli *CLI) projectStores(ctx context.Context, yard domain.Context) (map[string]ports.ProjectStore, error) {
	names, err := config.YardNames(config.RegistryDirectories(yard.Paths.ConfigDir, yard.Paths.ConfigHome)...)
	if err != nil {
		return nil, err
	}
	stores := make(map[string]ports.ProjectStore, len(names))
	loaded := config.Loaded{Context: yard}
	for _, name := range names {
		contextForYard, err := cli.loadInventoryContext(name, loaded)
		if err != nil {
			return nil, err
		}
		store, err := openProjectStore(ctx, contextForYard.Paths.StateDir)
		if err != nil {
			return nil, err
		}
		if _, err := store.List(ctx); err != nil {
			return nil, err
		}
		stores[name] = store
	}
	return stores, nil
}

func (cli *CLI) projectStoresReadOnly(
	ctx context.Context, yard domain.Context,
) (map[string]ports.ProjectStore, error) {
	names, err := config.YardNames(config.RegistryDirectories(yard.Paths.ConfigDir, yard.Paths.ConfigHome)...)
	if err != nil {
		return nil, err
	}
	stores := make(map[string]ports.ProjectStore, len(names))
	loaded := config.Loaded{Context: yard}
	for _, name := range names {
		contextForYard, loadErr := cli.loadInventoryContext(name, loaded)
		if loadErr != nil {
			return nil, loadErr
		}
		store, storeErr := openProjectStoreReadOnly(contextForYard.Paths.StateDir)
		if storeErr != nil {
			return nil, storeErr
		}
		if _, listErr := store.List(ctx); listErr != nil {
			return nil, listErr
		}
		stores[name] = store
	}
	return stores, nil
}

func (cli *CLI) resolveGlobalProject(
	ctx context.Context,
	yard domain.Context,
	selector string,
) (state.Match, error) {
	stores, err := cli.projectStores(ctx, yard)
	if err != nil {
		return state.Match{}, err
	}
	resolver := state.Resolver{Stores: stores}
	if !isExplicitProjectPath(selector) {
		return resolver.Resolve(ctx, selector)
	}
	path, err := filepath.EvalSymlinks(cli.projectSelectorPath(selector))
	if err != nil {
		return state.Match{}, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return state.Match{}, err
	}
	matches := make([]state.Match, 0)
	for yardName, store := range stores {
		records, listErr := store.List(ctx)
		if listErr != nil {
			return state.Match{}, listErr
		}
		for _, record := range recordsBySource(records, path) {
			matches = append(matches, state.Match{Yard: yardName, Record: record})
		}
	}
	if len(matches) > 1 {
		return state.Match{}, fmt.Errorf("%w: project path %q is registered in multiple yards",
			state.ErrAmbiguous, selector)
	}
	if len(matches) == 0 {
		return state.Match{}, fmt.Errorf("project path %q is not in any yard", selector)
	}
	return matches[0], nil
}

func (cli *CLI) resolveGlobalProjectReadOnly(
	ctx context.Context,
	yard domain.Context,
	selector string,
) (state.Match, error) {
	stores, err := cli.projectStoresReadOnly(ctx, yard)
	if err != nil {
		return state.Match{}, err
	}
	resolver := state.Resolver{Stores: stores}
	if !isExplicitProjectPath(selector) {
		return resolver.Resolve(ctx, selector)
	}
	path, err := filepath.EvalSymlinks(cli.projectSelectorPath(selector))
	if err != nil {
		return state.Match{}, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return state.Match{}, err
	}
	matches := make([]state.Match, 0)
	for yardName, store := range stores {
		records, listErr := store.List(ctx)
		if listErr != nil {
			return state.Match{}, listErr
		}
		for _, record := range recordsBySource(records, path) {
			matches = append(matches, state.Match{Yard: yardName, Record: record})
		}
	}
	if len(matches) > 1 {
		return state.Match{}, fmt.Errorf("%w: project path %q is registered in multiple yards",
			state.ErrAmbiguous, selector)
	}
	if len(matches) == 0 {
		return state.Match{}, fmt.Errorf("project path %q is not in any yard", selector)
	}
	return matches[0], nil
}

func isExplicitProjectPath(selector string) bool {
	separator := string(filepath.Separator)
	return filepath.IsAbs(selector) || selector == "." || selector == ".." ||
		strings.HasPrefix(selector, "."+separator) ||
		strings.HasPrefix(selector, ".."+separator)
}

func (cli *CLI) projectSelectorPath(selector string) string {
	if filepath.IsAbs(selector) || cli.options.WorkingDir == "" {
		return selector
	}
	return filepath.Join(cli.options.WorkingDir, selector)
}

func (cli *CLI) routeProjectSource(
	ctx context.Context,
	yard domain.Context,
	source string,
	explicitTarget string,
) (string, error) {
	stores, err := cli.projectStores(ctx, yard)
	if err != nil {
		return "", err
	}
	if cli.env["SUBYARD_YARD_EXPLICIT"] != "" {
		if explicitTarget != "" && explicitTarget != yard.YardName {
			return "", fmt.Errorf("conflicting yard: context is -Y %s but @%s was given", yard.YardName, explicitTarget)
		}
		return yard.YardName, nil
	}
	if explicitTarget != "" {
		if _, ok := stores[explicitTarget]; !ok {
			return "", fmt.Errorf("unknown yard %q", explicitTarget)
		}
		return explicitTarget, nil
	}
	matches := make([]string, 0)
	for name, store := range stores {
		records, err := store.List(ctx)
		if err != nil {
			return "", err
		}
		if len(recordsBySource(records, source)) != 0 {
			matches = append(matches, name)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return yard.YardName, nil
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("this path is already in multiple yards (%s) — pick one with @<yard> or -Y <yard>",
			strings.Join(matches, " "))
	}
}

func recordsBySource(
	records []domain.ProjectRecord,
	source string,
) []domain.ProjectRecord {
	result := make([]domain.ProjectRecord, 0)
	for _, record := range records {
		if record.SourceKey == state.SourceKey(source) ||
			record.SourceKey == "" && record.HostPath == source {
			result = append(result, record)
		}
	}
	return result
}

func (cli *CLI) runOwnerProjectState(
	ctx context.Context,
	service state.Service,
	yard domain.Context,
	arguments []string,
) int {
	if len(arguments) == 0 {
		cli.errorf("internal: _project-state expects preview, reserve, finalize, abort, upsert, remove or unregister")
		return 2
	}
	switch arguments[0] {
	case "preview":
		if len(arguments) != 5 {
			cli.errorf("internal: _project-state preview needs <source> <mode> <name> <explicit>")
			return 2
		}
		store, ok := service.Store.(*state.FileStore)
		if !ok {
			cli.errorf("internal: owner project preview requires the file store")
			return 1
		}
		admission, err := store.PreviewAdmission(
			ctx, arguments[1], domain.ProjectMode(arguments[2]),
			arguments[3], arguments[4] == "1",
		)
		if err != nil {
			cli.errorf("preview owner project identity: %v", err)
			return 1
		}
		response := map[string]any{
			"projectId": admission.ProjectID,
			"name":      admission.Name,
			"existing":  admission.Existing,
		}
		if err := json.NewEncoder(cli.options.Stdout).Encode(response); err != nil {
			cli.errorf("encode owner project preview: %v", err)
			return 1
		}
	case "reserve":
		if len(arguments) != 6 {
			cli.errorf("internal: _project-state reserve needs <operation> <source> <mode> <name> <explicit>")
			return 2
		}
		store, ok := service.Store.(*state.FileStore)
		if !ok {
			cli.errorf("internal: owner project reservation requires the file store")
			return 1
		}
		admission, err := store.Admit(
			ctx, arguments[1], arguments[2], domain.ProjectMode(arguments[3]),
			arguments[4], arguments[5] == "1",
		)
		if err != nil {
			cli.errorf("reserve owner project identity: %v", err)
			return 1
		}
		response := map[string]any{
			"projectId": admission.ProjectID,
			"name":      admission.Name,
			"reserved":  admission.Reservation != nil,
			"existing":  admission.Existing,
		}
		if err := json.NewEncoder(cli.options.Stdout).Encode(response); err != nil {
			cli.errorf("encode owner project reservation: %v", err)
			return 1
		}
	case "finalize":
		if len(arguments) != 9 {
			cli.errorf("internal: _project-state finalize needs <operation> <id> <name> <mode> <target> <source> <imported-at> <identity-version>")
			return 2
		}
		store, ok := service.Store.(*state.FileStore)
		if !ok {
			cli.errorf("internal: owner project finalize requires the file store")
			return 1
		}
		identityVersion, err := strconv.Atoi(arguments[8])
		if err != nil {
			cli.errorf("internal: invalid project identity version")
			return 2
		}
		record := domain.ProjectRecord{
			Schema: 1, IdentityVersion: identityVersion,
			ProjectID: arguments[2], Name: arguments[3],
			Mode: domain.ProjectMode(arguments[4]), Target: arguments[5],
			HostPath: arguments[6], SourceKey: state.SourceKey(arguments[6]),
			ImportedAt: arguments[7], YardPath: state.YardPath(arguments[2]),
			SSHHost: yard.SSHHost,
		}
		if record.Target != "" && record.Target != "yard" {
			record.Profile = record.Target
		}
		if err := store.FinalizeOperation(ctx, arguments[1], record); err != nil {
			cli.errorf("finalize owner project identity: %v", err)
			return 1
		}
	case "abort":
		if len(arguments) != 2 {
			cli.errorf("internal: _project-state abort needs <operation>")
			return 2
		}
		store, ok := service.Store.(*state.FileStore)
		if !ok {
			cli.errorf("internal: owner project abort requires the file store")
			return 1
		}
		if err := store.AbortAdmission(ctx, arguments[1]); err != nil {
			cli.errorf("abort owner project identity: %v", err)
			return 1
		}
	case "upsert":
		if len(arguments) != 5 && len(arguments) != 8 {
			cli.errorf("internal: _project-state upsert needs <id> <name> <mode> <target> [<source> <imported-at> <identity-version>]")
			return 2
		}
		hostPath, importedAt, identityVersion := "", "", 0
		if len(arguments) == 8 {
			hostPath, importedAt = arguments[5], arguments[6]
			parsedVersion, parseErr := strconv.Atoi(arguments[7])
			if parseErr != nil {
				cli.errorf("internal: invalid project identity version")
				return 2
			}
			identityVersion = parsedVersion
		}
		if err := service.UpsertProject(
			ctx, arguments[1], arguments[2], domain.ProjectMode(arguments[3]),
			arguments[4], yard.SSHHost, hostPath, importedAt, identityVersion,
		); err != nil {
			cli.errorf("converge owner project state: %v", err)
			return 1
		}
	case "remove":
		if len(arguments) != 3 {
			cli.errorf("internal: _project-state remove needs <id> <source-key>")
			return 2
		}
		if err := service.RemoveProject(ctx, arguments[1], arguments[2]); err != nil {
			cli.errorf("remove owner project state: %v", err)
			return 1
		}
	case "unregister":
		if len(arguments) != 2 {
			cli.errorf("internal: _project-state unregister needs <id>")
			return 2
		}
		if err := service.UnregisterYard(ctx, arguments[1]); err != nil {
			cli.errorf("converge owner project state: %v", err)
			return 1
		}
	default:
		cli.errorf("internal: _project-state expects preview, reserve, finalize, abort, upsert, remove or unregister")
		return 2
	}
	return 0
}

func (cli *CLI) handlerEnvironment(name, arg0 string) map[string]string {
	return map[string]string{
		"YARD_ENGINE":              Version,
		"YARD_VERSION":             Version,
		"SUBYARD_DISPATCH_PATH":    cli.options.DispatcherPath,
		"SUBYARD_DISPATCH_COMMAND": name,
		"SUBYARD_DISPATCH_ARG0":    arg0,
		"SUBYARD_REPOSITORY_ROOT":  cli.options.RepositoryRoot,
	}
}

type wallClock struct{}

func (wallClock) Now() time.Time                             { return time.Now() }
func (wallClock) After(delay time.Duration) <-chan time.Time { return time.After(delay) }

type fixedIDSource struct{ value string }

func (source fixedIDSource) NewID() string { return source.value }

type streamPrompt struct {
	input       io.Reader
	output      io.Writer
	interactive func() bool
}

func (prompt streamPrompt) Confirm(_ context.Context, request domain.ConfirmationRequest) (bool, error) {
	label, ok := confirmationPromptLabel(request.Default)
	if !ok {
		return false, fmt.Errorf("%w: invalid confirmation default %q", domain.ErrConfirmationRequired, request.Default)
	}
	if prompt.interactive == nil || !prompt.interactive() {
		return false, fmt.Errorf("%w: interactive terminal required", domain.ErrConfirmationRequired)
	}
	reader := bufio.NewReader(prompt.input)
	for {
		fmt.Fprintf(prompt.output, "\n%s\nThis will:\n", request.Summary)
		for _, consequence := range request.Consequences {
			fmt.Fprintf(prompt.output, "  - %s\n", consequence)
		}
		fmt.Fprintf(prompt.output, "\nProceed? %s ", label)
		answer, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, fmt.Errorf("%w: interactive input ended", domain.ErrConfirmationRequired)
			}
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "":
			if request.Default == domain.ConfirmationDefaultYes {
				return true, nil
			}
			return false, domain.ErrOperationDeclined
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, domain.ErrOperationDeclined
		}
	}
}

func confirmationPromptLabel(defaultValue domain.ConfirmationDefault) (string, bool) {
	switch defaultValue {
	case domain.ConfirmationDefaultYes:
		return "[Y/n]", true
	case domain.ConfirmationDefaultNo:
		return "[y/N]", true
	default:
		return "", false
	}
}

type rpcOperationEvents struct{ emit rpc.Emit }

func (events rpcOperationEvents) Publish(_ context.Context, event domain.OperationEvent) error {
	_, err := events.emit(event.Kind, event)
	return err
}

func (cli *CLI) operationOrchestrator(
	operationID string,
	loaded config.Loaded,
	events ports.EventSink,
	definition *command.Definition,
) *application.Orchestrator {
	clock := cli.options.Clock
	if clock == nil {
		clock = wallClock{}
	}
	prompt := cli.options.Prompt
	if prompt == nil {
		prompt = streamPrompt{input: cli.options.Stdin, output: cli.options.Stdout, interactive: cli.promptInputTerminal}
	}
	runner := cli.options.AdapterRunner
	if runner == nil {
		contextValues := structuredCommandContext(loaded)
		contextKeys := make(map[string]struct{}, len(contextValues))
		for key := range contextValues {
			contextKeys[key] = struct{}{}
		}
		for _, key := range []string{
			"SUBYARD_PROJECT_SNAPSHOT", "SUBYARD_PROJECT_ID", "SUBYARD_PROJECT_NAME",
			"SUBYARD_PROJECT_HOST_PATH", "SUBYARD_PROJECT_YARD_PATH", "SUBYARD_PROJECT_MODE",
			"SUBYARD_PROJECT_SSH_HOST", "SUBYARD_PROJECT_TARGET", "SUBYARD_PROJECT_PROFILE",
			"SUBYARD_PROJECT_DEVICE", "SUBYARD_PROJECT_EXISTS", "SUBYARD_PROJECT_PROFILES",
			"SUBYARD_PROJECT_REMOVE_SOFT", "SUBYARD_PROJECT_REBUILD",
			"SUBYARD_POWER_DESIRED", "SUBYARD_SUDO_PREAUTHORIZED", "SUBYARD_TEARDOWN_KEEP_DATA",
			"SUBYARD_TEARDOWN_KEEP_SHARED",
		} {
			contextKeys[key] = struct{}{}
		}
		actions := map[string]map[string]shelladapter.Action{}
		if definition != nil {
			adapter, handler := "command", definition.Handler
			if definition.Handler == "@lifecycle" {
				adapter, handler = "lifecycle", "lifecycle-guard.sh"
			}
			if definition.Handler == "@provision" {
				path := filepath.Join(cli.options.RepositoryRoot, "scripts", "lifecycle-guard.sh")
				actions["lifecycle"] = map[string]shelladapter.Action{
					"start": {Path: path, Direct: true}, "stop": {Path: path, Direct: true},
				}
				actions["provision"] = map[string]shelladapter.Action{
					"profile":       {Path: filepath.Join(cli.options.RepositoryRoot, "scripts/provision-profile.sh"), Direct: true, Timeout: 45 * time.Minute},
					"profile-check": {Path: filepath.Join(cli.options.RepositoryRoot, "scripts/provision-profile.sh"), Direct: true, Capture: true},
				}
			} else if definition.Handler == "@test-vms" {
				path := filepath.Join(cli.options.RepositoryRoot, "scripts/e2e-lab/invoke.sh")
				actions["test-vms"] = map[string]shelladapter.Action{
					"up": {Path: path, Direct: true}, "status": {Path: path, Direct: true},
					"down": {Path: path, Direct: true}, "revoke": {Path: path, Direct: true},
					"recover": {Path: path, Direct: true},
				}
			} else if definition.Handler == "@teardown" {
				path := filepath.Join(cli.options.RepositoryRoot, "scripts/teardown-physical.sh")
				actions["teardown"] = map[string]shelladapter.Action{
					"apply": {Path: path, Direct: true},
				}
			} else {
				actions[adapter] = map[string]shelladapter.Action{definition.Name: {
					Path: filepath.Join(cli.options.RepositoryRoot, "scripts", handler), Direct: true,
				}}
			}
		}
		runner = shelladapter.Runner{
			RepositoryRoot: cli.options.RepositoryRoot,
			Actions:        actions,
			ContextKeys:    contextKeys,
			Diagnostics:    cli.options.Stderr,
			Timeout:        10 * time.Minute,
		}
	}
	auditSink := cli.options.Audit
	if auditSink == nil && cli.env["SUBYARD_NO_AUDIT"] == "" {
		home := cli.env["SUBYARD_HOME"]
		if home == "" {
			home = loaded.Context.Paths.DataHome
		}
		auditSink = audit.OperationLog{
			Home: home, WorkingDir: cli.options.WorkingDir, Yard: loaded.Context.YardName,
			Remote: loaded.Context.OwnerEndpoint, Maximum: audit.MaximumFrom(cli.env["SUBYARD_AUDIT_MAX_BYTES"]),
		}
	}
	return &application.Orchestrator{
		Clock: clock, IDs: fixedIDSource{value: operationID}, Prompt: prompt,
		Runner: runner, Audit: auditSink, Events: events, Actions: cli.coreActions,
	}
}

func structuredAdapterContext(yard domain.Context) map[string]string {
	boolValue := func(value bool) string {
		if value {
			return "1"
		}
		return "0"
	}
	yardName := yard.YardName
	selectedYard := yardName
	if yardName == "default" {
		yardName = ""
		selectedYard = ""
	}
	return map[string]string{
		"YARD_VERSION":                  Version,
		"HOME":                          yard.Paths.OperatorHome,
		"SUBYARD_OPERATOR_HOME":         yard.Paths.OperatorHome,
		"SUBYARD_CONFIG_DIR":            yard.Paths.ConfigDir,
		"SUBYARD_CONFIG_HOME":           yard.Paths.ConfigHome,
		"SUBYARD_HOME":                  yard.Paths.DataHome,
		"SUBYARD_STATE_DIR":             yard.Paths.StateDir,
		"SUBYARD_CONFIG_LOADED":         "1",
		"SUBYARD_ENGINE_CONTEXT":        "1",
		"SUBYARD_ENGINE_CONTEXT_SCHEMA": "1",
		"SUBYARD_YARD":                  selectedYard,
		"YARD_NAME":                     yardName,
		"ACCESS_KIND":                   string(yard.AccessKind),
		"OWNER_ENDPOINT":                yard.OwnerEndpoint,
		"OWNER_YARD_NAME":               yard.OwnerYardName,
		"YARD_KIND":                     string(yard.YardKind),
		"YARD_INSTANCE_NAME":            yard.YardInstanceName,
		"INCUS_PROJECT":                 yard.IncusProject,
		"INCUS_BRIDGE":                  yard.IncusBridge,
		"SSH_HOST":                      yard.SSHHost,
		"SSH_PORT":                      strconv.Itoa(yard.SSHPort),
		"DEV_USER":                      yard.DevUser,
		"DEV_UID":                       strconv.Itoa(yard.DevUID),
		"DEV_SUDO":                      boolValue(yard.DevSudo),
		"FORWARD_SSH_AGENT":             boolValue(yard.ForwardSSHAgent),
		"NESTED_E2E_VMS":                boolValue(yard.NestedE2EVMs),
		"SHIFT_MODE":                    yard.ShiftMode,
		"STORAGE_PATH":                  yard.Paths.StoragePath,
		"HOST_BASE":                     yard.Paths.HostBase,
		"RESTRICTED_DISK_PATHS":         yard.Paths.HostBase,
		"ASSUME_YES":                    "1",
		"PROG":                          cliProgramName,
	}
}

var legacyAdapterAliases = map[string]string{
	"YARD_TYPE":           "ACCESS_KIND",
	"INSTANCE_TYPE":       "YARD_KIND",
	"INSTANCE_NAME":       "YARD_INSTANCE_NAME",
	"REMOTE_DEST":         "OWNER_ENDPOINT",
	"REMOTE_YARD":         "OWNER_YARD_NAME",
	"BASE_IMAGE":          "YARD_IMAGE",
	"BASE_IMAGE_FALLBACK": "YARD_IMAGE_FALLBACK",
	"YARD_PROFILES":       "ENVIRONMENT_PROFILES",
	"AGENTS":              "CODING_TOOL_INTEGRATIONS",
}

func addLegacyAdapterAliases(environment map[string]string) {
	for legacy, canonical := range legacyAdapterAliases {
		environment[legacy] = environment[canonical]
	}
}

var structuredRuntimeRoleKeys = map[string]struct{}{
	"SUBYARD_KEYS_CONSUMER_ROOT":       {},
	"SUBYARD_KEYS_PROD_FINGERPRINTS":   {},
	"SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE": {},
	"SUBYARD_CONFIG_GENERATED_DIR":     {}, "SUBYARD_CONFIG_HOST_DIR": {},
	"SUBYARD_CONFIG_SHARED_DIR": {}, "SUBYARD_CONFIG_YARD_DIR": {},
	"YARD_RUNTIME_ROOT": {},
}

func structuredCommandContext(loaded config.Loaded) map[string]string {
	values := structuredAdapterContext(loaded.Context)
	for name, value := range loaded.Environment {
		_, setting := config.LookupSetting(name)
		_, runtimeRole := structuredRuntimeRoleKeys[name]
		if setting || runtimeRole {
			values[name] = value
		}
	}
	return values
}

const cliProgramName = "yard"

func resolveConfirmationPolicy(
	definition command.Definition,
	_ domain.CommandEffect,
) domain.ConfirmationPolicy {
	if definition.Confirmation == command.ConfirmationDynamic {
		return ""
	}
	return domain.ConfirmationPolicy(definition.Confirmation)
}

func resolveCommandConfirmation(
	definition command.Definition,
	policy domain.CommandPolicy,
) domain.CommandPolicy {
	policy.Confirmation = resolveConfirmationPolicy(definition, policy.Effect)
	return policy
}

func commandPolicy(
	definition command.Definition,
	yard domain.Context,
	arguments []string,
	project *projectExecution,
	remote *domain.RemotePrepared,
) domain.CommandPolicy {
	if remote != nil {
		return application.RemotePolicy(*remote)
	}
	consequences := []string{
		fmt.Sprintf("%s in yard %s", definition.Summary, yard.YardName),
		"publish typed operation events and an audit result",
	}
	if !strings.HasPrefix(definition.Handler, "@") {
		consequences = append(consequences, fmt.Sprintf("execute the allowlisted physical adapter for %s", definition.Name))
	}
	if len(arguments) != 0 {
		consequences = append(consequences, "use validated command arguments")
	}
	if project != nil && project.Record.ProjectID != "" {
		consequences = append(consequences, fmt.Sprintf("operate on project %s (%s)",
			project.Record.Name, project.Record.ProjectID))
		consequences = append(consequences, application.ProjectConsequences(definition.Name,
			project.Record, project.Environment["SUBYARD_PROJECT_REMOVE_SOFT"] == "1")...)
		switch project.Commit {
		case projectCommitPut:
			consequences = append(consequences, "publish project state only after the physical operation succeeds")
		case projectCommitDelete:
			consequences = append(consequences, "delete project state only after physical cleanup succeeds")
		}
	}
	return domain.CommandPolicy{
		Name: definition.Name, Effect: domain.CommandEffect(definition.Effect),
		RemotePolicy: domain.RemotePolicy(definition.Remote), Consequences: consequences,
	}
}

func structuredCommandSupported(name string) bool {
	switch name {
	case "init", "start", "provision", "test-vms", "stop", "teardown", "sync", "bind", "clone", "code",
		"export", "remove", "up", "down", "info", "remote":
		return true
	default:
		return false
	}
}

func commandHelpRequested(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "-h" || argument == "--help" {
			return true
		}
	}
	return false
}

func writeAdapterDiagnostics(output io.Writer, value string) {
	if value == "" {
		return
	}
	_, _ = io.WriteString(output, value)
	if !strings.HasSuffix(value, "\n") {
		_, _ = io.WriteString(output, "\n")
	}
}

func (cli *CLI) runStructuredCommand(
	ctx context.Context,
	loaded config.Loaded,
	definition command.Definition,
	arguments []string,
	assumeYes bool,
	project *projectExecution,
	remote *domain.RemotePrepared,
	bootstrap *initBootstrap,
) int {
	for _, argument := range arguments {
		if argument == "-y" || argument == "--yes" {
			assumeYes = true
		}
	}
	var initRun *initExecution
	if definition.Handler == "@init" && loaded.Context.AccessKind != domain.AccessRemote {
		var err error
		initRun, err = cli.prepareInitExecution(ctx, loaded, arguments, bootstrap)
		if err != nil {
			cli.errorf("prepare init: %v", err)
			return 1
		}
		cli.printInitPlan(initRun)
	}
	var lifecycleRun *lifecycleExecution
	if definition.Handler == "@lifecycle" {
		var err error
		lifecycleRun, err = prepareLifecycleExecution(definition, arguments)
		if err != nil {
			cli.errorf("prepare %s: %v", definition.Name, err)
			return 2
		}
	}
	var provisionRun *provisionExecution
	if definition.Handler == "@provision" {
		var err error
		provisionRun, err = cli.prepareProvisionExecution(loaded, arguments, project)
		if err != nil {
			cli.errorf("prepare provision: %v", err)
			return 2
		}
		if provisionRun.list {
			provisionRun.printList(cli.options.Stdout)
			return 0
		}
	}
	var testVMRun *testVMExecution
	if definition.Handler == "@test-vms" {
		var err error
		testVMRun, err = cli.prepareTestVMExecution(ctx, loaded, arguments)
		if err != nil {
			cli.errorf("prepare test-vms: %v", err)
			if errors.Is(err, domain.ErrPlanStale) {
				return 1
			}
			return 2
		}
	}
	var teardownRun *teardownExecution
	if definition.Handler == "@teardown" {
		var err error
		teardownRun, err = prepareTeardownExecution(arguments)
		if err != nil {
			cli.errorf("prepare teardown: %v", err)
			return 2
		}
	}
	orchestrator := cli.operationOrchestrator(cli.env["SUBYARD_OPERATION_ID"], loaded, nil, &definition)
	policy := commandPolicy(definition, loaded.Context, arguments, project, remote)
	if initRun != nil {
		policy.Consequences = initRun.consequences()
	}
	if lifecycleRun != nil {
		policy = lifecycleRun.policy(definition, loaded.Context)
	}
	if provisionRun != nil {
		policy = provisionRun.policy(definition, loaded.Context)
	}
	if teardownRun != nil {
		policy = teardownRun.policy(definition, loaded.Context)
	}
	action, delta, typedAction, actionErr := cli.assessStructuredAction(
		ctx, loaded, definition, project, initRun, lifecycleRun, provisionRun, teardownRun,
	)
	if actionErr != nil {
		cli.errorf("prepare %s action: %v", definition.Name, actionErr)
		return 1
	}
	var (
		plan domain.OperationPlan
		err  error
	)
	if remote != nil {
		plan, err = cli.prepareRemoteOperation(orchestrator, loaded, *remote)
		if err == nil {
			plan, err = orchestrator.Confirm(ctx, plan, assumeYes)
		}
	} else if testVMRun != nil {
		action, delta, actionErr := testVMRun.actionPlan()
		if actionErr != nil {
			err = actionErr
		} else {
			plan, err = orchestrator.PlanAction(
				ctx, loaded.Context, definition.Name, domain.RemotePolicy(definition.Remote),
				action, delta, assumeYes,
			)
		}
	} else if typedAction {
		plan, err = orchestrator.PlanAction(
			ctx, loaded.Context, definition.Name, domain.RemotePolicy(definition.Remote),
			action, delta, assumeYes,
		)
	} else {
		policy = resolveCommandConfirmation(definition, policy)
		plan, err = orchestrator.Plan(ctx, loaded.Context, policy, assumeYes)
	}
	if err != nil {
		if errors.Is(err, application.ErrDeclined) {
			cli.errorf("operation declined")
		} else {
			cli.errorf("plan %s: %v", definition.Name, err)
		}
		return 1
	}
	if operationPlanNoOp(plan) && initRun != nil && initRun.mode == initReconcile {
		fmt.Fprintln(cli.options.Stdout, "  [ ok ] Everything is already set up")
	}
	if plan.Target == domain.TargetRemoteOwner {
		remoteArguments := append([]string(nil), arguments...)
		if testVMRun != nil {
			remoteArguments, err = testVMRun.remoteArguments(arguments)
			if err != nil {
				cli.errorf("prepare remote test-vms: %v", err)
				return 1
			}
		}
		hasYes := false
		for _, argument := range remoteArguments {
			hasYes = hasYes || argument == "-y" || argument == "--yes"
		}
		if !hasYes {
			remoteArguments = append([]string{"--yes"}, remoteArguments...)
		}
		return cli.forwardRemote(ctx, loaded.Context, definition.Name, remoteArguments)
	}
	result, err := cli.executeStructuredCommand(ctx, orchestrator, loaded, definition, arguments,
		plan, project, remote, initRun, lifecycleRun, provisionRun, testVMRun, teardownRun,
		cli.options.Stdout)
	if err != nil {
		cli.errorf("%s: %v", definition.Name, err)
		return 1
	}
	if result.Status != "ok" {
		cli.errorf("%s adapter returned %s (%s)", definition.Name, result.Status, result.ErrorCode)
		return 1
	}
	if project != nil && !operationPlanNoOp(plan) {
		if err := cli.commitProjectExecution(ctx, project); err != nil {
			cli.errorf("commit %s: %v", definition.Name, err)
			return 1
		}
	}
	if remote != nil {
		cli.printRemoteResult(result)
	}
	return 0
}

func (cli *CLI) executeStructuredCommand(
	ctx context.Context,
	orchestrator *application.Orchestrator,
	loaded config.Loaded,
	definition command.Definition,
	arguments []string,
	plan domain.OperationPlan,
	project *projectExecution,
	remote *domain.RemotePrepared,
	initRun *initExecution,
	lifecycleRun *lifecycleExecution,
	provisionRun *provisionExecution,
	testVMRun *testVMExecution,
	teardownRun *teardownExecution,
	diagnostics io.Writer,
) (domain.AdapterResult, error) {
	if operationPlanNoOp(plan) {
		return domain.AdapterResult{
			Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID, Status: "ok",
		}, nil
	}
	if plan.Assessment != nil && remote == nil && testVMRun == nil &&
		(project != nil || initRun != nil || lifecycleRun != nil || provisionRun != nil || teardownRun != nil) {
		if initRun != nil {
			if err := initRun.refreshAssessment(ctx); err != nil {
				return domain.AdapterResult{}, err
			}
		}
		action, delta, typed, err := cli.assessStructuredAction(
			ctx, loaded, definition, project, initRun, lifecycleRun, provisionRun, teardownRun,
		)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if !typed || action != plan.Assessment.Action {
			return domain.AdapterResult{}, fmt.Errorf("%w: structured action changed after confirmation", domain.ErrPlanStale)
		}
		if !delta.Changed {
			return domain.AdapterResult{
				Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID, Status: "ok",
			}, nil
		}
		if !slices.Equal(delta.Consequences, plan.Assessment.Consequences) {
			return domain.AdapterResult{}, fmt.Errorf("%w: action consequences changed after confirmation", domain.ErrPlanStale)
		}
	}
	if err := cli.reserveProjectExecution(ctx, project); err != nil {
		return domain.AdapterResult{}, err
	}
	if remote != nil {
		orchestrator.Runner = application.RemoteRunner{Control: cli.remoteService(loaded).Control, Prepared: *remote}
		request := domain.AdapterRequest{
			Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
			Adapter: "remote", Action: string(remote.Action),
		}
		result, _, err := orchestrator.RunAdapter(ctx, plan, request, nil)
		return result, err
	}
	if initRun != nil && definition.Handler == "@init" {
		if cli.options.InitPlatform == nil && initRun.mode != initConfigs {
			if err := cli.prepareSudoPrivileges(
				ctx, diagnostics, cli.effectiveUID(), definition.Name,
			); err != nil {
				return domain.AdapterResult{}, err
			}
			initRun.platform = cli.initPlatform(initRun.loaded, initRun.powerYards)
		}
		orchestrator.Runner = initAdapter{
			execution: initRun, cli: cli, output: diagnostics,
		}
		request := domain.AdapterRequest{
			Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
			Adapter: "init", Action: "reconcile",
		}
		result, _, err := orchestrator.RunAdapter(ctx, plan, request, nil)
		return result, err
	}
	if lifecycleRun != nil && definition.Handler == "@lifecycle" {
		return cli.executeLifecycle(ctx, orchestrator, loaded.Context, plan, lifecycleRun, diagnostics)
	}
	if provisionRun != nil && definition.Handler == "@provision" {
		return cli.executeProvision(ctx, orchestrator, loaded, plan, provisionRun, diagnostics)
	}
	if testVMRun != nil && definition.Handler == "@test-vms" {
		return cli.executeTestVMs(ctx, orchestrator, loaded, plan, testVMRun, diagnostics)
	}
	if teardownRun != nil && definition.Handler == "@teardown" {
		return cli.executeTeardown(ctx, orchestrator, loaded, plan, teardownRun, diagnostics)
	}
	if project != nil && definition.Handler == "@project" {
		incusPort, _ := cli.statusPorts()
		orchestrator.Runner = application.ProjectActionRunner{
			Data: cli.projectDataPlane(), Devices: cli.projectDeviceManager(), Archive: cli.projectArchiver(),
			Exports: cli.projectExportStore(loaded), Instances: incusPort, VSCode: cli.projectVSCode(),
			Extensions:         strings.Fields(cli.env["CODE_RECOMMENDED_EXTENSIONS"]),
			WorkspaceDirectory: filepath.Join(loaded.Context.Paths.ConfigHome, "workspaces"),
			Yard:               loaded.Context, Project: project.Record, YardIdentity: project.YardIdentity,
			SoftRemove: project.Environment["SUBYARD_PROJECT_REMOVE_SOFT"] == "1",
		}
		request := domain.AdapterRequest{
			Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
			Adapter: "project", Action: definition.Name,
		}
		result, stderr, err := orchestrator.RunAdapter(ctx, plan, request, nil)
		writeAdapterDiagnostics(diagnostics, stderr)
		return result, err
	}
	if project != nil && definition.Handler == "@project-env" {
		var protected io.ReadCloser
		if project.SecretPath != "" {
			file, err := os.Open(project.SecretPath)
			if err != nil {
				return domain.AdapterResult{}, err
			}
			protected = file
			defer protected.Close()
		}
		orchestrator.Runner = application.ProjectEnvironmentRunner{
			Data: cli.projectDataPlane(), Yard: loaded.Context, Project: project.Record,
			Profile: project.Profile, HostLinks: project.HostLinks,
			Rebuild:   project.Environment["SUBYARD_PROJECT_REBUILD"] == "1",
			HasSecret: project.SecretPath != "",
		}
		request := domain.AdapterRequest{
			Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
			Adapter: "project-env", Action: definition.Name,
		}
		result, stderr, err := orchestrator.RunAdapter(ctx, plan, request, protected)
		writeAdapterDiagnostics(diagnostics, stderr)
		return result, err
	}
	handlerArguments := append([]string(nil), arguments...)
	if definition.Arg0 != "" {
		handlerArguments = append([]string{definition.Arg0}, handlerArguments...)
	}
	contextValues := structuredCommandContext(loaded)
	if structuredCommandNeedsSudo(definition.Name) {
		if cli.options.AdapterRunner == nil {
			if err := cli.prepareSudoPrivileges(
				ctx, diagnostics, cli.effectiveUID(), definition.Name,
			); err != nil {
				return domain.AdapterResult{}, err
			}
		}
		if cli.env["SUBYARD_SUDO_PREAUTHORIZED"] == "1" {
			contextValues["SUBYARD_SUDO_PREAUTHORIZED"] = "1"
		}
	}
	if definition.Name == "provision" {
		desired, err := cli.preparePowerIntent(ctx, loaded.Context)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		contextValues["SUBYARD_POWER_DESIRED"] = desired
	}
	if project != nil {
		for key, value := range project.Environment {
			contextValues[key] = value
		}
	}
	request := domain.AdapterRequest{
		Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
		Adapter: "command", Action: definition.Name, Arguments: handlerArguments, Context: contextValues,
	}
	result, stderr, err := orchestrator.RunAdapter(ctx, plan, request, nil)
	writeAdapterDiagnostics(diagnostics, stderr)
	return result, err
}

func structuredCommandNeedsSudo(name string) bool {
	return name == "teardown"
}

func (cli *CLI) prepareSudoPrivileges(
	ctx context.Context,
	diagnostics io.Writer,
	effectiveUID int,
	operation string,
) error {
	if effectiveUID == 0 {
		cli.env["SUBYARD_SUDO_PREAUTHORIZED"] = "1"
		return nil
	}
	if cli.sudoAvailableWithoutPrompt(ctx) {
		cli.env["SUBYARD_SUDO_PREAUTHORIZED"] = "1"
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("authorize root steps for %s: %w", operation, err)
	}
	stdin, stdout, stderr := cli.options.Stdin, cli.options.Stdout, cli.options.Stderr
	var terminal *os.File
	if cli.operatorTerminal == nil || !cli.operatorTerminal() {
		if cli.env["SUBYARD_INTERNAL_MIGRATION_CHILD"] != "1" ||
			cli.openTerminal == nil {
			return fmt.Errorf(
				"sudo authorization is required for root steps in %s; rerun 'yard %s' in an operator terminal",
				operation, operation,
			)
		}
		var err error
		terminal, err = cli.openTerminal()
		if err != nil {
			return fmt.Errorf(
				"sudo authorization is required for root steps in %s; rerun 'yard update' in an operator terminal",
				operation,
			)
		}
		defer terminal.Close()
		stdin, stdout, stderr = terminal, terminal, terminal
	}
	fmt.Fprintf(diagnostics, "  [ .. ] authorizing root steps for %s\n", operation)
	command := exec.CommandContext(ctx, "sudo", "-v")
	command.Env = environmentList(cli.env, nil)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("authorize root steps for %s: %w", operation, err)
	}
	if !cli.sudoAvailableWithoutPrompt(ctx) {
		return fmt.Errorf(
			"sudo authorization for root steps in %s did not create a non-interactive credential",
			operation,
		)
	}
	cli.env["SUBYARD_SUDO_PREAUTHORIZED"] = "1"
	return nil
}

func (cli *CLI) sudoAvailableWithoutPrompt(ctx context.Context) bool {
	command := exec.CommandContext(ctx, "sudo", "-n", "true")
	command.Env = environmentList(cli.env, nil)
	command.Stdin = nil
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run() == nil
}

func (cli *CLI) prepareNetworkManagerPrivileges(
	ctx context.Context,
	diagnostics io.Writer,
	effectiveUID int,
	operation string,
) error {
	if effectiveUID == 0 {
		return nil
	}
	check := exec.CommandContext(ctx, "systemctl", "is-active", "NetworkManager")
	check.Env = environmentList(cli.env, nil)
	output, checkErr := check.Output()
	state := strings.TrimSpace(string(output))
	switch state {
	case "inactive", "failed", "unknown":
		return nil
	case "active", "activating", "reloading", "deactivating":
	case "":
		if checkErr != nil {
			return fmt.Errorf("inspect NetworkManager before host network check: %w", checkErr)
		}
		return errors.New("inspect NetworkManager before host network check: empty service state")
	default:
		return fmt.Errorf("inspect NetworkManager before host network check: unexpected state %q", state)
	}
	return cli.prepareSudoPrivileges(ctx, diagnostics, effectiveUID, operation)
}

func terminalStream(stream any) bool {
	file, ok := stream.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func (cli *CLI) globalQuery(arguments []string) (int, bool) {
	query := arguments[0]
	switch query {
	case "-h", "--help":
		cli.usage()
		return 0, true
	case "-l", "--list":
		for _, name := range cli.manifest.PublicNames() {
			fmt.Fprintln(cli.options.Stdout, name)
		}
		for _, definition := range cli.resources.Definitions() {
			fmt.Fprintln(cli.options.Stdout, definition.Command)
		}
		return 0, true
	case "--resources":
		for _, definition := range cli.resources.Definitions() {
			fmt.Fprintf(cli.options.Stdout, "%s\t%s\n", definition.Command, strings.Join(definition.Verbs, " "))
		}
		return 0, true
	case "-V", "--version":
		fmt.Fprintf(cli.options.Stdout, "%s %s\n", cli.options.Program, Version)
		return 0, true
	case "--command-manifest":
		for _, definition := range cli.manifest.Commands() {
			fmt.Fprintln(cli.options.Stdout, definition.Row())
		}
		return 0, true
	case "--command-completion", "--command-options", "--command-verbs", "--command-effect":
		if len(arguments) < 2 {
			cli.errorf("%s needs a command", query)
			return 2, true
		}
		definition, ok := cli.manifest.Lookup(arguments[1])
		if !ok {
			return 2, true
		}
		switch query {
		case "--command-completion":
			fmt.Fprintln(cli.options.Stdout, definition.Completion)
		case "--command-options":
			fmt.Fprintln(cli.options.Stdout, strings.Join(definition.Options, " "))
		case "--command-verbs":
			fmt.Fprintln(cli.options.Stdout, strings.Join(definition.Verbs, " "))
		case "--command-effect":
			fmt.Fprintln(cli.options.Stdout, definition.Effect)
		}
		return 0, true
	default:
		if strings.HasPrefix(query, "-") {
			cli.errorf("unknown option %q\nTry %q.", query, cli.options.Program+" --help")
			return 2, true
		}
	}
	return 0, false
}

func (cli *CLI) usage() {
	fmt.Fprintf(cli.options.Stdout, "%s %s — a local yard for isolated project environments.\n\n", cli.options.Program, Version)
	fmt.Fprintf(cli.options.Stdout, "Usage: %s [option] <command> [args]\n\n", cli.options.Program)
	cli.usageSection("Yard lifecycle:", "lifecycle")
	cli.usageSection("Project workflow (default path '.'):", "projects")
	cli.usageSection("Project-env (L2 box; for a project added with --target <profile>):", "project_env")
	cli.usageSection("Remote-yard registry:", "remote")
	if definitions := cli.resources.Definitions(); len(definitions) != 0 {
		fmt.Fprintf(cli.options.Stdout, "Profile resources (long-lived in-yard services; run '%s <cmd> -h' for each):\n", cli.options.Program)
		for _, definition := range definitions {
			fmt.Fprintf(cli.options.Stdout, "  %-9s %s\n            verbs: %s\n", definition.Command, definition.Title, strings.Join(definition.Verbs, " "))
		}
		fmt.Fprintln(cli.options.Stdout)
	}
	fmt.Fprintf(cli.options.Stdout, `Named yards:
  Run several independent yards on one host, each with its own instance, /srv, ssh port,
  personal-data mount root and projects. Pick one for a command with -Y/--yard (or the
  sugar '@<name>' as the first token). Most commands still use the default yard without a
  selector; 'yard status' instead summarizes all known yards, while '-Y default status'
  shows its details. Define an installed yard in ~/.config/subyard/yards/<name>/config.env;
  source checkouts may also use
  private/yards/<name>.env (SSH_PORT required).
  '%s yards' lists them all.

Remote yards:
  Drive a yard on ANOTHER host as if it were local. '%s remote add <name> <user@host>'
  probes it, registers a context, wires a collision-free ProxyJump ssh identity, authorizes
  your key and verifies the complete data plane before reporting it ready.
  Then '%s -Y <name> <cmd>': lifecycle commands (start/stop/status/provision/logs/shell/...)
  forward over ssh to the owner host with their native prompts; data-plane commands
  (code/sync/export/clone/remove) go straight into the yard. 'bind' is host-local and
  disabled for remote yards. 'remote add' never copies secrets; only a separate confirmed
  'keys trust' permits authorized encrypted ledger records to sync between owner hosts.
  A real in-yard host-key change stays blocked. Verify its fingerprint on the trusted owner
  host, then use 'remote repair-key <name>' for an explicit, context-scoped rotation.
  Subcommands:  remote add <name> <user@host> [--yard <remote-yard>] | remote repair-key <name> | remote remove <name> | remote list

Options:
  -Y, --yard <name>  run the command against a named yard ('@<name>' first-token sugar)
  -h, --help         show this help and exit
  -l, --list         list command names (one per line) and exit
      --resources    list profile-resource commands + verbs and exit
  -V, --version      show version and exit
  -y, --yes          skip a command's confirmation prompt (pass-through)

Run '%s <command> -h' for a command's own help.
`, cli.options.Program, cli.options.Program, cli.options.Program, cli.options.Program)
}

func (cli *CLI) usageSection(title, section string) {
	fmt.Fprintln(cli.options.Stdout, title)
	for _, definition := range cli.manifest.Section(section) {
		fmt.Fprintf(cli.options.Stdout, "  %-20s %s\n", definition.Display, definition.Summary)
	}
	fmt.Fprintln(cli.options.Stdout)
}

func (cli *CLI) loadContext(yard string) (config.Loaded, error) {
	return cli.loadContextWithYardSettings(yard, "")
}

func (cli *CLI) loadContextWithYardSettings(yard, yardSettingsFile string) (config.Loaded, error) {
	loaded, err := cli.resolveContextWithYardSettings(yard, yardSettingsFile)
	if err != nil {
		return config.Loaded{}, err
	}
	cli.adoptContext(loaded)
	return loaded, nil
}

func (cli *CLI) resolveContextWithYardSettings(yard, yardSettingsFile string) (config.Loaded, error) {
	operatorHome := cli.env["SUBYARD_OPERATOR_HOME"]
	if operatorHome == "" {
		operatorHome = cli.env["HOME"]
	}
	return config.Load(config.LoadOptions{
		RepositoryRoot:   cli.options.RepositoryRoot,
		OperatorHome:     operatorHome,
		YardName:         yard,
		YardSettingsFile: yardSettingsFile,
		Environment:      cli.env,
	})
}

// resolveContextAllowPending loads only enough owner context to route config
// sync. It deliberately does not adopt an interrupted configuration into the
// command environment; local mutating sync recovers and performs a normal load
// after routing, while remote sync leaves this owner's journal untouched.
func (cli *CLI) resolveContextAllowPending(yard string) (config.Loaded, error) {
	operatorHome := cli.env["SUBYARD_OPERATOR_HOME"]
	if operatorHome == "" {
		operatorHome = cli.env["HOME"]
	}
	return config.Load(config.LoadOptions{
		RepositoryRoot:          cli.options.RepositoryRoot,
		OperatorHome:            operatorHome,
		YardName:                yard,
		Environment:             cli.env,
		AllowPendingTransaction: true,
	})
}

func (cli *CLI) adoptContext(loaded config.Loaded) {
	for name, value := range loaded.Environment {
		cli.env[name] = value
	}
	cli.env["SUBYARD_CONFIG_LOADED"] = "1"
	cli.env["SUBYARD_ENGINE_CONTEXT"] = "1"
	cli.env["SUBYARD_ENGINE_CONTEXT_SCHEMA"] = "1"
}

func (cli *CLI) runCommand(ctx context.Context, path string, arguments []string, extra map[string]string) int {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		cli.errorf("command handler is unavailable: %s", path)
		return 2
	}
	command := exec.CommandContext(ctx, path, arguments...)
	command.Dir = cli.options.WorkingDir
	command.Env = environmentList(cli.env, extra)
	command.Stdin = cli.options.Stdin
	command.Stdout = cli.options.Stdout
	command.Stderr = cli.options.Stderr
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return exitError.ExitCode()
		}
		cli.errorf("run command: %v", err)
		return 1
	}
	return 0
}

func (cli *CLI) forwardRemote(ctx context.Context, yardContext domain.Context, name string, arguments []string) int {
	remote := []string{"yard"}
	if yardContext.OwnerYardName != "" {
		remote = append(remote, "-Y", yardContext.OwnerYardName)
	}
	remote = append(remote, name)
	remote = append(remote, arguments...)
	parts := make([]string, len(remote))
	for index, argument := range remote {
		parts[index] = shellquote.Word(argument)
	}
	remoteLine := "SUBYARD_OPERATION_ID=" + shellquote.Word(cli.env["SUBYARD_OPERATION_ID"]) + " " + strings.Join(parts, " ")
	if name == "usage" {
		hint := "yard -Y " + cli.env["SUBYARD_YARD"] + " init"
		remoteLine = "SUBYARD_USAGE_REPAIR_HINT=" + shellquote.Word(hint) + " " + remoteLine
	}
	return cli.runExternal(ctx, "ssh", []string{"-t", yardContext.OwnerEndpoint, "--", "bash", "-lc", shellquote.Word(remoteLine)})
}

func (cli *CLI) runExternal(ctx context.Context, program string, arguments []string) int {
	command := exec.CommandContext(ctx, program, arguments...)
	command.Dir = cli.options.WorkingDir
	command.Env = environmentList(cli.env, nil)
	command.Stdin = cli.options.Stdin
	command.Stdout = cli.options.Stdout
	command.Stderr = cli.options.Stderr
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return exitError.ExitCode()
		}
		cli.errorf("run %s: %v", program, err)
		return 1
	}
	return 0
}

func (cli *CLI) audit(commandName string, arguments []string, yard, remote string) {
	home := cli.env["SUBYARD_HOME"]
	if home == "" {
		operatorHome := cli.env["SUBYARD_OPERATOR_HOME"]
		if operatorHome == "" {
			operatorHome = cli.env["HOME"]
		}
		home = filepath.Join(operatorHome, ".subyard")
	}
	_ = audit.WriteInvocation(audit.Invocation{
		Home: home, Command: commandName, Arguments: arguments, WorkingDir: cli.options.WorkingDir,
		Yard: yard, Remote: remote, Maximum: audit.MaximumFrom(cli.env["SUBYARD_AUDIT_MAX_BYTES"]),
		OperationID: cli.env["SUBYARD_OPERATION_ID"],
	})
}

func newOperationID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err == nil {
		return "op-" + hex.EncodeToString(value)
	}
	seed := fmt.Sprintf("%d-%d-%d", os.Getpid(), time.Now().UnixNano(), operationCounter.Add(1))
	hash := sha256.Sum256([]byte(seed))
	return "op-" + hex.EncodeToString(hash[:16])
}

func (cli *CLI) errorf(format string, arguments ...any) {
	fmt.Fprintf(cli.options.Stderr, "%s: ", cli.options.Program)
	fmt.Fprintf(cli.options.Stderr, format, arguments...)
	fmt.Fprintln(cli.options.Stderr)
}

func parseGlobals(arguments []string, inheritedYard string) (yard string, explicit, yes bool, remaining []string, err error) {
	yard = inheritedYard
	for len(arguments) != 0 {
		switch {
		case arguments[0] == "-Y" || arguments[0] == "--yard":
			if len(arguments) < 2 {
				return "", false, false, nil, fmt.Errorf("unknown option %q", arguments[0])
			}
			yard, explicit, arguments = arguments[1], true, arguments[2:]
		case strings.HasPrefix(arguments[0], "--yard="):
			yard, explicit, arguments = strings.TrimPrefix(arguments[0], "--yard="), true, arguments[1:]
		case strings.HasPrefix(arguments[0], "@") && len(arguments[0]) > 1:
			yard, explicit, arguments = strings.TrimPrefix(arguments[0], "@"), true, arguments[1:]
		case arguments[0] == "-y" || arguments[0] == "--yes":
			yes, arguments = true, arguments[1:]
		default:
			if yard == "" {
				yard = "default"
			}
			return yard, explicit, yes, arguments, nil
		}
	}
	if yard == "" {
		yard = "default"
	}
	return yard, explicit, yes, arguments, nil
}

func environmentMap(environment []string) map[string]string {
	values := make(map[string]string)
	for _, pair := range environment {
		name, value, ok := strings.Cut(pair, "=")
		if ok {
			values[name] = value
		}
	}
	return values
}

func environmentList(values map[string]string, extra map[string]string) []string {
	merged := make(map[string]string, len(values)+len(extra))
	for key, value := range values {
		merged[key] = value
	}
	for key, value := range extra {
		merged[key] = value
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+merged[key])
	}
	return environment
}

func (cli *CLI) serveRPC(ctx context.Context, yard string, arguments []string) int {
	if len(arguments) != 1 || arguments[0] != "--stdio" {
		cli.errorf("rpc requires exactly --stdio")
		return 2
	}
	loaded, err := cli.loadContext(yard)
	if err != nil {
		cli.errorf("load RPC context: %v", err)
		return 2
	}
	// An RPC session is bound to one validated context. Cross-yard selection is represented as a
	// remote-owner route, never as an implicit context switch inside the session.
	cli.env["SUBYARD_YARD_EXPLICIT"] = "1"
	handler := &rpcHandler{cli: cli, loaded: loaded, plans: make(map[string]rpcPlannedOperation)}
	session := rpc.Session{Handler: handler, EngineVersion: Version, Capabilities: []string{
		"snapshot", "ordered-events", "cancellation", "deadlines", "commands", "context",
		"projects", "yard-status", "credential-metadata", "credential-status",
		"operation-plan", "operation-execute", "resync", "owner-inventory-v1",
		credentialPrepareCapability,
	}, DrainOnEOF: true}
	if err := session.Serve(ctx, cli.options.Stdin, cli.options.Stdout); err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
			cli.errorf("RPC session: %v", err)
			return 1
		}
	}
	return 0
}

type rpcHandler struct {
	cli     *CLI
	loaded  config.Loaded
	plansMu sync.Mutex
	plans   map[string]rpcPlannedOperation
}

type rpcPlannedOperation struct {
	Plan       domain.OperationPlan
	Definition command.Definition
	Arguments  []string
	Loaded     config.Loaded
	Project    *projectExecution
	Remote     *domain.RemotePrepared
	Init       *initExecution
	Lifecycle  *lifecycleExecution
	Provision  *provisionExecution
	TestVMs    *testVMExecution
	Teardown   *teardownExecution
	Release    *releaseExecution
}

type rpcProjectList struct {
	Projects    []domain.ProjectRecord    `json:"projects"`
	Observation domain.ProjectObservation `json:"observation"`
}

type rpcSnapshot struct {
	Revision         uint64                      `json:"revision"`
	Context          domain.Context              `json:"context"`
	Commands         []map[string]any            `json:"commands"`
	Projects         rpcProjectList              `json:"projects"`
	Status           domain.YardStatus           `json:"status"`
	Credentials      []domain.CredentialMetadata `json:"credentials"`
	CredentialStatus domain.CredentialStatus     `json:"credentialStatus"`
}

func operationRPCError(fallback string, err error) *rpc.Error {
	code := fallback
	if errors.Is(err, domain.ErrPlanStale) {
		code = domain.PlanStaleCode
	} else if errors.Is(err, domain.ErrActionPolicyInvalid) ||
		domain.ActionPolicyErrorClass(err) == domain.ActionPolicyInvalid {
		code = domain.ActionPolicyInvalid
	}
	return &rpc.Error{Code: code, Message: err.Error()}
}

func (handler *rpcHandler) Handle(ctx context.Context, call rpc.Call, emit rpc.Emit) (any, error) {
	switch call.Method {
	case "command.list":
		return handler.commands(), nil
	case "context.get":
		return handler.loaded.Context, nil
	case "keys.prepare":
		var params struct {
			Arguments []string `json:"arguments"`
		}
		if err := decodeRPCParams(call.Params, &params); err != nil {
			return nil, err
		}
		definition, ok := handler.cli.manifest.Lookup("keys")
		if !ok || definition.Visibility != command.VisibilityPublic {
			return nil, &rpc.Error{Code: "command_not_found", Message: "keys"}
		}
		previousOperationID := handler.cli.env["SUBYARD_OPERATION_ID"]
		handler.cli.env["SUBYARD_OPERATION_ID"] = call.OperationID
		defer func() { handler.cli.env["SUBYARD_OPERATION_ID"] = previousOperationID }()
		credentialRuntime, err := handler.cli.credentialRuntimeWithStreams(
			handler.loaded, strings.NewReader(""), io.Discard, io.Discard,
		)
		if err != nil {
			return nil, operationRPCError("plan_failed", err)
		}
		prepared, err := credentialRuntime.Prepare(
			ctx, definition.Arg0, keysWithoutConsent(params.Arguments),
		)
		if err != nil {
			return nil, operationRPCError("plan_failed", err)
		}
		orchestrator := handler.cli.operationOrchestrator(call.OperationID, handler.loaded, nil, &definition)
		plan, err := orchestrator.PrepareAction(
			handler.loaded.Context, publicKeysCommandName(params.Arguments),
			domain.RemotePolicy(definition.Remote), prepared.Action,
			domain.ActionDelta{Changed: prepared.Changed, Consequences: prepared.Consequences},
		)
		if err != nil {
			return nil, operationRPCError("plan_failed", err)
		}
		return plan, nil
	case "operation.plan":
		var params struct {
			Command   string   `json:"command"`
			Arguments []string `json:"arguments"`
		}
		if err := decodeRPCParams(call.Params, &params); err != nil {
			return nil, err
		}
		definition, ok := handler.cli.manifest.Lookup(params.Command)
		if !ok || definition.Visibility != command.VisibilityPublic {
			return nil, &rpc.Error{Code: "command_not_found", Message: params.Command}
		}
		if definition.Effect != command.EffectMutate {
			return nil, &rpc.Error{Code: "command_not_mutating", Message: params.Command}
		}
		if definition.Name != "update" && !structuredCommandSupported(definition.Name) {
			return nil, &rpc.Error{Code: "interactive_or_payload_command", Message: params.Command}
		}
		previousOperationID := handler.cli.env["SUBYARD_OPERATION_ID"]
		handler.cli.env["SUBYARD_OPERATION_ID"] = call.OperationID
		project, err := handler.cli.prepareProjectExecution(
			ctx, handler.loaded, definition, params.Arguments, true,
		)
		handler.cli.env["SUBYARD_OPERATION_ID"] = previousOperationID
		if err != nil {
			return nil, operationRPCError("invalid_params", err)
		}
		keepProjectReservation := false
		defer func() {
			if !keepProjectReservation {
				handler.cli.abortProjectExecution(context.Background(), project)
			}
		}()
		loaded := handler.loaded
		arguments := append([]string(nil), params.Arguments...)
		if project != nil {
			loaded = project.Loaded
			arguments = project.Arguments
		}
		var remote *domain.RemotePrepared
		if definition.Name == "remote" {
			remote, err = handler.cli.prepareRemoteExecution(ctx, loaded, arguments)
			if err != nil {
				return nil, operationRPCError("invalid_params", err)
			}
		}
		var initRun *initExecution
		if definition.Handler == "@init" && loaded.Context.AccessKind != domain.AccessRemote {
			initRun, err = handler.cli.prepareInitExecution(ctx, loaded, arguments, nil)
			if err != nil {
				return nil, operationRPCError("plan_failed", err)
			}
		}
		var releaseRun *releaseExecution
		if definition.Handler == "@update" {
			releaseRun, err = handler.cli.prepareRelease(ctx, loaded, arguments)
			if err != nil {
				return nil, operationRPCError("plan_failed", err)
			}
		}
		var lifecycleRun *lifecycleExecution
		if definition.Handler == "@lifecycle" {
			lifecycleRun, err = prepareLifecycleExecution(definition, arguments)
			if err != nil {
				return nil, operationRPCError("invalid_params", err)
			}
		}
		var provisionRun *provisionExecution
		if definition.Handler == "@provision" {
			provisionRun, err = handler.cli.prepareProvisionExecution(loaded, arguments, project)
			if err != nil {
				return nil, operationRPCError("invalid_params", err)
			}
		}
		var testVMRun *testVMExecution
		if definition.Handler == "@test-vms" {
			testVMRun, err = handler.cli.prepareTestVMExecution(ctx, loaded, arguments)
			if err != nil {
				return nil, operationRPCError("invalid_params", err)
			}
		}
		var teardownRun *teardownExecution
		if definition.Handler == "@teardown" {
			teardownRun, err = prepareTeardownExecution(arguments)
			if err != nil {
				return nil, operationRPCError("invalid_params", err)
			}
		}
		policy := commandPolicy(definition, loaded.Context, arguments, project, remote)
		if initRun != nil {
			policy.Consequences = initRun.consequences()
		}
		if lifecycleRun != nil {
			policy = lifecycleRun.policy(definition, loaded.Context)
		}
		if provisionRun != nil {
			policy = provisionRun.policy(definition, loaded.Context)
		}
		if teardownRun != nil {
			policy = teardownRun.policy(definition, loaded.Context)
		}
		action, delta, typedAction, actionErr := handler.cli.assessStructuredAction(
			ctx, loaded, definition, project, initRun, lifecycleRun, provisionRun, teardownRun,
		)
		if actionErr != nil {
			return nil, operationRPCError("plan_failed", actionErr)
		}
		orchestrator := handler.cli.operationOrchestrator(call.OperationID, loaded, nil, nil)
		var plan domain.OperationPlan
		if remote != nil {
			plan, err = handler.cli.prepareRemoteOperation(orchestrator, loaded, *remote)
		} else if releaseRun != nil {
			plan, err = releaseRun.prepareAction(orchestrator, loaded, definition)
		} else if testVMRun != nil {
			plan, err = testVMRun.prepareAction(orchestrator, loaded, definition)
		} else if typedAction {
			plan, err = orchestrator.PrepareAction(
				loaded.Context, definition.Name, domain.RemotePolicy(definition.Remote),
				action, delta,
			)
		} else {
			policy = resolveCommandConfirmation(definition, policy)
			plan, err = orchestrator.Prepare(loaded.Context, policy)
		}
		if err != nil {
			return nil, operationRPCError("plan_failed", err)
		}
		handler.plansMu.Lock()
		if handler.plans == nil {
			handler.plans = make(map[string]rpcPlannedOperation)
		}
		if _, exists := handler.plans[plan.OperationID]; exists {
			handler.plansMu.Unlock()
			return nil, &rpc.Error{Code: "duplicate_plan", Message: plan.OperationID}
		}
		if len(handler.plans) >= 64 {
			handler.plansMu.Unlock()
			return nil, &rpc.Error{Code: "too_many_plans", Message: "execute an existing plan or start a new RPC session"}
		}
		handler.plans[plan.OperationID] = rpcPlannedOperation{
			Plan: plan, Definition: definition, Arguments: arguments, Loaded: loaded, Project: project,
			Remote: remote, Init: initRun, Lifecycle: lifecycleRun, Provision: provisionRun,
			TestVMs: testVMRun, Teardown: teardownRun, Release: releaseRun,
		}
		handler.plansMu.Unlock()
		keepProjectReservation = true
		return plan, nil
	case "operation.execute":
		var params struct {
			Confirmed bool `json:"confirmed"`
		}
		if err := decodeRPCParams(call.Params, &params); err != nil {
			return nil, err
		}
		if !params.Confirmed {
			return nil, &rpc.Error{Code: "confirmation_required", Message: "execute requires confirmed=true"}
		}
		handler.plansMu.Lock()
		planned, ok := handler.plans[call.OperationID]
		if ok {
			delete(handler.plans, call.OperationID)
		}
		handler.plansMu.Unlock()
		if !ok {
			return nil, &rpc.Error{Code: "plan_not_found", Message: call.OperationID}
		}
		defer handler.cli.abortProjectExecution(context.Background(), planned.Project)
		if planned.Plan.Target == domain.TargetRemoteOwner {
			return nil, &rpc.Error{Code: "remote_owner_required", Message: "execute this plan through owner-host SSH stdio"}
		}
		orchestrator := handler.cli.operationOrchestrator(
			call.OperationID, planned.Loaded, rpcOperationEvents{emit: emit}, &planned.Definition,
		)
		plan, err := orchestrator.Confirm(ctx, planned.Plan, true)
		if err != nil {
			return nil, operationRPCError("confirmation_failed", err)
		}
		var result domain.AdapterResult
		if planned.Release != nil {
			result, err = handler.cli.executeRelease(ctx, orchestrator, plan, planned.Release)
		} else {
			result, err = handler.cli.executeStructuredCommand(
				ctx, orchestrator, planned.Loaded, planned.Definition, planned.Arguments,
				plan, planned.Project, planned.Remote, planned.Init, planned.Lifecycle,
				planned.Provision, planned.TestVMs, planned.Teardown, handler.cli.options.Stderr,
			)
		}
		if err != nil {
			if errors.Is(err, domain.ErrPlanStale) || errors.Is(err, domain.ErrActionPolicyInvalid) ||
				domain.ActionPolicyErrorClass(err) == domain.ActionPolicyInvalid {
				return nil, operationRPCError("operation_failed", err)
			}
			return nil, err
		}
		if result.Status != "ok" {
			return nil, &rpc.Error{Code: "adapter_failed", Message: result.ErrorCode}
		}
		if planned.Project != nil && !operationPlanNoOp(plan) {
			if err := handler.cli.commitProjectExecution(ctx, planned.Project); err != nil {
				return nil, &rpc.Error{Code: "state_commit_failed", Message: err.Error()}
			}
		}
		return map[string]any{"plan": plan, "result": result}, nil
	case "operation.route":
		var params struct {
			Command string `json:"command"`
		}
		if err := decodeRPCParams(call.Params, &params); err != nil {
			return nil, err
		}
		definition, ok := handler.cli.manifest.Lookup(params.Command)
		if !ok || definition.Visibility != command.VisibilityPublic {
			return nil, &rpc.Error{Code: "command_not_found", Message: params.Command}
		}
		target, err := application.Route(handler.loaded.Context, domain.RemotePolicy(definition.Remote))
		if err != nil {
			return nil, &rpc.Error{Code: "route_denied", Message: err.Error()}
		}
		return map[string]any{
			"operationId": call.OperationID, "command": definition.Name, "effect": definition.Effect,
			"target": target, "summary": definition.Summary,
		}, nil
	case "project.list":
		var params struct {
			Live bool `json:"live"`
		}
		if err := decodeRPCParams(call.Params, &params); err != nil {
			return nil, err
		}
		return handler.projects(ctx, params.Live)
	case "owner.inventory":
		if err := decodeRPCParams(call.Params, &struct{}{}); err != nil {
			return nil, err
		}
		inventory, err := handler.cli.ownerInventoryReadOnly(ctx, handler.loaded)
		if err != nil {
			return nil, &rpc.Error{Code: "owner_inventory_failed", Message: err.Error()}
		}
		return inventory, nil
	case "yard.status":
		if err := decodeRPCParams(call.Params, &struct{}{}); err != nil {
			return nil, err
		}
		return handler.status(ctx)
	case "credential.list":
		if err := decodeRPCParams(call.Params, &struct{}{}); err != nil {
			return nil, err
		}
		return handler.credentials().ListMetadata(ctx)
	case "credential.status":
		if err := decodeRPCParams(call.Params, &struct{}{}); err != nil {
			return nil, err
		}
		return handler.credentialStatus(ctx)
	case "incus.events":
		var params struct {
			Types []string `json:"types"`
		}
		if err := decodeRPCParams(call.Params, &params); err != nil {
			return nil, err
		}
		if len(params.Types) == 0 {
			params.Types = []string{"lifecycle", "operation"}
		}
		if len(params.Types) > 8 {
			return nil, &rpc.Error{Code: "invalid_params", Message: "too many Incus event types"}
		}
		for _, eventType := range params.Types {
			if !domain.SafeName(eventType) {
				return nil, &rpc.Error{Code: "invalid_params", Message: "invalid Incus event type"}
			}
		}
		incusPort, _ := handler.cli.statusPorts()
		events, errorsOut := incusPort.Events(ctx, params.Types)
		for events != nil || errorsOut != nil {
			select {
			case event, ok := <-events:
				if !ok {
					events = nil
					continue
				}
				if _, err := emit("incus."+event.Kind, event); err != nil {
					return nil, err
				}
			case streamErr, ok := <-errorsOut:
				if !ok {
					errorsOut = nil
					continue
				}
				if streamErr != nil {
					return nil, &rpc.Error{Code: "incus_disconnected", Message: streamErr.Error()}
				}
			case <-ctx.Done():
				return nil, context.Cause(ctx)
			}
		}
		return map[string]any{"closed": true}, nil
	case "system.snapshot", "system.resync":
		if err := decodeRPCParams(call.Params, &struct{}{}); err != nil {
			return nil, err
		}
		projects, err := handler.projects(ctx, false)
		if err != nil {
			return nil, err
		}
		status, err := handler.status(ctx)
		if err != nil {
			return nil, err
		}
		credentials, err := handler.credentials().ListMetadata(ctx)
		if err != nil {
			return nil, err
		}
		credentialStatus, err := handler.credentialStatus(ctx)
		if err != nil {
			return nil, err
		}
		snapshot := rpcSnapshot{
			Context: handler.loaded.Context, Commands: handler.commands(), Projects: projects,
			Status: status, Credentials: credentials, CredentialStatus: credentialStatus,
		}
		revision, err := emit("snapshot.ready", map[string]any{"complete": true})
		if err != nil {
			return nil, err
		}
		snapshot.Revision = revision
		return snapshot, nil
	case "system.ping":
		if _, err := emit("operation.started", map[string]any{"method": call.Method}); err != nil {
			return nil, err
		}
		if _, err := emit("operation.finished", map[string]any{"method": call.Method}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	default:
		return nil, &rpc.Error{Code: "method_not_found", Message: call.Method}
	}
}

func operationPlanNoOp(plan domain.OperationPlan) bool {
	if plan.Assessment == nil || plan.Assessment.Changed {
		return false
	}
	return plan.Assessment.Effect == domain.ActionMutation ||
		plan.Assessment.Effect == domain.ActionDestruction
}

func (handler *rpcHandler) commands() []map[string]any {
	definitions := handler.cli.manifest.Commands()
	result := make([]map[string]any, 0, len(definitions))
	for _, definition := range definitions {
		if definition.Visibility != command.VisibilityPublic {
			continue
		}
		result = append(result, map[string]any{
			"name": definition.Name, "aliases": definition.Aliases, "effect": definition.Effect,
			"remote": definition.Remote, "summary": definition.Summary,
			"options": definition.Options, "verbs": definition.Verbs,
		})
	}
	return result
}

func (handler *rpcHandler) projects(ctx context.Context, live bool) (rpcProjectList, error) {
	store, err := openProjectStore(ctx, handler.loaded.Context.Paths.StateDir)
	if err != nil {
		return rpcProjectList{}, err
	}
	inventory := application.ProjectInventory{Store: store}
	records, observation, err := inventory.Read(ctx, handler.loaded.Context, live)
	return rpcProjectList{Projects: records, Observation: observation}, err
}

func (handler *rpcHandler) status(ctx context.Context) (domain.YardStatus, error) {
	store, err := openProjectStore(ctx, handler.loaded.Context.Paths.StateDir)
	if err != nil {
		return domain.YardStatus{}, err
	}
	incusPort, executor := handler.cli.statusPorts()
	return (application.StatusService{
		Incus: incusPort, Executor: executor, Store: store,
		Facts: handler.cli.statusFacts(handler.loaded),
	}).Read(ctx, handler.loaded.Context)
}

func (handler *rpcHandler) credentials() ports.CredentialMetadataReader {
	if handler.cli.options.Credentials != nil {
		return handler.cli.options.Credentials
	}
	root := handler.loaded.Environment["SUBYARD_KEYS_ROOT"]
	if root == "" {
		root = filepath.Join(handler.loaded.Context.Paths.ConfigHome, "keys")
	}
	return credentialmeta.Reader{Root: root}
}

func (handler *rpcHandler) credentialStatus(ctx context.Context) (domain.CredentialStatus, error) {
	reader := handler.credentials()
	if statusReader, ok := reader.(ports.CredentialStatusReader); ok {
		return statusReader.ReadCredentialStatus(ctx)
	}
	metadata, err := reader.ListMetadata(ctx)
	if err != nil {
		return domain.CredentialStatus{}, err
	}
	summaries, err := credential.Summarize(metadata)
	if err != nil {
		return domain.CredentialStatus{}, err
	}
	return domain.CredentialStatus{Credentials: summaries, Peers: []domain.CredentialPeerStatus{}}, nil
}

func decodeRPCParams(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &rpc.Error{Code: "invalid_params", Message: err.Error()}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return &rpc.Error{Code: "invalid_params", Message: "params have trailing data"}
	}
	return nil
}
