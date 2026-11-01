package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

type physicalStartupInput struct {
	Repository, Yard, Result string
	Deadline                 int64
	Original                 *ownerLifecycleRecord
	Sources                  []byte
	Init                     []byte
	SelfImage                string
}

type physicalStartupObserver struct {
	ownerGraphObserver
	started, accepted, terminal []ports.NativeOperation
	unknown                     bool
	commandTerminals            map[uint64]bool
}

func (o *physicalStartupObserver) Before(v ports.NativeOperation) error {
	o.started = append(o.started, v)
	return nil
}
func (o *physicalStartupObserver) Accepted(v ports.NativeOperation) error {
	o.accepted = append(o.accepted, v)
	return nil
}
func (o *physicalStartupObserver) Terminal(v ports.NativeOperation) error {
	o.terminal = append(o.terminal, v)
	return nil
}
func (o *physicalStartupObserver) Uncertain() { o.unknown = true }
func (o *physicalStartupObserver) CommandTerminal(n uint64) error {
	if o.commandTerminals == nil {
		o.commandTerminals = map[uint64]bool{}
	}
	if n == 0 || n > o.commands || o.commandTerminals[n] {
		return errors.New("physical command terminal binding")
	}
	o.commandTerminals[n] = true
	return nil
}

// This capability belongs only to the opted-in fixture. It retains the real
// installed candidate engine; it does not stand in for Serve admission proof.
type physicalStartupSelf struct {
	file   *os.File
	digest string
}

func (image physicalStartupSelf) Borrow() (*os.File, string, error) {
	fd, err := syscall.Dup(int(image.file.Fd()))
	if err != nil {
		return nil, "", err
	}
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "fixture retained installed engine"), image.digest, nil
}
func physicalSelfImage(file *os.File) (physicalStartupSelf, error) {
	info, err := file.Stat()
	if err != nil {
		return physicalStartupSelf{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Size() > 128<<20 {
		return physicalStartupSelf{}, errors.New("physical installed engine image bound")
	}
	hasher := sha256.New()
	count, err := io.Copy(hasher, io.NewSectionReader(file, 0, (128<<20)+1))
	if err != nil {
		return physicalStartupSelf{}, err
	}
	if count > 128<<20 {
		return physicalStartupSelf{}, errors.New("physical installed engine image grew past bound")
	}
	return physicalStartupSelf{file, fmt.Sprintf("%x", hasher.Sum(nil))}, nil
}

// The owning profile fixture supplies an already provisioned, stopped dedicated
// yard and keeps its actual handler assertions. This opt-in harness proves only
// original Go native-child reconstruction/OFD/SDK custody, not Rust lineage.
func TestOwnerStartupPhysicalOriginal(t *testing.T) {
	runOwnerResourcePhysicalOriginal(t, false)
}

func runOwnerResourcePhysicalOriginal(t *testing.T, orphan bool) {
	root := os.Getenv("SUBYARD_NATIVE_STARTUP_REPOSITORY")
	if root == "" {
		t.Skip("allocated original-startup physical fixture")
	}
	input := physicalStartupInput{Repository: root, Yard: os.Getenv("SUBYARD_NATIVE_STARTUP_YARD"), Result: os.Getenv("SUBYARD_NATIVE_STARTUP_RESULT"), Deadline: time.Now().Add(15 * time.Minute).UnixNano()}
	child := os.Getenv("SUBYARD_NATIVE_STARTUP_CHILD")
	if child != "" {
		file, err := os.Open(child)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(io.LimitReader(file, reconcileruntime.MaxInstalledRecipe+1))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&input)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			t.Fatal("original physical input", err, closeErr)
		}
	}
	if input.Repository != root || input.Yard == "" || !filepath.IsAbs(input.Result) {
		t.Fatal("physical fixture scope missing")
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, input.Deadline))
	defer cancel()
	cli, err := New(Options{RepositoryRoot: root, WorkingDir: root, DispatcherPath: filepath.Join(root, "bin/yard-engine"), Environment: physicalStartupEnvironment(os.Environ()), Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := cli.loadContext(input.Yard)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Context.AccessKind != domain.AccessLocal || loaded.Context.YardKind != domain.YardVM {
		t.Fatal("physical startup requires a named local VM")
	}
	yards, err := cli.powerYardContexts(loaded)
	if err != nil {
		t.Fatal(err)
	}
	runtime, ok := cli.initPlatform(loaded, yards).(reconcileruntime.Runtime)
	if !ok {
		t.Fatal("physical runtime unavailable")
	}
	runtime.LogicalRepositoryRoot = root
	if child != "" {
		owner := os.NewFile(3, "original physical owner")
		defer owner.Close()
		release, err := uninstall.RegisterBorrowedOwnerLock(loaded.Context.Paths.DataHome, owner)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		observer := &physicalStartupObserver{}
		ctx = ports.WithNativeOwnerHome(ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(ctx, observer), nativeValueDigest(struct {
			Original     *ownerLifecycleRecord
			Source, Init string
		}{input.Original, nativeDigest(input.Sources), nativeDigest(input.Init)})), loaded.Context.Paths.DataHome)
		image := os.NewFile(4, "original installed engine")
		defer image.Close()
		capability, err := physicalSelfImage(image)
		if err != nil || capability.digest != input.SelfImage {
			t.Fatal("original installed engine custody changed", err)
		}
		ctx = ports.WithNativeSelfExecutable(ctx, capability)
		uuidMismatchRejected := false
		if orphan {
			uuidMismatchRejected, err = runPhysicalOrphan(ctx, cli, loaded, runtime, input, observer)
		} else {
			sources, sourceErr := runtime.BindInstalledRuntimeContext(ctx, input.Sources)
			if sourceErr != nil {
				t.Fatal("original startup sources", sourceErr)
			}
			defer sources.Close()
			prepared := &preparedOwnerChildren{operation: cli, inits: map[string]preparedOwnerInitChild{input.Yard: {loaded: loaded, runtime: runtime, start: input.Original, sources: sources}}}
			err = prepared.runStart(ctx, input.Yard, os.Stderr)
			if err == nil {
				incus, _ := cli.statusPorts()
				var instance ports.InstanceInfo
				instance, err = incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
				if err == nil && instance.Config["volatile.uuid"] != input.Original.Observed.Instance.Config.restore()["volatile.uuid"] {
					err = domain.ErrPlanStale
				}
				var startup *resourceStartup
				if err == nil {
					startup, err = restoreOwnerResourceStartup(input.Original.Startup, cli, loaded, root)
				}
				var owned *resourceOwnedIngress
				if err == nil {
					owned, err = startup.originalIngress()
				}
				if err == nil && (!maps.Equal(instance.LocalDevices[startup.definition.Proxy.Device], owned.desired) || instance.LocalConfig[startup.definition.Proxy.OwnershipKey()] != owned.desiredMarker || instance.Config[startupIntentKey(startup.definition)] != startup.intent.after) {
					err = errors.New("original physical resource postcondition changed")
				}
				if err == nil {
					// The live SDK must refuse an otherwise exact route closure with a
					// substituted UUID before any mutation reservation or PUT.
					wrong := *owned
					wrong.uuid += "-substituted"
					wrong.published = true
					started := len(observer.started)
					negative := wrong.write(ctx, &resourceApplyRunner{cli: cli, ingress: startup.ingress}, true)
					uuidMismatchRejected = errors.Is(negative, domain.ErrPlanStale) && len(observer.started) == started
					if !uuidMismatchRejected {
						err = fmt.Errorf("physical original UUID guard did not refuse: %w", negative)
					}
				}
			}
		}
		result := struct {
			Commands                    uint64 `json:"commands"`
			CommandTerminals            int    `json:"commandTerminals"`
			Started, Accepted, Terminal []ports.NativeOperation
			Unknown                     bool   `json:"unknown"`
			Success                     bool   `json:"success"`
			UUIDMismatchRejected        bool   `json:"uuidMismatchRejected"`
			SelfImage                   string `json:"selfImage"`
			SelfProof                   string `json:"selfProof"`
			RustLineage                 bool   `json:"rustLineage"`
		}{observer.commands, len(observer.commandTerminals), observer.started, observer.accepted, observer.terminal, observer.unknown, err == nil, uuidMismatchRejected, capability.digest, "trusted-fixture-installed-engine", false}
		if err := writePhysicalStartupReceipt(input.Result, result); err != nil {
			t.Fatal("physical custody receipt", err)
		}
		if err != nil {
			t.Fatal("original physical start", err)
		}
		minimum := uint64(2)
		if orphan {
			minimum = 1
		}
		if observer.unknown || observer.commands < minimum || len(observer.commandTerminals) != int(observer.commands) || len(observer.accepted) == 0 || len(observer.accepted) != len(observer.terminal) {
			t.Fatal("original physical custody incomplete")
		}
		return
	}
	image, err := os.Open(filepath.Join(root, "bin/yard-engine"))
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	capability, err := physicalSelfImage(image)
	if err != nil {
		t.Fatal(err)
	}
	input.SelfImage = capability.digest
	if orphan {
		plan, prepareErr := cli.prepareOrphanIngress(ctx, loaded)
		if prepareErr != nil || plan == nil || len(plan.entries) == 0 {
			t.Fatal("original physical orphan required", prepareErr)
		}
		execution := &initExecution{loaded: loaded, mode: initReconcile, platform: runtime, orphanIngress: plan}
		input.Init, err = captureOwnerInitRecord(execution, cli)
		if err != nil {
			t.Fatal("original physical init resource", err)
		}
	} else {
		input.Original, err = captureOwnerStart(ctx, cli, loaded, root)
		if err != nil || input.Original == nil || len(input.Original.Startup) == 0 {
			t.Fatal("original pending physical startup required", err)
		}
	}
	sources, err := runtime.PrepareOwnerRuntimeSources(ctx)
	if err != nil {
		t.Fatal("original physical sources", err)
	}
	input.Sources = sources.Recipe()
	if err := sources.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(testkit.TempDir(t), "original-startup.json")
	testkit.WriteFile(t, path, body, 0600)
	_, release, err := uninstall.OwnerLock(ctx, loaded.Context.Paths.DataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	owner, err := uninstall.BorrowOwnerLock(loaded.Context.Paths.DataHome)
	if err != nil || owner == nil {
		t.Fatal("original physical owner descriptor", err)
	}
	defer owner.Close()
	testName := "TestOwnerStartupPhysicalOriginal"
	if orphan {
		testName = "TestOwnerOrphanPhysicalOriginal"
	}
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+testName+"$", "-test.v")
	command.Env = append(physicalStartupEnvironment(os.Environ()), "SUBYARD_NATIVE_STARTUP_CHILD="+path)
	command.ExtraFiles = []*os.File{owner, image}
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		t.Fatal("fresh physical startup helper", err)
	}
}

// physicalStartupEnvironment consumes only this fixture's transport marker.
// The product source guard continues to bind every other environment entry.
func physicalStartupEnvironment(environment []string) []string {
	original := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, "SUBYARD_NATIVE_STARTUP_CHILD=") {
			original = append(original, entry)
		}
	}
	return original
}

func TestOwnerStartupPhysicalTransportPreservesSourceEnvironment(t *testing.T) {
	original := []string{"PATH=/usr/bin:/bin", "SUBYARD_HOME=/fixture", "EXTERNAL=original"}
	parent := reconcileruntime.Runtime{Environment: original}
	inherited := append(append([]string(nil), original...), "SUBYARD_NATIVE_STARTUP_CHILD=/fixture/input")
	child := reconcileruntime.Runtime{Environment: inherited}
	if parent.OwnerEnvironmentBinding() == child.OwnerEnvironmentBinding() {
		t.Fatal("transport marker did not exercise source drift guard")
	}
	child.Environment = physicalStartupEnvironment(inherited)
	if parent.OwnerEnvironmentBinding() != child.OwnerEnvironmentBinding() {
		t.Fatal("helper changed original source environment")
	}
	child.Environment = physicalStartupEnvironment(append(inherited, "EXTERNAL=changed"))
	if parent.OwnerEnvironmentBinding() == child.OwnerEnvironmentBinding() {
		t.Fatal("fixture filtering weakened product environment guard")
	}
}

func writePhysicalStartupReceipt(path string, result any) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(body)
	}
	return errors.Join(err, file.Close())
}

// The owning fixture perturbs only kernel CPUWeight for its already-owned VM.
// This proves the real installed CLI component boundary with an explicit trusted
// fixture capability. Actual Serve admission is proved separately; no Rust or
// whole registration lineage is claimed here.
func TestOwnerVMCPUPhysicalOriginal(t *testing.T) {
	root := os.Getenv("SUBYARD_NATIVE_STARTUP_REPOSITORY")
	if root == "" {
		t.Skip("allocated original CPU physical fixture")
	}
	yardName, resultPath := os.Getenv("SUBYARD_NATIVE_STARTUP_YARD"), os.Getenv("SUBYARD_NATIVE_STARTUP_RESULT")
	if yardName == "" || !filepath.IsAbs(resultPath) {
		t.Fatal("physical CPU fixture scope missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cli, err := New(Options{RepositoryRoot: root, WorkingDir: root, DispatcherPath: filepath.Join(root, "bin/yard-engine"), Environment: physicalStartupEnvironment(os.Environ()), Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := cli.loadContext(yardName)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Context.AccessKind != domain.AccessLocal || loaded.Context.YardKind != domain.YardVM {
		t.Fatal("physical CPU requires named local VM")
	}
	image, err := os.Open(filepath.Join(root, "bin/yard-engine"))
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	capability, err := physicalSelfImage(image)
	if err != nil {
		t.Fatal(err)
	}
	incus, _ := cli.statusPorts()
	scheduler, ok := incus.(interface {
		VMCPUConvergedIf(context.Context, string, string, bool, string) (bool, error)
	})
	if !ok {
		t.Fatal("guarded VM CPU scheduler unavailable")
	}
	before, err := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if err != nil {
		t.Fatal(err)
	}
	uuid := before.Config["volatile.uuid"]
	if uuid == "" || before.Config["user.subyard.vm_cpu_weight"] == "" || !strings.EqualFold(before.Status, "running") {
		t.Fatal("original physical CPU identity/weight missing")
	}
	readyBefore, err := scheduler.VMCPUConvergedIf(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName, false, uuid)
	if err != nil || readyBefore {
		t.Fatal("physical CPU fixture did not establish kernel-only scheduling drift", readyBefore, err)
	}
	_, release, err := uninstall.OwnerLock(ctx, loaded.Context.Paths.DataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	observer := &physicalStartupObserver{}
	ctx = ports.WithNativeSelfExecutable(ports.WithNativeOwnerHome(ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(ctx, observer), nativeValueDigest(struct{ UUID, Image string }{uuid, capability.digest})), loaded.Context.Paths.DataHome), capability)
	readyAfter, err := scheduler.VMCPUConvergedIf(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName, true, uuid)
	if err != nil {
		t.Fatal("actual installed native CPU component", err)
	}
	after, err := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if err != nil {
		t.Fatal(err)
	}
	if !readyAfter || !strings.EqualFold(after.Status, "running") || after.Config["volatile.uuid"] != uuid || after.Config["user.subyard.vm_cpu_weight"] != before.Config["user.subyard.vm_cpu_weight"] || observer.commands != 1 || len(observer.commandTerminals) != 1 || observer.unknown {
		t.Fatal("physical CPU original metadata/custody changed")
	}
	result := struct {
		Success          bool   `json:"success"`
		ReadyBefore      bool   `json:"readyBefore"`
		ReadyAfter       bool   `json:"readyAfter"`
		Commands         uint64 `json:"commands"`
		CommandTerminals int    `json:"commandTerminals"`
		Unknown          bool   `json:"unknown"`
		SelfImage        string `json:"selfImage"`
		SelfProof        string `json:"selfProof"`
		RustLineage      bool   `json:"rustLineage"`
	}{true, readyBefore, readyAfter, observer.commands, len(observer.commandTerminals), observer.unknown, capability.digest, "trusted-fixture-installed-engine", false}
	if err := writePhysicalStartupReceipt(resultPath, result); err != nil {
		t.Fatal(err)
	}
}
