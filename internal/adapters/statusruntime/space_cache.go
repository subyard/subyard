package statusruntime

import (
	"fmt"
	"strconv"
	"time"
)

// describeSpaceCache decides whether to request a background refresh and renders
// the last measurement without waiting for that refresh to finish.
func describeSpaceCache(
	measurement SpaceMeasurement, now time.Time, configuredTTL string, refresh func() bool,
) string {
	ttl := 10 * time.Minute
	if seconds, err := strconv.Atoi(configuredTTL); err == nil && seconds > 0 {
		ttl = time.Duration(seconds) * time.Second
	}
	age := now.Sub(measurement.MeasuredAt)
	stale := measurement.Figure == "" || age > ttl
	refreshing := stale && refresh()
	if measurement.Figure == "" {
		if refreshing {
			return "in-yard size unavailable — refresh started"
		}
		return "in-yard size unavailable"
	}
	if age < 0 {
		age = 0
	}
	note := ""
	if stale {
		note = ", refresh unavailable"
		if refreshing {
			note = ", refresh started"
		}
	}
	return fmt.Sprintf("%s  (in-yard rootfs, %s ago%s)", measurement.Figure, ageHuman(age), note)
}
