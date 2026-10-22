package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/config"
	"io"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

type ownerChildCheck struct {
	Kind        string
	Yard        domain.Context
	Yards       []domain.Context
	Environment map[string]string
}

type ownerChildInput struct {
	Path      []byte
	Fact      []byte
	Projected bool
	Approved  []byte
}

type ownerChildrenRecord struct {
	Version            int
	Before             releaseowner.OwnerBefore
	Checks             []ownerCheckRecord
	Settings           string
	Inputs             []ownerChildInput
	Inits              []ownerInitChildRecord       `json:",omitempty"`
	Teardowns          []ownerTeardownChildRecord   `json:",omitempty"`
	Projects           []ports.PreparedImageProject `json:",omitempty"`
	TemporaryStop      *ports.PreparedTemporaryStop `json:",omitempty"`
	RegistrationSource []byte                       `json:",omitempty"`
	LegacyCheck        *ownerCheckRecord            `json:",omitempty"`
	LegacyInputs       []ownerChildInput            `json:",omitempty"`
	PeerScopes         [][]byte                     `json:",omitempty"`
	YardScopes         [][]string                   `json:",omitempty"`
}

type ownerSourceRecipeRecord struct {
	Header      []byte
	Blobs       []string
	Digest      string
	NilContents []bool
}
type ownerPeerScopeRecord struct {
	Yards []ownerInitContext
}
type ownerInitChildRecord struct {
	Kind       string                       `json:",omitempty"`
	Projection []byte                       `json:",omitempty"`
	Project    *ports.PreparedImageProject  `json:",omitempty"`
	Instance   *ports.PreparedOwnerInstance `json:",omitempty"`
	Volume     *ports.PreparedOwnerVolume   `json:",omitempty"`
	Yard       string
	Metadata   []byte
	Sources    ownerSourceRecipeRecord
	Power      *reconcileruntime.OwnerHostPowerRecord `json:",omitempty"`
	Start      *ownerLifecycleRecord                  `json:",omitempty"`
}
type preparedOwnerInitChild struct {
	sourceRaw  []byte
	projection *reconcileruntime.OwnerInitProjection
	loaded     config.Loaded
	execution  *initExecution
	sources    *reconcileruntime.PreparedInstalledRuntime
	runtime    reconcileruntime.Runtime
	power      *reconcileruntime.OwnerHostPowerRecord
	start      *ownerLifecycleRecord
	instance   *ports.PreparedOwnerInstance
	volume     *ports.PreparedOwnerVolume
}
type ownerTeardownChildRecord struct {
	Yard     string
	Metadata []byte
}
type preparedOwnerTeardownChild struct {
	loaded    config.Loaded
	execution *teardownExecution
}

type ownerRecipeFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

// The existing opaque native recipe grammar is shared with Rust. Only this
// component interprets the fixed owner-child metadata leaf.
type ownerChildrenWire struct {
	Version    int               `json:"version"`
	Owner      string            `json:"owner"`
	Source     string            `json:"source"`
	Runtime    string            `json:"runtime"`
	Resources  string            `json:"resources"`
	Launcher   string            `json:"launcher"`
	HookBefore string            `json:"hookBefore"`
	Leaves     []string          `json:"leaves"`
	Files      []ownerRecipeFile `json:"files"`
}

type preparedOwnerChildren struct {
	operation              *CLI
	before                 releaseowner.OwnerBefore
	wire                   ownerChildrenWire
	raw                    []byte
	checks                 map[string]ownerChildCheck
	baselines              map[string]*ownerInputBaseline
	inits                  map[string]preparedOwnerInitChild
	recreate               map[string]preparedOwnerInitChild
	removed                map[string]bool
	createdCurrentProject  *ports.NativeOwnerCreated
	createdLegacyProject   *ports.NativeOwnerCreated
	createdRecreateProject *ports.NativeOwnerCreated
	teardowns              map[string]preparedOwnerTeardownChild
	registrationSource     *testyardmigration.PreparedOwnerSource
	checkedSource          *testyardmigration.CheckedOwnerSource
}

func ownerSemanticBefore(before releaseowner.OwnerBefore) releaseowner.OwnerBefore {
	before.PreparedRecipe = ""
	return before
}

func (c *releaseOwnerComponents) prepareOwnerChildren(ctx context.Context, before releaseowner.OwnerBefore) (*preparedOwnerChildren, error) {
	return c.captureOwnerChildren(ctx, before, true)
}
func (c *releaseOwnerComponents) captureOwnerChildren(ctx context.Context, before releaseowner.OwnerBefore, children bool) (*preparedOwnerChildren, error) {
	before = ownerSemanticBefore(before)
	switch testyardmigration.State(before.State) {
	case testyardmigration.StateAbsent, testyardmigration.StateCurrent:
		return nil, nil
	case testyardmigration.StateLegacyFlatAdoptCurrent, testyardmigration.StateLegacyDirectoryAdoptCurrent:
	case testyardmigration.StateLegacyFlat, testyardmigration.StateLegacyDirectory, testyardmigration.StateLegacyDirectoryProjects, testyardmigration.StateLegacyDirectoryOverrides, testyardmigration.StateLegacyDirectoryState:
	default:
		return nil, errors.New("unknown owner registration child scope")
	}
	operation := c.ownerChildOperation()
	original := releasetransition.OwnerRegistrationObservation{State: releasetransition.OwnerRegistrationState(before.State), Registration: releasetransition.Fingerprint(before.Registration), Overrides: releasetransition.Fingerprint(before.Overrides), Controller: releasetransition.Fingerprint(before.Controller), SharedImages: before.SharedImages}
	draft, err := operation.prepareSourceActivationDraft(c.context.ConfigHome, releasetransition.V2SourceActivationView{Settings: c, OwnerRegistration: &original})
	if err != nil {
		return nil, err
	}
	loaded, err := draft.load(testyardmigration.CurrentYard)
	if err != nil {
		return nil, err
	}
	peers := []domain.Context{}
	for _, name := range draft.names {
		value, err := draft.load(name)
		if err != nil {
			return nil, err
		}
		if value.Context.AccessKind == domain.AccessLocal {
			peers = append(peers, value.Context)
		}
	}
	baseline, err := operation.captureOwnerInputs(loaded, "", peers)
	if err != nil {
		return nil, err
	}
	baseline.projected, err = prepareOwnerInputProjection(baseline, draft.inputs, c.root)
	if err != nil {
		return nil, err
	}
	environment := map[string]string{}
	for _, key := range []string{"MIN_DISK_GIB", "RESUME_MIN_DISK_GIB", "MIN_INCUS_VER"} {
		if value, ok := loaded.Environment[key]; ok {
			environment[key] = value
		}
	}
	check := ownerChildCheck{Kind: "check", Yard: loaded.Context, Yards: peers, Environment: environment}
	descriptor, err := encodeOwnerCheck(c.ownerCheckLabel(check))
	if err != nil {
		return nil, err
	}
	settings, err := c.ownerSettingsBinding(loaded.Settings)
	if err != nil {
		return nil, err
	}
	record := ownerChildrenRecord{Version: 1, Before: before, Checks: []ownerCheckRecord{descriptor}, Settings: settings}
	for path, fact := range baseline.files {
		encoded, err := encodeOwnerFact(fact)
		if err != nil {
			return nil, err
		}
		input := ownerChildInput{Path: []byte(c.ownerPathLabel(path)), Fact: encoded}
		if payload, ok := baseline.projected.approved[path]; ok {
			input.Projected = true
			input.Approved = bytes.Clone(payload)
		}
		record.Inputs = append(record.Inputs, input)
	}
	slices.SortFunc(record.Inputs, func(a, b ownerChildInput) int { return bytes.Compare(a.Path, b.Path) })
	metadata, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	p := &preparedOwnerChildren{operation: operation, before: before, checks: map[string]ownerChildCheck{testyardmigration.CurrentYard: check}, baselines: map[string]*ownerInputBaseline{testyardmigration.CurrentYard: baseline}, inits: map[string]preparedOwnerInitChild{}, recreate: map[string]preparedOwnerInitChild{}, removed: map[string]bool{}, teardowns: map[string]preparedOwnerTeardownChild{}}
	p.wire = ownerChildrenWire{Version: 1, Owner: "owner-registration", Source: nativeValueDigest(ownerChildrenSemantic(record)), Runtime: nativeValueDigest(struct{ Session, Artifact, Repository string }{c.context.Session, c.context.Artifact, c.context.RepositoryRoot}), Resources: nativeValueDigest(record.Checks), Files: []ownerRecipeFile{{"prepared/owner-children.json", metadata}}}
	adopt := before.State == string(testyardmigration.StateLegacyFlatAdoptCurrent) || before.State == string(testyardmigration.StateLegacyDirectoryAdoptCurrent)
	if !adopt && children {
		port, ok := c.cli.ownerRegistrationTransition(c.request()).(*ownerRegistrationTransition)
		if !ok {
			return nil, domain.ErrPlanStale
		}
		options, err := port.options()
		if err != nil {
			return nil, err
		}
		source, err := testyardmigration.CaptureOwnerChildSource(options, ownerMigrationBefore(before), c.root)
		if err != nil {
			return nil, err
		}
		p.registrationSource = &source
		record.RegistrationSource, err = nativejson.EncodeLosslessNativeJSON(source, 32<<20)
		if err != nil {
			return nil, err
		}
		originalPeers := []domain.Context{}
		for _, name := range c.yards {
			value, err := c.load(name)
			if err != nil {
				return nil, err
			}
			if value.Context.AccessKind == domain.AccessLocal {
				originalPeers = append(originalPeers, value.Context)
			}
		}
		legacyLoaded, err := c.load(testyardmigration.LegacyYard)
		if err != nil {
			return nil, err
		}
		legacyBaseline, err := operation.captureOwnerInputs(legacyLoaded, "", originalPeers)
		if err != nil {
			return nil, err
		}
		legacyCheck := ownerChildCheck{Kind: "check", Yard: legacyLoaded.Context, Yards: originalPeers, Environment: map[string]string{}}
		for _, key := range []string{"MIN_DISK_GIB", "RESUME_MIN_DISK_GIB", "MIN_INCUS_VER"} {
			if value, ok := legacyLoaded.Environment[key]; ok {
				legacyCheck.Environment[key] = value
			}
		}
		legacyDescription, err := encodeOwnerCheck(c.ownerCheckLabel(legacyCheck))
		if err != nil {
			return nil, err
		}
		record.LegacyCheck = &legacyDescription
		p.checks[testyardmigration.LegacyYard] = legacyCheck
		p.baselines[testyardmigration.LegacyYard] = legacyBaseline
		for path, fact := range legacyBaseline.files {
			encoded, err := encodeOwnerFact(fact)
			if err != nil {
				return nil, err
			}
			record.LegacyInputs = append(record.LegacyInputs, ownerChildInput{Path: []byte(c.ownerPathLabel(path)), Fact: encoded})
		}
		slices.SortFunc(record.LegacyInputs, func(a, b ownerChildInput) int { return bytes.Compare(a.Path, b.Path) })
		intermediate := slices.Clone(originalPeers)
		for _, peer := range peers {
			i := slices.IndexFunc(intermediate, func(value domain.Context) bool { return value.YardName == peer.YardName })
			if i < 0 {
				intermediate = append(intermediate, peer)
			} else if !reflect.DeepEqual(intermediate[i], peer) {
				return nil, domain.ErrPlanStale
			}
		}
		slices.SortFunc(intermediate, func(a, b domain.Context) int { return strings.Compare(a.YardName, b.YardName) })
		for _, scope := range [][]domain.Context{originalPeers, intermediate, peers} {
			labeled := c.ownerCheckLabel(ownerChildCheck{Yards: scope}).Yards
			var values []ownerInitContext
			for _, peer := range labeled {
				values = append(values, ownerInitContext(peer))
			}
			raw, err := nativejson.EncodeLosslessNativeJSON(ownerPeerScopeRecord{Yards: values}, 4<<20)
			if err != nil {
				return nil, err
			}
			record.PeerScopes = append(record.PeerScopes, raw)
		}
		intermediateNames := append(slices.Clone(c.yards), draft.names...)
		slices.Sort(intermediateNames)
		intermediateNames = slices.Compact(intermediateNames)
		record.YardScopes = [][]string{slices.Clone(c.yards), intermediateNames, slices.Clone(draft.names)}
		if err := c.captureOwnerNativeResources(ctx, &record); err != nil {
			return nil, err
		}
		var recreate *ownerInitChildRecord
		for _, yard := range []string{testyardmigration.CurrentYard, testyardmigration.LegacyYard} {
			childLoaded := loaded
			if yard == testyardmigration.LegacyYard {
				childLoaded, err = c.load(yard)
				if err != nil {
					return nil, err
				}
			}
			childPeers := peers
			if yard == testyardmigration.LegacyYard {
				childPeers = originalPeers
			}
			teardownLoaded := c.ownerLabeledLoaded(childLoaded)
			teardown, err := prepareTeardownExecution([]string{"--yes"})
			if err != nil {
				return nil, err
			}
			if err := operation.observeTeardownExecution(ctx, teardownLoaded, teardown); err != nil {
				return nil, err
			}
			body, err := captureOwnerTeardownRecord(teardown)
			if err != nil {
				return nil, err
			}
			record.Teardowns = append(record.Teardowns, ownerTeardownChildRecord{yard, body})
			p.teardowns[yard] = preparedOwnerTeardownChild{teardownLoaded, teardown}
			execution, sources, err := c.prepareOwnerInit(ctx, operation, childLoaded, childPeers)
			if err != nil {
				return nil, fmt.Errorf("prepare original %s init: %w", yard, err)
			}
			metadata, err := captureOwnerInitRecord(execution)
			if err != nil {
				return nil, fmt.Errorf("record original %s init: %w", yard, err)
			}
			sourceRecord, err := p.addSourceRecipe(sources.Recipe())
			if err != nil {
				return nil, err
			}
			childRuntime := execution.platform.(reconcileruntime.Runtime)
			projectConfig, err := childRuntime.RenderOwnerProjectConfig(ctx, sources)
			if err != nil {
				return nil, err
			}
			for i := range record.Projects {
				if record.Projects[i].Project == childLoaded.Context.IncusProject && record.Projects[i].Absent {
					record.Projects[i].InitialConfig = bytes.Clone(projectConfig)
				}
			}
			childRecord := ownerInitChildRecord{Yard: yard, Metadata: metadata, Sources: sourceRecord}
			scope := "current"
			if yard == testyardmigration.LegacyYard {
				scope = "legacy"
			}
			childRecord.Volume, err = childRuntime.PrepareOwnerVolume(ctx, sources, scope)
			if err != nil {
				return nil, err
			}
			_, originalInstanceErr := childRuntime.Incus.Instance(ctx, childLoaded.Context.IncusProject, childLoaded.Context.YardInstanceName)
			if errors.Is(originalInstanceErr, ports.ErrInstanceNotFound) {
				scope := "current"
				if yard == testyardmigration.LegacyYard {
					scope = "legacy"
				}
				childRecord.Instance, err = childRuntime.PrepareOwnerInstancePolicy(ctx, sources, scope)
				if err != nil {
					return nil, err
				}
			} else if originalInstanceErr != nil {
				return nil, originalInstanceErr
			}
			if yard == testyardmigration.LegacyYard {
				incus, _ := operation.statusPorts()
				_, instanceErr := incus.Instance(ctx, childLoaded.Context.IncusProject, childLoaded.Context.YardInstanceName)
				if instanceErr == nil {
					power, err := childRuntime.PrepareHostPowerRuntime(ctx)
					if err != nil {
						return nil, err
					}
					powerRecord, recordErr := childRuntime.RecordOwnerHostPower(power)
					closeErr := power.Close()
					if recordErr != nil || closeErr != nil {
						return nil, errors.Join(recordErr, closeErr)
					}
					childRecord.Power = &powerRecord
				} else if !errors.Is(instanceErr, ports.ErrInstanceNotFound) {
					return nil, instanceErr
				}
				if record.TemporaryStop != nil {
					childRecord.Start, err = captureOwnerStart(ctx, operation, childLoaded)
					if err != nil {
						return nil, err
					}
				}
			}
			record.Inits = append(record.Inits, childRecord)
			p.inits[yard] = preparedOwnerInitChild{sourceRaw: bytes.Clone(sources.Recipe()), loaded: childLoaded, execution: execution, sources: sources, runtime: childRuntime, power: childRecord.Power, start: childRecord.Start, instance: childRecord.Instance, volume: childRecord.Volume}
			if yard == testyardmigration.LegacyYard {
				projection, err := childRuntime.PrepareOwnerInitProjection(ctx, teardown.snapshot().State, teardown.snapshot().Resources)
				if err != nil {
					return nil, fmt.Errorf("project original legacy removal: %w", err)
				}
				view, err := childRuntime.OwnerInitPlanningView(projection)
				if err != nil {
					return nil, err
				}
				future, futureSources, err := c.prepareOwnerInitView(ctx, operation, childLoaded, childPeers, &view)
				if err != nil {
					return nil, fmt.Errorf("prepare original legacy recreation: %w", err)
				}
				metadata, err := captureOwnerInitRecord(future)
				if err != nil {
					return nil, err
				}
				sourceRecord, err := p.addSourceRecipe(futureSources.Recipe())
				if err != nil {
					return nil, err
				}
				projectionRaw, err := projection.Record()
				if err != nil {
					return nil, err
				}
				futureRuntime := future.platform.(reconcileruntime.Runtime)
				futureProjectConfig, err := futureRuntime.RenderOwnerProjectConfig(ctx, futureSources)
				if err != nil {
					return nil, err
				}
				futureInstance, err := childRuntime.PrepareOwnerInstancePolicy(ctx, futureSources, "recreate")
				if err != nil {
					return nil, err
				}
				futureVolume, err := futureRuntime.PrepareOwnerVolume(ctx, futureSources, "recreate")
				if err != nil {
					return nil, err
				}
				recreate = &ownerInitChildRecord{Volume: futureVolume, Instance: futureInstance, Kind: "recreate", Projection: projectionRaw, Project: &ports.PreparedImageProject{Project: childLoaded.Context.IncusProject, Absent: true, SharedImages: before.SharedImages, InitialConfig: futureProjectConfig, Server: record.Projects[1].Server}, Yard: yard, Metadata: metadata, Sources: sourceRecord}
				p.recreate[yard] = preparedOwnerInitChild{sourceRaw: bytes.Clone(futureSources.Recipe()), projection: &projection, loaded: childLoaded, execution: future, sources: futureSources, runtime: childRuntime, instance: futureInstance, volume: futureVolume}
			}
		}
		if recreate == nil {
			return nil, domain.ErrPlanStale
		}
		record.Inits = append(record.Inits, *recreate)
		metadata, err = json.Marshal(record)
		if err != nil {
			return nil, err
		}
		p.wire.Files[0].Content = metadata
		p.wire.Source = nativeValueDigest(ownerChildrenSemantic(record))
		slices.SortFunc(p.wire.Files, func(a, b ownerRecipeFile) int { return strings.Compare(a.Path, b.Path) })
	}
	p.raw, err = json.Marshal(p.wire)
	if err != nil {
		return nil, err
	}
	var payloadBytes int
	if len(p.wire.Files) > 8192 || len(p.raw) > reconcileruntime.MaxInstalledRecipe {
		return nil, errors.New("owner child recipe capacity")
	}
	for _, file := range p.wire.Files {
		payloadBytes += len(file.Content)
		if len(file.Content) > 32<<20 || payloadBytes > 64<<20 {
			return nil, errors.New("owner child payload capacity")
		}
	}
	c.closers = append(c.closers, p)
	return p, nil
}

func (p *preparedOwnerChildren) Close() error {
	var result error
	if p.checkedSource != nil {
		result = errors.Join(result, p.checkedSource.Close())
	}
	for _, child := range p.inits {
		if child.sources != nil {
			result = errors.Join(result, child.sources.Close())
		}
	}
	for _, child := range p.recreate {
		if child.sources != nil {
			result = errors.Join(result, child.sources.Close())
		}
	}
	return result
}

func (p *preparedOwnerChildren) addSourceRecipe(raw []byte) (ownerSourceRecipeRecord, error) {
	var wire ownerChildrenWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return ownerSourceRecipeRecord{}, err
	}
	r := ownerSourceRecipeRecord{Digest: nativeDigest(raw)}
	for i, file := range wire.Files {
		r.NilContents = append(r.NilContents, file.Content == nil)
		name := "prepared/owner-blobs/" + nativeDigest(file.Content)
		if !slices.ContainsFunc(p.wire.Files, func(existing ownerRecipeFile) bool { return existing.Path == name }) {
			p.wire.Files = append(p.wire.Files, ownerRecipeFile{name, file.Content})
		}
		r.Blobs = append(r.Blobs, name)
		wire.Files[i].Content = nil
	}
	var err error
	r.Header, err = json.Marshal(wire)
	return r, err
}
func (p *preparedOwnerChildren) sourceRecipe(r ownerSourceRecipeRecord) ([]byte, error) {
	var wire ownerChildrenWire
	if err := json.Unmarshal(r.Header, &wire); err != nil {
		return nil, err
	}
	if len(wire.Files) != len(r.Blobs) || len(wire.Files) != len(r.NilContents) {
		return nil, domain.ErrPlanStale
	}
	for i, name := range r.Blobs {
		index := slices.IndexFunc(p.wire.Files, func(file ownerRecipeFile) bool { return file.Path == name })
		if index < 0 || name != "prepared/owner-blobs/"+nativeDigest(p.wire.Files[index].Content) {
			return nil, domain.ErrPlanStale
		}
		wire.Files[i].Content = p.wire.Files[index].Content
		if r.NilContents[i] {
			wire.Files[i].Content = nil
		} else if wire.Files[i].Content == nil {
			wire.Files[i].Content = []byte{}
		}
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	if nativeDigest(raw) != r.Digest {
		return nil, domain.ErrPlanStale
	}
	return raw, nil
}

func (p *preparedOwnerChildren) binding() string { return nativeDigest(p.raw) }

func (c *releaseOwnerComponents) bindOwnerChildren(ctx context.Context, hash string, raw []byte) error {
	return c.bindOwnerChildrenForApply(ctx, hash, raw, "")
}

func ownerMigrationBefore(before releaseowner.OwnerBefore) testyardmigration.Prepared {
	return testyardmigration.Prepared{State: testyardmigration.State(before.State), RegistrationDigest: before.Registration, OverridesDigest: before.Overrides, ControllerDigest: before.Controller, SharedImages: before.SharedImages}
}

func ownerInitChildSlot(child ownerInitChildRecord, index int) bool {
	if index < 2 {
		return child.Yard == []string{testyardmigration.CurrentYard, testyardmigration.LegacyYard}[index] && child.Kind == "" && len(child.Projection) == 0 && child.Project == nil
	}
	return index == 2 && child.Yard == testyardmigration.LegacyYard && child.Kind == "recreate" && len(child.Projection) > 0 && child.Power == nil && child.Start == nil && child.Project != nil && child.Project.Project == "subyard-"+testyardmigration.LegacyYard && child.Project.Absent && child.Project.Config == nil
}

func (p *preparedOwnerChildren) originalInitChild(yard string) (preparedOwnerInitChild, error) {
	child, ok := p.inits[yard]
	if p.removed[yard] {
		child, ok = p.recreate[yard]
		if child.projection == nil {
			return preparedOwnerInitChild{}, domain.ErrPlanStale
		}
	}
	if !ok || child.execution == nil || len(child.sourceRaw) == 0 {
		return preparedOwnerInitChild{}, domain.ErrPlanStale
	}
	return child, nil
}

func (c *releaseOwnerComponents) bindOwnerChildrenForApply(ctx context.Context, hash string, raw []byte, token string) error {
	if nativeDigest(raw) != hash {
		return domain.ErrPlanStale
	}
	var wire ownerChildrenWire
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	canonical, _ := json.Marshal(wire)
	metadataIndex := slices.IndexFunc(wire.Files, func(file ownerRecipeFile) bool { return file.Path == "prepared/owner-children.json" })
	if !bytes.Equal(canonical, raw) || metadataIndex < 0 || wire.Owner != "owner-registration" || wire.Version != 1 || wire.Launcher != "" || wire.HookBefore != "" || len(wire.Leaves) != 0 {
		return domain.ErrPlanStale
	}
	// Current preparation is only a guard against original source/scope drift.
	// The immutable original bytes remain the incoming bundle authority.
	var original ownerChildrenRecord
	if err := json.Unmarshal(wire.Files[metadataIndex].Content, &original); err != nil {
		return err
	}
	metadata, _ := json.Marshal(original)
	if !bytes.Equal(metadata, wire.Files[metadataIndex].Content) || original.Before.PreparedRecipe != "" || len(original.Inputs) > 4096 {
		return domain.ErrPlanStale
	}
	if c.ownerChildren != nil && original.Before != c.ownerChildren.before {
		return domain.ErrPlanStale
	}
	adopt := original.Before.State == string(testyardmigration.StateLegacyFlatAdoptCurrent) || original.Before.State == string(testyardmigration.StateLegacyDirectoryAdoptCurrent)
	var registration *testyardmigration.PreparedOwnerSource
	if !adopt {
		var source testyardmigration.PreparedOwnerSource
		if err := nativejson.DecodeLosslessNativeJSON(original.RegistrationSource, &source, 32<<20); err != nil {
			return err
		}
		if source.Before != ownerMigrationBefore(original.Before) {
			return domain.ErrPlanStale
		}
		registration = &source
		if len(original.Inits) != 3 || len(original.Teardowns) != 2 {
			return domain.ErrPlanStale
		}
		for i, child := range original.Inits {
			if !ownerInitChildSlot(child, i) {
				return domain.ErrPlanStale
			}
			if child.Project != nil && child.Project.SharedImages != original.Before.SharedImages {
				return domain.ErrPlanStale
			}
			var metadata ownerInitRecord
			if err := nativejson.DecodeLosslessNativeJSON(child.Metadata, &metadata, 32<<20); err != nil {
				return err
			}
			input := metadata.Input.Context
			if metadata.Version != 1 || input.YardName != child.Yard || input.AccessKind != domain.AccessLocal || input.Paths.ConfigHome != c.context.ConfigHome || input.Paths.DataHome != c.context.DataHome || input.Paths.RepositoryRoot != c.context.RepositoryRoot || input.Paths.ConfigDir != filepath.Join(c.context.RepositoryRoot, "config") {
				return domain.ErrPlanStale
			}
		}
		// Binding restores only original durable bytes. The original token and
		// borrowed mutation scope arrive at ApplyOwner, where every live source
		// and native guard is checked before these handles may be used.
		if token == "" {
			inert := &preparedOwnerChildren{operation: c.ownerChildOperation(), before: original.Before, raw: bytes.Clone(raw), wire: wire, registrationSource: registration}
			var total int
			previous := ""
			if len(wire.Files) > 8192 || len(raw) > reconcileruntime.MaxInstalledRecipe || wire.Source != nativeValueDigest(ownerChildrenSemantic(original)) {
				return domain.ErrPlanStale
			}
			for _, file := range wire.Files {
				total += len(file.Content)
				if file.Path <= previous || len(file.Content) > 32<<20 || total > 64<<20 {
					return domain.ErrPlanStale
				}
				previous = file.Path
			}
			for _, child := range original.Inits {
				if _, err := inert.sourceRecipe(child.Sources); err != nil {
					return err
				}
			}
			c.ownerChildren = inert
			if c.installedBound == nil {
				c.installedBound = map[string]bool{}
			}
			c.installedBound[hash] = true
			return nil
		}
	}
	var checked *testyardmigration.CheckedOwnerSource
	if registration != nil {
		port, ok := c.cli.ownerRegistrationTransition(c.request()).(*ownerRegistrationTransition)
		if !ok {
			return domain.ErrPlanStale
		}
		options, err := port.options()
		if err != nil {
			return err
		}
		options.RecoveryToken = token
		checked, err = testyardmigration.CheckOwnerChildSource(options, ownerMigrationBefore(original.Before), *registration, c.root)
		if err != nil {
			observer, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeOwnerRestorationObserver)
			if !ok {
				return err
			}
			member, readErr := observer.ReadOwnerRestoration(token, nativeDigest(original.RegistrationSource))
			if readErr != nil {
				return errors.Join(err, readErr)
			}
			checked, err = testyardmigration.CheckRestoredOwnerChildSource(options, ownerMigrationBefore(original.Before), *registration, c.root, member)
			if err != nil {
				return err
			}
		}
		defer func() {
			if checked != nil {
				checked.Close()
			}
		}()
	}
	current, err := c.captureOwnerChildren(ctx, original.Before, false)
	if err != nil {
		return err
	}
	if current == nil {
		return domain.ErrPlanStale
	}
	var observed ownerChildrenRecord
	if err := json.Unmarshal(current.wire.Files[0].Content, &observed); err != nil {
		return err
	}
	semantic := ownerChildrenSemantic(original)
	semantic.Inits = nil
	semantic.Teardowns = nil
	semantic.Projects = nil
	semantic.TemporaryStop = nil
	semantic.RegistrationSource = nil
	semantic.LegacyCheck = nil
	semantic.LegacyInputs = nil
	semantic.PeerScopes = nil
	semantic.YardScopes = nil
	if !reflect.DeepEqual(semantic, ownerChildrenSemantic(observed)) {
		return fmt.Errorf("%w: original owner input scope changed", domain.ErrPlanStale)
	}
	expected := current.wire
	expected.Source = nativeValueDigest(ownerChildrenSemantic(original))
	expected.Files = wire.Files
	wanted, _ := json.Marshal(expected)
	if !bytes.Equal(wanted, raw) {
		return fmt.Errorf("%w: original owner source header changed", domain.ErrPlanStale)
	}
	baseline := current.baselines[testyardmigration.CurrentYard]
	for _, input := range original.Inputs {
		path := c.ownerPathPhysical(string(input.Path))
		if _, ok := baseline.files[path]; !ok {
			return domain.ErrPlanStale
		}
		fact, err := decodeOwnerFact(input.Fact)
		if err != nil {
			return err
		}
		baseline.files[path] = fact
	}
	current.raw, current.wire = bytes.Clone(raw), wire
	current.registrationSource = registration
	current.checkedSource = checked
	checked = nil
	if registration != nil {
		if original.LegacyCheck == nil || len(original.PeerScopes) != 3 || len(original.YardScopes) != 3 || len(original.LegacyInputs) > 4096 {
			return domain.ErrPlanStale
		}
		var scopes [3][]domain.Context
		for i, raw := range original.PeerScopes {
			var scope ownerPeerScopeRecord
			if err := nativejson.DecodeLosslessNativeJSON(raw, &scope, 4<<20); err != nil {
				return err
			}
			for _, value := range scope.Yards {
				peer := domain.Context(value)
				peer.Paths.RepositoryRoot = c.ownerPathPhysical(peer.Paths.RepositoryRoot)
				peer.Paths.ConfigDir = c.ownerPathPhysical(peer.Paths.ConfigDir)
				scopes[i] = append(scopes[i], peer)
			}
		}
		port, _ := c.cli.ownerRegistrationTransition(c.request()).(*ownerRegistrationTransition)
		options, err := port.options()
		if err != nil {
			return err
		}
		options.RecoveryToken = token
		view := &preparedOwnerChildInputView{root: c.root, source: *registration, sourceDigest: nativeDigest(original.RegistrationSource), options: options, peers: scopes, current: current.checks[testyardmigration.CurrentYard]}
		for i, names := range original.YardScopes {
			if len(names) == 0 || len(names) > 128 || !slices.IsSorted(names) {
				return domain.ErrPlanStale
			}
			for j, name := range names {
				if !domain.SafeName(name) || j > 0 && names[j-1] == name {
					return domain.ErrPlanStale
				}
			}
			view.names[i] = slices.Clone(names)
		}
		baseline.ownerSource = view
		current.baselines[testyardmigration.LegacyYard] = &ownerInputBaseline{files: map[string]ownerInputFile{}, peers: scopes[0], runtimeRoot: c.cli.options.RepositoryRoot, ownerSource: view}
		for _, input := range original.LegacyInputs {
			if input.Projected || len(input.Approved) != 0 {
				return domain.ErrPlanStale
			}
			fact, err := decodeOwnerFact(input.Fact)
			if err != nil {
				return err
			}
			path := c.ownerPathPhysical(string(input.Path))
			if _, duplicate := current.baselines[testyardmigration.LegacyYard].files[path]; duplicate {
				return domain.ErrPlanStale
			}
			current.baselines[testyardmigration.LegacyYard].files[path] = fact
		}
	}
	if adopt && len(original.Inits) != 0 || !adopt && len(original.Inits) != 3 {
		return domain.ErrPlanStale
	}
	for i, child := range original.Inits {
		if !ownerInitChildSlot(child, i) {
			return domain.ErrPlanStale
		}
		var description ownerInitRecord
		if err := nativejson.DecodeLosslessNativeJSON(child.Metadata, &description, 32<<20); err != nil {
			return err
		}
		loaded, err := description.Input.restore(c.cli.catalog)
		if err != nil {
			return err
		}
		loaded = ownerLoadedLabels(loaded, c.context.RepositoryRoot, c.cli.options.RepositoryRoot)
		peers := current.checks[testyardmigration.CurrentYard].Yards
		if child.Yard == testyardmigration.LegacyYard {
			peers = current.baselines[child.Yard].peers
		}
		runtime, ok := c.platform(loaded, peers).(reconcileruntime.Runtime)
		if !ok {
			return domain.ErrPlanStale
		}
		sourceRaw, err := current.sourceRecipe(child.Sources)
		if err != nil {
			return err
		}
		execution, err := restoreOwnerInitRecord(child.Metadata, loaded, runtime)
		if err != nil {
			return fmt.Errorf("restore original %s init: %w", child.Yard, err)
		}
		runtime = execution.platform.(reconcileruntime.Runtime)
		// Check every original immutable source before the owning component's
		// first publication. The returned source handle is discarded; actual
		// branch execution binds its original payload again at that boundary.
		guard, err := runtime.BindInstalledRuntimeContext(ctx, sourceRaw)
		if err != nil {
			return fmt.Errorf("check original %s source: %w", child.Yard, err)
		}
		projectConfig, renderErr := runtime.RenderOwnerProjectConfig(ctx, guard)
		if renderErr != nil {
			return errors.Join(renderErr, guard.Close())
		}
		if child.Project != nil {
			if !bytes.Equal(child.Project.InitialConfig, projectConfig) {
				return errors.Join(domain.ErrPlanStale, guard.Close())
			}
		} else {
			for _, project := range original.Projects {
				if project.Project == runtime.Yard.IncusProject && project.Absent && !bytes.Equal(project.InitialConfig, projectConfig) {
					return errors.Join(domain.ErrPlanStale, guard.Close())
				}
			}
		}
		if child.Instance != nil {
			expectedScope := "current"
			if child.Yard == testyardmigration.LegacyYard {
				expectedScope = "legacy"
			}
			if child.Kind == "recreate" {
				expectedScope = "recreate"
			}
			if child.Instance.Scope != expectedScope {
				return errors.Join(domain.ErrPlanStale, guard.Close())
			}
			runtime, err = runtime.WithOwnerInstance(ctx, guard, *child.Instance)
			if err != nil {
				return errors.Join(fmt.Errorf("check original %s instance capsule: %w", child.Yard, err), guard.Close())
			}
			execution.platform = runtime
		}
		if child.Volume == nil || (child.Instance == nil) != (child.Volume.InstanceUUID != "") {
			return errors.Join(fmt.Errorf("%w: original %s create capsule set changed", domain.ErrPlanStale, child.Yard), guard.Close())
		}
		expectedVolumeScope := "current"
		if child.Yard == testyardmigration.LegacyYard {
			expectedVolumeScope = "legacy"
		}
		if child.Kind == "recreate" {
			expectedVolumeScope = "recreate"
		}
		if child.Volume.Scope != expectedVolumeScope {
			return errors.Join(domain.ErrPlanStale, guard.Close())
		}
		runtime, err = runtime.WithOwnerVolume(ctx, guard, *child.Volume)
		if err != nil {
			return errors.Join(fmt.Errorf("check original %s SRV capsule: %w", child.Yard, err), guard.Close())
		}
		var restoredResults []ports.NativeOwnerCreated
		if c.ownerResultReader != nil {
			restoredResults, err = c.checkedOwnerResults()
			if err != nil {
				return errors.Join(err, guard.Close())
			}
			runtime, err = runtime.WithOwnerCreatedResults(ctx, restoredResults)
			if err != nil {
				return errors.Join(err, guard.Close())
			}
		}
		knownVolume := false
		for _, result := range restoredResults {
			if result.Intent.Kind == "srv-volume" && result.Intent.Scope == expectedVolumeScope && result.Intent.Project == child.Volume.Project && result.Intent.Resource == child.Volume.Volume {
				knownVolume = true
			}
		}
		if child.Kind != "recreate" && !knownVolume {
			checker, ok := runtime.Incus.(interface {
				CheckOwnerVolumeGuard(context.Context, ports.PreparedOwnerVolume) error
			})
			if !ok {
				return errors.Join(domain.ErrPlanStale, guard.Close())
			}
			if err := checker.CheckOwnerVolumeGuard(ctx, *child.Volume); err != nil {
				return errors.Join(err, guard.Close())
			}
		}
		execution.platform = runtime
		if err := guard.Close(); err != nil {
			return err
		}
		if child.Kind == "recreate" {
			projection, err := runtime.RestoreOwnerInitProjection(child.Projection)
			if err != nil {
				return err
			}
			execution.inputBaseline = current.baselines[child.Yard]
			if execution.inputBaseline == nil {
				return domain.ErrPlanStale
			}
			current.recreate[child.Yard] = preparedOwnerInitChild{sourceRaw: bytes.Clone(sourceRaw), projection: &projection, loaded: loaded, execution: execution, runtime: runtime, instance: child.Instance, volume: child.Volume}
			continue
		}
		childBaseline := current.baselines[child.Yard]
		if childBaseline == nil {
			return domain.ErrPlanStale
		}
		if child.Yard == testyardmigration.LegacyYard {
			childBaseline.initial = loaded
			legacyCheck := ownerChildCheck{Kind: "check", Yard: loaded.Context, Yards: childBaseline.peers, Environment: map[string]string{}}
			for _, key := range []string{"MIN_DISK_GIB", "RESUME_MIN_DISK_GIB", "MIN_INCUS_VER"} {
				if value, ok := loaded.Environment[key]; ok {
					legacyCheck.Environment[key] = value
				}
			}
			description, err := encodeOwnerCheck(c.ownerCheckLabel(legacyCheck))
			if err != nil || !reflect.DeepEqual(description, *original.LegacyCheck) {
				return domain.ErrPlanStale
			}
			current.checks[child.Yard] = legacyCheck
		}
		execution.inputBaseline = childBaseline
		current.inits[child.Yard] = preparedOwnerInitChild{sourceRaw: bytes.Clone(sourceRaw), loaded: loaded, execution: execution, runtime: runtime, power: child.Power, start: child.Start, instance: child.Instance, volume: child.Volume}
	}
	if adopt && len(original.Teardowns) != 0 || !adopt && len(original.Teardowns) != 2 {
		return domain.ErrPlanStale
	}
	for i, child := range original.Teardowns {
		if child.Yard != []string{testyardmigration.CurrentYard, testyardmigration.LegacyYard}[i] {
			return domain.ErrPlanStale
		}
		loaded := c.ownerLabeledLoaded(current.inits[child.Yard].loaded)
		execution, err := restoreOwnerTeardownRecord(child.Metadata, loaded, current.operation)
		if err != nil {
			return fmt.Errorf("restore original %s teardown: %w", child.Yard, err)
		}
		if execution == nil {
			return domain.ErrPlanStale
		}
		current.teardowns[child.Yard] = preparedOwnerTeardownChild{loaded, execution}
	}
	c.ownerChildren = current
	if c.installedBound == nil {
		c.installedBound = map[string]bool{}
	}
	c.installedBound[hash] = true
	return nil
}

func (c *releaseOwnerComponents) ownerLabeledLoaded(loaded config.Loaded) config.Loaded {
	loaded.Context = c.ownerCheckLabel(ownerChildCheck{Yard: loaded.Context}).Yard
	return loaded
}

func (c *releaseOwnerComponents) applyOwnerChildren(ctx context.Context, before releaseowner.OwnerBefore, token string) error {
	p := c.ownerChildren
	if p == nil || before.PreparedRecipe != p.binding() || ownerSemanticBefore(before) != p.before || !c.installedBound[before.PreparedRecipe] {
		return domain.ErrPlanStale
	}
	if err := c.bindOwnerChildrenForApply(ctx, before.PreparedRecipe, p.raw, token); err != nil {
		return err
	}
	p = c.ownerChildren
	if c.ownerResultReader != nil {
		values, err := c.checkedOwnerResults()
		if err != nil {
			return err
		}
		for _, value := range values {
			if value.Intent.Kind == "project" {
				copy := value
				switch value.Intent.Scope {
				case "current":
					p.createdCurrentProject = &copy
				case "legacy":
					p.createdLegacyProject = &copy
				case "recreate":
					p.createdRecreateProject = &copy
				}
			}
		}
	}
	port, ok := c.cli.ownerRegistrationTransition(c.request()).(*ownerRegistrationTransition)
	if !ok {
		return domain.ErrPlanStale
	}
	options, err := port.options()
	if err != nil {
		return err
	}
	options.RecoveryToken = token
	var record ownerChildrenRecord
	for _, file := range p.wire.Files {
		if file.Path == "prepared/owner-children.json" {
			if err := json.Unmarshal(file.Content, &record); err != nil {
				return err
			}
		}
	}
	if len(record.Inits) != 0 {
		if err := c.ownerNativeOptions(record, &options, p); err != nil {
			return err
		}
		originalEnsure := options.EnsureImageProject
		options.EnsureImageProject = func(ctx context.Context, yard string, shared bool) error {
			if !p.removed[yard] {
				return originalEnsure(ctx, yard, shared)
			}
			if yard != testyardmigration.LegacyYard || len(record.Inits) != 3 || !ownerInitChildSlot(record.Inits[2], 2) || record.Inits[2].Project.SharedImages != shared {
				return domain.ErrPlanStale
			}
			incus, _ := c.cli.statusPorts()
			native, ok := incus.(ownerNativeResources)
			if !ok {
				return domain.ErrPlanStale
			}
			return p.applyOwnerImageProject(ctx, native, *record.Inits[2].Project, "recreate")
		}
		if p.registrationSource == nil {
			return domain.ErrPlanStale
		}
		observer, ok := ports.NativeOperationObserverFrom(ctx).(ports.NativeOwnerRestorationObserver)
		if !ok {
			return domain.ErrPlanStale
		}
		options.RecordRegistrationRestore = func(member testyardmigration.PreparedOwnerMember) error {
			if err := p.registrationSource.ValidateRestoredMember(member); err != nil {
				return err
			}
			return observer.RecordOwnerRestoration(token, nativeDigest(record.RegistrationSource), member)
		}
	}
	options.RunYard = func(ctx context.Context, yard string, output io.Writer, args ...string) error {
		baseline := p.baselines[yard]
		if baseline == nil {
			return domain.ErrPlanStale
		}
		if err := baseline.check(ctx, p.operation); err != nil {
			return err
		}
		if yard == testyardmigration.LegacyYard {
			if slices.Equal(args, []string{"_migrate", "reconcile-power-reconciler"}) {
				return p.runPower(ctx, yard)
			}
			if slices.Equal(args, []string{"start", "--yes"}) {
				return p.runStart(ctx, yard, output)
			}
			if slices.Equal(args, []string{"test-vms", "status"}) {
				return p.readBrokerStatus(ctx, yard, output)
			}
		}
		if slices.Equal(args, []string{"teardown", "--yes"}) {
			return p.runTeardown(ctx, yard, output)
		}
		if slices.Equal(args, []string{"init", "--yes"}) {
			child, err := p.originalInitChild(yard)
			if err != nil {
				return err
			}
			if p.removed[yard] {
				if child.projection == nil {
					return domain.ErrPlanStale
				}
				if p.createdRecreateProject != nil && yard == testyardmigration.LegacyYard {
					if len(record.Inits) != 3 || record.Inits[2].Project == nil || child.runtime.CheckOwnerInitCreatedProject(ctx, *child.projection, *record.Inits[2].Project, *p.createdRecreateProject) != nil {
						return domain.ErrPlanStale
					}
				} else if child.runtime.CheckOwnerInitRemoval(ctx, *child.projection) != nil {
					return domain.ErrPlanStale
				}
			}
			baseline := p.baselines[yard]
			if baseline == nil || yard == testyardmigration.CurrentYard && (baseline.projected == nil || baseline.projected.requirePublished() != nil) {
				return domain.ErrPlanStale
			}
			if err := baseline.check(ctx, p.operation); err != nil {
				return err
			}
			runtime := child.runtime
			sources, err := child.runtime.BindInstalledRuntimeContext(ctx, child.sourceRaw)
			if err != nil {
				return err
			}
			executionRuntime, err := runtime.OwnerSourceRuntime(ctx, sources)
			if err != nil {
				return errors.Join(err, sources.Close())
			}
			var project *ports.PreparedImageProject
			var projectResult *ports.NativeOwnerCreated
			scope := "current"
			if yard == testyardmigration.LegacyYard {
				scope = "legacy"
			}
			if p.removed[yard] {
				scope = "recreate"
				project = record.Inits[2].Project
				projectResult = p.createdRecreateProject
			} else {
				for i := range record.Projects {
					if record.Projects[i].Project == child.loaded.Context.IncusProject {
						project = &record.Projects[i]
					}
				}
				if scope == "current" {
					projectResult = p.createdCurrentProject
				} else {
					projectResult = p.createdLegacyProject
				}
			}
			if project == nil {
				return errors.Join(domain.ErrPlanStale, sources.Close())
			}
			executionRuntime, err = executionRuntime.WithOwnerYardProject(ctx, sources, *project, scope, projectResult)
			if err != nil {
				return errors.Join(err, sources.Close())
			}
			executionRuntime.RuntimePlan = child.execution.runtimePlan
			executionRuntime.HookPlan = child.execution.hookPlan
			child.execution.platform = executionRuntime
			child.execution.operationID = "owner-registration"
			approved := map[string][]byte{}
			if baseline.projected != nil {
				approved = baseline.projected.approved
			}
			for path, payload := range approved {
				if err := child.execution.integrationBaseline.acceptPublishedSource(path, payload); err != nil {
					return err
				}
			}
			err = child.execution.run(ctx, p.operation, output)
			if result := executionRuntime.OwnerCreatedProject(); result != nil {
				switch scope {
				case "current":
					p.createdCurrentProject = result
				case "legacy":
					p.createdLegacyProject = result
				case "recreate":
					p.createdRecreateProject = result
				}
			}
			return errors.Join(err, sources.FinishOwnerSources(err))
		}
		check, ok := p.checks[yard]
		if !ok || check.Kind != "check" || !slices.Equal(args, []string{"check"}) {
			return domain.ErrPlanStale
		}
		if baseline == nil || yard == testyardmigration.CurrentYard && (baseline.projected == nil || baseline.projected.requirePublished() != nil) {
			return domain.ErrPlanStale
		}
		if err := baseline.check(ctx, p.operation); err != nil {
			return err
		}
		incus, _ := c.cli.statusPorts()
		return (hostruntime.HostCheck{Yard: check.Yard, Yards: check.Yards, Environment: check.Environment, Incus: incus, Output: output}).Run(ctx, hostruntime.CheckOptions{})
	}
	return testyardmigration.Commit(ctx, options, testyardmigration.Prepared{State: testyardmigration.State(before.State), RegistrationDigest: before.Registration, OverridesDigest: before.Overrides, ControllerDigest: before.Controller, SharedImages: before.SharedImages})
}

func (c *releaseOwnerComponents) ownerPathLabel(path string) string {
	root := c.context.PhysicalRepositoryRoot
	if root == "" {
		root = c.cli.options.RepositoryRoot
	}
	if path == root {
		return c.context.RepositoryRoot
	}
	if strings.HasPrefix(path, root+string(filepath.Separator)) {
		return c.context.RepositoryRoot + strings.TrimPrefix(path, root)
	}
	return path
}
func (c *releaseOwnerComponents) ownerPathPhysical(path string) string {
	root := c.context.RepositoryRoot
	if path == root {
		return c.cli.options.RepositoryRoot
	}
	if strings.HasPrefix(path, root+string(filepath.Separator)) {
		return c.cli.options.RepositoryRoot + strings.TrimPrefix(path, root)
	}
	return path
}
func (c *releaseOwnerComponents) ownerCheckLabel(check ownerChildCheck) ownerChildCheck {
	label := func(y domain.Context) domain.Context {
		y.Paths.RepositoryRoot = c.ownerPathLabel(y.Paths.RepositoryRoot)
		y.Paths.ConfigDir = c.ownerPathLabel(y.Paths.ConfigDir)
		return y
	}
	check.Yard = label(check.Yard)
	check.Yards = slices.Clone(check.Yards)
	for i := range check.Yards {
		check.Yards[i] = label(check.Yards[i])
	}
	return check
}
func ownerChildrenSemantic(record ownerChildrenRecord) ownerChildrenRecord {
	record.Inputs = slices.Clone(record.Inputs)
	for i := range record.Inputs {
		if record.Inputs[i].Projected {
			record.Inputs[i].Fact = nil
		}
	}
	return record
}

func (c *releaseOwnerComponents) ownerChildOperation() *CLI {
	operation := c.cli.releaseActivationOperation()
	operation.env = maps.Clone(operation.env)
	operation.baseEnv = maps.Clone(operation.baseEnv)
	for _, environment := range []map[string]string{operation.env, operation.baseEnv} {
		delete(environment, "SUBYARD_CONFIG_LOADED")
		environment["SUBYARD_ENGINE_CONTEXT"] = "1"
		environment["SUBYARD_ENGINE_CONTEXT_SCHEMA"] = "1"
		environment["SUBYARD_INTERNAL_MIGRATION_CHILD"] = "1"
	}
	operation.options.DispatcherPath = filepath.Join(c.cli.options.RepositoryRoot, "bin", "yard-engine")
	operation.options.Stdin = strings.NewReader("")
	operation.releaseTransitionChild = true
	operation.configApplyRepair = nil
	operation.profileInitRepair = nil
	operation.integrationCleanupRepair = nil
	operation.resourceShutdownRepair = nil
	return operation
}

// Context and trace strings may contain legitimate non-UTF8 host paths. Keep
// those private owning values as typed binary bytes inside the opaque capsule;
// JSON is used only for the bounded outer byte table and public identifiers.
type ownerStringPair struct{ Name, Value []byte }
type ownerCheckRecord struct {
	Kind        string
	Yard        []byte
	Yards       [][]byte
	Environment []ownerStringPair
}

func encodeOwnerCheck(check ownerChildCheck) (ownerCheckRecord, error) {
	r := ownerCheckRecord{Kind: check.Kind}
	var err error
	r.Yard, err = json.Marshal(ownerContextDescription(check.Yard))
	if err != nil {
		return r, err
	}
	for _, yard := range check.Yards {
		body, err := json.Marshal(ownerContextDescription(yard))
		if err != nil {
			return r, err
		}
		r.Yards = append(r.Yards, body)
	}
	names := make([]string, 0, len(check.Environment))
	for name := range check.Environment {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		r.Environment = append(r.Environment, ownerStringPair{[]byte(name), []byte(check.Environment[name])})
	}
	return r, nil
}

type ownerContextRecord struct {
	Text                                   [][]byte
	SSHPort, DevUID                        int
	ForwardSSHAgent, DevSudo, NestedE2EVMs bool
}

func ownerContextDescription(y domain.Context) ownerContextRecord {
	values := []string{y.YardName, string(y.AccessKind), string(y.YardKind), y.YardInstanceName, y.IncusProject, y.IncusBridge, y.SSHHost, y.DevUser, y.OwnerEndpoint, y.OwnerYardName, string(y.YardImageRef), y.ShiftMode, y.Paths.RepositoryRoot, y.Paths.ConfigDir, y.Paths.OperatorHome, y.Paths.ConfigHome, y.Paths.DataHome, y.Paths.StoragePath, y.Paths.HostBase, y.Paths.StateDir}
	text := make([][]byte, len(values))
	for i, value := range values {
		text[i] = []byte(value)
	}
	return ownerContextRecord{text, y.SSHPort, y.DevUID, y.ForwardSSHAgent, y.DevSudo, y.NestedE2EVMs}
}

type ownerFactRecord struct {
	Fact ownerInputFile
	Link []byte
}

func encodeOwnerFact(fact ownerInputFile) ([]byte, error) {
	link := []byte(fact.Link)
	fact.Link = ""
	return json.Marshal(ownerFactRecord{fact, link})
}
func decodeOwnerFact(raw []byte) (ownerInputFile, error) {
	var r ownerFactRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		return ownerInputFile{}, err
	}
	canonical, _ := json.Marshal(r)
	if !bytes.Equal(canonical, raw) || r.Fact.Link != "" {
		return ownerInputFile{}, domain.ErrPlanStale
	}
	r.Fact.Link = string(r.Link)
	return r.Fact, nil
}

type ownerTraceRecord struct {
	Text, Aliases, Scopes [][]byte
	Syncable, Sensitive   bool
	Resolutions           []ownerResolutionRecord
}
type ownerResolutionRecord struct {
	Text [][]byte
	Line int
}

func ownerStrings(values []string) [][]byte {
	if values == nil {
		return nil
	}
	result := make([][]byte, len(values))
	for i, v := range values {
		result[i] = []byte(v)
	}
	return result
}
func (c *releaseOwnerComponents) ownerSettingsBinding(settings map[string]config.SettingTrace) (string, error) {
	names := make([]string, 0, len(settings))
	for name := range settings {
		names = append(names, name)
	}
	slices.Sort(names)
	rows := make([]ownerTraceRecord, 0, len(names))
	for _, name := range names {
		v := settings[name]
		effective := v.EffectiveValue
		if v.Kind == config.SettingFile || v.Type == config.SettingRegularFilePath {
			effective = c.ownerPathLabel(effective)
		}
		row := ownerTraceRecord{Text: ownerStrings([]string{name, effective, string(v.Kind), string(v.Type), v.Merge, string(v.Application), v.Owner}), Aliases: ownerStrings(v.Aliases), Syncable: v.Syncable, Sensitive: v.Sensitive}
		if v.Scopes != nil {
			row.Scopes = make([][]byte, len(v.Scopes))
			for i, s := range v.Scopes {
				row.Scopes[i] = []byte(s)
			}
		}
		if v.Resolutions != nil {
			row.Resolutions = make([]ownerResolutionRecord, len(v.Resolutions))
			for i, r := range v.Resolutions {
				row.Resolutions[i] = ownerResolutionRecord{ownerStrings([]string{r.Scope, r.Role, c.ownerPathLabel(r.Path), r.Value, r.Status, r.Detail}), r.Line}
			}
		}
		rows = append(rows, row)
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return "", err
	}
	return nativeDigest(raw), nil
}
