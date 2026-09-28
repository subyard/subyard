package hostruntime

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConntrackCleanerUsesOnlyExactStaleUDPTuple(t *testing.T) {
	endpoint := netip.MustParseAddrPort("10.20.30.40:51820")
	wantFilter := []string{"-f", "ipv4", "-p", "udp", "--orig-dst", "10.20.30.40",
		"--orig-port-dst", "51820", "--reply-src", "10.20.30.40"}
	for _, scenario := range []struct {
		name      string
		list      []string
		deleteErr error
		wantCalls int
		wantErr   bool
	}{
		{name: "empty", list: []string{""}, wantCalls: 1},
		{name: "stale", list: []string{"stale-entry"}, wantCalls: 2},
		{name: "delete race", list: []string{"stale-entry", ""}, deleteErr: errors.New("no such entry"), wantCalls: 3},
		{name: "delete failure", list: []string{"stale-entry", "stale-entry"}, deleteErr: errors.New("permission denied"), wantCalls: 3, wantErr: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			calls, lists := 0, 0
			cleaner := ConntrackCleaner{
				Lookup: func(name string) (string, error) {
					if name != "conntrack" {
						t.Fatalf("lookup %q", name)
					}
					return "/usr/sbin/conntrack", nil
				},
				Run: func(ctx context.Context, binary string, args ...string) ([]byte, error) {
					deadline, bounded := ctx.Deadline()
					if !bounded || time.Until(deadline) > 30*time.Second || time.Until(deadline) <= 0 {
						t.Fatalf("conntrack command lacks bounded deadline")
					}
					calls++
					if binary != "/usr/sbin/conntrack" || len(args) != len(wantFilter)+1 ||
						!reflect.DeepEqual(args[1:], wantFilter) || args[0] != "-L" && args[0] != "-D" {
						t.Fatalf("unsafe conntrack invocation: %q %q", binary, args)
					}
					if args[0] == "-D" {
						return nil, scenario.deleteErr
					}
					value := scenario.list[lists]
					lists++
					return []byte(value), nil
				},
			}
			err := cleaner.Clear(context.Background(), endpoint)
			if (err != nil) != scenario.wantErr || calls != scenario.wantCalls {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if err != nil && (strings.Contains(err.Error(), "permission denied") || strings.Contains(err.Error(), "stale-entry")) {
				t.Fatalf("conntrack output leaked: %v", err)
			}
		})
	}
}

func TestConntrackCleanerListFailureDoesNotDelete(t *testing.T) {
	commands := 0
	cleaner := ConntrackCleaner{
		Lookup: func(string) (string, error) { return "/usr/sbin/conntrack", nil },
		Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			commands++
			if args[0] != "-L" {
				t.Fatalf("deleted after list failure: %q", args)
			}
			return nil, errors.New("sensitive conntrack error")
		},
	}
	if err := cleaner.Clear(context.Background(), netip.MustParseAddrPort("10.20.30.40:51820")); err == nil || commands != 1 || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("list failure did not stop safely: commands=%d err=%v", commands, err)
	}
}

func TestConntrackCleanerPropagatesCancellationWithoutRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cleaner := ConntrackCleaner{
		Lookup: func(string) (string, error) { t.Fatal("lookup after cancellation"); return "", nil },
		Run: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("command after cancellation")
			return nil, nil
		},
	}
	if err := cleaner.Clear(ctx, netip.MustParseAddrPort("10.20.30.40:51820")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was not propagated: %v", err)
	}
}

func TestConntrackCleanerMissingToolFailsOnlyWhenCalled(t *testing.T) {
	cleaner := ConntrackCleaner{Lookup: func(string) (string, error) { return "", errors.New("missing") }}
	if err := cleaner.Clear(context.Background(), netip.MustParseAddrPort("10.20.30.40:51820")); err == nil || !strings.Contains(err.Error(), "rerun yard init") {
		t.Fatalf("missing dependency was not diagnosed: %v", err)
	}
}
