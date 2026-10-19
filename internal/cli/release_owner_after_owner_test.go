package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

func afterOwnerFilesFixture(t *testing.T) (*releaseOwnerComponents, *testkit.Incus, *nativeMaterializedState, releaseowner.Target, []byte) {
	t.Helper()
	ctx := context.Background()
	c, _ := nativeActivationSourceFixture(t)
	nativeMaterializedSourceFiles(t, c.context.RepositoryRoot)
	if err := os.MkdirAll(c.context.DataHome, 0700); err != nil {
		t.Fatal(err)
	}
	incus := c.cli.options.Incus.(*testkit.Incus)
	delete(incus.Instances, "subyard/yard")
	incus.Reconcile = ports.ReconcileState{}
	state := &nativeMaterializedState{files: map[string][]byte{}}
	c.cli.options.Executor = state
	if _, _, err := c.CaptureInventory(ctx); err != nil {
		t.Fatal(err)
	}
	targets, err := c.CaptureActivation(ctx, "materialized-config", false)
	if err != nil || len(targets) != 1 {
		t.Fatal("original absent targets", len(targets), err)
	}
	prepared := c.activation[activationKey(targets[0])]
	prepared, err = c.prepareAfterOwnerFiles(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	original := prepared.value
	c.activation[activationKey(original)] = prepared
	raw := prepared.nativeInstalled.Recipe()
	if err := c.BindInstalledRuntimeRecipeForTarget(ctx, original.PreparedRecipe, raw, original); err != nil {
		t.Fatal("original source-only bind", err)
	}
	return c, incus, state, original, raw
}
func bindAfterOwnerFilesResults(t *testing.T, c *releaseOwnerComponents, target releaseowner.Target) releaseowner.AfterOwnerCompletion {
	t.Helper()
	hash := strings.Repeat("a", 64)
	before := releaseowner.OwnerBefore{State: "legacy-directory", Registration: hash, Overrides: hash, Controller: hash}
	record := ownerChildrenRecord{Version: 1, Before: before, Inits: []ownerInitChildRecord{{Yard: testyardmigration.CurrentYard, Instance: &ports.PreparedOwnerInstance{Scope: "current", Project: target.Project, Instance: target.Instance, Server: hash}}}}
	metadata, _ := json.Marshal(record)
	wire := ownerChildrenWire{Version: 1, Owner: "owner-registration", Files: []ownerRecipeFile{{Path: "prepared/owner-children.json", Content: metadata}}}
	raw, _ := json.Marshal(wire)
	c.ownerChildren = &preparedOwnerChildren{before: before, wire: wire, raw: raw}
	before.PreparedRecipe = c.ownerChildren.binding()
	c.installedBound[before.PreparedRecipe] = true
	value := ports.NativeOwnerCreated{Ref: ports.NativeOwnerCreateRef{Producer: hash, Domain: "operation", Index: 1}, Intent: ports.NativeOwnerCreateIntent{Scope: "current", Kind: "instance", Server: hash, Project: target.Project, Resource: target.Instance, Token: strings.Repeat("1", 32), Fingerprint: hash}, InstanceUUID: "original-created-uuid", Status: 200}
	if err := c.BindOwnerResults(ownerResultTestContext([]ports.NativeOwnerCreated{value}), before, strings.Repeat("1", 32), []ports.NativeOwnerCreated{value}); err != nil {
		t.Fatal(err)
	}
	return releaseowner.AfterOwnerCompletion{Producer: releaseowner.Binding{Session: c.context.Session, Transaction: "original-owner", Plan: "plan-v1-" + hash, Journal: hash, Artifact: c.context.Artifact, Helper: hash, Ordinal: 1}, Terminal: hash}
}
func TestAfterOwnerFilesCLIConstructorResolutionAndBoundObservation(t *testing.T) {
	ctx := context.Background()
	c, incus, state, original, raw := afterOwnerFilesFixture(t)
	if state.writes != 0 {
		t.Fatal("preconsent source capture wrote guest")
	}
	completed := bindAfterOwnerFilesResults(t, c, original)
	instance := ports.InstanceInfo{Name: original.Instance, Project: original.Project, Status: "Running", Config: map[string]string{"volatile.uuid": "original-created-uuid", "user.subyard.owner_create": strings.Repeat("1", 32)}}
	incus.Instances[original.Project+"/"+original.Instance] = instance
	incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: instance}
	resolved, recipe, err := c.ResolveAfterOwner(ctx, original, completed)
	if err != nil {
		t.Fatal("resolve", err)
	}
	if resolved.Source != original.Source || resolved.InstanceUUID != "original-created-uuid" || resolved.PreparedRecipe == original.PreparedRecipe || recipe == "" {
		t.Fatal("logical authority changed")
	}
	derived, err := c.InstalledRuntimeRecipe(recipe)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := afterOwnerRecipeMetadata(derived)
	if err != nil {
		t.Fatal(err)
	}
	m, err := releaseowner.DecodeAfterOwnerMetadata(metadata)
	if err != nil || m.Original != nativeValueDigest(original) || m.Producer != completed.Producer || m.ProducerTerminal != completed.Terminal {
		t.Fatal("resolution provenance", err)
	}
	if err := c.ApplyActivation(ctx, resolved); err != nil {
		t.Fatal("original payload apply", err)
	}
	if state.writes == 0 {
		t.Fatal("selected config payload was not applied")
	}
	values, err := c.CaptureActivation(ctx, original.Owner, true)
	if err != nil {
		t.Fatal("bound post-observe", err)
	}
	found := false
	for _, value := range values {
		if value.Class == original.Class {
			found = true
			if value.Source != resolved.Source || value.Desired != resolved.Desired || value.Before != resolved.Before || value.State != "current" {
				t.Fatal("post-observe replaced measured baseline")
			}
		}
	}
	if !found {
		t.Fatal("selected view missing")
	}
	// A new component session starts with the actual converged filesystem, then
	// binds the SAME original body and immutable physical record in that order.
	fresh := &releaseOwnerComponents{cli: c.cli, root: c.root, context: c.context, inputs: map[string]config.PersistentFileSnapshot{}, overlays: map[string]config.PersistentFileSnapshot{}, loaded: map[string]config.Loaded{}, activation: map[string]releaseActivationPrepared{}, installedBound: map[string]bool{}}
	if _, _, err := fresh.CaptureInventory(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.CaptureActivation(ctx, original.Owner, true); err != nil {
		t.Fatal(err)
	}
	if err := fresh.BindInstalledRuntimeRecipeForTarget(ctx, original.PreparedRecipe, raw, original); err != nil {
		t.Fatal("fresh original body", err)
	}
	completed2 := bindAfterOwnerFilesResults(t, fresh, original)
	if completed2 != completed {
		t.Fatal("fixture producer changed")
	}
	if err := fresh.BindAfterOwnerCompletion(completed); err != nil {
		t.Fatal(err)
	}
	if err := fresh.BindInstalledRuntimeRecipeForTarget(ctx, recipe, derived, resolved); err != nil {
		t.Fatal("fresh selected view", err)
	}
	values, err = fresh.CaptureActivation(ctx, original.Owner, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		if value.Class == original.Class && (value.Source != resolved.Source || value.Desired != resolved.Desired || value.Before != resolved.Before || value.State != "current") {
			t.Fatal("fresh helper adopted current mixture")
		}
	}
	if !bytes.Equal(raw, c.afterOwnerOriginals[original.PreparedRecipe].prepared.nativeInstalled.Recipe()) {
		t.Fatal("original source recipe mutated")
	}
	if path := os.Getenv("SUBYARD_AFTER_OWNER_CORPUS"); path != "" {
		rows := []any{map[string]any{"name": "materialized-files", "original": map[string]any{"raw": raw, "sha256": original.PreparedRecipe, "target": original}, "resolved": map[string]any{"raw": derived, "sha256": recipe, "target": resolved}, "producer": completed.Producer, "producerTerminal": completed.Terminal, "ownerOrdinal": 1}}
		body, _ := json.Marshal(rows)
		testkit.WriteFile(t, path, body, 0600)
	}
	t.Cleanup(func() { _ = fresh.Close() })
}
func TestAfterOwnerFilesSourceUUIDAndNoopGuards(t *testing.T) {
	for _, change := range []string{"uuid", "source", "missing-completion"} {
		t.Run(change, func(t *testing.T) {
			c, incus, _, original, _ := afterOwnerFilesFixture(t)
			completed := bindAfterOwnerFilesResults(t, c, original)
			instance := ports.InstanceInfo{Name: original.Instance, Project: original.Project, Status: "Running", Config: map[string]string{"volatile.uuid": "original-created-uuid", "user.subyard.owner_create": strings.Repeat("1", 32)}}
			if change == "uuid" {
				instance.Config["volatile.uuid"] = "foreign-instance"
			}
			incus.Instances[original.Project+"/"+original.Instance] = instance
			incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: instance}
			if change == "source" {
				testkit.WriteFile(t, filepath.Join(c.context.RepositoryRoot, "config/agents/sample/rules"), []byte("source B\n"), 0755)
			}
			if change == "missing-completion" {
				c.ownerResultReader = nil
			}
			if _, _, err := c.ResolveAfterOwner(context.Background(), original, completed); err == nil {
				t.Fatal("unproved conditional accepted", change)
			}
		})
	}
	c, _, _, _, _ := afterOwnerFilesFixture(t)
	if c.needsAfterOwner("default") {
		t.Fatal("ordinary owner no-op manufactured condition")
	}
	c.ownerChildren = &preparedOwnerChildren{before: releaseowner.OwnerBefore{State: string(testyardmigration.StateCurrent)}, inits: map[string]preparedOwnerInitChild{testyardmigration.CurrentYard: {execution: &initExecution{}}}}
	if c.needsAfterOwner(testyardmigration.CurrentYard) {
		t.Fatal("current owner manufactured invocation")
	}
}

func TestAfterOwnerRuntimeCLIUsesOriginalHandlerAndGeneration(t *testing.T) {
	ctx := context.Background()
	c, _ := nativeActivationSourceFixture(t)
	sourceRoot := repositoryRoot(t)
	if err := filepath.WalkDir(filepath.Join(sourceRoot, "scripts"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(c.context.RepositoryRoot, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		testkit.WriteFile(t, target, body, 0755)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(c.context.RepositoryRoot, "config/profiles/synthetic")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.context.DataHome, 0700); err != nil {
		t.Fatal(err)
	}
	generation := filepath.Join(c.context.DataHome, "physical-generation")
	testkit.WriteFile(t, generation, []byte("0"), 0600)
	old, newDigest := strings.Repeat("a", 64), strings.Repeat("b", 64)
	script := `#!/bin/sh
set -eu
case "$1" in
 observe) printf '{"state":"stale","actual":"%s","desired":"%s"}\n' "$OLD" "$NEW";;
 custody-observe) printf '{"generation":"%s"}\n' "$(cat "$GENERATION")";;
 *) exit 93;;
esac
`
	testkit.WriteFile(t, filepath.Join(root, "runtime.sh"), []byte(script), 0755)
	runtime := reconcileruntime.Runtime{RepositoryRoot: c.context.RepositoryRoot, Profiles: []profile.Definition{{SchemaVersion: 1, Name: "synthetic", Root: root, Runtime: &profile.RuntimeHook{ActivationID: "synthetic-runtime", Handler: "runtime.sh", Custody: true}}}, Integrations: []profile.IntegrationDefinition{}, Environment: []string{"PATH=/usr/bin:/bin", "OLD=" + old, "NEW=" + newDigest, "GENERATION=" + generation}, Yard: domain.Context{YardName: "default", IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1000, Paths: domain.RuntimePaths{DataHome: c.context.DataHome, ConfigHome: c.context.ConfigHome, RepositoryRoot: c.context.RepositoryRoot}}, Incus: c.cli.options.Incus.(*testkit.Incus), Executor: c.cli.options.Incus.(*testkit.Incus)}
	source, err := runtime.SelectedRuntimeInputs("synthetic-runtime")
	if err != nil {
		t.Fatal(err)
	}
	original := releaseowner.Target{Owner: "synthetic-runtime", Class: "profile-runtime", Yard: "default", Project: "subyard", Instance: "yard", InstanceUUID: "absent", State: "absent", Actual: old, Desired: newDigest, Source: source}
	prepared, err := c.prepareAfterOwnerRuntime(ctx, releaseActivationPrepared{value: original, platform: runtime})
	if err != nil {
		t.Fatal(err)
	}
	original = prepared.value
	c.activation[activationKey(original)] = prepared
	raw := prepared.nativeInstalled.Recipe()
	if err := c.BindInstalledRuntimeRecipeForTarget(ctx, original.PreparedRecipe, raw, original); err != nil {
		t.Fatal(err)
	}
	completed := bindAfterOwnerFilesResults(t, c, original)
	incus := c.cli.options.Incus.(*testkit.Incus)
	instance := ports.InstanceInfo{Name: "yard", Project: "subyard", Status: "Running", Config: map[string]string{"volatile.uuid": "original-created-uuid", "user.subyard.owner_create": strings.Repeat("1", 32)}}
	incus.Instances["subyard/yard"] = instance
	incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: instance}
	resolved, recipe, err := c.ResolveAfterOwner(ctx, original, completed)
	if err != nil {
		t.Fatal("runtime physical resolution", err)
	}
	if resolved.Source != original.Source || resolved.AfterOwner != original.AfterOwner || resolved.ReportedDesired != newDigest || resolved.Before == "" {
		t.Fatal("runtime source/generation not retained")
	}
	if _, err := c.observeAfterOwnerViews(ctx, original.Owner, []releaseowner.Target{resolved}); err != nil {
		t.Fatal("selected cached view", err)
	}
	testkit.WriteFile(t, generation, []byte(strings.Repeat("e", 64)), 0600)
	if _, err := c.observeAfterOwnerViews(ctx, original.Owner, []releaseowner.Target{resolved}); err == nil {
		t.Fatal("foreign generation replaced original fence")
	}
	testkit.WriteFile(t, generation, []byte("0"), 0600)
	derived, err := c.InstalledRuntimeRecipe(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("SUBYARD_AFTER_OWNER_RUNTIME_CORPUS"); path != "" {
		rows := []any{map[string]any{"name": "runtime", "original": map[string]any{"raw": raw, "sha256": original.PreparedRecipe, "target": original}, "resolved": map[string]any{"raw": derived, "sha256": recipe, "target": resolved}, "producer": completed.Producer, "producerTerminal": completed.Terminal, "ownerOrdinal": 1}}
		body, _ := json.Marshal(rows)
		testkit.WriteFile(t, path, body, 0600)
	}
	testkit.WriteFile(t, filepath.Join(root, "runtime.sh"), []byte("#!/bin/sh\nexit 94\n"), 0755)
	if _, err := c.observeAfterOwnerViews(ctx, original.Owner, []releaseowner.Target{resolved}); err == nil {
		t.Fatal("changed handler B became selected authority")
	}
}

type afterOwnerCLIIntegrationInventory struct{ *nativeMaterializedState }

func (s *afterOwnerCLIIntegrationInventory) Exec(ctx context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	result, err := s.nativeMaterializedState.Exec(ctx, project, name, request)
	if err == nil && len(request.Command) >= 5 && request.Command[0] == "python3" && strings.Contains(request.Command[3], "Version 1 records observed ownership only") && request.Command[4] == "observe" {
		var input struct {
			Entries []struct{ Kind, Path, ID string }
		}
		_ = json.Unmarshal(request.Stdin, &input)
		var body map[string]any
		_ = json.Unmarshal(result.Stdout, &body)
		facts := []any{}
		for _, entry := range input.Entries {
			path := entry.Path
			if path == "" {
				path = entry.ID
			}
			facts = append(facts, map[string]any{"key": entry.Kind + ":" + path, "desired": strings.Repeat("a", 64), "observed": strings.Repeat("b", 64), "at_desired": false})
		}
		body["facts"] = facts
		result.Stdout, _ = json.Marshal(body)
	}
	return result, err
}
func TestAfterOwnerIntegrationsCLIOriginalResolveDerivedBind(t *testing.T) {
	ctx := context.Background()
	c, incus, state, files, _ := afterOwnerFilesFixture(t)
	originalPrepared := c.activation[activationKey(files)]
	runtime := originalPrepared.platform.(reconcileruntime.Runtime)
	c.cli.options.Executor = &afterOwnerCLIIntegrationInventory{state}
	runtime.Executor = c.cli.options.Executor
	c.ownerChildren = &preparedOwnerChildren{inits: map[string]preparedOwnerInitChild{}}
	prepared, err := c.prepareAfterOwnerIntegration(ctx, originalPrepared.configTarget, runtime, files.Source)
	if err != nil {
		t.Fatal("constructor", err)
	}
	original := prepared.value
	c.activation[activationKey(original)] = prepared
	raw := prepared.nativeInstalled.Recipe()
	if err = c.BindInstalledRuntimeRecipeForTarget(ctx, original.PreparedRecipe, raw, original); err != nil {
		t.Fatal("original bind", err)
	}
	completed := bindAfterOwnerFilesResults(t, c, original)
	instance := ports.InstanceInfo{Name: original.Instance, Project: original.Project, Status: "Running", Config: map[string]string{"volatile.uuid": "original-created-uuid", "user.subyard.owner_create": strings.Repeat("1", 32)}}
	incus.Instances[original.Project+"/"+original.Instance] = instance
	incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: instance}
	resolved, hash, err := c.ResolveAfterOwner(ctx, original, completed)
	if err != nil {
		t.Fatal("resolve", err)
	}
	derived, err := c.InstalledRuntimeRecipe(hash)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.BindInstalledRuntimeRecipeForTarget(ctx, hash, derived, resolved); err != nil {
		t.Fatal("derived bind", err)
	}
	values, err := c.observeAfterOwnerViews(ctx, original.Owner, []releaseowner.Target{resolved})
	if err != nil || len(values) != 1 || values[0].Before != resolved.Before || values[0].Source != original.Source {
		t.Fatal("selected observation", err)
	}
}

func afterOwnerUUIDFixture(t *testing.T, kind string) (*releaseOwnerComponents, *testkit.Incus, *nativeMaterializedState, releaseowner.Target, []byte) {
	t.Helper()
	c, incus, state, original, raw := afterOwnerFilesFixture(t)
	if kind == "materialized-integrations" {
		prepared := c.activation[activationKey(original)]
		runtime := prepared.platform.(reconcileruntime.Runtime)
		c.cli.options.Executor = &afterOwnerCLIIntegrationInventory{state}
		runtime.Executor = c.cli.options.Executor
		c.ownerChildren = &preparedOwnerChildren{inits: map[string]preparedOwnerInitChild{}}
		var err error
		prepared, err = c.prepareAfterOwnerIntegration(context.Background(), prepared.configTarget, runtime, original.Source)
		if err != nil {
			t.Fatal(err)
		}
		original, raw = prepared.value, prepared.nativeInstalled.Recipe()
		c.activation[activationKey(original)] = prepared
		if err := c.BindInstalledRuntimeRecipeForTarget(context.Background(), original.PreparedRecipe, raw, original); err != nil {
			t.Fatal(err)
		}
	}
	return c, incus, state, original, raw
}

type afterOwnerUUIDReplacement struct {
	*testkit.Incus
	original, replacement ports.InstanceInfo
	reads                 int
}

func (r *afterOwnerUUIDReplacement) Instance(ctx context.Context, project, instance string) (ports.InstanceInfo, error) {
	if project != r.original.Project || instance != r.original.Name {
		return r.Incus.Instance(ctx, project, instance)
	}
	r.reads++
	if r.reads == 1 {
		return r.original, nil
	}
	return r.replacement, nil
}

func TestAfterOwnerConfigsRejectUUIDReplacementBetweenReads(t *testing.T) {
	for _, kind := range []string{"materialized-files", "materialized-integrations"} {
		t.Run(kind, func(t *testing.T) {
			c, incus, state, original, _ := afterOwnerUUIDFixture(t, kind)
			completed := bindAfterOwnerFilesResults(t, c, original)
			// The owner-result read sees A; the independent physical capture sees B.
			a := ports.InstanceInfo{Name: original.Instance, Project: original.Project, Status: "Running", Config: map[string]string{"volatile.uuid": "original-created-uuid", "user.subyard.owner_create": strings.Repeat("1", 32)}}
			b := a
			b.Config = map[string]string{"volatile.uuid": "replacement-uuid", "user.subyard.owner_create": strings.Repeat("1", 32)}
			incus.Instances[original.Project+"/"+original.Instance] = a
			incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: b}
			reader := &afterOwnerUUIDReplacement{Incus: incus, original: a, replacement: b}
			c.cli.options.Incus = reader
			bound := c.afterOwnerOriginals[original.PreparedRecipe]
			runtime := bound.prepared.platform.(reconcileruntime.Runtime)
			runtime.Incus = reader
			bound.prepared.platform = runtime
			c.afterOwnerOriginals[original.PreparedRecipe] = bound
			recipes, closers := len(c.installedRecipes), len(c.closers)
			if _, _, err := c.ResolveAfterOwner(context.Background(), original, completed); err == nil {
				t.Fatal("replacement physical UUID accepted for original owner result")
			}
			if len(c.afterOwnerViews) != 0 || len(c.installedRecipes) != recipes || len(c.closers) != closers || state.writes != 0 || len(incus.ConfigUpdates) != 0 {
				t.Fatal("refused UUID mismatch registered a recipe or wrote native state")
			}
		})
	}
}

func TestAfterOwnerConfigsFreshReplayRejectsMetadataUUIDMismatch(t *testing.T) {
	for _, kind := range []string{"materialized-files", "materialized-integrations"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			c, incus, state, original, raw := afterOwnerUUIDFixture(t, kind)
			completed := bindAfterOwnerFilesResults(t, c, original)
			a := ports.InstanceInfo{Name: original.Instance, Project: original.Project, Status: "Running", Config: map[string]string{"volatile.uuid": "original-created-uuid", "user.subyard.owner_create": strings.Repeat("1", 32)}}
			incus.Instances[original.Project+"/"+original.Instance] = a
			incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: a}
			resolved, hash, err := c.ResolveAfterOwner(ctx, original, completed)
			if err != nil {
				t.Fatal(err)
			}
			derived, err := c.InstalledRuntimeRecipe(hash)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := afterOwnerRecipeMetadata(derived)
			if err != nil {
				t.Fatal(err)
			}
			m, err := releaseowner.DecodeAfterOwnerMetadata(metadata)
			if err != nil {
				t.Fatal(err)
			}
			b := a
			b.Config = map[string]string{"volatile.uuid": "replacement-uuid"}
			incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: b}
			runtime := c.afterOwnerOriginals[original.PreparedRecipe].prepared.platform.(reconcileruntime.Runtime)
			var captured *reconcileruntime.PreparedInstalledRuntime
			if kind == "materialized-integrations" {
				captured, m.Physical, err = runtime.ResolveAfterOwnerIntegrations(ctx, raw)
			} else {
				captured, m.Physical, err = runtime.ResolveAfterOwnerMaterializedConfigs(ctx, raw)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer captured.Close()
			resolved.Before = captured.PreparedConfigs().Binding()
			if kind == "materialized-integrations" {
				resolved.Before = captured.PreparedIntegrations().Binding()
			}
			m.Delta.Before = resolved.Before
			metadata, err = json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			mismatched, err := captured.WithAfterOwnerMetadata(metadata)
			if err != nil {
				t.Fatal(err)
			}
			defer mismatched.Close()
			resolved.PreparedRecipe = mismatched.Binding()
			fresh := &releaseOwnerComponents{cli: c.cli, root: c.root, context: c.context, inputs: map[string]config.PersistentFileSnapshot{}, overlays: map[string]config.PersistentFileSnapshot{}, loaded: map[string]config.Loaded{}, activation: map[string]releaseActivationPrepared{}, installedBound: map[string]bool{}}
			defer fresh.Close()
			if _, _, err := fresh.CaptureInventory(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := fresh.CaptureActivation(ctx, original.Owner, true); err != nil {
				t.Fatal(err)
			}
			if err := fresh.BindInstalledRuntimeRecipeForTarget(ctx, original.PreparedRecipe, raw, original); err != nil {
				t.Fatal(err)
			}
			bindAfterOwnerFilesResults(t, fresh, original)
			if err := fresh.BindAfterOwnerCompletion(completed); err != nil {
				t.Fatal(err)
			}
			recipes, closers := len(fresh.installedRecipes), len(fresh.closers)
			if err := fresh.BindInstalledRuntimeRecipeForTarget(ctx, mismatched.Binding(), mismatched.Recipe(), resolved); err == nil {
				t.Fatal("fresh replay accepted target A with physical recipe B")
			}
			if len(fresh.afterOwnerViews) != 0 || len(fresh.installedRecipes) != recipes || len(fresh.closers) != closers || state.writes != 0 || len(incus.ConfigUpdates) != 0 {
				t.Fatal("refused replay registered a recipe or wrote native state")
			}
		})
	}
}
