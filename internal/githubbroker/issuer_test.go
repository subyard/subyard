package githubbroker

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIssueSignsJWTAndSendsNoScopeOverrides(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/app/installations/987/access_tokens" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != `{}` || request.Header.Get("Authorization") == "" {
			t.Fatalf("unexpected request body/auth: %q", body)
		}
		parts := strings.Split(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "), ".")
		if len(parts) != 3 {
			t.Fatalf("JWT parts = %d", len(parts))
		}
		decode := func(value string, target any) {
			raw, decodeErr := base64.RawURLEncoding.DecodeString(value)
			if decodeErr != nil || json.Unmarshal(raw, target) != nil {
				t.Fatalf("invalid JWT part")
			}
		}
		var header struct {
			Algorithm string `json:"alg"`
		}
		var claims struct {
			Issued int64  `json:"iat"`
			Expiry int64  `json:"exp"`
			Issuer string `json:"iss"`
		}
		decode(parts[0], &header)
		decode(parts[1], &claims)
		signature, decodeErr := base64.RawURLEncoding.DecodeString(parts[2])
		if decodeErr != nil || rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sha256Digest(parts[0]+"."+parts[1]), signature) != nil {
			t.Fatalf("JWT signature did not verify")
		}
		if header.Algorithm != "RS256" || claims.Issuer != "42" || claims.Issued != now.Add(-time.Minute).Unix() || claims.Expiry != now.Add(9*time.Minute).Unix() {
			t.Fatalf("claims = %#v header = %#v", claims, header)
		}
		_, _ = writer.Write([]byte(`{"token":"ghs_test-token","expires_at":"2026-09-18T12:30:00Z"}`))
	}))
	defer server.Close()
	issuer := newTestIssuer(Config{AppID: "42", InstallationID: 987}, key, server.Client(), server.URL, func() time.Time { return now })
	token, err := issuer.Issue(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if token.Value != "ghs_test-token" || !token.ExpiresAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("token = %#v", token)
	}
}

func TestLoadConfigAndKeyRejectUnsafeFiles(t *testing.T) {
	root := t.TempDir()
	keyPath := filepath.Join(root, "app.pem")
	configPath := filepath.Join(root, "broker.json")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyData := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, keyData, 0o600); err != nil {
		t.Fatal(err)
	}
	configData := []byte(`{"app_id":"42","installation_id":987,"private_key_file":"` + keyPath + `"}`)
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil || cfg.AppID != "42" || cfg.InstallationID != 987 {
		t.Fatalf("config = %#v err=%v", cfg, err)
	}
	if _, err := NewIssuer(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewIssuer(cfg); err == nil {
		t.Fatal("world-readable key was accepted")
	}
}

func TestIssueRejectsRedirectExpiryAndRedactsUpstreamBody(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	redirect := httptest.NewServer(http.RedirectHandler("https://example.invalid/leak", http.StatusFound))
	defer redirect.Close()
	issuer := newTestIssuer(Config{AppID: "42", InstallationID: 987}, key, redirect.Client(), redirect.URL, func() time.Time { return now })
	if _, err := issuer.Issue(t.Context()); err == nil || strings.Contains(err.Error(), "example.invalid") {
		t.Fatalf("redirect error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
		_, _ = writer.Write([]byte("ghs_secret-token-from-upstream"))
	}))
	defer server.Close()
	issuer = newTestIssuer(Config{AppID: "42", InstallationID: 987}, key, server.Client(), server.URL, func() time.Time { return now })
	handler := Handler{Issuer: issuer}
	request := httptest.NewRequest(http.MethodPost, "/token", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "ghs_secret") {
		t.Fatalf("handler response = %d %q", response.Code, response.Body.String())
	}
	status := httptest.NewRecorder()
	handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
	if status.Code != http.StatusOK || strings.Contains(status.Body.String(), "token") {
		t.Fatalf("status response = %d %q", status.Code, status.Body.String())
	}
}

func sha256Digest(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}

// newTestIssuer is intentionally unexported: production callers cannot select an
// arbitrary endpoint or HTTP client through configuration.
func newTestIssuer(cfg Config, key *rsa.PrivateKey, client *http.Client, base string, now func() time.Time) *Issuer {
	if client == nil {
		client = &http.Client{}
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Issuer{cfg: cfg, key: key, client: &clone, base: strings.TrimRight(base, "/"), now: now}
}

func TestIssueRejectsInvalidAndFailedResponses(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name   string
		status int
		body   string
	}{
		{"rate limit", 429, "secret-upstream-body"},
		{"revoked installation", 401, "secret-upstream-body"},
		{"malformed", 201, "secret-upstream-body"},
		{"expired", 201, `{"token":"fixture","expires_at":"2020-01-01T00:00:00Z"}`},
		{"invalid token", 201, `{"token":"fixture\npassword=other","expires_at":"2026-09-18T12:30:00Z"}`},
		{"excessive expiry", 201, `{"token":"fixture","expires_at":"2099-01-01T00:00:00Z"}`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(scenario.status)
				_, _ = io.WriteString(w, scenario.body)
			}))
			defer server.Close()
			issuer := newTestIssuer(Config{AppID: "42", InstallationID: 1}, key, server.Client(), server.URL, time.Now)
			_, err := issuer.Issue(t.Context())
			if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "fixture") {
				t.Fatal("invalid response was accepted or leaked")
			}
		})
	}
}

func TestHandlerRejectsScopeOverridesWithoutIssuing(t *testing.T) {
	// A nil issuer would return 502 if a request reached issuance.
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/token?repository=example", nil),
		httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(`{"permissions":{"contents":"write"}}`)),
		httptest.NewRequest(http.MethodGet, "/token", nil),
	} {
		response := httptest.NewRecorder()
		NewHandler(nil).ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest && response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("unexpected validation status: %d", response.Code)
		}
	}
}

func TestProtectedFilesRejectSymlinksAndOversize(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "protected")
	if err := os.WriteFile(path, []byte("fixture-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readProtected(link, 64); err == nil {
		t.Fatal("symlink was accepted")
	}
	if _, err := readProtected(path, 2); err == nil {
		t.Fatal("oversized file was accepted")
	}
}
