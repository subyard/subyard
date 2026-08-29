package testyardmigration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestRouteConsumersCommitAttachesExistingRunningYard(t *testing.T) {
	options, state := routeConsumerFixture(t, StateCurrent, true)
	before, err := PrepareRouteConsumers(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := CommitRouteConsumers(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(read(t, state.defaultDevice)); got != "correct" {
		t.Fatalf("default yard device state = %q", got)
	}
	calls := read(t, state.calls)
	for _, expected := range []string{
		"config device add yard subyard-e2e-routes disk --project subyard ",
		"exec yard --project subyard -- /bin/sh -c ",
		"exec yard-test-yard --project subyard-test-yard -- /bin/sh -c ",
	} {
		if !strings.Contains(calls, expected) {
			t.Fatalf("Incus calls omitted %q:\n%s", expected, calls)
		}
	}
	if err := VerifyRouteConsumers(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	if err := CommitRouteConsumers(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(
		read(t, state.calls),
		"config device add yard subyard-e2e-routes",
	); got != 1 {
		t.Fatalf("idempotent commit added the default route device %d times", got)
	}
}

func TestRouteConsumersPrepareRejectsForeignDevice(t *testing.T) {
	options, state := routeConsumerFixture(t, StateCurrent, true)
	write(t, state.defaultDevice, "wrong\n")
	before, err := PrepareRouteConsumers(context.Background(), options)
	if err == nil || before != "" || !strings.Contains(err.Error(), "foreign or drifted") {
		t.Fatalf("foreign route device preparation = %q, %v", before, err)
	}
	if calls, err := os.ReadFile(state.calls); err == nil &&
		strings.Contains(string(calls), "config device") {
		t.Fatal("foreign device reached mutation")
	}
}

func TestRouteConsumersCommitReconcilesStaleCanonicalRoute(t *testing.T) {
	options, state := routeConsumerFixture(t, StateCurrent, true)
	before, err := PrepareRouteConsumers(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	write(t, state.ownerAddress, "10.0.0.9\n")
	if err := CommitRouteConsumers(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(read(t, state.defaultDevice)); got != "correct" {
		t.Fatalf("stale route was not reconciled before consumer mutation: %q", got)
	}
	if !strings.Contains(
		read(t, state.yardCalls),
		"-Y test-yard _migrate reconcile-test-vm-broker\n",
	) {
		t.Fatal("stale canonical route did not trigger bounded broker reconcile")
	}
}

func TestRouteConsumerSnapshotSurvivesOwnerTransitionAndRejectsNewYard(t *testing.T) {
	options, state := routeConsumerFixture(t, StateLegacyDirectory, true)
	beforeLegacy, err := PrepareRouteConsumers(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	oldRegistration := filepath.Join(options.ConfigHome, "yards", LegacyYard)
	newRegistration := filepath.Join(options.ConfigHome, "yards", CurrentYard)
	if err := os.Rename(oldRegistration, newRegistration); err != nil {
		t.Fatal(err)
	}
	write(t, state.owner, "current\n")
	beforeCurrent, err := PrepareRouteConsumers(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if beforeLegacy != beforeCurrent {
		t.Fatalf(
			"owner transition changed prepared consumer snapshot:\nlegacy=%s\ncurrent=%s",
			beforeLegacy,
			beforeCurrent,
		)
	}
	write(t, state.extra, "1\n")
	err = CommitRouteConsumers(context.Background(), options, beforeLegacy)
	if err == nil || !strings.Contains(err.Error(), "inventory changed") {
		t.Fatalf("new consumer commit error = %v", err)
	}
	if got := strings.TrimSpace(read(t, state.defaultDevice)); got != "missing" {
		t.Fatalf("new consumer reached mutation: %q", got)
	}
}

func TestRouteConsumersCommitRepublishesMissingCanonicalRoute(t *testing.T) {
	options, state := routeConsumerFixture(t, StateCurrent, false)
	before, err := PrepareRouteConsumers(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := CommitRouteConsumers(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		read(t, state.yardCalls),
		"-Y test-yard _migrate reconcile-test-vm-broker\n",
	) {
		t.Fatal("missing canonical route did not trigger bounded broker reconcile")
	}
}

func TestRouteConsumersSkipGuestProbeForStoppedYard(t *testing.T) {
	options, state := routeConsumerFixture(t, StateCurrent, true)
	write(t, state.defaultStatus, "STOPPED\n")
	before, err := PrepareRouteConsumers(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := CommitRouteConsumers(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, state.calls), "exec yard --project subyard --") {
		t.Fatal("stopped consumer received an in-guest route probe")
	}
}

func TestRouteConsumersSkipOwnerGuestProbeForStoppedCanonicalYard(t *testing.T) {
	options, state := routeConsumerFixture(t, StateCurrent, true)
	write(t, state.ownerStatus, "STOPPED\n")
	before, err := PrepareRouteConsumers(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := CommitRouteConsumers(context.Background(), options, before); err != nil {
		t.Fatal(err)
	}
	calls := read(t, state.calls)
	if strings.Contains(calls, "exec yard-test-yard --project subyard-test-yard --") {
		t.Fatal("stopped canonical yard received an in-guest route identity probe")
	}
	if calls, err := os.ReadFile(state.yardCalls); err == nil && len(calls) != 0 {
		t.Fatalf("stopped canonical yard was reconciled: %s", calls)
	}
}

type routeFixtureState struct {
	calls         string
	yardCalls     string
	owner         string
	ownerStatus   string
	defaultDevice string
	defaultStatus string
	extra         string
	ownerAddress  string
}

func routeConsumerFixture(
	t *testing.T,
	registrationState State,
	publishCurrent bool,
) (Options, routeFixtureState) {
	t.Helper()
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	routeRoot := filepath.Join(dataHome, "e2e", "routes", CurrentYard)
	registrationYard := CurrentYard
	owner := "current"
	if registrationState == StateLegacyDirectory {
		registrationYard = LegacyYard
		owner = "legacy"
	}
	write(
		t,
		filepath.Join(configHome, "yards", registrationYard, "config.env"),
		"YARD_TEMPLATE=test-vms\n",
	)
	hostKey := writeRouteGeneration(t, routeRoot, publishCurrent)

	state := routeFixtureState{
		calls:         filepath.Join(root, "incus-calls"),
		yardCalls:     filepath.Join(root, "yard-calls"),
		owner:         filepath.Join(root, "owner"),
		ownerStatus:   filepath.Join(root, "owner-status"),
		defaultDevice: filepath.Join(root, "default-device"),
		defaultStatus: filepath.Join(root, "default-status"),
		extra:         filepath.Join(root, "extra"),
		ownerAddress:  filepath.Join(root, "owner-address"),
	}
	write(t, state.owner, owner+"\n")
	write(t, state.ownerStatus, "RUNNING\n")
	write(t, state.defaultDevice, "missing\n")
	write(t, state.defaultStatus, "RUNNING\n")
	write(t, state.extra, "0\n")
	write(t, state.ownerAddress, "10.0.0.2\n")

	executable := filepath.Join(root, "yard")
	write(t, executable, `#!/bin/sh
set -eu
[ "$SUBYARD_INTERNAL_MIGRATION_CHILD" = 1 ]
printf '%s\n' "$*" >> "$ROUTE_YARD_CALLS"
case "$*" in
  "-Y test-yard _migrate reconcile-test-vm-broker")
    generation="$ROUTE_ROOT/.route-reconciled"
    mkdir -p "$generation"
    printf 'subyard-e2e-route-v1\nhostname\t%s\nport\t22\nhost_key_alias\tsubyard-e2e-bastion\n' \
      "$(cat "$ROUTE_OWNER_ADDRESS")" > "$generation/route.tsv"
    cp "$ROUTE_ROOT/.route-test/known_hosts" "$generation/known_hosts"
    ln -s .route-reconciled "$ROUTE_ROOT/.current-new"
    mv -Tf "$ROUTE_ROOT/.current-new" "$ROUTE_ROOT/current"
    ;;
  *) exit 2 ;;
esac
`)
	if err := os.Chmod(executable, 0o700); err != nil {
		t.Fatal(err)
	}

	incus := filepath.Join(root, "incus")
	write(t, incus, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$ROUTE_INCUS_CALLS"
device_json() {
  case "$(cat "$1")" in
    missing) printf '{}' ;;
    correct)
      printf '{"subyard-e2e-routes":{"type":"disk","source":"%s","path":"/var/lib/subyard/e2e-routes","readonly":"true"}}' "$ROUTE_SOURCE"
      ;;
    wrong)
      printf '{"subyard-e2e-routes":{"type":"disk","source":"/foreign","path":"/var/lib/subyard/e2e-routes","readonly":"true"}}'
      ;;
    *) exit 3 ;;
  esac
}
instance_json() {
  project="$1"
  instance="$2"
  yard="$3"
  status="$4"
  devices="$5"
  printf '{"name":"%s","project":"%s","status":"%s","config":{"user.subyard.managed":"true","user.subyard.name":"%s"},"devices":%s,"expanded_devices":%s}' \
    "$instance" "$project" "$status" "$yard" "$devices" "$devices"
}
if [ "$*" = "list --all-projects --format=json" ]; then
  default_devices="$(device_json "$ROUTE_DEFAULT_DEVICE")"
  default_status="$(cat "$ROUTE_DEFAULT_STATUS")"
  owner="$(cat "$ROUTE_OWNER")"
  printf '['
  instance_json subyard yard default "$default_status" "$default_devices"
  printf ','
  if [ "$owner" = legacy ]; then
    instance_json subyard-e2e-yard yard-e2e-yard e2e-yard "$(cat "$ROUTE_OWNER_STATUS")" '{}'
  else
    current_devices="$(device_json "$ROUTE_CURRENT_DEVICE")"
    instance_json subyard-test-yard yard-test-yard test-yard "$(cat "$ROUTE_OWNER_STATUS")" "$current_devices"
  fi
  if [ "$(cat "$ROUTE_EXTRA")" = 1 ]; then
    printf ','
    instance_json subyard-demo yard-demo demo STOPPED '{}'
  fi
  printf ']\n'
  exit 0
fi
if [ "$1 $2 $3" = "config device add" ]; then
  case "$4" in
    yard) printf 'correct\n' > "$ROUTE_DEFAULT_DEVICE" ;;
    yard-test-yard) printf 'correct\n' > "$ROUTE_CURRENT_DEVICE" ;;
    yard-demo) printf 'correct\n' > "$ROUTE_EXTRA_DEVICE" ;;
    *) exit 4 ;;
  esac
  exit 0
fi
if [ "$1 $2 $3" = "config device remove" ]; then
  case "$4" in
    yard) printf 'missing\n' > "$ROUTE_DEFAULT_DEVICE" ;;
    yard-test-yard) printf 'missing\n' > "$ROUTE_CURRENT_DEVICE" ;;
    yard-demo) printf 'missing\n' > "$ROUTE_EXTRA_DEVICE" ;;
    *) exit 4 ;;
  esac
  exit 0
fi
if [ "$*" = "exec yard-test-yard --project subyard-test-yard -- ip -4 -o route show default" ]; then
  printf 'default via 10.0.0.1 dev eth0\n'
  exit 0
fi
if [ "$*" = "exec yard-test-yard --project subyard-test-yard -- ip -4 -o address show dev eth0 scope global" ]; then
  printf '2: eth0 inet %s/24 scope global eth0\n' "$(cat "$ROUTE_OWNER_ADDRESS")"
  exit 0
fi
if [ "$*" = "exec yard-test-yard --project subyard-test-yard -- cat /etc/ssh/ssh_host_ed25519_key.pub" ]; then
  printf '%s fixture\n' "$ROUTE_HOST_KEY"
  exit 0
fi
if [ "$1" = exec ]; then
  exit 0
fi
exit 2
`)
	if err := os.Chmod(incus, 0o700); err != nil {
		t.Fatal(err)
	}
	currentDevice := filepath.Join(root, "current-device")
	extraDevice := filepath.Join(root, "extra-device")
	write(t, currentDevice, "correct\n")
	write(t, extraDevice, "missing\n")
	environment := append(
		os.Environ(),
		"ROUTE_INCUS_CALLS="+state.calls,
		"ROUTE_YARD_CALLS="+state.yardCalls,
		"ROUTE_OWNER="+state.owner,
		"ROUTE_OWNER_STATUS="+state.ownerStatus,
		"ROUTE_DEFAULT_DEVICE="+state.defaultDevice,
		"ROUTE_DEFAULT_STATUS="+state.defaultStatus,
		"ROUTE_CURRENT_DEVICE="+currentDevice,
		"ROUTE_EXTRA="+state.extra,
		"ROUTE_EXTRA_DEVICE="+extraDevice,
		"ROUTE_OWNER_ADDRESS="+state.ownerAddress,
		"ROUTE_HOST_KEY="+hostKey,
		"ROUTE_SOURCE="+filepath.Join(dataHome, "e2e", "routes"),
		"ROUTE_ROOT="+routeRoot,
	)
	return Options{
		Executable:  executable,
		Incus:       incus,
		ConfigHome:  configHome,
		DataHome:    dataHome,
		Environment: environment,
	}, state
}

func writeRouteGeneration(t *testing.T, root string, publishCurrent bool) string {
	t.Helper()
	generation := filepath.Join(root, ".route-test")
	write(
		t,
		filepath.Join(generation, "route.tsv"),
		"subyard-e2e-route-v1\nhostname\t10.0.0.2\nport\t22\n"+
			"host_key_alias\tsubyard-e2e-bastion\n",
	)
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := ssh.NewPublicKey(privateKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	hostKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey)))
	write(
		t,
		filepath.Join(generation, "known_hosts"),
		"subyard-e2e-bastion "+hostKey+"\n",
	)
	if publishCurrent {
		if err := os.Symlink(".route-test", filepath.Join(root, "current")); err != nil {
			t.Fatal(err)
		}
	}
	return hostKey
}
