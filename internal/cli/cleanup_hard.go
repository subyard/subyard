package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/uninstall"
)

type cleanupStateEntry struct {
	Path      string
	Mode      uint32
	GID       int
	Content   []byte
	Directory bool
}
type hardCleanupJournal struct {
	Schema                                                   int
	Token, Phase, ConfigBinding, SourceBinding, StateBinding string
	Context                                                  domain.Context
	Environment                                              map[string]string
	Native                                                   ports.YardCleanupBackup
	Projects                                                 []domain.ProjectRecord
	State                                                    []cleanupStateEntry
	Preserved                                                json.RawMessage
	Reset                                                    *uninstall.RecoveryPlan
	Retirement                                               *releasetransition.RegisteredYardJournalBinding
	RetiredBindings                                          []releasetransition.RegisteredYardJournalBinding
	ReplacementUUID                                          string
	Provision                                                map[string]profile.ProvisionBinding
}

func cleanupAncestors(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("unsafe cleanup recovery path")
	}
	for parent := filepath.Dir(path); parent != "/"; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("cleanup recovery ancestor is not a real directory")
		}
	}
	return nil
}

func captureCleanupState(root string) ([]cleanupStateEntry, error) {
	if err := cleanupAncestors(root); err != nil {
		return nil, err
	}
	var entries []cleanupStateEntry
	var size int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) && path == root {
			return nil
		}
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || (!info.IsDir() && (!info.Mode().IsRegular() || st.Nlink != 1)) {
			return errors.New("project registry backup requires owned ordinary files and directories")
		}
		relative, _ := filepath.Rel(root, path)
		item := cleanupStateEntry{Path: relative, Mode: uint32(info.Mode().Perm()), GID: int(st.Gid), Directory: info.IsDir()}
		if !info.IsDir() {
			size += info.Size()
			if size > 16<<20 {
				return errors.New("project registry exceeds backup limit")
			}
			item.Content, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		entries = append(entries, item)
		return nil
	})
	return entries, err
}

func restoreCleanupState(root string, entries []cleanupStateEntry) error {
	if err := cleanupAncestors(root); err != nil {
		return err
	}
	for _, entry := range entries {
		if filepath.Clean(entry.Path) != entry.Path || filepath.IsAbs(entry.Path) || entry.Path == ".." || strings.HasPrefix(entry.Path, "../") {
			return errors.New("invalid registry backup entry")
		}
		target := filepath.Join(root, entry.Path)
		if entry.Directory {
			if err := os.Mkdir(target, os.FileMode(entry.Mode)); err != nil && !os.IsExist(err) {
				return err
			}
			info, err := os.Lstat(target)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return domain.ErrPlanStale
			}
			if err := os.Chmod(target, os.FileMode(entry.Mode)); err != nil {
				return err
			}
			if err := os.Chown(target, -1, entry.GID); err != nil {
				return err
			}
			continue
		}
		file, err := os.CreateTemp(filepath.Dir(target), ".cleanup-state-*")
		if err != nil {
			return err
		}
		name := file.Name()
		writeErr := func() error {
			if err := file.Chmod(os.FileMode(entry.Mode)); err != nil {
				return err
			}
			if err := file.Chown(-1, entry.GID); err != nil {
				return err
			}
			if _, err := file.Write(entry.Content); err != nil {
				return err
			}
			return file.Sync()
		}()
		closeErr := file.Close()
		if writeErr == nil {
			writeErr = closeErr
		}
		if writeErr == nil {
			writeErr = os.Rename(name, target)
		}
		_ = os.Remove(name)
		if writeErr != nil {
			return writeErr
		}
	}
	actual, err := captureCleanupState(root)
	if err != nil {
		return err
	}
	if operationStateDigest(actual) != operationStateDigest(entries) {
		return errors.New("restored project registry differs from backup")
	}
	if len(entries) > 0 {
		dir, err := os.Open(root)
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	}
	return nil
}
func hardCleanupPath(yard domain.Context) string {
	key := sha256.Sum256([]byte(yard.IncusProject + "\x00" + yard.YardInstanceName))
	return filepath.Join(yard.Paths.DataHome, "cleanup", hex.EncodeToString(key[:12]), "journal.json")
}

func readHardCleanup(path string) (*hardCleanupJournal, error) {
	if err := cleanupAncestors(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 || info.Size() > 32<<20 {
		return nil, errors.New("unsafe hard cleanup recovery journal")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 32<<20))
	decoder.DisallowUnknownFields()
	var journal hardCleanupJournal
	if err := decoder.Decode(&journal); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF || journal.Schema != 1 || !domain.SafeID(journal.Token) {
		return nil, errors.New("invalid hard cleanup recovery journal")
	}
	return &journal, nil
}

func saveHardCleanup(path string, journal *hardCleanupJournal) error {
	return saveCleanupRecord(path, journal)
}

func saveCleanupRecord(path string, record any) error {
	if err := cleanupAncestors(path); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	for _, directory := range []string{filepath.Dir(parent), parent} {
		if err := os.Mkdir(directory, 0700); err == nil {
			if err := os.Chmod(directory, 0700); err != nil {
				return err
			}
		} else if !os.IsExist(err) {
			return err
		}
		info, err := os.Lstat(directory)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || st.Uid != uint32(os.Getuid()) {
			return errors.New("unsafe hard cleanup recovery directory")
		}
		ancestor, err := os.Open(filepath.Dir(directory))
		if err != nil {
			return err
		}
		err = ancestor.Sync()
		ancestor.Close()
		if err != nil {
			return err
		}
	}
	content, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(content) > 32<<20 {
		return errors.New("cleanup recovery record exceeds size limit")
	}
	file, err := os.CreateTemp(parent, ".journal-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	_, writeErr := file.Write(append(content, '\n'))
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func cleanupConfigBinding(loaded config.Loaded) string {
	values := map[string]string{}
	for name, setting := range loaded.Settings {
		values[name] = setting.EffectiveValue
	}
	return operationStateDigest([]any{loaded.Context, values})
}

func cleanupSourceBinding(root string) (string, error) {
	hash := sha256.New()
	for _, directory := range []string{"scripts", "config"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "tests" || entry.Name() == "__pycache__" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(entry.Name(), ".pyc") || entry.Name() == "profile.env" {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return errors.New("hard cleanup runtime sources must be regular files")
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, _ := filepath.Rel(root, path)
			fmt.Fprintf(hash, "%s\x00%o\x00%x\n", relative, info.Mode(), sha256.Sum256(content))
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (prepared *preparedCommand) prepareHardCleanup(ctx context.Context) error {
	cli, loaded := prepared.CLI.hardCleanupOperation(), prepared.Loaded
	if loaded.Context.AccessKind != domain.AccessLocal {
		return errors.New("hard cleanup must execute on the selected yard owner")
	}
	if err := cli.checkNativeTargets(loaded, false, nil); err != nil {
		return err
	}
	reader, _ := cli.statusPorts()
	native, ok := reader.(ports.YardBackupManager)
	if !ok {
		return errors.New("native independent yard backup is unavailable")
	}
	path := hardCleanupPath(loaded.Context)
	journal, err := readHardCleanup(path)
	if err != nil {
		return err
	}
	source, err := cleanupSourceBinding(cli.options.RepositoryRoot)
	if err != nil {
		return err
	}
	inspector, err := os.ReadFile(filepath.Join(cli.options.RepositoryRoot, "scripts/lib/yard-cleanup-hard.py"))
	if err != nil {
		return err
	}
	if _, err := os.Lstat(loaded.Context.Paths.DataHome + ".uninstall"); err == nil {
		return errors.New("finish full installation recovery before hard cleanup")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, pending, err := configsync.AssessHostIDRenameRecovery(loaded.Context.Paths.ConfigHome); err != nil {
		return err
	} else if pending {
		return errors.New("finish owner HostID rename recovery before hard cleanup")
	}
	if journal == nil {
		baseline, err := cli.prepareResetTeardownBaseline(ctx, loaded)
		if err != nil {
			return err
		}
		for _, resource := range baseline.resources() {
			if resource.Kind == "instance" && resource.Name != loaded.Context.YardInstanceName {
				return errors.New("hard cleanup refuses additional instances in selected project")
			}
		}
		store, err := state.NewFileStore(loaded.Context.Paths.StateDir)
		if err != nil {
			return err
		}
		projects, err := store.ListReadOnly(ctx)
		if err != nil {
			return err
		}
		entries, err := captureCleanupState(loaded.Context.Paths.StateDir)
		if err != nil {
			return err
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		token := hex.EncodeToString(random[:])
		binds := map[string]string{}
		for _, project := range projects {
			if project.Mode == domain.ProjectBind {
				binds[state.WorkspaceDeviceFor(project)] = project.HostPath
			}
		}
		plan, err := native.PrepareYardBackup(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName, loaded.Environment["SRV_POOL"], loaded.Environment["SRV_VOLUME"], token, binds)
		if err != nil {
			return err
		}
		instance, err := reader.Instance(ctx, plan.Project, plan.Instance)
		if err != nil {
			return err
		}
		for key, want := range map[string]string{"user.subyard.managed": "true", "user.subyard.name": loaded.Context.YardName, "user.subyard.bridge": loaded.Context.IncusBridge} {
			if instance.Config[key] != want || instance.LocalConfig[key] != want {
				return errors.New("selected yard native ownership differs from registration")
			}
		}
		for _, project := range projects {
			if project.Mode == domain.ProjectBind && instance.Devices[state.WorkspaceDeviceFor(project)]["path"] != project.YardPath {
				return errors.New("registered host bind destination differs")
			}
		}
		journal = &hardCleanupJournal{Schema: 1, Token: token, Phase: "backup", Context: loaded.Context, Environment: loaded.Environment, ConfigBinding: cleanupConfigBinding(loaded), SourceBinding: source, State: entries, StateBinding: operationStateDigest(entries), Projects: projects, Native: plan, Provision: map[string]profile.ProvisionBinding{}}
		for _, name := range strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]) {
			pkg, err := profile.ResolvePackage(cli.options.RepositoryRoot, name)
			if err != nil {
				return err
			}
			if pkg.Provision != nil {
				binding, err := config.ProfileProvisionBinding(pkg, loaded.Environment)
				if err != nil {
					return err
				}
				journal.Provision[name] = binding
			}
		}
	} else if journal.ConfigBinding != cleanupConfigBinding(loaded) || journal.SourceBinding != source || journal.Context.IncusProject != loaded.Context.IncusProject || journal.Context.YardInstanceName != loaded.Context.YardInstanceName {
		return fmt.Errorf("%w: restore the original configuration and runtime before resuming cleanup", domain.ErrPlanStale)
	}
	check := func(ctx context.Context) error {
		current, err := cleanupSourceBinding(cli.options.RepositoryRoot)
		if err != nil {
			return err
		}
		if current != journal.SourceBinding {
			return domain.ErrPlanStale
		}
		if journal.Phase == "backup" {
			entries, err := captureCleanupState(loaded.Context.Paths.StateDir)
			if err != nil {
				return err
			}
			if operationStateDigest(entries) != journal.StateBinding {
				return domain.ErrPlanStale
			}
			return native.CheckYardBackupSource(ctx, journal.Native)
		}
		if journal.Phase != "verified" {
			return native.CheckYardBackup(ctx, journal.Native)
		}
		return nil
	}
	if err := check(ctx); err != nil {
		return err
	}
	consequences := []string{"stop selected yard; independently back up and verify complete registered project wrappers and registration", "delete selected environment and recreate it through the current lifecycle using its known configuration", "restore project bytes, Git data, file modes, symlinks, reservations and exact external binds; discard all other guest data", "retain independent backup and recovery checkpoint on interruption; rerun cleanup --hard with the same selected yard"}
	prepared.policy.Consequences = consequences
	prepared.exactState = operationStateDigest(journal)
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return "yard.cleanup.hard", domain.ActionDelta{Changed: true, Consequences: consequences}, nil
	}
	prepared.refresh = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		if err := check(ctx); err != nil {
			return "", domain.ActionDelta{}, err
		}
		return prepared.assess(ctx)
	}
	prepared.stepsComplete = true
	prepared.steps = func() []domain.OperationStep {
		var steps []domain.OperationStep
		for _, phase := range []string{"backup", "verify", "reset", "restore"} {
			steps = append(steps, domain.OperationStep{ID: "cleanup.hard." + phase, Target: loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName, Observed: "owned selected yard and independent backup boundary", Desired: phase + " projects with exact registration and bind semantics", Decision: domain.StepApply, Verify: "native operation completion and project content comparison", Consequence: "recreate selected environment while restoring registered projects from verified backup"})
		}
		return steps
	}
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, out io.Writer) (domain.AdapterResult, error) {
		if err := check(ctx); err != nil {
			return domain.AdapterResult{}, err
		}
		if cli.options.InitPlatform == nil {
			if err := cli.prepareSudoPrivileges(ctx, out, cli.effectiveUID(), "cleanup"); err != nil {
				return domain.AdapterResult{}, err
			}
		}
		if err := saveHardCleanup(path, journal); err != nil {
			return domain.AdapterResult{}, err
		}
		if err := cli.applyHardCleanup(ctx, loaded, journal, path, native, inspector, out); err != nil {
			return domain.AdapterResult{}, fmt.Errorf("hard cleanup retained its backup/checkpoint; rerun cleanup --hard for the same yard: %w", err)
		}
		return domain.AdapterResult{Schema: 1, OperationID: prepared.Plan.OperationID, Status: "ok"}, nil
	}
	return nil
}

func (cli *CLI) boundHardCleanupRecovery(_ context.Context, loaded config.Loaded, arguments []string) (bool, error) {
	hard, err := cleanupMode(arguments)
	if err != nil || !hard {
		return false, err
	}
	journal, err := readHardCleanup(hardCleanupPath(loaded.Context))
	if err != nil || journal == nil {
		return false, err
	}
	if !slices.Contains([]string{"backup", "reset", "recreate", "restored", "verified"}, journal.Phase) {
		return false, errors.New("unknown hard cleanup checkpoint")
	}
	source, err := cleanupSourceBinding(cli.options.RepositoryRoot)
	if err != nil {
		return false, err
	}
	if journal.ConfigBinding != cleanupConfigBinding(loaded) || journal.SourceBinding != source {
		return false, domain.ErrPlanStale
	}
	return true, nil
}

func (cli *CLI) cleanupPower(ctx context.Context, loaded config.Loaded, action string) error {
	reader, _ := cli.statusPorts()
	instance, err := reader.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if err != nil {
		return err
	}
	desired := map[string]string{"start": "running", "stop": "stopped"}[action]
	if strings.EqualFold(instance.Status, desired) {
		return nil
	}
	if cli.options.InitPlatform == nil {
		runtime := cli.initPlatform(loaded, nil).(reconcileruntime.Runtime)
		return runtime.RunMigrationPhysicalLifecycle(ctx, action)
	}
	power, ok := reader.(ports.InstancePowerManager)
	if !ok {
		return errors.New("native power lifecycle unavailable")
	}
	return power.SetInstancePower(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName, action, false)
}

func (cli *CLI) cleanupGuest(ctx context.Context, loaded config.Loaded, phase string, request ports.InstanceExecRequest) ([]byte, error) {
	result, err := cli.projectDataPlane().Execute(ctx, loaded.Context, request)
	if err != nil || result.ExitCode != 0 {
		var failures []error
		if err != nil {
			failures = append(failures, err)
		}
		if result.ExitCode != 0 {
			failures = append(failures, fmt.Errorf("instance command exited with status %d", result.ExitCode))
		}
		if message := strings.TrimSpace(string(result.Stderr)); message != "" {
			failures = append(failures, errors.New(message))
		}
		return nil, fmt.Errorf("cleanup guest %s: %w", phase, errors.Join(failures...))
	}
	return result.Stdout, nil
}

// Incus PathNameEncode doubles the hyphen in the fixed cleanup-backup device
// name before using it in the virtio-scsi serial. Never select by filesystem UUID:
// the original and the read-only copy deliberately have the same UUID.
const cleanupBackupMountScript = `set -eu
failed=0
refuse() { printf 'cleanup backup [%s]: %s\n' "$1" "$2" >&2; failed=1; }
disk=/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_incus_cleanup--backup
target=/mnt/subyard-cleanup-backup
udevadm settle --timeout=30 >/dev/null 2>&1 || refuse device-settle 'device discovery did not settle'
disk_id=
if test -b "$disk"; then
    if readonly=$(blockdev --getro "$disk" 2>/dev/null); then
        test "$readonly" = 1 || refuse device-readonly 'backup block device is not read-only'
    else
        refuse device-readonly 'could not read block device read-only state'
    fi
    if header=$(dumpe2fs -h "$disk" 2>/dev/null); then
        if printf '%s\n' "$header" | grep -q '^Filesystem features:.*needs_recovery'; then
            refuse filesystem 'backup filesystem requires journal recovery'
        fi
    else
        refuse filesystem 'could not read ext4 filesystem header'
    fi
    if disk_id=$(lsblk -rdn -o MAJ:MIN "$disk" 2>/dev/null) && test -n "$disk_id"; then
        :
    else
        disk_id=
        refuse device-identity 'could not read backup device identity'
    fi
else
    refuse block-device 'expected Incus backup block device is missing'
fi
if test -L "$target"; then
    refuse mountpoint 'backup mountpoint is a symlink'
elif test -e "$target" && ! test -d "$target"; then
    refuse mountpoint 'backup mountpoint is not a directory'
fi
mounted=0
if mountpoint -q "$target"; then mounted=1; fi
if test "$failed" = 0 && test "$mounted" = 0; then
    if ! mkdir -p "$target" 2>/dev/null; then
        refuse mountpoint 'could not create backup mountpoint'
    elif mount -t ext4 -o ro,noload "$disk" "$target" >/dev/null 2>&1; then
        mounted=1
    else
        refuse mount 'could not mount backup read-only without journal replay'
    fi
fi
if test "$mounted" = 1; then
    if mounted_id=$(findmnt -rn -o MAJ:MIN --mountpoint "$target" 2>/dev/null) && test -n "$mounted_id"; then
        if test -n "$disk_id" && test "$mounted_id" != "$disk_id"; then
            refuse mounted-identity 'mount does not match the backup block device'
        fi
    else
        refuse mounted-identity 'could not read mounted device identity'
    fi
    if options=$(findmnt -rn -o OPTIONS --mountpoint "$target" 2>/dev/null) && test -n "$options"; then
        case ",$options," in
            *,ro,*) ;;
            *) refuse mounted-readonly 'backup mount is not read-only' ;;
        esac
    else
        refuse mounted-readonly 'could not read backup mount options'
    fi
fi
exit "$failed"
`

func (cli *CLI) inspectCleanupBackup(ctx context.Context, loaded config.Loaded, journal *hardCleanupJournal, payload []byte, restore bool) (json.RawMessage, error) {
	if _, err := cli.cleanupGuest(ctx, loaded, "prepare inspection", ports.InstanceExecRequest{Command: []string{"sh", "-ec", "command -v python3 >/dev/null && command -v tar >/dev/null || { apt-get update >&2; DEBIAN_FRONTEND=noninteractive apt-get install -y python3 tar >&2; }"}}); err != nil {
		return nil, err
	}
	if journal.Native.ContentType == "block" {
		if _, err := cli.cleanupGuest(ctx, loaded, "mount backup", ports.InstanceExecRequest{Command: []string{"sh", "-ec", cleanupBackupMountScript}}); err != nil {
			return nil, err
		}
	}
	request, _ := json.Marshal(map[string]any{"projects": journal.Projects, "preserved": journal.Preserved})
	action := "inspect"
	if restore {
		action = "restore"
	}
	result, err := cli.cleanupGuest(ctx, loaded, action+" backup", ports.InstanceExecRequest{Command: []string{"python3", "-c", string(payload), action, "/mnt/subyard-cleanup-backup"}, Stdin: request})
	if err != nil {
		return nil, err
	}
	var witness map[string]any
	if err := json.Unmarshal(result, &witness); err != nil || witness["projects"] == nil {
		return nil, errors.New("incomplete project backup witness")
	}
	canonical, err := json.Marshal(witness)
	return canonical, err
}

func (cli *CLI) detachCleanupBackup(ctx context.Context, loaded config.Loaded, journal *hardCleanupJournal, native ports.YardBackupManager, uuid string) error {
	if journal.Native.ContentType == "block" {
		if _, err := cli.cleanupGuest(ctx, loaded, "unmount backup", ports.InstanceExecRequest{Command: []string{"sh", "-ec", "if mountpoint -q /mnt/subyard-cleanup-backup; then umount /mnt/subyard-cleanup-backup; fi"}}); err != nil {
			return err
		}
	}
	return native.DetachYardBackup(ctx, journal.Native, uuid)
}

// hardCleanupOperation preserves reporting-only values throughout recovery,
// including recreation, so an authorized rollback remains readable.
func (cli *CLI) hardCleanupOperation() *CLI {
	operation := *cli
	operation.env = maps.Clone(cli.env)
	operation.env["SUBYARD_PRESERVE_VM_REPORTING"] = "1"
	return &operation
}

// Bootstrap the base guest before restoring projects: ordinary lifecycle and
// integration inspection require its user and Docker substrate. Restore before
// preparing normal init so its project metadata and hooks bind the restored data.
func (cli *CLI) recreateCleanupProjects(ctx context.Context, loaded config.Loaded, restore func(context.Context) error, out io.Writer) error {
	stages := application.InitStages(loaded.Context)
	end := slices.IndexFunc(stages, func(stage application.ReconcileStage) bool { return stage.ID == ports.ReconcileStageMounts }) + 1
	if end == 0 {
		return errors.New("host mounts lifecycle stage is missing")
	}
	platform := cli.initPlatform(loaded, nil)
	base, ok := platform.(interface{ ProvisionBase(context.Context) error })
	if !ok {
		return errors.New("base guest provisioning is unavailable")
	}
	prefix := application.Reconciler{Stages: stages[:end], Runner: platform, Reporter: initReporter{output: out}}
	plan, err := prefix.Plan(ctx)
	if err != nil {
		return err
	}
	if err := prefix.Apply(ctx, plan); err != nil {
		return err
	}
	if err := base.ProvisionBase(ctx); err != nil {
		return err
	}
	if err := restore(ctx); err != nil {
		return err
	}
	execution, err := cli.prepareInitExecution(ctx, loaded, []string{"--yes"}, nil)
	if err != nil {
		return err
	}
	return execution.run(ctx, cli, out)
}

func (cli *CLI) applyHardCleanup(ctx context.Context, loaded config.Loaded, journal *hardCleanupJournal, path string, native ports.YardBackupManager, inspector []byte, out io.Writer) error {
	save := func(phase string) error { journal.Phase = phase; return saveHardCleanup(path, journal) }
	if journal.Phase == "backup" {
		if err := native.CheckYardBackupSource(ctx, journal.Native); err != nil {
			return err
		}
		if err := cli.cleanupPower(ctx, loaded, "stop"); err != nil {
			return err
		}
		if err := native.DetachYardBackup(ctx, journal.Native, journal.Native.OriginalUUID); err != nil {
			return err
		}
		if err := native.ReleaseYardBackup(ctx, journal.Native); err != nil {
			return err
		}
		if err := native.CopyYardBackup(ctx, journal.Native); err != nil {
			return err
		}
		// Boot on the original disk before hot-plugging its read-only duplicate.
		if err := cli.cleanupPower(ctx, loaded, "start"); err != nil {
			return err
		}
		if err := native.AttachYardBackup(ctx, journal.Native, journal.Native.OriginalUUID); err != nil {
			return err
		}
		preserved, err := cli.inspectCleanupBackup(ctx, loaded, journal, inspector, false)
		if err != nil {
			return err
		}
		journal.Preserved = preserved
		if err := cli.detachCleanupBackup(ctx, loaded, journal, native, journal.Native.OriginalUUID); err != nil {
			return err
		}
		if err := cli.cleanupPower(ctx, loaded, "stop"); err != nil {
			return err
		}
		if err := native.CheckYardBackupSource(ctx, journal.Native); err != nil {
			return err
		}
		baseline, err := cli.prepareResetTeardownBaseline(ctx, loaded)
		if err != nil {
			return err
		}
		journal.Reset = &baseline.execution.RecoveryPlan
		store, err := releasetransition.NewPOSIXV2Store(loaded.Context.Paths.ConfigHome)
		if err != nil {
			return err
		}
		release, err := store.Lock()
		if err != nil {
			return err
		}
		err = cli.captureHardRetirement(loaded, journal, store)
		release()
		if err != nil {
			return err
		}
		if err := save("reset"); err != nil {
			return err
		}
	}
	if journal.Phase == "reset" {
		if err := func() error {
			store, err := releasetransition.NewPOSIXV2Store(loaded.Context.Paths.ConfigHome)
			if err != nil {
				return err
			}
			release, err := store.Lock()
			if err != nil {
				return err
			}
			defer release()
			if err := cli.captureHardRetirement(loaded, journal, store); err != nil {
				return err
			}
			if err := native.CheckYardBackup(ctx, journal.Native); err != nil {
				return err
			}
			if journal.Reset == nil {
				return errors.New("missing exact teardown checkpoint")
			}
			execution := &teardownExecution{RecoveryPlan: *journal.Reset, Assessed: true}
			execution.selectYard(loaded)
			baseline := &resetTeardownBaseline{loaded: loaded, execution: execution}
			if err := baseline.check(ctx, cli); err != nil {
				return err
			}
			platform := cli.initPlatform(loaded, nil)
			if runtime, ok := platform.(reconcileruntime.Runtime); ok {
				runtime.TeardownResources = baseline.resources()
				runtime.TeardownArtifacts = baseline.artifacts()
				runtime.TeardownServices = execution.snapshot().Services
				platform = runtime
			}
			if err := platform.Teardown(ctx); err != nil {
				return err
			}
			if err := baseline.verify(ctx, cli); err != nil {
				return err
			}
			return save("recreate")
		}(); err != nil {
			return err
		}
	}
	if journal.Phase == "recreate" {
		if err := native.CheckYardBackup(ctx, journal.Native); err != nil {
			return err
		}
		reader, _ := cli.statusPorts()
		if current, err := reader.Instance(ctx, journal.Native.Project, journal.Native.Instance); err == nil {
			if journal.ReplacementUUID != "" && current.Config["volatile.uuid"] != journal.ReplacementUUID {
				return domain.ErrPlanStale
			}
			if current.Devices["cleanup-backup"] != nil {
				if strings.EqualFold(current.Status, "running") {
					if err := cli.detachCleanupBackup(ctx, loaded, journal, native, current.Config["volatile.uuid"]); err != nil {
						return err
					}
				} else if err := native.DetachYardBackup(ctx, journal.Native, current.Config["volatile.uuid"]); err != nil {
					return err
				}
			}
		} else if !errors.Is(err, ports.ErrInstanceNotFound) {
			return err
		}
		if err := restoreCleanupState(loaded.Context.Paths.StateDir, journal.State); err != nil {
			return err
		}
		restoreProjects := func(ctx context.Context) error {
			reader, _ := cli.statusPorts()
			instance, err := reader.Instance(ctx, journal.Native.Project, journal.Native.Instance)
			if err != nil {
				return err
			}
			uuid := instance.Config["volatile.uuid"]
			if uuid == "" || uuid == journal.Native.OriginalUUID || journal.ReplacementUUID != "" && uuid != journal.ReplacementUUID {
				return domain.ErrPlanStale
			}
			if journal.ReplacementUUID == "" {
				journal.ReplacementUUID = uuid
				if err := save("recreate"); err != nil {
					return err
				}
			}
			if err := cli.cleanupPower(ctx, loaded, "start"); err != nil {
				return err
			}
			if err := native.AttachYardBackup(ctx, journal.Native, uuid); err != nil {
				return err
			}
			restored, err := cli.inspectCleanupBackup(ctx, loaded, journal, inspector, true)
			if err != nil {
				return err
			}
			if !bytes.Equal(restored, journal.Preserved) {
				return errors.New("project restore differs from verified backup")
			}
			if err := cli.detachCleanupBackup(ctx, loaded, journal, native, uuid); err != nil {
				return err
			}
			return native.RestoreYardBinds(ctx, journal.Native, uuid)
		}
		if err := cli.recreateCleanupProjects(ctx, loaded, restoreProjects, out); err != nil {
			return err
		}
		if err := save("restored"); err != nil {
			return err
		}
	}
	if journal.Phase == "restored" {
		if err := cli.cleanupPower(ctx, loaded, "start"); err != nil {
			return err
		}
		if err := cli.hardProvisionProfiles(ctx, loaded, journal, out); err != nil {
			return err
		}
		request, _ := json.Marshal(map[string]any{"projects": journal.Projects})
		current, err := cli.cleanupGuest(ctx, loaded, "verify restored projects", ports.InstanceExecRequest{Command: []string{"python3", "-c", string(inspector), "inspect", "/srv"}, Stdin: request})
		if err != nil {
			return err
		}
		var normalized any
		if err := json.Unmarshal(current, &normalized); err != nil {
			return err
		}
		current, _ = json.Marshal(normalized)
		if !bytes.Equal(current, journal.Preserved) {
			return errors.New("recreated project contents differ; backup is retained")
		}
		entries, err := captureCleanupState(loaded.Context.Paths.StateDir)
		if err != nil {
			return err
		}
		if operationStateDigest(entries) != journal.StateBinding {
			return errors.New("project registry changed during recreation; backup is retained")
		}
		if _, err := cli.cleanupGuest(ctx, loaded, "sync restored projects", ports.InstanceExecRequest{Command: []string{"sync", "-f", "/srv/workspaces"}}); err != nil {
			return err
		}
		if journal.Native.DesiredPower == "stopped" {
			if err := cli.cleanupPower(ctx, loaded, "stop"); err != nil {
				return err
			}
		}
		if err := cli.publishHardRetirement(loaded, journal); err != nil {
			return err
		}
		if err := save("verified"); err != nil {
			return err
		}
	}
	if journal.Phase != "verified" {
		return errors.New("unknown hard cleanup checkpoint")
	}
	if err := native.ReleaseYardBackup(ctx, journal.Native); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return err
	}
	fmt.Fprintln(out, "Yard recreated; verified project data, registration and external binds restored.")
	return nil
}

func (cli *CLI) hardProvisionProfiles(ctx context.Context, loaded config.Loaded, journal *hardCleanupJournal, diagnostics io.Writer) error {
	definition := command.Definition{Handler: "@provision"}
	runner := cli.operationOrchestrator(journal.Token, loaded, nil, &definition).Runner
	contextValues := structuredCommandContext(loaded)
	contextValues["SUBYARD_DISPATCHER_PATH"] = cli.options.DispatcherPath
	for _, name := range strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]) {
		binding, exists := journal.Provision[name]
		if !exists {
			continue
		}
		if err := binding.Check(); err != nil {
			return domain.ErrPlanStale
		}
		for _, action := range []string{"profile", "profile-check"} {
			arguments := []string{name}
			if action == "profile-check" {
				arguments = []string{"--check", name}
			}
			result, output, err := runner.Run(ctx, domain.AdapterRequest{Schema: shelladapter.ProtocolSchema, OperationID: journal.Token, Adapter: "provision", Action: action, Arguments: arguments, Context: contextValues, Input: map[string]any{"profileBinding": binding}}, nil)
			if err != nil || result.Status != "ok" {
				return fmt.Errorf("recreate profile %s: %w", name, errors.Join(err, errors.New("profile provision did not complete")))
			}
			if action == "profile-check" {
				if strings.TrimSpace(output) != "converged" {
					return fmt.Errorf("profile %s did not converge after recreation", name)
				}
			} else {
				writeAdapterDiagnostics(diagnostics, output)
			}
		}
	}
	return nil
}
