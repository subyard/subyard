package githubbroker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func brokerServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "github.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		_ = listener.Close()
	})
	go func() { _ = server.Serve(listener) }()
	return socket
}

func tokenResponse(t *testing.T, value string) []byte {
	t.Helper()
	payload, err := json.Marshal(Token{Value: value, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestRunClientRunScopesTokenAndPropagatesExit(t *testing.T) {
	socket := brokerServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/token" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(tokenResponse(t, "short-lived"))
	}))
	oldGH, oldGithub := os.Getenv("GH_TOKEN"), os.Getenv("GITHUB_TOKEN")
	defer os.Setenv("GH_TOKEN", oldGH)
	defer os.Setenv("GITHUB_TOKEN", oldGithub)
	_ = os.Setenv("GH_TOKEN", "parent-secret")
	_ = os.Setenv("GITHUB_TOKEN", "other-parent-secret")
	var stdout, stderr strings.Builder
	code := RunClient(context.Background(), []string{"--socket", socket, "run", "--", "sh", "-c", "test \"$GH_TOKEN\" = short-lived && test -z \"$GITHUB_TOKEN\"; exit 37"}, strings.NewReader(""), &stdout, &stderr)
	if code != 37 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "short-lived") {
		t.Fatalf("token leaked to client output: %q %q", stdout.String(), stderr.String())
	}
}

func TestRunClientCredentialProtocolAndNoMintForStoreErase(t *testing.T) {
	mints := 0
	socket := brokerServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/token" {
			mints++
			_, _ = writer.Write(tokenResponse(t, "credential-secret"))
			return
		}
		http.NotFound(writer, request)
	}))
	for _, operation := range []string{"store", "erase"} {
		var stdout, stderr strings.Builder
		code := RunClient(context.Background(), []string{"--socket", socket, "credential", operation}, strings.NewReader("protocol=https\nhost=github.com\nusername=ignored\npassword=ignored\n\n"), &stdout, &stderr)
		if code != 0 || stderr.Len() != 0 || stdout.Len() != 0 {
			t.Fatalf("%s: code=%d stdout=%q stderr=%q", operation, code, stdout.String(), stderr.String())
		}
	}
	if mints != 0 {
		t.Fatalf("store/erase minted %d tokens", mints)
	}
	var stdout, stderr strings.Builder
	code := RunClient(context.Background(), []string{"--socket", socket, "credential", "get"}, strings.NewReader("protocol=https\nhost=example.com\n\n"), &stdout, &stderr)
	if code == 0 || mints != 0 || strings.Contains(stdout.String()+stderr.String(), "credential-secret") {
		t.Fatalf("unsupported host: code=%d mints=%d stdout=%q stderr=%q", code, mints, stdout.String(), stderr.String())
	}
}

func TestRunClientCredentialGetMintsOnlyForGitHub(t *testing.T) {
	socket := brokerServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/token" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		_, _ = writer.Write(tokenResponse(t, "credential-secret"))
	}))
	var stdout, stderr strings.Builder
	code := RunClient(context.Background(), []string{"--socket", socket, "credential", "get"}, strings.NewReader("protocol=https\nhost=github.com\n\n"), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "username=x-access-token\npassword=credential-secret\n") {
		t.Fatalf("credential response = %q", stdout.String())
	}
}

func TestRunClientStatusDoesNotMintOrPrintSensitiveResponse(t *testing.T) {
	mints := 0
	socket := brokerServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/token" {
			mints++
		}
		if request.URL.Path == "/status" {
			_, _ = io.WriteString(writer, `{"configured":true,"unexpected_secret":"must-not-be-printed"}`)
			return
		}
		http.NotFound(writer, request)
	}))
	var stdout, stderr strings.Builder
	code := RunClient(context.Background(), []string{"--socket", socket, "status"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || mints != 0 || stdout.String() != `{"configured":true}`+"\n" || stderr.Len() != 0 {
		t.Fatalf("code=%d mints=%d stdout=%q stderr=%q", code, mints, stdout.String(), stderr.String())
	}
}

func TestRunClientExpiredTokenIsRejected(t *testing.T) {
	socket := brokerServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		payload, _ := json.Marshal(Token{Value: "expired-secret", ExpiresAt: time.Now().Add(-time.Minute)})
		_, _ = writer.Write(payload)
	}))
	var stdout, stderr strings.Builder
	code := RunClient(context.Background(), []string{"--socket", socket, "run", "--", "true"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 || strings.Contains(stdout.String()+stderr.String(), "expired-secret") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunClientGitUsesEphemeralHelperAndPreservesConfiguration(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "gitconfig")
	original := "[credential]\n\thelper = store --file=" + filepath.Join(root, "credentials") + "\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", configPath)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "test.inherited")
	t.Setenv("GIT_CONFIG_VALUE_0", "preserved")
	mints := 0
	socket := brokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mints++
		_, _ = w.Write(tokenResponse(t, "git-fixture"))
	}))
	var stdout, stderr strings.Builder
	code := RunClient(t.Context(), []string{"--socket", socket, "run", "--", "sh", "-eu", "-c", `
credentials=$(printf 'protocol=https\nhost=github.com\n\n' | git credential fill)
case "$credentials" in *"password=$GH_TOKEN"*) ;; *) exit 10 ;; esac
[ "$(git config --get test.inherited)" = preserved ]
printf '%s\n\n' "$credentials" | git credential approve
`}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stdout.Len() != 0 || stderr.Len() != 0 || mints != 1 {
		t.Fatalf("Git wrapper failed: code=%d mints=%d (output withheld)", code, mints)
	}
	if data, err := os.ReadFile(configPath); err != nil || string(data) != original {
		t.Fatal("Git configuration changed")
	}
	if _, err := os.Stat(filepath.Join(root, "credentials")); !os.IsNotExist(err) {
		t.Fatal("Git persisted credentials")
	}
}
