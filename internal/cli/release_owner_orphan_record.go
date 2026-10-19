package cli

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

const ownerOrphanRecordLimit = 96 << 20

type ownerOrphanAction struct {
	Local, Verb string
	Definition  domain.ActionDefinition
}

// Definition's private declarations and executable binding are reconstructed
// only from the sealed registry. The record retains their checked identities.
type ownerOrphanDefinition struct {
	Profile, Name, Command, Handler, HandlerPath string
	BringUp, Shutdown, Title                     string
	Verbs, ControllerSessions                    []string
	Proxy                                        *resource.ProxyContract
	Dashboard                                    *resource.DashboardContract
	Management                                   *resource.ManagementContract
	Endpoint                                     *resource.EndpointDefaults
	Bootstrap, Startup                           bool
	Actions                                      []ownerOrphanAction
}

// Plain fields preserve empty assessment slices, whose domain JSON tags omit
// empty values. The wire grammar has no compatibility or custom decoders.
type ownerOrphanAssessment struct {
	Action       domain.ActionID
	Effect       domain.ActionEffect
	Changed      bool
	Impacts      []domain.ActionImpact
	Recovery     domain.RecoveryClass
	Consequences []string
}

func describeOwnerOrphanAssessment(value *domain.ActionAssessment) *ownerOrphanAssessment {
	if value == nil {
		return nil
	}
	return &ownerOrphanAssessment{value.Action, value.Effect, value.Changed, value.Impacts, value.Recovery, value.Consequences}
}
func (value *ownerOrphanAssessment) restore() *domain.ActionAssessment {
	if value == nil {
		return nil
	}
	return &domain.ActionAssessment{Action: value.Action, Effect: value.Effect, Changed: value.Changed, Impacts: value.Impacts, Recovery: value.Recovery, Consequences: value.Consequences}
}

type ownerOrphanDeviceRow struct{ Key, Value string }
type ownerOrphanEntry struct {
	Definition                            ownerOrphanDefinition
	Contract                              resource.ProxyContract
	DevicePresent                         bool
	Device                                []ownerOrphanDeviceRow
	Marker                                string
	ShutdownEligible, ShutdownUnavailable bool
	Shutdown                              *ownerOrphanAssessment
}
type ownerOrphanRecord struct {
	Version        int
	RepositoryRoot string
	Yard           yardnetwork.Yard
	Entries        []ownerOrphanEntry
}

func ownerOrphanHandlerLabel(value string, cli *CLI, logical string) string {
	from := cli.options.RepositoryRoot
	if strings.HasPrefix(from, "/proc/self/fd/") {
		value = ownerRepositoryValue(value, strings.Replace(from, "/proc/self/", "/proc/"+strconv.Itoa(os.Getpid())+"/", 1), logical)
	}
	return ownerRepositoryValue(value, from, logical)
}

func describeOwnerOrphanDefinition(value resource.Definition, cli *CLI, logical string) (ownerOrphanDefinition, error) {
	if value.Command == "" || value.HandlerPath() == "" || value.Proxy == nil || !value.Proxy.IsOwnerIPv4Ingress() {
		return ownerOrphanDefinition{}, domain.ErrPlanStale
	}
	d := ownerOrphanDefinition{Profile: value.Profile, Name: value.Name, Command: value.Command, Handler: value.Handler, HandlerPath: ownerOrphanHandlerLabel(value.HandlerPath(), cli, logical), BringUp: value.BringUp, Shutdown: value.Shutdown, Title: value.Title, Verbs: value.Verbs, ControllerSessions: value.ControllerSessions, Proxy: value.Proxy, Dashboard: value.Dashboard, Management: value.Management, Endpoint: value.Endpoint, Bootstrap: value.Bootstrap, Startup: value.Startup}
	for _, action := range cli.resources.ActionDefinitions() {
		local, belongs := localResourceAction(value, action.Action)
		if !belongs {
			continue
		}
		verb := ""
		for _, candidate := range value.Verbs {
			if id, ok := cli.resources.LookupAction(value.Command, candidate, local); ok && id == action.Action {
				if verb != "" {
					return ownerOrphanDefinition{}, errors.New("ambiguous original orphan action")
				}
				verb = candidate
			}
		}
		if verb == "" {
			return ownerOrphanDefinition{}, domain.ErrPlanStale
		}
		d.Actions = append(d.Actions, ownerOrphanAction{local, verb, action})
	}
	if len(d.Actions) == 0 {
		return ownerOrphanDefinition{}, domain.ErrPlanStale
	}
	return d, nil
}

func validateOwnerOrphanEntry(entry orphanIngressEntry, description ownerOrphanDefinition) error {
	if entry.definition.Proxy == nil || *entry.definition.Proxy != entry.contract || entry.shutdownUnavailable && !entry.shutdownEligible || entry.shutdown != nil && (!entry.shutdownEligible || entry.shutdownUnavailable) {
		return domain.ErrPlanStale
	}
	if entry.shutdown == nil {
		return nil
	}
	if err := validateOrphanShutdown(entry.definition, *entry.shutdown); err != nil {
		return err
	}
	for _, action := range description.Actions {
		if action.Definition.Action == entry.shutdown.Action && action.Verb == entry.definition.Shutdown && action.Definition.Effect == entry.shutdown.Effect && action.Definition.Recovery == entry.shutdown.Recovery && slices.Equal(action.Definition.Impacts, entry.shutdown.Impacts) {
			return nil
		}
	}
	return domain.ErrPlanStale
}

// The surrounding source capsule proves executable bytes and retained source
// descriptors. This record restores only the original fixed metadata graph.
func captureOwnerOrphanRecord(plan *orphanIngressPlan, cli *CLI, logicalRepositoryRoot string) ([]byte, error) {
	if plan == nil {
		return nil, nil
	}
	if len(plan.entries) != 0 && (cli == nil || !filepath.IsAbs(logicalRepositoryRoot) || filepath.Clean(logicalRepositoryRoot) != logicalRepositoryRoot) {
		return nil, domain.ErrPlanStale
	}
	r := ownerOrphanRecord{Version: 1, RepositoryRoot: logicalRepositoryRoot, Yard: plan.yard}
	if plan.entries != nil {
		r.Entries = []ownerOrphanEntry{}
	}
	seen := map[string]bool{}
	for _, entry := range plan.entries {
		definition, ok := cli.resources.Lookup(entry.definition.Command)
		if !ok || definition.Command != entry.definition.Command || !reflect.DeepEqual(definition, entry.definition) || seen[definition.Command] {
			return nil, domain.ErrPlanStale
		}
		seen[definition.Command] = true
		description, err := describeOwnerOrphanDefinition(definition, cli, logicalRepositoryRoot)
		if err != nil {
			return nil, err
		}
		if err := validateOwnerOrphanEntry(entry, description); err != nil {
			return nil, err
		}
		row := ownerOrphanEntry{Definition: description, Contract: entry.contract, DevicePresent: entry.device != nil, Marker: entry.marker, ShutdownEligible: entry.shutdownEligible, ShutdownUnavailable: entry.shutdownUnavailable, Shutdown: describeOwnerOrphanAssessment(entry.shutdown)}
		for _, key := range slices.Sorted(maps.Keys(entry.device)) {
			row.Device = append(row.Device, ownerOrphanDeviceRow{key, entry.device[key]})
		}
		r.Entries = append(r.Entries, row)
	}
	return nativejson.EncodeLosslessNativeJSON(r, ownerOrphanRecordLimit)
}

func restoreOwnerOrphanRecord(raw []byte, cli *CLI, logicalRepositoryRoot string) (*orphanIngressPlan, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var r ownerOrphanRecord
	if err := nativejson.DecodeLosslessNativeJSON(raw, &r, ownerOrphanRecordLimit); err != nil {
		return nil, err
	}
	if r.Version != 1 || r.RepositoryRoot != logicalRepositoryRoot || len(r.Entries) != 0 && (cli == nil || !filepath.IsAbs(logicalRepositoryRoot) || filepath.Clean(logicalRepositoryRoot) != logicalRepositoryRoot) {
		return nil, domain.ErrPlanStale
	}
	plan := &orphanIngressPlan{yard: r.Yard}
	if r.Entries != nil {
		plan.entries = []orphanIngressEntry{}
	}
	seen := map[string]bool{}
	for _, row := range r.Entries {
		// Lookup accepts aliases generally; this boundary accepts only the exact
		// originally recorded command, never a newly chosen resource or verb.
		definition, ok := cli.resources.Lookup(row.Definition.Command)
		if !ok || definition.Command != row.Definition.Command || seen[definition.Command] {
			return nil, domain.ErrPlanStale
		}
		seen[definition.Command] = true
		description, err := describeOwnerOrphanDefinition(definition, cli, logicalRepositoryRoot)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(description, row.Definition) {
			return nil, domain.ErrPlanStale
		}
		entry := orphanIngressEntry{definition: definition, contract: row.Contract, marker: row.Marker, shutdownEligible: row.ShutdownEligible, shutdownUnavailable: row.ShutdownUnavailable, shutdown: row.Shutdown.restore()}
		if row.DevicePresent {
			entry.device = map[string]string{}
		} else if len(row.Device) != 0 {
			return nil, domain.ErrPlanStale
		}
		if row.Device != nil && len(row.Device) == 0 {
			return nil, errors.New("noncanonical original orphan device rows")
		}
		for index, pair := range row.Device {
			if index > 0 && pair.Key <= row.Device[index-1].Key {
				return nil, errors.New("invalid original orphan device rows")
			}
			entry.device[pair.Key] = pair.Value
		}
		if err := validateOwnerOrphanEntry(entry, description); err != nil {
			return nil, err
		}
		plan.entries = append(plan.entries, entry)
	}
	return plan, nil
}
