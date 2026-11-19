package collector

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestLegacyCounterAndIdentityDelivery(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	source := strings.Repeat("s", 128) + "/z"
	const at = "2026-01-01T01:00:00Z"
	// These use the prior v1 envelopes with a previously valid source ID and
	// a sample whose uint counter was serialized as a decimal string.
	const oldBody = `{"object_id":"scope/path","cpu_ns":"+000000000000000000000000000000000007"}`
	old := fmt.Sprintf(`{"schema":"subyard.telemetry.export/v1","origin_id":"producer:%[1]s","event_id":"old-event","source_id":"%[1]s","source_version":"1","source_time":"%[2]s","received_time":"%[2]s","kind":"sample","encoding":"json","policy_version":"subyard.telemetry.capture/v1","body":%[3]s}`, source, at, oldBody)
	following := fmt.Sprintf(`{"schema":"subyard.telemetry.export/v1","origin_id":"producer:%[1]s","event_id":"following","source_id":"%[1]s","source_version":"1","source_time":"%[2]s","kind":"scope","encoding":"json","policy_version":"subyard.telemetry.capture/v1","body":{"object_id":"scope/path"}}`, source, at)
	for range 2 {
		if err := s.Replay(ctx, strings.NewReader(old+"\n"+following)); err != nil {
			t.Fatal(err)
		}
	}
	var count, total int
	if err := s.db.QueryRow(`SELECT count(*),sum(json_extract(samples.body,'$.cpu_ns')) FROM samples
		JOIN objects USING(origin_id,object_id) JOIN records USING(origin_id,event_id)
		WHERE origin_id=? AND source_id=? AND json_type(samples.body,'$.cpu_ns')='integer'`,
		"producer:"+source, source).Scan(&count, &total); err != nil || count != 1 || total != 7 {
		t.Fatalf("legacy joins, counters or retries failed: count=%d total=%d err=%v", count, total, err)
	}
}
