package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestAfterOwnerRuntimePreservesCachedHandlerAndPhysicalGeneration(t *testing.T) {
	runtime := preparedProfileFixture(t)
	root := runtime.Profiles[0].Root
	runtime.Profiles[0].Runtime.Custody = true
	state := filepath.Join(runtime.Yard.Paths.DataHome, "observed")
	generation := filepath.Join(runtime.Yard.Paths.DataHome, "generation")
	testkit.WriteFile(t, state, []byte(testRuntimeOldDigest), 0600)
	testkit.WriteFile(t, generation, []byte("0"), 0600)
	script := `#!/bin/sh
set -eu
case "$1" in
 custody-observe) printf '{"generation":"%s"}\n' "$(cat "$GENERATION")";;
 observe) printf '{"state":"stale","actual":"%s","desired":"%s"}\n' "$(cat "$STATE")" "$NEW";;
 *) exit 92;;
esac
`
	testkit.WriteFile(t, filepath.Join(root, "runtime.sh"), []byte(script), 0755)
	runtime.Environment = append(runtime.Environment, "PATH=/usr/bin:/bin", "STATE="+state, "GENERATION="+generation, "NEW="+testRuntimeNewDigest)
	source, err := runtime.PrepareProfileRuntimeSources(context.Background(), "synthetic-runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	original, err := source.ReserveAfterOwnerMetadata([]byte(`{"version":1,"phase":"template","metadataBytes":2048,"descriptorBytes":2048}`))
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	for index, bind := range []func(context.Context, []byte) (*PreparedInstalledRuntime, error){runtime.BindInstalledRuntimeContext, runtime.BindAfterOwnerRuntimeSources} {
		raw := [][]byte{source.Recipe(), original.Recipe()}[index]
		for _, invalid := range [][]byte{
			append(bytes.Clone(raw), ' '),
			bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":2`), 1),
			append([]byte(`{"unexpected":0,`), raw[1:]...),
		} {
			if bound, err := bind(context.Background(), invalid); err == nil {
				bound.Close()
				t.Fatalf("invalid recipe admitted by binder=%d", index)
			}
		}
	}
	prepared, err := runtime.BindAfterOwnerRuntimeSources(context.Background(), original.Recipe())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	observation, gen, capable, err := runtime.ObservePreparedAfterOwnerRuntime(context.Background(), prepared)
	if err != nil || observation.State != ports.RuntimeStateStale || observation.Actual != testRuntimeOldDigest || gen != "0" || !capable {
		t.Fatal("cached physical observation", observation, gen, capable, err)
	}
	if _, err := os.Stat(filepath.Join(prepared.directory, "prepared/after-owner.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inert metadata entered executable tree", err)
	}
	// Metadata replacement changes no source byte or native field; restore still
	// uses the original source proof and its registered handler.
	derived, err := original.WithAfterOwnerMetadata([]byte(`{"version":1,"phase":"resolved"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer derived.Close()
	if derived.Binding() != fmt.Sprintf("%x", sha256.Sum256(derived.Recipe())) {
		t.Fatal("metadata replacement binding differs from exact canonical bytes")
	}
	rebound, err := runtime.BindAfterOwnerRuntimeSources(context.Background(), derived.Recipe())
	if err != nil {
		t.Fatal(err)
	}
	defer rebound.Close()
	testkit.WriteFile(t, filepath.Join(root, "runtime.sh"), []byte("#!/bin/sh\nexit 99\n"), 0755)
	if _, err := runtime.BindAfterOwnerRuntimeSources(context.Background(), derived.Recipe()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("source B selected after resolution", err)
	}
}
