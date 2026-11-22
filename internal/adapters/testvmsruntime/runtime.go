package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/safefs"
)

const provisionedGuestCount = 2

// Only the broker configures low-level VM settings. Incus disables q35 S3 by
// default; override that fixed ICH9 setting so x86_64 guests can suspend to RAM.
// Other architectures retain Incus's default low-level restriction.
func vmSleepPolicy(architecture string) (lowlevel, rawQEMU string) {
	if architecture == "x86_64" {
		return "allow", "[global][0]\ndriver = \"ICH9-LPC\"\nproperty = \"disable_s3\"\nvalue = \"0\"\n"
	}
	return "", ""
}

type Runtime struct {
	slotAccountRoot   string
	lookupUser        func(string) (*user.User, error)
	allocation        *LeaseIdentity
	agentLogOwner     string
	memoryProbe       func() (MemoryCapacity, error)
	brokerMemoryProbe func() (BrokerMemoryCapacity, error)
	usageProbe        func(context.Context, LeaseSlot) allocationUsage
	cacheProbe        func(context.Context) (CacheUsage, error)
	diskUsageProbe    func(context.Context) (uint64, error)
	Config            Config
	ConfigPath        string
	Runner            CommandRunner
	Stdout            io.Writer
	Stderr            io.Writer
	Now               func() time.Time
	Sleep             func(context.Context, time.Duration) error
	AvailableBytes    func(string) (uint64, error)
	ExecutablePath    string
	Events            *EventRecorder
	recoverySlot      *LeaseSlot
	finishDrain       func(context.Context, LeaseStore, LeaseSlot) error
	finishQuarantine  func(context.Context, LeaseStore, LeaseSlot, error) error
	finishRecovery    func(context.Context, LeaseStore, LeaseSlot) error
}

func LoadRuntime(path string, stdout, stderr io.Writer) (*Runtime, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return &Runtime{Config: cfg, ConfigPath: path, Stdout: stdout, Stderr: stderr}, nil
}

func (runtime *Runtime) eventRecorder() EventRecorder {
	if runtime.Events != nil {
		return *runtime.Events
	}
	return EventRecorder{
		StateDir: runtime.Config.StateDir,
		Source:   runtime.Config.BrokerSource,
		Now:      runtime.Now,
	}
}

func (runtime *Runtime) Run(ctx context.Context, arguments []string, environment map[string]string) error {
	if runtime.Runner == nil {
		runtime.Runner = ProcessRunner{}
	}
	if runtime.Stdout == nil {
		runtime.Stdout = io.Discard
	}
	if runtime.Stderr == nil {
		runtime.Stderr = io.Discard
	}
	if runtime.Now == nil {
		runtime.Now = time.Now
	}
	if runtime.Sleep == nil {
		runtime.Sleep = sleepContext
	}
	if runtime.Events == nil {
		recorder := EventRecorder{
			StateDir: runtime.Config.StateDir,
			Source:   runtime.Config.BrokerSource,
			Now:      runtime.Now,
		}
		runtime.Events = &recorder
	}
	yes := false
	image := ""
	var expectedGeneration uint64
	var expectedEpoch uint64
	expectedGenerationSet := false
	expectedEpochSet := false
	var positional []string
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch argument {
		case "--image":
			index++
			if index >= len(arguments) || arguments[index] == "" || image != "" {
				return errors.New("--image requires one image")
			}
			image = arguments[index]
			if err := ValidateImage(image); err != nil {
				return err
			}
		case "-y", "--yes":
			yes = true
		case "--expect-resource-generation", "--expect-lease-epoch":
			index++
			if index >= len(arguments) {
				return fmt.Errorf("%s requires a positive integer", argument)
			}
			value, err := strconv.ParseUint(arguments[index], 10, 64)
			if err != nil || (value == 0 && argument != "--expect-lease-epoch") {
				return fmt.Errorf("%s requires an unsigned integer (generation must be positive)", argument)
			}
			if argument == "--expect-resource-generation" {
				if expectedGenerationSet {
					return errors.New("expected resource generation was provided more than once")
				}
				expectedGeneration = value
				expectedGenerationSet = true
			} else {
				if expectedEpochSet {
					return errors.New("expected lease epoch was provided more than once")
				}
				expectedEpoch = value
				expectedEpochSet = true
			}
		default:
			if strings.HasPrefix(argument, "-") {
				return fmt.Errorf("unknown test-vms worker option %q", argument)
			}
			positional = append(positional, argument)
		}
	}
	if len(positional) != 1 {
		return errors.New("test-vms worker requires exactly one command")
	}
	action := positional[0]
	if image != "" && !strings.HasPrefix(action, "refresh-") {
		return errors.New("--image is valid only for refresh")
	}
	expectsLeaseIdentity := strings.HasPrefix(action, "revoke-slot-") ||
		strings.HasPrefix(action, "recover-slot-") || strings.HasPrefix(action, "retire-legacy-slot-")
	if expectsLeaseIdentity && (!expectedGenerationSet || !expectedEpochSet) {
		return errors.New("complete lease target identity is required")
	}
	if !expectsLeaseIdentity && (expectedGenerationSet || expectedEpochSet) {
		return errors.New("lease target identity is only valid for revoke or recover")
	}
	if action == "doctor" {
		return runtime.doctor(ctx, environment)
	}
	if err := runtime.Config.Validate(); err != nil {
		return err
	}
	if action == "spool-export" {
		batch, err := runtime.eventRecorder().Export()
		if err != nil {
			return err
		}
		return json.NewEncoder(runtime.Stdout).Encode(batch)
	}
	if action == "spool-ack" {
		if !yes {
			return errors.New("confirmation required (re-run with --yes for automation)")
		}
		var acknowledgement struct {
			EventIDs    []string `json:"event_ids"`
			IncidentIDs []string `json:"incident_ids"`
		}
		payload := environment["SUBYARD_SPOOL_ACK"]
		if len(payload) > 64<<10 {
			return errors.New("broker spool acknowledgement is too large")
		}
		if err := json.Unmarshal([]byte(payload), &acknowledgement); err != nil {
			return errors.New("invalid broker spool acknowledgement")
		}
		return runtime.eventRecorder().Ack(
			acknowledgement.EventIDs,
			acknowledgement.IncidentIDs,
		)
	}
	if !runtime.Config.Enabled {
		return errors.New("nested E2E VMs are disabled; enable test-vms and run yard init")
	}
	if _, err := runtime.Runner.LookPath(runtime.Config.Incus); err != nil {
		return errors.New("inner Incus is not installed")
	}
	if strings.HasPrefix(action, "fence-pool-expected-") {
		return (LeaseStore{Path: runtime.Config.leaseState(), SlotCount: runtime.Config.SlotCount, Now: runtime.Now}).FenceExpectedPool(strings.TrimPrefix(action, "fence-pool-expected-"))
	}
	if action == "resume-admission" {
		return (LeaseStore{Path: runtime.Config.leaseState(), SlotCount: runtime.Config.SlotCount, Now: runtime.Now}).OpenAdmission()
	}
	if strings.HasPrefix(action, "refresh-") {
		if !yes {
			return errors.New("confirmation required (re-run with --yes for automation)")
		}
		return runtime.RefreshBase(ctx, LeaseStore{Path: runtime.Config.leaseState(), SlotCount: runtime.Config.SlotCount, Now: runtime.Now}, strings.TrimPrefix(action, "refresh-"), image)
	}
	if strings.HasPrefix(action, "retire-legacy-slot-") {
		if !yes {
			return errors.New("confirmation required (re-run with --yes for automation)")
		}
		number, err := strconv.Atoi(strings.TrimPrefix(action, "retire-legacy-slot-"))
		if err != nil || number < 1 || number > runtime.Config.SlotCount {
			return errors.New("invalid legacy retirement slot")
		}
		return runtime.RetireLegacySlot(ctx, LeaseStore{Path: runtime.Config.leaseState(), SlotCount: runtime.Config.SlotCount, Now: runtime.Now},
			LeaseIdentity{SlotID: fmt.Sprintf("slot-%03d", number), ResourceGeneration: expectedGeneration, LeaseEpoch: expectedEpoch})
	}
	if strings.HasPrefix(action, "revoke-slot-") {
		if !yes {
			return errors.New("confirmation required (re-run with --yes for automation)")
		}
		number, err := strconv.Atoi(strings.TrimPrefix(action, "revoke-slot-"))
		if err != nil || number < 1 || number > runtime.Config.SlotCount {
			return errors.New("invalid revoke slot")
		}
		expected, err := expectedLeaseIdentity(
			fmt.Sprintf("slot-%03d", number),
			expectedGeneration, expectedGenerationSet,
			expectedEpoch, expectedEpochSet,
		)
		if err != nil {
			return err
		}
		return runtime.RevokeExpectedSlot(ctx, LeaseStore{
			Path: runtime.Config.leaseState(), SlotCount: runtime.Config.SlotCount, Now: runtime.Now,
		}, expected)
	}
	if strings.HasPrefix(action, "recover-slot-") {
		if !yes {
			return errors.New("confirmation required (re-run with --yes for automation)")
		}
		number, err := strconv.Atoi(strings.TrimPrefix(action, "recover-slot-"))
		if err != nil || number < 1 || number > runtime.Config.SlotCount {
			return errors.New("invalid recovery slot")
		}
		expected, err := expectedLeaseIdentity(
			fmt.Sprintf("slot-%03d", number),
			expectedGeneration, expectedGenerationSet,
			expectedEpoch, expectedEpochSet,
		)
		if err != nil {
			return err
		}
		return runtime.RecoverExpectedSlot(ctx, LeaseStore{
			Path: runtime.Config.leaseState(), SlotCount: runtime.Config.SlotCount, Now: runtime.Now,
		}, expected)
	}
	if strings.HasPrefix(action, "recover-corrupt-slot-") {
		if !yes {
			return errors.New("confirmation required (re-run with --yes for automation)")
		}
		if expectedGenerationSet || expectedEpochSet {
			return errors.New("corrupt-state recovery does not accept a lease target identity")
		}
		number, err := strconv.Atoi(strings.TrimPrefix(action, "recover-corrupt-slot-"))
		if err != nil || number < 1 || number > runtime.Config.SlotCount {
			return errors.New("invalid corrupt-state recovery slot")
		}
		return runtime.RecoverSlot(ctx, LeaseStore{
			Path: runtime.Config.leaseState(), SlotCount: runtime.Config.SlotCount, Now: runtime.Now,
		}, fmt.Sprintf("slot-%03d", number))
	}
	switch action {
	case "reconcile-pool":
		if !yes {
			return errors.New("confirmation required (re-run with --yes for automation)")
		}
		return runtime.ReconcilePool(ctx, LeaseStore{
			Path: runtime.Config.leaseState(), SlotCount: runtime.Config.SlotCount, Now: runtime.Now,
		})
	case "status":
		return (Facade{
			OnStatus: func(pool LeasePool) ResourceStatus { return runtime.ResourceStatus(ctx, pool) },
			Store: LeaseStore{
				Path: runtime.Config.leaseState(), SlotCount: runtime.Config.SlotCount, Now: runtime.Now,
			},
			Output: runtime.Stdout,
		}).Run("status")
	case "gc":
		return runtime.runGC(ctx)
	case "broker-start":
		if _, err := runtime.eventRecorder().Record(BrokerEvent{Kind: "broker.start"}); err != nil {
			return err
		}
		return (LeaseStore{Path: runtime.Config.leaseState(), SlotCount: runtime.Config.SlotCount, Now: runtime.Now}).OpenAdmission()
	case "drain-all":
		return runtime.DrainAll(ctx, LeaseStore{
			Path: runtime.Config.leaseState(), SlotCount: runtime.Config.SlotCount, Now: runtime.Now,
		}, "outer yard stopping")
	default:
		return fmt.Errorf("unknown test-vms worker command %q", action)
	}
}

func expectedLeaseIdentity(
	slotID string,
	resourceGeneration uint64,
	resourceGenerationSet bool,
	leaseEpoch uint64,
	leaseEpochSet bool,
) (LeaseIdentity, error) {
	if !resourceGenerationSet || !leaseEpochSet {
		return LeaseIdentity{}, errors.New("complete lease target identity is required")
	}
	return LeaseIdentity{
		SlotID:             slotID,
		ResourceGeneration: resourceGeneration,
		LeaseEpoch:         leaseEpoch,
	}, nil
}

func (runtime *Runtime) runGC(ctx context.Context) error {
	defer func() {
		if err := CleanupBrokerMemoryInputs(); err != nil {
			fmt.Fprintln(runtime.Stderr, "test-vms: native worker handoff cleanup unavailable")
		}
	}()
	if _, err := runtime.eventRecorder().Record(BrokerEvent{Kind: "reaper.start"}); err != nil {
		return fmt.Errorf("persist reaper start: %w", err)
	}
	err := runtime.gc(ctx)
	if err == nil {
		return nil
	}
	_, eventErr := runtime.eventRecorder().Record(BrokerEvent{
		Kind:  "reaper.fatal",
		Error: err.Error(),
	})
	if eventErr != nil {
		eventErr = fmt.Errorf("persist reaper fatal error: %w", eventErr)
	}
	return errors.Join(err, eventErr)
}

func (runtime *Runtime) provisionPair(ctx context.Context) (err error) {
	cfg := runtime.Config
	fmt.Fprintf(runtime.Stdout,
		"Create or start the lease environment with SSH and passwordless sudo.\n")
	if err = runtime.restrictAgentAccess("provisioning"); err != nil {
		return err
	}
	if err = runtime.ensureKey(ctx); err != nil {
		return err
	}
	_ = os.Remove(cfg.failureLog())
	defer func() {
		if err == nil {
			return
		}
		_ = runtime.restrictAgentAccess("allocation-failed")
		_ = runtime.collectFailureDiagnostics(ctx, err)
		fmt.Fprintln(runtime.Stderr,
			"test-vms: failed slot provisioning is handled by guarded broker recovery")
	}()
	if err = runtime.ensureProject(ctx); err != nil {
		return err
	}
	if err = writePrivateFile(cfg.knownHosts(), nil); err != nil {
		return err
	}
	for index := 1; index <= cfg.guestCount(); index++ {
		if err = runtime.ensureVM(ctx, cfg.vm(index)); err != nil {
			return err
		}
	}
	if err = runtime.tightenProject(ctx); err != nil {
		return err
	}
	for index := 1; index <= cfg.guestCount(); index++ {
		if err = runtime.startVM(ctx, cfg.vm(index)); err != nil {
			return err
		}
	}
	for index := 1; index <= cfg.guestCount(); index++ {
		vm := cfg.vm(index)
		if err = runtime.waitAgent(ctx, vm); err != nil {
			return err
		}
		if err = runtime.ensureGuestTools(ctx, vm); err != nil {
			return err
		}
		if err = runtime.installManagedGuestKeys(ctx, vm); err != nil {
			return err
		}
		if err = runtime.recordHostKey(ctx, vm); err != nil {
			return err
		}
	}
	if err = os.Chmod(cfg.knownHosts(), 0o600); err != nil {
		return err
	}
	if err = runtime.ensurePeerTrust(ctx); err != nil {
		return err
	}
	for index := 1; index <= cfg.guestCount(); index++ {
		if err = runtime.sshSmoke(ctx, cfg.vm(index)); err != nil {
			return err
		}
	}
	_ = os.Remove(cfg.revokedKey())
	fmt.Fprintln(runtime.Stdout,
		"  [ ok ] all VMs are ready for lease context and bounded agent access")
	return nil
}

func (runtime *Runtime) ensureProject(ctx context.Context) error {
	cfg := runtime.Config
	totalCPU := strconv.Itoa(cfg.CPU * cfg.guestCount())
	totalMemory := cfg.totalSize(cfg.Memory)
	vmLimit := strconv.Itoa(cfg.guestCount())
	if cfg.systemContainer() {
		totalMemory, vmLimit = "", "0"
	}
	exists, err := runtime.projectPresence(ctx)
	if err != nil {
		return fmt.Errorf("inventory inner Incus projects: %w", err)
	}
	if exists {
		marker, err := runtime.incus(ctx, "project", "get", cfg.Project, "user.subyard.managed")
		if err != nil {
			return err
		}
		if strings.TrimSpace(marker) != managedMarker {
			return fmt.Errorf("project %q exists without the Subyard marker; refusing to modify it",
				cfg.Project)
		}
		names, err := runtime.projectInstances(ctx)
		if err != nil {
			return err
		}
		if err := cfg.validateManagedNames(names); err != nil {
			return err
		}
		for _, name := range names {
			if err := runtime.requireAllocationMarker(ctx, name); err != nil {
				return err
			}
		}
		currentCPU, err := runtime.incus(ctx, "project", "get", cfg.Project, "limits.cpu")
		if err != nil {
			return err
		}
		if number, parseErr := strconv.Atoi(strings.TrimSpace(currentCPU)); parseErr == nil &&
			number < cfg.CPU*cfg.guestCount() {
			if _, err := runtime.incus(ctx, "project", "set", cfg.Project, "limits.cpu", totalCPU); err != nil {
				return err
			}
		}
		currentMemory, err := runtime.incus(ctx, "project", "get", cfg.Project, "limits.memory")
		if err != nil {
			return err
		}
		if strings.TrimSpace(currentMemory) != "" {
			currentMiB, parseErr := sizeMiB(strings.TrimSpace(currentMemory))
			if parseErr != nil {
				return fmt.Errorf("managed project has an unsupported memory limit: %s",
					strings.TrimSpace(currentMemory))
			}
			targetMiB, _ := sizeMiB(totalMemory)
			if currentMiB < targetMiB {
				if _, err := runtime.incus(ctx, "project", "set", cfg.Project,
					"limits.memory", totalMemory); err != nil {
					return err
				}
			}
		}
	} else {
		if err := runtime.progress(ctx, "creating inner Incus project '"+cfg.Project+"'", func() error {
			_, err := runtime.incus(ctx, "project", "create", cfg.Project,
				"-c", "features.images=false",
				"-c", "user.subyard.managed="+managedMarker,
				"-c", "limits.instances="+strconv.Itoa(cfg.guestCount()),
				"-c", "limits.virtual-machines="+vmLimit,
				"-c", "limits.cpu="+totalCPU,
				"-c", "limits.memory="+totalMemory,
				"-c", "restricted=true")
			return err
		}); err != nil {
			return err
		}
		fmt.Fprintf(runtime.Stdout, "  [ ok ] created inner Incus project %q\n", cfg.Project)
	}
	settings := [][2]string{
		{"limits.instances", strconv.Itoa(cfg.guestCount())}, {"limits.virtual-machines", vmLimit},
		{"restricted", "true"}, {"restricted.networks.access", cfg.Network},
		{"user.subyard.managed", managedMarker},
	}
	if cfg.systemContainer() {
		// Only the broker operates this project. The lowlevel permission is
		// required by Incus for the fixed, enlarged nested ID map.
		settings = append(settings, [2]string{"limits.memory", ""}, [2]string{"restricted.virtual-machines.lowlevel", ""},
			[2]string{"restricted.containers.privilege", "isolated"}, [2]string{"restricted.containers.nesting", "allow"},
			[2]string{"restricted.containers.lowlevel", "allow"})
	} else if lowlevel, _ := vmSleepPolicy(imageArchitecture()); lowlevel != "" {
		settings = append(settings, [2]string{"restricted.virtual-machines.lowlevel", lowlevel})
	}
	for _, setting := range settings {
		if _, err := runtime.incus(ctx, "project", "set", cfg.Project, setting[0], setting[1]); err != nil {
			return err
		}
	}
	devices, err := runtime.incus(ctx, "profile", "device", "list", "default", "--project", cfg.Project)
	if err != nil {
		return err
	}
	if cfg.systemContainer() {
		for _, device := range nonemptyLines(devices) {
			if device != "root" && device != "eth0" {
				return fmt.Errorf("unexpected profile device %q in container lease project", device)
			}
		}
	}
	if !linePresent(devices, "root") {
		if _, err := runtime.incus(ctx, "profile", "device", "add", "default", "root",
			"disk", "pool=default", "path=/", "--project", cfg.Project); err != nil {
			return err
		}
	}
	if _, err := runtime.incus(ctx, "profile", "device", "set", "default", "root",
		"size", cfg.Disk, "--project", cfg.Project); err != nil {
		return err
	}
	if !linePresent(devices, "eth0") {
		if _, err := runtime.incus(ctx, "profile", "device", "add", "default", "eth0",
			"nic", "network="+cfg.Network, "name=eth0", "--project", cfg.Project); err != nil {
			return err
		}
	}
	return nil
}

func (cfg Config) validateManagedNames(names []string) error {
	for _, name := range names {
		if name != cfg.vm(1) && (cfg.guestCount() != 2 || name != cfg.vm(2)) {
			return fmt.Errorf("unexpected instance blocks reconciliation: %s", name)
		}
	}
	return nil
}

func (runtime *Runtime) ensureVM(ctx context.Context, vm string) error {
	cfg := runtime.Config
	if runtime.vmExists(ctx, vm) {
		kind, err := instanceColumn(ctx, runtime.incus, cfg.Project, vm, "t")
		if err != nil {
			return err
		}
		expected := "VIRTUAL-MACHINE"
		if cfg.systemContainer() {
			expected = "CONTAINER"
		}
		if strings.TrimSpace(kind) != expected {
			return fmt.Errorf("managed name %q is not a %s", vm, expected)
		}
		if err := runtime.requireAllocationMarker(ctx, vm); err != nil {
			return err
		}
	} else {
		if err := runtime.progress(ctx, "creating "+vm+" from "+cfg.Image, func() error {
			return runtime.initVM(ctx, vm)
		}); err != nil {
			return err
		}
		if _, err := runtime.incus(ctx, "config", "set", vm, "cloud-init.user-data",
			runtime.cloudConfig(), "--project", cfg.Project); err != nil {
			return err
		}
		fmt.Fprintf(runtime.Stdout, "  [ ok ] created %s\n", vm)
	}
	for _, setting := range cfg.instanceSettings() {
		key, value, _ := strings.Cut(setting, "=")
		if _, err := runtime.incus(ctx, "config", "set", vm, key, value, "--project", cfg.Project); err != nil {
			return err
		}
	}
	raw, err := runtime.incus(ctx, "config", "get", vm, "raw.apparmor", "--project", cfg.Project)
	if err != nil {
		return err
	}
	if strings.TrimSpace(raw) != "" {
		_, err = runtime.incus(ctx, "config", "unset", vm, "raw.apparmor", "--project", cfg.Project)
	}
	return err
}

func (cfg Config) instanceSettings() []string {
	settings := []string{"limits.cpu=" + strconv.Itoa(cfg.CPU), "boot.autostart=false"}
	if cfg.systemContainer() {
		return append(settings, "limits.processes=4096", "security.nesting=true",
			"security.idmap.isolated=true", "security.idmap.size=200000")
	}
	settings = append(settings, "limits.memory="+cfg.Memory)
	if _, raw := vmSleepPolicy(imageArchitecture()); raw != "" {
		settings = append(settings, "raw.qemu.conf="+raw)
	}
	return settings
}

func (runtime *Runtime) allocationSettings() []string {
	var settings []string
	if runtime.allocation != nil {
		settings = append(settings, "user.subyard.generation="+strconv.FormatUint(runtime.allocation.ResourceGeneration, 10),
			"user.subyard.lease-epoch="+strconv.FormatUint(runtime.allocation.LeaseEpoch, 10))
	}
	if runtime.agentLogOwner != "" {
		settings = append(settings, agentLogOwnerMarker+"="+runtime.agentLogOwner)
	}
	return settings
}

func (runtime *Runtime) initVM(ctx context.Context, vm string) error {
	cfg := runtime.Config
	settings := runtime.allocationSettings()
	if cfg.systemContainer() && runtime.allocation != nil {
		return runtime.initPreparedGuest(ctx, vm, settings)
	}
	return runtime.initFromImage(ctx, vm, settings)
}

func (runtime *Runtime) initFromImage(ctx context.Context, vm string, settings []string) error {
	cfg := runtime.Config
	sleep := runtime.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	args := []string{"init", cfg.Image, vm}
	if cfg.systemContainer() {
		args = append(args, "--project", cfg.Project)
		for _, setting := range cfg.instanceSettings() {
			args = append(args, "-c", setting)
		}
	} else {
		args = append(args, "--vm", "--project", cfg.Project, "-c", "limits.cpu="+strconv.Itoa(cfg.CPU), "-c", "limits.memory="+cfg.Memory)
	}
	args = append(args, "-c", "user.subyard.managed="+managedMarker)
	for _, setting := range settings {
		args = append(args, "-c", setting)
	}
	if !cfg.systemContainer() {
		if _, raw := vmSleepPolicy(imageArchitecture()); raw != "" {
			args = append(args, "-c", "raw.qemu.conf="+raw)
		}
	}
	const attempts = 4
	for attempt := 1; attempt <= attempts; attempt++ {
		_, err := runtime.incus(ctx, args...)
		if err == nil || !remoteImageLookupError(err) || attempt == attempts {
			return err
		}
		delay := time.Duration(attempt*5) * time.Second
		fmt.Fprintf(runtime.Stdout,
			"  [warn] remote image lookup failed for %s; retrying init (%d/%d) in %s\n",
			vm, attempt+1, attempts, delay)
		if err := sleep(ctx, delay); err != nil {
			return err
		}
	}
	panic("unreachable")
}

func remoteImageLookupError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "failed getting remote image info") ||
		(strings.Contains(message, "failed getting image") &&
			strings.Contains(message, "requested image couldn't be found"))
}

func (runtime *Runtime) tightenProject(ctx context.Context) error {
	cfg := runtime.Config
	if cfg.systemContainer() {
		_, err := runtime.incus(ctx, "project", "set", cfg.Project, "limits.cpu", strconv.Itoa(cfg.CPU*cfg.guestCount()))
		return err
	}
	for _, setting := range [][2]string{
		{"limits.cpu", strconv.Itoa(cfg.CPU * cfg.guestCount())}, {"limits.memory", cfg.totalSize(cfg.Memory)},
	} {
		if _, err := runtime.incus(ctx, "project", "set", cfg.Project, setting[0], setting[1]); err != nil {
			return err
		}
	}
	if lowlevel, _ := vmSleepPolicy(imageArchitecture()); lowlevel != "" {
		return nil
	}
	_, err := runtime.incus(ctx, "project", "unset", cfg.Project,
		"restricted.virtual-machines.lowlevel")
	return err
}

func (runtime *Runtime) startVM(ctx context.Context, vm string) error {
	state, err := instanceColumn(ctx, runtime.incus, runtime.Config.Project, vm, "s")
	if err != nil {
		return err
	}
	if strings.TrimSpace(state) == "RUNNING" {
		return nil
	}
	if strings.TrimSpace(state) != "STOPPED" {
		return fmt.Errorf("%s is not stopped before initialization", vm)
	}
	if !runtime.Config.systemContainer() {
		if err := runtime.ensureCloudInitDisk(ctx, vm); err != nil {
			return err
		}
	}
	if err := runtime.progress(ctx, "starting "+vm, func() error {
		_, err := runtime.incus(ctx, "start", vm, "--project", runtime.Config.Project)
		return err
	}); err != nil {
		return err
	}
	fmt.Fprintf(runtime.Stdout, "  [ ok ] started %s\n", vm)
	return nil
}

func (runtime *Runtime) gc(ctx context.Context) error {
	store := LeaseStore{
		Path: runtime.Config.leaseState(), SlotCount: runtime.Config.SlotCount, Now: runtime.Now,
	}
	leaseErr := runtime.ReapExpired(ctx, store)
	return errors.Join(leaseErr, runtime.ReapImages(ctx, store))
}

func (runtime *Runtime) cleanupManaged(ctx context.Context, quiet bool) error {
	cfg := runtime.Config
	if _, err := runtime.findUser(cfg.AgentUser); err == nil {
		if err := runtime.restrictAgentAccess("cleanup"); err != nil {
			return err
		}
	}
	exists, err := runtime.projectPresence(ctx)
	if err != nil {
		return fmt.Errorf("inventory managed project before cleanup: %w", err)
	}
	if exists {
		if err := runtime.requireProjectMarker(ctx); err != nil {
			return err
		}
		names, err := runtime.projectInstances(ctx)
		if err != nil {
			return errors.New("could not inventory managed project before cleanup")
		}
		for _, name := range names {
			if name != cfg.vm(1) && (cfg.guestCount() != 2 || name != cfg.vm(2)) {
				return fmt.Errorf("unexpected instance blocks cleanup: %s", name)
			}
		}
		for index := 1; index <= cfg.guestCount(); index++ {
			vm := cfg.vm(index)
			if !runtime.vmExists(ctx, vm) {
				continue
			}
			if err := runtime.requireVMMarker(ctx, vm); err != nil {
				return err
			}
			if _, err := runtime.incus(ctx, "delete", vm, "--project", cfg.Project, "--force"); err != nil {
				return err
			}
		}
		if _, err := runtime.incus(ctx, "project", "delete", cfg.Project); err != nil {
			return err
		}
	}
	for _, path := range []string{
		cfg.keyPath(), cfg.keyPath() + ".pub", cfg.knownHosts(),
		cfg.failureLog(), cfg.keyRevision(), cfg.revokedKey(),
	} {
		_ = os.Remove(path)
	}
	if !quiet {
		fmt.Fprintln(runtime.Stdout,
			"  [ ok ] deleted both disposable VMs, inner project and operator worker identity")
	}
	return nil
}

// projectPresence distinguishes a genuinely absent managed project from an
// unavailable inner Incus daemon. Destructive recovery must never treat a
// failed inventory command as proof that the disposable data is absent.
func (runtime *Runtime) projectPresence(ctx context.Context) (bool, error) {
	value, err := runtime.incus(ctx, "project", "list", "--format", "csv", "-c", "n")
	if err != nil {
		return false, err
	}
	return linePresent(value, runtime.Config.Project), nil
}

func (runtime *Runtime) vmExists(ctx context.Context, vm string) bool {
	_, err := runtime.incus(ctx, "info", vm, "--project", runtime.Config.Project)
	return err == nil
}

func (runtime *Runtime) projectInstances(ctx context.Context) ([]string, error) {
	value, err := runtime.incus(ctx, "list", "--project", runtime.Config.Project, "-f", "csv", "-c", "n")
	if err != nil {
		return nil, err
	}
	return nonemptyLines(value), nil
}

func (runtime *Runtime) requireProjectMarker(ctx context.Context) error {
	value, err := runtime.incus(ctx, "project", "get", runtime.Config.Project,
		"user.subyard.managed")
	if err != nil {
		return err
	}
	if strings.TrimSpace(value) != managedMarker {
		return fmt.Errorf("project %q is not Subyard-managed", runtime.Config.Project)
	}
	return nil
}

func (runtime *Runtime) requireVMMarker(ctx context.Context, vm string) error {
	value, err := runtime.incus(ctx, "config", "get", vm, "user.subyard.managed",
		"--project", runtime.Config.Project)
	if err != nil {
		return err
	}
	if strings.TrimSpace(value) != managedMarker {
		return fmt.Errorf("VM %q is not Subyard-managed", vm)
	}
	return nil
}

func (runtime *Runtime) vmIP(ctx context.Context, vm string) (string, error) {
	routes, err := runtime.guest(ctx, vm, nil, "ip", "-4", "route", "show", "default")
	if err != nil {
		return "", err
	}
	interfaces := map[string]bool{}
	for _, line := range strings.Split(routes, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "default" {
			continue
		}
		for index := 0; index+1 < len(fields); index++ {
			if fields[index] == "dev" {
				interfaces[fields[index+1]] = true
			}
		}
	}
	if len(interfaces) != 1 {
		return "", fmt.Errorf("expected exactly one default-route interface for %s", vm)
	}
	var name string
	for candidate := range interfaces {
		name = candidate
	}
	path := "/1.0/instances/" + url.PathEscape(vm) + "/state?project=" + url.QueryEscape(runtime.Config.Project)
	payload, err := runtime.incus(ctx, "query", path)
	if err != nil {
		return "", err
	}
	var state struct {
		Network map[string]struct {
			Addresses []struct {
				Family  string `json:"family"`
				Scope   string `json:"scope"`
				Address string `json:"address"`
			} `json:"addresses"`
		} `json:"network"`
	}
	if err := json.Unmarshal([]byte(payload), &state); err != nil {
		return "", fmt.Errorf("decode %s network state: %w", vm, err)
	}
	unique := map[string]bool{}
	for _, address := range state.Network[name].Addresses {
		if address.Family == "inet" && address.Scope == "global" &&
			net.ParseIP(address.Address).To4() != nil {
			unique[address.Address] = true
		}
	}
	if len(unique) != 1 {
		return "", errors.New("expected exactly one global IPv4 address on the default-route interface")
	}
	for address := range unique {
		return address, nil
	}
	panic("unreachable")
}

// instanceColumn selects one status or type column from an exact instance name.
func instanceColumn(ctx context.Context, incus func(context.Context, ...string) (string, error), project, instance, column string) (string, error) {
	rows, err := incus(ctx, "list", instance, "--project", project, "-f", "csv", "-c", "n"+column)
	if err != nil {
		return "", err
	}
	// Incus names and the status/type columns contain no CSV delimiters.
	for row := range strings.SplitSeq(rows, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(row), instance+","); ok {
			return value, nil
		}
	}
	return "", nil
}

func (runtime *Runtime) incus(ctx context.Context, arguments ...string) (string, error) {
	stdout, _, err := runtime.Runner.Run(ctx, runtime.Config.Incus, arguments, nil, nil)
	return string(stdout), err
}

func (runtime *Runtime) guest(
	ctx context.Context,
	vm string,
	environment []string,
	arguments ...string,
) (string, error) {
	incusArguments := []string{"exec", vm, "--project", runtime.Config.Project}
	for _, value := range environment {
		incusArguments = append(incusArguments, "--env", value)
	}
	incusArguments = append(incusArguments, "--")
	incusArguments = append(incusArguments, arguments...)
	return runtime.incus(ctx, incusArguments...)
}

func (runtime *Runtime) progress(ctx context.Context, label string, operation func() error) error {
	fmt.Fprintf(runtime.Stdout, "  [ .. ] %s\n", label)
	done := make(chan error, 1)
	go func() { done <- operation() }()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	started := runtime.Now()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			fmt.Fprintf(runtime.Stdout, "  [ .. ] %s (still working, %ds elapsed)\n",
				label, int(runtime.Now().Sub(started)/time.Second))
		case <-ctx.Done():
			// Reap the native operation before allowing diagnostics or recovery.
			<-done
			return context.Cause(ctx)
		}
	}
}

func (runtime *Runtime) waitFor(
	ctx context.Context,
	label string,
	probe func(context.Context) error,
) error {
	waitCtx, cancel := context.WithTimeout(ctx, runtime.Config.BootTimeout)
	defer cancel()
	started := runtime.Now()
	deadline := started.Add(runtime.Config.BootTimeout)
	nextReport := started.Add(10 * time.Second)
	lastFailure := "error_kind=not_probed"
	fmt.Fprintf(runtime.Stdout, "  [ .. ] %s\n", label)
	for {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		now := runtime.Now()
		if !now.Before(deadline) || waitCtx.Err() != nil {
			return &readinessFailure{Label: label, Budget: runtime.Config.BootTimeout,
				StartedAt: started, ObservedAt: now, LastProbe: lastFailure}
		}
		probeErr := probe(waitCtx)
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		now = runtime.Now()
		if probeErr == nil && waitCtx.Err() == nil && now.Before(deadline) {
			return nil
		}
		if probeErr == nil {
			probeErr = context.DeadlineExceeded
		}
		lastFailure = readinessProbeMetadata(probeErr)
		if !now.Before(deadline) || waitCtx.Err() != nil {
			continue
		}
		if !now.Before(nextReport) {
			fmt.Fprintf(runtime.Stdout, "  [ .. ] %s (%ds elapsed)\n",
				label, int(now.Sub(started)/time.Second))
			nextReport = now.Add(10 * time.Second)
		}
		if err := runtime.Sleep(waitCtx, min(2*time.Second, deadline.Sub(now))); err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			if waitCtx.Err() != nil {
				continue
			}
			return err
		}
	}
}

func readinessProbeMetadata(err error) string {
	var commandError *CommandError
	switch {
	case errors.As(err, &commandError) && commandError != nil:
		return fmt.Sprintf("error_kind=command_error exit_code=%d duration=%s", commandError.ExitCode, commandError.Duration)
	case errors.Is(err, context.DeadlineExceeded):
		return "error_kind=context_deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "error_kind=context_canceled"
	default:
		return "error_kind=probe_error"
	}
}

// Readiness failures retain only classified probe metadata, never argv or output.
// VM and phase are assigned by the owning guest operation before crossing recovery.
type readinessFailure struct {
	Label      string
	Project    string
	VM         string
	Phase      string
	Budget     time.Duration
	StartedAt  time.Time
	ObservedAt time.Time
	LastProbe  string
}

func (failure *readinessFailure) Error() string {
	if failure.LastProbe == "error_kind=not_observed" {
		return failure.Label + "; readiness outcome unavailable"
	}
	return fmt.Sprintf("%s did not become ready within %s; last readiness probe: %s",
		failure.Label, failure.Budget, failure.LastProbe)
}

func guestReadinessFailure(err error, vm, phase string) error {
	var failure *readinessFailure
	if errors.As(err, &failure) {
		failure.VM, failure.Phase = vm, phase
	}
	return err
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (runtime *Runtime) ensureStateDir() error {
	cfg := runtime.Config
	if os.Geteuid() == 0 {
		if err := safefs.EnsureDir(cfg.StateDir, safefs.DirOptions{ParentMode: 0o700, Existing: safefs.Repair}); err != nil {
			return err
		}
		return writePrivateFile(cfg.stateMarker(), []byte(managedMarker+"\n"))
	}
	info, err := os.Stat(cfg.StateDir)
	if err != nil || !info.IsDir() {
		return errors.New("state directory is not writable; re-run yard init on the owner host")
	}
	file, err := os.CreateTemp(cfg.StateDir, ".write-check-*") //nolint:forbidigo // Writability probe.
	if err != nil {
		return errors.New("state directory is not writable; re-run yard init on the owner host")
	}
	name := file.Name()
	_ = file.Close()
	_ = os.Remove(name)
	return nil
}

func (runtime *Runtime) ensureKey(ctx context.Context) error {
	cfg := runtime.Config
	if err := runtime.ensureStateDir(); err != nil {
		return err
	}
	if _, err := os.Stat(cfg.keyRevision()); os.IsNotExist(err) {
		if payload, readErr := os.ReadFile(cfg.keyPath() + ".pub"); readErr == nil {
			if key, normalizeErr := normalizedPublicKey(string(payload)); normalizeErr == nil {
				if err := writePrivateFile(cfg.revokedKey(), []byte(key+"\n")); err != nil {
					return err
				}
			}
		}
		_ = os.Remove(cfg.keyPath())
		_ = os.Remove(cfg.keyPath() + ".pub")
	}
	if !regularNonempty(cfg.keyPath()) || !regularNonempty(cfg.keyPath()+".pub") {
		_ = os.Remove(cfg.keyPath())
		_ = os.Remove(cfg.keyPath() + ".pub")
		if _, _, err := runtime.Runner.Run(ctx, "ssh-keygen", []string{
			"-q", "-t", "ed25519", "-N", "", "-C", "subyard-managed-e2e-worker",
			"-f", cfg.keyPath(),
		}, nil, nil); err != nil {
			return err
		}
	}
	if err := writePrivateFile(cfg.keyRevision(), nil); err != nil {
		return err
	}
	if err := os.Chmod(cfg.keyPath(), 0o600); err != nil {
		return err
	}
	return os.Chmod(cfg.keyPath()+".pub", 0o644)
}

func writePrivateFile(path string, payload []byte) error {
	return safefs.WriteFile(path, payload, safefs.Options{Mode: 0o600, ParentMode: 0o700})
}

func writeAtomic(path string, payload []byte, mode os.FileMode) error {
	return safefs.WriteFile(path, payload, safefs.Options{
		Mode: mode, ParentMode: 0o755, TempPattern: "." + filepath.Base(path) + ".*",
	})
}

func regularNonempty(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func linePresent(value, line string) bool {
	for _, candidate := range strings.Split(value, "\n") {
		if strings.TrimSpace(candidate) == line {
			return true
		}
	}
	return false
}

func nonemptyLines(value string) []string {
	var result []string
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}

func (runtime *Runtime) findUser(name string) (*user.User, error) {
	if runtime.lookupUser != nil {
		return runtime.lookupUser(name)
	}
	return user.Lookup(name)
}
