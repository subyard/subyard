package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestRootlessReleaseCapabilities(t *testing.T) {
	selected, err := releaseruntime.CompiledRustReleaseOwner()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"capabilities", "lifecycle-capabilities"} {
		payload := `{"schemaVersion":2,"mode":"` + mode + `"}`
		var output bytes.Buffer
		replay, handled, err := ReadRootlessReleaseCapabilities(strings.NewReader(payload), &output)
		if err != nil || handled != selected {
			t.Fatalf("%s: handled=%v err=%v", mode, handled, err)
		}
		if !handled {
			actual, _ := io.ReadAll(replay)
			if string(actual) != payload || output.Len() != 0 {
				t.Fatal("ordinary payload changed")
			}
			continue
		}
		response, err := releasetransition.ParseRecoveryProcessResponse(output.Bytes())
		want := releasetransition.ActivationOnlyRecoveryCapabilities()
		if mode == "lifecycle-capabilities" {
			want = releasetransition.ActivationRecoveryLifecycleCapabilities()
		}
		if err != nil || response.Capabilities == nil || *response.Capabilities != want {
			t.Fatalf("response=%+v err=%v", response, err)
		}
	}
	for _, payload := range []string{
		`{"schemaVersion":2,"mode":"runtime-activation-capabilities"}`,
		`{"schemaVersion":2,"mode":"migration-owner-capabilities"}`,
		`{"schemaVersion":2,"mode":"capabilities","extra":true}`,
		`{"schemaVersion":2,"mode":"capabilities"} {}`,
		`{"schemaVersion":1,"mode":"inspect"}`,
	} {
		var output bytes.Buffer
		replay, handled, err := ReadRootlessReleaseCapabilities(strings.NewReader(payload), &output)
		if err != nil || handled || output.Len() != 0 {
			t.Fatalf("payload handled: %s: %v", payload, err)
		}
		actual, err := io.ReadAll(replay)
		if err != nil || string(actual) != payload {
			t.Fatal("operational payload was not replayed unchanged", err)
		}
	}
	if selected {
		_, handled, err := ReadRootlessReleaseCapabilities(strings.NewReader(strings.Repeat(" ", releaseTransitionRequestLimit+1)), io.Discard)
		if err == nil || handled {
			t.Fatal("oversized request admitted")
		}
	}
}
