package githubbroker

import (
	"crypto"
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
	issuer, err := LoadIssuer(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(connection))), "github-app.json"), connection)
	if err != nil {
		t.Fatal(err)
	}
	if issuer.cfg.AppID != app || issuer.cfg.InstallationID != installation {
		t.Fatal("synchronized connection settings differ")
	}
	publicPEM, err := os.ReadFile(os.Getenv("SUBYARD_GITHUB_FIXTURE_PUBLIC_KEY"))
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
	if status.Code != http.StatusOK || strings.TrimSpace(status.Body.String()) != `{"configured":true}` {
		t.Fatal("received connection is not configured")
	}
	if token, err := issuer.Issue(t.Context()); err != nil || token.Value != "ghs_synthetic-connection-fixture" {
		t.Fatal("synthetic issuer rejected the synchronized connection")
	}
}
