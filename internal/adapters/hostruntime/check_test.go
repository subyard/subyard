package hostruntime

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

// The check --host preflight keeps its rendered bytes; inspection shares its facts.
func TestHostCheckPreflightGoldenAndInspection(t *testing.T) {
	const ready = `Subyard host check

OS:
  [ ok ] Test OS (kernel 1.0)

CPU virtualization:
  [ ok ] hardware virtualization flags present (vmx)

KVM device:
  [ ok ] /dev/kvm present

Resources:
  [ ok ] 4 CPU(s)
  [ ok ] 8.0 GiB RAM

Storage (/storage):
  [ ok ] 100 GiB free on / (fs: test)

Existing tools:
  [ ok ] incus present (6.0.6)
  [ ok ] no host Docker (expected; Docker lives inside the yard)

Yard SSH port:
  [ ok ] SSH_PORT 2323 is unique across configured yards
  [ ok ] host loopback port 2323 is free

Host is ready.
`
	for _, failed := range []bool{false, true} {
		facts, want := readyFacts(), ready
		if failed {
			facts.KVM, facts.StorageFree, facts.PortListening = false, 0, true
			for _, replacement := range [][2]string{
				{"[ ok ] /dev/kvm present", "[warn] /dev/kvm missing — needed for hardware-accelerated virtualization"},
				{"[ ok ] 100 GiB free on / (fs: test)", "[fail] only 0 GiB free on /; need >= 2 GiB (fs: test)"},
				{"[ ok ] host loopback port 2323 is free", "[warn] host port 2323 is already listening (another service, or this yard is running)"},
				{"Host is ready.", "Host is not ready: 1 hard requirement(s) failed, 2 warning(s)."},
			} {
				want = strings.Replace(want, replacement[0], replacement[1], 1)
			}
		}
		var output bytes.Buffer
		check := HostCheck{Yard: testYard("alpha", 2323), Output: &output,
			Probe: func(context.Context, HostCheck) (HostFacts, error) { return facts, nil }}
		if err := check.Run(context.Background(), CheckOptions{}); errors.Is(err, ErrHostNotReady) != failed || output.String() != want {
			t.Fatalf("check --host bytes changed: err=%v\ngot:  %q\nwant: %q", err, output.String(), want)
		}
		output.Reset()
		observed := check.Observe(context.Background(), true)
		var expected []domain.Inspected
		if failed {
			expected = []domain.Inspected{
				{Key: "preflight.kvm", State: domain.InspectedNotice, Resource: "KVM device", Message: "/dev/kvm missing — needed for hardware-accelerated virtualization", Next: "yard check --host"},
				{Key: "preflight.storage", State: domain.InspectedBlocked, Resource: "Storage (/storage)", Message: "only 0 GiB free on /; need >= 2 GiB (fs: test)", Next: "yard check --host"},
			}
		}
		if output.Len() != 0 || !reflect.DeepEqual(observed, expected) {
			t.Fatalf("inspection rendered or changed the preflight: %#v output=%q", observed, output.String())
		}
		if failed && len(check.Observe(context.Background(), false)) != 1 {
			t.Fatal("unconfigured inspection kept yard storage facts")
		}
	}
	failure := errors.New("probe unavailable")
	check := HostCheck{Probe: func(context.Context, HostCheck) (HostFacts, error) { return HostFacts{}, failure }}
	if observed := check.Observe(context.Background(), true); len(observed) != 1 || observed[0].State != domain.InspectedUnknown {
		t.Fatalf("failed facts read: %#v", observed)
	}
}

func TestHostCheckRejectsStrictPortCollisionWithoutHostAccess(t *testing.T) {
	var output bytes.Buffer
	check := HostCheck{
		Yard:   testYard("alpha", 2323),
		Yards:  []domain.Context{testYard("default", 2222), testYard("beta", 2323)},
		Output: &output,
		Probe: func(context.Context, HostCheck) (HostFacts, error) {
			return readyFacts(), nil
		},
	}
	err := check.Run(context.Background(), CheckOptions{Strict: true})
	if !errors.Is(err, ErrHostNotReady) {
		t.Fatalf("expected host readiness failure, got %v", err)
	}
	if !strings.Contains(output.String(), "also used by yard(s): beta") {
		t.Fatalf("missing collision diagnostic:\n%s", output.String())
	}
}

func TestHostCheckRejectsListeningPortForFreshStrictInit(t *testing.T) {
	facts := readyFacts()
	facts.PortListening = true
	var output bytes.Buffer
	check := HostCheck{
		Yard: testYard("alpha", 2323), Yards: []domain.Context{testYard("alpha", 2323)},
		Output: &output,
		Probe: func(context.Context, HostCheck) (HostFacts, error) {
			return facts, nil
		},
	}
	if err := check.Run(context.Background(), CheckOptions{Strict: true}); !errors.Is(err, ErrHostNotReady) {
		t.Fatalf("fresh strict init accepted a listening port: %v", err)
	}
	if err := check.Run(context.Background(), CheckOptions{Strict: true, BasePresent: true}); err != nil {
		t.Fatalf("existing yard could not reuse its listening port: %v", err)
	}
	if !strings.Contains(output.String(), "host port 2323 is already listening") {
		t.Fatalf("missing listening-port diagnostic:\n%s", output.String())
	}
}

func TestHostCheckIgnoresRemotePortAndWarnsOutsideStrictMode(t *testing.T) {
	var output bytes.Buffer
	remote := testYard("remote", 2323)
	remote.AccessKind = domain.AccessRemote
	check := HostCheck{
		Yard: testYard("alpha", 2323),
		Yards: []domain.Context{
			remote,
			testYard("beta", 2323),
		},
		Output: &output,
		Probe: func(context.Context, HostCheck) (HostFacts, error) {
			return readyFacts(), nil
		},
	}
	if err := check.Run(context.Background(), CheckOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "remote") || !strings.Contains(output.String(), "[warn]") {
		t.Fatalf("unexpected collision output:\n%s", output.String())
	}
}

func TestHostCheckRequiresNestedDevices(t *testing.T) {
	var output bytes.Buffer
	yard := testYard("alpha", 2323)
	yard.NestedE2EVMs = true
	facts := readyFacts()
	delete(facts.NestedDevices, "/dev/vsock")
	check := HostCheck{
		Yard: yard, Yards: []domain.Context{yard}, Output: &output,
		Probe: func(context.Context, HostCheck) (HostFacts, error) {
			return facts, nil
		},
	}
	if err := check.Run(context.Background(), CheckOptions{}); !errors.Is(err, ErrHostNotReady) {
		t.Fatalf("expected nested-device failure, got %v", err)
	}
}

func TestHostCheckUsesLowerRepairFloorForExistingYard(t *testing.T) {
	facts := readyFacts()
	facts.StorageFree = 100 << 30
	check := HostCheck{
		Yard: testYard("alpha", 2323), Yards: []domain.Context{testYard("alpha", 2323)},
		Environment: map[string]string{"MIN_DISK_GIB": "999999"},
		Probe: func(context.Context, HostCheck) (HostFacts, error) {
			return facts, nil
		},
	}
	if err := check.Run(context.Background(), CheckOptions{}); !errors.Is(err, ErrHostNotReady) {
		t.Fatalf("fresh yard accepted low storage: %v", err)
	} else if !strings.Contains(err.Error(), "Storage (/storage): only 100 GiB free on /; need >= 999999 GiB") {
		t.Fatalf("refusal does not name the failed requirement: %v", err)
	}
	var output bytes.Buffer
	check.Output = &output
	if err := check.Run(context.Background(), CheckOptions{BasePresent: true}); err != nil {
		t.Fatalf("existing yard could not use repair floor: %v", err)
	}
	if !strings.Contains(output.String(), "enough to repair this managed yard") {
		t.Fatalf("missing repair-floor diagnostic:\n%s", output.String())
	}
}

func TestHostCheckDoesNotRecommendStorageCapacity(t *testing.T) {
	facts := readyFacts()
	facts.StorageFree = 32 << 30
	var output bytes.Buffer
	check := HostCheck{
		Yard: testYard("alpha", 2323), Yards: []domain.Context{testYard("alpha", 2323)},
		Output: &output,
		Probe: func(context.Context, HostCheck) (HostFacts, error) {
			return facts, nil
		},
	}
	if err := check.Run(context.Background(), CheckOptions{}); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "[ ok ] 32 GiB free on / (fs: test)") {
		t.Fatalf("storage fact was not reported as ready:\n%s", got)
	}
	if strings.Contains(got, "50 GiB") || strings.Contains(got, "profile wants") {
		t.Fatalf("storage recommendation leaked into host check:\n%s", got)
	}
}

func testYard(name string, port int) domain.Context {
	return domain.Context{
		YardName: name, AccessKind: domain.AccessLocal, SSHPort: port,
		Paths: domain.RuntimePaths{StoragePath: "/storage"},
	}
}

func readyFacts() HostFacts {
	return HostFacts{
		OSName: "Test OS", Kernel: "1.0", CPUFlags: []string{"vmx"}, KVM: true,
		NestedDevices: map[string]bool{
			"/dev/kvm": true, "/dev/vsock": true, "/dev/vhost-vsock": true, "/dev/net/tun": true,
		},
		CPUs: 4, MemoryBytes: 8 << 30, StoragePath: "/", StorageFree: 100 << 30,
		StorageFS: "test", StorageKnown: true, IncusPresent: true, IncusVersion: "6.0.6",
	}
}

func TestHostCheckRemindsOfThePendingNetworkManagerReboot(t *testing.T) {
	var output bytes.Buffer
	check := HostCheck{
		Yard: testYard("alpha", 2323), Output: &output,
		Probe: func(context.Context, HostCheck) (HostFacts, error) {
			facts := readyFacts()
			facts.NetworkManagerRebootPending = true
			return facts, nil
		},
	}
	if err := check.Run(context.Background(), CheckOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), NetworkManagerRebootReminder) {
		t.Fatalf("check omitted the reboot reminder:\n%s", output.String())
	}
}
