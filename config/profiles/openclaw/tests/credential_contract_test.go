package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/credentialruntime"
	"github.com/Subyard/Subyard/internal/testkit"
)

func shippedCredentialRuntime(t *testing.T) (*credentialruntime.Runtime, string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fixture := testkit.TempDir(t)
	consumerRoot := filepath.Join(fixture, "generated")
	runtime, err := credentialruntime.New(credentialruntime.Config{
		RepositoryRoot: root, Root: filepath.Join(fixture, "keys"), ConsumerRoot: consumerRoot,
		HostBase: filepath.Join(fixture, "yards"), ToolsDirectory: filepath.Join(fixture, "tools"), Context: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, fixture
}

func TestShippedCredentialConsumerPaths(t *testing.T) {
	runtime, fixture := shippedCredentialRuntime(t)
	consumerRoot := filepath.Join(fixture, "generated")
	for _, item := range []struct{ consumer, zone, path string }{
		{"staging-env", "canonical", "staging/canonical.env"},
		{"staging-env", "ephemeral", "staging/ephemeral.env"},
		{"qa-secrets", "global", "qa-pool/secrets.env"},
		{"qa-pool", "global", "qa-pool/pool.jsonl"},
		{"qa-pool", "qa", "qa-pool/pool.jsonl"},
	} {
		path, err := runtime.ConsumerPath(item.consumer, item.zone)
		if err != nil || path != filepath.Join(consumerRoot, item.path) {
			t.Fatalf("consumer=%s zone=%s path=%s err=%v", item.consumer, item.zone, path, err)
		}
	}
	if _, err := runtime.ConsumerPath("staging-env", "../outside"); err == nil {
		t.Fatal("unsafe staging zone accepted")
	}
}

func TestMutableStagingCredentialImportsAreExcluded(t *testing.T) {
	runtime, fixture := shippedCredentialRuntime(t)
	for _, item := range []struct {
		path    string
		blocked bool
	}{
		{"srv/staging/demo/creds/token.bin", true},
		{"creds/srv/staging/demo/token.bin", true},
		{"srv/staging/demo/static/token.bin", false},
		{"srv/another/demo/creds/token.bin", false},
		{"srv/staging-other/demo/creds/token.bin", false},
	} {
		path := filepath.Join(fixture, item.path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte("synthetic credential file"), 0o600)
		_, err := runtime.Prepare(context.Background(), "", []string{"import", path, "--dry-run"})
		if (err != nil) != item.blocked {
			t.Fatalf("path=%s blocked=%t err=%v", item.path, item.blocked, err)
		}
	}
}
