//go:build ignore

// Captured component facts for whole-operation differential tests. Release
// inspection, planning, journals, authorization and outcomes stay in the owner.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

type facts struct {
	RequireOwnerProjection bool                             `json:"requireOwnerProjection,omitempty"`
	OwnerRecipe            []byte                           `json:"ownerRecipe,omitempty"`
	RecoveryFault          string                           `json:"recoveryFault,omitempty"`
	Name                   string                           `json:"name"`
	Yards                  []string                         `json:"yards"`
	ActivationYards        []string                         `json:"activationYards"`
	Owners                 []string                         `json:"owners"`
	Targets                map[string][]releaseowner.Target `json:"targets"`
	Owner                  releaseowner.OwnerBefore         `json:"owner"`
}

type components struct {
	ownerPrepared                                              bool
	ownerBound                                                 bool
	input                                                      facts
	root                                                       *os.File
	context                                                    releaseowner.Context
	trace                                                      *os.File
	ownerReads                                                 *os.File
	ownerCurrent                                               bool
	ownerCleaned                                               bool
	ownerToken                                                 string
	ownerStarted                                               bool
	ownerPublished, ownerArchived, ownerMoved, ownerController bool
	ownerProjects                                              [2]bool
	ownerInstances                                             [2]string
	applied                                                    map[string]bool
	inputs                                                     map[string]config.PersistentFileSnapshot
	settings                                                   map[string][]byte
}

type interrupted struct{ cause error }

func (e interrupted) Error() string { return "private scripted component detail" }
func (e interrupted) Unwrap() error { return e.cause }
func (interrupted) ActivationDiagnostic() (string, string) {
	return "the component fixture was interrupted", "run yard update"
}

func (c *components) failAfterEffect(ctx context.Context) error {
	observer := ports.NativeOperationObserverFrom(ctx)
	operation := ports.NativeOperation{Kind: "instance-state", Server: sum([]byte("operation-peer")), Project: "project", Resource: "yard"}
	if err := observer.Before(operation); err != nil {
		return err
	}
	operation.ID = "captured-failed-operation"
	if err := observer.Accepted(operation); err != nil {
		return err
	}
	operation.Status = 400
	if err := observer.Terminal(operation); err != nil {
		return err
	}
	if c.input.Name == "failed-reobserve" {
		if err := os.Remove(filepath.Join(c.context.RuntimeRoot, "current")); err != nil {
			return err
		}
	}
	if c.input.RecoveryFault == "foreign-pending" || c.input.RecoveryFault == "corrupt-bundle" {
		path := filepath.Join(c.context.ConfigHome, "release-transition/v2/journal.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		journal, err := releasetransition.ParseJournal(raw)
		if err != nil {
			return err
		}
		if c.input.RecoveryFault == "foreign-pending" {
			journal.Goal.Target = "foreign"
			journal.Releases.Target = "foreign"
			for index := range journal.Steps {
				if journal.Steps[index].Evidence != nil {
					journal.Steps[index].Evidence.Releases = journal.Releases
				}
			}
			if err = journal.Validate(); err != nil {
				return err
			}
			raw, err = releasetransition.MarshalJournal(journal)
			if err != nil {
				return err
			}
			if _, err = releasetransition.ParseJournal(raw); err != nil {
				return err
			}
		} else {
			path = filepath.Join(c.context.ConfigHome, "migrations/native-custody/v1", string(journal.Transaction), "bundle.json")
			raw = []byte("{}\n")
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			return err
		}
	}
	if c.input.RecoveryFault == "source-drift" {
		if err := os.WriteFile(filepath.Join(c.context.ConfigHome, "config.env"), []byte("UNRELATED=changed\n"), 0600); err != nil {
			return err
		}
	}
	return interrupted{&ports.NativeOperationFailure{Operation: operation, Cause: errors.New("private captured final failure")}}
}

func sum(raw []byte) string            { value := sha256.Sum256(raw); return hex.EncodeToString(value[:]) }
func hash(value any) string            { raw, _ := json.Marshal(value); return sum(raw) }
func (c *components) call(name string) { _ = json.NewEncoder(c.trace).Encode(name) }
func (c *components) read(path string) (config.PersistentFileSnapshot, error) {
	value, err := config.ReadPersistentSnapshotAt(c.root, path)
	if err == nil {
		c.inputs[path] = value
	}
	return value, err
}
func assignments(path string, snapshot config.PersistentFileSnapshot) ([]releaseowner.Assignment, error) {
	parsed, err := config.ParsePersistentAssignments(path, snapshot.Content)
	result := []releaseowner.Assignment{}
	for _, a := range parsed {
		result = append(result, releaseowner.Assignment{Name: a.Name, Value: a.Value, Line: a.Line, Direct: a.Direct, Dynamic: a.Dynamic})
	}
	return result, err
}
func (c *components) CaptureInventory(context.Context) ([]string, []string, error) {
	c.ownerPrepared = false
	c.ownerBound = false
	c.call("inventory.capture")
	c.inputs = map[string]config.PersistentFileSnapshot{}
	return slices.Clone(c.input.Yards), slices.Clone(c.input.Owners), nil
}
func (c *components) CaptureIngress(context.Context, *releaseowner.IngressBinding) (releaseowner.IngressFacts, error) {
	return releaseowner.IngressFacts{Yards: slices.Clone(c.input.Yards), Operations: []releaseowner.IngressOperation{}, Decisions: []releasetransition.RedactedDecision{}, Blockers: []releasetransition.Blocker{}}, nil
}

// CaptureRustIngress is the explicit source-only component contract. These
// immutable fixture facts perform no legacy discovery or release assessment.
func (c *components) CaptureRustIngress(context.Context, *releaseowner.IngressBinding) (releaseowner.IngressFacts, error) {
	return releaseowner.IngressFacts{Yards: slices.Clone(c.input.Yards), Operations: []releaseowner.IngressOperation{}, Decisions: []releasetransition.RedactedDecision{}, Blockers: []releasetransition.Blocker{}}, nil
}
func (*components) ObserveIngress(context.Context, releaseowner.IngressStep) (string, error) {
	return "", errors.New("unexpected ingress")
}
func (*components) ApplyIngress(context.Context, releaseowner.IngressStep) error {
	return errors.New("unexpected ingress")
}
func (*components) VerifyIngress(context.Context, releaseowner.IngressStep) error {
	return errors.New("unexpected ingress")
}
func (c *components) CaptureYard(_ context.Context, yard string, resolved bool) (releaseowner.YardSettings, error) {
	c.call("yard.capture." + yard)
	if resolved || !slices.Contains(c.input.Yards, yard) {
		return releaseowner.YardSettings{}, errors.New("uncaptured yard")
	}
	canonical, flat := "yards/"+yard+"/config.env", "yards/"+yard+".env"
	nested, err := c.read(canonical)
	if err != nil {
		return releaseowner.YardSettings{}, err
	}
	legacy, err := c.read(flat)
	if err != nil {
		return releaseowner.YardSettings{}, err
	}
	snapshot, path := nested, canonical
	if legacy.Exists {
		snapshot, path = legacy, flat
	}
	parsed, err := assignments(path, snapshot)
	if err != nil {
		return releaseowner.YardSettings{}, err
	}
	value := releaseowner.YardSettings{Yard: yard, CanonicalExists: nested.Exists, FlatExists: legacy.Exists, Assignments: parsed, Inherited: []string{}, Sources: []releaseowner.Source{}, TemplateSupported: true}
	for _, input := range []struct{ role, path string }{{"shared", "overrides/shared/config.env"}, {"host", "config.env"}} {
		snapshot, err := c.read(input.path)
		if err != nil {
			return value, err
		}
		parsed, err := assignments(input.path, snapshot)
		if err != nil {
			return value, err
		}
		value.Sources = append(value.Sources, releaseowner.Source{Role: input.role, Label: input.path, Exists: snapshot.Exists, SHA256: sum(snapshot.Content), Assignments: parsed})
	}
	return value, nil
}
func (c *components) PrepareSettings(_ context.Context, yard string, edits []releaseowner.Edit) (releaseowner.PreparedSettings, error) {
	path, target := "yards/"+yard+"/config.env", "canonical"
	expected, err := c.read(path)
	if err != nil {
		return releaseowner.PreparedSettings{}, err
	}
	flat, err := c.read("yards/" + yard + ".env")
	if err != nil {
		return releaseowner.PreparedSettings{}, err
	}
	if flat.Exists {
		expected, path, target = flat, "yards/"+yard+".env", "flat"
	}
	desired := slices.Clone(expected.Content)
	for _, edit := range edits {
		desired, err = config.EditPersistentAssignmentContent(path, desired, edit.Setting, edit.Value)
		if err != nil {
			return releaseowner.PreparedSettings{}, err
		}
	}
	inputs := []releaseowner.InputBinding{}
	for relative, snapshot := range c.inputs {
		if strings.HasPrefix(relative, "yards/") {
			continue
		}
		binding := config.BindPersistentSnapshot(snapshot)
		inputs = append(inputs, releaseowner.InputBinding{Relative: relative, Exists: binding.Exists, Digest: binding.Digest, Identity: binding.Identity})
	}
	slices.SortFunc(inputs, func(a, b releaseowner.InputBinding) int { return strings.Compare(a.Relative, b.Relative) })
	c.settings[path] = desired
	return releaseowner.PreparedSettings{Yard: yard, Target: target, ExpectedExists: expected.Exists, ExpectedFile: sum(expected.Content), DesiredFile: sum(desired), Source: hash(inputs), Inputs: inputs}, nil
}
func (c *components) CaptureOwnerObservation(context.Context) (releaseowner.OwnerBefore, error) {
	c.call("owner.observe-registration")
	if c.input.RecoveryFault == "ambiguous" {
		return releaseowner.OwnerBefore{}, errors.New("ambiguous registration")
	}
	if c.input.RecoveryFault == "legacy-drift" || c.input.RecoveryFault == "current" {
		state := "legacy-directory"
		if c.input.RecoveryFault == "current" {
			state = "current"
		}
		return releaseowner.OwnerBefore{State: state, Registration: strings.Repeat("b", 64), Overrides: strings.Repeat("b", 64), Controller: strings.Repeat("b", 64)}, nil
	}
	value := c.input.Owner
	if c.ownerCurrent {
		value.State = "current"
	}
	value.PreparedRecipe = ""
	value.Capture = nil
	return value, nil
}
func (c *components) originalOwner() releaseowner.OwnerBefore {
	value := c.input.Owner
	if strings.HasPrefix(value.State, "legacy-") && value.Capture == nil {
		instance := "running"
		if strings.HasSuffix(value.State, "-adopt-current") {
			instance = "adopted"
		}
		value.Capture = &releaseowner.OwnerCapture{Instance: instance}
	}
	return value
}
func (c *components) CaptureOwner(_ context.Context, source *releaseowner.CompleteSourceReference) (releaseowner.OwnerBefore, error) {
	// The captured corpus has no COMPLETE-source facts or continuation authority.
	if source != nil {
		return releaseowner.OwnerBefore{}, errors.New("unexpected complete source reference")
	}
	c.call("owner.prepare")
	c.ownerPrepared = true
	if c.ownerCurrent && !c.ownerCleaned && c.input.Name == "owner-original-progress" {
		return releaseowner.OwnerBefore{}, errors.New("original source archived; observe original owner preparation")
	}
	value := c.originalOwner()
	if c.ownerCurrent {
		value.State = "current"
		value.Capture = nil
	}
	return value, nil
}
func (c *components) ProjectActivation(_ context.Context, before releaseowner.OwnerBefore) ([]string, error) {
	c.call("owner.project-activation")
	if c.input.RequireOwnerProjection && c.ownerBound {
		c.ownerPrepared = true
	}
	if !reflect.DeepEqual(before, c.originalOwner()) {
		return nil, errors.New("changed original owner activation projection")
	}
	yards := slices.Clone(c.input.ActivationYards)
	if len(yards) == 0 {
		return nil, errors.New("missing captured activation projection facts")
	}
	_ = json.NewEncoder(c.ownerReads).Encode(struct {
		Method string                   `json:"method"`
		Before releaseowner.OwnerBefore `json:"before"`
		Yards  []string                 `json:"yards"`
	}{"project-activation", before, yards})
	return yards, nil
}
func (c *components) ObserveOwner(_ context.Context, before releaseowner.OwnerBefore, token string) (string, error) {
	c.call("owner.observe")
	progress := "expected"
	if c.ownerCurrent {
		progress = "desired"
	} else if c.ownerPublished || c.ownerArchived || c.ownerMoved {
		progress = "in-progress"
	}
	_ = json.NewEncoder(c.ownerReads).Encode(struct {
		Method      string                   `json:"method"`
		Before      releaseowner.OwnerBefore `json:"before"`
		Token       string                   `json:"token"`
		Progress    string                   `json:"progress"`
		AfterEffect bool                     `json:"afterEffect"`
	}{"observe", before, token, progress, c.ownerCurrent})
	if !reflect.DeepEqual(before, c.originalOwner()) || len(token) != 32 || strings.Trim(token, "0123456789abcdef") != "" || c.ownerToken != "" && token != c.ownerToken {
		return "", errors.New("changed original owner observation binding")
	}
	if c.ownerToken == "" {
		c.ownerToken = token
	}
	return progress, nil
}
func (c *components) CaptureActivation(_ context.Context, owner string, _ bool) ([]releaseowner.Target, error) {
	c.call("component.observe." + owner)
	values := slices.Clone(c.input.Targets[owner])
	if c.input.RequireOwnerProjection && (owner == "sample-runtime" || owner == "test-yard-route-consumers") {
		if c.ownerPrepared {
			c.call("component.reserved." + owner)
		} else {
			c.call("component.unprepared." + owner)
		}
	}

	if c.input.RequireOwnerProjection && !c.ownerPrepared && (owner == "sample-runtime" || owner == "test-yard-route-consumers") {
		for i := range values {
			values[i].Source = sum([]byte("unprepared reserved owner view"))
			values[i].Desired = sum([]byte("unprepared reserved owner desired"))
		}
	}
	for i := range values {
		if c.applied[owner+"."+values[i].Yard] {
			values[i].State = "current"
			values[i].Actual = values[i].Desired
			values[i].ReportedActual = values[i].ReportedDesired
		}
	}
	return values, nil
}
func settingsPath(recipe releaseowner.SettingsRecipe) (string, error) {
	if recipe.Target == "canonical" {
		return "yards/" + recipe.Yard + "/config.env", nil
	}
	if recipe.Target == "flat" {
		return "yards/" + recipe.Yard + ".env", nil
	}
	return "", errors.New("unexpected settings target")
}
func (c *components) snapshot(recipe releaseowner.SettingsRecipe) (config.PersistentFileSnapshot, error) {
	if hash(recipe.Inputs) != recipe.Source {
		return config.PersistentFileSnapshot{}, errors.New("changed native input digest")
	}
	for _, input := range recipe.Inputs {
		snapshot, err := config.ReadPersistentSnapshotAt(c.root, input.Relative)
		if err != nil || config.BindPersistentSnapshot(snapshot) != (config.PersistentBinding{Exists: input.Exists, Digest: input.Digest, Identity: input.Identity}) {
			c.call("settings.source-refused")
			return snapshot, errors.New("changed native input identity")
		}
	}
	path, err := settingsPath(recipe)
	if err != nil {
		return config.PersistentFileSnapshot{}, err
	}
	return config.ReadPersistentSnapshotAt(c.root, path)
}
func (c *components) ObserveSettings(_ context.Context, recipe releaseowner.SettingsRecipe) (string, error) {
	snapshot, err := c.snapshot(recipe)
	if err != nil {
		return "", err
	}
	if snapshot.Exists == recipe.ExpectedExists && sum(snapshot.Content) == recipe.ExpectedFile {
		return "expected", nil
	}
	if snapshot.Exists && sum(snapshot.Content) == recipe.DesiredFile {
		return "desired", nil
	}
	return "conflict", nil
}
func (c *components) CaptureSettings(_ context.Context, recipe releaseowner.SettingsRecipe) (string, error) {
	snapshot, err := c.snapshot(recipe)
	return hash(struct {
		Exists bool   `json:"exists"`
		SHA256 string `json:"sha256"`
	}{snapshot.Exists, sum(snapshot.Content)}), err
}
func (c *components) ApplySettings(ctx context.Context, recipe releaseowner.SettingsRecipe) error {
	c.call("settings.apply")
	snapshot, err := c.snapshot(recipe)
	if err != nil {
		return err
	}
	if snapshot.Exists != recipe.ExpectedExists || sum(snapshot.Content) != recipe.ExpectedFile {
		return errors.New("changed settings")
	}
	path, err := settingsPath(recipe)
	if err != nil {
		return err
	}
	desired := c.settings[path]
	if sum(desired) != recipe.DesiredFile {
		return errors.New("changed prepared settings")
	}
	if err := config.CompareAndSwapLocalSettingsAt(c.root, path, config.BindPersistentSnapshot(snapshot), desired, nil, nil, nil); err != nil {
		return err
	}
	if c.input.Name == "apply-error-after-effect" || c.input.Name == "failed-reobserve" {
		return c.failAfterEffect(ctx)
	}
	return nil
}
func (c *components) ownerAction(before releaseowner.OwnerBefore, token string) error {
	if !reflect.DeepEqual(before, c.originalOwner()) || before.Capture == nil || len(token) != 32 || strings.Trim(token, "0123456789abcdef") != "" || c.ownerToken != "" && token != c.ownerToken {
		return errors.New("changed owner")
	}
	c.ownerToken = token
	if !c.ownerStarted {
		c.ownerStarted = true
		c.ownerProjects[1] = true
		c.ownerInstances = [2]string{"absent", before.Capture.Instance}
		if before.Capture.Instance == "adopted" {
			c.ownerProjects = [2]bool{true, false}
			c.ownerInstances = [2]string{"running", "absent"}
		}
	}
	return nil
}
func (c *components) ApplyOwnerAction(ctx context.Context, before releaseowner.OwnerBefore, token string, reference releaseowner.OwnerActionReference) error {
	if err := c.ownerAction(before, token); err != nil {
		return err
	}
	action := reference.Action
	c.call(fmt.Sprintf("owner.action.%s.%d", action.Operation, action.Slot))
	yard := 1
	if action.Slot == 0 {
		yard = 0
	}
	switch action.Operation {
	case "validate-original", "power", "remove-empty-flat":
		return nil
	case "project":
		c.ownerProjects[yard] = true
	case "init":
		if !c.ownerProjects[yard] || yard == 0 && !c.ownerPublished || yard == 1 && c.ownerArchived {
			return errors.New("original owner init prerequisites changed")
		}
		c.ownerInstances[yard] = "running"
	case "teardown":
		c.ownerInstances[yard] = "absent"
		c.ownerProjects[yard] = false
	case "start":
		if c.ownerInstances[1] == "absent" {
			return errors.New("original owner instance absent")
		}
		c.ownerInstances[1] = "running"
	case "check", "check-lease":
		if action.Operation == "check" && action.Slot == 1 && c.input.RecoveryFault == "owner-check-failed" {
			return errors.New("synthetic original owner check failed")
		}
		if c.ownerInstances[yard] != "running" {
			return errors.New("original owner is not running")
		}
	case "restore-stopped":
		if before.Capture.Instance != "stopped" || c.ownerInstances[1] == "absent" {
			return errors.New("original stopped owner changed")
		}
		c.ownerInstances[1] = "stopped"
	case "publish-registration":
		c.ownerPublished = true
	case "move-overrides":
		c.ownerMoved = strings.Contains(before.State, "+overrides")
	case "archive-registration":
		if !c.ownerPublished {
			return errors.New("current registration absent")
		}
		c.ownerArchived = true
		c.ownerPrepared = false
	case "archive-controller":
		c.ownerController = true
	case "restore-registration":
		c.ownerArchived = false
	case "restore-overrides":
		c.ownerMoved = false
	case "park-current-registration":
		if c.ownerArchived {
			return errors.New("original registration not restored")
		}
		c.ownerPublished, c.ownerCurrent = false, false
	case "verify-compensated":
		_, err := c.ObserveOwnerAction(ctx, before, token, reference)
		return err
	case "verify-desired":
		if !c.ownerPublished || !c.ownerArchived || c.ownerMoved != strings.Contains(before.State, "+overrides") || !c.ownerController || !c.ownerProjects[0] || c.ownerInstances[0] != "running" || before.Capture.Instance != "adopted" && (c.ownerProjects[1] || c.ownerInstances[1] != "absent") {
			return errors.New("desired owner state not reached")
		}
		c.ownerCurrent = true
		c.call("owner.apply")
	default:
		return errors.New("unknown prepared owner action")
	}
	return nil
}
func (c *components) ObserveOwnerAction(ctx context.Context, before releaseowner.OwnerBefore, token string, reference releaseowner.OwnerActionReference) (string, error) {
	if err := c.ownerAction(before, token); err != nil {
		return "", err
	}
	if reference.Action.Operation == "verify-desired" {
		return c.ObserveOwner(ctx, before, token)
	}
	if reference.Action.Operation != "verify-compensated" || c.ownerPublished || c.ownerArchived || c.ownerMoved {
		return "", errors.New("original registration not restored")
	}
	if before.Capture.Instance == "adopted" {
		if !c.ownerProjects[0] || c.ownerInstances[0] != "running" || c.ownerProjects[1] {
			return "", errors.New("adopted original owner not restored")
		}
	} else if c.ownerProjects[0] || c.ownerInstances[0] != "absent" {
		return "", errors.New("failed canonical owner remains")
	}
	if reference.Action.Slot != 0 {
		want := "running"
		if before.Capture.Instance == "stopped" {
			want = "stopped"
		}
		if !c.ownerProjects[1] || c.ownerInstances[1] != want && !(reference.Action.Slot == 1 && before.Capture.Instance == "absent" && c.ownerInstances[1] == "absent") {
			return "", errors.New("original owner not restored")
		}
	}
	return "desired", nil
}
func (c *components) CleanupOwner(_ context.Context, before releaseowner.OwnerBefore, token string) error {
	c.call("owner.cleanup")
	if !reflect.DeepEqual(before, c.originalOwner()) || !c.ownerCurrent || token != c.ownerToken || len(token) != 32 {
		return errors.New("changed owner cleanup")
	}
	if c.input.Name == "owner-cleanup-failure" {
		return errors.New("private captured cleanup failure")
	}
	c.ownerCleaned = true
	return nil
}
func (c *components) ApplyActivation(ctx context.Context, target releaseowner.Target) error {
	c.call("component.apply." + target.Owner + "." + target.Yard)
	if !slices.ContainsFunc(c.input.Targets[target.Owner], func(value releaseowner.Target) bool { return value == target }) {
		return errors.New("changed target")
	}
	c.applied[target.Owner+"."+target.Yard] = true
	// A scripted accepted SDK operation gives the normal closed helper protocol
	// real durable acceptance/terminal facts; it proves policy and ordering only.
	observer := ports.NativeOperationObserverFrom(ctx)
	operation := ports.NativeOperation{Kind: "instance-state", Server: sum([]byte("operation-peer")), Project: target.Project, Resource: target.Instance}
	if err := observer.Before(operation); err != nil {
		return err
	}
	operation.ID = "captured-operation"
	if err := observer.Accepted(operation); err != nil {
		return err
	}
	operation.Status = 200
	return observer.Terminal(operation)
}
func (*components) Close() error { return nil }

func main() {
	if len(os.Args) != 2 || os.Args[1] != "_release-owner-native-v1" {
		os.Exit(2)
	}
	err := releaseowner.Serve(context.Background(), os.Stdin, os.Stdout, func(_ context.Context, scope releaseowner.Context, root *os.File) (releaseowner.Components, error) {
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(scope.ConfigHome), "operation-peer-input.json"))
		if err != nil {
			return nil, err
		}
		var input facts
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if len(raw) > 8<<20 {
			return nil, errors.New("captured facts bound")
		}
		if err = decoder.Decode(&input); err != nil {
			return nil, err
		}
		trace, err := os.OpenFile(filepath.Join(filepath.Dir(scope.ConfigHome), "component-calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		reads, err := os.OpenFile(filepath.Join(filepath.Dir(scope.ConfigHome), "owner-reads.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			trace.Close()
			return nil, err
		}
		return &components{input: input, root: root, context: scope, trace: trace, ownerReads: reads, applied: map[string]bool{}, inputs: map[string]config.PersistentFileSnapshot{}, settings: map[string][]byte{}}, nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "operation peer refused")
		os.Exit(1)
	}
}

func (c *components) InstalledRuntimeRecipe(hash string) ([]byte, error) {
	if !c.input.RequireOwnerProjection || sum(c.input.OwnerRecipe) != hash {
		return nil, errors.New("unbound original owner recipe")
	}
	return bytes.Clone(c.input.OwnerRecipe), nil
}
func (c *components) BindInstalledRuntimeRecipe(_ context.Context, hash string, raw []byte) error {
	if !c.input.RequireOwnerProjection || sum(raw) != hash || !bytes.Equal(raw, c.input.OwnerRecipe) {
		c.call("owner.original-recipe-refused")
		return errors.New("changed original owner recipe")
	}
	c.ownerBound = true
	c.call("owner.bind-original-recipe")
	return nil
}

func (c *components) BindOwnerResults(_ context.Context, before releaseowner.OwnerBefore, token string, values []ports.NativeOwnerCreated) error {
	if !c.input.RequireOwnerProjection || !c.ownerBound || !reflect.DeepEqual(before, c.originalOwner()) || c.ownerToken != "" && token != c.ownerToken || len(token) != 32 || len(values) != 0 {
		c.call("owner.original-results-refused")
		return errors.New("changed original component result scope")
	}
	c.ownerToken = token // The boundary already checked this closed producer's original intent.
	c.call("owner.bind-empty-original-results")
	return nil
}

// This is a synthetic transport binding, not an SDK physical-state oracle.
func ownerPhysicalBinding(before releaseowner.OwnerBefore, token string) string {
	return hash(struct {
		Before releaseowner.OwnerBefore
		Token  string
	}{before, token})
}
func (c *components) CaptureOwnerPhysical(_ context.Context, before releaseowner.OwnerBefore, token string, _ releaseowner.OwnerActionReference) (string, error) {
	if !reflect.DeepEqual(before, c.originalOwner()) || token != c.ownerToken || len(token) != 32 || before.PreparedRecipe != "" && !c.ownerBound {
		c.call("owner.physical-capture-refused")
		return "", errors.New("changed synthetic owner output binding")
	}
	return ownerPhysicalBinding(before, token), nil
}
func (c *components) BindOwnerPhysical(_ context.Context, before releaseowner.OwnerBefore, token string, _ releaseowner.OwnerActionReference, physical string) error {
	if !c.ownerBound || !reflect.DeepEqual(before, c.originalOwner()) || token != c.ownerToken || physical != ownerPhysicalBinding(before, token) {
		c.call("owner.physical-bind-refused")
		return errors.New("changed synthetic owner output receipt")
	}
	c.call("owner.bind-original-physical")
	return nil
}
