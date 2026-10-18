package githubbroker

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

// The pair fixture supplies a real synchronized owner file and the sending
// owner's public key. Ordinary host-free runs do not have those artifacts.
func TestConnectionFixtureIssuer(t *testing.T) {
	connection := os.Getenv("SUBYARD_GITHUB_CONNECTION_FIXTURE")
	if connection == "" {
		t.Skip("requires the GitHub connection pair fixture")
	}
	if !strings.HasPrefix(connection, "/tmp/subyard-github-connection-e2e-") {
		t.Fatal("connection fixture is outside its disposable root")
	}
	installation, err := strconv.ParseInt(os.Getenv("SUBYARD_GITHUB_FIXTURE_INSTALLATION"), 10, 64)
	if err != nil || installation <= 0 {
		t.Fatal("invalid synthetic installation fixture")
	}
	app := os.Getenv("SUBYARD_GITHUB_FIXTURE_APP")
	assertConnectionFixtureIssuer(t, connection, os.Getenv("SUBYARD_GITHUB_FIXTURE_PUBLIC_KEY"), app, installation)
}

func TestConnectionFixtureIssuerLocal(t *testing.T) {
	root := testkit.TempDir(t)
	connection := filepath.Join(root, "config", "generated", "github", "connection.json")
	if err := os.MkdirAll(filepath.Dir(connection), 0o700); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyData := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	bundle, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"settings":       map[string]any{"app_id": "123456", "installation_id": int64(42)},
		"private_key":    string(keyData),
	})
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, connection, bundle, 0o600)
	testkit.WriteFile(t, filepath.Join(root, "config", "github-app.json"), []byte("{\"use_credential_settings\":true}"), 0o600)
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(root, "public.pem")
	testkit.WriteFile(t, publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}), 0o600)
	assertConnectionFixtureIssuer(t, connection, publicPath, "123456", 42)
}

func assertConnectionFixtureIssuer(t *testing.T, connection, publicKey, app string, installation int64) {
	t.Helper()
	issuer, err := LoadIssuer(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(connection))), "github-app.json"), connection)
	if err != nil {
		t.Fatal(err)
	}
	if issuer.cfg.AppID != app || issuer.cfg.InstallationID != installation {
		t.Fatal("synchronized connection settings differ")
	}
	publicPEM, err := os.ReadFile(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(publicPEM)
	if block == nil {
		t.Fatal("invalid public-key fixture")
	}
	public, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := public.(*rsa.PublicKey)
	if !ok {
		t.Fatal("expected synthetic RSA public key")
	}
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if request.Method != http.MethodPost || request.URL.Path != "/app/installations/"+strconv.FormatInt(installation, 10)+"/access_tokens" || string(body) != "{}" {
			t.Error("issuer did not request the transferred installation")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		parts := strings.Split(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "), ".")
		if len(parts) != 3 {
			t.Error("invalid JWT")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var header struct {
			Algorithm string `json:"alg"`
		}
		var claims struct {
			Issuer string `json:"iss"`
		}
		headerJSON, headerErr := base64.RawURLEncoding.DecodeString(parts[0])
		claimsJSON, claimsErr := base64.RawURLEncoding.DecodeString(parts[1])
		signature, signatureErr := base64.RawURLEncoding.DecodeString(parts[2])
		if headerErr != nil || claimsErr != nil || signatureErr != nil || json.Unmarshal(headerJSON, &header) != nil || json.Unmarshal(claimsJSON, &claims) != nil || header.Algorithm != "RS256" || claims.Issuer != app || rsa.VerifyPKCS1v15(key, crypto.SHA256, sha256Digest(parts[0]+"."+parts[1]), signature) != nil {
			t.Error("JWT does not match the sending App ID and public key")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "ghs_synthetic-connection-fixture", "expires_at": now.Add(30 * time.Minute).Format(time.RFC3339)})
	}))
	defer server.Close()
	issuer.client, issuer.base = server.Client(), server.URL
	status := httptest.NewRecorder()
	NewHandler(issuer).ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
	var observed map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &observed); err != nil {
		t.Fatal("received connection returned an invalid status")
	}
	digest, _ := observed["executable_sha256"].(string)
	if status.Code != http.StatusOK || len(observed) != 3 || observed["configured"] != true || observed["protocol"] != float64(runtimeProtocol) || !validRuntimeDigest(digest) {
		t.Fatal("received connection is not configured")
	}
	if token, err := issuer.Issue(t.Context()); err != nil || token.Value != "ghs_synthetic-connection-fixture" {
		t.Fatal("synthetic issuer rejected the synchronized connection")
	}
}
