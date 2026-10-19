package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Subyard/Subyard/internal/config"
	"io"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releaseowner"
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
	Version  int
	Before   releaseowner.OwnerBefore
	Checks   []ownerCheckRecord
	Settings string
	Inputs   []ownerChildInput
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
	operation *CLI
	before    releaseowner.OwnerBefore
	wire      ownerChildrenWire
	raw       []byte
	checks    map[string]ownerChildCheck
	baselines map[string]*ownerInputBaseline
}

func ownerSemanticBefore(before releaseowner.OwnerBefore) releaseowner.OwnerBefore {
	before.PreparedRecipe = ""
	return before
}

func (c *releaseOwnerComponents) prepareOwnerChildren(ctx context.Context, before releaseowner.OwnerBefore) (*preparedOwnerChildren, error) {
	before = ownerSemanticBefore(before)
	switch testyardmigration.State(before.State) {
	case testyardmigration.StateAbsent, testyardmigration.StateCurrent:
		return nil, nil
	case testyardmigration.StateLegacyFlatAdoptCurrent, testyardmigration.StateLegacyDirectoryAdoptCurrent:
	default:
		return nil, errors.New("original owner init and teardown child recipes are unavailable")
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
	p := &preparedOwnerChildren{operation: operation, before: before, checks: map[string]ownerChildCheck{testyardmigration.CurrentYard: check}, baselines: map[string]*ownerInputBaseline{testyardmigration.CurrentYard: baseline}}
	p.wire = ownerChildrenWire{Version: 1, Owner: "owner-registration", Source: nativeValueDigest(ownerChildrenSemantic(record)), Runtime: nativeValueDigest(struct{ Session, Artifact, Repository string }{c.context.Session, c.context.Artifact, c.context.RepositoryRoot}), Resources: nativeValueDigest(record.Checks), Files: []ownerRecipeFile{{"prepared/owner-children.json", metadata}}}
	p.raw, err = json.Marshal(p.wire)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (p *preparedOwnerChildren) binding() string { return nativeDigest(p.raw) }

func (c *releaseOwnerComponents) bindOwnerChildren(ctx context.Context, hash string, raw []byte) error {
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
	if !bytes.Equal(canonical, raw) || len(wire.Files) != 1 || wire.Files[0].Path != "prepared/owner-children.json" || wire.Owner != "owner-registration" || wire.Version != 1 || wire.Launcher != "" || wire.HookBefore != "" || len(wire.Leaves) != 0 {
		return domain.ErrPlanStale
	}
	// Current preparation is only a guard against original source/scope drift.
	// The immutable original bytes remain the incoming bundle authority.
	var original ownerChildrenRecord
	if err := json.Unmarshal(wire.Files[0].Content, &original); err != nil {
		return err
	}
	metadata, _ := json.Marshal(original)
	if !bytes.Equal(metadata, wire.Files[0].Content) || original.Before.PreparedRecipe != "" || len(original.Inputs) > 4096 {
		return domain.ErrPlanStale
	}
	if c.ownerChildren != nil && original.Before != c.ownerChildren.before {
		return domain.ErrPlanStale
	}
	current, err := c.prepareOwnerChildren(ctx, original.Before)
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
	if !reflect.DeepEqual(ownerChildrenSemantic(original), ownerChildrenSemantic(observed)) {
		return domain.ErrPlanStale
	}
	expected := current.wire
	expected.Source = nativeValueDigest(ownerChildrenSemantic(original))
	expected.Files = wire.Files
	wanted, _ := json.Marshal(expected)
	if !bytes.Equal(wanted, raw) {
		return domain.ErrPlanStale
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
	c.ownerChildren = current
	if c.installedBound == nil {
		c.installedBound = map[string]bool{}
	}
	c.installedBound[hash] = true
	return nil
}

func (c *releaseOwnerComponents) applyOwnerChildren(ctx context.Context, before releaseowner.OwnerBefore, token string) error {
	p := c.ownerChildren
	if p == nil || before.PreparedRecipe != p.binding() || ownerSemanticBefore(before) != p.before || !c.installedBound[before.PreparedRecipe] {
		return domain.ErrPlanStale
	}
	if err := c.bindOwnerChildren(ctx, before.PreparedRecipe, p.raw); err != nil {
		return err
	}
	p = c.ownerChildren
	port, ok := c.cli.ownerRegistrationTransition(c.request()).(*ownerRegistrationTransition)
	if !ok {
		return domain.ErrPlanStale
	}
	options, err := port.options()
	if err != nil {
		return err
	}
	options.RecoveryToken = token
	options.RunYard = func(ctx context.Context, yard string, output io.Writer, args ...string) error {
		check, ok := p.checks[yard]
		if !ok || check.Kind != "check" || !slices.Equal(args, []string{"check"}) {
			return domain.ErrPlanStale
		}
		baseline := p.baselines[yard]
		if baseline == nil || baseline.projected == nil || baseline.projected.requirePublished() != nil {
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
