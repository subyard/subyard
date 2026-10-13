package ownerpublication

import (
	"errors"
	"net/netip"
	"testing"
)

func TestPublicationAttachAddressDisappearanceRetriesAsDeferral(t *testing.T) {
	address := netip.MustParseAddr("192.0.2.12")
	reads, calls := 0, 0
	waiting, err := ReconcileAddresses(func() ([]netip.Addr, error) {
		reads++
		if reads == 1 {
			return []netip.Addr{address}, nil
		}
		return nil, nil
	}, func(addresses []netip.Addr) ([]string, error) {
		calls++
		if len(addresses) > 0 {
			return nil, errors.New("bind address disappeared")
		}
		return []string{"tcp:192.0.2.12:8080"}, nil
	})
	if err != nil || len(waiting) != 1 || reads != 2 || calls != 2 {
		t.Fatalf("race: waiting=%v err=%v reads=%d calls=%d", waiting, err, reads, calls)
	}
}
func TestStableAddressConflictKeepsOriginalFailure(t *testing.T) {
	conflict := errors.New("port collision")
	calls := 0
	_, err := ReconcileAddresses(func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.0.2.12")}, nil }, func([]netip.Addr) ([]string, error) { calls++; return nil, conflict })
	if !errors.Is(err, conflict) || calls != 1 {
		t.Fatal("stable conflict was hidden or retried")
	}
}
