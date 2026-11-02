package migration

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/resource"
)

// RegisteredYardTarget is loaded by the CLI through its normal protected resolver.
type RegisteredYardTarget struct {
	Loaded  config.Loaded
	Runtime reconcileruntime.Runtime
}
type RegisteredYardMigration struct {
	prepared                        map[string]config.PersistentFileSnapshot
	observedActual, observedDesired releasetransition.Fingerprint
	Yard                            string
	LoadTargets                     func(context.Context, releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error)
	settingsView                    releasetransition.V2SettingsSnapshotView
	ConfigHome                      string
	Resources                       resource.Registry
	GuestTmp                        bool
	Prompt                          ports.Prompter
}

func (m *RegisteredYardMigration) ID() string { return "yard." + m.Yard }

type registeredYardPlan struct {
	state                      ports.ReconcileState
	resources                  []ports.TeardownResource
	loaded                     config.Loaded
	runtime                    reconcileruntime.Runtime
	instance                   ports.InstanceInfo
	metadata, before, receipts map[string]string
	settingsPath               string
	settingsBefore             config.PersistentFileSnapshot
	settingsDesired            []byte
}

func migrationFingerprint(value any) releasetransition.Fingerprint {
	payload, _ := json.Marshal(value)
	return releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(payload)))
}
func (m *RegisteredYardMigration) plans(ctx context.Context) ([]registeredYardPlan, error) {
	m.prepared = nil
	targets, err := m.LoadTargets(ctx, m.settingsView)
	if err != nil {
		return nil, err
	}
	plans := []registeredYardPlan{}
	for _, target := range targets {
		loaded, runtime := target.Loaded, target.Runtime
		if loaded.Context.YardName != m.Yard {
			continue
		}
		if native, ok := runtime.Incus.(interface{ LocalInstallationAbsent() bool }); ok && native.LocalInstallationAbsent() {
			continue
		}
		instance, err := runtime.Incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
		if errors.Is(err, ports.ErrInstanceNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if instance.Project != loaded.Context.IncusProject || instance.Name != loaded.Context.YardInstanceName || instance.Config["volatile.uuid"] == "" || instance.Config["user.subyard.name"] != loaded.Context.YardName || instance.Config["user.subyard.managed"] != "true" || instance.Config["user.subyard.bridge"] != loaded.Context.IncusBridge {
			return nil, errors.New("registered yard native identity differs from protected registration")
		}
		for _, key := range []string{"volatile.uuid", "user.subyard.managed", "user.subyard.name", "user.subyard.bridge", "user.subyard.desired_power", "user.subyard.disk_tmp"} {
			if instance.Config[key] != instance.LocalConfig[key] {
				return nil, errors.New("registered yard identity or power is inherited or inconsistent")
			}
		}
		if marker := instance.LocalConfig["user.subyard.disk_tmp"]; marker != "" && marker != "v1" {
			return nil, errors.New("registered yard has foreign guest temporary policy metadata")
		}
		if power := instance.Config["user.subyard.desired_power"]; power != "running" && power != "stopped" {
			return nil, errors.New("registered yard has no canonical desired power")
		}
		if _, _, err := configsync.ResolveHostID(m.ConfigHome, loaded.Environment); err != nil {
			return nil, err
		}
		plan := registeredYardPlan{loaded: loaded, runtime: runtime, instance: instance}
		contracts := m.Resources.ProxyContracts(strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]))
		state, resources, metadata, before, receipts, err := inspectRegisteredYardMigration(ctx, runtime, contracts)
		if err != nil {
			return nil, fmt.Errorf("yard %s: %w", loaded.Context.YardName, err)
		}
		plan.instance, plan.metadata, plan.before, plan.receipts = state.Instance, metadata, before, receipts
		plan.state, plan.resources = state, resources
		plan.settingsPath, err = registeredSettingsPath(loaded)
		if err != nil {
			return nil, err
		}
		plan.settingsBefore, err = config.ReadPersistentFileSnapshot(m.ConfigHome, plan.settingsPath)
		if m.settingsView != nil {
			plan.settingsBefore, err = m.settingsView.ReadSnapshot(plan.settingsPath)
		}
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		if err != nil {
			return nil, err
		}
		plan.settingsDesired = slices.Clone(plan.settingsBefore.Content)
		for _, contract := range contracts {
			device := instance.LocalDevices[contract.Device]
			if len(device) == 0 {
				continue
			}
			host, port, _ := net.SplitHostPort(strings.TrimPrefix(device["listen"], "tcp:"))
			for _, assignment := range []struct{ key, value string }{{contract.AdvertiseHostSetting, host}, {contract.HostPortSetting, port}} {
				key, value := assignment.key, assignment.value
				if loaded.Environment[key] == value {
					continue
				}
				trace, declared := loaded.Settings[key]
				if !declared || !registeredMigrationDefault(trace) {
					return nil, fmt.Errorf("yard %s: explicit %s conflicts with working native endpoint", loaded.Context.YardName, key)
				}
				if err := loaded.Catalog.ValidateSetting(config.ScopeYard, key, value, false); err != nil {
					return nil, err
				}
				plan.settingsDesired, err = config.EditPersistentAssignmentContent(plan.settingsPath, plan.settingsDesired, key, &value)
				if err != nil {
					return nil, err
				}
			}
		}
		if string(plan.settingsBefore.Content) != string(plan.settingsDesired) {
			if err := config.CheckLocalSettingsWritable(m.ConfigHome); err != nil {
				return nil, err
			}
			if loaded.Context.YardName != "default" {
				source, err := config.FindYardRegistrationFile(loaded.Context.Paths.ConfigDir, m.ConfigHome, loaded.Context.YardName)
				if err != nil {
					return nil, err
				}
				if source != plan.settingsPath {
					return nil, errors.New("endpoint import requires a protected local yard registration; refusing to shadow the existing private or Git registration")
				}
			}
		}
		if m.GuestTmp && plan.resources == nil {
			plan.resources, err = runtime.Incus.TeardownInventory(ctx, loaded.Context.IncusProject)
			if err != nil {
				return nil, err
			}
		}
		if !m.GuestTmp && (plan.settingsBefore.Exists || len(plan.settingsDesired) != 0) {
			if m.prepared == nil {
				m.prepared = map[string]config.PersistentFileSnapshot{}
			}
			snapshot := plan.settingsBefore
			snapshot.Exists = snapshot.Exists || len(plan.settingsDesired) != 0
			snapshot.Content = slices.Clone(plan.settingsDesired)
			m.prepared[plan.settingsPath] = snapshot
		}
		plans = append(plans, plan)
	}
	return plans, nil
}
func (m *RegisteredYardMigration) Observe(ctx context.Context, _ releasetransition.ReleasePair, _ releasetransition.ReleaseLinks) (releasetransition.V2ActivationObservation, error) {
	m.settingsView = nil
	return m.observe(ctx)
}
func (m *RegisteredYardMigration) ObserveProspective(ctx context.Context, _ releasetransition.ReleasePair, _ releasetransition.ReleaseLinks, view releasetransition.V2SettingsSnapshotView) (releasetransition.V2ActivationObservation, error) {
	m.settingsView = view
	return m.observe(ctx)
}
func (m *RegisteredYardMigration) observe(ctx context.Context) (releasetransition.V2ActivationObservation, error) {
	plans, err := m.plans(ctx)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	observed, err := m.observePlans(ctx, plans)
	if err == nil {
		m.observedActual, m.observedDesired = observed.Actual, observed.Desired
	}
	return observed, err
}
func (m *RegisteredYardMigration) observePlans(ctx context.Context, plans []registeredYardPlan) (releasetransition.V2ActivationObservation, error) {
	identities, err := m.boundIdentities(plans)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	complete := true
	consequences := []string{}
	for _, plan := range plans {
		instance := plan.instance
		if m.GuestTmp {
			ready, err := registeredDiskTmpReady(ctx, plan.runtime)
			if err != nil {
				return releasetransition.V2ActivationObservation{}, err
			}
			if strings.EqualFold(instance.Status, "running") && (!ready || instance.LocalConfig["user.subyard.disk_tmp"] != "v1") {
				if err := assessRegisteredDiskTmp(ctx, plan.runtime); err != nil {
					return releasetransition.V2ActivationObservation{}, err
				}
			}
			powerMatches := strings.EqualFold(instance.Status, instance.LocalConfig["user.subyard.desired_power"])
			complete = complete && ready && powerMatches && instance.LocalConfig["user.subyard.disk_tmp"] == "v1"
			if !ready || instance.LocalConfig["user.subyard.disk_tmp"] != "v1" {
				consequences = append(consequences, "yard "+plan.loaded.Context.YardName+": install disk-backed /tmp policy; clear /tmp at later stops and boots; any necessary restart requires separate later confirmation")
			}
		} else {
			projectGrant := false
			for key, value := range plan.metadata {
				projectGrant = projectGrant || plan.before[key] != value
			}
			complete = complete && !projectGrant
			if projectGrant {
				consequences = append(consequences, fmt.Sprintf("yard %s: register ownership of project %s; allow normal management and full removal of this project and its assessed resources", plan.loaded.Context.YardName, plan.loaded.Context.IncusProject))
			}
			for key, value := range plan.receipts {
				if instance.LocalConfig[key] == value {
					continue
				}
				complete = false
				device := instance.LocalDevices[strings.TrimPrefix(key, "user.subyard.resource.")]
				consequences = append(consequences, fmt.Sprintf("yard %s: register resource receipt %s; manage and remove native listener %s -> %s", plan.loaded.Context.YardName, key, device["listen"], device["connect"]))
			}

		}
	}
	desired := migrationFingerprint([]any{m.ID(), m.GuestTmp, identities, "complete"})
	actual := desired
	if !complete {
		actual = migrationFingerprint([]any{m.ID(), m.GuestTmp, identities, "pending"})
	}
	return releasetransition.V2ActivationObservation{Actual: actual, Desired: desired, Converged: complete, Consequences: consequences}, nil
}
func (m *RegisteredYardMigration) boundIdentities(plans []registeredYardPlan) ([]any, error) {
	identities := []any{}
	for _, plan := range plans {
		binding, err := m.binding(plan)
		if err != nil {
			return nil, err
		}
		identities = append(identities, binding)
	}
	return identities, nil
}
func (m *RegisteredYardMigration) Reconcile(ctx context.Context, _ releasetransition.ReleaseLinks) error {
	plans, err := m.plans(ctx)
	if err != nil {
		return err
	}
	checkFresh := func() error {
		fresh, err := m.plans(ctx)
		if err != nil {
			return err
		}
		if m.GuestTmp {
			identities, err := m.boundIdentities(fresh)
			if err != nil {
				return err
			}
			if m.observedDesired == "" || migrationFingerprint([]any{m.ID(), m.GuestTmp, identities, "complete"}) != m.observedDesired {
				return domain.ErrPlanStale
			}
			return nil
		}
		observed, err := m.observePlans(ctx, fresh)
		if err != nil {
			return err
		}
		if m.observedDesired == "" || observed.Desired != m.observedDesired || observed.Actual != m.observedActual && observed.Actual != m.observedDesired {
			return domain.ErrPlanStale
		}
		return nil
	}
	if err := checkFresh(); err != nil {
		return err
	}
	for _, plan := range plans {
		if m.GuestTmp {
			if err := migrateRegisteredDiskTmp(ctx, plan.runtime, func(request domain.ConfirmationRequest) (bool, error) { return m.confirmMaintenance(ctx, request) }, checkFresh); err != nil {
				return err
			}
			continue
		}
		if len(plan.metadata) != 0 {
			adopter, ok := plan.runtime.Incus.(ports.ProjectAdopter)
			if !ok {
				return errors.New("registered project ownership CAS is unavailable")
			}
			if err := adopter.AdoptProject(ctx, plan.loaded.Context.IncusProject, plan.before, plan.metadata); err != nil {
				return err
			}
		}
		if len(plan.receipts) != 0 {
			if err := checkFresh(); err != nil {
				return err
			}
			writer, ok := plan.runtime.Incus.(ports.RegisteredInstanceAdopter)
			if !ok {
				return errors.New("registered resource ownership CAS is unavailable")
			}
			if err := writer.AdoptRegisteredInstance(ctx, plan.instance, plan.receipts); err != nil {
				return err
			}
		}

	}
	return nil
}
func (m *RegisteredYardMigration) confirmMaintenance(ctx context.Context, request domain.ConfirmationRequest) (bool, error) {
	if m.Prompt == nil {
		return false, domain.ErrConfirmationRequired
	}
	return m.Prompt.Confirm(ctx, request)
}
func registeredSettingsPath(loaded config.Loaded) (string, error) {
	if !domain.SafeName(loaded.Context.YardName) {
		return "", errors.New("unsafe registered yard")
	}
	nested := filepath.Join(loaded.Context.Paths.ConfigHome, "yards", loaded.Context.YardName, "config.env")
	if loaded.Context.YardName != "default" {
		source, err := config.FindYardRegistrationFile(loaded.Context.Paths.ConfigDir, loaded.Context.Paths.ConfigHome, loaded.Context.YardName)
		if err != nil {
			return "", err
		}
		flat := filepath.Join(loaded.Context.Paths.ConfigHome, "yards", loaded.Context.YardName+".env")
		if source == flat {
			return flat, nil
		}
	}
	return nested, nil
}

func registeredMigrationDefault(trace config.SettingTrace) bool {
	for _, resolution := range trace.Resolutions {
		if resolution.Status == "effective" {
			return resolution.Scope == "shipped" || resolution.Scope == "default" && resolution.Role == "shipped defaults"
		}
	}
	return trace.EffectiveValue == ""
}

func (m *RegisteredYardMigration) binding(plan registeredYardPlan) (any, error) {
	configuration := maps.Clone(plan.instance.LocalConfig)
	expanded := maps.Clone(plan.instance.Config)
	for _, values := range []map[string]string{configuration, expanded} {
		// Only exact lifecycle outputs and independently validated producer results.
		for _, key := range []string{"volatile.last_state.power", "volatile.last_state.ready", "volatile.eth0.host_name", "volatile.eth0.last_state.ip_addresses"} {
			delete(values, key)
		}
		if plan.runtime.Yard.YardKind == domain.YardVM && plan.instance.Type == domain.YardVM {
			for _, key := range []string{"volatile.vm.rtc_adjustment", "volatile.vm.rtc_offset"} {
				if parsed, err := strconv.ParseInt(values[key], 10, 64); err == nil && strconv.FormatInt(parsed, 10) == values[key] {
					delete(values, key)
				}
			}
		}
		for key, value := range plan.receipts {
			values[key] = value
		}
		if m.GuestTmp {
			values["user.subyard.disk_tmp"] = "v1"
		}
	}
	project := maps.Clone(plan.before)
	for key, value := range plan.metadata {
		project[key] = value
	}
	// Verified asset handles are process-local labels, not persisted yard identity.
	assetRoot, yard := plan.runtime.RepositoryRoot, plan.loaded.Context
	assetRoles := filepath.IsAbs(assetRoot) && yard.Paths.RepositoryRoot == assetRoot
	if assetRoles {
		yard = domain.ContinuationContext(yard)
		if plan.loaded.Context.Paths.ConfigDir != filepath.Join(assetRoot, "config") {
			yard.Paths.ConfigDir = plan.loaded.Context.Paths.ConfigDir
		}
	}
	environment := map[string]string{}
	for key, trace := range plan.loaded.Settings {
		value := trace.EffectiveValue
		if assetRoles && registeredMigrationDefault(trace) && (trace.Kind == config.SettingFile || trace.Type == config.SettingRegularFilePath) && filepath.Clean(value) == value && strings.HasPrefix(value, assetRoot+string(filepath.Separator)) {
			value = "release-asset:" + strings.TrimPrefix(value, assetRoot+string(filepath.Separator))
		}
		environment[key] = value
	}
	for _, contract := range m.Resources.ProxyContracts(strings.Fields(environment["ENVIRONMENT_PROFILES"])) {
		device := plan.instance.LocalDevices[contract.Device]
		if len(device) == 0 {
			continue
		}
		host, port, _ := net.SplitHostPort(strings.TrimPrefix(device["listen"], "tcp:"))
		environment[contract.AdvertiseHostSetting], environment[contract.HostPortSetting] = host, port
	}
	inputs := map[string]config.PersistentFileSnapshot{}
	hostID, err := config.ReadPersistentFileSnapshot(m.ConfigHome, configsync.HostIDPath(m.ConfigHome))
	if err != nil {
		return nil, err
	}
	inputs[configsync.HostIDPath(m.ConfigHome)] = hostID
	for _, layer := range plan.loaded.ConfigurationLayers {
		if !filepath.IsAbs(layer.Path) || layer.Path == plan.settingsPath || !strings.HasPrefix(layer.Path, m.ConfigHome+string(filepath.Separator)) {
			continue
		}
		snapshot, err := config.ReadPersistentFileSnapshot(m.ConfigHome, layer.Path)
		if err != nil {
			return nil, err
		}
		inputs[layer.Path] = snapshot
	}
	inventory := []string{}
	for _, entry := range plan.resources {
		identity := entry.Kind + "/" + entry.Pool + "/" + entry.Name
		if entry.Kind != "project" && entry.Kind != "instance" {
			identity += "/" + entry.Binding
		}
		inventory = append(inventory, identity)
	}
	slices.Sort(inventory)
	mode, uid, gid := uint32(0600), uint32(os.Getuid()), uint32(os.Getgid())
	security := plan.settingsBefore
	if security.Identity.Mode == 0 {
		actual, err := config.ReadPersistentFileSnapshot(m.ConfigHome, plan.settingsPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		security = actual
	}
	if security.Exists {
		mode = security.Identity.Mode & 0777
		uid = security.Identity.UID
		gid = security.Identity.GID
	}
	return []any{yard, plan.instance.Type, configuration, expanded, plan.instance.Devices, plan.instance.LocalDevices, project, plan.state.ProfileConfig, plan.state.ProfileDevices, inventory, environment, inputs, plan.settingsPath, string(plan.settingsDesired), mode, uid, gid}, nil
}

func (m *RegisteredYardMigration) ReadPreparedInput(path string) (config.PersistentFileSnapshot, bool, error) {
	snapshot, present := m.prepared[path]
	snapshot.Content = slices.Clone(snapshot.Content)
	return snapshot, present, nil
}

func (m *RegisteredYardMigration) PreparedInputPaths() ([]string, error) {
	paths := make([]string, 0, len(m.prepared))
	for path := range m.prepared {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths, nil
}
