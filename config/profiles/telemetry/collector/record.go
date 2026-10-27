// Package collector implements the optional local telemetry data plane.
package collector

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

const RecordSchema = "subyard.telemetry.record/v1"
const ExportSchema = "subyard.telemetry.export/v1"
const timestampLayout = "2006-01-02T15:04:05.000000000Z"

type Record struct {
	Schema        string         `json:"schema,omitempty"`
	SchemaVersion int            `json:"schema_version,omitempty"`
	SourceID      string         `json:"source_id"`
	EventID       string         `json:"event_id"`
	SourceVersion string         `json:"source_version"`
	SourceTime    string         `json:"source_time"`
	Kind          string         `json:"kind"`
	Body          map[string]any `json:"body"`
}

type Envelope struct {
	Schema        string `json:"schema"`
	OriginID      string `json:"origin_id"`
	EventID       string `json:"event_id"`
	Sequence      int64  `json:"sequence"`
	ReceivedTime  string `json:"received_time"`
	SourceID      string `json:"source_id"`
	SourceVersion string `json:"source_version"`
	SourceTime    string `json:"source_time,omitempty"`
	Kind          string `json:"kind"`
	Encoding      string `json:"encoding"`
	PolicyVersion string `json:"policy_version"`
	// PolicyVersion is the source capture claim; imports independently record
	// the trusted local policy applied before the imported bytes are stored.
	ImportPolicyVersion string          `json:"import_policy_version,omitempty"`
	Redactions          int64           `json:"redactions"`
	Body                json.RawMessage `json:"body,omitempty"`
	Protobuf            []byte          `json:"protobuf,omitempty"`
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

var identityPattern = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,512}$`)

func validID(s string) bool { return identityPattern.MatchString(s) }

// Internal producers use a closed field vocabulary. Unknown fields cannot carry
// command content or credentials into the durable outbox/import path.
var bodyFields = strings.Fields(`agent_run_id state agent agent_version session_id parent_run_id task_id project_id worktree_id yard_id host_id owner_id object_id object_kind parent_object_id accounting boot_id generation counter_generation monotonic_ns cpu_ns cpu_user_ns cpu_system_ns memory_bytes memory_peak_bytes swap_bytes rss_bytes pss_bytes io_read_bytes io_write_bytes io_read_ops io_write_ops quality scope_quality coverage_start coverage_end counter_reset observed_process_count cgroup_namespace_id process_namespace_id pid start_ticks resource_id selector slot_id resource_generation lease_epoch requested_cpu reserved_cpu reserved_memory_bytes correlation_id acquired_at ready_at expires_at stopped_at start_time end_time shared source reason relation phase missing final_complete observed_at exit_code allocation_id association_id native_session_id executable_name resource_completed agent_exited`)
var additionalBodyFields = strings.Fields(`purpose allocated_cpu allocated_memory_bytes allocated_disk_bytes reservation_semantics rss_peak_bytes signal available sampled_peak_bytes acquire_id reserved_disk_bytes actual_cpu_ns actual_memory_bytes actual_disk_bytes cpu_count memory_limit_bytes disk_limit_bytes interval_ns status resource_kind device_count device_id sample_id source_event_id cursor stale boundary host_monotonic_ns runtime_generation requested reserved actual lifecycle type cpu memory disk resources present final main_exited supported process_count value unit observed_time allocation_generation requested_memory_bytes last_seen source_time requested_disk_bytes reserved_vcpu reserved_ram_bytes measured_cpu_ns measured_memory_bytes vcpus disk_bytes cpu_usage_ns memory_current_bytes memory_limit swap_current_bytes read_bytes write_bytes read_ops write_ops populated attribution_id source_sequence`)
var allowedBodies = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range bodyFields {
		m[k] = true
	}
	for _, k := range additionalBodyFields {
		m[k] = true
	}
	return m
}()
var kinds = map[string]bool{"run": true, "session": true, "relation": true, "scope": true, "process": true, "sample": true, "allocation": true, "association": true, "gap": true}

func sanitizeRecord(r Record) (Record, int64, error) {
	if r.Schema != RecordSchema && !(r.Schema == "" && r.SchemaVersion == 1) {
		return r, 0, errors.New("unsupported record schema")
	}
	if !validID(r.SourceID) || !validID(r.EventID) || !kinds[r.Kind] || r.Body == nil {
		return r, 0, errors.New("invalid record identity or kind")
	}
	parsedTime, err := time.Parse(time.RFC3339Nano, r.SourceTime)
	if err != nil {
		return r, 0, errors.New("invalid source time")
	}
	r.SourceTime = parsedTime.UTC().Format(timestampLayout)
	if len(r.SourceVersion) > 128 {
		return r, 0, errors.New("source version too long")
	}
	r.Schema = RecordSchema
	r.SchemaVersion = 0
	body := make(map[string]any, len(r.Body))
	var removed int64
	for key, value := range r.Body {
		if !allowedBodies[key] {
			removed++
			continue
		}
		switch v := value.(type) {
		case nil, bool, float64, json.Number, int, int64, uint64:
			body[key] = v
		case string:
			if len(v) > 1024 || strings.ContainsAny(v, "\x00\r\n") {
				return r, removed, fmt.Errorf("invalid %s", key)
			}
			if key == "start_time" || key == "end_time" || strings.HasSuffix(key, "_at") || key == "coverage_start" || key == "coverage_end" {
				if parsed, e := time.Parse(time.RFC3339Nano, v); e == nil {
					v = parsed.UTC().Format(timestampLayout)
				}
			}
			body[key] = v
		case []any:
			if key != "missing" {
				removed++
				continue
			}
			values := []string{}
			for _, x := range v {
				s, ok := x.(string)
				if !ok || !allowedBodies[s] {
					return r, removed, errors.New("invalid missing metric")
				}
				values = append(values, s)
			}
			body[key] = values
		default:
			removed++
		}
	}
	r.Body = body
	if r.Kind == "run" && !validID(stringValue(body, "agent_run_id")) {
		return r, removed, errors.New("run identity missing")
	}
	if r.Kind == "scope" || r.Kind == "process" || r.Kind == "sample" || r.Kind == "allocation" || r.Kind == "association" {
		if !validID(stringValue(body, "object_id")) {
			return r, removed, errors.New("object identity missing")
		}
	}
	if r.Kind == "sample" {
		for key, value := range body {
			if value == nil {
				continue
			}
			if key == "monotonic_ns" || strings.HasSuffix(key, "_bytes") || strings.HasSuffix(key, "_ns") || strings.HasSuffix(key, "_ops") || key == "pid" || key == "start_ticks" {
				number, ok := new(big.Int).SetString(fmt.Sprint(value), 10)
				if !ok || number.Sign() < 0 || number.BitLen() > 64 {
					return r, removed, errors.New("invalid sample counter")
				}
			}
		}
	}
	return r, removed, nil
}

func decodeBody(data []byte) (map[string]any, error) {
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	err := dec.Decode(&body)
	return body, err
}
