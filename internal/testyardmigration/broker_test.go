package testyardmigration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrokerRuntimeOperationSkipsAnAlreadyCurrentActiveBroker(t *testing.T) {
	options, state := brokerRuntimeFixture(t, "RUNNING", "active")
	var output strings.Builder
	options.Stdout = &output
	expected, err := fileDigest(filepath.Join(options.RepositoryRoot, "bin", "yard-engine"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, state.installedHash, expected+"\n")

	before, _, err := PrepareBrokerRuntimeTarget(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBrokerRuntime(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	calls := read(t, state.yardCalls)
	if strings.Contains(calls, "-Y test-yard _migrate reconcile-test-vm-broker\n") {
		t.Fatalf("already current broker was initialized twice:\n%s", calls)
	}
	if !strings.Contains(calls, "-Y test-yard test-vms status\n") {
		t.Fatalf("already current broker skipped verification:\n%s", calls)
	}
	if output.Len() != 0 {
		t.Fatalf("broker verification leaked internal status: %q", output.String())
	}
}

func TestBrokerRuntimeVerificationIncludesHostSinkForCurrentRelease(t *testing.T) {
	options, state := brokerRuntimeFixture(t, "RUNNING", "active")
	engine := read(t, filepath.Join(options.RepositoryRoot, "bin", "yard-engine"))
	expected, err := fileDigest(filepath.Join(options.RepositoryRoot, "bin", "yard-engine"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, state.installedHash, expected+"\n")
	write(
		t,
		filepath.Join(options.RepositoryRoot, "scripts", "install-test-vms-host-sink.sh"),
		"#!/bin/sh\n",
	)
	sink := filepath.Join(filepath.Dir(options.RepositoryRoot), "test-vms-host-sink")
	write(t, sink, engine)
	options.Environment = append(
		options.Environment,
		"SUBYARD_TEST_VMS_SINK_PATH="+sink,
	)
	if err := VerifyBrokerRuntime(
		context.Background(),
		options,
		BrokerRuntimeActive,
	); err != nil {
		t.Fatal(err)
	}
	write(t, sink, "stale sink\n")
	if err := VerifyBrokerRuntime(
		context.Background(),
		options,
		BrokerRuntimeActive,
	); err == nil {
		t.Fatal("stale host sink passed broker runtime verification")
	}
}

func TestBrokerRuntimeOperationLeavesStoppedBrokerUntouched(t *testing.T) {
	options, state := brokerRuntimeFixture(t, "STOPPED", "inactive")
	original := strings.Repeat("1", 64) + "\n"
	write(t, state.installedHash, original)

	before, _, err := PrepareBrokerRuntimeTarget(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if before != BrokerRuntimeInactive {
		t.Fatalf("prepared broker state = %q", before)
	}
	if err := VerifyBrokerRuntime(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	if actual := read(t, state.installedHash); actual != original {
		t.Fatal("inactive broker engine was changed")
	}
	if _, err := os.Stat(state.yardCalls); !os.IsNotExist(err) {
		t.Fatalf("inactive broker invoked yard: %v", err)
	}
}

func TestBrokerRuntimeOperationRetainsDesiredRunningAcrossOwnerMigration(t *testing.T) {
	for _, yard := range []string{LegacyYard, CurrentYard} {
		t.Run(yard, func(t *testing.T) {
			options, _ := brokerRuntimeFixtureForYardWithDesiredPower(
				t,
				yard,
				"STOPPED",
				"inactive",
				"loaded",
				"running",
			)

			before, _, err := PrepareBrokerRuntimeTarget(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			if before != BrokerRuntimeActive {
				t.Fatalf(
					"stopped desired-running broker state = %q, want %q",
					before,
					BrokerRuntimeActive,
				)
			}
		})
	}
}

func TestBrokerRuntimeOperationReloadsTheSelectedYardContext(t *testing.T) {
	options, _ := brokerRuntimeFixtureForYard(
		t,
		CurrentYard,
		"RUNNING",
		"active",
		"loaded",
	)
	options.Environment = withEnvironment(
		options.Environment,
		"SUBYARD_CONFIG_LOADED",
		"1",
	)
	options.Environment = withEnvironment(
		options.Environment,
		"SUBYARD_ENGINE_CONTEXT",
		"1",
	)
	options.Environment = withEnvironment(
		options.Environment,
		"INCUS_PROJECT",
		"subyard-"+LegacyYard,
	)
	options.Environment = withEnvironment(
		options.Environment,
		"YARD_INSTANCE_NAME",
		"yard-"+LegacyYard,
	)

	before, _, err := PrepareBrokerRuntimeTarget(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if before != BrokerRuntimeActive {
		t.Fatalf("reloaded broker state = %q, want %q", before, BrokerRuntimeActive)
	}
}

func TestBrokerRuntimeOperationAcceptsSourceUpgradeRegistrationPermissions(t *testing.T) {
	options, _ := brokerRuntimeFixture(t, "RUNNING", "active")
	directory := filepath.Join(options.ConfigHome, "yards", CurrentYard)
	registration := filepath.Join(directory, "config.env")
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(registration, 0o640); err != nil {
		t.Fatal(err)
	}

	state, yard, err := PrepareBrokerRuntimeTarget(context.Background(), options)
	if err != nil || state != BrokerRuntimeActive || yard != CurrentYard {
		t.Fatalf("protected source-upgrade registration = %q/%q, err=%v",
			state, yard, err)
	}
}

func TestBrokerRuntimeOperationRetainsDesiredRunningWhenServiceStops(t *testing.T) {
	options, _ := brokerRuntimeFixtureForYardWithDesiredPower(
		t,
		CurrentYard,
		"RUNNING",
		"inactive",
		"loaded",
		"running",
	)

	before, _, err := PrepareBrokerRuntimeTarget(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if before != BrokerRuntimeActive {
		t.Fatalf(
			"desired-running broker state = %q, want %q",
			before,
			BrokerRuntimeActive,
		)
	}
}

func TestBrokerRuntimeOperationRejectsUnsupportedDesiredPower(t *testing.T) {
	options, _ := brokerRuntimeFixtureForYardWithDesiredPower(
		t,
		CurrentYard,
		"STOPPED",
		"inactive",
		"loaded",
		"paused",
	)

	_, _, err := PrepareBrokerRuntimeTarget(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "unsupported desired power") {
		t.Fatalf("unsupported desired power error = %v", err)
	}
}

func TestBrokerRuntimeOperationNormalizesRunningLegacyBackend(t *testing.T) {
	options, _ := brokerRuntimeFixtureForYard(
		t,
		LegacyYard,
		"RUNNING",
		"inactive",
		"not-found",
	)

	before, _, err := PrepareBrokerRuntimeTarget(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if before != BrokerRuntimeActive {
		t.Fatalf("running legacy backend state = %q, want %q", before, BrokerRuntimeActive)
	}
}

func TestBrokerRuntimeOperationAdoptsCanonicalBackendAfterSourceRecovery(t *testing.T) {
	options, _ := brokerRuntimeFixtureForYard(
		t,
		CurrentYard,
		"RUNNING",
		"active",
		"loaded",
	)
	current := filepath.Join(options.ConfigHome, "yards", CurrentYard, "config.env")
	legacy := filepath.Join(options.ConfigHome, "yards", LegacyYard, "config.env")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(current, legacy); err != nil {
		t.Fatal(err)
	}

	before, _, err := PrepareBrokerRuntimeTarget(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if before != BrokerRuntimeActive {
		t.Fatalf("adopted canonical broker state = %q, want %q", before, BrokerRuntimeActive)
	}
}

func TestBrokerRuntimeOperationKeepsStoppedLegacyServiceInactive(t *testing.T) {
	options, _ := brokerRuntimeFixtureForYard(
		t,
		LegacyYard,
		"RUNNING",
		"inactive",
		"loaded",
	)

	before, _, err := PrepareBrokerRuntimeTarget(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if before != BrokerRuntimeInactive {
		t.Fatalf("stopped legacy broker state = %q, want %q", before, BrokerRuntimeInactive)
	}
}

func TestBrokerRuntimeOperationTreatsActivatingServiceAsActive(t *testing.T) {
	options, _ := brokerRuntimeFixture(t, "RUNNING", "activating")

	before, _, err := PrepareBrokerRuntimeTarget(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if before != BrokerRuntimeActive {
		t.Fatalf("activating broker state = %q, want %q", before, BrokerRuntimeActive)
	}
}

func TestBrokerRuntimeOperationIsNoopWithoutRegistration(t *testing.T) {
	root := t.TempDir()
	options := Options{
		Executable:     filepath.Join(root, "must-not-run"),
		RepositoryRoot: brokerRepository(t, filepath.Join(root, "release")),
		Incus:          filepath.Join(root, "must-not-run-incus"),
		ConfigHome:     filepath.Join(root, "config"),
		DataHome:       filepath.Join(root, "data"),
		Environment: append(os.Environ(),
			"HOME="+root,
			"SUBYARD_OPERATOR_HOME="+root,
		),
	}
	before, _, err := PrepareBrokerRuntimeTarget(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if before != BrokerRuntimeAbsent {
		t.Fatalf("prepared broker state = %q", before)
	}
	if err := VerifyBrokerRuntime(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
}

type brokerFixtureState struct {
	installedHash string
	yardCalls     string
}

func brokerRuntimeFixture(
	t *testing.T,
	yardState string,
	serviceState string,
) (Options, brokerFixtureState) {
	return brokerRuntimeFixtureForYard(
		t,
		CurrentYard,
		yardState,
		serviceState,
		"loaded",
	)
}

func brokerRuntimeFixtureForYard(
	t *testing.T,
	yard string,
	yardState string,
	serviceState string,
	serviceLoadState string,
) (Options, brokerFixtureState) {
	return brokerRuntimeFixtureForYardWithDesiredPower(
		t,
		yard,
		yardState,
		serviceState,
		serviceLoadState,
		"stopped",
	)
}

func brokerRuntimeFixtureForYardWithDesiredPower(
	t *testing.T,
	yard string,
	yardState string,
	serviceState string,
	serviceLoadState string,
	desiredPower string,
) (Options, brokerFixtureState) {
	t.Helper()
	root := t.TempDir()
	repository := brokerRepository(t, filepath.Join(root, "candidate"))
	executable := filepath.Join(repository, "bin", "yard-engine")
	writeBrokerExecutable(t, executable)
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	write(
		t,
		filepath.Join(configHome, "yards", yard, "config.env"),
		"YARD_TEMPLATE=test-vms\n",
	)
	state := brokerFixtureState{
		installedHash: filepath.Join(root, "installed-hash"),
		yardCalls:     filepath.Join(root, "yard-calls"),
	}
	incus := filepath.Join(root, "incus")
	write(t, incus, `#!/bin/sh
set -eu
case "$*" in
  "project list --format=json")
    printf '[{"name":"%s"}]\n' "$BROKER_PROJECT"
    ;;
  "list $BROKER_INSTANCE --project $BROKER_PROJECT --format=json")
    printf '[{"name":"%s","status":"%s","config":{"user.subyard.desired_power":"%s"}}]\n' \
      "$BROKER_INSTANCE" "$BROKER_YARD_STATE" "$BROKER_DESIRED_POWER"
    ;;
  "exec $BROKER_INSTANCE --project $BROKER_PROJECT -- systemctl is-active subyard-test-vms-broker.service")
    printf '%s\n' "$BROKER_SERVICE_STATE"
    [ "$BROKER_SERVICE_STATE" = active ]
    ;;
  "exec $BROKER_INSTANCE --project $BROKER_PROJECT -- systemctl show --property=LoadState --value subyard-test-vms-broker.service")
    printf '%s\n' "$BROKER_SERVICE_LOAD_STATE"
    ;;
  "exec $BROKER_INSTANCE --project $BROKER_PROJECT -- sha256sum /usr/local/libexec/subyard/test-vms-inner")
    printf '%s  /usr/local/libexec/subyard/test-vms-inner\n' \
      "$(cat "$BROKER_INSTALLED_HASH")"
    ;;
  *)
    printf 'unexpected incus call: %s\n' "$*" >&2
    exit 2
    ;;
esac
`)
	if err := os.Chmod(incus, 0o700); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(),
		"HOME="+root,
		"SUBYARD_OPERATOR_HOME="+root,
		"SUBYARD_CONFIG_HOME="+configHome,
		"SUBYARD_HOME="+dataHome,
		"BROKER_YARD_STATE="+yardState,
		"BROKER_SERVICE_STATE="+serviceState,
		"BROKER_SERVICE_LOAD_STATE="+serviceLoadState,
		"BROKER_DESIRED_POWER="+desiredPower,
		"BROKER_YARD="+yard,
		"BROKER_PROJECT=subyard-"+yard,
		"BROKER_INSTANCE=yard-"+yard,
		"BROKER_INSTALLED_HASH="+state.installedHash,
		"BROKER_YARD_CALLS="+state.yardCalls,
	)
	return Options{
		Executable: executable, RepositoryRoot: repository,
		Incus: incus, ConfigHome: configHome, DataHome: dataHome,
		Environment: environment,
	}, state
}

func writeBrokerExecutable(t *testing.T, path string) {
	t.Helper()
	payload := `#!/bin/sh
set -eu
printf '%s %s\n' "$0" "$*" >> "$BROKER_YARD_CALLS"
if [ "$*" = "-Y $BROKER_YARD _migrate reconcile-test-vm-broker" ]; then
  if [ "${BROKER_SKIP_INSTALL:-0}" != 1 ]; then
    sha256sum "$0" | awk '{print $1}' > "$BROKER_INSTALLED_HASH"
  fi
  exit "${BROKER_INIT_RC:-0}"
fi
if [ "$*" = "-Y $BROKER_YARD test-vms status" ]; then
  printf '{"schema_version":1,"status":"ok"}\n'
fi
`
	if current, err := os.ReadFile(path); err == nil {
		payload += "# revision " + strings.Repeat("x", len(current)%17+1) + "\n"
	}
	write(t, path, payload)
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func brokerRepository(t *testing.T, root string) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot, err := filepath.Abs(filepath.Join(workingDirectory, "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		"incus.project.env",
		"subyard.env",
		"host.env",
		"ports.env",
		filepath.Join("yards", "profiles", "test-vms.env"),
	} {
		payload, err := os.ReadFile(filepath.Join(sourceRoot, "config", relative))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(root, "config", relative), string(payload))
	}
	return root
}
