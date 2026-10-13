package ownerinventory

import "time"

const Freshness = 30 * time.Second

// Fresh reports whether a validated snapshot can be reused for a read.
// A nonpositive TTL uses the default; a future fetch time remains fresh when
// the reader's clock lags behind.
func (snapshot Snapshot) Fresh(now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		ttl = Freshness
	}
	return now.Sub(snapshot.FetchedAt) <= ttl
}
