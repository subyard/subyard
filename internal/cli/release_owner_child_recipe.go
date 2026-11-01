package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/config"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
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
type ownerLifecycleChildMetadata struct {
	Input       ownerLoadedSnapshot
	Publication *reconcileruntime.OwnerPublicationRecord
}

type preparedOwnerInitChild struct {
	projection          *reconcileruntime.OwnerInitProjection
	loaded              config.Loaded
	execution           *initExecution
	sources             *reconcileruntime.PreparedInstalledRuntime
	runtime             reconcileruntime.Runtime
	power               *reconcileruntime.OwnerHostPowerRecord
	start               *ownerLifecycleRecord
	publicationConsumer *ownerpublication.Consumer
	instance            *ports.PreparedOwnerInstance
	volume              *ports.PreparedOwnerVolume
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
	publicationInstallation *reconcileruntime.OwnerPublicationInstallation
	operation               *CLI
	before                  releaseowner.OwnerBefore
	wire                    ownerChildrenWire
	raw                     []byte
	checks                  map[string]ownerChildCheck
	baselines               map[string]*ownerInputBaseline
	inits                   map[string]preparedOwnerInitChild
	recreate                map[string]preparedOwnerInitChild
	createdCurrentProject   *ports.NativeOwnerCreated
	createdLegacyProject    *ports.NativeOwnerCreated
	createdRecreateProject  *ports.NativeOwnerCreated
	teardowns               map[string]preparedOwnerTeardownChild
	registrationSource      *testyardmigration.PreparedOwnerSource
	checkedSource           *testyardmigration.CheckedOwnerSource
}

func ownerSemanticBefore(before releaseowner.OwnerBefore) releaseowner.OwnerBefore {
	before.PreparedRecipe = ""
	before.Capture = nil
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
	// Native capture and its prospective component views use the same admitted
	// command inputs. Persisted source bytes still come from the original view.
	draft.commandInputs = c.activationEnvironment(operation.options.RepositoryRoot)
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
	record.Inputs, err = canonicalOwnerChildInputs(record.Inputs)
	if err != nil {
		return nil, err
	}
	metadata, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	p := &preparedOwnerChildren{operation: operation, before: before, checks: map[string]ownerChildCheck{testyardmigration.CurrentYard: check}, baselines: map[string]*ownerInputBaseline{testyardmigration.CurrentYard: baseline}, inits: map[string]preparedOwnerInitChild{}, recreate: map[string]preparedOwnerInitChild{}, teardowns: map[string]preparedOwnerTeardownChild{}}
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
		record.LegacyInputs, err = canonicalOwnerChildInputs(record.LegacyInputs)
		if err != nil {
			return nil, err
		}
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
		legacyTeardown, err := prepareTeardownExecution([]string{"--yes"})
		if err != nil {
			return nil, err
		}
		// The approved replacement retains this installation's shared producers.
		legacyTeardown.holdShared = true
		legacyTeardownLoaded := c.ownerLabeledLoaded(legacyLoaded)
		if err := operation.observeTeardownExecution(ctx, legacyTeardownLoaded, legacyTeardown); err != nil {
			return nil, err
		}
		legacyTeardown.retainOwnerSourceSettings(legacyTeardownLoaded)
		legacyRuntime, ok := c.platform(legacyLoaded, originalPeers).(reconcileruntime.Runtime)
		if !ok {
			return nil, domain.ErrPlanStale
		}
		legacyProjection, err := legacyRuntime.PrepareOwnerInitProjection(ctx, legacyTeardown.snapshot().State, legacyTeardown.snapshot().Resources)
		if err != nil {
			return nil, fmt.Errorf("project original legacy removal: %w", err)
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
			teardown := legacyTeardown
			if yard != testyardmigration.LegacyYard {
				teardown, err = prepareTeardownExecution([]string{"--yes"})
				if err != nil {
					return nil, err
				}
				if err := operation.observeTeardownExecution(ctx, teardownLoaded, teardown); err != nil {
					return nil, err
				}
				teardown.retainOwnerSourceSettings(teardownLoaded)
			}
			body, err := captureOwnerTeardownRecord(teardown, c.cli.options.RepositoryRoot)
			if err != nil {
				return nil, err
			}
			record.Teardowns = append(record.Teardowns, ownerTeardownChildRecord{yard, body})
			p.teardowns[yard] = preparedOwnerTeardownChild{teardownLoaded, teardown}
			view, ok := c.platform(childLoaded, childPeers).(reconcileruntime.Runtime)
			if !ok {
				return nil, domain.ErrPlanStale
			}
			view, err = view.OwnerInitPortReleaseView(legacyRuntime, legacyProjection)
			if err != nil {
				return nil, err
			}
			var execution *initExecution
			var sources *reconcileruntime.PreparedInstalledRuntime
			var metadata []byte
			childRuntime := view
			lifecycle := yard == testyardmigration.LegacyYard && legacyTeardown.snapshot().State.InstanceFound
			if lifecycle {
				childRuntime, sources, err = c.prepareOwnerChildSources(ctx, view)
				if err == nil {
					metadata, err = nativejson.EncodeLosslessNativeJSON(ownerLifecycleChildMetadata{captureOwnerLoaded(ownerLoadedLabels(childLoaded, childRuntime.RepositoryRoot, childRuntime.LogicalRepositoryRoot)), childRuntime.OwnerPublicationRecord()}, 32<<20)
				}
			} else {
				execution, sources, err = c.prepareOwnerInitView(ctx, operation, childLoaded, childPeers, &view)
				if err == nil {
					metadata, err = captureOwnerInitRecord(execution, operation)
					childRuntime = execution.platform.(reconcileruntime.Runtime)
				}
			}
			if err != nil {
				return nil, fmt.Errorf("prepare original %s init: %w", yard, err)
			}
			sourceRecord, err := p.addSourceRecipe(sources.Recipe())
			if err != nil {
				return nil, err
			}
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
			if lifecycle {
				childRecord.Kind = "lifecycle"
			}
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
				var publicationPlan *reconcileruntime.PublicationConsumerPlan
				incus, _ := operation.statusPorts()
				_, instanceErr := incus.Instance(ctx, childLoaded.Context.IncusProject, childLoaded.Context.YardInstanceName)
				if instanceErr == nil {
					power, err := childRuntime.PrepareHostPowerRuntime(ctx)
					if err != nil {
						return nil, err
					}
					powerRecord, recordErr := childRuntime.RecordOwnerHostPower(power)
					if recordErr == nil && record.TemporaryStop != nil {
						if publication := childRuntime.OwnerPublicationRecord(); publication != nil {
							plan := publication.Consumer
							publicationPlan = &plan
						}
					}
					closeErr := power.Close()
					if recordErr != nil || closeErr != nil {
						return nil, errors.Join(recordErr, closeErr)
					}
					childRecord.Power = &powerRecord
				} else if !errors.Is(instanceErr, ports.ErrInstanceNotFound) {
					return nil, instanceErr
				}
				if record.TemporaryStop != nil {
					childRecord.Start, err = captureOwnerStart(ctx, operation, childLoaded, c.context.RepositoryRoot)
					if err != nil {
						return nil, err
					}
					childRecord.Start.Publication = publicationPlan
				}
			}
			record.Inits = append(record.Inits, childRecord)
			p.inits[yard] = preparedOwnerInitChild{loaded: childLoaded, execution: execution, sources: sources, runtime: childRuntime, power: childRecord.Power, start: childRecord.Start, instance: childRecord.Instance, volume: childRecord.Volume}
			if yard == testyardmigration.LegacyYard {
				projection := legacyProjection
				view, err := childRuntime.OwnerInitPlanningView(projection)
				if err != nil {
					return nil, err
				}
				future, futureSources, err := c.prepareOwnerInitView(ctx, operation, childLoaded, childPeers, &view)
				if err != nil {
					return nil, fmt.Errorf("prepare original legacy recreation: %w", err)
				}
				metadata, err := captureOwnerInitRecord(future, operation)
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
				p.recreate[yard] = preparedOwnerInitChild{projection: &projection, loaded: childLoaded, execution: future, sources: futureSources, runtime: futureRuntime, instance: futureInstance, volume: futureVolume}
			}
		}
		if recreate == nil {
			return nil, domain.ErrPlanStale
		}
		record.Inits = append(record.Inits, *recreate)
		var policies []*ports.PreparedOwnerImagePolicy
		for _, child := range record.Inits {
			if child.Instance != nil && child.Instance.Policy != nil && child.Instance.Policy.CacheBinding != "" {
				policies = append(policies, child.Instance.Policy)
			}
		}
		if len(policies) > 0 {
			incus, _ := operation.statusPorts()
			binder, ok := incus.(interface {
				BindOwnerImagePreparationCache(context.Context, []*ports.PreparedOwnerImagePolicy) error
			})
			if !ok {
				return nil, domain.ErrPlanStale
			}
			if err := binder.BindOwnerImagePreparationCache(ctx, policies); err != nil {
				return nil, err
			}
		}
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
	if index == 1 && child.Kind == "lifecycle" {
		return child.Yard == testyardmigration.LegacyYard && len(child.Projection) == 0 && child.Project == nil && child.Instance == nil && child.Volume != nil && child.Volume.InstanceUUID != "" && child.Power != nil
	}
	if index < 2 {
		return child.Yard == []string{testyardmigration.CurrentYard, testyardmigration.LegacyYard}[index] && child.Kind == "" && len(child.Projection) == 0 && child.Project == nil
	}
	return index == 2 && child.Yard == testyardmigration.LegacyYard && child.Kind == "recreate" && len(child.Projection) > 0 && child.Power == nil && child.Start == nil && child.Project != nil && child.Project.Project == "subyard-"+testyardmigration.LegacyYard && child.Project.Absent && child.Project.Config == nil
}

func (child ownerInitChildRecord) input() (ownerLoadedSnapshot, *reconcileruntime.OwnerPublicationRecord, error) {
	if child.Kind == "lifecycle" {
		var record ownerLifecycleChildMetadata
		if err := nativejson.DecodeLosslessNativeJSON(child.Metadata, &record, 32<<20); err != nil {
			return ownerLoadedSnapshot{}, nil, err
		}
		return record.Input, record.Publication, nil
	}
	var record ownerInitRecord
	if err := nativejson.DecodeLosslessNativeJSON(child.Metadata, &record, 32<<20); err != nil || record.Version != 1 {
		return ownerLoadedSnapshot{}, nil, domain.ErrPlanStale
	}
	return record.Input, record.Publication, nil
}

// prepareOwnerProjection restores only the original checked descriptions. It
// grants no invocation token and executes no init, resolver or publication.
func (c *releaseOwnerComponents) prepareOwnerProjection(ctx context.Context, before releaseowner.OwnerBefore) error {
	if before.PreparedRecipe == "" {
		return nil
	}
	p := c.ownerChildren
	if p == nil || p.binding() != before.PreparedRecipe || p.before != ownerSemanticBefore(before) {
		return domain.ErrPlanStale
	}
	if c.ownerRecoverySource != "" {
		reader, ok := c.ownerRecoveryObserver.(ports.NativeOwnerRestorationObserver)
		if !ok || c.ownerResultsRecipe != before.PreparedRecipe || p.registrationSource == nil {
			return domain.ErrPlanStale
		}
		member, err := reader.ReadOwnerRestoration(c.ownerRecoveryToken, c.ownerRecoverySource)
		if err != nil || p.registrationSource.ValidateRestoredMember(member) != nil {
			return domain.ErrPlanStale
		}
	}
	if child, ok := p.inits[testyardmigration.CurrentYard]; ok && child.execution != nil {
		return nil
	}
	if p.before.State == string(testyardmigration.StateLegacyFlatAdoptCurrent) || p.before.State == string(testyardmigration.StateLegacyDirectoryAdoptCurrent) {
		return nil
	}
	if !c.installedBound[before.PreparedRecipe] {
		return domain.ErrPlanStale
	}
	ctx = ports.WithNativePreparationExecutable(ctx, c.context.PreparationExecutable)
	ctx = ports.WithNativeInteraction(ctx, c.context.Interaction)
	token := ""
	if c.ownerResultsRecipe == before.PreparedRecipe {
		if c.ownerRecoveryToken == "" || c.ownerRecoveryObserver == nil {
			return domain.ErrPlanStale
		}
		token = c.ownerRecoveryToken
		ctx = ports.WithNativeOperationObserver(ctx, c.ownerRecoveryObserver)
	}
	return c.restoreOwnerChildren(ctx, before.PreparedRecipe, p.raw, token, true)
}
func (c *releaseOwnerComponents) bindOwnerChildrenForApply(ctx context.Context, hash string, raw []byte, token string) error {
	return c.restoreOwnerChildren(ctx, hash, raw, token, false)
}
func (c *releaseOwnerComponents) restoreOwnerChildren(ctx context.Context, hash string, raw []byte, token string, projection bool) error {
	if projection && token != "" && (token != c.ownerRecoveryToken || c.ownerResultsRecipe != hash || c.ownerRecoveryObserver == nil) {
		return domain.ErrPlanStale
	}
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
	// Share only the already owned immutable capsule, never its physical views.
	if previous := c.ownerChildren; previous != nil && bytes.Equal(previous.raw, raw) {
		raw, wire = previous.raw, previous.wire
	} else {
		raw = bytes.Clone(raw)
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
			metadata, _, err := child.input()
			if err != nil {
				return err
			}
			input := metadata.Context
			if input.YardName != child.Yard || input.AccessKind != domain.AccessLocal || input.Paths.ConfigHome != c.context.ConfigHome || input.Paths.DataHome != c.context.DataHome || input.Paths.RepositoryRoot != c.context.RepositoryRoot || input.Paths.ConfigDir != filepath.Join(c.context.RepositoryRoot, "config") {
				return domain.ErrPlanStale
			}
		}
		// Binding restores only original durable bytes. The original token and
		// borrowed mutation scope arrive at ApplyOwner, where every live source
		// and native guard is checked before these handles may be used.
		if token == "" && !projection {
			inert := &preparedOwnerChildren{operation: c.ownerChildOperation(), before: original.Before, raw: raw, wire: wire, registrationSource: registration}
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
			if c.ownerRecoverySource == nativeDigest(original.RegistrationSource) && c.ownerRecoveryToken == token && c.ownerResultsRecipe == hash {
				observer, ok = c.ownerRecoveryObserver.(ports.NativeOwnerRestorationObserver)
			}
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
		path := c.ownerInputPathPhysical(string(input.Path), baseline.initial.Context.Paths)
		if _, ok := baseline.files[path]; !ok {
			return domain.ErrPlanStale
		}
		fact, err := decodeOwnerFact(input.Fact)
		if err != nil {
			return err
		}
		baseline.files[path] = fact
	}
	current.raw, current.wire = raw, wire
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
		if c.ownerRecoverySource == view.sourceDigest && c.ownerRecoveryToken == token && c.ownerResultsRecipe == hash {
			view.restoration, _ = c.ownerRecoveryObserver.(ports.NativeOwnerRestorationObserver)
		}
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
	}
	if adopt && len(original.Inits) != 0 || !adopt && len(original.Inits) != 3 {
		return domain.ErrPlanStale
	}
	for i, child := range original.Inits {
		if !ownerInitChildSlot(child, i) {
			return domain.ErrPlanStale
		}
		input, publication, err := child.input()
		if err != nil {
			return err
		}
		loaded, err := input.restore(c.cli.catalog)
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
		var execution *initExecution
		if child.Kind == "lifecycle" {
			runtime, err = runtime.RestoreOwnerPublication(publication)
		} else {
			execution, err = restoreOwnerInitRecord(child.Metadata, loaded, runtime)
			if err == nil {
				runtime = execution.platform.(reconcileruntime.Runtime)
			}
		}
		if err != nil {
			return fmt.Errorf("restore original %s init: %w", child.Yard, err)
		}
		// Check every original immutable source before the owning component's
		// first publication. The returned source handle is discarded; actual
		// branch execution binds its original payload again at that boundary.
		guard, err := runtime.BindInstalledRuntimeContext(ctx, sourceRaw)
		if err != nil {
			return fmt.Errorf("check original %s source: %w", child.Yard, err)
		}
		if err := guard.ShareSources(c.retainPreparedSource); err != nil {
			return errors.Join(err, guard.Close())
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
		if execution != nil {
			execution.platform = runtime
		}
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
			current.recreate[child.Yard] = preparedOwnerInitChild{sources: guard, projection: &projection, loaded: loaded, execution: execution, runtime: runtime, instance: child.Instance, volume: child.Volume}
			continue
		}
		childBaseline := current.baselines[child.Yard]
		if childBaseline == nil {
			return domain.ErrPlanStale
		}
		if child.Yard == testyardmigration.LegacyYard {
			childBaseline.initial = loaded
			for _, input := range original.LegacyInputs {
				if input.Projected || len(input.Approved) != 0 {
					return domain.ErrPlanStale
				}
				fact, err := decodeOwnerFact(input.Fact)
				if err != nil {
					return err
				}
				path := c.ownerInputPathPhysical(string(input.Path), loaded.Context.Paths)
				if _, duplicate := current.baselines[testyardmigration.LegacyYard].files[path]; duplicate {
					return domain.ErrPlanStale
				}
				current.baselines[testyardmigration.LegacyYard].files[path] = fact
			}
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
		if execution != nil {
			execution.inputBaseline = childBaseline
		}
		current.inits[child.Yard] = preparedOwnerInitChild{sources: guard, loaded: loaded, execution: execution, runtime: runtime, power: child.Power, start: child.Start, instance: child.Instance, volume: child.Volume}
	}
	if adopt && len(original.Teardowns) != 0 || !adopt && len(original.Teardowns) != 2 {
		return domain.ErrPlanStale
	}
	for i, child := range original.Teardowns {
		if child.Yard != []string{testyardmigration.CurrentYard, testyardmigration.LegacyYard}[i] {
			return domain.ErrPlanStale
		}
		loaded := c.ownerLabeledLoaded(current.inits[child.Yard].loaded)
		var descriptor ownerTeardownRecord
		if err := nativejson.DecodeLosslessNativeJSON(child.Metadata, &descriptor, ownerTeardownRecordLimit); err != nil {
			return err
		}
		var peers []config.Loaded
		for _, row := range descriptor.Yards {
			if domain.Context(row.Context) == loaded.Context {
				continue
			}
			found := false
			for _, prepared := range current.inits {
				peer := c.ownerLabeledLoaded(prepared.loaded)
				if peer.Context == domain.Context(row.Context) {
					peers = append(peers, peer)
					found = true
					break
				}
			}
			if !found {
				return domain.ErrPlanStale
			}
		}
		execution, err := restoreOwnerTeardownRecord(child.Metadata, loaded, current.operation, peers...)
		if err != nil {
			return fmt.Errorf("restore original %s teardown: %w", child.Yard, err)
		}
		if execution == nil || original.Inits[i].Kind == "lifecycle" && !execution.snapshot().State.InstanceFound {
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

func (p *preparedOwnerChildren) record() (ownerChildrenRecord, error) {
	var record ownerChildrenRecord
	for _, file := range p.wire.Files {
		if file.Path == "prepared/owner-children.json" {
			err := json.Unmarshal(file.Content, &record)
			return record, err
		}
	}
	return record, domain.ErrPlanStale
}

func (record ownerChildrenRecord) capture() (releaseowner.OwnerCapture, error) {
	capture := releaseowner.OwnerCapture{Instance: "adopted"}
	if len(record.Inits) == 0 {
		return capture, nil
	}
	if len(record.Inits) != 3 {
		return capture, domain.ErrPlanStale
	}
	capture.Instance = "absent"
	if record.Inits[1].Kind == "lifecycle" {
		capture.Instance = "running"
	}
	if record.TemporaryStop != nil {
		capture.Instance = "stopped"
	}
	return capture, nil
}

func (c *releaseOwnerComponents) ApplyOwnerAction(ctx context.Context, before releaseowner.OwnerBefore, token string, reference releaseowner.OwnerActionReference) (actionErr error) {
	phase := "owner action inputs"
	defer func() {
		if actionErr != nil {
			actionErr = materializedConfigActivationError{actionErr, c.context.Yard, phase, "update", "apply", c.context.ConfigHome}
		}
	}()
	c.ownerPhysicalValidated = false
	p := c.ownerChildren
	if p == nil || before.PreparedRecipe != p.binding() || ownerSemanticBefore(before) != p.before || !c.installedBound[before.PreparedRecipe] {
		return domain.ErrPlanStale
	}
	if err := c.bindOwnerChildrenForApply(ctx, before.PreparedRecipe, p.raw, token); err != nil {
		return err
	}
	p = c.ownerChildren
	if c.hostPowerInstallation != nil {
		imported := false
		for _, child := range p.inits {
			record := child.runtime.OwnerPublicationRecord()
			if record == nil || record.OutputDigest != c.hostPowerInstallation.Output || string(record.Power.Source) != c.hostPowerInstallation.HostSource {
				continue
			}
			runtime, e := child.runtime.ImportOwnerPublicationInstallation(ctx, *c.hostPowerInstallation)
			if e != nil {
				return e
			}
			p.publicationInstallation = runtime.OwnerPublicationInstallation()
			imported = true
			break
		}
		if !imported {
			return domain.ErrPlanStale
		}
	}
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
	record, err := p.record()
	if err != nil {
		return err
	}
	capture, err := record.capture()
	if err != nil || before.Capture == nil || capture != *before.Capture || reference.Action.Slot > 2 {
		return domain.ErrPlanStale
	}
	options.AllowMissingLegacyProject = len(record.Projects) == 2 && record.Projects[1].Absent
	if err := c.restoreOwnerPhysical(ctx, before, token, reference); err != nil {
		return err
	}
	output := options.Stdout
	switch reference.Action.Operation {
	case "power", "start", "check-lease", "teardown":
		yard := testyardmigration.LegacyYard
		if reference.Action.Operation == "teardown" && reference.Action.Slot == 0 {
			yard = testyardmigration.CurrentYard
		}
		baseline := p.baselines[yard]
		if baseline == nil {
			return domain.ErrPlanStale
		}
		if err := baseline.check(ctx, p.operation); err != nil {
			return err
		}
	}
	if len(record.Inits) != 0 {
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
	switch reference.Action.Operation {
	case "project":
		incus, _ := c.cli.statusPorts()
		native, ok := incus.(ownerNativeResources)
		if !ok || len(record.Projects) != 2 {
			return domain.ErrPlanStale
		}
		index, scope := reference.Action.Slot, "current"
		if index == 1 {
			scope = "legacy"
		}
		project := record.Projects[0]
		if index < 2 {
			project = record.Projects[index]
		} else {
			if len(record.Inits) != 3 || record.Inits[2].Project == nil {
				return domain.ErrPlanStale
			}
			project, scope = *record.Inits[2].Project, "recreate"
		}
		return p.applyOwnerImageProject(ctx, native, project, scope)
	case "init":
		phase = "owner init volume"
		yard := testyardmigration.CurrentYard
		if reference.Action.Slot != 0 {
			yard = testyardmigration.LegacyYard
		}
		child, found := p.inits[yard]
		recreate := reference.Action.Slot == 2
		knownRecreated := c.ownerPhysical != nil && c.hasOwnerCreatedInstance("recreate")
		if reference.Action.Slot == 1 && knownRecreated {
			// This logical legacy role now has the incarnation produced by the
			// original compensated recreation. Reuse that approved component
			// capsule and its causal results, never prepare a new init.
			recreate = true
		}
		if recreate {
			child, found = p.recreate[yard]
		}
		if !found || child.execution == nil || child.sources == nil {
			return domain.ErrPlanStale
		}
		// Only this selected init consumes its original live volume precondition.
		// Other captured slots may already have been intentionally torn down.
		knownVolume := false
		for _, value := range c.ownerResults {
			if child.volume != nil && value.Intent.Kind == "srv-volume" && value.Intent.Scope == child.volume.Scope && value.Intent.Project == child.volume.Project && value.Intent.Resource == child.volume.Volume {
				knownVolume = true
			}
		}
		if !recreate && !knownVolume {
			checker, ok := child.runtime.Incus.(interface {
				CheckOwnerVolumeGuard(context.Context, ports.PreparedOwnerVolume) error
			})
			if !ok || child.volume == nil {
				return domain.ErrPlanStale
			}
			if err := checker.CheckOwnerVolumeGuard(ctx, *child.volume); err != nil {
				return err
			}
		}
		if recreate {
			if child.projection == nil {
				return domain.ErrPlanStale
			}
			// The exact output receipt and restored create results supersede
			// the pre-create absence guard. Native init retains its source,
			// UUID, volume and stage guards.
			if !knownRecreated {
				if p.createdRecreateProject != nil && yard == testyardmigration.LegacyYard {
					if len(record.Inits) != 3 || record.Inits[2].Project == nil || child.runtime.CheckOwnerInitCreatedProject(ctx, *child.projection, *record.Inits[2].Project, *p.createdRecreateProject) != nil {
						return domain.ErrPlanStale
					}
				} else if child.runtime.CheckOwnerInitRemoval(ctx, *child.projection) != nil {
					return domain.ErrPlanStale
				}
			}
		}
		phase = "owner init input view"
		baseline := p.baselines[yard]
		if baseline == nil || yard == testyardmigration.CurrentYard && (baseline.projected == nil || baseline.projected.requirePublished() != nil) {
			return domain.ErrPlanStale
		}
		if err := baseline.check(ctx, p.operation); err != nil {
			return err
		}
		runtime := child.runtime
		phase = "owner init sources"
		sources, err := child.runtime.BindInstalledRuntimeContext(ctx, child.sources.Recipe())
		if err != nil {
			return err
		}
		phase = "owner init project"
		var project *ports.PreparedImageProject
		var projectResult *ports.NativeOwnerCreated
		scope := "current"
		if yard == testyardmigration.LegacyYard {
			scope = "legacy"
		}
		if recreate {
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
		// Bind the captured project before materialization adds execution-only paths.
		runtime, err = runtime.WithOwnerYardProject(ctx, sources, *project, scope, projectResult)
		if err != nil {
			return errors.Join(err, sources.Close())
		}
		phase = "owner init sources"
		executionRuntime, err := runtime.OwnerSourceRuntime(ctx, sources)
		if err != nil {
			return errors.Join(err, sources.Close())
		}
		phase = "owner init host power"
		if p.publicationInstallation != nil && executionRuntime.OwnerPublicationRecord() != nil {
			executionRuntime, err = executionRuntime.WithOwnerPublicationInstallation(ctx, p.publicationInstallation)
			if err != nil {
				return errors.Join(err, sources.Close())
			}
		}
		executionRuntime.RuntimePlan = child.execution.runtimePlan
		executionRuntime.HookPlan = child.execution.hookPlan
		child.execution.platform = executionRuntime
		child.execution.operationID = "owner-registration"
		phase = "owner init published inputs"
		approved := map[string][]byte{}
		if baseline.projected != nil {
			approved = baseline.projected.approved
		}
		for path, payload := range approved {
			if err := child.execution.integrationBaseline.acceptPublishedSource(path, payload, p.operation.options.RepositoryRoot); err != nil {
				return err
			}
		}
		phase = "owner init execution"
		err = runOriginalOwnerInit(ctx, p.operation, child.execution, child.runtime, executionRuntime, output)
		if err == nil && executionRuntime.OwnerPublicationInstallation() != nil {
			p.publicationInstallation = executionRuntime.OwnerPublicationInstallation()
		}
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
	case "teardown":
		if reference.Action.Slot > 1 {
			return domain.ErrPlanStale
		}
		yard := []string{testyardmigration.CurrentYard, testyardmigration.LegacyYard}[reference.Action.Slot]
		return p.runTeardown(ctx, yard)
	case "power":
		return p.runPower(ctx, testyardmigration.LegacyYard)
	case "start":
		return p.runStart(ctx, testyardmigration.LegacyYard, output)
	case "check-lease":
		var status bytes.Buffer
		if err := p.readBrokerStatus(ctx, testyardmigration.LegacyYard, &status); err != nil {
			return err
		}
		return testyardmigration.VerifyNoActiveOwnerLease(status.Bytes())
	case "restore-stopped":
		if record.TemporaryStop == nil {
			return domain.ErrPlanStale
		}
		stop := *record.TemporaryStop
		stop.UUID, err = c.originalLegacyInstance(reference.Action.Slot)
		if err != nil || stop.UUID == "" {
			return errors.Join(domain.ErrPlanStale, err)
		}
		incus, _ := c.cli.statusPorts()
		native, ok := incus.(ownerNativeResources)
		if !ok {
			return domain.ErrPlanStale
		}
		return native.ApplyPreparedTemporaryStop(ctx, stop)
	case "check":
		if reference.Action.Slot > 1 {
			return domain.ErrPlanStale
		}
		yard := []string{testyardmigration.CurrentYard, testyardmigration.LegacyYard}[reference.Action.Slot]
		baseline := p.baselines[yard]
		check, ok := p.checks[yard]
		if !ok || check.Kind != "check" {
			return domain.ErrPlanStale
		}
		if baseline == nil || yard == testyardmigration.CurrentYard && (baseline.projected == nil || baseline.projected.requirePublished() != nil) {
			return domain.ErrPlanStale
		}
		if err := baseline.check(ctx, p.operation); err != nil {
			return materializedConfigActivationError{err, c.context.Yard, "owner registration inputs", "update", "verify", c.context.ConfigHome}
		}
		incus, _ := c.cli.statusPorts()
		if err := (hostruntime.HostCheck{Yard: check.Yard, Yards: check.Yards, Environment: check.Environment, Incus: incus, Output: output}).Run(ctx, hostruntime.CheckOptions{}); err != nil {
			return materializedConfigActivationError{err, c.context.Yard, "owner host readiness", "update", "verify", c.context.ConfigHome}
		}
		return nil
	case "verify-desired":
		actual, err := c.ObserveOwner(ctx, before, token)
		if err != nil || actual != "desired" {
			return errors.Join(domain.ErrPlanStale, err)
		}
		return nil
	case "verify-compensated":
		_, err := c.ObserveOwnerAction(ctx, before, token, reference)
		return err
	default:
		err := testyardmigration.ApplyPreparedRegistrationAction(ctx, options, ownerMigrationBefore(before), reference.Action.Operation)
		if testyardmigration.PreparedRegistrationActionUncertain(err) {
			if observer := ports.NativeOperationObserverFrom(ctx); observer != nil {
				observer.Uncertain()
			}
		}
		return err
	}
}

func (c *releaseOwnerComponents) ObserveOwnerAction(ctx context.Context, before releaseowner.OwnerBefore, token string, reference releaseowner.OwnerActionReference) (string, error) {
	if reference.Action.Operation == "verify-desired" {
		return c.ObserveOwner(ctx, before, token)
	}
	if reference.Action.Operation != "verify-compensated" {
		return "", domain.ErrPlanStale
	}
	if err := c.prepareOwnerProjection(ctx, before); err != nil {
		return "", err
	}
	if c.ownerPhysical != nil {
		if err := c.restoreOwnerPhysical(ctx, before, token, reference); err != nil {
			return "", err
		}
	}
	port, ok := c.cli.ownerRegistrationTransition(c.request()).(*ownerRegistrationTransition)
	if !ok {
		return "", domain.ErrPlanStale
	}
	options, err := port.options()
	if err != nil {
		return "", err
	}
	options.RecoveryToken = token
	record, err := c.ownerChildren.record()
	if err != nil {
		return "", err
	}
	options.AllowMissingLegacyProject = reference.Action.Slot == 1 && len(record.Projects) == 2 && record.Projects[1].Absent
	if err := testyardmigration.ApplyPreparedRegistrationAction(ctx, options, ownerMigrationBefore(before), "verify-compensated"); err != nil {
		return "", err
	}
	if reference.Action.Slot != 0 {
		if before.Capture == nil {
			return "", domain.ErrPlanStale
		}
		uuid, err := c.originalLegacyInstance(reference.Action.Slot)
		if err != nil {
			return "", err
		}
		incus, _ := c.cli.statusPorts()
		actual, err := incus.Instance(ctx, "subyard-"+testyardmigration.LegacyYard, "yard-"+testyardmigration.LegacyYard)
		if uuid == "" && errors.Is(err, ports.ErrInstanceNotFound) {
			return "desired", nil
		}
		status := "running"
		if before.Capture.Instance == "stopped" {
			status = "stopped"
		}
		if err != nil || uuid == "" || actual.Config["volatile.uuid"] != uuid || !strings.EqualFold(actual.Status, status) || actual.Config["user.subyard.desired_power"] != "running" {
			return "", errors.Join(domain.ErrPlanStale, err)
		}
	}
	return "desired", nil
}

func (c *releaseOwnerComponents) originalLegacyInstance(slot uint64) (string, error) {
	p := c.ownerChildren
	if p == nil || slot < 1 || slot > 2 {
		return "", domain.ErrPlanStale
	}
	var selected *ports.NativeOwnerCreated
	if c.ownerResultReader != nil {
		values, err := c.checkedOwnerResults()
		if err != nil {
			return "", err
		}
		scope := "legacy"
		if slot == 2 {
			scope = "recreate"
		}
		for i, value := range values {
			if value.Intent.Kind != "instance" || value.Intent.Scope != scope && !(slot == 1 && c.ownerPhysical != nil && value.Intent.Scope == "recreate") {
				continue
			}
			if selected != nil || value.Intent.Project != "subyard-"+testyardmigration.LegacyYard || value.Intent.Resource != "yard-"+testyardmigration.LegacyYard || value.InstanceUUID == "" {
				return "", domain.ErrPlanStale
			}
			selected = &values[i]
		}
	}
	if selected != nil {
		return selected.InstanceUUID, nil
	}
	if slot == 2 {
		return "", domain.ErrPlanStale
	}
	child, found := p.teardowns[testyardmigration.LegacyYard]
	if !found || child.execution == nil {
		return "", domain.ErrPlanStale
	}
	state := child.execution.snapshot().State
	if !state.InstanceFound {
		return "", nil
	}
	uuid := state.Instance.Config["volatile.uuid"]
	if uuid == "" {
		return "", domain.ErrPlanStale
	}
	return uuid, nil
}

func (c *releaseOwnerComponents) ownerPathLabel(path string) string {
	root := c.context.PhysicalRepositoryRoot
	if root == "" {
		root = c.cli.options.RepositoryRoot
	}
	return ownerSourcePathLabel(path, root, c.context.RepositoryRoot)
}

func (c *releaseOwnerComponents) ownerInputPathPhysical(path string, paths domain.RuntimePaths) string {
	return ownerRepositoryValue(path, c.context.RepositoryRoot, c.cli.options.RepositoryRoot, ownerMutableRoots(paths)...)
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

// Canonical labels may name one physical input through both owned FD spellings.
// Coalesce only byte-identical evidence; presence and projected authority remain strict.
func canonicalOwnerChildInputs(inputs []ownerChildInput) ([]ownerChildInput, error) {
	if len(inputs) < 2 {
		return inputs, nil
	}
	ordered := slices.Clone(inputs)
	slices.SortFunc(ordered, func(a, b ownerChildInput) int { return bytes.Compare(a.Path, b.Path) })
	result := ordered[:0]
	for _, input := range ordered {
		if len(result) > 0 && bytes.Equal(result[len(result)-1].Path, input.Path) {
			if !reflect.DeepEqual(result[len(result)-1], input) {
				return nil, domain.ErrPlanStale
			}
			continue
		}
		result = append(result, input)
	}
	return result, nil
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

// The native session retains this descriptor throughout synchronous children.
func ownerChildDispatcher(root string) string {
	if strings.HasPrefix(root, "/proc/self/fd/") {
		root = fmt.Sprintf("/proc/%d/fd/%s", os.Getpid(), strings.TrimPrefix(root, "/proc/self/fd/"))
	}
	return filepath.Join(root, "bin", "yard-engine")
}

func (c *releaseOwnerComponents) ownerChildOperation() *CLI {
	operation := c.cli.releaseActivationOperation()
	operation.ownerCompleteSource = c.completeSource
	operation.ownerLegacyRepository = c.context.RepositoryRoot
	operation.ownerLegacySource = nil
	operation.env = maps.Clone(operation.env)
	operation.baseEnv = maps.Clone(operation.baseEnv)
	for _, environment := range []map[string]string{operation.env, operation.baseEnv} {
		// Keep the command inputs admitted by the native front door after
		// discarding the predecessor's resolved environment.
		for _, name := range c.context.InheritedSettingIDs {
			if value, present := c.cli.baseEnv[name]; present {
				environment[name] = value
			}
		}
		delete(environment, "SUBYARD_CONFIG_LOADED")
		environment["SUBYARD_ENGINE_CONTEXT"] = "1"
		environment["SUBYARD_ENGINE_CONTEXT_SCHEMA"] = "1"
		environment["SUBYARD_INTERNAL_MIGRATION_CHILD"] = "1"
	}
	operation.options.DispatcherPath = ownerChildDispatcher(c.cli.options.RepositoryRoot)
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
	settings = ownerSettingsLabels(settings, c.ownerPathLabel)
	names := make([]string, 0, len(settings))
	for name := range settings {
		names = append(names, name)
	}
	slices.Sort(names)
	rows := make([]ownerTraceRecord, 0, len(names))
	for _, name := range names {
		v := settings[name]
		effective := v.EffectiveValue
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
				row.Resolutions[i] = ownerResolutionRecord{ownerStrings([]string{r.Scope, r.Role, r.Path, r.Value, r.Status, r.Detail}), r.Line}
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
