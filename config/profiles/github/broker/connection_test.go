package githubbroker

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestLoadIssuerCoherentConnectionAndLocalPolicy(t *testing.T) {
	root := testkit.TempDir(t)
	cfg := filepath.Join(root, "github-app.json")
	connection := filepath.Join(root, "connection.json")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyData := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	writeBundle := func(app string) {
		data, err := json.Marshal(map[string]any{"schema_version": 1, "settings": map[string]any{"app_id": app, "installation_id": int64(987)}, "private_key": string(keyData)})
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, connection, data, 0o600)
	}
	writeBundle("42")
	issuer, err := LoadIssuer(cfg, connection)
	if err != nil || issuer.cfg.AppID != "42" || issuer.cfg.InstallationID != 987 || issuer.key.N.Cmp(key.N) != 0 {
		t.Fatalf("incoherent connection: %v", err)
	}
	testkit.WriteFile(t, cfg, []byte(`{"app_id":"99","installation_id":987}`), 0o600)
	if _, err := LoadIssuer(cfg, connection); err == nil {
		t.Fatal("unrelated local connection silently replaced")
	}
	testkit.WriteFile(t, cfg, []byte(`{"use_credential_settings":true}`), 0o600)
	writeBundle("100")
	issuer, err = LoadIssuer(cfg, connection)
	if err != nil || issuer.cfg.AppID != "100" {
		t.Fatalf("delegated ID rotation rejected: %v", err)
	}
	testkit.WriteFile(t, cfg, []byte(`{"use_credential_settings":true,"app_id":"42"}`), 0o600)
	if _, err := LoadIssuer(cfg, connection); err == nil {
		t.Fatal("mixed delegation marker accepted")
	}
	testkit.WriteFile(t, cfg, []byte(`{"use_credential_settings":true}`), 0o644)
	if _, err := LoadIssuer(cfg, connection); err == nil {
		t.Fatal("unsafe local config ignored")
	}
	testkit.WriteFile(t, cfg, []byte(`{"use_credential_settings":true}`), 0o600)
	if err := os.Remove(connection); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "github-app.pem"), keyData, 0o600)
	if _, err := LoadIssuer(cfg, connection); err == nil {
		t.Fatal("revoked connection resurrected from legacy PEM")
	}
	override := filepath.Join(root, "override.pem")
	testkit.WriteFile(t, override, keyData, 0o600)
	local, _ := json.Marshal(Config{AppID: "200", InstallationID: 123, PrivateKeyFile: override})
	testkit.WriteFile(t, cfg, local, 0o600)
	writeBundle("100")
	issuer, err = LoadIssuer(cfg, connection)
	if err != nil || issuer.cfg.AppID != "200" || issuer.cfg.InstallationID != 123 {
		t.Fatalf("local override replaced: %v", err)
	}
}
