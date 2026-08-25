package testvmsruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Backend struct {
	RepositoryRoot string
	DataHome       string
	Dispatcher     string
	Project        string
	Instance       string
	YardName       string
	DesiredPower   string
	Environment    map[string]string
	Runner         CommandRunner
	Output         io.Writer
	Start          func(context.Context) error
	Stop           func(context.Context) error
}

type backendState struct {
	enabled         string
	cpu             string
	image           string
	memory          string
	disk            string
	slotCount       string
	bootTimeout     string
	brokerSource    string
	engineHash      string
	marker          string
	clientDirectory string
	provision       string
	downloadHelper  string
}

func (backend *Backend) Converged(ctx context.Context) (bool, error) {
	state, err := backend.state()
	if err != nil {
		return false, err
	}
	if backend.Runner == nil {
		backend.Runner = ProcessRunner{}
	}
	marker, err := backend.incus(ctx, "config", "get", backend.Instance,
		"user.subyard.test_vms_revision", "--project", backend.Project)
	if err != nil || strings.TrimSpace(marker) != state.marker {
		return false, nil
	}
	outer, err := backend.outerState(ctx)
	if err != nil {
		return false, nil
	}
	if ok, err := backend.routeConverged(ctx, state, outer == "RUNNING"); err != nil || !ok {
		return false, err
	}
	if outer != "RUNNING" {
		return outer == "STOPPED" && backend.DesiredPower == "stopped", nil
	}
	_, err = backend.incus(ctx,
		"exec", backend.Instance, "--project", backend.Project,
		"--env", "WANT_ENABLED="+state.enabled,
		"--env", "WANT_ENGINE_HASH="+state.engineHash,
		"--", DefaultInstalledPath, "_test-vms-worker", "doctor")
	return err == nil, nil
}

func (backend *Backend) Apply(ctx context.Context) (err error) {
	state, err := backend.state()
	if err != nil {
		return err
	}
	if backend.Runner == nil {
		backend.Runner = ProcessRunner{}
	}
	if backend.Output == nil {
		backend.Output = io.Discard
	}
	if backend.DesiredPower != "running" && backend.DesiredPower != "stopped" {
		return errors.New("prepared desired power is required")
	}
	outer, err := backend.outerState(ctx)
	if err != nil {
		return fmt.Errorf("inspect outer yard: %w", err)
	}
	temporaryStart := false
	if outer != "RUNNING" {
		if outer != "STOPPED" {
			return fmt.Errorf("cannot reconcile nested VM backend while yard state is %q", outer)
		}
		if backend.Start == nil || backend.Stop == nil {
			return errors.New("temporary yard power callbacks are required")
		}
		if err := backend.Start(ctx); err != nil {
			return err
		}
		temporaryStart = backend.DesiredPower == "stopped"
	}
	defer func() {
		if !temporaryStart {
			return
		}
		if stopErr := backend.Stop(ctx); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("restore desired stopped state: %w", stopErr))
		}
	}()

	// Replacing an executing binary through SFTP fails with ETXTBSY. Publish a
	// sibling candidate and rename it into place so active lease workers keep
	// their old inode while the next invocation sees the new engine.
	installCandidate := DefaultInstalledPath + ".new"
	if _, err := backend.incus(ctx, "file", "push", backend.Dispatcher,
		backend.Instance+installCandidate, "--project", backend.Project,
		"--create-dirs", "--uid", "0", "--gid", "0", "--mode", "0755"); err != nil {
		return err
	}
	if _, err := backend.incus(ctx, "exec", backend.Instance, "--project", backend.Project,
		"--", "mv", "-f", "--", installCandidate, DefaultInstalledPath); err != nil {
		return err
	}
	downloadHelper, err := os.Open(state.downloadHelper)
	if err != nil {
		return err
	}
	defer downloadHelper.Close()
	provision, err := os.Open(state.provision)
	if err != nil {
		return err
	}
	defer provision.Close()
	arguments := []string{"exec", backend.Instance, "--project", backend.Project}
	for _, name := range []string{
		"NESTED_E2E_VMS", "DEV_USER", "E2E_VM_IMAGE", "E2E_VM_CPU", "E2E_VM_MEMORY",
		"E2E_VM_DISK", "E2E_VM_SLOT_COUNT", "E2E_VM_BOOT_TIMEOUT", "E2E_BROKER_SOURCE",
	} {
		value := backend.Environment[name]
		if name == "E2E_BROKER_SOURCE" && value == "" {
			value = state.brokerSource
		}
		arguments = append(arguments, "--env", name+"="+value)
	}
	arguments = append(arguments, "--env", "E2E_AGENT_PUBLIC_KEY=",
		"--", "bash", "-euo", "pipefail", "-s")
	fmt.Fprintln(backend.Output, "  [ .. ] reconciling nested VM physical backend")
	payload := io.MultiReader(downloadHelper, provision)
	_, stderr, runErr := backend.Runner.Run(ctx, "incus", arguments, nil, payload)
	if runErr != nil {
		if len(stderr) != 0 {
			fmt.Fprint(backend.Output, string(stderr))
		}
		return runErr
	}
	if state.enabled == "1" {
		if err := backend.publishRoute(ctx, state); err != nil {
			return err
		}
	} else if err := backend.removeRoute(state); err != nil {
		return err
	}
	if _, err := backend.incus(ctx, "config", "set", backend.Instance,
		"user.subyard.test_vms_revision", state.marker, "--project", backend.Project); err != nil {
		return err
	}
	if _, err := backend.incus(ctx, "config", "set", backend.Instance,
		"user.subyard.test_vms_spool_schema",
		fmt.Sprint(BrokerSpoolSchemaVersion),
		"--project", backend.Project); err != nil {
		return err
	}
	fmt.Fprintf(backend.Output, "  [ ok ] nested E2E VM backend reconciled (enabled=%s)\n",
		state.enabled)
	return nil
}

func (backend *Backend) state() (backendState, error) {
	value := func(name, fallback string) string {
		if backend.Environment[name] != "" {
			return backend.Environment[name]
		}
		return fallback
	}
	state := backendState{
		enabled: value("NESTED_E2E_VMS", "0"), cpu: value("E2E_VM_CPU", "4"),
		image:           value("E2E_VM_IMAGE", "images:debian/13/cloud"),
		memory:          value("E2E_VM_MEMORY", "4GiB"),
		disk:            value("E2E_VM_DISK", "20GiB"),
		slotCount:       value("E2E_VM_SLOT_COUNT", "2"),
		bootTimeout:     value("E2E_VM_BOOT_TIMEOUT", "300"),
		brokerSource:    value("E2E_BROKER_SOURCE", backend.YardName),
		provision:       filepath.Join(backend.RepositoryRoot, "scripts", "e2e-lab", "provision.sh"),
		downloadHelper:  filepath.Join(backend.RepositoryRoot, "scripts", "lib", "download.sh"),
		clientDirectory: backend.Environment["SUBYARD_E2E_CLIENT_EXPORT_DIR"],
	}
	if state.brokerSource == "" {
		state.brokerSource = "default"
	}
	if state.enabled != "0" && state.enabled != "1" {
		return state, errors.New("invalid NESTED_E2E_VMS")
	}
	if backend.Dispatcher == "" {
		return state, errors.New("test-vms engine source is required")
	}
	if state.clientDirectory == "" {
		yard := backend.YardName
		if yard == "" {
			yard = "default"
		}
		state.clientDirectory = filepath.Join(backend.DataHome, "e2e", "routes", yard)
	}
	engineHash, err := fileSHA256(backend.Dispatcher)
	if err != nil {
		return state, err
	}
	provisionHash, err := fileSHA256(state.provision)
	if err != nil {
		return state, err
	}
	downloadHash, err := fileSHA256(state.downloadHelper)
	if err != nil {
		return state, err
	}
	state.engineHash = engineHash
	revision := sha256.Sum256([]byte(engineHash + "\n" + provisionHash + "\n" + downloadHash + "\n"))
	state.marker = strings.Join([]string{
		state.enabled, hex.EncodeToString(revision[:]), state.image, state.cpu,
		state.memory, state.disk, state.slotCount, state.bootTimeout,
		state.brokerSource,
	}, ":")
	return state, nil
}

func (backend *Backend) routeConverged(
	ctx context.Context,
	state backendState,
	running bool,
) (bool, error) {
	if state.enabled != "1" {
		_, err := os.Lstat(filepath.Join(state.clientDirectory, "current"))
		return os.IsNotExist(err), nil
	}
	published, exists, err := ReadPublishedRoute(state.clientDirectory)
	if err != nil || !exists {
		return false, err
	}
	if !running {
		return true, nil
	}
	observed, err := backend.observeRoute(ctx)
	if err != nil {
		return false, err
	}
	return published == observed, nil
}

func (backend *Backend) observeRoute(ctx context.Context) (RouteIdentity, error) {
	return ObserveRoute(func(arguments ...string) (string, error) {
		incusArguments := []string{
			"exec", backend.Instance, "--project", backend.Project, "--",
		}
		return backend.incus(ctx, append(incusArguments, arguments...)...)
	})
}

func (backend *Backend) publishRoute(ctx context.Context, state backendState) error {
	identity, err := backend.observeRoute(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(state.clientDirectory, 0o755); err != nil {
		return err
	}
	generation, err := os.MkdirTemp(state.clientDirectory, ".route-")
	if err != nil {
		return err
	}
	if err := os.Chmod(generation, 0o755); err != nil {
		_ = os.RemoveAll(generation)
		return err
	}
	keepGeneration := false
	defer func() {
		if !keepGeneration {
			_ = os.RemoveAll(generation)
		}
	}()
	route, knownHosts := RoutePayload(identity)
	if err := writeAtomic(filepath.Join(generation, "route.tsv"), []byte(route), 0o644); err != nil {
		return err
	}
	if err := writeAtomic(
		filepath.Join(generation, "known_hosts"), []byte(knownHosts), 0o644,
	); err != nil {
		return err
	}
	link := filepath.Join(state.clientDirectory, ".current-new")
	_ = os.Remove(link)
	if err := os.Symlink(filepath.Base(generation), link); err != nil {
		return err
	}
	if err := os.Rename(link, filepath.Join(state.clientDirectory, "current")); err != nil {
		_ = os.Remove(link)
		return err
	}
	keepGeneration = true
	return removeInactiveRouteGenerations(state.clientDirectory, filepath.Base(generation))
}

func (backend *Backend) removeRoute(state backendState) error {
	current := filepath.Join(state.clientDirectory, "current")
	if err := os.Remove(current); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func removeInactiveRouteGenerations(root, active string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == active || !strings.HasPrefix(name, ".route-") {
			continue
		}
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe inactive test-vms route generation %q", path)
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

func (backend *Backend) outerState(ctx context.Context) (string, error) {
	state, err := backend.incus(ctx, "list", backend.Instance, "--project", backend.Project,
		"-f", "csv", "-c", "s")
	return strings.TrimSpace(state), err
}

func (backend *Backend) incus(ctx context.Context, arguments ...string) (string, error) {
	environment := make([]string, 0, len(backend.Environment))
	for name, value := range backend.Environment {
		environment = append(environment, name+"="+value)
	}
	stdout, _, err := backend.Runner.Run(ctx, "incus", arguments, environment, nil)
	return string(stdout), err
}

func bytesCount(value []byte, needle byte) int {
	count := 0
	for _, current := range value {
		if current == needle {
			count++
		}
	}
	return count
}
