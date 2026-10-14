package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWaitAgentRequiresCompletedCloudInit(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		err          error
		ready        bool
	}{
		{"done", `{"status":"done"}`, nil, true},
		{"disabled exit zero", `{"status":"disabled","boot_status_code":"disabled-by-generator","errors":[],"recoverable_errors":{}}`, nil, false},
		{"missing", `{}`, nil, false},
		{"malformed", `status: done`, nil, false},
		{"unexpected", `{"status":"running"}`, nil, false},
		{"native error", `{"status":"done"}`, &CommandError{ExitCode: 1, Message: "private config"}, false},
		{"degraded", `{"status":"done","extended_status":"degraded done"}`, &CommandError{ExitCode: 2, Message: "private config"}, false},
		{"degraded exit zero", `{"status":"done","extended_status":"degraded done"}`, nil, false},
		{"canceled", `{"status":"done"}`, context.Canceled, false},
		{"deadline", `{"status":"done"}`, context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				if strings.Contains(strings.Join(args, " "), "cloud-init status") {
					return []byte(tc.output), nil, tc.err
				}
				return nil, nil, nil
			}}
			rt := &Runtime{Config: fixtureConfig(t), Runner: runner, Stdout: io.Discard, Now: time.Now}
			err := rt.waitAgent(context.Background(), "e2e-vm-1")
			if (err == nil) != tc.ready {
				t.Fatalf("readiness error = %v, want ready=%v", err, tc.ready)
			}
			if err != nil && strings.Contains(err.Error(), "private config") {
				t.Fatal("readiness disclosed command output")
			}
			if errors.Is(tc.err, context.Canceled) && !errors.Is(err, context.Canceled) {
				t.Fatal("lost native cancellation")
			}
			if errors.Is(tc.err, context.DeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("lost native deadline")
			}
			var native *CommandError
			if errors.As(tc.err, &native) {
				var got *CommandError
				if !errors.As(err, &got) || got.ExitCode != native.ExitCode {
					t.Fatal("lost native failure exit code")
				}
			}
			if err == nil {
				return
			}
			if strings.Contains(callsText(runner.calls), "apt-get") {
				t.Fatal("rejected initialization ran APT")
			}
		})
	}
}

func TestCloudInitDiskFailsClosedAndPreservesDevices(t *testing.T) {
	want := map[string]string{"type": "disk", "source": "cloud-init:config"}
	for _, tc := range []struct {
		name                                  string
		local, expanded                       map[string]map[string]string
		marker                                string
		invalid, addFails, noEffect, mismatch bool
		adds                                  int
	}{
		{name: "missing", local: map[string]map[string]string{"data": {"type": "disk", "source": "unrelated"}}, expanded: map[string]map[string]string{"data": {"type": "disk", "source": "unrelated"}}, marker: managedMarker, adds: 1},
		{name: "idempotent", local: map[string]map[string]string{cloudInitDevice: want}, expanded: map[string]map[string]string{cloudInitDevice: want}, marker: managedMarker},
		{name: "foreign", local: map[string]map[string]string{}, expanded: map[string]map[string]string{}, marker: "foreign", invalid: true},
		{name: "inherited", local: map[string]map[string]string{}, expanded: map[string]map[string]string{cloudInitDevice: want}, marker: managedMarker, invalid: true},
		{name: "drift", local: map[string]map[string]string{cloudInitDevice: {"type": "disk", "source": "unrelated"}}, expanded: map[string]map[string]string{cloudInitDevice: {"type": "disk", "source": "unrelated"}}, marker: managedMarker, invalid: true},
		{name: "duplicate", local: map[string]map[string]string{}, expanded: map[string]map[string]string{"other": want}, marker: managedMarker, invalid: true},
		{name: "incomplete", marker: managedMarker, invalid: true},
		{name: "allocation mismatch", local: map[string]map[string]string{}, expanded: map[string]map[string]string{}, marker: managedMarker, invalid: true, mismatch: true},
		{name: "add failure", local: map[string]map[string]string{}, expanded: map[string]map[string]string{}, marker: managedMarker, invalid: true, addFails: true, adds: 1},
		{name: "unverified add", local: map[string]map[string]string{}, expanded: map[string]map[string]string{}, marker: managedMarker, invalid: true, noEffect: true, adds: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adds := 0
			before, _ := json.Marshal(tc.local["data"])
			runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				if args[0] == "query" {
					body, err := json.Marshal(map[string]any{"type": "virtual-machine", "config": map[string]string{"user.subyard.managed": tc.marker}, "devices": tc.local, "expanded_devices": tc.expanded})
					return body, nil, err
				}
				if strings.HasPrefix(strings.Join(args, " "), "config device add ") {
					adds++
					if tc.addFails {
						return nil, nil, errors.New("private config")
					}
					if !tc.noEffect {
						tc.local[cloudInitDevice], tc.expanded[cloudInitDevice] = want, want
					}
					return nil, nil, nil
				}
				t.Fatalf("unexpected device mutation: %v", args)
				return nil, nil, nil
			}}
			rt := &Runtime{Config: fixtureConfig(t), Runner: runner}
			if tc.mismatch {
				rt.allocation = &LeaseIdentity{ResourceGeneration: 7, LeaseEpoch: 9}
			}
			err := rt.ensureCloudInitDisk(context.Background(), rt.Config.vm(1))
			if (err != nil) != tc.invalid || adds != tc.adds {
				t.Fatalf("error=%v adds=%d, want invalid=%v adds=%d", err, adds, tc.invalid, tc.adds)
			}
			if err != nil && strings.Contains(err.Error(), "private config") {
				t.Fatal("device failure disclosed command output")
			}
			after, _ := json.Marshal(tc.local["data"])
			if string(before) != string(after) {
				t.Fatal("unrelated device changed")
			}
			if !tc.invalid {
				if err := rt.ensureCloudInitDisk(context.Background(), rt.Config.vm(1)); err != nil || adds != tc.adds {
					t.Fatalf("repeat error=%v adds=%d", err, adds)
				}
			}
		})
	}
}

func TestCloudInitDiskPreservesCommandFailureIdentity(t *testing.T) {
	customCause := errors.New("operation canceled by caller")
	native := &CommandError{Name: "private command", Args: []string{"private argument"},
		ExitCode: 7, Duration: time.Second, Message: "private config"}
	for _, tc := range []struct {
		name  string
		call  int
		err   error
		cause error
	}{
		{"inventory custom cause", 1, native, customCause},
		{"attachment custom cause", 2, native, customCause},
		{"verification custom cause", 3, native, customCause},
		{"inventory canceled", 1, fmt.Errorf("native cancellation: %w", context.Canceled), nil},
		{"attachment deadline", 2, fmt.Errorf("native deadline: %w", context.DeadlineExceeded), nil},
		{"inventory native failure", 1, native, nil},
		{"attachment native failure", 2, native, nil},
		{"verification native failure", 3, native, nil},
		{"inventory unknown failure", 1, errors.New("private config"), nil},
		{"verification unknown failure", 3, errors.New("private config"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			calls := 0
			runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				calls++
				if calls == tc.call {
					if tc.cause != nil {
						cancel(tc.cause)
					}
					return nil, nil, tc.err
				}
				if args[0] == "query" {
					return []byte(`{"type":"virtual-machine","config":{"user.subyard.managed":"` + managedMarker + `"},"devices":{},"expanded_devices":{}}`), nil, nil
				}
				if strings.HasPrefix(strings.Join(args, " "), "config device add ") {
					return nil, nil, nil
				}
				t.Fatalf("unexpected command: %v", args)
				return nil, nil, nil
			}}
			rt := &Runtime{Config: fixtureConfig(t), Runner: runner}
			err := rt.ensureCloudInitDisk(ctx, rt.Config.vm(1))
			if err == nil || calls != tc.call {
				t.Fatalf("error=%v calls=%d, want failure at call %d", err, calls, tc.call)
			}
			if tc.cause != nil {
				if !errors.Is(err, tc.cause) {
					t.Fatalf("error=%v, lost context cause", err)
				}
				return
			}
			if errors.Is(tc.err, context.Canceled) || errors.Is(tc.err, context.DeadlineExceeded) {
				if !errors.Is(err, tc.err) {
					t.Fatalf("error=%v, lost native cancellation identity", err)
				}
				return
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("device failure disclosed command details")
			}
			var got *CommandError
			if errors.As(tc.err, &got) {
				if !errors.As(err, &got) || got.ExitCode != native.ExitCode || got.Duration != native.Duration {
					t.Fatalf("error=%v, lost native command failure metadata", err)
				}
				if got.Name != "incus" || len(got.Args) != 0 {
					t.Fatal("device failure retained private command details")
				}
			}
		})
	}
}

func TestStartVMDoesNotMutateRunningInstance(t *testing.T) {
	runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		if args[0] != "list" {
			t.Fatalf("running VM mutated: %v", args)
		}
		return []byte("RUNNING"), nil, nil
	}}
	rt := &Runtime{Config: fixtureConfig(t), Runner: runner}
	if err := rt.startVM(context.Background(), rt.Config.vm(1)); err != nil {
		t.Fatal(err)
	}
}

func TestBuildBaseRejectsDisabledCloudInitBeforeAPT(t *testing.T) {
	for _, failure := range []string{"disabled cloud-init", "metadata update", "metadata readback", "builder replacement", "foreign builder", "unsupported seed"} {
		t.Run(failure, func(t *testing.T) {
			cfg := fixtureConfig(t)
			if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
				t.Fatal(err)
			}
			spec, err := cfg.EnvironmentSpec(EnvironmentPair)
			if err != nil {
				t.Fatal(err)
			}
			attached, started, statusChecked, updated := false, false, false, false
			var buildKey string
			metadata := `{"architecture":"x86_64","creation_date":1,"expiry_date":0,"properties":{"unrelated":"preserved"},"templates":{"/etc/hostname":{"when":["start"],"template":"hostname.tpl"},"/var/lib/cloud/seed/nocloud-net/meta-data":{"when":["create"],"template":"meta.tpl"},"/var/lib/cloud/seed/nocloud-net/user-data":{"when":["create"],"template":"user.tpl"},"/var/lib/cloud/seed/nocloud-net/vendor-data":{"when":["create"],"template":"vendor.tpl"},"/var/lib/cloud/seed/nocloud-net/network-config":{"when":["create"],"template":"network.tpl"}}}`
			if failure == "unsupported seed" {
				metadata = strings.ReplaceAll(metadata, "nocloud-net/network-config", "unknown/meta-data")
			}
			runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				command := strings.Join(args, " ")
				if strings.Contains(command, "apt-get") || strings.Contains(command, "base.sh") || args[0] == "publish" {
					t.Fatalf("disabled cloud-init advanced preparation: %s", command)
				}
				switch {
				case args[0] == "image":
					return []byte(`[{"fingerprint":"` + strings.Repeat("a", 64) + `","type":"virtual-machine","architecture":"` + imageArchitecture() + `","aliases":[{"name":"debian/13/cloud"}]}]`), nil, nil
				case strings.HasPrefix(command, "query /1.0/storage-pools/"):
					return []byte(`{"space":{"total":171798691840,"used":10737418240}}`), nil, nil
				case strings.HasPrefix(command, "project get ") && strings.HasSuffix(command, "user.subyard.managed"):
					return []byte(managedMarker), nil, nil
				case args[0] == "info":
					return nil, nil, errors.New("absent")
				case strings.HasPrefix(command, "profile device list "):
					return []byte("root\neth0"), nil, nil
				case strings.Contains(command, " -c s"):
					return []byte("STOPPED"), nil, nil
				case args[0] == "query" && strings.Contains(args[1], "/metadata?"):
					if len(args) == 2 {
						return []byte(metadata), nil, nil
					}
					if !attached || started || args[2] != "-X" || args[3] != "PUT" || args[4] != "-d" {
						t.Fatal("metadata update occurred outside stopped CIDATA preparation")
					}
					if failure == "metadata update" {
						return nil, nil, &CommandError{ExitCode: 7, Message: "private metadata"}
					}
					var got struct {
						Properties map[string]string
						Templates  map[string]any
					}
					if json.Unmarshal([]byte(args[5]), &got) != nil || got.Properties["unrelated"] != "preserved" || len(got.Templates) != 1 || got.Templates["/etc/hostname"] == nil {
						t.Fatal("metadata lost unrelated fields or retained duplicate seed templates")
					}
					updated = true
					if failure != "metadata readback" {
						metadata = args[5]
					}
					return nil, nil, nil
				case args[0] == "init":
					for _, arg := range args {
						if value, ok := strings.CutPrefix(arg, "user.subyard.base_key="); ok {
							buildKey = value
						}
					}
				case args[0] == "query":
					devices := `{}`
					if attached {
						devices = `{"subyard-cloud-init":{"type":"disk","source":"cloud-init:config"}}`
					}
					uuid, owner := "00000000-0000-0000-0000-000000000001", strings.Repeat("b", 32)
					if updated && failure == "builder replacement" {
						uuid = "00000000-0000-0000-0000-000000000002"
					}
					if failure == "foreign builder" {
						owner = "foreign"
					}
					return []byte(`{"type":"virtual-machine","status":"Stopped","stateful":false,"config":{"user.subyard.managed":"` + managedMarker + `","user.subyard.base_owner":"` + owner + `","user.subyard.base_key":"` + buildKey + `","volatile.uuid":"` + uuid + `"},"devices":` + devices + `,"expanded_devices":` + devices + `}`), nil, nil
				case strings.HasPrefix(command, "config device add "):
					attached = true
				case args[0] == "start":
					if !attached || !updated {
						t.Fatal("builder started before metadata")
					}
					started = true
				case strings.Contains(command, "cloud-init status"):
					statusChecked = true
					return []byte(`{"status":"disabled","boot_status_code":"disabled-by-generator","errors":[],"recoverable_errors":{}}`), nil, nil
				}
				return nil, nil, nil
			}}
			rt := &Runtime{Config: cfg, Runner: runner, Stdout: io.Discard, Stderr: io.Discard, Now: time.Now,
				memoryProbe:    func() (MemoryCapacity, error) { return MemoryCapacity{Available: 64 << 30}, nil },
				diskUsageProbe: func(context.Context) (uint64, error) { return 10 << 30, nil },
				cacheProbe:     func(context.Context) (CacheUsage, error) { return CacheUsage{}, nil },
			}
			rt.prepareDefaults()
			registry := &ImageRegistry{SchemaVersion: 1, Owner: strings.Repeat("b", 32)}
			_, err = rt.buildBase(context.Background(), LeaseStore{Path: cfg.leaseState(), SlotCount: 2}, LeaseGrant{Environment: &spec}, registry, strings.Repeat("c", 64))
			if err == nil || started != (failure == "disabled cloud-init") || statusChecked != started {
				t.Fatalf("error=%v started=%v statusChecked=%v", err, started, statusChecked)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("metadata error disclosed native output")
			}
			if failure == "metadata update" {
				var native *CommandError
				if !errors.As(err, &native) || native.ExitCode != 7 {
					t.Fatal("metadata failure lost native exit")
				}
			}
		})
	}
}

func TestStartVMRequiresCloudInitDiskBeforeFirstBoot(t *testing.T) {
	for _, prefix := range []string{"e2e-vm", "e2e-base"} {
		for _, attachFails := range []bool{false, true} {
			t.Run(prefix+map[bool]string{false: " success", true: " attach failure"}[attachFails], func(t *testing.T) {
				cfg := fixtureConfig(t)
				cfg.Prefix = prefix
				vm := cfg.vm(1)
				attached, started := false, false
				runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
					command := strings.Join(args, " ")
					switch {
					case args[0] == "query":
						devices := `{}`
						if attached {
							devices = `{"subyard-cloud-init":{"type":"disk","source":"cloud-init:config"}}`
						}
						return []byte(`{"type":"virtual-machine","config":{"user.subyard.managed":"` + managedMarker + `"},"devices":` + devices + `,"expanded_devices":` + devices + `}`), nil, nil
					case strings.Contains(command, " -c s"):
						return []byte("STOPPED"), nil, nil
					case strings.HasPrefix(command, "config device add "):
						if attachFails {
							return nil, nil, errors.New("attach failed")
						}
						attached = true
					case args[0] == "start":
						if !attached {
							t.Error("VM started without detectable metadata")
						}
						started = true
					}
					return nil, nil, nil
				}}
				rt := &Runtime{Config: cfg, Runner: runner, Stdout: io.Discard, Now: time.Now}
				err := rt.startVM(context.Background(), vm)
				if (err != nil) != attachFails || started == attachFails {
					t.Fatalf("error=%v started=%v attachFails=%v", err, started, attachFails)
				}
			})
		}
	}
}
