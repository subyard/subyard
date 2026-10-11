package githubbroker

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRepositoryInstallationRoutingAndDefault(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var requests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("missing App authentication")
		}
		switch r.URL.Path {
		case "/repos/person/one/installation":
			_, _ = io.WriteString(w, `{"id":7}`)
		case "/repos/organization/two/installation":
			_, _ = io.WriteString(w, `{"id":8}`)
		default:
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{}` {
				t.Error("installation scope was overridden")
			}
			_ = json.NewEncoder(w).Encode(Token{Value: "fixture", ExpiresAt: time.Now().Add(time.Hour)})
		}
	}))
	defer upstream.Close()
	issuer := newTestIssuer(Config{AppID: "9", InstallationID: 7}, key, upstream.Client(), upstream.URL, time.Now)
	socket := brokerServer(t, NewHandler(issuer))
	for _, repository := range []string{"", "person/one", "organization/two"} {
		if _, err := (Client{SocketPath: socket}).token(t.Context(), repository); err != nil {
			t.Fatal(err)
		}
	}
	want := "POST /app/installations/7/access_tokens\nGET /repos/person/one/installation\nPOST /app/installations/7/access_tokens\nGET /repos/organization/two/installation\nPOST /app/installations/8/access_tokens"
	if strings.Join(requests, "\n") != want {
		t.Fatalf("requests = %v", requests)
	}
}

func TestRepositoryFailuresAreSafeAndDoNotFallBack(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name, lookupBody         string
		lookupStatus, mintStatus int
		want                     error
	}{
		{"not installed", "upstream-secret", 404, 0, errRepositoryUnavailable},
		{"unauthorized", "upstream-secret", 401, 0, errInstallationLookup},
		{"redirect", "upstream-secret", 301, 0, errInstallationLookup},
		{"invalid id", `{"id":0}`, 200, 0, errInstallationLookup},
		{"oversized", strings.Repeat("s", maxResponse+1), 200, 0, errInstallationLookup},
		{"mint denied", `{"id":8}`, 200, 403, errTokenRejected},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			mints := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(scenario.lookupStatus)
					_, _ = io.WriteString(w, scenario.lookupBody)
					return
				}
				mints++
				if r.URL.Path != "/app/installations/8/access_tokens" {
					t.Error("mint used default instead of selected installation")
				}
				w.WriteHeader(scenario.mintStatus)
				_, _ = io.WriteString(w, "upstream-secret")
			}))
			defer upstream.Close()
			issuer := newTestIssuer(Config{AppID: "9", InstallationID: 7}, key, upstream.Client(), upstream.URL, time.Now)
			socket := brokerServer(t, NewHandler(issuer))
			_, err := (Client{SocketPath: socket}).token(t.Context(), "organization/repo")
			if err == nil || err.Error() != scenario.want.Error() || strings.Contains(err.Error(), "upstream-secret") {
				t.Fatalf("unexpected diagnostic: %v", err)
			}
			if (mints != 0) != (scenario.mintStatus != 0) {
				t.Fatalf("mints = %d", mints)
			}
		})
	}
}

func TestRepositoryRequestValidation(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"repository":"org/../repo"}`, `{"repository":"org/repo","permissions":{}}`, `{"repository":"org/repo"}{}`, strings.Repeat("x", requestLimit+1)} {
		response := httptest.NewRecorder()
		NewHandler(nil).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(body)))
		if response.Code != 400 && response.Code != 413 {
			t.Fatalf("body validation status = %d", response.Code)
		}
	}
}

func TestCommandRepositorySelection(t *testing.T) {
	t.Setenv("GH_HOST", "")
	t.Setenv("GH_REPO", "context/repo")
	for _, command := range [][]string{
		{"gh", "run", "list", "-R", "organization/repo"},
		{"gh", "run", "list", "--repo=organization/repo"},
		{"gh", "repo", "view", "https://github.com/organization/repo"},
		{"gh", "repo", "view", "--json", "nameWithOwner", "organization/repo"},
		{"gh", "repo", "view", "--web", "--branch=main", "organization/repo"},
		{"gh", "repo", "view", "-bmain", "organization/repo"},
		{"gh", "repo", "view", "--", "organization/repo"},
		{"gh", "repo", "clone", "--no-upstream", "organization/repo"},
		{"gh", "repo", "clone", "--upstream-remote-name", "parent", "organization/repo", "--", "--depth=1"},
		{"gh", "api", "/repos/organization/repo/actions/runs"},
		{"gh", "api", "https://api.github.com/repos/organization/repo/actions/runs"},
	} {
		got, err := commandRepository(t.Context(), command, "")
		if err != nil || got != "organization/repo" {
			t.Fatalf("selection for %v = %q, %v", command, got, err)
		}
	}
	got, err := commandRepository(t.Context(), []string{"gh", "run", "list"}, "")
	if err != nil || got != "context/repo" {
		t.Fatalf("GH_REPO = %q, %v", got, err)
	}
	got, err = commandRepository(t.Context(), []string{"gh", "api", "repos/{owner}/{repo}/actions/runs"}, "organization/repo")
	if err != nil || got != "organization/repo" {
		t.Fatalf("native API placeholders = %q, %v", got, err)
	}
	if _, err := commandRepository(t.Context(), []string{"gh", "api", "repos/organization/repo/issues"}, "other/repo"); err == nil {
		t.Fatal("conflicting explicit target was accepted")
	}
	if _, err := commandRepository(t.Context(), []string{"gh", "repo", "view", "--json", "nameWithOwner", "organization/repo"}, "other/repo"); err == nil {
		t.Fatal("repository after value flag bypassed explicit target mismatch")
	}
	if _, err := commandRepository(t.Context(), []string{"gh", "repo", "clone", "--no-upstream", "organization/repo"}, "other/repo"); err == nil {
		t.Fatal("repository after boolean flag bypassed explicit target mismatch")
	}
	if _, err := commandRepository(t.Context(), []string{"gh", "repo", "view", "--unknown", "organization/repo"}, ""); err == nil {
		t.Fatal("ambiguous flag silently selected context/default installation")
	}
	got, err = commandRepository(t.Context(), []string{"gh", "repo", "view", "--unknown", "organization/repo"}, "organization/repo")
	if err != nil || got != "organization/repo" {
		t.Fatalf("explicit fallback for unknown flags = %q, %v", got, err)
	}
	if _, err := commandRepository(t.Context(), []string{"gh", "api", "repos/organization/repo/issues", "--hostname", "elsewhere.example"}, ""); err == nil {
		t.Fatal("non-GitHub host accepted")
	}
	got, err = commandRepository(t.Context(), []string{"git", "clone", "https://github.com/organization/repo.git"}, "")
	if err != nil || got != "" {
		t.Fatalf("Git eagerly selected a context installation: %q, %v", got, err)
	}
}

func TestWrapperRepositorySetsChildContextAndRejectsOldBroker(t *testing.T) {
	requests := 0
	socket := brokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var selection struct {
			Repository string `json:"repository"`
		}
		if json.NewDecoder(r.Body).Decode(&selection) != nil || selection.Repository != "organization/repo" {
			t.Error("wrapper did not select requested repository")
		}
		_, _ = w.Write(tokenResponse(t, "context-fixture"))
	}))
	t.Setenv("GH_REPO", "unrelated/repo")
	var stdout, stderr strings.Builder
	code := RunClient(t.Context(), []string{"--socket", socket, "run", "--repo", "organization/repo", "--", "sh", "-c", `test "$GH_REPO" = organization/repo && test "$GH_TOKEN" = context-fixture`}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || requests != 1 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("wrapper context exit=%d requests=%d (output withheld)", code, requests)
	}
	requests = 0
	oldSocket := brokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, _ := io.ReadAll(r.Body)
		if len(body) == 0 {
			t.Error("new client fell back to old default installation")
		}
		http.Error(w, "request body must be empty", http.StatusBadRequest)
	}))
	code = RunClient(t.Context(), []string{"--socket", oldSocket, "run", "--repo", "organization/repo", "--", "true"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || requests != 1 || stdout.Len() != 0 {
		t.Fatalf("old broker failure exit=%d requests=%d", code, requests)
	}
}

func TestGhContextUsesSSHOriginWithoutChangingIt(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "github-context-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, args := range [][]string{{"init", "-q", root}, {"-C", root, "remote", "add", "origin", "git@github.com:organization/repo.git"}} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	t.Setenv("GH_REPO", "")
	t.Setenv("GH_HOST", "")
	got, err := commandRepository(t.Context(), []string{"gh", "run", "list"}, "")
	if err != nil || got != "organization/repo" {
		t.Fatalf("origin context = %q, %v", got, err)
	}
	output, err := exec.Command("git", "config", "--get", "remote.origin.url").Output()
	if err != nil || strings.TrimSpace(string(output)) != "git@github.com:organization/repo.git" {
		t.Fatal("SSH origin changed")
	}
	if err := exec.Command("git", "config", "remote.origin.url", strings.Repeat("x", maxProtocolInput+1)).Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := commandRepository(t.Context(), []string{"gh", "run", "list"}, ""); err == nil {
		t.Fatal("oversized context silently selected the default installation")
	}
}

func TestGitCredentialPathRoutesWithoutInitialDefaultMint(t *testing.T) {
	var selections []string
	socket := brokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var selection struct {
			Repository string `json:"repository"`
		}
		if json.NewDecoder(r.Body).Decode(&selection) != nil || !validRepository(selection.Repository) {
			t.Error("Git requested default rather than URL repository")
			w.WriteHeader(400)
			return
		}
		selections = append(selections, selection.Repository)
		_, _ = w.Write(tokenResponse(t, "credential-fixture"))
	}))
	root := testkit.TempDir(t)
	gitPath := filepath.Join(root, "git")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// A direct Git process requests the real Git credential protocol for two URLs.
	testkit.WriteFile(t, gitPath, []byte(fmt.Sprintf("#!/bin/sh\nprintf 'protocol=https\\nhost=github.com\\npath=person/one.git\\n\\n' | %q credential fill >/dev/null\nprintf 'protocol=https\\nhost=github.com\\npath=organization/two.git\\n\\n' | %q credential fill >/dev/null\n", realGit, realGit)), 0o700)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "absent"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	var stdout, stderr strings.Builder
	code := RunClient(t.Context(), []string{"--socket", socket, "run", "--", gitPath, "clone", "https://github.com/organization/two.git"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stdout.Len() != 0 || stderr.Len() != 0 || strings.Join(selections, ",") != "person/one,organization/two" {
		t.Fatalf("Git routing exit=%d selections=%v (output withheld)", code, selections)
	}
}

func TestRelativeSocketSurvivesGitDirectoryChange(t *testing.T) {
	requests := 0
	socket := brokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var selection struct {
			Repository string `json:"repository"`
		}
		if json.NewDecoder(r.Body).Decode(&selection) != nil || selection.Repository != "organization/repo" {
			t.Error("Git helper did not select its URL repository")
		}
		_, _ = w.Write(tokenResponse(t, "directory-change-fixture"))
	}))
	root := filepath.Dir(socket)
	if err := os.Mkdir(filepath.Join(root, "changed"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "absent"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	var stdout, stderr strings.Builder
	code := RunClient(t.Context(), []string{"--socket", filepath.Base(socket), "run", "--", "git", "-C", "changed", "credential", "fill"}, strings.NewReader("protocol=https\nhost=github.com\npath=organization/repo.git\n\n"), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || requests != 1 || !strings.Contains(stdout.String(), "password=directory-change-fixture") {
		t.Fatalf("relative socket Git helper exit=%d requests=%d (output withheld)", code, requests)
	}
}
