package incusclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/cliconfig"
	"github.com/lxc/incus/v6/shared/proxy"
)

const ownerImageLimit = 32 << 20

// These are the environment inputs read by cliconfig, the Unix connector,
// net/http proxy selection and the Linux system certificate loader. The project
// is an explicit original CLI override, represented separately in the record.
var ownerImageEnvironmentKeys = []string{
	"HOME", "INCUS_CONF", "INCUS_GLOBAL_CONF", "INCUS_REMOTE", "INCUS_SOCKET", "INCUS_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"REQUEST_METHOD", "SSL_CERT_FILE", "SSL_CERT_DIR", "INCUS_INSECURE_TLS",
}

func ownerImageEnvironment(environment []string) (map[string]string, string, error) {
	values := map[string]string{}
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || strings.IndexByte(value, 0) >= 0 {
			return nil, "", domain.ErrPlanStale
		}
		if _, duplicate := values[key]; duplicate {
			return nil, "", domain.ErrPlanStale
		}
		values[key] = value
	}
	projection := make([][]byte, 0, len(ownerImageEnvironmentKeys))
	for _, key := range ownerImageEnvironmentKeys {
		projection = append(projection, []byte(key+"="+values[key]))
	}
	return values, ownerCreateDigest(projection), nil
}

func ownerImageEnvironmentCurrent(expected string) error {
	_, actual, err := ownerImageEnvironment(os.Environ())
	if err != nil || expected == "" || actual != expected {
		return domain.ErrPlanStale
	}
	return nil
}

// File bytes are hashed, never retained in an approved record. Lstat and Stat
// bind both a symlink and its target; ancestor identities catch link replacement.
func ownerImagePathState(path string, content bool) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", errors.New("original image source guard unavailable")
	}
	state := []any{ownerImageFileIdentity(info, content)}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(path)
		if err != nil {
			return "", errors.New("original image source link unavailable")
		}
		state = append(state, []byte(link))
		target, targetErr := os.Stat(path)
		err = targetErr
		if errors.Is(err, os.ErrNotExist) {
			state = append(state, "target-absent")
		} else if err != nil {
			return "", errors.New("original image source target unavailable")
		} else {
			state = append(state, ownerImageFileIdentity(target, content))
			info = target
		}
	}
	if content && err == nil {
		if !info.Mode().IsRegular() {
			return "", errors.New("original image source guard is not a regular file")
		}
		file, err := os.Open(path)
		if err != nil {
			return "", errors.New("original image source guard unreadable")
		}
		hash := sha256.New()
		n, readErr := io.Copy(hash, io.LimitReader(file, ownerImageLimit+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || n > ownerImageLimit {
			return "", errors.New("original image source guard exceeds read boundary")
		}
		state = append(state, hex.EncodeToString(hash.Sum(nil)))
	}
	return ownerCreateDigest(state), nil
}

func ownerImageFileIdentity(info os.FileInfo, content bool) any {
	stat := info.Sys().(*syscall.Stat_t)
	identity := []any{stat.Dev, stat.Ino, stat.Uid, stat.Gid, stat.Mode}
	if content {
		// Atime changes when guards themselves read the file. Directory times
		// change for unrelated entries; neither describes the admitted bytes.
		identity = append(identity, stat.Size, stat.Mtim, stat.Ctim)
	}
	return identity
}

func ownerImageGuardPaths(paths []string) ([]ports.OwnerImageSourceGuard, error) {
	var guards []ports.OwnerImageSourceGuard
	seen := map[string]bool{}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, errors.New("original image source requires absolute paths")
		}
		for current := filepath.Clean(path); ; current = filepath.Dir(current) {
			if !seen[current] {
				state, err := ownerImagePathState(current, current == filepath.Clean(path))
				if err != nil {
					return nil, err
				}
				// The prefix records whether contents or traversal identity was bound.
				prefix := "directory:"
				if current == filepath.Clean(path) {
					prefix = "file:"
				}
				guards = append(guards, ports.OwnerImageSourceGuard{Path: []byte(current), State: prefix + state})
				seen[current] = true
			}
			if filepath.Dir(current) == current {
				break
			}
		}
	}
	return guards, nil
}

func ownerImageCheckGuards(environment string, guards []ports.OwnerImageSourceGuard) error {
	if err := ownerImageEnvironmentCurrent(environment); err != nil || len(guards) == 0 {
		return domain.ErrPlanStale
	}
	for _, guard := range guards {
		prefix, expected, ok := strings.Cut(guard.State, ":")
		if !ok || !slices.Contains([]string{"file", "directory", "trust-directory"}, prefix) || !filepath.IsAbs(string(guard.Path)) {
			return domain.ErrPlanStale
		}
		state, err := ownerImagePathState(string(guard.Path), prefix == "file")
		if prefix == "trust-directory" {
			state, err = ownerImageTrustDirectory(string(guard.Path))
		}
		if err != nil || state != expected {
			return domain.ErrPlanStale
		}
	}
	return ownerImageEnvironmentCurrent(environment)
}

func ownerImageTrustDirectory(path string) (string, error) {
	identity, err := ownerImagePathState(path, false)
	if err != nil || identity == "absent" {
		return identity, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", errors.New("original image trust directory unavailable")
	}
	state := []any{identity}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		entryState, err := ownerImagePathState(filepath.Join(path, entry.Name()), true)
		if err != nil {
			return "", err
		}
		state = append(state, []byte(entry.Name()), entryState)
	}
	return ownerCreateDigest(state), nil
}

func ownerImageTrustGuards(values map[string]string) ([]ports.OwnerImageSourceGuard, error) {
	// Match the owning Linux Go toolchain's x509 fallback paths. Guard all
	// fallback absences as well as present roots; a newly created file matters.
	files := []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem", "/etc/pki/tls/cacert.pem", "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", "/etc/ssl/cert.pem"}
	if values["SSL_CERT_FILE"] != "" {
		files = []string{values["SSL_CERT_FILE"]}
	}
	guards, err := ownerImageGuardPaths(files)
	if err != nil {
		return nil, err
	}
	directories := []string{"/etc/ssl/certs", "/etc/pki/tls/certs"}
	if values["SSL_CERT_DIR"] != "" {
		directories = strings.Split(values["SSL_CERT_DIR"], ":")
	}
	for _, directory := range directories {
		if !filepath.IsAbs(directory) {
			return nil, errors.New("original image trust requires absolute paths")
		}
		state, err := ownerImageTrustDirectory(directory)
		if err != nil {
			return nil, err
		}
		guards = append(guards, ports.OwnerImageSourceGuard{Path: []byte(directory), State: "trust-directory:" + state})
	}
	return guards, nil
}

type ownerImageTransport struct {
	base http.RoundTripper
	ctx  context.Context
}

func (transport ownerImageTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request.Clone(transport.ctx))
	if err == nil && response.Body != nil {
		response.Body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(response.Body, ownerImageLimit+1), response.Body}
	}
	return response, err
}

func (transport ownerImageTransport) Transport() *http.Transport {
	value, _ := transport.base.(*http.Transport)
	return value
}

func (transport ownerImageTransport) CloseIdleConnections() {
	if closer, ok := transport.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func ownerImageBoundServer(ctx context.Context, server incus.ImageServer) error {
	httpClient, err := server.GetHTTPClient()
	if err != nil {
		return err
	}
	httpClient.Timeout = 30 * time.Second
	if _, alreadyBound := httpClient.Transport.(ownerImageTransport); alreadyBound {
		return nil
	}
	httpClient.Transport = ownerImageTransport{base: httpClient.Transport, ctx: ctx}
	return nil
}

func ownerImagePublicEndpoint(address string) (*url.URL, error) {
	original, err := url.Parse(address)
	if err != nil || original.Scheme != "https" || original.Host == "" || original.User != nil || original.RawQuery != "" || original.ForceQuery || original.Fragment != "" || original.RawFragment != "" || strings.Contains(address, "#") {
		return nil, domain.ErrPlanStale
	}
	return original, nil
}

func ownerImageProxy(transport *http.Transport) {
	// The SDK proxy helper caches environment on first use. Use its explicit
	// config API for this original connection so another invocation's cache
	// cannot choose its route. A nonempty comma means an empty exclusion list.
	first := func(upper, lower string) string {
		if value := os.Getenv(upper); value != "" {
			return value
		}
		return os.Getenv(lower)
	}
	https, httpProxy, noProxy := first("HTTPS_PROXY", "https_proxy"), first("HTTP_PROXY", "http_proxy"), first("NO_PROXY", "no_proxy")
	if noProxy == "" {
		noProxy = ","
	}
	transport.Proxy = func(request *http.Request) (*url.URL, error) {
		if request.URL.Scheme == "https" && https == "" || request.URL.Scheme == "http" && httpProxy == "" {
			return nil, nil
		}
		return proxy.FromConfig(https, httpProxy, noProxy)(request)
	}
}

func ownerImageKind(kind domain.YardKind) (string, error) {
	switch kind {
	case domain.YardContainer:
		return "container", nil
	case domain.YardVM:
		return "virtual-machine", nil
	default:
		return "", domain.ErrPlanStale
	}
}

func ownerImageConfigPaths(values map[string]string, operatorHome string) ([]string, error) {
	configDir := values["INCUS_CONF"]
	if configDir == "" {
		if values["HOME"] == "" || values["HOME"] != operatorHome {
			return nil, errors.New("original image source home fallback remains required")
		}
		configDir = filepath.Join(operatorHome, ".config", "incus")
	}
	if strings.Contains(configDir, "$") {
		return nil, errors.New("expanded original image configuration paths remain required")
	}
	globalDir := values["INCUS_GLOBAL_CONF"]
	if globalDir == "" {
		globalDir = "/etc/incus"
	}
	return []string{filepath.Join(configDir, "config.yml"), filepath.Join(globalDir, "config.yml")}, nil
}

func ownerImagePolicyEnvironment(environment []string) (map[string]string, string, string, error) {
	values, ambient, err := ownerImageEnvironment(environment)
	if err != nil {
		return nil, "", "", err
	}
	keys := append(slices.Clone(ownerImageEnvironmentKeys), "INCUS_PROJECT", "PATH", "PWD")
	slices.Sort(keys)
	projection := make([][]byte, 0, len(keys))
	for _, key := range keys {
		projection = append(projection, []byte(key+"="+values[key]))
	}
	return values, ownerCreateDigest(projection), ambient, nil
}

func ownerImagePolicyReference(reference []byte) bool {
	if len(reference) == 0 || strings.IndexByte(string(reference), 0) >= 0 {
		return false
	}
	// Component inputs are remote/object references. A credential URL belongs
	// only to its guarded configuration source, never the public policy record.
	parsed, err := url.Parse(string(reference))
	return err == nil && !(parsed.IsAbs() && parsed.Host != "")
}

// CaptureOwnerImagePolicy reads only original filesystem inputs. It deliberately
// does not connect, select an image, parse a private key, invoke a helper, refresh
// credentials or issue an image token. The owning runtime binds its renderer and
// full semantic environment; this record binds the SDK's relevant source inputs.
func (client *Client) CaptureOwnerImagePolicy(ctx context.Context, primary, fallback string, kind domain.YardKind, environment []string, operatorHome string) (ports.PreparedOwnerImagePolicy, error) {
	p := ports.PreparedOwnerImagePolicy{}
	if !ownerImagePolicyReference([]byte(primary)) || !ownerImagePolicyReference([]byte(fallback)) {
		return p, domain.ErrPlanStale
	}
	values, digest, ambient, err := ownerImagePolicyEnvironment(environment)
	if err != nil || ownerImageEnvironmentCurrent(ambient) != nil {
		return p, errors.New("original image policy environment differs; exact environment support remains required")
	}
	imageKind, err := ownerImageKind(kind)
	if err != nil {
		return p, err
	}
	paths, err := ownerImageConfigPaths(values, operatorHome)
	if err != nil {
		return p, err
	}
	if err := ctx.Err(); err != nil {
		return p, context.Cause(ctx)
	}
	before, err := ownerImageGuardPaths(paths)
	if err != nil {
		return p, err
	}
	config, err := cliconfig.LoadConfig(paths[0])
	if err != nil {
		return p, errors.New("original image policy configuration unavailable")
	}
	needsTrust := false
	for _, reference := range []string{primary, fallback} {
		name, _, err := config.ParseRemote(reference)
		if err != nil {
			// An unavailable fallback need not be selected. Its absence remains
			// bound by the complete original configuration-file guards.
			continue
		}
		remote, exists := config.Remotes[name]
		if !exists {
			continue
		}
		if remote.AuthType == api.AuthenticationMethodOIDC {
			paths = append(paths, config.OIDCTokenPath(name))
		}
		if strings.HasPrefix(remote.Addr, "unix:") {
			continue
		}
		needsTrust = true
		paths = append(paths, config.ServerCertPath(name))
		if remote.Protocol == "incus" && remote.AuthType != api.AuthenticationMethodOIDC {
			// Both the remote-specific pair and the global fallback participate
			// in the official certificate-path choice, including absent files.
			for _, suffix := range []string{"crt", "key", "ca"} {
				paths = append(paths, config.ConfigPath("clientcerts", name+"."+suffix), config.ConfigPath("client."+suffix))
			}
		}
		if remote.Protocol == "oci" && filepath.IsAbs(remote.CredHelper) {
			paths = append(paths, remote.CredHelper)
		}
		// Relative/named helper executable resolution needs the original
		// component working directory. Its config and PATH are bound here;
		// executable custody remains with the accepted owning component.
	}
	guards, err := ownerImageGuardPaths(paths)
	if err != nil {
		return p, err
	}
	if needsTrust {
		trust, err := ownerImageTrustGuards(values)
		if err != nil {
			return p, err
		}
		guards = append(guards, trust...)
	}
	if err := ownerImageCheckGuards(ambient, before); err != nil {
		return p, err
	}
	p = ports.PreparedOwnerImagePolicy{Version: 1, Primary: []byte(primary), Fallback: []byte(fallback), Kind: imageKind, Environment: digest, Project: []byte(values["INCUS_PROJECT"]), Guards: guards}
	if err := client.CheckOwnerImagePolicy(ctx, p, environment, operatorHome); err != nil {
		return ports.PreparedOwnerImagePolicy{}, err
	}
	return p, nil
}

// CheckOwnerImagePolicy checks only the captured input envelope. Image selection
// belongs to the original renderer after parent acceptance; a selected reference
// must equal Primary or Fallback, and its kind must equal this record's Kind.
func (client *Client) CheckOwnerImagePolicy(ctx context.Context, p ports.PreparedOwnerImagePolicy, environment []string, operatorHome string) error {
	if p.Version != 1 || !ownerImagePolicyReference(p.Primary) || !ownerImagePolicyReference(p.Fallback) || !slices.Contains([]string{"container", "virtual-machine"}, p.Kind) {
		return domain.ErrPlanStale
	}
	values, digest, ambient, err := ownerImagePolicyEnvironment(environment)
	if err != nil || p.Environment != digest || !slices.Equal(p.Project, []byte(values["INCUS_PROJECT"])) {
		return domain.ErrPlanStale
	}
	if _, err := ownerImageConfigPaths(values, operatorHome); err != nil {
		return domain.ErrPlanStale
	}
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	return ownerImageCheckGuards(ambient, p.Guards)
}

var errOwnerPolicyImageAuthentication = errors.New("accepted image source authentication custody remains required")

type ownerPolicyImageConnection struct {
	incus.ImageServer
	object string
	local  bool
	guards ports.PreparedOwnerImageSource
	args   *incus.ConnectionArgs
}

func (connection ownerPolicyImageConnection) close() {
	if connection.ImageServer != nil {
		if client, err := connection.GetHTTPClient(); err == nil {
			client.CloseIdleConnections()
		}
		connection.Disconnect()
	}
}

// ownerPolicyImageServer loads only a fixed policy member. Callers provide
// accepted component custody or an authenticated original-operation recovery.
func (client *Client) ownerPolicyImageServer(ctx context.Context, policy ports.PreparedOwnerImagePolicy, reference string, environment []string, operatorHome string) (ownerPolicyImageConnection, error) {
	connection := ownerPolicyImageConnection{}
	if reference != string(policy.Primary) && reference != string(policy.Fallback) {
		return connection, domain.ErrPlanStale
	}
	if err := client.CheckOwnerImagePolicy(ctx, policy, environment, operatorHome); err != nil {
		return connection, err
	}
	values, _, err := ownerImageEnvironment(environment)
	if err != nil {
		return connection, err
	}
	paths, err := ownerImageConfigPaths(values, operatorHome)
	if err != nil {
		return connection, err
	}
	config, err := cliconfig.LoadConfig(paths[0])
	if err != nil {
		return connection, errors.New("accepted image source configuration unavailable")
	}
	config.ProjectOverride = string(policy.Project)
	name, object, err := config.ParseRemote(reference)
	if err != nil || object == "" {
		return connection, errors.New("accepted image reference unavailable")
	}
	remote, exists := config.Remotes[name]
	if !exists {
		return connection, errors.New("accepted image remote unavailable")
	}
	// GetImageServer can refresh OIDC tokens or invoke an OCI helper before
	// returning. Those effects need their own original custody, not this probe.
	if remote.AuthType == api.AuthenticationMethodOIDC || remote.Protocol == "oci" && remote.CredHelper != "" {
		return connection, errOwnerPolicyImageAuthentication
	}
	connection.object = object
	if strings.HasPrefix(remote.Addr, "unix:") {
		project := remote.Project
		if config.ProjectOverride != "" {
			project = config.ProjectOverride
		}
		connection.ImageServer, connection.guards, err = client.ownerPolicyLocalImageServer(ctx, remote.Addr, project, values)
		connection.local = true
		return connection, err
	}
	if remote.Protocol != "simplestreams" && remote.Protocol != "incus" && remote.Protocol != "oci" {
		return connection, errors.New("accepted image source protocol remains required")
	}
	if _, err := ownerImagePublicEndpoint(remote.Addr); err != nil {
		return connection, errOwnerPolicyImageAuthentication
	}
	var certificate []byte
	file, err := os.Open(config.ServerCertPath(name))
	if err == nil {
		certificate, err = io.ReadAll(io.LimitReader(file, ownerImageLimit+1))
		closeErr := file.Close()
		if err != nil || closeErr != nil || len(certificate) > ownerImageLimit {
			return connection, errors.New("accepted image source certificate unavailable")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return connection, errors.New("accepted image source certificate unavailable")
	}
	args := &incus.ConnectionArgs{
		TLSServerCert: string(certificate), SkipGetEvents: true, SkipGetServer: true,
		AuthType: remote.AuthType, UserAgent: config.UserAgent,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		TransportWrapper: func(transport *http.Transport) incus.HTTPTransporter {
			ownerImageProxy(transport)
			return (&ownerImageSecretTransport{ctx: ctx, work: ctx}).wrap(ctx, transport)
		},
	}
	if remote.Protocol == "simplestreams" {
		connection.ImageServer, err = incus.ConnectSimpleStreams(remote.Addr, args)
	} else if remote.Protocol == "oci" {
		connection.ImageServer, err = incus.ConnectOCI(remote.Addr, args)
	} else {
		// The official loader chooses remote-specific versus global credentials
		// and refuses encrypted keys without a prompt. No helper or OIDC path.
		config.PromptPassword = nil
		args.TLSClientCert, args.TLSClientKey, args.TLSCA, err = ownerPolicyClientCertificate(config, name)
		if err != nil {
			return connection, errOwnerPolicyImageAuthentication
		}
		if !remote.Public || args.TLSClientCert != "" || args.TLSClientKey != "" {
			if args.TLSClientCert == "" || args.TLSClientKey == "" {
				return connection, errOwnerPolicyImageAuthentication
			}
			args.HTTPClient, args.TransportWrapper = nil, nil
			project := "default"
			if !remote.Public {
				if remote.Project != "" {
					project = remote.Project
				}
				if config.ProjectOverride != "" {
					project = config.ProjectOverride
				}
			}
			connection.args = args
			connection.ImageServer, err = ownerImageSecretConnect(ctx, remote.Addr, *args, project, &ownerImageSecretTransport{ctx: ctx, work: ctx}, 30*time.Second)
		} else {
			connection.ImageServer, err = incus.ConnectPublicIncusWithContext(ctx, remote.Addr, args)
		}
	}
	if err != nil {
		return ownerPolicyImageConnection{}, errors.New("accepted image source connection unavailable")
	}
	if err := client.CheckOwnerImagePolicy(ctx, policy, environment, operatorHome); err != nil {
		connection.close()
		return ownerPolicyImageConnection{}, err
	}
	return connection, nil
}

func (client *Client) ownerPolicyLocalImageServer(ctx context.Context, address, project string, values map[string]string) (incus.ImageServer, ports.PreparedOwnerImageSource, error) {
	p := ports.PreparedOwnerImageSource{}
	_, p.Environment, _ = ownerImageEnvironment(os.Environ())
	path := strings.TrimPrefix(strings.TrimPrefix(address, "unix:"), "//")
	sockets := []string{path}
	if path == "" {
		if values["INCUS_SOCKET"] != "" {
			sockets = []string{values["INCUS_SOCKET"]}
		} else if values["INCUS_DIR"] != "" {
			sockets = []string{filepath.Join(values["INCUS_DIR"], "unix.socket"), filepath.Join(values["INCUS_DIR"], "unix.socket.user")}
		} else {
			sockets = []string{"/run/incus/unix.socket", "/run/incus/unix.socket.user", "/var/lib/incus/unix.socket", "/var/lib/incus/unix.socket.user"}
		}
	}
	for _, socket := range sockets {
		if !filepath.IsAbs(socket) {
			return nil, p, domain.ErrPlanStale
		}
		state, err := ownerImagePathState(socket, false)
		if err != nil {
			return nil, p, err
		}
		p.Guards = append(p.Guards, ports.OwnerImageSourceGuard{Path: []byte(socket), State: "directory:" + state})
	}
	server, err := incus.ConnectIncusUnixWithContext(ctx, path, &incus.ConnectionArgs{SkipGetEvents: true, SkipGetServer: true})
	if err != nil {
		return nil, p, errors.New("original local image connection unavailable")
	}
	if project != "" {
		server = server.UseProject(project)
	}
	info, err := server.GetConnectionInfo()
	target, targetErr := client.connect(ctx, true)
	if targetErr != nil {
		server.Disconnect()
		return nil, p, targetErr
	}
	targetInfo, targetErr := target.GetConnectionInfo()
	if err != nil || targetErr != nil || info.SocketPath != targetInfo.SocketPath {
		server.Disconnect()
		return nil, p, errors.New("different local image source support remains required")
	}
	if err := ownerImageBoundServer(ctx, server); err != nil {
		server.Disconnect()
		return nil, p, err
	}
	return server, p, nil
}

func ownerPolicyClientCertificate(config *cliconfig.Config, name string) (certificate, key, ca string, err error) {
	// The pinned SDK dereferences a nil PEM block for malformed key files.
	// Keep that credential parse failure inside this accepted adapter boundary.
	defer func() {
		if recover() != nil {
			certificate, key, ca, err = "", "", "", errOwnerPolicyImageAuthentication
		}
	}()
	return config.GetClientCertificate(name)
}

func (client *Client) checkOwnerPolicyImageRead(ctx context.Context, policy ports.PreparedOwnerImagePolicy, connection ownerPolicyImageConnection, environment []string, operatorHome string) error {
	if err := client.CheckOwnerImagePolicy(ctx, policy, environment, operatorHome); err != nil {
		return err
	}
	if connection.local {
		return ownerImageCheckGuards(connection.guards.Environment, connection.guards.Guards)
	}
	return nil
}

func ownerPolicyImageTransient(err error) bool {
	var dns *net.DNSError
	var network net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &dns) && (dns.IsNotFound || dns.IsTemporary || dns.IsTimeout) || errors.As(err, &network) && network.Timeout() || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENETUNREACH)
}

func ownerPolicyImageReadError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, domain.ErrPlanStale) || errors.Is(err, errOwnerPolicyImageAuthentication) {
		return err
	}
	// SDK errors can contain delegated redirect URLs. Keep those only in the
	// transient reader, never in the component diagnostic or reservation.
	return errors.New("accepted owner image metadata unavailable")
}

// ReadOwnerPolicyImageCatalog runs the original three bounded read-only
// metadata attempts after parent acceptance. Only the retained shell rule may
// choose a fallback; no instance creation happens here.
func (client *Client) ReadOwnerPolicyImageCatalog(ctx context.Context, policy ports.PreparedOwnerImagePolicy, environment []string, operatorHome string) ([]byte, error) {
	if ports.NativeOperationObserverFrom(ctx) == nil || ports.NativeInvocationKey(ctx) == "" {
		return nil, domain.ErrPlanStale
	}
	ctx, cancel := context.WithTimeout(ctx, 95*time.Second)
	defer cancel()
	var readErr error
	for attempt := 0; attempt < 3; attempt++ {
		attemptContext, stop := context.WithTimeout(ctx, 30*time.Second)
		connection, err := client.ownerPolicyImageServer(attemptContext, policy, string(policy.Primary), environment, operatorHome)
		var images []api.Image
		if err == nil {
			images, err = connection.GetImages()
			guardErr := client.checkOwnerPolicyImageRead(ctx, policy, connection, environment, operatorHome)
			connection.close()
			if guardErr != nil {
				err = guardErr
			}
		}
		stop()
		if err == nil {
			if images == nil {
				images = []api.Image{}
			}
			raw, err := json.Marshal(images)
			if err != nil || len(raw) > ownerImageLimit {
				return nil, errors.New("accepted image catalog exceeds read boundary")
			}
			return raw, nil
		}
		readErr = err
		if ctx.Err() != nil || attempt == 2 || !ownerPolicyImageTransient(err) {
			break
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, context.Cause(ctx)
		case <-timer.C:
		}
	}
	return nil, ownerPolicyImageReadError(ctx, readErr)
}

// resolveOwnerPolicyImageSource retains only the accepted selection. A private
// source returns an owned token handle, never a serialized credential record.
func (client *Client) resolveOwnerPolicyImageSource(parent context.Context, policy ports.PreparedOwnerImagePolicy, selected, scope string, environment []string, operatorHome string) (api.InstanceSource, *ownerImageSecret, error) {
	if ports.NativeOperationObserverFrom(parent) == nil || ports.NativeInvocationKey(parent) == "" || !ownerCreateScope(scope) {
		return api.InstanceSource{}, nil, domain.ErrPlanStale
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	connection, err := client.ownerPolicyImageServer(ctx, policy, selected, environment, operatorHome)
	if err != nil {
		return api.InstanceSource{}, nil, ownerPolicyImageReadError(ctx, err)
	}
	defer connection.close()
	info, err := connection.GetConnectionInfo()
	if err != nil {
		return api.InstanceSource{}, nil, errors.New("accepted image source connection identity unavailable")
	}
	if info.Protocol == "oci" {
		// The original CLI delegates a selected OCI fallback alias to the daemon.
		// Client-side alias resolution would introduce skopeo and credential files.
		if selected != string(policy.Fallback) || selected == string(policy.Primary) || policy.Kind != "container" {
			return api.InstanceSource{}, nil, domain.ErrPlanStale
		}
		source := api.InstanceSource{Type: "image", Mode: "pull", Protocol: "oci", Server: info.URL, Certificate: info.Certificate, Alias: connection.object}
		if !ownerImageOCISource(source) {
			return api.InstanceSource{}, nil, domain.ErrPlanStale
		}
		if err := client.checkOwnerPolicyImageRead(ctx, policy, connection, environment, operatorHome); err != nil {
			return api.InstanceSource{}, nil, err
		}
		return source, nil, nil
	}
	image, _, err := ownerImageResolve(connection.ImageServer, connection.object, policy.Kind)
	if err != nil {
		return api.InstanceSource{}, nil, ownerPolicyImageReadError(ctx, err)
	}
	source := api.InstanceSource{Type: "image", Fingerprint: image.Fingerprint, Protocol: info.Protocol}
	if connection.local {
		source.Project = info.Project
	} else {
		source.Mode, source.Server, source.Certificate = "pull", info.URL, info.Certificate
		if connection.args != nil && info.Project != "default" {
			source.Project = info.Project
		}
	}
	if err := client.checkOwnerPolicyImageRead(ctx, policy, connection, environment, operatorHome); err != nil {
		return api.InstanceSource{}, nil, err
	}
	if !connection.local && !image.Public {
		if connection.args == nil {
			return api.InstanceSource{}, nil, errOwnerPolicyImageAuthentication
		}
		kind := ownerPolicyImageSecretKind(policy, selected, scope)
		expected, err := nativeOperation(connection.ImageServer.(incus.InstanceServer), kind, info.Project, image.Fingerprint)
		if err != nil {
			return api.InstanceSource{}, nil, errOwnerImageSecret
		}
		handle, err := beginOwnerImageSecret(parent, info.URL, *connection.args, expected)
		if err != nil {
			return api.InstanceSource{}, nil, err
		}
		if err := client.CheckOwnerImagePolicy(parent, policy, environment, operatorHome); err != nil {
			return api.InstanceSource{}, nil, errors.Join(err, handle.Close())
		}
		return handle.Source(), handle, nil
	}
	return source, nil, nil
}

// An OCI source contains only the original public endpoint and selected alias.
func ownerImageOCISource(source api.InstanceSource) bool {
	if source.Type != "image" || source.Protocol != "oci" || source.Mode != "pull" || source.Alias == "" || strings.IndexByte(source.Alias, 0) >= 0 || source.Fingerprint != "" || source.Secret != "" || source.Project != "" {
		return false
	}
	_, err := ownerImagePublicEndpoint(source.Server)
	return err == nil
}

func ownerPolicyImageSecretKind(policy ports.PreparedOwnerImagePolicy, selected, scope string) string {
	role := "primary"
	if selected != string(policy.Primary) {
		if selected != string(policy.Fallback) {
			return ""
		}
		role = "fallback"
	}
	kind := "owner-image-secret-" + scope + "-" + role
	if !ownerImageSecretKind(kind) {
		return ""
	}
	return kind
}

// DrainOwnerImageSecretOperation is called only by the native original-fact
// gateway while both original leases are held. It never resolves an alias or
// creates a token; a missing exact UUID remains unknown.
func (client *Client) DrainOwnerImageSecretOperation(ctx context.Context, policy ports.PreparedOwnerImagePolicy, selected string, environment []string, home string, expected ports.NativeOperation) (ports.NativeOperation, error) {
	scopeRole := strings.TrimPrefix(expected.Kind, "owner-image-secret-")
	scope, _, _ := strings.Cut(scopeRole, "-")
	deadline, bounded := ctx.Deadline()
	if !bounded || !ownerImageSecretKind(expected.Kind) || ownerPolicyImageSecretKind(policy, selected, scope) != expected.Kind || expected.Status != 0 || !ownerImageHex(expected.Resource, 64) || !ownerImageSecretUUID(expected.ID) {
		return ports.NativeOperation{}, errOwnerImageSecret
	}
	metadata, stopMetadata := context.WithTimeout(ctx, 30*time.Second)
	connection, err := client.ownerPolicyImageServer(metadata, policy, selected, environment, home)
	if err != nil {
		stopMetadata()
		return ports.NativeOperation{}, ownerPolicyImageReadError(ctx, err)
	}
	defer connection.close()
	if connection.args == nil || connection.local {
		stopMetadata()
		return ports.NativeOperation{}, errOwnerImageSecret
	}
	info, err := connection.GetConnectionInfo()
	if err != nil || info.Project != expected.Project {
		stopMetadata()
		return ports.NativeOperation{}, errOwnerImageSecret
	}
	identity, err := nativeOperation(connection.ImageServer.(incus.InstanceServer), expected.Kind, expected.Project, expected.Resource)
	projection := expected
	projection.ID = ""
	if err != nil || identity != projection {
		stopMetadata()
		return ports.NativeOperation{}, errOwnerImageSecret
	}
	if err := client.checkOwnerPolicyImageRead(metadata, policy, connection, environment, home); err != nil {
		stopMetadata()
		return ports.NativeOperation{}, err
	}
	stopMetadata()
	custody, stop := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	work, cancelWork := context.WithCancelCause(ctx)
	work, stopWork := context.WithDeadline(work, deadline.Add(-min(30*time.Second, time.Until(deadline)/6)))
	transport := &ownerImageSecretTransport{ctx: custody, work: work}
	h := &ownerImageSecret{work: work, cancelWork: cancelWork, stopWork: stopWork, stop: stop, custody: custody, transport: transport, operation: expected, done: make(chan struct{})}
	h.control, err = ownerImageSecretConnect(custody, info.URL, *connection.args, expected.Project, transport, 30*time.Second)
	if err != nil {
		h.release()
		return ports.NativeOperation{}, errOwnerImageSecret
	}
	remote, _, err := h.control.GetOperation(expected.ID)
	if err != nil || remote == nil || !ownerImageSecretIdentity(*remote, expected) {
		h.release()
		return ports.NativeOperation{}, errOwnerImageSecret
	}
	if err := client.CheckOwnerImagePolicy(ctx, policy, environment, home); err != nil {
		h.release()
		return ports.NativeOperation{}, err
	}
	if remote.StatusCode == api.Cancelled {
		h.release()
		expected.Status = 401
		return expected, nil
	}
	if remote.StatusCode != api.Running {
		h.release()
		return ports.NativeOperation{}, errOwnerImageSecret
	}
	ready, err := h.armWatch(custody, info.URL, *connection.args, nil, true)
	if err != nil {
		_ = h.cleanup()
		h.release()
		return ports.NativeOperation{}, errOwnerImageSecret
	}
	armed := false
	select {
	case <-ready:
		armed = true
	case <-h.done:
	case <-work.Done():
	}
	if err := h.Close(); err != nil {
		return ports.NativeOperation{}, err
	}
	if !armed && h.terminal.Status != 401 {
		return ports.NativeOperation{}, errOwnerImageSecret
	}
	if err := client.CheckOwnerImagePolicy(ctx, policy, environment, home); err != nil {
		return ports.NativeOperation{}, err
	}
	return h.terminal, nil
}

func ownerImageResolve(server incus.ImageServer, object, kind string) (*api.Image, string, error) {
	alias, _, aliasErr := server.GetImageAliasType(kind, object)
	fingerprint, aliasName := object, ""
	if aliasErr == nil && alias != nil {
		fingerprint, aliasName = alias.Target, object
	}
	if fingerprint == "" || len(fingerprint) > 64 || strings.Trim(fingerprint, "0123456789abcdef") != "" {
		return nil, "", errors.New("original selected image alias unavailable")
	}
	image, _, err := server.GetImage(fingerprint)
	if err != nil || image == nil || len(image.Fingerprint) != 64 || strings.Trim(image.Fingerprint, "0123456789abcdef") != "" || !strings.HasPrefix(image.Fingerprint, fingerprint) || image.Type != kind && !(image.Type == "" && kind == "container") {
		return nil, "", errors.New("original selected image unavailable")
	}
	return image, aliasName, nil
}
