package testvmsruntime

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	schema       = "subyard-e2e-route-v1"
	hostKeyAlias = "subyard-e2e-bastion"
)

type RouteIdentity struct{ Hostname, HostKey string }

type routeConflictError struct{ message string }

func (failure routeConflictError) Error() string    { return failure.message }
func (routeConflictError) ActivationConflict() bool { return true }

func ReadPublishedRoute(root string) (RouteIdentity, bool, error) {
	identity := RouteIdentity{}
	generation, err := PublishedRouteDirectory(root)
	if errors.Is(err, os.ErrNotExist) {
		return identity, false, nil
	}
	if err != nil {
		return identity, false, err
	}
	info, err := os.Lstat(generation)
	if err != nil {
		return identity, false, fmt.Errorf("inspect route generation: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return identity, false, errors.New("route generation is not a directory")
	}
	route, err := readRegular(filepath.Join(generation, "route.tsv"))
	if err != nil {
		return identity, false, err
	}
	lines := strings.Split(strings.TrimSuffix(string(route), "\n"), "\n")
	if len(lines) != 4 || lines[0] != schema {
		return identity, false, errors.New("route payload is invalid")
	}
	values := map[string]string{}
	for _, line := range lines[1:] {
		fields := strings.Split(line, "\t")
		if len(fields) != 2 || values[fields[0]] != "" {
			return identity, false, errors.New("route fields are invalid")
		}
		values[fields[0]] = fields[1]
	}
	if !ipv4(values["hostname"]) || values["port"] != "22" ||
		values["host_key_alias"] != hostKeyAlias {
		return identity, false, errors.New("route values are invalid")
	}
	knownHosts, err := readRegular(filepath.Join(generation, "known_hosts"))
	if err != nil {
		return identity, false, err
	}
	fields := strings.Fields(string(knownHosts))
	if len(fields) != 3 || fields[0] != hostKeyAlias {
		return identity, false, errors.New("route host-key pin is invalid")
	}
	hostKey, err := normalizeHostKey(strings.Join(fields[1:], " "))
	if err != nil {
		return identity, false, errors.New("route host-key pin cannot be parsed")
	}
	return RouteIdentity{Hostname: values["hostname"], HostKey: hostKey}, true, nil
}

func ObserveRoute(run func(arguments ...string) (string, error)) (RouteIdentity, error) {
	routes, err := run("ip", "-4", "-o", "route", "show", "default")
	if err != nil {
		return RouteIdentity{}, err
	}
	devices := map[string]bool{}
	for _, line := range strings.Split(routes, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "default" {
			continue
		}
		for index := 0; index+1 < len(fields); index++ {
			if fields[index] == "dev" {
				devices[fields[index+1]] = true
			}
		}
	}
	if len(devices) == 0 {
		return RouteIdentity{}, errors.New("outer-yard default route is unavailable")
	}
	if len(devices) != 1 {
		return RouteIdentity{}, routeConflictError{"outer-yard route device is ambiguous"}
	}
	var device string
	for device = range devices {
	}
	addresses, err := run(
		"ip", "-4", "-o", "address", "show", "dev", device, "scope", "global",
	)
	if err != nil {
		return RouteIdentity{}, err
	}
	found := map[string]bool{}
	for _, line := range strings.Split(addresses, "\n") {
		fields := strings.Fields(line)
		for index := 0; index+1 < len(fields); index++ {
			if fields[index] == "inet" {
				found[strings.SplitN(fields[index+1], "/", 2)[0]] = true
			}
		}
	}
	if len(found) == 0 {
		return RouteIdentity{}, errors.New("outer-yard IPv4 address is unavailable")
	}
	if len(found) != 1 {
		return RouteIdentity{}, routeConflictError{"outer-yard IPv4 address is ambiguous"}
	}
	var hostname string
	for hostname = range found {
	}
	if !ipv4(hostname) {
		return RouteIdentity{}, errors.New("outer-yard IPv4 address is invalid")
	}
	hostKeyPayload, err := run("cat", "/etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		return RouteIdentity{}, err
	}
	hostKey, err := normalizeHostKey(hostKeyPayload)
	if err != nil {
		return RouteIdentity{}, errors.New("outer-yard SSH host key is invalid")
	}
	return RouteIdentity{Hostname: hostname, HostKey: hostKey}, nil
}

func RoutePayload(identity RouteIdentity) (route, knownHosts string) {
	route = schema + "\nhostname\t" + identity.Hostname +
		"\nport\t22\nhost_key_alias\t" + hostKeyAlias + "\n"
	knownHosts = hostKeyAlias + " " + identity.HostKey + "\n"
	return route, knownHosts
}

func PublishedRouteDirectory(root string) (string, error) {
	target, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil {
		return "", err
	}
	if filepath.Base(target) != target || !strings.HasPrefix(target, ".route-") {
		return "", errors.New("unsafe route generation link")
	}
	return filepath.Join(root, target), nil
}

func readRegular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect route artifact %s: %w", filepath.Base(path), err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("route artifact %s is not a regular file", filepath.Base(path))
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read route artifact %s: %w", filepath.Base(path), err)
	}
	return payload, nil
}

func normalizeHostKey(value string) (string, error) {
	fields := strings.Fields(value)
	if len(fields) < 2 || fields[0] != ssh.KeyAlgoED25519 {
		return "", errors.New("Ed25519 public key is required")
	}
	key := fields[0] + " " + fields[1]
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key)); err != nil {
		return "", err
	}
	return key, nil
}

func ipv4(value string) bool {
	ip := net.ParseIP(value)
	return ip != nil && ip.To4() != nil
}
