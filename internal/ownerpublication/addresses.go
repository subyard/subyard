package ownerpublication

import (
	"net/netip"
	"slices"
)

// ReconcileAddresses reobserves once after an attach failure. A disappearing
// exact address becomes a deferred publication; stable-address conflicts remain
// errors. It never probes a listener or changes power.
func ReconcileAddresses(read func() ([]netip.Addr, error), apply func([]netip.Addr) ([]string, error)) ([]string, error) {
	addresses, err := read()
	if err != nil {
		return nil, err
	}
	waiting, err := apply(addresses)
	if err == nil {
		return waiting, nil
	}
	fresh, observationErr := read()
	if observationErr != nil || slices.Equal(addresses, fresh) {
		return nil, err
	}
	return apply(fresh)
}
