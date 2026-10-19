package incusclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/simplestreams"
)

type ownerImageFixture struct {
	mu             sync.Mutex
	root, config   string
	socket         string
	image          api.Image
	alias          api.ImageAliasesEntry
	missing        bool
	oversized      bool
	onRead         func()
	requests       []string
	wrongNamespace bool
}

func newOwnerImageFixture(t *testing.T) *ownerImageFixture {
	t.Helper()
	root := testkit.TempDir(t)
	fixture := &ownerImageFixture{root: root, config: filepath.Join(root, "config")}
	for _, path := range []string{fixture.config, filepath.Join(root, "global")} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range ownerImageEnvironmentKeys {
		t.Setenv(key, "")
	}
	t.Setenv("HOME", root)
	t.Setenv("INCUS_CONF", fixture.config)
	t.Setenv("INCUS_GLOBAL_CONF", filepath.Join(root, "global"))
	t.Setenv("SSL_CERT_FILE", filepath.Join(root, "missing-system-cert"))
	t.Setenv("SSL_CERT_DIR", filepath.Join(root, "missing-system-certs"))
	fixture.socket = filepath.Join(root, "incus.socket")
	t.Setenv("INCUS_SOCKET", fixture.socket)
	fixture.image = api.Image{Fingerprint: strings.Repeat("a", 64), Type: "container", Aliases: []api.ImageAlias{{Name: "debian/13"}}}
	fixture.alias = api.ImageAliasesEntry{ImageAliasesEntryPut: api.ImageAliasesEntryPut{Target: fixture.image.Fingerprint}, Name: "debian/13", Type: "container"}
	listener, err := net.Listen("unix", fixture.socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: fixture}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return fixture
}

func (fixture *ownerImageFixture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.requests = append(fixture.requests, request.Method+" "+request.URL.RequestURI())
	if request.Method != http.MethodGet || request.URL.Query().Get("project") != "original-project" {
		fixture.wrongNamespace = true
	}
	var metadata any
	switch {
	case request.URL.Path == "/1.0/images":
		if fixture.oversized {
			_, _ = io.WriteString(writer, `{"type":"sync","status_code":200,"metadata":[{"fingerprint":"`+fixture.image.Fingerprint+`","properties":{"large":"`+strings.Repeat("x", ownerImageLimit)+`"}}]}`)
			return
		}
		if fixture.onRead != nil {
			fixture.onRead()
		}
		metadata = []api.Image{fixture.image}
	case strings.HasPrefix(request.URL.Path, "/1.0/images/aliases/"):
		if strings.TrimPrefix(request.URL.Path, "/1.0/images/aliases/") != fixture.alias.Name || fixture.missing {
			writer.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(writer).Encode(map[string]any{"type": "error", "error_code": 404, "error": "alias absent"})
			return
		}
		metadata = fixture.alias
	case strings.HasPrefix(request.URL.Path, "/1.0/images/"):
		if fixture.onRead != nil {
			fixture.onRead()
		}
		if !strings.HasPrefix(fixture.image.Fingerprint, strings.TrimPrefix(request.URL.Path, "/1.0/images/")) || fixture.missing {
			writer.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(writer).Encode(map[string]any{"type": "error", "error_code": 404, "error": "image absent"})
			return
		}
		metadata = fixture.image
	default:
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"type": "sync", "status_code": 200, "metadata": metadata})
}

func (fixture *ownerImageFixture) environment() []string {
	values := slicesWithoutEnvironment(os.Environ(), "INCUS_PROJECT")
	return append(values, "INCUS_PROJECT=original-project")
}

func slicesWithoutEnvironment(environment []string, key string) []string {
	var result []string
	for _, value := range environment {
		if !strings.HasPrefix(value, key+"=") {
			result = append(result, value)
		}
	}
	return result
}

func ownerImageTLSCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func ownerImageTLSIdentity(t *testing.T) (tls.Certificate, []byte, []byte) {
	t.Helper()
	ca, configuredPEM := ownerImageTLSCertificate(t)
	parent, err := x509.ParseCertificate(ca.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: parent.NotBefore, NotAfter: parent.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, parent, &key.PublicKey, ca.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.Certificate[0]}, PrivateKey: key}, configuredPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func ownerImagePolicyFiles(t *testing.T, fixture *ownerImageFixture) (string, string) {
	t.Helper()
	for _, name := range []string{"servercerts", "clientcerts", "oidctokens"} {
		if err := os.Mkdir(filepath.Join(fixture.config, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	helper := filepath.Join(fixture.root, "credential-helper")
	testkit.WriteFile(t, helper, []byte("#!/bin/sh\n: > '"+filepath.Join(fixture.root, "policy-helper-ran")+"'\nexit 73\n"), 0o700)
	for _, name := range []string{"servercerts/primary.crt", "servercerts/fallback.crt", "clientcerts/primary.crt", "clientcerts/primary.key", "clientcerts/primary.ca", "client.crt", "client.key", "client.ca", "oidctokens/primary.json", "oidctokens/fallback.json"} {
		// Invalid credential material proves Capture does not ask the SDK to
		// parse/decrypt it or authenticate; only file identities are retained.
		testkit.WriteFile(t, filepath.Join(fixture.config, name), []byte("synthetic-private-material-marker"), 0o600)
	}
	return helper, filepath.Join(fixture.root, "policy-helper-ran")
}

func TestOwnerImagePolicyCaptureAndCheckAreFilesystemOnly(t *testing.T) {
	for _, mode := range []string{"private TLS", "OCI helper", "OIDC", "redirect authority", "missing fallback"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newOwnerImageFixture(t)
			helper, marker := ownerImagePolicyFiles(t, fixture)
			var connections atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("policy performed an HTTP request") }))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			server.StartTLS()
			t.Cleanup(server.Close)
			protocol, auth := "incus", "tls"
			if mode == "OCI helper" {
				protocol = "oci"
			}
			if mode == "OIDC" {
				auth = "oidc"
			}
			if mode == "redirect authority" {
				protocol = "simplestreams"
			}
			configuration := fmt.Sprintf("remotes:\n  primary:\n    addr: %q\n    protocol: %s\n    auth_type: %s\n    credentials_helper: %q\n  fallback:\n    addr: %q\n    protocol: oci\n    credentials_helper: %q\n", server.URL+"?access_token=synthetic-private-material-marker#fragment", protocol, auth, helper, server.URL, helper)
			testkit.WriteFile(t, filepath.Join(fixture.config, "config.yml"), []byte(configuration), 0o600)
			fallback := "fallback:ubuntu/24.04"
			if mode == "missing fallback" {
				fallback = "unavailable:ubuntu/24.04"
			}
			client := New(fixture.socket)
			p, err := client.CaptureOwnerImagePolicy(context.Background(), "primary:debian/13", fallback, domain.YardVM, fixture.environment(), fixture.root)
			if err != nil {
				t.Fatal(err)
			}
			if p.Version != 1 || string(p.Primary) != "primary:debian/13" || string(p.Fallback) != fallback || p.Kind != "virtual-machine" || string(p.Project) != "original-project" {
				t.Fatal("policy changed the original reference/kind/project envelope")
			}
			if err := client.CheckOwnerImagePolicy(context.Background(), p, fixture.environment(), fixture.root); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(p)
			if err != nil || strings.Contains(string(raw), "synthetic-private-material-marker") || strings.Contains(string(raw), server.URL) {
				t.Fatal("policy retained credential contents or a source URL")
			}
			if connections.Load() != 0 || len(fixture.requests) != 0 {
				t.Fatal("filesystem policy opened an image-source connection")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("filesystem policy executed a credential helper")
			}
		})
	}
}

func TestOwnerImagePolicyGuardsBothCredentialSources(t *testing.T) {
	for _, changed := range []string{"config.yml", "global config", "servercerts/primary.crt", "servercerts/fallback.crt", "clientcerts/primary.crt", "clientcerts/primary.key", "clientcerts/primary.ca", "client.key", "oidctokens/fallback.json", "helper", "new remote key"} {
		t.Run(changed, func(t *testing.T) {
			fixture := newOwnerImageFixture(t)
			helper, _ := ownerImagePolicyFiles(t, fixture)
			configuration := fmt.Sprintf("remotes:\n  primary:\n    addr: https://example.invalid\n    protocol: incus\n    auth_type: tls\n  fallback:\n    addr: https://fallback.invalid\n    protocol: oci\n    auth_type: oidc\n    credentials_helper: %q\n", helper)
			testkit.WriteFile(t, filepath.Join(fixture.config, "config.yml"), []byte(configuration), 0o600)
			if changed == "new remote key" {
				if err := os.Remove(filepath.Join(fixture.config, "clientcerts", "primary.key")); err != nil {
					t.Fatal(err)
				}
			}
			client := New(fixture.socket)
			p, err := client.CaptureOwnerImagePolicy(context.Background(), "primary:debian/13", "fallback:ubuntu/24.04", domain.YardContainer, fixture.environment(), fixture.root)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(fixture.config, changed)
			switch changed {
			case "global config":
				path = filepath.Join(fixture.root, "global", "config.yml")
			case "helper":
				path = helper
			case "new remote key":
				path = filepath.Join(fixture.config, "clientcerts", "primary.key")
			}
			testkit.WriteFile(t, path, []byte("changed-original-source"), 0o600)
			if err := client.CheckOwnerImagePolicy(context.Background(), p, fixture.environment(), fixture.root); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("changed original credential/config source accepted: %v", err)
			}
			if len(fixture.requests) != 0 {
				t.Fatal("changed policy guard attempted image resolution")
			}
		})
	}
}

func TestOwnerImagePolicyPreservesRawInputsAndRejectsChangedEnvironment(t *testing.T) {
	fixture := newOwnerImageFixture(t)
	fixture.config = filepath.Join(fixture.root, string([]byte{'c', 0xff}))
	if err := os.Mkdir(fixture.config, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INCUS_CONF", fixture.config)
	client := New(fixture.socket)
	primary, fallback := "local:unresolved-primary", "local:unresolved-fallback"
	p, err := client.CaptureOwnerImagePolicy(context.Background(), primary, fallback, domain.YardContainer, fixture.environment(), fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, guard := range p.Guards {
		found = found || string(guard.Path) == filepath.Join(fixture.config, "config.yml")
	}
	if !found || string(p.Primary) != primary || string(p.Fallback) != fallback || len(fixture.requests) != 0 {
		t.Fatal("policy resolved references or changed raw source paths")
	}
	for _, key := range []string{"INCUS_PROJECT", "PATH", "PWD"} {
		changed := append(slicesWithoutEnvironment(fixture.environment(), key), key+"=different-original-value")
		if err := client.CheckOwnerImagePolicy(context.Background(), p, changed, fixture.root); !errors.Is(err, domain.ErrPlanStale) {
			t.Fatalf("changed %s accepted", key)
		}
	}
	changed := append(slicesWithoutEnvironment(fixture.environment(), "INCUS_CONF"), "INCUS_CONF="+filepath.Join(fixture.root, string([]byte{'c', 0xfe})))
	if err := client.CheckOwnerImagePolicy(context.Background(), p, changed, fixture.root); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("distinct raw environment path bytes collapsed")
	}
	if _, err := client.CaptureOwnerImagePolicy(context.Background(), "https://user:synthetic-credential-marker@example.invalid", fallback, domain.YardContainer, fixture.environment(), fixture.root); err == nil || strings.Contains(err.Error(), "synthetic-credential-marker") {
		t.Fatal("policy accepted or disclosed a credential URL as an image reference")
	}
}

func ownerPolicyAcceptedContext(ctx context.Context) context.Context {
	return ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(ctx, &custodyObserver{}), "synthetic-accepted-owner-component")
}

func ownerPolicyStreamReply(writer http.ResponseWriter, request *http.Request, fingerprint string) {
	writer.Header().Set("Content-Type", "application/json")
	if strings.Contains(request.URL.Path, "index") {
		_ = json.NewEncoder(writer).Encode(simplestreams.Stream{Index: map[string]simplestreams.StreamIndex{"images": {DataType: "image-downloads", Path: "streams/v1/images.json", Products: []string{"debian"}}}})
		return
	}
	_ = json.NewEncoder(writer).Encode(simplestreams.Products{Products: map[string]simplestreams.Product{"debian": {Architecture: "x86_64", Aliases: "debian/13", Versions: map[string]simplestreams.ProductVersion{"20261019": {Items: map[string]simplestreams.ProductVersionItem{"meta": {FileType: "incus_combined.tar.gz", Path: "images/meta.tar.gz", HashSha256: fingerprint}}}}}}})
}

func ownerPolicyTLSServer(t *testing.T, certificate tls.Certificate, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func ownerPolicyRemoteConfiguration(t *testing.T, fixture *ownerImageFixture, address, protocol string, certificate []byte) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(fixture.config, "servercerts"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(fixture.config, "servercerts", "primary.crt"), certificate, 0o600)
	testkit.WriteFile(t, filepath.Join(fixture.config, "config.yml"), []byte(fmt.Sprintf("remotes:\n  primary:\n    addr: %q\n    protocol: %s\n    public: true\n    project: ignored-public-project\n", address, protocol)), 0o600)
}

func TestOwnerImagePolicyAcceptedPublicIncusResolution(t *testing.T) {
	for _, kind := range []domain.YardKind{domain.YardContainer, domain.YardVM} {
		t.Run(string(kind), func(t *testing.T) {
			fixture := newOwnerImageFixture(t)
			certificate, configured, observed := ownerImageTLSIdentity(t)
			image := fixture.image
			image.Public = true
			if kind == domain.YardVM {
				image.Type = "virtual-machine"
			}
			var requests atomic.Int32
			server := ownerPolicyTLSServer(t, certificate, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				if request.Method != http.MethodGet || request.URL.Query().Get("project") != "" || strings.Contains(request.URL.Path, "/secret") {
					t.Error("public metadata changed the official project/read-only semantics")
				}
				var value any
				switch request.URL.Path {
				case "/1.0/images":
					value = []api.Image{image}
				case "/1.0/images/aliases/debian/13":
					value = api.ImageAliasesEntry{ImageAliasesEntryPut: api.ImageAliasesEntryPut{Target: image.Fingerprint}, Name: "debian/13", Type: image.Type}
				case "/1.0/images/" + image.Fingerprint:
					value = image
				default:
					t.Error("unexpected public metadata request")
					writer.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(writer).Encode(map[string]any{"type": "sync", "status_code": 200, "metadata": value})
			}))
			ownerPolicyRemoteConfiguration(t, fixture, server.URL, "incus", configured)
			client := New(fixture.socket)
			policy, err := client.CaptureOwnerImagePolicy(context.Background(), "primary:debian/13", "local:fallback", kind, fixture.environment(), fixture.root)
			if err != nil || requests.Load() != 0 {
				t.Fatal("policy capture connected to public Incus", err)
			}
			if _, err := client.ReadOwnerPolicyImageCatalog(context.Background(), policy, fixture.environment(), fixture.root); !errors.Is(err, domain.ErrPlanStale) || requests.Load() != 0 {
				t.Fatal("catalog ran before accepted component custody")
			}
			ctx := ownerPolicyAcceptedContext(context.Background())
			raw, err := client.ReadOwnerPolicyImageCatalog(ctx, policy, fixture.environment(), fixture.root)
			var catalog []api.Image
			if err != nil || json.Unmarshal(raw, &catalog) != nil || len(catalog) != 1 {
				t.Fatal("accepted public Incus catalog failed", err)
			}
			source, _, err := client.resolveOwnerPolicyImageSource(ctx, policy, "primary:debian/13", "current", fixture.environment(), fixture.root)
			if err != nil || source.Type != "image" || source.Mode != "pull" || source.Protocol != "incus" || source.Server != server.URL || source.Certificate != string(configured) || source.Certificate == string(observed) || source.Fingerprint != image.Fingerprint || source.Alias != "" || source.Secret != "" || source.Project != "" {
				t.Fatal("accepted source did not retain the configured-certificate and fixed-fingerprint semantics", err)
			}
			before := requests.Load()
			if _, _, err := client.resolveOwnerPolicyImageSource(ctx, policy, "unapproved:other", "current", fixture.environment(), fixture.root); !errors.Is(err, domain.ErrPlanStale) || requests.Load() != before {
				t.Fatal("selected reference escaped the original two-reference envelope")
			}
		})
	}
}

func TestOwnerImagePolicyLocalSelectedReferenceAndKind(t *testing.T) {
	for _, scenario := range []string{"container", "virtual-machine", "full-fingerprint", "fingerprint-prefix", "missing", "wrong-kind", "short-fingerprint", "invalid-alias-target", "foreign-reference", "changed-config"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newOwnerImageFixture(t)
			// An empty Client socket follows the original INCUS_SOCKET default.
			client := New("")
			kind := domain.YardContainer
			if scenario == "virtual-machine" || scenario == "wrong-kind" {
				kind = domain.YardVM
			}
			if scenario == "virtual-machine" {
				fixture.alias.Type, fixture.image.Type = "virtual-machine", "virtual-machine"
			}
			if scenario == "missing" {
				fixture.missing = true
			}
			if scenario == "short-fingerprint" {
				fixture.alias.Target, fixture.image.Fingerprint = "abc123", "abc123"
			}
			if scenario == "invalid-alias-target" {
				fixture.alias.Target = "invalid-original-target"
			}
			primary := "local:debian/13"
			if scenario == "full-fingerprint" {
				primary = "local:" + fixture.image.Fingerprint
			}
			if scenario == "fingerprint-prefix" {
				primary = "local:" + fixture.image.Fingerprint[:12]
			}
			policy, err := client.CaptureOwnerImagePolicy(context.Background(), primary, "local:fallback", kind, fixture.environment(), fixture.root)
			if err != nil {
				t.Fatal(err)
			}
			selected := primary
			if scenario == "foreign-reference" {
				selected = "local:foreign"
			}
			if scenario == "changed-config" {
				testkit.WriteFile(t, filepath.Join(fixture.config, "config.yml"), []byte("default-remote: local\n"), 0o600)
			}
			source, secret, err := client.resolveOwnerPolicyImageSource(ownerPolicyAcceptedContext(context.Background()), policy, selected, "current", fixture.environment(), fixture.root)
			if scenario == "container" || scenario == "virtual-machine" || scenario == "full-fingerprint" || scenario == "fingerprint-prefix" {
				if err != nil || secret != nil || source.Fingerprint != fixture.image.Fingerprint || source.Project != "original-project" || source.Protocol != "incus" || source.Server != "" {
					t.Fatal("original same-target local source failed", err)
				}
			} else if err == nil || secret != nil || source.Fingerprint != "" {
				t.Fatal("invalid source gained create input", scenario)
			}
			if (scenario == "foreign-reference" || scenario == "changed-config") && len(fixture.requests) != 0 {
				t.Fatal("unapproved source reached metadata")
			}
			if scenario == "invalid-alias-target" && len(fixture.requests) != 1 {
				t.Fatal("invalid alias target reached image metadata")
			}
			if fixture.wrongNamespace {
				t.Fatal("local image resolution changed original project")
			}
		})
	}
}

func TestOwnerImagePolicyAcceptedNormalRedirectsAndTrust(t *testing.T) {
	for _, mode := range []string{"same host", "other host", "untrusted redirect"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newOwnerImageFixture(t)
			certificate, configured, observed := ownerImageTLSIdentity(t)
			var delegated atomic.Int32
			targetCertificate := certificate
			if mode == "untrusted redirect" {
				targetCertificate, _ = ownerImageTLSCertificate(t)
			}
			target := ownerPolicyTLSServer(t, targetCertificate, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				delegated.Add(1)
				ownerPolicyStreamReply(writer, request, fixture.image.Fingerprint)
			}))
			primary := ownerPolicyTLSServer(t, certificate, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/streams/v1/index.json" {
					location := "/delegated-index?access_token=synthetic-redirect-marker"
					if mode != "same host" {
						location = target.URL + location
					}
					http.Redirect(writer, request, location, http.StatusFound)
					return
				}
				delegated.Add(1)
				ownerPolicyStreamReply(writer, request, fixture.image.Fingerprint)
			}))
			ownerPolicyRemoteConfiguration(t, fixture, primary.URL, "simplestreams", configured)
			client := New(fixture.socket)
			policy, err := client.CaptureOwnerImagePolicy(context.Background(), "primary:debian/13", "local:fallback", domain.YardContainer, fixture.environment(), fixture.root)
			if err != nil {
				t.Fatal(err)
			}
			ctx := ownerPolicyAcceptedContext(context.Background())
			source, _, err := client.resolveOwnerPolicyImageSource(ctx, policy, "primary:debian/13", "current", fixture.environment(), fixture.root)
			if mode == "untrusted redirect" {
				if err == nil || strings.Contains(err.Error(), "synthetic-redirect-marker") || source.Fingerprint != "" || source.Server != "" || source.Certificate != "" {
					t.Fatal("redirect trust was bypassed or its credential URL disclosed")
				}
				return
			}
			if err != nil || delegated.Load() == 0 || source.Server != primary.URL || source.Certificate != string(configured) || source.Certificate == string(observed) || source.Fingerprint != fixture.image.Fingerprint || strings.Contains(source.Server, "synthetic-redirect-marker") {
				t.Fatal("normal SDK redirect/configured trust semantics were changed", err)
			}
			if _, err := client.ReadOwnerPolicyImageCatalog(ctx, policy, fixture.environment(), fixture.root); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOwnerImagePolicyRedirectRetriesVerifiedEndpointHostname(t *testing.T) {
	for _, validName := range []bool{true, false} {
		t.Run(fmt.Sprint(validName), func(t *testing.T) {
			fixture := newOwnerImageFixture(t)
			ca, configured := ownerImageTLSCertificate(t) // SDK initial ServerName=localhost
			parent, err := x509.ParseCertificate(ca.Certificate[0])
			if err != nil {
				t.Fatal(err)
			}
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			leaf := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: parent.NotBefore, NotAfter: parent.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			if validName {
				leaf.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
			} else {
				leaf.DNSNames = []string{"different.invalid"}
			}
			der, err := x509.CreateCertificate(rand.Reader, leaf, parent, &key.PublicKey, ca.PrivateKey)
			if err != nil {
				t.Fatal(err)
			}
			target := ownerPolicyTLSServer(t, tls.Certificate{Certificate: [][]byte{der, ca.Certificate[0]}, PrivateKey: key}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ownerPolicyStreamReply(w, r, fixture.image.Fingerprint) }))
			primary := ownerPolicyTLSServer(t, ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/streams/v1/index.json" {
					http.Redirect(w, r, target.URL+"/delegated-index", http.StatusFound)
					return
				}
				ownerPolicyStreamReply(w, r, fixture.image.Fingerprint)
			}))
			// The pinned original SDK accepts a separately named trusted leaf by
			// retrying with the redirected endpoint hostname, never skipping trust.
			original, err := incus.ConnectSimpleStreams(primary.URL, &incus.ConnectionArgs{TLSServerCert: string(configured), HTTPClient: &http.Client{Timeout: 2 * time.Second}})
			if err != nil {
				t.Fatal(err)
			}
			_, originalErr := original.GetImages()
			if (originalErr == nil) != validName {
				t.Fatal("fixture does not reproduce original verified hostname retry", originalErr)
			}
			ownerPolicyRemoteConfiguration(t, fixture, primary.URL, "simplestreams", configured)
			client := New(fixture.socket)
			policy, err := client.CaptureOwnerImagePolicy(context.Background(), "primary:debian/13", "local:fallback", domain.YardContainer, fixture.environment(), fixture.root)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ReadOwnerPolicyImageCatalog(ownerPolicyAcceptedContext(context.Background()), policy, fixture.environment(), fixture.root)
			if (err == nil) != validName {
				t.Fatal("bounded transport changed original verified hostname retry", err)
			}
		})
	}
}

func TestOwnerImagePolicyAcceptedCatalogRetriesOnlyTransientReads(t *testing.T) {
	for _, mode := range []string{"reset twice", "HTTP failure", "deadline", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newOwnerImageFixture(t)
			certificate, configured, _ := ownerImageTLSIdentity(t)
			var requests atomic.Int32
			var metadataCancel context.CancelCauseFunc
			server := ownerPolicyTLSServer(t, certificate, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				count := requests.Add(1)
				switch mode {
				case "reset twice":
					if count <= 2 {
						connection, _, _ := writer.(http.Hijacker).Hijack()
						raw := connection.(*tls.Conn).NetConn().(*net.TCPConn)
						_ = raw.SetLinger(0)
						_ = raw.Close()
						return
					}
				case "HTTP failure":
					writer.WriteHeader(http.StatusServiceUnavailable)
					return
				case "deadline":
					if count == 1 {
						// Begin after TLS setup so race instrumentation cannot
						// spend the multi-document budget before the first read.
						timer := time.AfterFunc(300*time.Millisecond, func() { metadataCancel(context.DeadlineExceeded) })
						t.Cleanup(func() { timer.Stop() })
					}
					select {
					case <-request.Context().Done():
						return
					case <-time.After(200 * time.Millisecond):
					}
				}
				ownerPolicyStreamReply(writer, request, fixture.image.Fingerprint)
			}))
			ownerPolicyRemoteConfiguration(t, fixture, server.URL, "simplestreams", configured)
			client := New(fixture.socket)
			policy, err := client.CaptureOwnerImagePolicy(context.Background(), "primary:debian/13", "local:fallback", domain.YardContainer, fixture.environment(), fixture.root)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(ownerPolicyAcceptedContext(context.Background()))
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			if mode == "deadline" {
				ctx, metadataCancel = context.WithCancelCause(ctx)
				defer metadataCancel(context.Canceled)
			}
			raw, err := client.ReadOwnerPolicyImageCatalog(ctx, policy, fixture.environment(), fixture.root)
			switch mode {
			case "reset twice":
				if err != nil || len(raw) == 0 || requests.Load() != 4 {
					t.Fatalf("read-only transient attempts were not retained: requests=%d err=%v", requests.Load(), err)
				}
			case "HTTP failure":
				if err == nil || raw != nil || requests.Load() != 1 {
					t.Fatal("terminal HTTP failure was retried or established absence")
				}
			case "deadline":
				if !errors.Is(err, context.DeadlineExceeded) || raw != nil || requests.Load() != 2 {
					t.Fatalf("timeout did not bound the whole multi-document read: %d %v", requests.Load(), err)
				}
			case "cancelled":
				if !errors.Is(err, context.Canceled) || raw != nil || requests.Load() != 0 {
					t.Fatal("cancelled catalog started more work")
				}
			}
		})
	}
}

func TestOwnerImagePolicyMetadataRefusesUncustodiedAuthentication(t *testing.T) {
	for _, mode := range []string{"private Incus", "OIDC", "OCI helper", "public client credentials", "credential URL", "fragment URL", "empty query URL"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newOwnerImageFixture(t)
			helper, marker := ownerImagePolicyFiles(t, fixture)
			var connections atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("uncustodied authentication reached HTTP") }))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			server.StartTLS()
			t.Cleanup(server.Close)
			protocol, auth, public, address := "incus", "tls", false, server.URL
			switch mode {
			case "OIDC":
				auth = "oidc"
			case "OCI helper":
				protocol = "oci"
			case "public client credentials":
				public = true
			case "credential URL":
				protocol = "simplestreams"
				address += "?access_token=synthetic-credential-marker"
			case "fragment URL":
				protocol = "simplestreams"
				address += "#synthetic-credential-marker"
			case "empty query URL":
				protocol = "simplestreams"
				address += "?"
			}
			testkit.WriteFile(t, filepath.Join(fixture.config, "config.yml"), []byte(fmt.Sprintf("remotes:\n  primary:\n    addr: %q\n    protocol: %s\n    auth_type: %s\n    public: %t\n    credentials_helper: %q\n", address, protocol, auth, public, helper)), 0o600)
			client := New(fixture.socket)
			policy, err := client.CaptureOwnerImagePolicy(context.Background(), "primary:debian/13", "local:fallback", domain.YardContainer, fixture.environment(), fixture.root)
			if err != nil {
				t.Fatal(err)
			}
			ctx := ownerPolicyAcceptedContext(context.Background())
			raw, catalogErr := client.ReadOwnerPolicyImageCatalog(ctx, policy, fixture.environment(), fixture.root)
			source, _, resolveErr := client.resolveOwnerPolicyImageSource(ctx, policy, "primary:debian/13", "current", fixture.environment(), fixture.root)
			if !errors.Is(catalogErr, errOwnerPolicyImageAuthentication) || !errors.Is(resolveErr, errOwnerPolicyImageAuthentication) || raw != nil || source.Fingerprint != "" || source.Server != "" || strings.Contains(catalogErr.Error(), "synthetic-credential-marker") || strings.Contains(resolveErr.Error(), "synthetic-credential-marker") {
				t.Fatal("authentication was attempted or credential data escaped its required custody")
			}
			if connections.Load() != 0 || len(fixture.requests) != 0 {
				t.Fatal("uncustodied metadata opened a source connection")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("uncustodied metadata executed a helper")
			}
		})
	}
}

func TestOwnerImagePolicyMetadataCancellationClosesStalledTLS(t *testing.T) {
	for _, protocol := range []string{"incus", "simplestreams"} {
		t.Run(protocol, func(t *testing.T) {
			fixture := newOwnerImageFixture(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			accepted, hello, closed := make(chan net.Conn, 1), make(chan struct{}), make(chan struct{})
			go func() {
				defer close(closed)
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				defer connection.Close()
				accepted <- connection
				if n, _ := connection.Read(make([]byte, 4096)); n > 0 {
					close(hello)
				}
				_, _ = io.Copy(io.Discard, connection)
			}()
			_, certificate := ownerImageTLSCertificate(t)
			ownerPolicyRemoteConfiguration(t, fixture, "https://"+listener.Addr().String(), protocol, certificate)
			client := New(fixture.socket)
			policy, err := client.CaptureOwnerImagePolicy(context.Background(), "primary:debian/13", "local:fallback", domain.YardContainer, fixture.environment(), fixture.root)
			if err != nil {
				t.Fatal(err)
			}
			deadline, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			ctx, cancel := context.WithCancel(ownerPolicyAcceptedContext(deadline))
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := client.ReadOwnerPolicyImageCatalog(ctx, policy, fixture.environment(), fixture.root)
				result <- err
			}()
			select {
			case connection := <-accepted:
				defer connection.Close()
			case <-deadline.Done():
				t.Fatal("metadata TCP dial missing")
			}
			select {
			case <-hello:
			case <-deadline.Done():
				t.Fatal("metadata TLS handshake missing")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("canceled metadata gained a catalog", err)
				}
			case <-deadline.Done():
				t.Fatal("metadata read outlived cancellation")
			}
			select {
			case <-closed:
			case <-deadline.Done():
				t.Fatal("metadata-owned TLS socket outlived cleanup")
			}
		})
	}
}

func TestOwnerImagePolicyOCIPrimaryRetainsCatalogErrorWithoutSourceEffects(t *testing.T) {
	fixture := newOwnerImageFixture(t)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	t.Cleanup(server.Close)
	ownerPolicyRemoteConfiguration(t, fixture, server.URL, "oci", nil)
	client := New(fixture.socket)
	policy, err := client.CaptureOwnerImagePolicy(context.Background(), "primary:library/alpine:3.22", "local:fallback", domain.YardContainer, fixture.environment(), fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := ownerPolicyAcceptedContext(context.Background())
	if raw, err := client.ReadOwnerPolicyImageCatalog(ctx, policy, fixture.environment(), fixture.root); err == nil || errors.Is(err, errOwnerPolicyImageAuthentication) || raw != nil {
		t.Fatal("OCI primary catalog error became fallback or authentication", err)
	}
	if source, secret, err := client.resolveOwnerPolicyImageSource(ctx, policy, string(policy.Primary), "current", fixture.environment(), fixture.root); err == nil || secret != nil || source.Type != "" {
		t.Fatal("OCI primary bypassed its original catalog error", err)
	}
	if requests.Load() != 0 || len(fixture.requests) != 0 {
		t.Fatal("OCI connection/catalog issued a source request")
	}
}
