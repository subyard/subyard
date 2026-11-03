package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// The fixed serial follows Incus's PathNameEncode("cleanup-backup"), which
// doubles hyphens before the virtio-scsi driver prefixes the serial with incus_.
// Every system effect is intercepted; the shell only exercises the real probe.
const cleanupBlockProbeFixture = `scenario=$1
test() {
    case "$1" in
        -b) command test "$2" = /dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_incus_cleanup--backup && command test "$scenario" != missing ;;
        -L) command test "$scenario" = unsafe || command test "$scenario" = missing ;;
        -e|-d) return 0 ;;
        *) command test "$@" ;;
    esac
}
udevadm() { test "$scenario" != unsafe; }
blockdev() { if test "$scenario" = unsafe; then printf '0\n'; else printf '1\n'; fi; }
dumpe2fs() {
    if test "$scenario" = read-failures; then return 1; fi
    if test "$scenario" = unsafe; then printf 'Filesystem features: needs_recovery extents\n'; else printf 'Filesystem features: extents\n'; fi
}
lsblk() {
    test "$scenario" != read-failures || return 1
    test "$*" = "-rdn -o MAJ:MIN $disk" || return 1
    printf '8:16\n'
}
mountpoint() { case "$scenario" in existing|wrong-mount|read-failures|mount-read-failures) return 0 ;; *) return 1 ;; esac; }
mkdir() { return 0; }
mount() {
    test "$scenario" != mount-failure || return 1
    test "$*" = "-t ext4 -o ro,noload $disk /mnt/subyard-cleanup-backup" || return 1
    printf 'mount called\n' >&3
}
findmnt() {
    test "$scenario" != mount-read-failures || return 1
    case "$*" in
        '-rn -o MAJ:MIN --mountpoint /mnt/subyard-cleanup-backup')
            if test "$scenario" = wrong-mount; then printf '8:32\n'; else printf '8:16\n'; fi ;;
        '-rn -o OPTIONS --mountpoint /mnt/subyard-cleanup-backup')
            case "$scenario" in wrong-mount|read-failures) printf 'rw,relatime\n' ;; *) printf 'ro,relatime,noload\n' ;; esac ;;
        *) return 1 ;;
    esac
}
exec 3>&1
`

func TestCleanupBlockBackupProbe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phases []string
		mount  bool
	}{
		{name: "clean", mount: true},
		{name: "existing"},
		{name: "unsafe", phases: []string{"device-settle", "device-readonly", "filesystem", "mountpoint"}},
		{name: "missing", phases: []string{"block-device", "mountpoint"}},
		{name: "read-failures", phases: []string{"filesystem", "device-identity", "mounted-readonly"}},
		{name: "wrong-mount", phases: []string{"mounted-identity", "mounted-readonly"}},
		{name: "mount-read-failures", phases: []string{"mounted-identity", "mounted-readonly"}},
		{name: "mount-failure", phases: []string{"mount"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-ec", cleanupBlockProbeFixture+cleanupBackupMountScript, "cleanup-probe", tc.name)
			cmd.Dir = testkit.TempDir(t)
			output, err := cmd.CombinedOutput()
			if (err != nil) != (len(tc.phases) != 0) {
				t.Fatalf("probe result: %v\n%s", err, output)
			}
			if strings.Contains(string(output), "mount called") != tc.mount {
				t.Fatalf("unexpected mount effect: %s", output)
			}
			var phases []string
			for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
				if strings.HasPrefix(line, "cleanup backup [") {
					phases = append(phases, strings.SplitN(strings.TrimPrefix(line, "cleanup backup ["), "]:", 2)[0])
				}
			}
			if strings.Join(phases, ",") != strings.Join(tc.phases, ",") {
				t.Fatalf("refusal order = %v, want %v\n%s", phases, tc.phases, output)
			}
		})
	}
}

type cleanupExecResultFixture struct {
	ports.YardExecutor
	result ports.InstanceExecResult
	err    error
}

func (f cleanupExecResultFixture) Execute(context.Context, domain.Context, ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	return f.result, f.err
}

func TestCleanupGuestFailureIdentifiesPhaseAndExit(t *testing.T) {
	for _, stderr := range []string{"", "cleanup backup [block-device]: expected Incus backup block device is missing\n"} {
		program := &CLI{options: Options{ProjectData: cleanupExecResultFixture{result: ports.InstanceExecResult{ExitCode: 1, Stderr: []byte(stderr)}}}}
		_, err := program.cleanupGuest(context.Background(), config.Loaded{}, "mount backup", ports.InstanceExecRequest{})
		if err == nil || !strings.Contains(err.Error(), "cleanup guest mount backup:") || !strings.Contains(err.Error(), "status 1") || !strings.Contains(err.Error(), strings.TrimSpace(stderr)) {
			t.Fatalf("guest failure lost context: %v", err)
		}
	}
	transport := errors.New("transport unavailable")
	program := &CLI{options: Options{ProjectData: cleanupExecResultFixture{err: transport}}}
	if _, err := program.cleanupGuest(context.Background(), config.Loaded{}, "mount backup", ports.InstanceExecRequest{}); !errors.Is(err, transport) {
		t.Fatalf("guest failure lost transport cause: %v", err)
	}
}

type cleanupDataFixture struct{ ports.YardExecutor }

func (f *cleanupDataFixture) Execute(context.Context, domain.Context, ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	return ports.InstanceExecResult{}, nil
}

func TestCleanupRegistryRoundTripRetainsReservationsAndModes(t *testing.T) {
	root := testkit.TempDir(t)
	source := filepath.Join(root, "registry")
	destination := filepath.Join(root, "restored")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "reservations"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(source, "project.json"), []byte("exact registration bytes\n"), 0600)
	testkit.WriteFile(t, filepath.Join(source, "reservations", "operation.json"), []byte("pending reservation\n"), 0400)
	original, err := captureCleanupState(source)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := restoreCleanupState(destination, original); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := captureCleanupState(destination)
	if err != nil || operationStateDigest(actual) != operationStateDigest(original) {
		t.Fatal("registry backup differs", err)
	}
	if err := os.Symlink(source, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCleanupState(filepath.Join(root, "linked")); err == nil {
		t.Fatal("linked registry accepted")
	}
}

func TestCleanupCheckpointIsPrivateAndYardScoped(t *testing.T) {
	root := testkit.TempDir(t)
	yard := domain.Context{IncusProject: "one", YardInstanceName: "yard", Paths: domain.RuntimePaths{DataHome: root}}
	journal := &hardCleanupJournal{Schema: 1, Token: "0123456789abcdef0123456789abcdef", Phase: "recreate"}
	path := hardCleanupPath(yard)
	if err := saveHardCleanup(path, journal); err != nil {
		t.Fatal(err)
	}
	if err := pendingHardCleanup(yard); err == nil {
		t.Fatal("selected pending cleanup ignored")
	}
	neighbor := yard
	neighbor.IncusProject = "two"
	if err := pendingHardCleanup(neighbor); err != nil {
		t.Fatal("neighbor blocked", err)
	}
	recovered, err := readHardCleanup(path)
	if err != nil || recovered.Phase != "recreate" {
		t.Fatal(recovered, err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readHardCleanup(path); err == nil {
		t.Fatal("unsafe checkpoint accepted")
	}
}

type cleanupBasePlatformFixture struct {
	*initPlatformFixture
	baseReady bool
	baseErr   error
}

func (platform *cleanupBasePlatformFixture) ProvisionBase(context.Context) error {
	if platform.baseErr != nil {
		return platform.baseErr
	}
	platform.baseReady = true
	return nil
}

func TestCleanupBootstrapsBeforeRestoreAndPlansHooksAfterRetry(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	platform := &cleanupBasePlatformFixture{initPlatformFixture: &initPlatformFixture{converged: map[ports.ReconcileStageID]bool{ports.ReconcileStageIncus: true}}}
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, InitPlatform: platform, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		called := false
		restore := func(context.Context) error {
			called = true
			if !platform.converged[ports.ReconcileStageInstance] || !platform.converged[ports.ReconcileStageMounts] || !platform.baseReady || platform.converged[ports.ReconcileStageProvision] || platform.projectHooks != 0 {
				t.Fatal("project restore needs the base guest before guarded power or normal init planning")
			}
			if attempt == 0 {
				return errors.New("restore interrupted")
			}
			return nil
		}
		err = program.recreateCleanupProjects(context.Background(), loaded, restore, io.Discard)
		if attempt == 0 && (err == nil || platform.projectHooks != 0 || !platform.baseReady || platform.converged[ports.ReconcileStageProvision]) {
			t.Fatal("failed restore must retain the base guest without reaching project hooks", err)
		}
		if attempt == 1 && err != nil {
			t.Fatal("fresh init retry", err)
		}
		if !called {
			t.Fatal("restore callback skipped")
		}
	}
	if platform.projectHooks != 1 {
		t.Fatal("ordinary hooks did not complete")
	}
}

func TestCleanupBaseProvisionFailureDefersRestoreAndCanRetry(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	failed := errors.New("base provisioning interrupted")
	platform := &cleanupBasePlatformFixture{initPlatformFixture: newInitPlatformFixture(), baseErr: failed}
	platform.converged[ports.ReconcileStageProject] = true
	platform.converged[ports.ReconcileStageProvision] = false
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, InitPlatform: platform, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func(context.Context) error { restored = true; return nil }
	if err := program.recreateCleanupProjects(context.Background(), loaded, restore, io.Discard); !errors.Is(err, failed) || restored || platform.projectHooks != 0 {
		t.Fatalf("base failure reached project restoration: restored=%v hooks=%d err=%v", restored, platform.projectHooks, err)
	}
	platform.baseErr = nil
	if err := program.recreateCleanupProjects(context.Background(), loaded, restore, io.Discard); err != nil || !restored || platform.projectHooks != 1 {
		t.Fatalf("base retry did not finish: restored=%v hooks=%d err=%v", restored, platform.projectHooks, err)
	}
}

type cleanupFailureNative struct {
	*testkit.Incus
	failure string
	backup  bool
}

func (f *cleanupFailureNative) PrepareYardBackup(context.Context, string, string, string, string, string, map[string]string) (ports.YardCleanupBackup, error) {
	return ports.YardCleanupBackup{}, nil
}
func (f *cleanupFailureNative) CheckYardBackupSource(context.Context, ports.YardCleanupBackup) error {
	return nil
}
func (f *cleanupFailureNative) CopyYardBackup(context.Context, ports.YardCleanupBackup) error {
	if f.failure == "copy" {
		return errors.New("copy failed")
	}
	f.backup = true
	return nil
}
func (f *cleanupFailureNative) CheckYardBackup(context.Context, ports.YardCleanupBackup) error {
	if !f.backup {
		return errors.New("backup absent")
	}
	return nil
}
func (f *cleanupFailureNative) AttachYardBackup(context.Context, ports.YardCleanupBackup, string) error {
	if f.failure == "attach" {
		return errors.New("attach failed")
	}
	return nil
}
func (f *cleanupFailureNative) DetachYardBackup(context.Context, ports.YardCleanupBackup, string) error {
	return nil
}
func (f *cleanupFailureNative) ReleaseYardBackup(context.Context, ports.YardCleanupBackup) error {
	f.backup = false
	return nil
}
func (f *cleanupFailureNative) RestoreYardBinds(context.Context, ports.YardCleanupBackup, string) error {
	return nil
}

type cleanupFailureData struct{ ports.YardExecutor }

func (cleanupFailureData) Execute(context.Context, domain.Context, ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	return ports.InstanceExecResult{ExitCode: 1, Stderr: []byte("external Git alternates")}, nil
}
func TestCleanupPredeleteFailuresKeepOriginalAndCheckpoint(t *testing.T) {
	for _, failure := range []string{"copy", "attach", "inspect"} {
		t.Run(failure, func(t *testing.T) {
			root := testkit.TempDir(t)
			yard := domain.Context{IncusProject: "selected", YardInstanceName: "yard", Paths: domain.RuntimePaths{DataHome: root}}
			native := &cleanupFailureNative{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"selected/yard": {Name: "yard", Project: "selected", Status: "Running", Config: map[string]string{"volatile.uuid": "original"}}}}, failure: failure}
			platform := &initPlatformFixture{converged: map[ports.ReconcileStageID]bool{}}
			program := &CLI{options: Options{Incus: native, InitPlatform: platform, ProjectData: cleanupFailureData{}}}
			journal := &hardCleanupJournal{Schema: 1, Token: "0123456789abcdef0123456789abcdef", Phase: "backup", Native: ports.YardCleanupBackup{Project: "selected", Instance: "yard", OriginalUUID: "original"}}
			path := hardCleanupPath(yard)
			if err := saveHardCleanup(path, journal); err != nil {
				t.Fatal(err)
			}
			err := program.applyHardCleanup(context.Background(), config.Loaded{Context: yard}, journal, path, native, []byte("inspector"), io.Discard)
			if err == nil || platform.teardowns != 0 || native.Instances["selected/yard"].Config["volatile.uuid"] != "original" {
				t.Fatal("predelete failure lost original", err)
			}
			saved, err := readHardCleanup(path)
			if err != nil || saved.Phase != "backup" {
				t.Fatal("recovery checkpoint lost", err)
			}
			if failure != "copy" && !native.backup {
				t.Fatal("independent completed backup lost")
			}
		})
	}
}
