package testvmsruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestAllocationDiskUsageRejectsUnknownEvidenceWithoutMutation(t *testing.T) {
	identity := fmt.Sprintf(`{"type":"virtual-machine","config":{"user.subyard.managed":%q,"user.subyard.generation":"2","user.subyard.lease-epoch":"3"}}`, managedMarker)
	for _, test := range []struct {
		name, instance, pool                      string
		instanceErr, poolErr                      error
		noReservation, noEnvironment, invalidSlot bool
	}{
		{name: "missing reservation", noReservation: true},
		{name: "missing environment", noEnvironment: true},
		{name: "invalid slot", invalidSlot: true},
		{name: "pool query", poolErr: errors.New("pool unavailable")},
		{name: "pool JSON", pool: "invalid JSON"},
		{name: "unsupported driver", pool: `{"driver":"zfs"}`, instance: identity},
		{name: "instance query", instanceErr: errors.New("instance unavailable")},
		{name: "instance deadline", instanceErr: context.DeadlineExceeded},
		{name: "instance JSON", instance: "invalid JSON"},
		{name: "instance type", instance: strings.Replace(identity, "virtual-machine", "container", 1)},
		{name: "managed marker", instance: strings.Replace(identity, managedMarker, "foreign", 1)},
		{name: "generation", instance: strings.Replace(identity, `"2"`, `"4"`, 1)},
		{name: "epoch", instance: strings.Replace(identity, `"3"`, `"4"`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			spec, _ := cfg.EnvironmentSpec(EnvironmentPair)
			spec.Count = 1
			slot := LeaseSlot{SlotID: "slot-001", Environment: &spec, Reserved: !test.noReservation, ResourceGeneration: 2, LeaseEpoch: 3}
			if test.noEnvironment {
				slot.Environment = nil
			}
			if test.invalidSlot {
				slot.SlotID = "invalid"
			}
			pool := test.pool
			if pool == "" {
				pool = `{"driver":"dir"}`
			}
			instance := test.instance
			if instance == "" {
				instance = "invalid JSON"
			}
			queries := 0
			rt := Runtime{Config: cfg, Runner: &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
				queries++
				if len(args) != 2 || args[0] != "query" {
					t.Fatalf("usage attempted mutation: %v", args)
				}
				if args[1] == "/1.0/storage-pools/default" {
					return []byte(pool), nil, test.poolErr
				}
				child := (&Runtime{Config: cfg}).slotRuntime(1, "")
				child.useLeaseSlot(slot)
				if args[1] != "/1.0/instances/"+child.Config.vm(1)+"?project="+child.Config.Project {
					t.Fatalf("unexpected usage query: %v", args)
				}
				return []byte(instance), nil, test.instanceErr
			}}}
			usage := rt.allocationUsage(context.Background(), slot)
			if usage.disk != 0 || usage.diskKnown {
				t.Fatalf("unknown evidence credited disk usage: %+v", usage)
			}
			wantQueries := 2
			if test.noReservation || test.noEnvironment || test.invalidSlot {
				wantQueries = 0
			}
			if queries != wantQueries {
				t.Fatalf("usage queries = %d; want %d", queries, wantQueries)
			}
		})
	}
}
