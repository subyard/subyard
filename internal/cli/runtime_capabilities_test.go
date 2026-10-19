package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestSourceTransitionCapabilitiesRequireVerifiedRustOwner(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeRuntimeProfileFixture(t, root)
	platform := newInitPlatformFixture()
	var output bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: platform, Stdout: &output, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	err = program.invokeRustReleaseTransitionFacade(context.Background(), []byte(`{"schemaVersion":2,"mode":"runtime-activation-capabilities"}`), &output)
	if err == nil || !strings.Contains(err.Error(), "verified published runtime") {
		t.Fatal("source fixture advertised an unverified owner", err)
	}
	if output.Len() != 0 || len(platform.applied) != 0 || platform.projectHooks != 0 {
		t.Fatal("unverified capability probe admitted results or lifecycle work", output.String())
	}
}
