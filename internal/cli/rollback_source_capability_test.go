package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestRollbackSourceCapabilityProbeIsReadOnlyAndHasNoTransitionInputs(t *testing.T) {
	for _, extra := range []string{"", `,"target":"release-b"`} {
		t.Run(map[bool]string{true: "probe", false: "transition-input-refused"}[extra == ""], func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			manifestPath := filepath.Join(root, "config/commands.registry")
			manifest, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, manifestPath, string(manifest)+"_release-transition||@release-transition||local|mutate|dynamic|hidden|internal|none|_release-transition|inspect or converge one exact release transition||\n", 0600)
			var stdout, stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Environment: environment,
				Arguments: []string{"_release-transition"},
				Stdin:     strings.NewReader(`{"schemaVersion":2,"mode":"rollback-source-activation-capabilities"` + extra + `}`),
				Stdout:    &stdout, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			before := nativeTreeSnapshot(t, root)
			code := program.Run(context.Background())
			if extra == "" {
				response, err := releasetransition.ParseRecoveryProcessResponse(stdout.Bytes())
				if code != 0 || err != nil || response.Capabilities == nil || *response.Capabilities != releasetransition.RollbackSourceActivationCapabilities() {
					t.Fatalf("fixed CLI capability: code=%d stdout=%s stderr=%s err=%v", code, stdout.String(), stderr.String(), err)
				}
			} else if code != 2 || stdout.Len() != 0 {
				t.Fatalf("capability accepted transition authority: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if !reflect.DeepEqual(before, nativeTreeSnapshot(t, root)) {
				t.Fatal("capability probe changed native state")
			}
		})
	}
}
