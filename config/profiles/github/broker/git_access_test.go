package githubbroker

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGitAccessPermissionsAndTargetedInstallations(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, operation, installation, token, state string }{
		{"read", "read", `{"contents":"read"}`, `{"contents":"read"}`, "available"},
		{"write can read", "read", `{"contents":"write"}`, `{"contents":"write"}`, "available"},
		{"push requires workflows", "push", `{"contents":"write"}`, `{"contents":"write"}`, "insufficient-permissions"},
		{"push", "push", `{"contents":"write","workflows":"write"}`, `{"contents":"write","workflows":"write"}`, "available"},
		{"token reduced grants", "push", `{"contents":"write","workflows":"write"}`, `{"contents":"read"}`, "insufficient-permissions"},
		{"unknown installation", "read", `null`, `{"contents":"read"}`, "unknown"},
		{"unknown token", "read", `{"contents":"read"}`, `null`, "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := []string{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.URL.Path)
				switch r.URL.Path {
				case "/repos/owner/one/installation":
					fmt.Fprintf(w, `{"id":11,"permissions":%s}`, test.installation)
				case "/repos/other/two/installation":
					fmt.Fprintf(w, `{"id":22,"permissions":%s}`, test.installation)
				case "/app/installations/11/access_tokens", "/app/installations/22/access_tokens":
					fmt.Fprintf(w, `{"token":"fixture-token","expires_at":%q,"permissions":%s}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), test.token)
				default:
					t.Errorf("unexpected default installation retry: %s", r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer upstream.Close()
			issuer := newTestIssuer(Config{AppID: "42", InstallationID: 987}, key, upstream.Client(), upstream.URL, time.Now)
			for _, repo := range []string{"owner/one", "other/two"} {
				response := httptest.NewRecorder()
				NewHandler(issuer).ServeHTTP(response, httptest.NewRequest("POST", "/git-access/v1", strings.NewReader(fmt.Sprintf(`{"repository":%q,"operation":%q}`, repo, test.operation))))
				var access GitAccess
				if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &access) != nil || access.State != test.state || access.Version != 1 {
					t.Fatalf("access = %s", response.Body.String())
				}
				if (access.Token != nil) != (test.state == "available") {
					t.Fatal("token exposed without usable grants")
				}
			}
			if len(requests) < 2 {
				t.Fatal("targeted lookup was not performed")
			}
		})
	}
}

func TestGitAccessRejectsMalformedRequestsAndOldBroker(t *testing.T) {
	for _, body := range []string{`{}`, `{"repository":"owner/repo","operation":"delete"}`, `{"repository":"owner/repo","operation":"read","extra":1}`, `{"repository":"owner/repo","operation":"read"} {}`} {
		response := httptest.NewRecorder()
		NewHandler(nil).ServeHTTP(response, httptest.NewRequest("POST", "/git-access/v1", strings.NewReader(body)))
		if response.Code != 400 {
			t.Fatalf("accepted %s", body)
		}
	}
	socket := brokerServer(t, http.NotFoundHandler())
	if access := (Client{SocketPath: socket}).gitAccess(t.Context(), "owner/repo", "read"); access.State != "unavailable" {
		t.Fatal(access.State)
	}
}

func TestCanonicalGitRemoteValidation(t *testing.T) {
	for _, raw := range []string{"https://github.com/owner/repo.git", "git@github.com:owner/repo.git", "ssh://git@github.com/owner/repo"} {
		if repo, _, err := gitRemoteURL(raw); err != nil || repo != "owner/repo" {
			t.Fatal(raw, repo, err)
		}
	}
	for _, raw := range []string{"file:///tmp/repo", "ext::command", "ssh://git@alias/owner/repo", "https://user:secret@github.com/owner/repo", "https://github.com:443/owner/repo", "https://github.com/owner/repo?x=y", "ssh://git@github.com/owner/repo%0aevil", "git@github.com:owner/../repo", "https://github.com/owner/repo#fragment"} {
		if _, _, err := gitRemoteURL(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
