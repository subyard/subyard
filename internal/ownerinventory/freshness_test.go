package ownerinventory

import (
	"testing"
	"time"
)

func TestSnapshotFresh(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for _, test := range []struct {
		name string
		age  time.Duration
		ttl  time.Duration
		want bool
	}{
		{name: "fresh", age: 10 * time.Second, want: true},
		{name: "default boundary", age: 30 * time.Second, want: true},
		{name: "default expired", age: 30*time.Second + time.Nanosecond},
		{name: "future clock", age: -time.Minute, want: true},
		{name: "custom boundary", age: time.Second, ttl: time.Second, want: true},
		{name: "custom expired", age: time.Second + time.Nanosecond, ttl: time.Second},
		{name: "negative TTL defaults", age: 30 * time.Second, ttl: -time.Second, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := Snapshot{FetchedAt: now.Add(-test.age)}
			if got := snapshot.Fresh(now, test.ttl); got != test.want {
				t.Fatalf("Fresh=%v, want %v", got, test.want)
			}
		})
	}
}
