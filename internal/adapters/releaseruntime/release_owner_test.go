package releaseruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestCompiledReleaseOwner(t *testing.T) {
	original := compiledReleaseOwner
	t.Cleanup(func() { compiledReleaseOwner = original })
	if original != "go" {
		t.Fatal("ordinary builds must default to Go")
	}
	for _, value := range []string{"go", "rust", "", "Rust", "invalid"} {
		compiledReleaseOwner = value
		selected, err := CompiledRustReleaseOwner()
		if selected != (value == "rust") || (err != nil) != (value != "go" && value != "rust") {
			t.Fatalf("%q: selected=%v err=%v", value, selected, err)
		}
	}
	compiledReleaseOwner = "invalid"
	runtime := New(Config{})
	defer runtime.Close()
	ctx := context.Background()
	checks := []func() error{
		func() error { _, err := runtime.inspectProtectedTransition(ctx, "", "", "", nil); return err },
		func() error { _, err := runtime.PrepareTransition(ctx, nil, "", ""); return err },
		func() error { _, err := runtime.prepareRetainedTransition(ctx, options{}, "", "", nil); return err },
		func() error {
			_, err := runtime.prepareVerifiedTransition(ctx, options{}, nil, nil, releasetransition.ProcessRequest{}, nil)
			return err
		},
		func() error { _, err := runtime.inspectActivationRecovery(ctx, nil, nil); return err },
		func() error {
			_, err := runtime.invokeVerifiedRecoveryTransition(ctx, nil, "", releasetransition.RecoveryProcessRequest{}, "")
			return err
		},
		func() error {
			_, err := runtime.invokeVerifiedRuntimeTransition(ctx, nil, nil, releasetransition.ProcessRequest{}, "")
			return err
		},
		func() error { _, err := runtime.InspectSourceAdmission(ctx, "", ""); return err },
	}
	for index, check := range checks {
		var selected selectedRustOwnerError
		if err := check(); !errors.As(err, &selected) {
			t.Fatalf("entry %d did not refuse invalid compiled authority before inspection: %v", index, err)
		}
	}
}
