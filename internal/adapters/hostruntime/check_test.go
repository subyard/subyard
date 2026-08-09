package hostruntime

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

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
	remote.YardType = domain.YardRemote
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
		Environment: map[string]string{"MIN_DISK_GIB": "999999", "REC_DISK_GIB": "999999"},
		Probe: func(context.Context, HostCheck) (HostFacts, error) {
			return facts, nil
		},
	}
	if err := check.Run(context.Background(), CheckOptions{}); !errors.Is(err, ErrHostNotReady) {
		t.Fatalf("fresh yard accepted low storage: %v", err)
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

func testYard(name string, port int) domain.Context {
	return domain.Context{
		YardName: name, YardType: domain.YardLocal, SSHPort: port,
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
