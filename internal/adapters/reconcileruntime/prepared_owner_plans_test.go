package reconcileruntime

import (
	"bytes"
	"encoding/gob"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/testkit"
)

func ownerPlansFixture(t *testing.T) (Runtime, IntegrationPlan) {
	t.Helper()
	root := testkit.TempDir(t)
	runtime := profileRuntimeFixtureAt(t, root, "#!/bin/sh\nprintf unexpected-native-call > \"$MARKER\"\nexit 99\n")
	runtime.Environment = []string{"MARKER=" + filepath.Join(root, "native-called"), "EXTERNAL=bytes\xff"}
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := runtime.profileRuntimeInputs(definitions)
	if err != nil {
		t.Fatal(err)
	}
	old := ports.RuntimeObservation{State: ports.RuntimeStateStale, Actual: testRuntimeOldDigest, Desired: testRuntimeNewDigest, HookBinding: testRuntimeOldDigest}
	current := ports.RuntimeObservation{State: ports.RuntimeStateCurrent, Actual: testRuntimeNewDigest, Desired: testRuntimeNewDigest, OwnershipAbsent: true}
	runtime.RuntimePlan = &ProfileRuntimePlan{definitions: definitions, approved: map[string]ports.RuntimeObservation{"synthetic-runtime": old}, current: map[string]ports.RuntimeObservation{"synthetic-runtime": current}, resolved: map[string]ports.RuntimeObservation{"synthetic-runtime": old}, conditional: map[string]bool{"synthetic-runtime": true}, inputs: inputs, unavailable: true}
	value := "external-\xff\xfe"
	runtime.HookPlan = &ProjectHookPlan{approved: hookObservation{Projects: "projects", Wiring: value, Hooks: []string{value}, Roots: []string{}, Facts: map[string]string{value: value}, Resolved: map[string]string{}}, current: hookObservation{Facts: map[string]string{"current": value}}, provisionedHooks: hookObservation{Resolved: map[string]string{value: "resolved"}}, conditional: true, unavailable: true, completed: true, stopped: true, allowedHooks: []string{value}, source: "source", ownerProjects: []domain.ProjectRecord{{HostPath: value, ProjectID: "project"}}, resolvedProjects: "resolved-projects", executionWiring: value, hookBindings: map[string]string{value: value}, integrationScope: "scope", metadata: &projectMetadataPlan{Targets: []projectMetadataTarget{{value, value}}, projectMetadataObservation: projectMetadataObservation{Expected: []byte{0xff, 0, 0xfe}, Projects: value}, ProjectedBindings: map[string]string{value: value}}, metadataCompleted: true}
	runtime.projectMetadataProjection = []byte{0xff, 0, 0xfe}
	runtime.AdoptLegacyIntegrations, runtime.LegacyIntegrationFingerprint = true, value
	adoption := IntegrationPlan{Fingerprint: "fingerprint", Changed: true, Steps: []string{value}, Adoption: []string{}, AdoptionFingerprint: value, Scope: value, facts: []integrationFact{{Key: value, Desired: value, Observed: value}}, sources: "sources", packageSources: map[string]string{value: value}, instance: ports.InstanceInfo{Name: value, Config: map[string]string{value: value}, Devices: map[string]map[string]string{value: {value: value}, "empty": {}, "nil": nil}, LocalConfig: map[string]string{}, LocalDevices: map[string]map[string]string{}}, instanceScope: value, fingerprints: []string{}, profileRoutes: []integrationProfileRoute{{Owner: value, MarkerKey: value, DeviceName: value, Marker: value, Device: map[string]string{value: value}}}}
	return runtime, adoption
}

func TestOwnerNativePlansRoundTripOriginalMapsFlagsMetadataAndIntegration(t *testing.T) {
	runtime, adoption := ownerPlansFixture(t)
	raw, err := runtime.CaptureOwnerNativePlans(adoption)
	if err != nil {
		t.Fatal(err)
	}
	again, err := runtime.CaptureOwnerNativePlans(adoption)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("capture is not deterministic: %v", err)
	}
	restored, gotAdoption, err := runtime.RestoreOwnerNativePlans(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runtime.RuntimePlan, restored.RuntimePlan) || !reflect.DeepEqual(runtime.HookPlan, restored.HookPlan) || !reflect.DeepEqual(adoption, gotAdoption) || !bytes.Equal(runtime.projectMetadataProjection, restored.projectMetadataProjection) || restored.AdoptLegacyIntegrations != runtime.AdoptLegacyIntegrations || restored.LegacyIntegrationFingerprint != runtime.LegacyIntegrationFingerprint {
		t.Fatal("original owner plan changed during restore")
	}
	restored.RuntimePlan.approved["synthetic-runtime"] = ports.RuntimeObservation{}
	restored.RuntimePlan.conditional["synthetic-runtime"] = false
	restored.HookPlan.allowedHooks[0] = "mutated"
	restored.HookPlan.metadata.Expected[0] = 0
	gotAdoption.instance.Devices["external-\xff\xfe"]["external-\xff\xfe"] = "mutated"
	restored.projectMetadataProjection[0] = 0
	if runtime.RuntimePlan.approved["synthetic-runtime"].State != ports.RuntimeStateStale || !runtime.RuntimePlan.conditional["synthetic-runtime"] || runtime.HookPlan.allowedHooks[0] != "external-\xff\xfe" || runtime.HookPlan.metadata.Expected[0] != 0xff || adoption.instance.Devices["external-\xff\xfe"]["external-\xff\xfe"] != "external-\xff\xfe" || runtime.projectMetadataProjection[0] != 0xff {
		t.Fatal("restored owner plan aliases original mutable data")
	}
}

func TestOwnerNativePlansPreserveNonUTF8Strings(t *testing.T) {
	runtime, adoption := ownerPlansFixture(t)
	raw, err := runtime.CaptureOwnerNativePlans(adoption)
	if err != nil {
		t.Fatal(err)
	}
	restored, got, err := runtime.RestoreOwnerNativePlans(raw)
	if err != nil {
		t.Fatal(err)
	}
	value := "external-\xff\xfe"
	if restored.HookPlan.approved.Facts[value] != value || restored.HookPlan.ownerProjects[0].HostPath != value || restored.HookPlan.metadata.Targets[0].Payload != value || got.instance.Devices[value][value] != value || got.profileRoutes[0].Device[value] != value || restored.LegacyIntegrationFingerprint != value {
		t.Fatal("non-UTF8 original bytes changed")
	}
}

func TestOwnerNativePlansRefuseMalformedDescriptors(t *testing.T) {
	runtime, adoption := ownerPlansFixture(t)
	raw, err := runtime.CaptureOwnerNativePlans(adoption)
	if err != nil {
		t.Fatal(err)
	}
	var d ownerNativePlans
	if err := nativejson.DecodeLosslessNativeJSON(raw, &d, MaxInstalledRecipe); err != nil {
		t.Fatal(err)
	}
	d.Runtime.Approved.Entries = append(d.Runtime.Approved.Entries, d.Runtime.Approved.Entries[0])
	duplicate, err := encodeOwnerPlans(d)
	if err != nil {
		t.Fatal(err)
	}
	var wrong bytes.Buffer
	if err := gob.NewEncoder(&wrong).Encode(struct{ Version uint32 }{1}); err != nil {
		t.Fatal(err)
	}
	for name, invalid := range map[string][]byte{"empty": nil, "truncated": raw[:len(raw)-1], "trailing byte": append(bytes.Clone(raw), 0), "trailing descriptor": append(bytes.Clone(raw), raw...), "wrong type": wrong.Bytes(), "duplicate map row": duplicate, "duplicate field": bytes.Replace(raw, []byte(`"Version":1`), []byte(`"Version":1,"Version":1`), 1), "unknown field": bytes.Replace(raw, []byte(`"Version":1`), []byte(`"Unknown":null,"Version":1`), 1), "invalid wire string": bytes.Replace(raw, []byte(`"SourceInputs":"`), []byte(`"SourceInputs":"!`), 1)} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := runtime.RestoreOwnerNativePlans(invalid); err == nil {
				t.Fatal("malformed owner descriptor accepted")
			}
		})
	}
	d.Runtime.Approved.Entries = d.Runtime.Approved.Entries[:1]
	d.Runtime.Approved.Entries[0].Value.State = "invalid"
	invalid, err := encodeOwnerPlans(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtime.RestoreOwnerNativePlans(invalid); err == nil {
		t.Fatal("invalid observation domain accepted")
	}

}

func TestOwnerNativePlansRefuseSourceDefinitionAndInputDriftWithoutNativeCalls(t *testing.T) {
	for _, drift := range []string{"source", "definition", "environment", "nonUTF8 environment"} {
		t.Run(drift, func(t *testing.T) {
			runtime, adoption := ownerPlansFixture(t)
			raw, err := runtime.CaptureOwnerNativePlans(adoption)
			if err != nil {
				t.Fatal(err)
			}
			switch drift {
			case "source":
				testkit.WriteFile(t, filepath.Join(runtime.RepositoryRoot, "config", "profiles", "synthetic", "runtime.sh"), []byte("#!/bin/sh\nexit 1\n"), 0o700)
			case "definition":
				runtime.Profiles, err = runtime.profileDefinitions()
				if err != nil {
					t.Fatal(err)
				}
				runtime.Profiles[0].Runtime.ActivationID = "different-runtime"
			case "environment":
				runtime.Environment = append(runtime.Environment, "CHANGED=1")
			case "nonUTF8 environment":
				runtime.Environment[1] = "EXTERNAL=bytes\xfe"
			}
			if _, _, err := runtime.RestoreOwnerNativePlans(raw); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("input drift accepted: %v", err)
			}
			if _, err := os.Stat(filepath.Join(runtime.RepositoryRoot, "native-called")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("native effect during restore: %v", err)
			}
		})
	}
}

func TestOwnerNativePlansNeverPrepareOrObserveAgain(t *testing.T) {
	runtime, adoption := ownerPlansFixture(t)
	raw, err := runtime.CaptureOwnerNativePlans(adoption)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtime.RestoreOwnerNativePlans(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(runtime.RepositoryRoot, "native-called")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native handler was called: %v", err)
	}
}

func TestOwnerNativePlansCodecCrossProcessOrdering(t *testing.T) {
	if root := os.Getenv("SUBYARD_TEST_OWNER_PLAN_ROOT"); root != "" {
		var unrelated bytes.Buffer
		if err := gob.NewEncoder(&unrelated).Encode(struct{ Unrelated string }{"first"}); err != nil {
			t.Fatal(err)
		}
		runtime := Runtime{RepositoryRoot: root, Environment: []string{"MARKER=" + filepath.Join(root, "native-called"), "EXTERNAL=bytes\xff"}}
		raw, err := os.ReadFile(filepath.Join(root, "owner-plan"))
		if err != nil {
			t.Fatal(err)
		}
		restored, adoption, err := runtime.RestoreOwnerNativePlans(raw)
		if err != nil {
			t.Fatal(err)
		}
		again, err := restored.CaptureOwnerNativePlans(adoption)
		if err != nil || !bytes.Equal(raw, again) {
			t.Fatalf("fresh-process capture changed wire bytes: %v", err)
		}
		return
	}
	runtime, adoption := ownerPlansFixture(t)
	raw, err := runtime.CaptureOwnerNativePlans(adoption)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(runtime.RepositoryRoot, "owner-plan"), raw, 0o600)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestOwnerNativePlansCodecCrossProcessOrdering$", "-test.count=1")
	command.Env = append(os.Environ(), "SUBYARD_TEST_OWNER_PLAN_ROOT="+runtime.RepositoryRoot)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fresh process restore: %v\n%s", err, output)
	}
}

func TestOwnerNativePlansPreserveNilAndEmptyWithoutSourceReads(t *testing.T) {
	for _, empty := range []bool{false, true} {
		runtime := Runtime{RepositoryRoot: filepath.Join(testkit.TempDir(t), "missing")}
		adoption := IntegrationPlan{}
		if empty {
			runtime.RuntimePlan = &ProfileRuntimePlan{definitions: []profile.Definition{}, approved: map[string]ports.RuntimeObservation{}, current: map[string]ports.RuntimeObservation{}, conditional: map[string]bool{}, resolved: map[string]ports.RuntimeObservation{}}
			runtime.HookPlan = &ProjectHookPlan{allowedHooks: []string{}, hookBindings: map[string]string{}, ownerProjects: []domain.ProjectRecord{}}
			runtime.projectMetadataProjection = []byte{}
			adoption = IntegrationPlan{Steps: []string{}, Adoption: []string{}, facts: []integrationFact{}, packageSources: map[string]string{}, fingerprints: []string{}, profileRoutes: []integrationProfileRoute{}}
		}
		raw, err := runtime.CaptureOwnerNativePlans(adoption)
		if err != nil {
			t.Fatal(err)
		}
		restored, got, err := runtime.RestoreOwnerNativePlans(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(runtime.RuntimePlan, restored.RuntimePlan) || !reflect.DeepEqual(runtime.HookPlan, restored.HookPlan) || !reflect.DeepEqual(runtime.projectMetadataProjection, restored.projectMetadataProjection) || !reflect.DeepEqual(adoption, got) {
			t.Fatal("nil or empty plan changed")
		}
	}
}
