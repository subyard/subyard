package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/resourceendpoint"
)

func (cli *CLI) printResourceEndpoint(loaded config.Loaded, definition resource.Definition) {
	host := loaded.Environment[definition.Proxy.AdvertiseHostSetting]
	port := loaded.Environment[definition.Proxy.HostPortSetting]
	if host == "" || port == "" {
		fmt.Fprintf(cli.options.Stdout, "Endpoint: not allocated; %s up discovers the owner Tailscale address and selects a port starting at %d\n", definition.Command, definition.Endpoint.PreferredPort)
		return
	}
	hostOverride, portOverride := config.ResourceEndpointOverrides(loaded, definition)
	hostSource, portSource := "saved", "saved"
	if hostOverride != "" {
		hostSource = "override"
	}
	if portOverride != "" {
		portSource = "override"
	}
	fmt.Fprintf(cli.options.Stdout, "Endpoint: %s:%s (host: %s; port: %s)\n", host, port, hostSource, portSource)
}

func (cli *CLI) resourceEndpointManager() resourceendpoint.Manager {
	return resourceendpoint.Manager{
		Discover: func(ctx context.Context) (string, error) {
			data, err := cli.readEndpointCommand(ctx, "tailscale", "status", "--json")
			if err != nil {
				return "", errors.New("cannot discover owner Tailscale address; connect this host to Tailscale or explicitly configure a loopback endpoint for SSH forwarding")
			}
			host, address, err := tailscaleEndpointHost(data)
			if err != nil {
				return "", err
			}
			if host != address {
				resolved, resolveErr := cli.readEndpointCommand(ctx, "getent", "ahostsv4", host)
				matches := false
				for _, line := range strings.Split(string(resolved), "\n") {
					fields := strings.Fields(line)
					if len(fields) > 0 && fields[0] == address {
						matches = true
					}
				}
				if resolveErr != nil || !matches {
					return address, nil
				}
			}
			return host, nil
		},
		Occupied: func(ctx context.Context, host string, port int) (bool, error) {
			address := host
			if host == "localhost" {
				address = "127.0.0.1"
			}
			if _, err := netip.ParseAddr(address); err != nil {
				data, err := cli.readEndpointCommand(ctx, "getent", "ahostsv4", host)
				if err != nil {
					return false, errors.New("cannot resolve owner endpoint hostname")
				}
				fields := strings.Fields(string(data))
				if len(fields) == 0 {
					return false, errors.New("owner endpoint hostname has no IPv4 address")
				}
				address = fields[0]
			}
			data, err := cli.readEndpointCommand(ctx, "ss", "-H", "-ltn", "sport = :"+strconv.Itoa(port))
			if err != nil {
				return false, errors.New("cannot inspect owner TCP listeners with ss")
			}
			return endpointListenerPresent(data, address, port), nil
		},
	}
}

func (cli *CLI) readEndpointCommand(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	path := cli.baseEnv["PATH"]
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	program := ""
	for _, directory := range filepath.SplitList(path) {
		if !filepath.IsAbs(directory) {
			continue
		}
		candidate := filepath.Join(directory, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			program = candidate
			break
		}
	}
	if program == "" {
		return nil, fmt.Errorf("%s is unavailable", name)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, program, arguments...)
	configureResourceProcess(command)
	command.Env = []string{"PATH=" + path, "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	output := &boundedResourceBuffer{limit: 4 << 20}
	command.Stdout = output
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("%s inspection failed", name)
	}
	if output.overflow {
		return nil, fmt.Errorf("%s inspection exceeded output limit", name)
	}
	return output.Bytes(), nil
}

func tailscaleEndpointHost(data []byte) (string, string, error) {
	var status struct {
		BackendState string
		Self         *struct {
			DNSName      string
			TailscaleIPs []string
		}
	}
	if json.Unmarshal(data, &status) != nil || status.BackendState != "Running" || status.Self == nil {
		return "", "", errors.New("owner Tailscale is not running or returned invalid status")
	}
	ipv4 := ""
	for _, value := range status.Self.TailscaleIPs {
		address, err := netip.ParseAddr(value)
		if err == nil && address.Is4() && netip.MustParsePrefix("100.64.0.0/10").Contains(address) {
			if ipv4 != "" {
				return "", "", errors.New("owner Tailscale has multiple IPv4 addresses")
			}
			ipv4 = address.String()
		}
	}
	if ipv4 == "" {
		return "", "", errors.New("owner Tailscale has no active IPv4 address")
	}
	if name := strings.TrimSuffix(status.Self.DNSName, "."); name != "" {
		return name, ipv4, nil
	}
	return ipv4, ipv4, nil
}

func endpointListenerPresent(data []byte, address string, port int) bool {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		host, value, err := net.SplitHostPort(fields[3])
		if err != nil {
			continue
		}
		if value == strconv.Itoa(port) && (host == address || host == "0.0.0.0" || host == "*" || host == "::") {
			return true
		}
	}
	return false
}
