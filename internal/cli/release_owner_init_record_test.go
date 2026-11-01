package cli

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestOwnerInitRecordFreshProcess(t *testing.T) {
	loaded := config.Loaded{Context: domain.Context{YardName: "test-yard", Paths: domain.RuntimePaths{ConfigHome: "/owner/config", DataHome: "/owner/data", RepositoryRoot: "/retained"}}, Environment: map[string]string{"EXTERNAL": "/payload-\xff"}, Settings: map[string]config.SettingTrace{}}
	runtime := reconcileruntime.Runtime{RepositoryRoot: "/retained", Yard: loaded.Context}
	if path := os.Getenv("SUBYARD_TEST_OWNER_INIT_RECORD"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := restoreOwnerInitRecord(raw, loaded, runtime)
		if err != nil {
			t.Fatal(err)
		}
		again, err := captureOwnerInitRecord(restored)
		if err != nil || !bytes.Equal(raw, again) || restored.loaded.Environment["EXTERNAL"] != "/payload-\xff" {
			t.Fatal("fresh process changed original capsule", err)
		}
		return
	}
	plan := application.ReconcilePlan{Steps: []application.ReconcileStep{{Stage: application.ReconcileStage{ID: "instance", Label: "original"}, Conditional: true}}}
	original := &initExecution{loaded: loaded, mode: initReconcile, platform: runtime, plan: plan, approvedPlan: plan, approvedFinalize: application.ReconcilePlan{Steps: []application.ReconcileStep{{Stage: application.FinalizeStage(), Conditional: true}}}, powerYards: []domain.Context{loaded.Context}, integrationBaseline: &initIntegrationBaseline{Settings: map[string]config.SettingTrace{}, Sources: map[string]initIntegrationSource{}, Packages: map[string]string{"external-\xff": "sha"}}}
	raw, err := captureOwnerInitRecord(original)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(testkit.TempDir(t), "original.json")
	testkit.WriteFile(t, path, raw, 0600)
	cmd := exec.Command(os.Args[0], "-test.run=^TestOwnerInitRecordFreshProcess$")
	cmd.Env = append(os.Environ(), "SUBYARD_TEST_OWNER_INIT_RECORD="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh process restore: %v\n%s", err, output)
	}
}

func TestOwnerInitRecordKeepsOriginalPlanAndLosslessInputs(t *testing.T) {
	loaded := config.Loaded{Context: domain.Context{YardName: "test-yard", Paths: domain.RuntimePaths{ConfigHome: "/owner-\xff/config", DataHome: "/owner/data", RepositoryRoot: "/retained"}}, Environment: map[string]string{"EXTERNAL": "/payload-\xff"}, Settings: map[string]config.SettingTrace{}}
	runtime := reconcileruntime.Runtime{RepositoryRoot: "/retained", Yard: loaded.Context}
	plan := application.ReconcilePlan{Steps: []application.ReconcileStep{{Stage: application.ReconcileStage{ID: "instance", Label: "original"}, Conditional: true}}}
	original := &initExecution{loaded: loaded, mode: initReconcile, platform: runtime, plan: plan, approvedPlan: plan, approvedFinalize: application.ReconcilePlan{Steps: []application.ReconcileStep{{Stage: application.FinalizeStage(), Conditional: true}}}, hostID: "host-original", powerYards: []domain.Context{loaded.Context}, integrationBaseline: &initIntegrationBaseline{Settings: map[string]config.SettingTrace{}, Sources: map[string]initIntegrationSource{}, Packages: map[string]string{"external-\xff": "sha"}}}
	raw, err := captureOwnerInitRecord(original)
	if err != nil {
		t.Fatal(err)
	}
	var description ownerInitRecord
	if err := nativejson.DecodeLosslessNativeJSON(raw, &description, 32<<20); err != nil {
		t.Fatal(err)
	}
	saved, err := description.Input.restore(loaded.Catalog)
	if err != nil || !reflect.DeepEqual(captureOwnerLoaded(saved), captureOwnerLoaded(loaded)) {
		t.Fatal("original loaded input graph changed", err)
	}
	restored, err := restoreOwnerInitRecord(raw, loaded, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.approvedPlan, plan) || !reflect.DeepEqual(restored.powerYards, original.powerYards) || restored.integrationBaseline.Packages["external-\xff"] != "sha" {
		t.Fatal("original authority changed")
	}
	again, err := captureOwnerInitRecord(restored)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("original capsule changed", err)
	}
	loaded.Environment["EXTERNAL"] = "/payload-\xfe"
	if _, err := restoreOwnerInitRecord(raw, loaded, runtime); err == nil {
		t.Fatal("byte-distinct source environment admitted")
	}
}

func TestOwnerInitRecordUsesStableCheckedRepositoryLabels(t *testing.T) {
	logical := "/retained/release"
	firstRoot, nextRoot := "/proc/self/fd/41", "/proc/self/fd/47"
	loaded := config.Loaded{Context: domain.Context{YardName: "e2e-yard", Paths: domain.RuntimePaths{RepositoryRoot: firstRoot, ConfigDir: firstRoot + "/config", ConfigHome: "/owner/config", DataHome: "/owner/data"}}, Environment: map[string]string{"SOURCE": firstRoot + "/config/file-\xff", "HOOKS": "/proc/" + strconv.Itoa(os.Getpid()) + "/fd/41/config/hook", "FOREIGN": "x" + firstRoot + "/config/foreign"}, Settings: map[string]config.SettingTrace{}}
	ownedPID := "/proc/" + strconv.Itoa(os.Getpid()) + "/fd/41"
	loaded.Settings = map[string]config.SettingTrace{"FILE": {Kind: config.SettingFile, Type: config.SettingRegularFilePath, EffectiveValue: ownedPID + "/config/file-\xff", Resolutions: []config.SettingResolution{{Path: ownedPID + "/config/integration.json", Value: ownedPID + "/config/file-\xff"}}}}
	runtime := reconcileruntime.Runtime{RepositoryRoot: firstRoot, LogicalRepositoryRoot: logical, Environment: []string{"ROOT_DIR=" + firstRoot}, Yard: loaded.Context}
	original := &initExecution{loaded: loaded, mode: initReconcile, platform: runtime, powerYards: []domain.Context{loaded.Context}, integrationBaseline: &initIntegrationBaseline{Settings: loaded.Settings, Sources: map[string]initIntegrationSource{ownedPID + "/config/file-\xff": {}}, Packages: map[string]string{}}}
	fact := initIntegrationSource{Exists: true, Digest: [32]byte{1}, Device: 2, Inode: 3, Mode: 0600}
	original.integrationBaseline.Sources = map[string]initIntegrationSource{ownedPID + "/config/file-\xff": fact, firstRoot + "/config/file-\xff": fact, "/q-external/contract": {}}
	raw, err := captureOwnerInitRecord(original)
	if err != nil {
		t.Fatal(err)
	}
	var record ownerInitRecord
	if err := nativejson.DecodeLosslessNativeJSON(raw, &record, 32<<20); err != nil {
		t.Fatal(err)
	}
	if len(record.Baseline.Sources) != 2 || record.Baseline.Sources[0].Path != "/q-external/contract" || record.Baseline.Sources[1].Path != logical+"/config/file-\xff" {
		t.Fatal("original baseline source aliases are not canonically sorted/coalesced")
	}
	reversed := *original
	baseline := *original.integrationBaseline
	baseline.Sources = map[string]initIntegrationSource{}
	for _, key := range []string{"/q-external/contract", firstRoot + "/config/file-\xff", ownedPID + "/config/file-\xff"} {
		baseline.Sources[key] = original.integrationBaseline.Sources[key]
	}
	reversed.integrationBaseline = &baseline
	same, err := captureOwnerInitRecord(&reversed)
	if err != nil || !bytes.Equal(raw, same) {
		t.Fatal("source map insertion order changed original capsule", err)
	}
	for _, field := range []string{"exists", "digest", "device", "inode", "mode"} {
		t.Run("alias-conflict-"+field, func(t *testing.T) {
			copy := *original
			b := *original.integrationBaseline
			b.Sources = maps.Clone(b.Sources)
			different := fact
			switch field {
			case "exists":
				different.Exists = false
			case "digest":
				different.Digest[0]++
			case "device":
				different.Device++
			case "inode":
				different.Inode++
			case "mode":
				different.Mode = 0644
			}
			b.Sources[firstRoot+"/config/file-\xff"] = different
			copy.integrationBaseline = &b
			if _, err := captureOwnerInitRecord(&copy); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("conflicting original source evidence accepted", err)
			}
		})
	}
	if record.Input.Context.Paths.RepositoryRoot != logical {
		t.Fatal("physical descriptor leaked into original capsule")
	}
	freshLoaded, err := record.Input.restore(loaded.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	if freshLoaded.Environment["FOREIGN"] != "x"+firstRoot+"/config/foreign" {
		t.Fatal("embedded foreign path label collapsed")
	}
	freshLoaded = ownerLoadedLabels(freshLoaded, logical, nextRoot)
	freshRuntime := reconcileruntime.Runtime{RepositoryRoot: nextRoot, LogicalRepositoryRoot: logical, Environment: []string{"ROOT_DIR=" + nextRoot}, Yard: freshLoaded.Context}
	restored, err := restoreOwnerInitRecord(raw, freshLoaded, freshRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := restored.integrationBaseline.Sources[nextRoot+"/config/file-\xff"]; !ok {
		t.Fatal("original source role not restored at current retained root")
	}
	for _, name := range []string{"duplicate-equal", "duplicate-conflict", "reversed-order"} {
		t.Run(name, func(t *testing.T) {
			var changed ownerInitRecord
			if err := nativejson.DecodeLosslessNativeJSON(raw, &changed, 32<<20); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "duplicate-equal", "duplicate-conflict":
				row := changed.Baseline.Sources[1]
				if name == "duplicate-conflict" {
					row.Source.Mode++
				}
				changed.Baseline.Sources = append(changed.Baseline.Sources, row)
			case "reversed-order":
				changed.Baseline.Sources[0], changed.Baseline.Sources[1] = changed.Baseline.Sources[1], changed.Baseline.Sources[0]
			}
			foreign, err := nativejson.EncodeLosslessNativeJSON(changed, 32<<20)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restoreOwnerInitRecord(foreign, freshLoaded, freshRuntime); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("noncanonical original source rows accepted", err)
			}
		})
	}
	drift := freshLoaded
	drift.Settings = maps.Clone(freshLoaded.Settings)
	trace := drift.Settings["FILE"]
	trace.EffectiveValue += ".changed"
	drift.Settings["FILE"] = trace
	if _, err := restoreOwnerInitRecord(raw, drift, freshRuntime); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("source setting drift accepted", err)
	}
	if restored.powerYards[0].Paths.RepositoryRoot != nextRoot {
		t.Fatal("original peer role retained dead descriptor spelling")
	}
	if _, ok := restored.integrationBaseline.Sources[firstRoot+"/config/file-\xff"]; ok {
		t.Fatal("old descriptor retained execution authority")
	}
}

func TestOwnerInitBaselineReloadsRetainedPackageInputs(t *testing.T) {
	for _, fault := range []string{"unchanged", "digest", "inode", "mode", "literal", "presence", "foreign-root", "published-alias", "published-digest", "published-conflict", "published-foreign"} {
		t.Run(fault, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			directory := filepath.Join(root, "config/agents/sample")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(directory, "integration.json"), []byte(`{"schema_version":2,"artifacts":[{"id":"config","kind":"config","source":"config.json","destination":".sample/config.json","format":"destination"}]}`), 0600)
			asset := filepath.Join(directory, "config.json")
			testkit.WriteFile(t, asset, []byte("{}\n"), 0600)
			first, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			second, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			load := func(file *os.File) (*CLI, config.Loaded, reconcileruntime.Runtime) {
				t.Helper()
				physical := "/proc/self/fd/" + strconv.FormatUint(uint64(file.Fd()), 10)
				program, err := New(Options{RepositoryRoot: physical, Environment: environment})
				if err != nil {
					t.Fatal(err)
				}
				loaded, err := program.loadContext("default")
				if err != nil {
					t.Fatal(err)
				}
				return program, loaded, reconcileruntime.Runtime{RepositoryRoot: physical, LogicalRepositoryRoot: root, Environment: environment, Yard: loaded.Context}
			}
			_, loaded, runtime := load(first)
			baseline, err := captureInitIntegrationBaseline(loaded)
			if err != nil {
				t.Fatal(err)
			}
			trace := loaded.Settings["AGENT_sample_CONFIG"]
			childPath := "/proc/" + strconv.Itoa(os.Getpid()) + "/fd/" + strconv.FormatUint(uint64(first.Fd()), 10) + "/config/agents/sample/config.json"
			if trace.Kind != config.SettingFile || trace.Type != config.SettingRegularFilePath || trace.EffectiveValue != childPath {
				t.Fatal("checked package did not establish the distinct child-safe source spelling")
			}
			raw, err := captureOwnerInitRecord(&initExecution{loaded: loaded, platform: runtime, integrationBaseline: baseline})
			if err != nil {
				t.Fatal(err)
			}
			program, fresh, next := load(second)
			restored, err := restoreOwnerInitRecord(raw, fresh, next)
			if err != nil {
				t.Fatal("restore checked original", err)
			}
			if strings.HasPrefix(fault, "published-") {
				path := next.RepositoryRoot + "/config/agents.env"
				fact, tracked := restored.integrationBaseline.Sources[path]
				if !tracked {
					t.Fatal("fixture source publication is not tracked")
				}
				alias := "/proc/" + strconv.Itoa(os.Getpid()) + "/fd/" + strconv.FormatUint(uint64(second.Fd()), 10) + "/config/agents.env"
				restored.integrationBaseline.Sources[alias] = fact
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				content = append(content, []byte("ENVIRONMENT_PROFILES=''\n")...)
				if fault == "published-conflict" {
					fact.Digest[0]++
					restored.integrationBaseline.Sources[alias] = fact
				} else if fault == "published-foreign" {
					foreign, err := os.Open(root)
					if err != nil {
						t.Fatal(err)
					}
					defer foreign.Close()
					alias = "/proc/self/fd/" + strconv.FormatUint(uint64(foreign.Fd()), 10) + "/config/agents.env"
				} else {
					testkit.WriteFile(t, path, content, 0600)
				}
				before := maps.Clone(restored.integrationBaseline.Sources)
				expected := content
				if fault == "published-digest" {
					expected = []byte("not the authorized publication\n")
				}
				err = restored.integrationBaseline.acceptPublishedSource(alias, expected, next.RepositoryRoot)
				if fault == "published-digest" || fault == "published-conflict" {
					if !errors.Is(err, domain.ErrPlanStale) || !maps.Equal(before, restored.integrationBaseline.Sources) {
						t.Fatal("invalid publication changed original facts", err)
					}
					return
				}
				if err != nil {
					t.Fatal("checked source publication", err)
				}
				if fault == "published-foreign" {
					if !maps.Equal(before, restored.integrationBaseline.Sources) {
						t.Fatal("untracked foreign alias changed original facts")
					}
				} else {
					observed, err := readInitIntegrationSource(path)
					if err != nil {
						t.Fatal(err)
					}
					for original, value := range before {
						if original == path || original == alias {
							value = observed
						}
						if restored.integrationBaseline.Sources[original] != value {
							t.Fatal("publication changed unrelated evidence or omitted tracked alias")
						}
					}
				}
			}
			switch fault {
			case "digest":
				testkit.WriteFile(t, asset, []byte("{\"changed\":true}\n"), 0600)
			case "inode":
				testkit.WriteFile(t, asset+".next", []byte("{}\n"), 0600)
				if err := os.Rename(asset+".next", asset); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(asset, 0644); err != nil {
					t.Fatal(err)
				}
			case "literal", "presence":
				content := "AGENT_sample_COMMAND=changed\n"
				if fault == "presence" {
					content = "AGENT_sample_COMMAND=''\n"
				}
				testkit.WriteFile(t, filepath.Join(root, "config/agents.env"), []byte("CODING_TOOL_INTEGRATIONS=\n"+content), 0600)
			case "foreign-root":
				path := next.RepositoryRoot + "/config/agents/sample/config.json"
				fact, tracked := restored.integrationBaseline.Sources[path]
				if !tracked {
					t.Fatal("fixture source is not tracked")
				}
				delete(restored.integrationBaseline.Sources, path)
				restored.integrationBaseline.Sources["/proc/self/fd/999999/config/agents/sample/config.json"] = fact
			}
			if fault == "digest" || fault == "inode" || fault == "mode" {
				before, tracked := restored.integrationBaseline.Sources[next.RepositoryRoot+"/config/agents/sample/config.json"]
				after, err := readInitIntegrationSource(asset)
				if err != nil || !tracked {
					t.Fatal("fixture source facts", err)
				}
				if fault == "digest" && before.Digest == after.Digest || fault == "inode" && before.Inode == after.Inode || fault == "mode" && before.Mode == after.Mode {
					t.Fatal("native source drift not established")
				}
			}
			err = restored.checkIntegrationBaseline(program)
			if fault == "unchanged" || fault == "published-alias" || fault == "published-foreign" {
				if err != nil {
					t.Fatal("unchanged checked retained baseline", err)
				}
			} else if err == nil {
				t.Fatal("changed original source admitted")
			}
		})
	}
}

func TestOwnerLoadedSnapshotRefusesInconsistentMapPresence(t *testing.T) {
	loaded := config.Loaded{Environment: map[string]string{}, Integrations: config.IntegrationSelection{DependencyReasons: map[string][]string{"selected": {"reason"}}}}
	record := captureOwnerLoaded(loaded)
	record.Selection.ReasonsPresent = false
	if _, err := record.restore(loaded.Catalog); err == nil {
		t.Fatal("inconsistent original map presence admitted")
	}
	record = captureOwnerLoaded(loaded)
	record.EnvironmentPresent = false
	record.Environment = []ownerStringPair{{Name: []byte("A"), Value: []byte("B")}}
	if _, err := record.restore(loaded.Catalog); err == nil {
		t.Fatal("inconsistent original environment presence admitted")
	}
}

func TestOwnerInitRecordKeepsPreconsentPublicationPlan(t *testing.T) {
	root := testkit.TempDir(t)
	for _, path := range []string{"config/systemd", "bin"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	template, err := os.ReadFile("../../config/systemd/subyard-publication-reconcile.service.in")
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/systemd/subyard-publication-reconcile.service.in"), template, 0644)
	powerTemplate, err := os.ReadFile("../../config/systemd/subyard-power-reconcile.service.in")
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/systemd/subyard-power-reconcile.service.in"), powerTemplate, 0644)
	engine := filepath.Join(root, "bin/yard-engine")
	testkit.WriteFile(t, engine, []byte("approved engine"), 0755)
	loaded := config.Loaded{Context: domain.Context{YardName: "test-yard", Paths: domain.RuntimePaths{ConfigHome: filepath.Join(root, "config-home"), DataHome: filepath.Join(root, "data-home"), RepositoryRoot: root}}, Environment: map[string]string{}, Settings: map[string]config.SettingTrace{}}
	runtime := reconcileruntime.Runtime{RepositoryRoot: root, Yard: loaded.Context, Environment: []string{"SUBYARD_POWER_ENGINE_SOURCE=" + engine, "SUBYARD_POWER_RECONCILER_PATH=" + filepath.Join(root, "installed/engine"), "SUBYARD_POWER_UNIT_PATH=" + filepath.Join(root, "installed/power.service")}}
	publication, err := runtime.PrepareOwnerPublication(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = runtime.RestoreOwnerPublication(publication)
	if err != nil {
		t.Fatal(err)
	}
	original := &initExecution{loaded: loaded, mode: initReconcile, platform: runtime}
	raw, err := captureOwnerInitRecord(original)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restoreOwnerInitRecord(raw, loaded, runtime)
	if err != nil {
		t.Fatal(err)
	}
	actual := restored.platform.(reconcileruntime.Runtime)
	if !reflect.DeepEqual(publication, actual.OwnerPublicationRecord()) || actual.OwnerPublicationInstallation() != nil {
		t.Fatal("original plan or fresh-process capability changed")
	}
	again, err := captureOwnerInitRecord(restored)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("publication record was not canonical", err)
	}
	publication.Consumer.FilesBefore = "changed"
	if _, err := runtime.RestoreOwnerPublication(publication); err == nil {
		t.Fatal("changed preconsent identity accepted")
	}
}

func TestOwnerLoadedLabelsKeepMutablePathAuthority(t *testing.T) {
	const logical, retained = "/release", "/proc/self/fd/47"
	loaded := config.Loaded{Context: domain.Context{Paths: domain.RuntimePaths{
		RepositoryRoot: logical, ConfigDir: logical + "/config",
		ConfigHome: logical + "/state/config", DataHome: logical + "/state/data",
		StoragePath: logical + "/custom-storage", HostBase: logical + "/custom-host",
	}}, Environment: map[string]string{
		"CONFIG":  logical + "/state/config/overrides/host",
		"DATA":    logical + "/state/data/runtime",
		"CUSTOM":  logical + "/custom-storage/image",
		"HOST":    logical + "/custom-host/tree",
		"SOURCE":  logical + "/config/source",
		"SIMILAR": logical + "/custom-storage-other/source",
	}}
	physical := ownerLoadedLabels(loaded, logical, retained)
	for _, key := range []string{"CONFIG", "DATA", "CUSTOM", "HOST"} {
		if physical.Environment[key] != loaded.Environment[key] {
			t.Fatal("mutable path changed authority", key)
		}
	}
	if physical.Environment["SOURCE"] != retained+"/config/source" || physical.Environment["SIMILAR"] != retained+"/custom-storage-other/source" || physical.Context.Paths.RepositoryRoot != retained {
		t.Fatal("immutable source lost retained authority")
	}
	if !reflect.DeepEqual(ownerLoadedLabels(physical, retained, logical), loaded) {
		t.Fatal("scoped labels did not round trip")
	}
	// The normal immutable installation is inside its data root. That ancestor
	// grants no authority to reopen the logical repository instead of the FD.
	loaded.Context.Paths.DataHome = "/"
	loaded.Context.Paths.ConfigHome = "/config"
	loaded.Context.Paths.StoragePath = "/storage"
	loaded.Context.Paths.HostBase = "/host"
	physical = ownerLoadedLabels(loaded, logical, retained)
	if physical.Environment["DATA"] != retained+"/state/data/runtime" {
		t.Fatal("ancestor data root bypassed retained source")
	}
}

func TestOwnerSemanticLabelsOnlyExactOwnedRepositoryAlias(t *testing.T) {
	const logical, self = "/approved/source", "/proc/self/fd/47"
	current := "/proc/" + strconv.Itoa(os.Getpid()) + "/fd/47"
	for _, root := range []string{self, current} {
		c := &releaseOwnerComponents{}
		c.context.RepositoryRoot = logical
		c.context.PhysicalRepositoryRoot = root
		for _, path := range []string{self, current, self + "/config/source", current + "/config/source"} {
			want := logical
			if filepath.Base(path) == "source" {
				want += "/config/source"
			}
			if got := c.ownerPathLabel(path); got != want {
				t.Errorf("exact owned alias not labelled: %q", got)
			}
		}
		for _, path := range []string{"/proc/" + strconv.Itoa(os.Getpid()+1) + "/fd/47/config/source", "/proc/self/fd/48/config/source", current + "0/config/source", self + "-other/config/source", "literal=" + current + "/config/source"} {
			if c.ownerPathLabel(path) != path {
				t.Fatal("unowned or embedded path gained source authority")
			}
		}
	}
}
func TestOwnerSettingsBindingLabelsTypedSourceValuesOnly(t *testing.T) {
	c := &releaseOwnerComponents{}
	c.context.RepositoryRoot = "/approved/source"
	c.context.PhysicalRepositoryRoot = "/proc/self/fd/47"
	freshRoot := "/proc/" + strconv.Itoa(os.Getpid()) + "/fd/47"
	settings := func(root string) map[string]config.SettingTrace {
		return map[string]config.SettingTrace{
			"FILE": {Kind: config.SettingFile, Type: config.SettingRegularFilePath, EffectiveValue: root + "/config/file", Resolutions: []config.SettingResolution{{Path: root + "/config/integration.json", Value: root + "/config/file", Detail: "literal unchanged"}}},
			"TEXT": {Kind: config.SettingScalar, Type: config.SettingString, EffectiveValue: "literal", Resolutions: []config.SettingResolution{{Path: root + "/config/integration.json", Value: "literal", Detail: "literal unchanged"}}},
		}
	}
	original := settings(c.context.RepositoryRoot)
	before := settings(c.context.RepositoryRoot)
	want, err := c.ownerSettingsBinding(original)
	if err != nil {
		t.Fatal(err)
	}
	actual := settings(freshRoot)
	if !reflect.DeepEqual(ownerSettingsLabels(original, c.ownerPathLabel), ownerSettingsLabels(actual, c.ownerPathLabel)) {
		t.Error("shared typed trace labels diverge from owner source binding")
	}
	got, err := c.ownerSettingsBinding(actual)
	if err != nil || got != want {
		t.Error("typed source aliases changed settings binding", err)
	}
	if !reflect.DeepEqual(original, before) || !reflect.DeepEqual(actual, settings(freshRoot)) {
		t.Fatal("label binding changed live settings")
	}
	for _, name := range []string{"source", "literal", "detail"} {
		t.Run(name, func(t *testing.T) {
			changed := settings(freshRoot)
			switch name {
			case "source":
				v := changed["FILE"]
				v.EffectiveValue += "-changed"
				changed["FILE"] = v
			case "literal":
				v := changed["TEXT"]
				v.Resolutions[0].Value = freshRoot + "/config/file"
				changed["TEXT"] = v
			case "detail":
				v := changed["FILE"]
				v.Resolutions[0].Detail = freshRoot + "/config/file"
				changed["FILE"] = v
			}
			digest, err := c.ownerSettingsBinding(changed)
			if err != nil || digest == want {
				t.Fatal("source or literal drift was hidden", err)
			}
		})
	}
}

func TestOwnerCanonicalInputAliasesRequireIdenticalEvidence(t *testing.T) {
	c := &releaseOwnerComponents{}
	c.context.RepositoryRoot = "/approved/source"
	c.context.PhysicalRepositoryRoot = "/proc/self/fd/47"
	paths := []string{"/proc/self/fd/47/config/file", "/proc/" + strconv.Itoa(os.Getpid()) + "/fd/47/config/file"}
	inputs := []ownerChildInput{}
	for _, path := range paths {
		inputs = append(inputs, ownerChildInput{Path: []byte(c.ownerPathLabel(path)), Fact: []byte("fact"), Projected: true, Approved: []byte("approved")})
	}
	before := append([]ownerChildInput(nil), inputs...)
	result, err := canonicalOwnerChildInputs(inputs)
	if err != nil || len(result) != 1 || !reflect.DeepEqual(result[0], inputs[0]) || !reflect.DeepEqual(inputs, before) {
		t.Fatal("equivalent aliases did not preserve exact original evidence", err)
	}
	for _, name := range []string{"fact", "fact-presence", "projected", "approved", "approved-presence", "path-presence"} {
		t.Run(name, func(t *testing.T) {
			a, b := inputs[0], inputs[1]
			switch name {
			case "fact":
				b.Fact = []byte("different")
			case "fact-presence":
				a.Fact = nil
				b.Fact = []byte{}
			case "projected":
				b.Projected = false
			case "approved":
				b.Approved = []byte("different")
			case "approved-presence":
				a.Approved = nil
				b.Approved = []byte{}
			case "path-presence":
				a.Path = nil
				b.Path = []byte{}
			}
			if _, err := canonicalOwnerChildInputs([]ownerChildInput{a, b}); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("conflicting alias evidence accepted", err)
			}
		})
	}
	for _, input := range [][]ownerChildInput{nil, {}} {
		got, err := canonicalOwnerChildInputs(input)
		if err != nil || !reflect.DeepEqual(got, input) {
			t.Fatal("empty input presence changed", err)
		}
	}
}
