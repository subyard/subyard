package releaseowner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This host-free preflight checks the actual opt-in physical controller's
// generated recipes against native durable-intent validation before a VM lease.
func TestPhysicalControllerNativeIntentPreflight(t *testing.T) {
	controller := os.Getenv("SUBYARD_NATIVE_PHYSICAL_CONTROLLER")
	if controller == "" {
		t.Skip("physical controller artifact not selected")
	}
	for _, kind := range []string{"source", "fence"} {
		t.Run(kind, func(t *testing.T) {
			command := exec.Command("python3", "-B", "-c", `import importlib.util,sys
spec=importlib.util.spec_from_file_location('physical',sys.argv[1]); module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
root,_,_=module.prepare(sys.executable,'local-intent-'+sys.argv[2],{'kind':sys.argv[2]})
print(root)`, controller, kind)
			raw, err := command.CombinedOutput()
			if err != nil {
				t.Fatal(err, string(raw))
			}
			root := strings.TrimSpace(string(raw))
			if !strings.HasPrefix(root, "/tmp/subyard-native-physical-") {
				t.Fatal("controller escaped private fixture root")
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			body, err := os.ReadFile(filepath.Join(root, "fixture.json"))
			if err != nil {
				t.Fatal(err)
			}
			var fixture struct {
				Binding Binding     `json:"binding"`
				Step    IngressStep `json:"step"`
			}
			if err := strict(body, &fixture); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			s := session{root: file}
			request := Request{Op: "apply-ingress", Binding: &fixture.Binding, Step: &fixture.Step}
			recipe, err := recipeFingerprint(request)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.checkIntent(fixture.Binding, "source-ingress", kind, recipe); err != nil {
				t.Fatal("controller cannot enter actual native intent gate", err)
			}
			if kind == "fence" && s.checkIntent(fixture.Binding, "source-ingress", "source", recipe) == nil {
				t.Fatal("wrong original source resource was accepted for fence")
			}
		})
	}
}
