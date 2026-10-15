package releaseruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func retainedRuntimeFixture(t *testing.T) (*Runtime, string, string, string, string) {
	t.Helper()
	root := testkit.TempDir(t)
	active, target := filepath.Join(root, "active"), filepath.Join(root, "target")
	for _, source := range []string{active, target} {
		profile := filepath.Join(source, "config/profiles/sample")
		if err := os.MkdirAll(profile, 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(profile, "runtime.sh"), []byte("#!/bin/sh\n"), 0700)
		testkit.WriteFile(t, filepath.Join(profile, "profile.json"), []byte(`{"schema_version":1,"default_yards":["default"],"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh"},"managed_paths":[{"root":"data","path":"service/{yard}"}]}`), 0600)
	}
	operator, data, home := filepath.Join(root, "operator"), filepath.Join(root, "data"), filepath.Join(root, "config")
	for _, path := range []string{operator, data, home, filepath.Join(data, "service")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	installed := filepath.Join(data, "service/default")
	inputs := CaptureCommandInputs([]string{"HOME=" + operator, "SUBYARD_OPERATOR_HOME=" + operator, "SUBYARD_HOME=" + data, "SUBYARD_CONFIG_HOME=" + home, "STORAGE_PATH=" + filepath.Join(root, "storage"), "HOST_BASE=" + filepath.Join(root, "host"), "RESTRICTED_DISK_PATHS=" + filepath.Join(root, "host"), "SHIFT_MODE=shift", "DEV_USER=dev", "DEV_UID=1000", "DEV_SUDO=0", "FORWARD_SSH_AGENT=0", "SSH_PORT=2222"}, nil)
	return New(Config{Inputs: inputs, ProfileRuntimeObservation: func(context.Context, string, config.Loaded, string) (ports.RuntimeObservation, error) {
		return ports.RuntimeObservation{State: ports.RuntimeStateAbsent}, nil
	}}), active, target, home, installed
}

func TestRetainedCoreGuardUsesInstalledPresenceAndPerYardPins(t *testing.T) {
	runtime, active, target, home, _ := retainedRuntimeFixture(t)
	for _, root := range []string{active, target} {
		if err := os.MkdirAll(filepath.Join(root, "config/preview"), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, "config/preview/subyard-preview"), []byte("same public helper"), 0755)
	}
	testkit.WriteFile(t, filepath.Join(active, "config/agents.env"), []byte("CCUSAGE_VERSION=20.0.30\n"), 0600)
	testkit.WriteFile(t, filepath.Join(target, "config/agents.env"), []byte("CCUSAGE_VERSION=20.0.20\n"), 0600)
	presence := ports.RuntimePresence{Actual: "absent"}
	runtime.config.InstalledRuntimePresence = func(_ context.Context, _ string, loaded config.Loaded, id string) (ports.RuntimePresence, error) {
		if id == "core-guest-runtime" {
			return presence, nil
		}
		return ports.RuntimePresence{Actual: "host-absent"}, nil
	}
	first, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil)
	if err != nil {
		t.Fatalf("absent different pins refused: %v", err)
	}
	presence.CCUsage = true
	if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err == nil {
		t.Fatal("installed changed pin lost its owner")
	}
	presence.CCUsage = false
	effects := 0
	prepared := runtime.guardRetainedRuntimeInputs(Prepared{run: func(context.Context) error { effects++; return nil }}, first, active, target, home, nil)
	presence.Actual = "changed presence metadata"
	if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) || effects != 0 {
		t.Fatalf("changed presence reached rollback effects: %v effects=%d", err, effects)
	}
	second, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil)
	if err != nil || first == second {
		t.Fatalf("absence metadata not bound for pre-effects CAS: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, "yards/named"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, "yards/named/config.env"), []byte("CCUSAGE_VERSION=21.0.0\n"), 0600)
	namedObserved := false
	runtime.config.InstalledRuntimePresence = func(_ context.Context, _ string, loaded config.Loaded, id string) (ports.RuntimePresence, error) {
		if id == "core-guest-runtime" && loaded.Context.YardName == "named" {
			namedObserved = true
			if loaded.Environment["CCUSAGE_VERSION"] != "21.0.0" {
				t.Fatalf("named effective pin ignored: %s", loaded.Environment["CCUSAGE_VERSION"])
			}
			return ports.RuntimePresence{CCUsage: true, Actual: "named-installed"}, nil
		}
		return ports.RuntimePresence{Actual: "absent"}, nil
	}
	if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err != nil {
		t.Fatalf("named installed override was compared with default: %v", err)
	}
	// Same default pins cannot hide a differing named-yard retained contract.
	testkit.WriteFile(t, filepath.Join(target, "config/agents.env"), []byte("CCUSAGE_VERSION=20.0.30\n"), 0600)
	if err := os.Remove(filepath.Join(home, "yards/named/config.env")); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ root, pin string }{{active, "21.0.0"}, {target, "22.0.0"}} {
		if err := os.MkdirAll(filepath.Join(entry.root, "private/yards"), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(entry.root, "private/yards/named.env"), []byte("CCUSAGE_VERSION="+entry.pin+"\n"), 0600)
	}
	if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err == nil || !namedObserved {
		t.Fatal("named installed differing pins bypassed authority")
	}
}

func TestRetainedCoreGuardFailsClosedOnUnavailablePresence(t *testing.T) {
	runtime, active, target, home, _ := retainedRuntimeFixture(t)
	if err := os.MkdirAll(filepath.Join(active, "config/preview"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(active, "config/preview/subyard-preview"), []byte("helper"), 0755)
	if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err == nil {
		t.Fatal("missing presence observer accepted")
	}
	runtime.config.InstalledRuntimePresence = func(context.Context, string, config.Loaded, string) (ports.RuntimePresence, error) {
		return ports.RuntimePresence{}, os.ErrPermission
	}
	if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err == nil {
		t.Fatal("unavailable presence treated as absent")
	}
}

func TestRetainedHostOnlyContractDoesNotProbeGuest(t *testing.T) {
	runtime, active, target, home, _ := retainedRuntimeFixture(t)
	for _, root := range []string{active, target} {
		if err := os.MkdirAll(filepath.Join(root, "config/preview"), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, "config/preview/subyard-preview"), []byte("same guest recipe"), 0755)
	}
	testkit.WriteFile(t, filepath.Join(active, "config/host.env"), []byte("SUBYARD_AGE_VERSION=1.3.1\n"), 0600)
	testkit.WriteFile(t, filepath.Join(target, "config/host.env"), []byte("SUBYARD_AGE_VERSION=1.3.2\n"), 0600)
	runtime.config.InstalledRuntimePresence = func(_ context.Context, _ string, _ config.Loaded, id string) (ports.RuntimePresence, error) {
		if id != "key-tools-runtime" {
			t.Fatal("host contract gained a guest prerequisite")
		}
		return ports.RuntimePresence{KeyTools: true, Actual: "host-installed"}, nil
	}
	if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, map[string]bool{"key-tools-runtime": true}); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedRuntimeOwnerLossRefusesInstalledAndDeselectedLeftovers(t *testing.T) {
	runtime, active, target, home, installed := retainedRuntimeFixture(t)
	testkit.WriteFile(t, installed, []byte("public owned service"), 0700)
	testkit.WriteFile(t, filepath.Join(target, "config/profiles/sample/profile.json"), []byte(`{"schema_version":1}`), 0600)
	for _, selection := range []string{"", "ENVIRONMENT_PROFILES=\n"} {
		testkit.WriteFile(t, filepath.Join(home, "config.env"), []byte(selection), 0600)
		if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err == nil || !strings.Contains(err.Error(), "cannot activate") {
			t.Fatalf("runtime owner loss accepted: %v", err)
		}
		payload, err := os.ReadFile(installed)
		if err != nil || string(payload) != "public owned service" {
			t.Fatal("read-only compatibility inspection mutated installation")
		}
	}
	if err := os.Remove(installed); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err != nil {
		t.Fatalf("absent unselected runtime rejected: %v", err)
	}
}

func TestRetainedRuntimeBindingRejectsFalseCapabilitiesAndBindsReplacement(t *testing.T) {
	runtime, active, target, home, installed := retainedRuntimeFixture(t)
	testkit.WriteFile(t, installed, []byte("public owned service"), 0700)
	if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, map[string]bool{"another-owner": true}); err == nil {
		t.Fatal("descriptor accepted missing compiled activation owner")
	}
	first, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, map[string]bool{"sample-runtime": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(installed); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, installed, []byte("public owned service"), 0700)
	second, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, map[string]bool{"sample-runtime": true})
	if err != nil || first == second {
		t.Fatalf("managed path replacement not bound: %v", err)
	}
}

func TestRetainedGuestOnlyProfileRequiresSelectedActivationOwner(t *testing.T) {
	runtime, active, target, home, _ := retainedRuntimeFixture(t)
	testkit.WriteFile(t, filepath.Join(active, "config/profiles/sample/profile.json"), []byte(`{"schema_version":1,"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh"}}`), 0600)
	testkit.WriteFile(t, filepath.Join(target, "config/profiles/sample/profile.json"), []byte(`{"schema_version":1}`), 0600)
	testkit.WriteFile(t, filepath.Join(home, "config.env"), []byte("ENVIRONMENT_PROFILES=sample\n"), 0600)
	if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err == nil {
		t.Fatal("selected guest-only runtime lost activation authority")
	}
	testkit.WriteFile(t, filepath.Join(home, "config.env"), []byte("ENVIRONMENT_PROFILES=\n"), 0600)
	runtime.config.ProfileRuntimeObservation = func(context.Context, string, config.Loaded, string) (ports.RuntimeObservation, error) {
		return ports.RuntimeObservation{State: ports.RuntimeStateAbsent}, nil
	}
	if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err != nil {
		t.Fatalf("unselected guest-only runtime rejected: %v", err)
	}
}

func TestRetainedMixedProfileRequiresNativeAbsenceWithoutHostReceipts(t *testing.T) {
	runtime, active, target, home, installed := retainedRuntimeFixture(t)
	testkit.WriteFile(t, filepath.Join(target, "config/profiles/sample/profile.json"), []byte(`{"schema_version":1}`), 0600)
	testkit.WriteFile(t, filepath.Join(home, "config.env"), []byte("ENVIRONMENT_PROFILES=\n"), 0600)
	if _, err := os.Stat(installed); !os.IsNotExist(err) {
		t.Fatal("fixture must have no host ownership receipt")
	}
	observation := ports.RuntimeObservation{State: ports.RuntimeStateAbsent}
	runtime.config.ProfileRuntimeObservation = func(context.Context, string, config.Loaded, string) (ports.RuntimeObservation, error) {
		return observation, nil
	}
	approved, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil)
	if err != nil {
		t.Fatal(err)
	}
	effects := 0
	prepared := runtime.guardRetainedRuntimeInputs(Prepared{run: func(context.Context) error { effects++; return nil }}, approved, active, target, home, nil)
	for _, state := range []ports.RuntimeState{ports.RuntimeStateCurrent, ports.RuntimeStateStale, ports.RuntimeStateDeferred} {
		observation.State = state
		if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err == nil {
			t.Fatalf("deselected guest leftovers bypassed absent host receipt: %s", state)
		}
		if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) || effects != 0 {
			t.Fatalf("changed mixed consumer crossed consent: %v", err)
		}
	}
	observation = ports.RuntimeObservation{State: ports.RuntimeStateCurrent, Actual: strings.Repeat("a", 64), Desired: strings.Repeat("a", 64), OwnershipAbsent: true}
	approved, err = runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil)
	if err != nil {
		t.Fatalf("current native ownership absence refused: %v", err)
	}
	prepared = runtime.guardRetainedRuntimeInputs(Prepared{run: func(context.Context) error { effects++; return nil }}, approved, active, target, home, nil)
	observation.OwnershipAbsent = false
	if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) || effects != 0 {
		t.Fatalf("current without ownership proof crossed consent: %v", err)
	}
	observation.OwnershipAbsent = true
	if err := prepared.Execute(context.Background()); err != nil || effects != 1 {
		t.Fatalf("unchanged native ownership absence refused: %v", err)
	}
}

func TestRetainedGuestOnlyLeftoversAndNativePresenceCAS(t *testing.T) {
	runtime, active, target, home, _ := retainedRuntimeFixture(t)
	testkit.WriteFile(t, filepath.Join(active, "config/profiles/sample/profile.json"), []byte(`{"schema_version":1,"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh"}}`), 0600)
	testkit.WriteFile(t, filepath.Join(target, "config/profiles/sample/profile.json"), []byte(`{"schema_version":1}`), 0600)
	testkit.WriteFile(t, filepath.Join(home, "config.env"), []byte("ENVIRONMENT_PROFILES=\n"), 0600)
	runtime.config.ProfileRuntimeObservation = nil
	if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err == nil {
		t.Fatal("unavailable native observer accepted as absent")
	}
	observation := ports.RuntimeObservation{State: ports.RuntimeStateAbsent}
	var unavailable error
	runtime.config.ProfileRuntimeObservation = func(_ context.Context, root string, loaded config.Loaded, id string) (ports.RuntimeObservation, error) {
		if root != active || loaded.Context.YardName != "default" || id != "sample-runtime" {
			t.Fatal("presence observer received wrong pinned scope")
		}
		return observation, unavailable
	}
	approved, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil)
	if err != nil {
		t.Fatal(err)
	}
	effects := 0
	prepared := runtime.guardRetainedRuntimeInputs(Prepared{run: func(context.Context) error { effects++; return nil }}, approved, active, target, home, nil)
	for _, state := range []ports.RuntimeState{ports.RuntimeStateCurrent, ports.RuntimeStateStale, ports.RuntimeStateDeferred, ""} {
		observation.State = state
		if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err == nil {
			t.Fatalf("unselected installed/unavailable runtime %s lost owner", state)
		}
		if err := prepared.Execute(context.Background()); !errors.Is(err, domain.ErrPlanStale) || effects != 0 {
			t.Fatalf("changed presence crossed approved CAS: %v effects=%d", err, effects)
		}
	}
	observation.State = ports.RuntimeStateAbsent
	unavailable = errors.New("native transport unavailable")
	if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err == nil {
		t.Fatal("transport failure accepted as absence")
	}
	unavailable = nil
	if err := prepared.Execute(context.Background()); err != nil || effects != 1 {
		t.Fatalf("unchanged absent presence refused: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, "yards/named"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(home, "yards/named/config.env"), []byte("ENVIRONMENT_PROFILES=\nSSH_PORT=2299\n"), 0600)
	named := false
	runtime.config.ProfileRuntimeObservation = func(_ context.Context, _ string, loaded config.Loaded, _ string) (ports.RuntimeObservation, error) {
		if loaded.Context.YardName == "named" {
			named = true
			return ports.RuntimeObservation{State: ports.RuntimeStateStale}, nil
		}
		return ports.RuntimeObservation{State: ports.RuntimeStateAbsent}, nil
	}
	if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err == nil || !named {
		t.Fatalf("named deselected guest leftovers were ignored: %v", err)
	}
}
