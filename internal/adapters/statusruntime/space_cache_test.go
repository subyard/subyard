package statusruntime

import (
	"testing"
	"time"
)

func TestDescribeSpaceCache(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for _, test := range []struct {
		name      string
		figure    string
		age       time.Duration
		ttl       string
		started   bool
		refreshes bool
		want      string
	}{
		{name: "missing", refreshes: true, want: "in-yard size unavailable"},
		{name: "missing refresh started", started: true, refreshes: true, want: "in-yard size unavailable — refresh started"},
		{name: "fresh", figure: "2G", age: 2 * time.Minute, started: true, want: "2G  (in-yard rootfs, 2m ago)"},
		{name: "TTL boundary", figure: "2G", age: 10 * time.Minute, want: "2G  (in-yard rootfs, 10m ago)"},
		{name: "expired", figure: "2G", age: 10*time.Minute + time.Second, refreshes: true, want: "2G  (in-yard rootfs, 10m ago, refresh unavailable)"},
		{name: "expired refresh started", figure: "2G", age: 10*time.Minute + time.Second, started: true, refreshes: true, want: "2G  (in-yard rootfs, 10m ago, refresh started)"},
		{name: "custom TTL", figure: "2G", age: 2 * time.Minute, ttl: "60", refreshes: true, want: "2G  (in-yard rootfs, 2m ago, refresh unavailable)"},
		{name: "invalid TTL", figure: "2G", age: 2 * time.Minute, ttl: "invalid", want: "2G  (in-yard rootfs, 2m ago)"},
		{name: "zero TTL", figure: "2G", age: 2 * time.Minute, ttl: "0", want: "2G  (in-yard rootfs, 2m ago)"},
		{name: "negative TTL", figure: "2G", age: 2 * time.Minute, ttl: "-1", want: "2G  (in-yard rootfs, 2m ago)"},
		{name: "future measurement", figure: "2G", age: -time.Minute, want: "2G  (in-yard rootfs, 0s ago)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			measurement := SpaceMeasurement{Figure: test.figure, MeasuredAt: now.Add(-test.age)}
			calls := 0
			got := describeSpaceCache(measurement, now, test.ttl, func() bool {
				calls++
				return test.started
			})
			if got != test.want || (calls == 1) != test.refreshes || calls > 1 {
				t.Errorf("got %q, refresh calls=%d; want %q, refresh=%v", got, calls, test.want, test.refreshes)
			}
		})
	}
}
