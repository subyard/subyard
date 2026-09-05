package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestReleaseTransitionRejectsUnknownProtocolBeforeAuthorization(t *testing.T) {
	// A version mismatch must be rejected before touching the authorization
	// channel. Released launchers cannot recover by speaking a newer schema.
	if os.Getenv("SUBYARD_TEST_UNKNOWN_PROTOCOL_CHILD") != "1" {
		grant, err := os.CreateTemp(t.TempDir(), "grant")
		if err != nil {
			t.Fatal(err)
		}
		defer grant.Close()
		if _, err := grant.WriteString("\x00\n"); err != nil {
			t.Fatal(err)
		}
		if _, err := grant.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestReleaseTransitionRejectsUnknownProtocolBeforeAuthorization$")
		child.Env = append(os.Environ(), "SUBYARD_TEST_UNKNOWN_PROTOCOL_CHILD=1", "SUBYARD_RELEASE_TRANSITION_GRANT_FD=3")
		child.ExtraFiles = []*os.File{grant}
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("unknown protocol process guard: %v\n%s", err, output)
		}
		return
	}
	root, environment, _ := nativeFixture(t)
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Environment: environment,
		Stdin:  strings.NewReader(`{"schemaVersion":2,"mode":"inspect"}`),
		Stdout: &bytes.Buffer{}, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.runReleaseTransition(context.Background(), nil); code != 2 {
		t.Fatalf("unsupported protocol exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "request is invalid") ||
		strings.Contains(stderr.String(), "authorization") {
		t.Fatalf("unsupported protocol reached authorization handling: %s", stderr.String())
	}
}

func operationContextProtocolRequest(t *testing.T, home string) string {
	t.Helper()
	payload, err := json.Marshal(releasetransition.ProcessRequest{
		SchemaVersion: 1, Mode: releasetransition.ProcessInspect,
		RuntimeRoot: filepath.Join(home, "runtime"), ConfigHome: filepath.Join(home, "config"),
		Yard: "default", Target: "release-next", Direction: releasetransition.DirectionActivateTarget,
		ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}
