package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
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
	runtime := reconcileruntime.Runtime{RepositoryRoot: firstRoot, LogicalRepositoryRoot: logical, Environment: []string{"ROOT_DIR=" + firstRoot}, Yard: loaded.Context}
	original := &initExecution{loaded: loaded, mode: initReconcile, platform: runtime, powerYards: []domain.Context{loaded.Context}, integrationBaseline: &initIntegrationBaseline{Settings: map[string]config.SettingTrace{}, Sources: map[string]initIntegrationSource{firstRoot + "/config/file-\xff": {}}, Packages: map[string]string{}}}
	raw, err := captureOwnerInitRecord(original)
	if err != nil {
		t.Fatal(err)
	}
	var record ownerInitRecord
	if err := nativejson.DecodeLosslessNativeJSON(raw, &record, 32<<20); err != nil {
		t.Fatal(err)
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
	if restored.powerYards[0].Paths.RepositoryRoot != nextRoot {
		t.Fatal("original peer role retained dead descriptor spelling")
	}
	if _, ok := restored.integrationBaseline.Sources[firstRoot+"/config/file-\xff"]; ok {
		t.Fatal("old descriptor retained execution authority")
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
