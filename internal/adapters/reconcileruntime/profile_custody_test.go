package reconcileruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
)

func TestNativeProfileCustodyUsesDeclaredClosedActions(t *testing.T) {
	runtime := profileRuntimeFixture(t, `#!/bin/sh
set -eu
case "$1" in
  observe)
    if [ -f "$STATE" ]; then printf '{"state":"current","actual":"%s","desired":"%s"}' "$NEW" "$NEW";
    else printf '{"state":"stale","actual":"%s","desired":"%s"}' "$OLD" "$NEW"; fi ;;
  custody-observe) printf '{"generation":"0"}' ;;
  custody-fence) printf '%s\n' "$*" > "$LOG"; printf '{"generation":"%s"}' "$4" ;;
  custody-apply) printf '%s\n' "$*" > "$LOG"; : > "$STATE"; printf '{"state":"current","actual":"%s","desired":"%s"}' "$NEW" "$NEW" ;;
  *) exit 2 ;;
esac
`)
	definitions, err := profile.Load(runtime.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	runtime.Profiles = definitions
	log := filepath.Join(runtime.RepositoryRoot, "calls")
	runtime.Environment = []string{"LOG=" + log, "STATE=" + filepath.Join(runtime.RepositoryRoot, "state"), "OLD=" + testRuntimeOldDigest, "NEW=" + testRuntimeNewDigest}
	if _, capable, err := runtime.CaptureProfileCustody(context.Background(), "synthetic-runtime"); err != nil || capable {
		t.Fatalf("undeclared custody admitted: %v %v", capable, err)
	}
	runtime.Profiles[0].Runtime.Custody = true
	if generation, capable, err := runtime.CaptureProfileCustody(context.Background(), "synthetic-runtime"); err != nil || !capable || generation != "0" {
		t.Fatalf("custody fact: %q %v %v", generation, capable, err)
	}
	generation, successor := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err := runtime.FenceProfileRuntime(context.Background(), "synthetic-runtime", "0", generation, successor); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(log)
	if err != nil || string(actual) != "custody-fence 0 "+generation+" "+successor+"\n" {
		t.Fatalf("fence action changed: %q %v", actual, err)
	}
	if err := runtime.ApplyProfileRuntimeCustody(context.Background(), "synthetic-runtime", "operation", testRuntimeOldDigest, testRuntimeNewDigest, successor, generation); err != nil {
		t.Fatal(err)
	}
	actual, err = os.ReadFile(log)
	want := "custody-apply operation " + testRuntimeOldDigest + " " + testRuntimeNewDigest + " " + successor + " " + generation + "\n"
	if err != nil || string(actual) != want {
		t.Fatalf("bound apply action changed: %q %v", actual, err)
	}
}
