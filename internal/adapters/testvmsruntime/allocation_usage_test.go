package testvmsruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestQemuMemoryEvidenceReasonsKeepGuards(t *testing.T) {
	for _, test := range []struct {
		name, file, body, reason string
		missing                  bool
	}{
		{"membership unreadable", "membership", "", "cgroup_membership_unavailable", true},
		{"no unified membership", "membership", "1:memory:/vm/one\n", "cgroup_membership_invalid", false},
		{"root membership", "membership", "0::/\n", "cgroup_membership_invalid", false},
		{"traversal", "membership", "0::/vm/../one\n", "cgroup_membership_invalid", false},
		{"deleted", "membership", "0::/vm/one (deleted)\n", "cgroup_membership_invalid", false},
		{"outside boundary", "membership", "0::/other/one\n", "cgroup_outside_memory_boundary", false},
		{"boundary itself", "membership", "0::/vm\n", "cgroup_outside_memory_boundary", false},
		{"processes unreadable", "cgroup.procs", "", "cgroup_processes_unavailable", true},
		{"shared processes", "cgroup.procs", "123\n456\n", "cgroup_processes_not_isolated", false},
		{"wrong process", "cgroup.procs", "456\n", "cgroup_processes_not_isolated", false},
		{"descendants unreadable", "cgroup.stat", "", "cgroup_descendants_unavailable", true},
		{"descendants missing", "cgroup.stat", "other 0\n", "cgroup_descendants_invalid", false},
		{"descendants malformed", "cgroup.stat", "nr_descendants secret-value\n", "cgroup_descendants_invalid", false},
		{"descendants present", "cgroup.stat", "nr_descendants 1\n", "cgroup_descendants_present", false},
		{"counters unreadable", "memory.stat", "", "memory_counters_unavailable", true},
		{"anon missing", "memory.stat", "shmem 20\n", "memory_counters_invalid", false},
		{"shmem missing", "memory.stat", "anon 10\n", "memory_counters_invalid", false},
		{"counter malformed", "memory.stat", "anon secret-value\nshmem 20\n", "memory_counters_invalid", false},
		{"other counter malformed", "memory.stat", "anon 10\nshmem 20\nfile secret-value\n", "memory_counters_invalid", false},
		{"counter overflow", "memory.stat", "anon 18446744073709551615\nshmem 1\n", "memory_counters_overflow", false},
		{"confirmed", "memory.stat", "anon 10\nshmem 20\nfile 999\n", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			proc, group := filepath.Join(root, "proc"), filepath.Join(root, "cgroup")
			files := map[string]string{
				"membership":   "0::/vm/one\n",
				"cgroup.procs": "123\n",
				"cgroup.stat":  "nr_descendants 0\n",
				"memory.stat":  "anon 10\nshmem 20\n",
			}
			files[test.file] = test.body
			for name, body := range files {
				if name == test.file && test.missing {
					continue
				}
				path := filepath.Join(group, "vm/one", name)
				if name == "membership" {
					path = filepath.Join(proc, "123/cgroup")
				}
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, []byte(body), 0600)
			}
			used, ok, reason := qemuAnonymousUsage(proc, group, 123, filepath.Join(group, "vm"))
			if test.reason == "" {
				if !ok || used != 30 {
					t.Fatalf("confirmed usage = %d, %v", used, ok)
				}
			} else if ok || used != 0 || reason.String() != test.reason {
				t.Fatalf("failed guard = %d, %v, %q; want zero credit and %q", used, ok, reason.String(), test.reason)
			}
		})
	}
}

func TestAllocationMemoryEvidenceReasons(t *testing.T) {
	identity := fmt.Sprintf(`{"type":"virtual-machine","config":{"user.subyard.managed":%q,"user.subyard.generation":"2","user.subyard.lease-epoch":"3"}}`, managedMarker)
	for _, test := range []struct {
		name, instance, state, reason string
		instanceErr, stateErr         error
		noScope, noReservation        bool
		invalidSlot                   bool
	}{
		{name: "missing reservation", noReservation: true, reason: "reservation_unavailable"},
		{name: "invalid slot", invalidSlot: true, reason: "slot_invalid"},
		{name: "missing boundary", noScope: true, reason: "memory_boundary_unavailable"},
		{name: "instance query", instanceErr: errors.New("secret-value /private/path pid=123"), reason: "incus_query_failed"},
		{name: "instance deadline", instanceErr: fmt.Errorf("secret-value: %w", context.DeadlineExceeded), reason: "incus_query_deadline"},
		{name: "instance JSON", instance: "secret-value", reason: "instance_identity_invalid"},
		{name: "instance type", instance: strings.Replace(identity, "virtual-machine", "container", 1), reason: "instance_identity_invalid"},
		{name: "managed marker", instance: strings.Replace(identity, managedMarker, "foreign", 1), reason: "instance_identity_invalid"},
		{name: "generation", instance: strings.Replace(identity, `"2"`, `"4"`, 1), reason: "instance_identity_invalid"},
		{name: "epoch", instance: strings.Replace(identity, `"3"`, `"4"`, 1), reason: "instance_identity_invalid"},
		{name: "state query", stateErr: errors.New("secret-value /private/path"), reason: "incus_query_failed"},
		{name: "state deadline", stateErr: context.DeadlineExceeded, reason: "incus_query_deadline"},
		{name: "state JSON", state: "secret-value", reason: "instance_state_invalid"},
		{name: "state unsupported", state: `{"status":"Frozen","pid":123}`, reason: "instance_state_invalid"},
		{name: "PID missing", state: `{"status":"Running"}`, reason: "instance_pid_invalid"},
		{name: "PID init", state: `{"status":"Running","pid":1}`, reason: "instance_pid_invalid"},
		{name: "stopped", state: `{"status":"Stopped"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			spec, _ := cfg.EnvironmentSpec(EnvironmentPair)
			slot := LeaseSlot{SlotID: "slot-001", Environment: &spec, Reserved: !test.noReservation, ResourceGeneration: 2, LeaseEpoch: 3}
			if test.invalidSlot {
				slot.SlotID = "invalid"
			}
			instance, state := test.instance, test.state
			if instance == "" {
				instance = identity
			}
			if state == "" {
				state = `{"status":"Stopped"}`
			}
			rt := Runtime{Config: cfg, Runner: &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				if args[0] != "query" {
					t.Fatalf("usage attempted mutation: %v", args)
				}
				if args[1] == "/1.0/storage-pools/default" {
					return []byte(`{"driver":"zfs"}`), nil, nil
				}
				// A different later failure must not replace the first VM's cause.
				if strings.Contains(args[1], "-2?") {
					return nil, nil, errors.New("secret-value later VM failure")
				}
				if strings.Contains(args[1], "/state?") {
					return []byte(state), nil, test.stateErr
				}
				return []byte(instance), nil, test.instanceErr
			}}}
			if test.reason == "" {
				// Confirm the stopped-VM path independently of the later-failure check.
				spec.Count = 1
			}
			scope := "synthetic-boundary"
			if test.noScope {
				scope = ""
			}
			usage := rt.allocationUsage(context.Background(), slot, scope)
			if usage.memory != 0 || usage.memoryKnown != (test.reason == "") || (test.reason != "" && usage.memoryReason.String() != test.reason) {
				t.Fatalf("memory usage = %+v; want reason %q and zero credit", usage, test.reason)
			}
		})
	}
}

func TestMemoryEvidenceReasonIsBounded(t *testing.T) {
	for value := 0; value <= 255; value++ {
		reason := memoryEvidenceReason(value).String()
		if reason == "" || len(reason) > 40 || strings.Trim(reason, "abcdefghijklmnopqrstuvwxyz_") != "" {
			t.Fatalf("unsafe public reason: %q", reason)
		}
	}
}
