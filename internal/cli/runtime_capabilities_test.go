package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestRuntimeCapabilityProbeReportsCompiledOwnersWithoutLifecycleEffects(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeRuntimeProfileFixture(t, root)
	platform := newInitPlatformFixture()
	var output bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: platform,
		Stdin: strings.NewReader(`{"schemaVersion":2,"mode":"runtime-activation-capabilities"}`), Stdout: &output, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.runReleaseTransition(context.Background(), nil); code != 0 {
		t.Fatalf("probe failed with code %d", code)
	}
	response, err := releasetransition.ParseRecoveryProcessResponse(output.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"core-guest-runtime", "key-tools-runtime", "sample-runtime"} {
		if !slices.Contains(response.ActivationOwners, id) {
			t.Fatalf("compiled owner %s absent from probe: %#v", id, response)
		}
	}
	if len(platform.applied) != 0 || platform.projectHooks != 0 || response.Outcome != nil || response.Inspection != nil || response.Capabilities != nil {
		t.Fatalf("probe admitted lifecycle work: %#v", response)
	}
}
