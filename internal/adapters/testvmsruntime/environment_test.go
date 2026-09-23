package testvmsruntime

import (
	"context"
	"io"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestNamedEnvironments(t *testing.T) {
	cfg := Config{CPU: 4, Memory: "4GiB", Disk: "20GiB"}
	for _, name := range []string{EnvironmentPair, EnvironmentAndroid} {
		spec, err := cfg.environmentSpecForArch(name, "amd64")
		if err != nil {
			t.Fatal(err)
		}
		memory, _ := sizeMiB(spec.Memory)
		disk, _ := sizeMiB(spec.Disk)
		if memory*spec.Count != 8*1024 || disk*spec.Count != 40*1024 {
			t.Fatalf("wrong aggregate limits: %+v", spec)
		}
		if cfg.withEnvironment(spec).guestCount() != spec.Count {
			t.Fatal("runtime composition differs from grant")
		}
	}
	if _, err := cfg.EnvironmentSpec("arbitrary-vm"); err == nil {
		t.Fatal("unknown type was accepted")
	}
	if _, err := cfg.environmentSpecForArch(EnvironmentAndroid, "arm64"); err == nil {
		t.Fatal("Android environment accepted on unsupported host architecture")
	}
	if _, err := cfg.environmentSpecForArch(EnvironmentPair, "arm64"); err != nil {
		t.Fatalf("regular pair rejected on arm64: %v", err)
	}
	if _, err := cfg.EnvironmentSpec(EnvironmentAndroid); (err == nil) != (runtime.GOARCH == "amd64") {
		t.Fatalf("host architecture resolution mismatch for %s: %v", runtime.GOARCH, err)
	}
	spec, _ := cfg.environmentSpecForArch(EnvironmentAndroid, "amd64")
	spec.Count = 2
	if err := spec.Validate(); err == nil {
		t.Fatal("wrong composition accepted")
	}
	spec.Count, spec.Lifecycle = 1, "retained"
	if err := spec.Validate(); err == nil {
		t.Fatal("wrong lifecycle accepted")
	}
	if cfg.guestCount() != 2 {
		t.Fatal("legacy lease composition changed")
	}
}

func TestNamedEnvironmentIncusResourceRequests(t *testing.T) {
	base, err := ConfigFromValues(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		count  int
		memory string
		disk   string
	}{
		{EnvironmentPair, 2, "4GiB", "20GiB"},
		{EnvironmentAndroid, 1, "8GiB", "40GiB"},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec, err := base.environmentSpecForArch(test.name, "amd64")
			if err != nil {
				t.Fatal(err)
			}
			if spec.Count != test.count || spec.CPU != 4 || spec.Memory != test.memory || spec.Disk != test.disk {
				t.Fatalf("unexpected named environment contract: %+v", spec)
			}
			cfg := base.withEnvironment(spec)
			cfg.Image = "local:" + strings.Repeat("a", 64)
			runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				switch strings.Join(args, " ") {
				case "project list --format csv -c n":
					return nil, nil, nil
				case "profile device list default --project " + cfg.Project:
					return []byte("root\neth0\n"), nil, nil
				}
				return nil, nil, nil
			}}
			rt := Runtime{Config: cfg, Runner: runner, Stdout: io.Discard, Stderr: io.Discard,
				allocation: &LeaseIdentity{SlotID: "slot-001", ResourceGeneration: 7, LeaseEpoch: 9}}
			rt.prepareDefaults()
			if err := rt.ensureProject(context.Background()); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= cfg.guestCount(); i++ {
				if err := rt.initVM(context.Background(), cfg.vm(i)); err != nil {
					t.Fatal(err)
				}
			}
			var projectCreates, rootSizes, vmInits int
			for _, call := range runner.calls {
				joined := strings.Join(call, " ")
				if strings.HasPrefix(joined, "incus project create "+cfg.Project+" ") {
					projectCreates++
					for _, setting := range []string{
						"features.images=false", "limits.instances=" + strconv.Itoa(test.count),
						"limits.virtual-machines=" + strconv.Itoa(test.count),
						"limits.cpu=" + strconv.Itoa(4*test.count),
						"limits.memory=8192MiB",
					} {
						if !strings.Contains(joined, setting) {
							t.Errorf("project create omitted %s: %s", setting, joined)
						}
					}
				}
				if joined == "incus profile device set default root size "+test.disk+" --project "+cfg.Project {
					rootSizes++
				}
				if len(call) > 1 && call[0] == "incus" && call[1] == "init" {
					vmInits++
					want := []string{"incus", "init", cfg.Image, cfg.vm(vmInits), "--vm", "--project", cfg.Project,
						"-c", "limits.cpu=4", "-c", "limits.memory=" + test.memory,
						"-c", "user.subyard.managed=" + managedMarker,
						"-c", "user.subyard.generation=7", "-c", "user.subyard.lease-epoch=9"}
					if !reflect.DeepEqual(call, want) {
						t.Errorf("VM init request = %v, want %v", call, want)
					}
				}
			}
			if projectCreates != 1 || rootSizes != 1 || vmInits != test.count {
				t.Fatalf("Incus resource requests: project=%d root=%d VM=%d, want 1/1/%d", projectCreates, rootSizes, vmInits, test.count)
			}
		})
	}
}
