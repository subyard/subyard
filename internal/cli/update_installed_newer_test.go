package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type latestReleaseTransport struct{ tag string }

func (transport latestReleaseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	switch request.URL.String() {
	case "https://github.com/Subyard/Subyard/releases/latest":
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{
			"Location": {"https://github.com/Subyard/Subyard/releases/tag/" + transport.tag},
		}, Body: http.NoBody, Request: request}, nil
	case "https://github.com/Subyard/Subyard/releases/tag/" + transport.tag:
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: http.NoBody, Request: request}, nil
	}
	return nil, errors.New("unexpected request " + request.URL.String())
}

func TestPlainUpdateWithOlderLatestReleaseIsANoOpAtTheCLIBoundary(t *testing.T) {
	cliFixture, root, environment, runtimeRoot := newUpdateReleaseFixture(t)
	if err := os.MkdirAll(filepath.Join(runtimeRoot, "releases", "9.9.9-aaaaaaaaaaaa"), 0o700); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(runtimeRoot, "current")
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/9.9.9-aaaaaaaaaaaa", current); err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	http.DefaultTransport = latestReleaseTransport{tag: "v1.2.3"}
	t.Cleanup(func() { http.DefaultTransport = original })
	var stdout, stderr bytes.Buffer
	program := cliFixture.New(Options{
		RepositoryRoot: root, Program: "yard", WorkingDir: root, Environment: environment,
		Arguments: []string{"update", "--yes", "--runtime-root", runtimeRoot},
		Stdout:    &stdout, Stderr: &stderr,
	})
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, expected := range []string{
		"[ ok ] No update installed",
		"installed version 9.9.9 is newer than the latest published release 1.2.3; no release was selected",
		"Active release: 9.9.9-aaaaaaaaaaaa",
		"yard update --version 1.2.3 --runtime-root " + runtimeRoot + " --check",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("missing %q in stdout=%q stderr=%q", expected, stdout.String(), stderr.String())
		}
	}
	if strings.Contains(stdout.String(), "Update completed successfully") {
		t.Fatalf("no-op reported an update: %q", stdout.String())
	}
	if active, err := os.Readlink(current); err != nil || active != "releases/9.9.9-aaaaaaaaaaaa" {
		t.Fatalf("no-op changed the active release: %q, %v", active, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "cache")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-op created the release cache: %v", err)
	}
}
