package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
)

// MaterializationScope preserves the different consumers of status and runtime
// reconciliation. Runtime additionally owns selected instruction files.
type MaterializationScope string

const (
	MaterializationConfigAssets MaterializationScope = "config-assets"
	MaterializationRuntime      MaterializationScope = "runtime"
)

type MaterializationSource struct {
	Asset               MaterializedAsset
	Integration         string
	Label               string
	PreviewInstructions bool
}

// SelectMaterializationSources resolves operation-local artifact declarations.
func SelectMaterializationSources(values map[string]string, developer string, scope MaterializationScope, definitions ...profile.IntegrationDefinition) ([]MaterializationSource, error) {
	if scope != MaterializationConfigAssets && scope != MaterializationRuntime {
		return nil, errors.New("invalid materialization scope")
	}
	if values["ALLOWS_HOST_ACCESS"] == "false" {
		for _, definition := range definitions {
			for _, artifact := range definition.Artifacts {
				if artifact.SourceSetting != "" && strings.TrimSpace(values[artifact.SourceSetting]) != "" {
					return nil, fmt.Errorf("selected yard role forbids non-empty %s", artifact.SourceSetting)
				}
			}
		}
	}
	assets, err := MaterializedAssets(values, developer, definitions...)
	if err != nil {
		return nil, err
	}
	var instructions []MaterializationSource
	for _, definition := range definitions {
		for _, artifact := range definition.Artifacts {
			if artifact.Kind != "instructions" {
				continue
			}
			path, err := artifactDestination(developer, artifact.Destination)
			if err != nil {
				return nil, err
			}
			label := artifact.Label
			if label == "" {
				label = definition.Name + " instructions"
			}
			instructions = append(instructions, MaterializationSource{Integration: definition.Name, Label: label, PreviewInstructions: true, Asset: MaterializedAsset{Name: definition.Name + "." + artifact.ID, Setting: artifact.SourceSetting, Source: values[artifact.SourceSetting], Destination: path, FollowSymlinks: true}})
		}
	}
	result := []MaterializationSource{}
	if scope == MaterializationConfigAssets {
		for _, asset := range assets {
			integration, _, _ := strings.Cut(asset.Name, ".")
			result = append(result, MaterializationSource{Asset: asset, Integration: integration, Label: strings.Replace(asset.Name, ".", " ", 1)})
		}
		return result, nil
	}

	for _, integration := range strings.Fields(values["CODING_TOOL_INTEGRATIONS"]) {
		if scope == MaterializationRuntime && values["ALLOWS_CODING_TOOLS"] != "false" {
			for _, instruction := range instructions {
				if instruction.Integration == integration {
					result = append(result, instruction)
				}
			}
		}
		for _, asset := range assets {
			if strings.HasPrefix(asset.Name, integration+".") {
				result = append(result, MaterializationSource{Asset: asset, Integration: integration, Label: strings.Replace(asset.Name, ".", " ", 1)})
			}
		}
	}
	destinations := make([]string, 0, len(result))
	for _, source := range result {
		destinations = append(destinations, source.Asset.Destination)
	}
	if err := validateManagedDestinations(destinations, values["INTEGRATION_HOST_LINKS"], filepath.Join("/home", developer)); err != nil {
		return nil, err
	}
	return result, nil
}

type MaterializationContent struct {
	Present bool
	Payload []byte
}

var ErrMaterializationSourceMissing = errors.New("materialization source is missing")

// PrepareMaterializationContent consumes safely acquired bytes, preserving absent
// source policy and exact generated instruction bytes without reading sources.
func PrepareMaterializationContent(scope MaterializationScope, instructions bool, source MaterializationContent, preview []byte) ([]byte, bool, error) {
	if scope != MaterializationConfigAssets && scope != MaterializationRuntime {
		return nil, false, errors.New("invalid materialization scope")
	}
	if instructions && scope != MaterializationRuntime {
		return nil, false, errors.New("instruction materialization requires runtime scope")
	}
	if !source.Present && !instructions {
		if scope == MaterializationConfigAssets {
			return nil, false, ErrMaterializationSourceMissing
		}
		return nil, false, nil
	}
	payload := slices.Clone(source.Payload)
	if !source.Present {
		payload = nil
	}
	if !instructions {
		return payload, true, nil
	}
	start, end := []byte("<!-- subyard-preview -->"), []byte("<!-- /subyard-preview -->")
	if begin := bytes.Index(payload, start); begin >= 0 {
		finish := bytes.Index(payload[begin:], end)
		if finish < 0 || bytes.Count(payload, start) != 1 || bytes.Count(payload, end) != 1 {
			return nil, false, errors.New("ambiguous static preview instruction block")
		}
		finish += begin + len(end)
		updated := append([]byte(nil), payload[:begin]...)
		updated = append(updated, bytes.TrimSuffix(preview, []byte("\n"))...)
		return append(updated, payload[finish:]...), true, nil
	}
	if len(payload) != 0 {
		payload = append(payload, '\n', '\n')
	}
	return append(payload, preview...), true, nil
}

// DesiredMaterializedArtifact contains logical consumer identity and a digest
// supplied by the existing raw/JSON/TOML producer. It contains no source binding.
type DesiredMaterializedArtifact struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Scope       string `json:"scope,omitempty"`
	Role        string `json:"role,omitempty"`
	DesiredHash string `json:"desired_hash"`
	OwnedFormat string `json:"-"`
}

type MaterializationObservation struct {
	ExitCode       int
	Output         []byte
	Fingerprint    string
	OwnedConverged bool
}

type MaterializedArtifactSnapshot struct {
	Name        string `json:"name"`
	GuestExit   int    `json:"guest_exit"`
	GuestHash   string `json:"guest_hash,omitempty"`
	GuestOutput string `json:"guest_output,omitempty"`
}

type MaterializedArtifactAssessment struct {
	Snapshot  MaterializedArtifactSnapshot
	Converged bool
}

// AssessMaterializedArtifact compares native facts with one desired contract.
// Structured facts have already passed the owning JSON/TOML protocol decoder.
func AssessMaterializedArtifact(desired DesiredMaterializedArtifact, observation MaterializationObservation) (MaterializedArtifactAssessment, error) {
	snapshot := MaterializedArtifactSnapshot{Name: desired.Name}
	if desired.OwnedFormat != "" {
		if desired.OwnedFormat != "json" && desired.OwnedFormat != "toml" {
			return MaterializedArtifactAssessment{}, errors.New("unsupported materialization format")
		}
		snapshot.GuestOutput, snapshot.GuestHash = "owned-"+desired.OwnedFormat, observation.Fingerprint
		return MaterializedArtifactAssessment{Snapshot: snapshot, Converged: observation.OwnedConverged}, nil
	}
	snapshot.GuestExit = observation.ExitCode
	if observation.ExitCode != 0 {
		snapshot.GuestOutput = "unavailable"
		return MaterializedArtifactAssessment{Snapshot: snapshot}, nil
	}
	fields := strings.Fields(string(observation.Output))
	if len(fields) == 0 {
		snapshot.GuestOutput = "malformed"
		return MaterializedArtifactAssessment{Snapshot: snapshot}, nil
	}
	snapshot.GuestOutput, snapshot.GuestHash = "sha256", fields[0]
	return MaterializedArtifactAssessment{Snapshot: snapshot, Converged: fields[0] == desired.DesiredHash}, nil
}

// Declaration order and omission rules of these snapshots are a persisted
// fingerprint contract. Physical source paths do not participate.
type MaterializationTarget struct {
	Name             string                        `json:"name"`
	AccessKind       domain.AccessKind             `json:"access_kind"`
	YardKind         domain.YardKind               `json:"yard_kind"`
	IncusProject     string                        `json:"incus_project"`
	YardInstanceName string                        `json:"yard_instance_name"`
	DevUser          string                        `json:"dev_user"`
	DevUID           int                           `json:"dev_uid"`
	Assets           []DesiredMaterializedArtifact `json:"assets,omitempty"`
}

type materializedTargetSnapshot struct {
	State  string                         `json:"state"`
	Assets []MaterializedArtifactSnapshot `json:"assets,omitempty"`
}

type MaterializationAssessment struct {
	State                   string
	Changed                 bool
	DesiredFingerprint      string
	MaterializedFingerprint string
}

type materializationActivationFingerprint struct {
	Name         string `json:"name"`
	Fingerprint  string `json:"fingerprint"`
	State        string `json:"state,omitempty"`
	Integrations string `json:"integrations,omitempty"`
}

// These closed snapshots contain only strings, integers and slices. Keep the
// declaration order and nil slices: their JSON bytes are persisted identities.
func materializationDigest[T interface {
	MaterializationTarget | materializedTargetSnapshot | []string | []materializationActivationFingerprint
}](value T) string {
	payload, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("invalid materialization snapshot: %v", err))
	}
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

// AssessMaterialization creates the same target fingerprints used by config
// status/apply and release activation from explicit desired artifacts and facts.
func AssessMaterialization(target MaterializationTarget, state string, check bool, facts []MaterializedArtifactAssessment) (MaterializationAssessment, error) {
	target.Assets = slices.Clone(target.Assets)
	sort.Slice(target.Assets, func(i, j int) bool { return target.Assets[i].Name < target.Assets[j].Name })
	observed := materializedTargetSnapshot{State: state}
	changed := false
	if target.AccessKind == domain.AccessRemote {
		target.Assets = nil
		observed.State = "remote (settings only; consumers not checked)"
	} else {
		if !check {
			target.Assets = nil
		}
		switch state {
		case "absent", "stopped":
		case "running":
			if check {
				if len(facts) != len(target.Assets) {
					return MaterializationAssessment{}, errors.New("materialization observations do not match desired assets")
				}
				facts = slices.Clone(facts)
				sort.Slice(facts, func(i, j int) bool { return facts[i].Snapshot.Name < facts[j].Snapshot.Name })
				for i, asset := range target.Assets {
					fact := facts[i]
					if fact.Snapshot.Name != asset.Name {
						return MaterializationAssessment{}, errors.New("materialization observation is missing")
					}
					observed.Assets = append(observed.Assets, fact.Snapshot)
					changed = changed || !fact.Converged
				}
				observed.State = "converged"
				if changed {
					observed.State = "drift"
				}
			}
		default:
			return MaterializationAssessment{}, errors.New("invalid materialization target state")
		}
	}
	return MaterializationAssessment{State: observed.State, Changed: changed, DesiredFingerprint: materializationDigest(target), MaterializedFingerprint: materializationDigest(observed)}, nil
}

type MaterializationActivationTarget struct {
	Name                   string
	Assessment             MaterializationAssessment
	IntegrationScope       string
	IntegrationFingerprint string
	IntegrationChanged     bool
}

type MaterializationActivationAssessment struct {
	Actual, Desired string
	Converged       bool
}

// AssessMaterializationActivation preserves release target ordering and exact
// composition bytes. Converged actual identity deliberately aliases desired.
func AssessMaterializationActivation(targets []MaterializationActivationTarget) MaterializationActivationAssessment {
	desired, actual := make([]materializationActivationFingerprint, 0, len(targets)), make([]materializationActivationFingerprint, 0, len(targets))
	converged := true
	for _, target := range targets {
		materialized := materializationDigest([]string{target.Assessment.MaterializedFingerprint, target.IntegrationFingerprint})
		desired = append(desired, materializationActivationFingerprint{Name: target.Name, Fingerprint: target.Assessment.DesiredFingerprint, Integrations: target.IntegrationScope})
		actual = append(actual, materializationActivationFingerprint{Name: target.Name, Fingerprint: materialized, State: target.Assessment.State})
		converged = converged && !target.Assessment.Changed && !target.IntegrationChanged
	}
	sort.Slice(desired, func(i, j int) bool { return desired[i].Name < desired[j].Name })
	sort.Slice(actual, func(i, j int) bool { return actual[i].Name < actual[j].Name })
	desiredDigest := materializationDigest(desired)
	actualDigest := desiredDigest
	if !converged {
		actualDigest = materializationDigest(actual)
	}
	return MaterializationActivationAssessment{Actual: actualDigest, Desired: desiredDigest, Converged: converged}
}
