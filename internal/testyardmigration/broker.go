package testyardmigration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
)

type BrokerRuntimeState string

const (
	BrokerRuntimeAbsent   BrokerRuntimeState = "absent"
	BrokerRuntimeInactive BrokerRuntimeState = "inactive"
	BrokerRuntimeActive   BrokerRuntimeState = "active"
)

type brokerRuntime struct {
	state    BrokerRuntimeState
	yard     string
	project  string
	instance string
}

// PrepareBrokerRuntimeTarget returns the exact registered/backend yard that a
// release activation must observe. During the one-time owner migration this
// may be e2e-yard before commit and test-yard afterwards.
func PrepareBrokerRuntimeTarget(
	ctx context.Context,
	options Options,
) (BrokerRuntimeState, string, error) {
	observed, err := inspectBrokerRuntime(ctx, options)
	return observed.state, observed.yard, err
}

// VerifyBrokerRuntime checks both liveness and the exact engine shipped by the
// selected release. This makes the typed operation reconcile active brokers on
// later runtime updates even after the layout transition has been applied.
func VerifyBrokerRuntime(
	ctx context.Context,
	options Options,
	before BrokerRuntimeState,
) error {
	if err := validateOptions(&options); err != nil {
		return err
	}
	if err := ValidateBrokerRuntimeState(before); err != nil {
		return err
	}
	observed, err := inspectBrokerRuntime(ctx, options)
	if err != nil {
		return err
	}
	if observed.state != before {
		return fmt.Errorf(
			"test VM broker changed from %s to %s during runtime migration",
			before,
			observed.state,
		)
	}
	if before != BrokerRuntimeActive {
		return nil
	}
	expected, err := fileDigest(filepath.Join(options.RepositoryRoot, "bin", "yard-engine"))
	if err != nil {
		return fmt.Errorf("hash release test VM broker engine: %w", err)
	}
	if _, err := os.Stat(filepath.Join(
		options.RepositoryRoot,
		"scripts",
		"install-test-vms-host-sink.sh",
	)); err == nil {
		hostSink, digestErr := fileDigest(hostSinkPath(options))
		if digestErr != nil {
			return fmt.Errorf("hash installed test VM broker host sink: %w", digestErr)
		}
		if hostSink != expected {
			return errors.New("test VM broker host sink does not use the selected runtime engine")
		}
	}
	payload, err := runIncus(
		ctx,
		options,
		"exec",
		observed.instance,
		"--project",
		observed.project,
		"--",
		"sha256sum",
		"/usr/local/libexec/subyard/test-vms-inner",
	)
	if err != nil {
		return fmt.Errorf("hash installed test VM broker engine: %w", err)
	}
	fields := strings.Fields(string(payload))
	if len(fields) != 2 || fields[0] != expected {
		return errors.New("active test VM broker does not use the selected runtime engine")
	}
	if err := run(ctx, options, observed.yard, io.Discard, "test-vms", "status"); err != nil {
		return fmt.Errorf("verify updated test VM broker facade: %w", err)
	}
	return nil
}

func hostSinkPath(options Options) string {
	const fallback = "/usr/local/libexec/subyard/test-vms-host-sink"
	for index := len(options.Environment) - 1; index >= 0; index-- {
		assignment := options.Environment[index]
		if value, ok := strings.CutPrefix(
			assignment,
			"SUBYARD_TEST_VMS_SINK_PATH=",
		); ok && value != "" {
			return value
		}
	}
	return fallback
}

func ValidateBrokerRuntimeState(state BrokerRuntimeState) error {
	switch state {
	case BrokerRuntimeAbsent, BrokerRuntimeInactive, BrokerRuntimeActive:
		return nil
	default:
		return fmt.Errorf("invalid prepared test VM broker state %q", state)
	}
}

func inspectBrokerRuntime(
	ctx context.Context,
	options Options,
) (brokerRuntime, error) {
	if err := validateOptions(&options); err != nil {
		return brokerRuntime{}, err
	}
	if options.RepositoryRoot == "" || !filepath.IsAbs(options.RepositoryRoot) {
		return brokerRuntime{}, errors.New("absolute migration repository root is required")
	}
	registrations, err := inspectRegistrationSet(options)
	if err != nil {
		return brokerRuntime{}, err
	}
	if registrations.legacy && registrations.current {
		return brokerRuntime{}, errors.New(
			"both legacy and current test-yard registrations exist; refusing broker migration",
		)
	}
	yard := ""
	switch {
	case registrations.current:
		yard = CurrentYard
	case registrations.legacy:
		yard = LegacyYard
	default:
		return brokerRuntime{state: BrokerRuntimeAbsent}, nil
	}
	loaded, err := loadBrokerYard(options, yard)
	if err != nil {
		return brokerRuntime{}, err
	}
	projects, err := inspectProjects(ctx, options)
	if err != nil {
		return brokerRuntime{}, err
	}
	backendYard := yard
	if yard == LegacyYard && projects.current && !projects.legacy {
		backendYard = CurrentYard
	}
	result := brokerRuntime{
		state:    BrokerRuntimeInactive,
		yard:     backendYard,
		project:  loaded.Context.IncusProject,
		instance: loaded.Context.YardInstanceName,
	}
	if backendYard != yard {
		result.project = "subyard-" + backendYard
		result.instance = "yard-" + backendYard
	}
	if !loaded.Context.NestedE2EVMs {
		return result, nil
	}
	projectExists := backendYard == LegacyYard && projects.legacy ||
		backendYard == CurrentYard && projects.current
	if !projectExists {
		return result, nil
	}
	instancesPayload, err := runIncus(
		ctx,
		options,
		"list",
		result.instance,
		"--project",
		result.project,
		"--format=json",
	)
	if err != nil {
		return brokerRuntime{}, fmt.Errorf("inspect test VM broker yard: %w", err)
	}
	var instances []struct {
		Name   string            `json:"name"`
		Status string            `json:"status"`
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal(instancesPayload, &instances); err != nil {
		return brokerRuntime{}, fmt.Errorf("decode test VM broker yard: %w", err)
	}
	if len(instances) == 0 {
		return result, nil
	}
	if len(instances) != 1 || instances[0].Name != result.instance {
		return brokerRuntime{}, errors.New("test VM broker yard inventory is not canonical")
	}
	switch strings.ToUpper(instances[0].Status) {
	case "STOPPED":
		switch strings.ToLower(strings.TrimSpace(
			instances[0].Config["user.subyard.desired_power"],
		)) {
		case "running":
			// A yard stopped underneath the operator during an upgrade or
			// reboot is still an active broker allocation when its durable
			// activation intent is running. Owner migration may restore it
			// before this operation commits, so retain a stable active state.
			result.state = BrokerRuntimeActive
		case "", "stopped":
		default:
			return brokerRuntime{}, fmt.Errorf(
				"test VM broker yard has unsupported desired power %q",
				instances[0].Config["user.subyard.desired_power"],
			)
		}
		return result, nil
	case "RUNNING":
	default:
		return brokerRuntime{}, fmt.Errorf(
			"test VM broker yard is in unsupported state %q",
			instances[0].Status,
		)
	}
	service, serviceErr := runIncus(
		ctx,
		options,
		"exec",
		result.instance,
		"--project",
		result.project,
		"--",
		"systemctl",
		"is-active",
		"subyard-test-vms-broker.service",
	)
	switch strings.TrimSpace(string(service)) {
	case "active", "activating", "reloading":
		result.state = BrokerRuntimeActive
		return result, nil
	case "inactive", "failed", "deactivating", "unknown":
		if strings.EqualFold(strings.TrimSpace(
			instances[0].Config["user.subyard.desired_power"],
		), "running") {
			// Preserve the durable activation intent when the service is
			// temporarily down during an update. Commit will reconcile and
			// verify the broker against the selected release.
			result.state = BrokerRuntimeActive
			return result, nil
		}
		if yard == LegacyYard {
			loadState, loadErr := runIncus(
				ctx,
				options,
				"exec",
				result.instance,
				"--project",
				result.project,
				"--",
				"systemctl",
				"show",
				"--property=LoadState",
				"--value",
				"subyard-test-vms-broker.service",
			)
			if loadErr != nil {
				return brokerRuntime{}, fmt.Errorf(
					"inspect legacy test VM broker service: %w",
					loadErr,
				)
			}
			switch strings.TrimSpace(string(loadState)) {
			case "not-found":
				// The legacy fixed-VM backend predates the lease-broker
				// unit. A running legacy yard is its active predecessor;
				// normalize it to active so owner migration can replace it
				// before this operation refreshes the canonical broker.
				result.state = BrokerRuntimeActive
				return result, nil
			case "loaded", "masked":
			default:
				return brokerRuntime{}, fmt.Errorf(
					"legacy test VM broker service returned unsupported load state %q",
					strings.TrimSpace(string(loadState)),
				)
			}
		}
		return result, nil
	default:
		if serviceErr != nil {
			return brokerRuntime{}, fmt.Errorf("inspect test VM broker service: %w", serviceErr)
		}
		return brokerRuntime{}, fmt.Errorf(
			"test VM broker service returned unsupported state %q",
			strings.TrimSpace(string(service)),
		)
	}
}

func loadBrokerYard(options Options, yard string) (config.Loaded, error) {
	environment := environmentMap(selectedYardEnvironment(options.Environment))
	environment["SUBYARD_CONFIG_HOME"] = options.ConfigHome
	environment["SUBYARD_HOME"] = options.DataHome
	operatorHome := environment["SUBYARD_OPERATOR_HOME"]
	if operatorHome == "" {
		operatorHome = environment["HOME"]
	}
	loaded, err := config.Load(config.LoadOptions{
		RepositoryRoot: options.RepositoryRoot,
		OperatorHome:   operatorHome,
		YardName:       yard,
		Environment:    environment,
		DisablePrivate: true,
	})
	if err != nil {
		return config.Loaded{}, fmt.Errorf("load %s for broker migration: %w", yard, err)
	}
	return loaded, nil
}

func environmentMap(source []string) map[string]string {
	result := make(map[string]string, len(source))
	for _, assignment := range source {
		name, value, ok := strings.Cut(assignment, "=")
		if ok {
			result[name] = value
		}
	}
	return result
}

func fileDigest(path string) (string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}
