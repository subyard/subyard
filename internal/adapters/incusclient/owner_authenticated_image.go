package incusclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/ports"
	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/cliconfig"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"golang.org/x/sys/unix"
)

func (client *Client) ownerAuthenticatedImageServer(ctx context.Context, policy ports.PreparedOwnerImagePolicy, reference string, environment []string, home string, config *cliconfig.Config, name, object string, remote cliconfig.Remote) (connection ownerPolicyImageConnection, result error) {
	endpoint := remote.Addr
	if remote.Protocol == "oci" {
		if u, err := url.Parse(endpoint); err == nil {
			u.User = nil
			endpoint = u.String()
		}
	}
	if _, err := ownerImagePublicEndpoint(endpoint); err != nil {
		return connection, err
	}
	leaf := sourceLeaf(ctx)
	if leaf == nil {
		return connection, errOwnerPolicyImageAuthentication
	}
	incusOIDC := remote.AuthType == api.AuthenticationMethodOIDC && remote.Protocol == "incus" && !strings.HasPrefix(remote.Addr, "unix:")
	var stopBrowser func() error
	if incusOIDC {
		var err error
		stopBrowser, err = leaf.oidcBrowser(ctx)
		if err != nil {
			return connection, err
		}
	}
	// The bridge must live through refresh/device flow in all subsequent SDK
	// reads and token cleanup. Its fixed listener closes with the source leaf.
	if stopBrowser != nil {
		leaf.browserStops = append(leaf.browserStops, stopBrowser)
	}
	config.PromptPassword = nil
	var server incus.ImageServer
	var args *incus.ConnectionArgs
	var err error
	if incusOIDC {
		// Keep the SDK-mutated pointer available even when the next metadata read
		// fails. Config.GetImageServer otherwise hides it behind its initial GET.
		tokens := &oidc.Tokens[*oidc.IDTokenClaims]{}
		raw, readErr := os.ReadFile(config.OIDCTokenPath(name))
		if readErr == nil {
			if len(raw) > ownerImageLimit || json.Unmarshal(raw, tokens) != nil {
				return connection, errors.New("original OIDC cache unavailable")
			}
		} else if !os.IsNotExist(readErr) {
			return connection, readErr
		}
		certificate, readErr := os.ReadFile(config.ServerCertPath(name))
		if readErr != nil && !os.IsNotExist(readErr) {
			return connection, readErr
		}
		args = &incus.ConnectionArgs{AuthType: api.AuthenticationMethodOIDC, OIDCTokens: tokens, TLSServerCert: string(certificate), UserAgent: config.UserAgent}
		project := ""
		if !remote.Public && remote.Project != "default" {
			project = remote.Project
		}
		if !remote.Public && config.ProjectOverride != "" {
			project = config.ProjectOverride
		}

		server, err = ownerImageSecretConnect(ctx, remote.Addr, *args, project, &ownerImageSecretTransport{ctx: ctx, work: ctx}, 30*time.Second)
	} else {
		if remote.CredHelper != "" {
			helper, err := client.retainOwnerImageHelper(ctx, policy, remote.CredHelper)
			if err != nil {
				return connection, err
			}
			leaf.helperFiles = append(leaf.helperFiles, helper)
			remote.CredHelper = fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), helper.Fd())
			config.Remotes[name] = remote
		}
		server, err = config.GetImageServer(name)
	}
	if err != nil {
		return connection, errors.New("original authenticated image connection unavailable")
	}
	connection = ownerPolicyImageConnection{ImageServer: server, object: object, args: args}
	if incusOIDC {
		tokens, ok := server.(interface {
			GetOIDCTokens() *oidc.Tokens[*oidc.IDTokenClaims]
		})
		if !ok {
			return connection, errOwnerPolicyImageAuthentication
		}
		certificate, err := os.ReadFile(config.ServerCertPath(name))
		if err != nil && !os.IsNotExist(err) {
			return connection, errors.New("original server certificate unavailable")
		}
		connection.args = &incus.ConnectionArgs{AuthType: api.AuthenticationMethodOIDC, OIDCTokens: tokens.GetOIDCTokens(), TLSServerCert: string(certificate), UserAgent: config.UserAgent}
		connection.save = func() error { return client.saveOwnerImageCache(ctx, policy, config, name, server) }
	}
	if err := ownerImageBoundServer(ctx, server); err != nil {
		connection.close()
		return ownerPolicyImageConnection{}, err
	}
	// OCI SDK credentials stay private in its connection and are materialized in
	// the single target request only inside this same leaf.
	if remote.Protocol == "oci" {
		if h, err := server.GetHTTPClient(); err == nil {
			h.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				if len(via) > 10 {
					return http.ErrUseLastResponse
				}
				return nil
			}
		}
	}
	if err := client.CheckOwnerImagePolicy(ctx, policy, environment, home); err != nil {
		connection.close()
		return ownerPolicyImageConnection{}, err
	}
	return connection, nil
}

func ownerImagePublicOCISource(source api.InstanceSource) api.InstanceSource {
	if source.Protocol == "oci" {
		if u, err := url.Parse(source.Server); err == nil {
			u.User = nil
			source.Server = u.String()
		}
	}
	return source
}
func ownerNativeOCISource(ctx context.Context, source api.InstanceSource) bool {
	if ownerImageOCISource(source) {
		return true
	}
	leaf := sourceLeaf(ctx)
	return leaf != nil && leaf.oci != nil && reflect.DeepEqual(*leaf.oci, source) && ownerImageOCISource(ownerImagePublicOCISource(source))
}

func (client *Client) retainOwnerImageHelper(ctx context.Context, p ports.PreparedOwnerImagePolicy, name string) (*os.File, error) {
	directory, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	path, err := ownerSourceExecutable(name, os.Getenv("PATH"), directory)
	if err != nil {
		return nil, err
	}
	if err := client.CheckOwnerImagePolicy(ctx, p, os.Environ(), sourceLeaf(ctx).request.Home); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	fd, err := unix.MemfdCreate("subyard-original-image-helper", unix.MFD_ALLOW_SEALING|unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	image := os.NewFile(uintptr(fd), "original-image-helper")
	fail := func(err error) (*os.File, error) { image.Close(); return nil, err }
	n, err := io.Copy(image, io.LimitReader(file, ownerImageLimit+1))
	if err != nil || n > ownerImageLimit {
		return fail(errors.New("original helper source bound"))
	}
	if err := client.CheckOwnerImagePolicy(ctx, p, os.Environ(), sourceLeaf(ctx).request.Home); err != nil {
		return fail(err)
	}
	if err := image.Chmod(0o500); err != nil {
		return fail(err)
	}
	if _, err := unix.FcntlInt(image.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		return fail(err)
	}
	if _, err := image.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return image, nil
}

func (client *Client) ownerImagePreparationAuth(policy ports.PreparedOwnerImagePolicy, environment []string, home string) bool {
	values, _, err := ownerImageEnvironment(environment)
	if err != nil {
		return false
	}
	paths, err := ownerImageConfigPaths(values, home)
	if err != nil {
		return false
	}
	config, err := cliconfig.LoadConfig(paths[0])
	if err != nil {
		return false
	}
	name, _, err := config.ParseRemote(string(policy.Primary))
	if err != nil {
		return false
	}
	remote := config.Remotes[name]
	return remote.AuthType == api.AuthenticationMethodOIDC || remote.Protocol == "oci" && remote.CredHelper != ""
}
