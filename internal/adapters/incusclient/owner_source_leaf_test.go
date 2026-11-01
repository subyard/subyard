package incusclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/sys/unix"
)

// This test-only capability is explicit: production mints it only from Serve.
// The selected engine is a real freshly built cmd/yard, not a test dispatch.
type ownerSourceTestImage struct {
	file   *os.File
	digest string
}

func (c ownerSourceTestImage) Borrow() (*os.File, string, error) {
	fd, err := unix.FcntlInt(c.file.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, "", err
	}
	return os.NewFile(uintptr(fd), "test retained actual engine"), c.digest, nil
}
func sourceTestImage(t *testing.T) ports.NativeSelfExecutable {
	t.Helper()
	path := os.Getenv("SUBYARD_NATIVE_SOURCE_ENGINE")
	if path == "" {
		t.Skip("opt-in actual built source engine")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (128<<20)+1))
	if err != nil || n > 128<<20 {
		t.Fatal("engine proof", err)
	}
	return ownerSourceTestImage{f, fmt.Sprintf("%x", h.Sum(nil))}
}

type sourceTestObserver struct {
	*ownerCreateObserverFixture
	cacheMu           sync.Mutex
	caches            []ports.OwnerImageCacheSuccessor
	denyCache         bool
	persistBeforeDeny bool
	started, closed   int
}

func (o *sourceTestObserver) CommandStarted(kind, source string) (uint64, error) {
	o.cacheMu.Lock()
	defer o.cacheMu.Unlock()
	if !strings.HasPrefix(kind, "owner-source-") || len(source) != 64 {
		return 0, errors.New("source command binding")
	}
	o.started++
	return uint64(o.started), nil
}
func (o *sourceTestObserver) CommandTerminal(n uint64) error {
	o.cacheMu.Lock()
	defer o.cacheMu.Unlock()
	if n != uint64(o.started) {
		return errors.New("source command ordinal")
	}
	o.closed++
	return nil
}
func (o *sourceTestObserver) RecordOwnerImageCache(v ports.OwnerImageCacheSuccessor) error {
	o.cacheMu.Lock()
	defer o.cacheMu.Unlock()
	if o.denyCache {
		if o.persistBeforeDeny {
			o.caches = append(o.caches, v)
		}
		return errors.New("withheld durable cache ack")
	}
	o.caches = append(o.caches, v)
	return nil
}
func (o *sourceTestObserver) ReadOwnerImageCache(key string) ([]ports.OwnerImageCacheSuccessor, error) {
	o.cacheMu.Lock()
	defer o.cacheMu.Unlock()
	var result []ports.OwnerImageCacheSuccessor
	for _, v := range o.caches {
		if v.Policy == key {
			result = append(result, v)
		}
	}
	return result, nil
}
func sourceTestContext(t *testing.T, home string, image ports.NativeSelfExecutable, o *sourceTestObserver) context.Context {
	t.Helper()
	_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	ctx, stop := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(stop)
	ctx = ports.WithNativeOperationObserver(ctx, o)
	ctx = ports.WithNativeInvocationKey(ctx, strings.Repeat("e", 64))
	ctx = ports.WithNativeOwnerHome(ctx, home)
	return ports.WithNativeSelfExecutable(ctx, image)
}

func TestOwnerSourceActualLeafOriginalEnvironmentAndDirectory(t *testing.T) {
	// Retain a copied actual image, then replace its live pathname. Only the
	// already-bound FD may run; a fresh pathname lookup would run the refusal.
	originalImage := sourceTestImage(t)
	file, _, err := originalImage.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(testkit.TempDir(t), "engine")
	out, err := os.OpenFile(imagePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, io.NewSectionReader(file, 0, 128<<20)); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(imagePath, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBYARD_NATIVE_SOURCE_ENGINE", imagePath)
	image := sourceTestImage(t)
	if err := os.Rename(imagePath, imagePath+".original"); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, imagePath, []byte("#!/bin/sh\nexit 77\n"), 0o700)
	fixture := newOwnerImageFixture(t)
	directory := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(fixture.config, "config.yml"), []byte("remotes:\n  local:\n    addr: unix:"+fixture.socket+"\n    protocol: incus\n    project: original-project\n"), 0o600)
	environment := append(fixture.environment(), "ORIGINAL_HELPER_VALUE=original-\xff")
	ctx := ports.WithNativePreparationExecutable(context.Background(), image)
	ctx = ports.WithNativeSourceDirectory(ctx, directory)
	policy, err := New(fixture.socket).CaptureOwnerImagePolicy(ctx, "local:debian/13", "local:fallback", domain.YardContainer, environment, fixture.root)
	if err != nil {
		t.Fatal("original isolated preparation", err)
	}
	if string(policy.Directory) != directory {
		t.Fatal("original option directory lost")
	}
	t.Setenv("INCUS_CONF", filepath.Join(fixture.root, "foreign-live-config"))
	t.Setenv("ORIGINAL_HELPER_VALUE", "foreign")
	if err := New(fixture.socket).CheckOwnerImagePolicy(ctx, policy, environment, fixture.root); err != nil {
		t.Fatal("helper ambient adopted over original env", err)
	}
	retained := &sourceTestObserver{ownerCreateObserverFixture: &ownerCreateObserverFixture{}}
	readonly := ports.WithNativeOperationObserver(ctx, retained)
	if err := New(fixture.socket).CheckOwnerImagePolicy(readonly, policy, environment, fixture.root); err != nil {
		t.Fatalf("read-only retained source check: owner_ofd=%t type=%T", strings.Contains(err.Error(), "native source owner OFD unavailable"), err)
	}
	if retained.started != 0 || retained.closed != 0 {
		t.Fatal("read-only source check used mutation custody")
	}
	if err := New(fixture.socket).CheckOwnerImagePolicy(ports.WithNativeInvocationKey(readonly, strings.Repeat("e", 64)), policy, environment, fixture.root); err == nil {
		t.Fatal("active source check accepted missing owner OFD")
	}
	changed := slices.Clone(environment)
	for i, s := range changed {
		if strings.HasPrefix(s, "INCUS_CONF=") {
			changed[i] = "INCUS_CONF=/foreign"
		}
	}
	if err := New(fixture.socket).CheckOwnerImagePolicy(ctx, policy, changed, fixture.root); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("changed original config accepted", err)
	}
	moved := directory + ".original"
	if err := os.Rename(directory, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(directory); os.Rename(moved, directory) })
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := New(fixture.socket).CheckOwnerImagePolicy(ctx, policy, environment, fixture.root); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("replacement original CWD accepted", err)
	}
}

func TestOwnerSourceActualOIDCRefreshMetadataFailureAndCacheAck(t *testing.T) {
	for _, scenario := range []string{"known-save", "missing-fact", "withheld-ack"} {
		t.Run(scenario, func(t *testing.T) {
			image := sourceTestImage(t)
			fixture := newOwnerImageFixture(t)
			directory := testkit.TempDir(t)
			var server *httptest.Server
			var mu sync.Mutex
			refreshes := 0
			acceptAccess := false
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					_ = json.NewEncoder(w).Encode(map[string]any{"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token", "jwks_uri": server.URL + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
				case "/token":
					mu.Lock()
					expectedRefresh := "original-refresh"
					if refreshes > 0 {
						expectedRefresh = "synthetic-rotated"
					}
					mu.Unlock()
					if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != expectedRefresh {
						t.Error("wrong original refresh request")
						w.WriteHeader(400)
						return
					}
					mu.Lock()
					refreshes++
					acceptAccess = true
					mu.Unlock()
					_, _ = io.WriteString(w, `{"access_token":"synthetic-refreshed","refresh_token":"synthetic-rotated","token_type":"Bearer","expires_in":3600}`)
				default:
					mu.Lock()
					accepted := acceptAccess
					mu.Unlock()
					if !accepted || r.Header.Get("Authorization") != "Bearer synthetic-refreshed" {
						w.Header().Set("X-Incus-OIDC-issuer", server.URL)
						w.Header().Set("X-Incus-OIDC-clientid", "synthetic-client")
						w.WriteHeader(401)
						_, _ = io.WriteString(w, `{"type":"error","error_code":401,"error":"refresh required"}`)
						return
					}
					w.WriteHeader(500)
					_, _ = io.WriteString(w, `{"type":"error","error_code":500,"error":"synthetic metadata failure"}`)
				}
			}))
			defer server.Close()
			for _, name := range []string{"servercerts", "oidctokens"} {
				if err := os.MkdirAll(filepath.Join(fixture.config, name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			testkit.WriteFile(t, filepath.Join(fixture.config, "servercerts/source.crt"), certificate, 0o600)
			// Discovery uses the same original system trust inputs as SDK transport.
			testkit.WriteFile(t, filepath.Join(fixture.root, "roots.pem"), certificate, 0o600)
			t.Setenv("SSL_CERT_FILE", filepath.Join(fixture.root, "roots.pem"))
			testkit.WriteFile(t, filepath.Join(fixture.config, "config.yml"), []byte(fmt.Sprintf("remotes:\n  source:\n    addr: %q\n    protocol: incus\n    auth_type: oidc\n    project: original-project\n", server.URL)), 0o600)
			tokens := oidc.Tokens[*oidc.IDTokenClaims]{Token: &oauth2.Token{AccessToken: "original-expired", RefreshToken: "original-refresh", TokenType: "Bearer"}}
			raw, _ := json.Marshal(&tokens)
			path := filepath.Join(fixture.config, "oidctokens/source.json")
			testkit.WriteFile(t, path, raw, 0o600)
			environment := fixture.environment()
			original, err := New(fixture.socket).CaptureOwnerImagePolicy(context.Background(), "source:debian/13", "local:fallback", domain.YardContainer, environment, fixture.root)
			if err != nil {
				t.Fatal(err)
			}
			observer := &sourceTestObserver{ownerCreateObserverFixture: &ownerCreateObserverFixture{}, denyCache: scenario != "known-save", persistBeforeDeny: scenario == "withheld-ack"}
			ctx := sourceTestContext(t, fixture.root, image, observer)
			// The direct capture's actual original directory is the package CWD.
			catalog, err := New(fixture.socket).ReadOwnerPolicyImageCatalog(ctx, original, environment, fixture.root)
			if err == nil || len(catalog) != 0 {
				t.Fatal("metadata failure became success")
			}
			saved, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Contains(saved, []byte("synthetic-rotated")) || ownerImageCacheFile(path) != nil {
				t.Fatal("refreshed SDK token discarded", readErr)
			}
			mu.Lock()
			count := refreshes
			mu.Unlock()
			if count != 1 {
				t.Fatal("refresh replayed", count)
			}
			if scenario == "known-save" {
				if len(observer.caches) != 1 || observer.uncertain {
					t.Fatal("known rotated cache lacked causal acknowledgement", len(observer.caches), observer.uncertain)
				}
				fresh := ports.WithNativePreparationExecutable(ports.WithNativeOperationObserver(context.Background(), observer), image)
				if err := New(fixture.socket).CheckOwnerImagePolicy(fresh, original, environment, fixture.root); err != nil {
					t.Fatal("fresh helper rejected original successor", err)
				}
				if err := New(fixture.socket).CheckOwnerImagePolicy(ports.WithNativePreparationExecutable(context.Background(), image), original, environment, fixture.root); !errors.Is(err, domain.ErrPlanStale) {
					t.Fatal("missing cache provenance adopted current token", err)
				}
				mu.Lock()
				acceptAccess = false
				mu.Unlock()
				if _, err := New(fixture.socket).ReadOwnerPolicyImageCatalog(ctx, original, environment, fixture.root); err == nil {
					t.Fatal("second metadata failure became success")
				}
				if len(observer.caches) != 2 {
					t.Fatal("second original cache successor missing", len(observer.caches))
				}
				// Producer hash order is unrelated to causal refresh order.
				slices.Reverse(observer.caches)
				if err := New(fixture.socket).CheckOwnerImagePolicy(fresh, original, environment, fixture.root); err != nil {
					t.Fatal("reverse producer order lost original two-step endpoint", err)
				}

			} else if len(observer.caches) != map[bool]int{true: 1, false: 0}[scenario == "withheld-ack"] || !observer.uncertain {
				t.Fatal("withheld cache ack gained closure", len(observer.caches), observer.uncertain)
			}
			_ = directory
		})
	}
}

func TestOwnerSourceActualOCIHelperOriginalEnvironmentAndRetainedImage(t *testing.T) {
	for _, fault := range []string{"original", "recreate", "helper-drift"} {
		t.Run(fault, func(t *testing.T) {
			image := sourceTestImage(t)
			observer := &sourceTestObserver{ownerCreateObserverFixture: &ownerCreateObserverFixture{}}
			client, target := ownerCreateClientFixture(t, observer.ownerCreateObserverFixture, "instance-success")
			target.oci = true
			original := ownerCreateInstanceOriginal(t, client, domain.YardContainer)
			home := os.Getenv("HOME")
			directory := filepath.Join(testkit.TempDir(t), "original-cwd-\xff")
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(home, "original-helper-observed")
			helper := `#!/usr/bin/python3
import os,json,pathlib,stat,sys
assert sys.argv[1:] == ['get']
assert os.fsencode(os.environ['AUTH_SENTINEL'])==b'original-provider-value-\xff'
assert os.getcwd()==os.environ['AUTH_EXPECTED_CWD']
s=os.fstat(4);assert stat.S_ISREG(s.st_mode) and stat.S_IMODE(s.st_mode)==0o600 and s.st_nlink==1
pathlib.Path(os.environ['AUTH_MARKER']).write_text('original env/cwd/OFD')
print(json.dumps(dict(Username='synthetic-user',Secret='synthetic-private-password')))
`
			testkit.WriteFile(t, filepath.Join(directory, "credentials"), []byte(helper), 0o700)
			if err := os.MkdirAll(os.Getenv("INCUS_CONF"), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(os.Getenv("INCUS_CONF"), "config.yml"), []byte("remotes:\n  local:\n    addr: unix:"+client.socket+"\n    protocol: incus\n  oci:\n    addr: https://registry.invalid\n    protocol: oci\n    public: true\n    credentials_helper: ./credentials\n"), 0o600)
			env := append(slicesWithoutEnvironment(os.Environ(), "INCUS_PROJECT"), "INCUS_PROJECT=original-project", "AUTH_SENTINEL=original-provider-value-\xff", "AUTH_EXPECTED_CWD="+directory, "AUTH_MARKER="+marker)
			prep := ports.WithNativeSourceDirectory(ports.WithNativePreparationExecutable(context.Background(), image), directory)
			policy, err := client.CaptureOwnerImagePolicy(prep, "local:missing", "oci:library/alpine:3.22", domain.YardContainer, env, home)
			if err != nil {
				t.Fatal("isolated original helper capture", err)
			}
			original.Policy = &policy
			if fault == "recreate" {
				original.Scope = "recreate"
			}
			original.Rendered = bytes.Replace(original.Rendered, []byte("local:debian/13"), policy.Fallback, 1)
			target.onInstancePost = func(request api.InstancesPost) {
				if !strings.Contains(request.Source.Server, "synthetic-private-password") || request.Source.Alias != "library/alpine:3.22" {
					t.Error("private helper result not used in exact leaf request")
				}
			}
			t.Setenv("AUTH_SENTINEL", "foreign-live-helper-value")
			if fault == "helper-drift" {
				testkit.WriteFile(t, filepath.Join(directory, "credentials"), []byte("#!/bin/sh\nexit 77\n"), 0o700)
			}
			ctx := sourceTestContext(t, home, image, observer)
			created, resolved, err := client.ApplyPreparedOwnerPolicyInstance(ctx, original, env, home)
			if fault == "helper-drift" {
				if err == nil || created != nil || target.posts != 0 {
					t.Fatal("drift reached target create", err, target.posts)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("changed helper ran")
				}
				return
			}
			if err != nil || created == nil || target.posts != 1 || observer.started != observer.closed || observer.uncertain {
				t.Fatal("exact private OCI leaf failed", err, target.posts, observer.started, observer.closed, observer.uncertain)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("original helper was not executed", err)
			}
			t.Setenv("INCUS_CONF", filepath.Join(home, "foreign-helper-config"))
			if err := client.CheckOwnerCreatedInstance(ctx, resolved, *created); err != nil {
				t.Fatal("postcreate adopted helper ambient env", err)
			}
			if err := client.SetOwnerCreatedPower(ctx, resolved, *created, "stopped", true); err != nil {
				t.Fatal("postcreate metadata lost original source", err)
			}
			if err := os.Chmod(filepath.Join(directory, "credentials"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := client.CheckOwnerCreatedInstance(ctx, resolved, *created); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("changed original source mode accepted", err)
			}
			raw, _ := json.Marshal(resolved)
			if bytes.Contains(raw, []byte("synthetic-private-password")) {
				t.Fatal("private credential escaped in public resolved record")
			}
		})
	}
}

func TestOwnerSourceActualDeviceFlowInteractionAndBrowserCustody(t *testing.T) {
	for _, mode := range []string{"original-opener", "manual-attempt-failure", "public-project"} {
		t.Run(mode, func(t *testing.T) {
			image := sourceTestImage(t)
			fixture := newOwnerImageFixture(t)
			directory := testkit.TempDir(t)
			marker := filepath.Join(fixture.root, "browser-observed")
			release := filepath.Join(fixture.root, "browser-release")
			browser := filepath.Join(directory, "browser")
			body := `#!/usr/bin/python3
import json,os,pathlib,time
assert len(__import__('sys').argv)==2
assert os.environ.get('SUBYARD_PRIVATE_OWNER_BROWSER') is None
assert os.environ['BROWSER']==os.environ['EXPECTED_BROWSER']
assert os.getcwd()==os.environ['EXPECTED_BROWSER_CWD']
fds=[]
for n in os.listdir('/proc/self/fd'):
 if int(n)<=2:continue
 try: fds.append(os.readlink('/proc/self/fd/'+n))
 except FileNotFoundError:pass
pathlib.Path(os.environ['BROWSER_MARKER']).write_text(json.dumps(dict(pid=os.getpid(),fds=fds)))
end=time.monotonic()+10
while not pathlib.Path(os.environ['BROWSER_RELEASE']).exists() and time.monotonic()<end:time.sleep(.01)
`
			testkit.WriteFile(t, browser, []byte(body), 0o700)
			if mode == "manual-attempt-failure" {
				browser = filepath.Join(directory, "missing-browser")
			}
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					_ = json.NewEncoder(w).Encode(map[string]any{"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token", "jwks_uri": server.URL + "/keys", "device_authorization_endpoint": server.URL + "/device", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
				case "/device":
					_, _ = io.WriteString(w, `{"device_code":"synthetic-device","user_code":"SYNTHETIC-CODE","verification_uri":"https://ui.invalid/device","verification_uri_complete":"https://ui.invalid/device?code=synthetic-device","expires_in":60,"interval":1}`)
				case "/token":
					_, _ = io.WriteString(w, `{"access_token":"device-access","refresh_token":"device-refresh","token_type":"Bearer","expires_in":3600}`)
				default:
					expectedProject := "original-project"
					if mode == "public-project" {
						expectedProject = ""
					}
					if r.URL.Query().Get("project") != expectedProject {
						t.Error("SDK original public/private project semantics changed")
						w.WriteHeader(400)
						return
					}
					if r.Header.Get("Authorization") != "Bearer device-access" {
						w.Header().Set("X-Incus-OIDC-issuer", server.URL)
						w.Header().Set("X-Incus-OIDC-clientid", "synthetic-client")
						w.WriteHeader(401)
						_, _ = io.WriteString(w, `{"type":"error","error_code":401,"error":"device required"}`)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "status_code": 200, "metadata": []api.Image{fixture.image}})
				}
			}))
			defer server.Close()
			if err := os.MkdirAll(filepath.Join(fixture.config, "servercerts"), 0o700); err != nil {
				t.Fatal(err)
			}
			certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			testkit.WriteFile(t, filepath.Join(fixture.config, "servercerts/source.crt"), certificate, 0o600)
			testkit.WriteFile(t, filepath.Join(fixture.root, "roots.pem"), certificate, 0o600)
			t.Setenv("SSL_CERT_FILE", filepath.Join(fixture.root, "roots.pem"))
			public := ""
			if mode == "public-project" {
				public = "    public: true\n"
			}
			testkit.WriteFile(t, filepath.Join(fixture.config, "config.yml"), []byte(fmt.Sprintf("remotes:\n  source:\n    addr: %q\n    protocol: incus\n    auth_type: oidc\n    project: original-project\n%s", server.URL, public)), 0o600)
			env := append(slicesWithoutEnvironment(fixture.environment(), "BROWSER"), "BROWSER="+browser, "EXPECTED_BROWSER="+browser, "EXPECTED_BROWSER_CWD="+directory, "BROWSER_MARKER="+marker, "BROWSER_RELEASE="+release)
			interaction, err := os.OpenFile(filepath.Join(fixture.root, "interaction"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer interaction.Close()
			if err := interaction.Chmod(0o600); err != nil {
				t.Fatal(err)
			}
			ctx := ports.WithNativeSourceDirectory(ports.WithNativeInteraction(ports.WithNativePreparationExecutable(context.Background(), image), interaction), directory)
			started := time.Now()
			policy, err := New(fixture.socket).CaptureOwnerImagePolicy(ctx, "source:debian/13", "local:fallback", domain.YardContainer, env, fixture.root)
			if err != nil {
				t.Fatal("device flow custody", err)
			}
			if len(policy.Cache) != 1 {
				t.Fatal("preparation did not return exact cache successor", len(policy.Cache))
			}
			content, err := os.ReadFile(interaction.Name())
			if err != nil || !bytes.Contains(content, []byte("SYNTHETIC-CODE")) || !bytes.Contains(content, []byte("https://ui.invalid/device")) {
				t.Fatal("device UI lost original interaction", err)
			}
			if mode != "manual-attempt-failure" {
				var observed struct {
					PID int
					FDs []string
				}
				end := time.Now().Add(3 * time.Second)
				for time.Now().Before(end) {
					if raw, err := os.ReadFile(marker); err == nil && json.Unmarshal(raw, &observed) == nil {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				if observed.PID == 0 || len(observed.FDs) != 0 {
					t.Fatal("detached opener inherited authority", observed.FDs)
				}
				if time.Since(started) > 5*time.Second {
					t.Fatal("SDK waited for independent browser")
				}
				testkit.WriteFile(t, release, nil, 0o600)
				for time.Now().Before(end) {
					var status syscall.WaitStatus
					pid, err := syscall.Wait4(observed.PID, &status, syscall.WNOHANG, nil)
					if err != nil || pid == observed.PID {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			if err := New(fixture.socket).CheckOwnerImagePolicy(ctx, policy, env, fixture.root); err != nil {
				t.Fatal("preparation successor capsule failed recheck", err)
			}
		})
	}
}

func TestOwnerSourceActualPrivateTLSCreateAndTokenClose(t *testing.T) {
	for _, mode := range []string{"instance-success", "instance-private-failure"} {
		t.Run(mode, func(t *testing.T) {
			image := sourceTestImage(t)
			client, target, _, original, base, _ := ownerPrivatePolicyFixture(t, mode, "")
			observer := &sourceTestObserver{ownerCreateObserverFixture: base}
			ctx := sourceTestContext(t, os.Getenv("HOME"), image, observer)
			environment := append(slicesWithoutEnvironment(os.Environ(), "INCUS_PROJECT"), "INCUS_PROJECT="+original.Project)
			created, resolved, err := client.ApplyPreparedOwnerPolicyInstance(ctx, original, environment, os.Getenv("HOME"))
			if mode == "instance-private-failure" {
				var failure *ports.NativeOperationFailure
				if created != nil || !errors.As(err, &failure) || failure.Operation.Kind != "owner-create-instance" {
					t.Fatal("known SDK operation failure lost", err)
				}
				return
			}
			if err != nil || created == nil || target.posts != 1 || observer.started != observer.closed || observer.uncertain {
				t.Fatal("private source/create/Close custody failed", err, target.posts, observer.started, observer.closed, observer.uncertain)
			}
			tokenClosed := false
			for _, op := range observer.terminal {
				if ownerImageSecretKind(op.Kind) && (op.Status == 200 || op.Status == 401) {
					tokenClosed = true
				}
			}
			if !tokenClosed {
				t.Fatal("private token cleanup terminal missing")
			}
			t.Setenv("INCUS_CONF", filepath.Join(os.Getenv("HOME"), "foreign-helper-config"))
			if err := client.CheckOwnerCreatedInstance(ctx, resolved, *created); err != nil {
				t.Fatal("original source guard adopted ambient provider", err)
			}
		})
	}
}

// This uses the real SDK credential-helper branch and real retained cmd/yard.
// Killing the leaf leaves its already-dispatched helper holding the original
// OFD. No observer terminal or target create may be inferred from that loss.
func TestOwnerSourceActualLeafLossKeepsOriginalOFDWithoutReplay(t *testing.T) {
	image := sourceTestImage(t)
	observer := &sourceTestObserver{ownerCreateObserverFixture: &ownerCreateObserverFixture{}}
	client, target := ownerCreateClientFixture(t, observer.ownerCreateObserverFixture, "instance-success")
	target.oci = true
	original := ownerCreateInstanceOriginal(t, client, domain.YardContainer)
	home := os.Getenv("HOME")
	directory := testkit.TempDir(t)
	marker := filepath.Join(home, "source-helper-dispatched")
	releasePath := filepath.Join(home, "source-helper-release")
	helper := `#!/usr/bin/python3
import json,os,pathlib,time
# Survive the parent's intentional cancellation of the leaf process group.
# Otherwise a blocked flock can merely observe this helper while it is dying.
os.setsid()
p=pathlib.Path(os.environ['SOURCE_LOSS_MARKER'])
start=pathlib.Path('/proc/self/stat').read_text().rsplit(')',1)[1].split()[19]
with p.open('a') as f:f.write(json.dumps(dict(pid=os.getpid(),leaf=os.getppid(),start=start))+'\n')
end=time.monotonic()+15
while not pathlib.Path(os.environ['SOURCE_LOSS_RELEASE']).exists() and time.monotonic()<end:time.sleep(.01)
print(json.dumps(dict(Username='synthetic',Secret='synthetic')))
`
	testkit.WriteFile(t, filepath.Join(directory, "credentials"), []byte(helper), 0o700)
	if err := os.MkdirAll(os.Getenv("INCUS_CONF"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(os.Getenv("INCUS_CONF"), "config.yml"), []byte("remotes:\n  local:\n    addr: unix:"+client.socket+"\n    protocol: incus\n  oci:\n    addr: https://registry.invalid\n    protocol: oci\n    public: true\n    credentials_helper: ./credentials\n"), 0o600)
	env := append(slicesWithoutEnvironment(os.Environ(), "INCUS_PROJECT"), "INCUS_PROJECT=original-project", "SOURCE_LOSS_MARKER="+marker, "SOURCE_LOSS_RELEASE="+releasePath)
	prep := ports.WithNativeSourceDirectory(ports.WithNativePreparationExecutable(context.Background(), image), directory)
	policy, err := client.CaptureOwnerImagePolicy(prep, "local:missing", "oci:library/alpine:3.22", domain.YardContainer, env, home)
	if err != nil {
		t.Fatal(err)
	}
	original.Policy = &policy
	original.Rendered = bytes.Replace(original.Rendered, []byte("local:debian/13"), policy.Fallback, 1)
	_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = ports.WithNativeSelfExecutable(ports.WithNativeOwnerHome(ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(ctx, observer), strings.Repeat("e", 64)), home), image)
	done := make(chan error, 1)
	go func() { _, _, err := client.ApplyPreparedOwnerPolicyInstance(ctx, original, env, home); done <- err }()
	var pid struct {
		PID, Leaf int
		Start     string
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, e := os.ReadFile(marker); e == nil && json.Unmarshal(bytes.TrimSpace(b), &pid) == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid.PID <= 1 || pid.Leaf <= 1 || pid.Start == "" {
		cancel()
		<-done
		t.Fatal("original SDK helper not dispatched")
	}
	// Match the exact helper after leaf loss too; a reaped or killed helper
	// proves cleanup, not custody of a surviving descendant.
	helperAlive := func() (bool, error) {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid.PID))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		end := strings.LastIndex(string(raw), ") ")
		if end < 0 {
			return false, errors.New("original helper process identity unavailable")
		}
		values := strings.Fields(string(raw)[end+2:])
		if len(values) <= 19 || values[19] != pid.Start || values[2] != strconv.Itoa(pid.PID) || values[3] != strconv.Itoa(pid.PID) {
			return false, errors.New("original helper process identity changed")
		}
		if values[0] == "Z" || values[0] == "X" {
			return false, nil
		}
		cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid.PID))
		if err != nil || cwd != directory {
			return false, errors.New("original helper working directory changed")
		}
		return true, nil
	}
	drainHelper := func() {
		cancel()
		if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
			t.Error("release original helper", err)
		}
		cleanupDeadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(cleanupDeadline) {
			alive, err := helperAlive()
			if err != nil {
				t.Error(err)
				return
			}
			if !alive {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Signal only the still-matching helper if its explicit release failed.
		if alive, err := helperAlive(); err == nil && alive {
			_ = syscall.Kill(pid.PID, syscall.SIGKILL)
			killDeadline := time.Now().Add(time.Second)
			for time.Now().Before(killDeadline) {
				if alive, err := helperAlive(); err == nil && !alive {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		t.Error("original SDK helper did not drain " + strconv.Itoa(pid.PID))
	}
	defer drainHelper()
	childFD, err := os.Stat(fmt.Sprintf("/proc/%d/fd/4", pid.PID))
	if err != nil {
		t.Fatal(err)
	}
	originalFD, err := os.Stat(uninstall.OwnerLockPath(home))
	if err != nil || !os.SameFile(childFD, originalFD) {
		t.Fatal("SDK helper lost original OFD", err)
	}
	args, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid.Leaf))
	if err != nil || !bytes.Contains(args, []byte("_owner-source-v1\x00apply")) {
		t.Fatal("foreign leaf", err)
	}
	if err := syscall.Kill(pid.Leaf, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("lost source leaf succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("lost source leaf not bounded")
	}
	release()
	if alive, err := helperAlive(); err != nil || !alive {
		t.Fatal("original SDK helper did not survive leaf/group cleanup", err)
	}
	childFD, err = os.Stat(fmt.Sprintf("/proc/%d/fd/4", pid.PID))
	if err != nil || !os.SameFile(childFD, originalFD) {
		t.Fatal("surviving SDK helper lost original descriptor", err)
	}
	contender, err := os.OpenFile(uninstall.OwnerLockPath(home), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()
	if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("surviving SDK helper lost original OFD")
	}
	if !observer.uncertain || observer.closed != 0 || target.posts != 0 {
		t.Fatal("loss gained authority/terminal", observer.uncertain, observer.closed, target.posts)
	}
	b, err := os.ReadFile(marker)
	if err != nil || bytes.Count(b, []byte("\n")) != 1 {
		t.Fatal("helper replayed", err)
	}
	testkit.WriteFile(t, releasePath, nil, 0o600)
	cleanupDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(cleanupDeadline) {
		if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("original SDK helper did not drain " + strconv.Itoa(pid.PID))
}

func TestOwnerSourceActualPendingTokenDrainUsesOnlyOriginalOperation(t *testing.T) {
	for _, fault := range []string{"original", "foreign-id"} {
		t.Run(fault, func(t *testing.T) {
			image := sourceTestImage(t)
			client, target, source, original, _, _ := ownerPrivatePolicyFixture(t, "instance-success", "")
			env := append(slicesWithoutEnvironment(os.Environ(), "INCUS_PROJECT"), "INCUS_PROJECT="+original.Project)
			connection, err := client.ownerPolicyImageServer(context.Background(), *original.Policy, string(original.Policy.Primary), env, os.Getenv("HOME"))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.close()
			expected, err := nativeOperation(connection.ImageServer.(incus.InstanceServer), "owner-image-secret-current-primary", original.Project, strings.Repeat("a", 64))
			if err != nil {
				t.Fatal(err)
			}
			expected.ID = source.op.ID
			if fault == "foreign-id" {
				expected.ID = "99999999-2222-4333-8444-555555555555"
			}
			_, release, err := uninstall.OwnerLock(context.Background(), os.Getenv("HOME"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ctx = ports.WithNativeSelfExecutable(ports.WithNativeOwnerHome(ctx, os.Getenv("HOME")), image)
			result, err := client.DrainOwnerImageSecretOperation(ctx, *original.Policy, string(original.Policy.Primary), env, os.Getenv("HOME"), expected)
			if fault != "original" {
				if err == nil || source.deletes != 0 || source.posts != 0 || target.posts != 0 {
					t.Fatal("foreign operation gained drain", err)
				}
				return
			}
			if err != nil || result.ID != expected.ID || result.Status != 401 || source.posts != 0 || source.deletes != 1 || target.posts != 0 {
				t.Fatal("exact pending drain lost custody", err, result.Status, source.posts, source.deletes, target.posts)
			}
		})
	}
}

func TestOwnerSourceParentPollRetriesInterruptedSyscall(t *testing.T) {
	done := make(chan struct{})
	calls := 0
	lost := ownerSourceParentLost(done, func(f []unix.PollFd, _ int) (int, error) {
		calls++
		if calls == 1 {
			return -1, unix.EINTR
		}
		f[0].Revents = unix.POLLRDHUP
		return 1, nil
	})
	if !lost || calls != 2 {
		t.Fatal("interrupted poll inferred loss", lost, calls)
	}
	close(done)
	if ownerSourceParentLost(done, func([]unix.PollFd, int) (int, error) { t.Fatal("completed session polled"); return 0, nil }) {
		t.Fatal("normal close inferred loss")
	}
}
func TestOwnerImageCacheOriginalCausalChainIgnoresProducerOrder(t *testing.T) {
	path := []byte("/original/oidctokens/source.json")
	a := ports.OwnerImageSourceGuard{Path: path, State: "file:" + strings.Repeat("a", 64)}
	b, c := a, a
	b.State = "file:" + strings.Repeat("b", 64)
	c.State = "file:" + strings.Repeat("c", 64)
	p := ports.PreparedOwnerImagePolicy{Version: 1, CacheBinding: strings.Repeat("a", 64), Primary: []byte("source:original"), Fallback: []byte("source:fallback"), Kind: "container", TokenPaths: [][]byte{path}, Guards: []ports.OwnerImageSourceGuard{a}}
	fact := func(before, after ports.OwnerImageSourceGuard) ports.OwnerImageCacheSuccessor {
		return ports.OwnerImageCacheSuccessor{Policy: ownerImagePolicyKey(p, path), Before: []ports.OwnerImageSourceGuard{before}, After: []ports.OwnerImageSourceGuard{after}}
	}
	for _, scenario := range []string{"reversed", "repeat-noop", "fork", "gap", "cycle"} {
		t.Run(scenario, func(t *testing.T) {
			observer := &sourceTestObserver{ownerCreateObserverFixture: &ownerCreateObserverFixture{}}
			observer.caches = []ports.OwnerImageCacheSuccessor{fact(b, c), fact(a, b)}
			switch scenario {
			case "repeat-noop":
				observer.caches = append(observer.caches, fact(a, b), fact(b, b))
			case "fork":
				observer.caches = append(observer.caches, fact(a, c))
			case "gap":
				observer.caches = []ports.OwnerImageCacheSuccessor{fact(b, c)}
			case "cycle":
				observer.caches = append(observer.caches, fact(c, a))
			}
			guards, err := ownerImageCacheGuards(ports.WithNativeOwnerImageCache(context.Background(), observer), p)
			if scenario == "reversed" || scenario == "repeat-noop" {
				if err != nil || len(guards) != 1 || !reflect.DeepEqual(guards[0], c) {
					t.Fatal("original causal chain lost", err)
				}
			} else if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("foreign causal chain accepted", err)
			}
		})
	}
}

func TestOwnerSourceActualOIDCPrivateCreateRefreshesDuringTokenClose(t *testing.T) {
	for _, mode := range []string{"acknowledged", "withheld-ack", "secret-create-failure"} {
		t.Run(mode, func(t *testing.T) {
			image := sourceTestImage(t)
			observer := &sourceTestObserver{ownerCreateObserverFixture: &ownerCreateObserverFixture{}, denyCache: mode == "withheld-ack", persistBeforeDeny: mode == "withheld-ack"}
			client, target := ownerCreateClientFixture(t, observer.ownerCreateObserverFixture, "instance-success")
			original := ownerCreateInstanceOriginal(t, client, domain.YardContainer)
			source, args, _, _, _ := newOwnerSecretFixtureTransport(t, false)
			source.project = original.Project
			var originalAccepted atomic.Bool
			originalAccepted.Store(true)
			var refreshes atomic.Int32
			source.authorize = func(w http.ResponseWriter, r *http.Request) bool {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					_ = json.NewEncoder(w).Encode(map[string]any{"issuer": source.server.URL, "authorization_endpoint": source.server.URL + "/authorize", "token_endpoint": source.server.URL + "/token", "jwks_uri": source.server.URL + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
					return false
				case "/token":
					if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "original-refresh" {
						t.Error("cleanup auth adopted foreign refresh")
						w.WriteHeader(400)
						return false
					}
					refreshes.Add(1)
					_, _ = io.WriteString(w, `{"access_token":"cleanup-access","refresh_token":"cleanup-rotated","token_type":"Bearer","expires_in":3600}`)
					return false
				}
				auth := r.Header.Get("Authorization")
				if mode == "secret-create-failure" && strings.HasSuffix(r.URL.Path, "/secret") {
					originalAccepted.Store(false)
					if auth == "Bearer cleanup-access" {
						w.WriteHeader(500)
						_, _ = io.WriteString(w, `{"type":"error","error_code":500,"error":"synthetic source secret failure"}`)
						return false
					}
				}
				if auth == "Bearer cleanup-access" || originalAccepted.Load() && auth == "Bearer original-access" {
					return true
				}
				w.Header().Set("X-Incus-OIDC-issuer", source.server.URL)
				w.Header().Set("X-Incus-OIDC-clientid", "synthetic-client")
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"type":"error","error_code":401,"error":"cleanup refresh required"}`)
				return false
			}
			home, config := os.Getenv("HOME"), os.Getenv("INCUS_CONF")
			for _, path := range []string{config, filepath.Join(config, "servercerts"), filepath.Join(config, "oidctokens")} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, filepath.Join(config, "servercerts/source.crt"), []byte(args.TLSServerCert), 0o600)
			roots := filepath.Join(home, "source-roots.pem")
			testkit.WriteFile(t, roots, []byte(args.TLSServerCert), 0o600)
			t.Setenv("SSL_CERT_FILE", roots)
			testkit.WriteFile(t, filepath.Join(config, "config.yml"), []byte(fmt.Sprintf("remotes:\n  source:\n    addr: %q\n    protocol: incus\n    auth_type: oidc\n    project: ignored-original-project\n", source.server.URL)), 0o600)
			tokens := oidc.Tokens[*oidc.IDTokenClaims]{Token: &oauth2.Token{AccessToken: "original-access", RefreshToken: "original-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}}
			raw, _ := json.Marshal(&tokens)
			tokenPath := filepath.Join(config, "oidctokens/source.json")
			testkit.WriteFile(t, tokenPath, raw, 0o600)
			env := append(slicesWithoutEnvironment(os.Environ(), "INCUS_PROJECT"), "INCUS_PROJECT="+original.Project)
			policy, err := client.CaptureOwnerImagePolicy(context.Background(), "source:debian/13", "local:fallback", domain.YardContainer, env, home)
			if err != nil {
				t.Fatal(err)
			}
			original.Policy = &policy
			original.Rendered = bytes.Replace(original.Rendered, []byte("local:debian/13"), policy.Primary, 1)
			target.onInstancePost = func(api.InstancesPost) {
				select {
				case <-source.armed:
				default:
					t.Error("target preceded original token wait")
				}
				originalAccepted.Store(false)
			}
			ctx := sourceTestContext(t, home, image, observer)
			created, _, err := client.ApplyPreparedOwnerPolicyInstance(ctx, original, env, home)
			expectedPosts := 1
			if mode == "secret-create-failure" {
				expectedPosts = 0
			}
			if refreshes.Load() != 1 || target.posts != expectedPosts || source.posts != expectedPosts || source.deletes != expectedPosts {
				t.Fatal("cleanup auth replayed or escaped exact effect", refreshes.Load(), target.posts, source.posts, source.deletes)
			}
			saved, readErr := os.ReadFile(tokenPath)
			if readErr != nil || !bytes.Contains(saved, []byte("cleanup-rotated")) || ownerImageCacheFile(tokenPath) != nil || len(observer.caches) != 1 {
				t.Fatal("cleanup rotated pointer/cache fact lost", readErr, len(observer.caches))
			}
			if mode == "withheld-ack" || mode == "secret-create-failure" {
				if err == nil || created != nil || !observer.uncertain {
					t.Fatal("lost cleanup cache ack gained closure", err)
				}
				return
			}
			if err != nil || created == nil || observer.uncertain || observer.started != observer.closed {
				t.Fatal("exact OIDC tokenClose custody", err)
			}
		})
	}
}

func TestOwnerSourceBrowserTruncatedDirectChildIsReaped(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "socket")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	child := exec.Command("/usr/bin/python3", "-c", "import socket,sys;s=socket.socket(socket.AF_UNIX);s.connect(sys.argv[1]);s.close()", path)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Release()
	reaped := false
	defer func() {
		if !reaped {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	conn, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := (&ownerSourceLeaf{}).browserAttempt(ctx, conn); err == nil {
		t.Fatal("truncated URI gained acknowledgement")
	}
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(child.Process.Pid, &status, syscall.WNOHANG, nil); err != syscall.ECHILD {
		t.Fatal("direct SDK bridge was not reaped", err)
	}
	reaped = true
}

func TestOwnerSourceActualSharedCacheAcrossOriginalCohort(t *testing.T) {
	image := sourceTestImage(t)
	fixture := newOwnerImageFixture(t)
	directory := testkit.TempDir(t)
	var server *httptest.Server
	var lock sync.Mutex
	stage := 0
	requireRefresh := true
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token", "jwks_uri": server.URL + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/token":
			lock.Lock()
			defer lock.Unlock()
			expected := "initial-refresh"
			if stage > 0 {
				expected = fmt.Sprint("refresh-", stage)
			}
			if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != expected {
				t.Error("cohort adopted a foreign refresh")
				w.WriteHeader(400)
				return
			}
			stage++
			requireRefresh = false
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprint("access-", stage), "refresh_token": fmt.Sprint("refresh-", stage), "token_type": "Bearer", "expires_in": 3600})
		default:
			lock.Lock()
			refuse := requireRefresh || r.Header.Get("Authorization") != fmt.Sprint("Bearer access-", stage)
			lock.Unlock()
			if refuse {
				w.Header().Set("X-Incus-OIDC-issuer", server.URL)
				w.Header().Set("X-Incus-OIDC-clientid", "synthetic-client")
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"type":"error","error_code":401,"error":"refresh required"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "status_code": 200, "metadata": []api.Image{fixture.image}})
		}
	}))
	defer server.Close()
	for _, path := range []string{fixture.config, filepath.Join(fixture.config, "servercerts"), filepath.Join(fixture.config, "oidctokens")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	roots := filepath.Join(fixture.root, "roots.pem")
	testkit.WriteFile(t, roots, certificate, 0o600)
	t.Setenv("SSL_CERT_FILE", roots)
	testkit.WriteFile(t, filepath.Join(fixture.config, "servercerts/source.crt"), certificate, 0o600)
	testkit.WriteFile(t, filepath.Join(fixture.config, "config.yml"), []byte(fmt.Sprintf("remotes:\n  source:\n    addr: %q\n    protocol: incus\n    auth_type: oidc\n", server.URL)), 0o600)
	initial := oidc.Tokens[*oidc.IDTokenClaims]{Token: &oauth2.Token{AccessToken: "expired", RefreshToken: "initial-refresh", TokenType: "Bearer"}}
	raw, _ := json.Marshal(&initial)
	testkit.WriteFile(t, filepath.Join(fixture.config, "oidctokens/source.json"), raw, 0o600)
	prep := ports.WithNativeSourceDirectory(ports.WithNativePreparationExecutable(context.Background(), image), directory)
	client := New(fixture.socket)
	var policies []*ports.PreparedOwnerImagePolicy
	var environments [][]string
	for _, role := range []string{"current", "legacy", "recreate"} {
		lock.Lock()
		requireRefresh = true
		lock.Unlock()
		env := append(slicesWithoutEnvironment(fixture.environment(), "INCUS_PROJECT"), "INCUS_PROJECT=original-"+role)
		p, err := client.CaptureOwnerImagePolicy(prep, "source:image-"+role, "local:fallback", domain.YardContainer, env, fixture.root)
		if err != nil {
			t.Fatal("original cohort preparation", role, err)
		}
		if len(p.Cache) != 1 {
			t.Fatal("preparation successor missing", role, len(p.Cache))
		}
		policies = append(policies, &p)
		environments = append(environments, env)
	}
	if policies[0].CacheBinding != policies[1].CacheBinding || policies[1].CacheBinding != policies[2].CacheBinding {
		t.Fatal("image/project/tokenbefore changed original endpoint identity")
	}
	if err := client.BindOwnerImagePreparationCache(prep, policies); err != nil {
		t.Fatal("original cohort finalization", err)
	}
	for i, p := range policies {
		if len(p.Cache) != 3 {
			t.Fatal("original preparation facts lost", i, len(p.Cache))
		}
		if err := client.CheckOwnerImagePolicy(prep, *p, environments[i], fixture.root); err != nil {
			t.Fatal("variant before not causally joined", i, err)
		}
	}
	observer := &sourceTestObserver{ownerCreateObserverFixture: &ownerCreateObserverFixture{}}
	admitted := sourceTestContext(t, fixture.root, image, observer)
	lock.Lock()
	requireRefresh = true
	lock.Unlock()
	if _, err := client.ReadOwnerPolicyImageCatalog(admitted, *policies[0], environments[0], fixture.root); err != nil {
		t.Fatal("original admitted refresh", err)
	}
	lock.Lock()
	requireRefresh = true
	lock.Unlock()
	if _, err := client.ReadOwnerPolicyImageCatalog(admitted, *policies[1], environments[1], fixture.root); err != nil {
		t.Fatal("legacy original shared endpoint successor", err)
	}
	fresh := ports.WithNativeOwnerImageCache(prep, observer)
	for i, p := range policies {
		if err := client.CheckOwnerImagePolicy(fresh, *p, environments[i], fixture.root); err != nil {
			t.Fatal("fresh original cohort lost admitted successor", i, err)
		}
	}
	if len(observer.caches) != 2 {
		t.Fatal("admitted successor replayed", len(observer.caches))
	}
	if output := os.Getenv("SUBYARD_NATIVE_IMAGE_ENDPOINT_FACTS"); output != "" {
		raw, err := json.Marshal(observer.caches)
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, output, raw, 0o600)
	}
	for _, fault := range []string{"config", "trust", "env", "cwd", "tokenpath", "cohort"} {
		t.Run(fault, func(t *testing.T) {
			p := *policies[1]
			p.Guards = slices.Clone(p.Guards)
			env := slices.Clone(environments[1])
			ctx := fresh
			switch fault {
			case "config", "trust":
				target := filepath.Join(fixture.config, "config.yml")
				if fault == "trust" {
					target = roots
				}
				index := slices.IndexFunc(p.Guards, func(g ports.OwnerImageSourceGuard) bool { return string(g.Path) == target })
				if index < 0 {
					t.Fatal("original guard missing")
				}
				p.Guards[index].State = "file:" + strings.Repeat("f", 64)
			case "env":
				env = append(slicesWithoutEnvironment(env, "INCUS_CONF"), "INCUS_CONF=/foreign")
			case "cwd":
				p.Directory = []byte(filepath.Join(fixture.root, "missing-original-cwd"))
			case "tokenpath":
				p.TokenPaths = [][]byte{[]byte(filepath.Join(fixture.root, "foreign-token"))}
			case "cohort":
				ctx = ports.WithNativeOwnerImageCache(prep, &sourceTestObserver{ownerCreateObserverFixture: &ownerCreateObserverFixture{}})
			}
			if err := client.CheckOwnerImagePolicy(ctx, p, env, fixture.root); err == nil {
				t.Fatal("foreign original cohort accepted")
			}
		})
	}
}
