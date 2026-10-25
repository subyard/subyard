// telemetry-compare sends deterministic synthetic OTLP to a leased benchmark VM.
// It is a load driver, never part of the installed runtime.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

type payload struct {
	signal  string
	data    []byte
	records int64
}

func payloads(seed int64) []payload {
	base := pcommon.Timestamp(1792800000000000000)
	logs := plog.NewLogs()
	lr := logs.ResourceLogs().AppendEmpty()
	lr.Resource().Attributes().PutStr("service.name", "telemetry-benchmark")
	lr.Resource().Attributes().PutInt("benchmark.seed", seed)
	ls := lr.ScopeLogs().AppendEmpty()
	ls.Scope().SetName("benchmark")
	for i := 0; i < 32; i++ {
		r := ls.LogRecords().AppendEmpty()
		r.SetTimestamp(base + pcommon.Timestamp(i))
		r.Body().SetStr("synthetic content excluded by default policy")
		r.Attributes().PutInt("gen_ai.usage.input_tokens", 100)
		r.Attributes().PutStr("gen_ai.request.model", "synthetic-model")
	}
	logBytes, _ := plogotlp.NewExportRequestFromLogs(logs).MarshalProto()
	metrics := pmetric.NewMetrics()
	mr := metrics.ResourceMetrics().AppendEmpty()
	mr.Resource().Attributes().PutStr("service.name", "telemetry-benchmark")
	ms := mr.ScopeMetrics().AppendEmpty()
	ms.Scope().SetName("benchmark")
	for i := 0; i < 32; i++ {
		m := ms.Metrics().AppendEmpty()
		m.SetName(fmt.Sprintf("benchmark.metric.%d", i))
		switch i % 5 {
		case 0:
			p := m.SetEmptyGauge().DataPoints().AppendEmpty()
			p.SetTimestamp(base)
			p.SetDoubleValue(42.5)
		case 1:
			s := m.SetEmptySum()
			s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
			s.SetIsMonotonic(true)
			p := s.DataPoints().AppendEmpty()
			p.SetStartTimestamp(base - 1000000000)
			p.SetTimestamp(base)
			p.SetIntValue(1000)
		case 2:
			h := m.SetEmptyHistogram()
			h.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
			p := h.DataPoints().AppendEmpty()
			p.SetStartTimestamp(base - 1000000000)
			p.SetTimestamp(base)
			p.SetCount(3)
			p.SetSum(12)
			p.SetMin(1)
			p.SetMax(7)
			p.ExplicitBounds().FromRaw([]float64{2, 5})
			p.BucketCounts().FromRaw([]uint64{1, 1, 1})
		case 3:
			h := m.SetEmptyExponentialHistogram()
			h.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
			p := h.DataPoints().AppendEmpty()
			p.SetStartTimestamp(base - 1000000000)
			p.SetTimestamp(base)
			p.SetCount(2)
			p.SetScale(2)
			p.SetSum(4)
			p.Positive().SetOffset(1)
			p.Positive().BucketCounts().FromRaw([]uint64{1, 1})
		case 4:
			p := m.SetEmptySummary().DataPoints().AppendEmpty()
			p.SetTimestamp(base)
			p.SetCount(2)
			p.SetSum(4)
			q := p.QuantileValues().AppendEmpty()
			q.SetQuantile(0.5)
			q.SetValue(2)
		}
	}
	metricBytes, _ := pmetricotlp.NewExportRequestFromMetrics(metrics).MarshalProto()
	traces := ptrace.NewTraces()
	tr := traces.ResourceSpans().AppendEmpty()
	tr.Resource().Attributes().PutStr("service.name", "telemetry-benchmark")
	ts := tr.ScopeSpans().AppendEmpty()
	ts.Scope().SetName("benchmark")
	for i := 0; i < 32; i++ {
		s := ts.Spans().AppendEmpty()
		s.SetName("benchmark.operation")
		id := pcommon.TraceID{1, 2, 3}
		id[15] = byte(i + 1)
		s.SetTraceID(id)
		s.SetSpanID(pcommon.SpanID{1, 2, 3, 4, 5, 6, 7, byte(i + 1)})
		s.SetStartTimestamp(base)
		s.SetEndTimestamp(base + 1000000)
		s.Attributes().PutStr("gen_ai.request.model", "synthetic-model")
		l := s.Links().AppendEmpty()
		l.SetTraceID(pcommon.TraceID{4, 5, 6})
		l.SetSpanID(pcommon.SpanID{7, 8, 9})
		e := s.Events().AppendEmpty()
		e.SetName("benchmark.event")
		e.SetTimestamp(base)
	}
	traceBytes, _ := ptraceotlp.NewExportRequestFromTraces(traces).MarshalProto()
	return []payload{{"logs", logBytes, 32}, {"metrics", metricBytes, 32}, {"traces", traceBytes, 32}}
}
func main() {
	duration := flag.Duration("duration", 90*time.Second, "offered load duration")
	rate := flag.Int("rate", 30, "mixed OTLP requests per second")
	seed := flag.Int64("seed", 1, "payload seed")
	tokenFile := flag.String("token-file", "", "private bearer token file")
	endpoint := flag.String("endpoint", "http://127.0.0.1:4318", "OTLP HTTP base endpoint")
	flag.Parse()
	if *rate < 1 || *rate > 10000 || *duration <= 0 {
		fmt.Fprintln(os.Stderr, "invalid load bounds")
		os.Exit(1)
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read token file")
		os.Exit(1)
	}
	data := payloads(*seed)
	client := &http.Client{Timeout: 10 * time.Second}
	jobs := make(chan payload, 128)
	var workers sync.WaitGroup
	var mu sync.Mutex
	var offered, accepted, rejected, offeredRecords, acceptedRecords, rejectedRecords, offeredBytes, acceptedBytes, capacityDrops int64
	latencies := []int64{}
	statuses := map[int]int64{}
	counts := map[string]int64{}
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for p := range jobs {
				start := time.Now()
				req, _ := http.NewRequestWithContext(context.Background(), "POST", strings.TrimRight(*endpoint, "/")+"/v1/"+p.signal, bytes.NewReader(p.data))
				req.Header.Set("Content-Type", "application/x-protobuf")
				req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
				response, err := client.Do(req)
				code := 0
				if err == nil {
					code = response.StatusCode
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
				mu.Lock()
				statuses[code]++
				if code == 200 {
					accepted++
					acceptedRecords += p.records
					acceptedBytes += int64(len(p.data))
					counts[p.signal] += p.records
				} else {
					rejected++
					rejectedRecords += p.records
				}
				latencies = append(latencies, time.Since(start).Microseconds())
				mu.Unlock()
			}
		}()
	}
	started := time.Now()
	ticker := time.NewTicker(time.Second / time.Duration(*rate))
	end := time.NewTimer(*duration)
	index := 0
loop:
	for {
		select {
		case <-end.C:
			break loop
		case <-ticker.C:
			p := data[index%len(data)]
			index++
			mu.Lock()
			offered++
			offeredRecords += p.records
			offeredBytes += int64(len(p.data))
			mu.Unlock()
			select {
			case jobs <- p:
			default:
				mu.Lock()
				capacityDrops++
				rejected++
				rejectedRecords += p.records
				mu.Unlock()
			}
		}
	}
	ticker.Stop()
	close(jobs)
	workers.Wait()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	percentile := func(q float64) int64 {
		if len(latencies) == 0 {
			return 0
		}
		return latencies[int(float64(len(latencies)-1)*q)]
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": "subyard.telemetry.benchmark/v1", "seed": *seed, "duration_seconds": duration.Seconds(), "elapsed_seconds": time.Since(started).Seconds(), "rate": *rate, "offered_requests": offered, "accepted_requests": accepted, "rejected_requests": rejected, "offered_records": offeredRecords, "accepted_records": acceptedRecords, "rejected_records": rejectedRecords, "offered_bytes": offeredBytes, "accepted_bytes": acceptedBytes, "client_capacity_drops": capacityDrops, "accepted_by_signal": counts, "http_statuses": statuses, "latency_us": map[string]int64{"p50": percentile(.5), "p95": percentile(.95), "p99": percentile(.99)}})
}
