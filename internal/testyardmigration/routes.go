package testyardmigration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
)

const (
	routeConsumersSchema = 1
	routeDeviceName      = "subyard-e2e-routes"
	routeGuestPath       = "/var/lib/subyard/e2e-routes"
)

type routeConsumerSnapshot struct {
	Project  string `json:"project"`
	Instance string `json:"instance"`
	Yard     string `json:"yard"`
	Mounted  bool   `json:"mounted"`
}

type routeConsumersState struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Active        bool                    `json:"active"`
	Consumers     []routeConsumerSnapshot `json:"consumers,omitempty"`
}

type managedYard struct {
	Project  string
	Instance string
	Yard     string
	Status   string
	Mounted  bool
}

// PrepareRouteConsumers snapshots every existing non-owner local yard before
// the preceding owner operation changes e2e-yard into test-yard.
func PrepareRouteConsumers(ctx context.Context, options Options) (string, error) {
	if err := validateOptions(&options); err != nil {
		return "", err
	}
	registration, err := inspectRegistration(options)
	if err != nil {
		return "", err
	}
	prepared := routeConsumersState{
		SchemaVersion: routeConsumersSchema,
		Active:        registration.state != StateAbsent,
	}
	if !prepared.Active {
		return encodeRouteConsumersState(prepared)
	}
	if _, err := inspectCurrentRoute(options); err != nil {
		return "", err
	}
	yards, err := inspectManagedYards(ctx, options)
	if err != nil {
		return "", err
	}
	for _, yard := range yards {
		if yard.Yard == LegacyYard || yard.Yard == CurrentYard {
			continue
		}
		prepared.Consumers = append(prepared.Consumers, routeConsumerSnapshot{
			Project:  yard.Project,
			Instance: yard.Instance,
			Yard:     yard.Yard,
			Mounted:  yard.Mounted,
		})
	}
	sort.Slice(prepared.Consumers, func(left, right int) bool {
		return consumerKey(prepared.Consumers[left].Project, prepared.Consumers[left].Instance) <
			consumerKey(prepared.Consumers[right].Project, prepared.Consumers[right].Instance)
	})
	return encodeRouteConsumersState(prepared)
}

// CommitRouteConsumers publishes the canonical route when needed and
// live-attaches the shared read-only route registry to every prepared consumer.
func CommitRouteConsumers(ctx context.Context, options Options, before string) error {
	if err := validateOptions(&options); err != nil {
		return err
	}
	prepared, err := decodeRouteConsumersState(before)
	if err != nil {
		return err
	}
	if !prepared.Active {
		return VerifyRouteConsumers(ctx, options, before)
	}
	yards, err := validateCommitInventory(ctx, options, prepared, false)
	if err != nil {
		return err
	}
	routeReady, err := inspectCurrentRoute(options)
	if err != nil {
		return err
	}
	if !routeReady || !hasManagedYard(yards, CurrentYard) ||
		managedYardRunning(yards, CurrentYard) {
		if err := run(
			ctx,
			options,
			CurrentYard,
			nil,
			"_migrate",
			"reconcile-test-vm-broker",
		); err != nil {
			return fmt.Errorf("publish canonical test-yard route: %w", err)
		}
	}
	if err := validateRouteSource(options); err != nil {
		return err
	}
	yards, err = validateCommitInventory(ctx, options, prepared, true)
	if err != nil {
		return err
	}
	if managedYardRunning(yards, CurrentYard) {
		if err := validateCurrentRouteIdentity(ctx, options); err != nil {
			return err
		}
	}
	for _, yard := range yards {
		if yard.Yard == LegacyYard || yard.Mounted {
			continue
		}
		if _, err := runIncus(
			ctx,
			options,
			"config",
			"device",
			"add",
			yard.Instance,
			routeDeviceName,
			"disk",
			"--project",
			yard.Project,
			"source="+routeSource(options),
			"path="+routeGuestPath,
			"readonly=true",
		); err != nil {
			return fmt.Errorf(
				"attach shared E2E routes to %s/%s: %w",
				yard.Project,
				yard.Instance,
				err,
			)
		}
	}
	if err := verifyRouteConsumers(ctx, options, true, prepared); err != nil {
		return err
	}
	fmt.Fprintln(options.Stdout, "reconciled shared E2E routes in existing local yards")
	return nil
}

// VerifyRouteConsumers checks the durable whole-host postcondition. Consumers
// created after the migration are accepted only when their own init already
// attached the exact product-owned device.
func VerifyRouteConsumers(ctx context.Context, options Options, before string) error {
	if err := validateOptions(&options); err != nil {
		return err
	}
	prepared, err := decodeRouteConsumersState(before)
	if err != nil {
		return err
	}
	return verifyRouteConsumers(ctx, options, false, prepared)
}

// VerifyRouteConsumersRollback checks the exact non-owner mount snapshot.
func VerifyRouteConsumersRollback(ctx context.Context, options Options, before string) error {
	if err := validateOptions(&options); err != nil {
		return err
	}
	prepared, err := decodeRouteConsumersState(before)
	if err != nil {
		return err
	}
	if !prepared.Active {
		return nil
	}
	yards, err := inspectManagedYards(ctx, options)
	if err != nil {
		return err
	}
	return validatePreparedSet(prepared.Consumers, nonOwnerYards(yards), false)
}

// ValidateRouteConsumersState validates the persisted typed-operation payload.
func ValidateRouteConsumersState(before string) error {
	_, err := decodeRouteConsumersState(before)
	return err
}

func verifyRouteConsumers(
	ctx context.Context,
	options Options,
	exact bool,
	prepared routeConsumersState,
) error {
	registration, err := inspectRegistration(options)
	if err != nil {
		return err
	}
	if !prepared.Active {
		if registration.state != StateAbsent {
			return fmt.Errorf(
				"route consumer migration expected no test-yard registration, found %s",
				registration.state,
			)
		}
		return nil
	}
	if registration.state != StateCurrent {
		return fmt.Errorf(
			"route consumer migration expected current registration, found %s",
			registration.state,
		)
	}
	routeReady, err := inspectCurrentRoute(options)
	if err != nil {
		return err
	}
	if !routeReady {
		return errors.New("canonical test-yard route is unavailable")
	}
	yards, err := inspectManagedYards(ctx, options)
	if err != nil {
		return err
	}
	if hasManagedYard(yards, LegacyYard) {
		return errors.New("legacy e2e-yard instance remains in the managed yard inventory")
	}
	if !hasManagedYard(yards, CurrentYard) {
		return errors.New("canonical test-yard instance is absent from the managed yard inventory")
	}
	if managedYardRunning(yards, CurrentYard) {
		if err := validateCurrentRouteIdentity(ctx, options); err != nil {
			return err
		}
	}
	if exact {
		if err := validatePreparedSet(prepared.Consumers, nonOwnerYards(yards), true); err != nil {
			return fmt.Errorf("route consumer inventory changed during commit: %w", err)
		}
	}
	for _, yard := range yards {
		if yard.Yard == LegacyYard {
			continue
		}
		if !yard.Mounted {
			return fmt.Errorf(
				"managed yard %s/%s lacks the shared E2E route mount",
				yard.Project,
				yard.Instance,
			)
		}
		if strings.EqualFold(yard.Status, "running") {
			if _, err := runIncus(
				ctx,
				options,
				"exec",
				yard.Instance,
				"--project",
				yard.Project,
				"--",
				"/bin/sh",
				"-c",
				`test -r "$1" && test -r "$2"`,
				"subyard-route-check",
				filepath.Join(routeGuestPath, CurrentYard, "current", "route.tsv"),
				filepath.Join(routeGuestPath, CurrentYard, "current", "known_hosts"),
			); err != nil {
				return fmt.Errorf(
					"verify shared E2E route inside %s/%s: %w",
					yard.Project,
					yard.Instance,
					err,
				)
			}
		}
	}
	return nil
}

func validateCommitInventory(
	ctx context.Context,
	options Options,
	prepared routeConsumersState,
	requireCurrent bool,
) ([]managedYard, error) {
	registration, err := inspectRegistration(options)
	if err != nil {
		return nil, err
	}
	if registration.state != StateCurrent {
		return nil, fmt.Errorf(
			"route consumer operation must follow owner migration; found %s registration",
			registration.state,
		)
	}
	yards, err := inspectManagedYards(ctx, options)
	if err != nil {
		return nil, err
	}
	if hasManagedYard(yards, LegacyYard) {
		return nil, errors.New("legacy e2e-yard instance remains before route reconciliation")
	}
	if requireCurrent && !hasManagedYard(yards, CurrentYard) {
		return nil, errors.New("canonical test-yard instance is absent after route publication")
	}
	if err := validatePreparedSet(prepared.Consumers, nonOwnerYards(yards), true); err != nil {
		return nil, fmt.Errorf("route consumer inventory changed before commit: %w", err)
	}
	return yards, nil
}

func managedYardRunning(yards []managedYard, name string) bool {
	for _, yard := range yards {
		if yard.Yard == name {
			return strings.EqualFold(yard.Status, "running")
		}
	}
	return false
}

func validatePreparedSet(
	prepared []routeConsumerSnapshot,
	actual map[string]managedYard,
	allowAddedMounts bool,
) error {
	if len(prepared) != len(actual) {
		return fmt.Errorf(
			"prepared %d non-owner yards, found %d",
			len(prepared),
			len(actual),
		)
	}
	for _, expected := range prepared {
		key := consumerKey(expected.Project, expected.Instance)
		yard, exists := actual[key]
		if !exists || yard.Yard != expected.Yard {
			return fmt.Errorf("prepared consumer %s is unavailable or changed", key)
		}
		if expected.Mounted != yard.Mounted &&
			!(allowAddedMounts && !expected.Mounted && yard.Mounted) {
			return fmt.Errorf("prepared consumer %s mount state changed", key)
		}
	}
	return nil
}

func inspectManagedYards(ctx context.Context, options Options) ([]managedYard, error) {
	payload, err := runIncus(ctx, options, "list", "--all-projects", "--format=json")
	if err != nil {
		return nil, fmt.Errorf("list local Incus instances: %w", err)
	}
	var instances []struct {
		Name            string                       `json:"name"`
		Project         string                       `json:"project"`
		Status          string                       `json:"status"`
		Config          map[string]string            `json:"config"`
		Devices         map[string]map[string]string `json:"devices"`
		ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
	}
	if err := json.Unmarshal(payload, &instances); err != nil {
		return nil, fmt.Errorf("decode local Incus instances: %w", err)
	}
	source := routeSource(options)
	result := make([]managedYard, 0, len(instances))
	seen := map[string]bool{}
	for _, instance := range instances {
		if instance.Config["user.subyard.managed"] != "true" {
			continue
		}
		yardName := instance.Config["user.subyard.name"]
		if yardName == "" {
			return nil, fmt.Errorf(
				"managed Incus instance %s/%s has no yard identity",
				instance.Project,
				instance.Name,
			)
		}
		expectedProject, expectedInstance := yardIdentity(yardName)
		if instance.Project != expectedProject || instance.Name != expectedInstance {
			return nil, fmt.Errorf(
				"managed yard %q has unexpected Incus identity %s/%s",
				yardName,
				instance.Project,
				instance.Name,
			)
		}
		key := consumerKey(instance.Project, instance.Name)
		if seen[key] {
			return nil, fmt.Errorf("duplicate managed Incus instance %s", key)
		}
		seen[key] = true
		device, local := instance.Devices[routeDeviceName]
		_, expanded := instance.ExpandedDevices[routeDeviceName]
		mounted := false
		switch {
		case local:
			if !routeDeviceMatches(device, source) {
				return nil, fmt.Errorf(
					"managed yard %s has a foreign or drifted %s device",
					key,
					routeDeviceName,
				)
			}
			mounted = true
		case expanded:
			return nil, fmt.Errorf(
				"managed yard %s inherits a foreign %s device",
				key,
				routeDeviceName,
			)
		}
		result = append(result, managedYard{
			Project:  instance.Project,
			Instance: instance.Name,
			Yard:     yardName,
			Status:   instance.Status,
			Mounted:  mounted,
		})
	}
	sort.Slice(result, func(left, right int) bool {
		return consumerKey(result[left].Project, result[left].Instance) <
			consumerKey(result[right].Project, result[right].Instance)
	})
	return result, nil
}

func inspectCurrentRoute(options Options) (bool, error) {
	_, ready, err := testvmsruntime.ReadPublishedRoute(
		filepath.Join(routeSource(options), CurrentYard),
	)
	return ready, err
}

func validateCurrentRouteIdentity(ctx context.Context, options Options) error {
	expected, ready, err := testvmsruntime.ReadPublishedRoute(
		filepath.Join(routeSource(options), CurrentYard),
	)
	if err != nil {
		return err
	}
	if !ready {
		return errors.New("canonical test-yard route is unavailable")
	}
	observed, err := testvmsruntime.ObserveRoute(func(arguments ...string) (string, error) {
		incusArguments := []string{
			"exec", "yard-" + CurrentYard, "--project", "subyard-" + CurrentYard, "--",
		}
		payload, err := runIncus(ctx, options, append(incusArguments, arguments...)...)
		return string(payload), err
	})
	if err != nil {
		return fmt.Errorf("inspect canonical test-yard route identity: %w", err)
	}
	if observed.Hostname != expected.Hostname {
		return errors.New("canonical test-yard route points to a stale outer-yard address")
	}
	if observed.HostKey != expected.HostKey {
		return errors.New("canonical test-yard route contains a stale host-key pin")
	}
	return nil
}

func validateRouteSource(options Options) error {
	source := routeSource(options)
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("inspect shared E2E route source: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("shared E2E route source is not a directory")
	}
	return nil
}

func encodeRouteConsumersState(state routeConsumersState) (string, error) {
	payload, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func decodeRouteConsumersState(before string) (routeConsumersState, error) {
	var state routeConsumersState
	if err := json.Unmarshal([]byte(before), &state); err != nil {
		return state, fmt.Errorf("decode route consumer state: %w", err)
	}
	if state.SchemaVersion != routeConsumersSchema {
		return state, fmt.Errorf("unsupported route consumer state schema %d", state.SchemaVersion)
	}
	if !state.Active && len(state.Consumers) != 0 {
		return state, errors.New("inactive route consumer state has consumers")
	}
	seen := map[string]bool{}
	for _, consumer := range state.Consumers {
		if consumer.Project == "" || consumer.Instance == "" || consumer.Yard == "" {
			return state, errors.New("route consumer state has an incomplete identity")
		}
		expectedProject, expectedInstance := yardIdentity(consumer.Yard)
		if consumer.Project != expectedProject || consumer.Instance != expectedInstance {
			return state, fmt.Errorf(
				"route consumer %q has an invalid Incus identity",
				consumer.Yard,
			)
		}
		if consumer.Yard == LegacyYard || consumer.Yard == CurrentYard {
			return state, errors.New("route consumer state includes an owner yard")
		}
		key := consumerKey(consumer.Project, consumer.Instance)
		if seen[key] {
			return state, fmt.Errorf("route consumer state duplicates %s", key)
		}
		seen[key] = true
	}
	return state, nil
}

func routeDeviceMatches(device map[string]string, source string) bool {
	return device["type"] == "disk" &&
		device["source"] == source &&
		device["path"] == routeGuestPath &&
		device["readonly"] == "true"
}

func routeSource(options Options) string {
	return filepath.Join(options.DataHome, "e2e", "routes")
}

func yardIdentity(yard string) (string, string) {
	if yard == "default" {
		return "subyard", "yard"
	}
	return "subyard-" + yard, "yard-" + yard
}

func consumerKey(project, instance string) string {
	return project + "/" + instance
}

func nonOwnerYards(yards []managedYard) map[string]managedYard {
	result := map[string]managedYard{}
	for _, yard := range yards {
		if yard.Yard == LegacyYard || yard.Yard == CurrentYard {
			continue
		}
		result[consumerKey(yard.Project, yard.Instance)] = yard
	}
	return result
}

func hasManagedYard(yards []managedYard, name string) bool {
	for _, yard := range yards {
		if yard.Yard == name {
			return true
		}
	}
	return false
}
