// Package collector implements the optional local telemetry data plane.
package collector

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const RecordSchema = "subyard.telemetry.record/v1"
const ExportSchema = "subyard.telemetry.export/v1"
const timestampLayout = "2006-01-02T15:04:05.000000000Z"
const unknownSourceTime = "0001-01-01T00:00:00.000000000Z"

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

var identityPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
var objectIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,224}$`)
var shortIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
var namespaceIDPattern = regexp.MustCompile(`^(?:pid|cgroup):\[[0-9]{1,20}\]$`)
var timestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:[.,][0-9]{1,9})?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`)

func validID(s string) bool { return identityPattern.MatchString(s) }

func canonicalTimestamp(value string) string {
	if !timestampPattern.MatchString(value) || strings.HasPrefix(value, "0000-") {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return ""
	}
	utc := parsed.UTC()
	if utc.Year() < 1 || utc.Year() > 9999 {
		return ""
	}
	return utc.Format(timestampLayout)
}

func sanitizeSourceVersion(version string) (string, int64) {
	if version != "" && !validID(version) {
		return "", 1
	}
	return version, 0
}

// Mirror of records.py, checked by TestRecordFieldParity.
const bodyRuleTable = `
id agent_run_id agent_version session_id parent_run_id task_id project_id worktree_id yard_id host_id owner_id boot_id generation counter_generation resource_id slot_id correlation_id allocation_id association_id native_session_id executable_name acquire_id device_id sample_id source_event_id cursor attribution_id
object object_id parent_object_id
short_id purpose resource_kind boundary lifecycle type unit
namespace cgroup_namespace_id process_namespace_id
uint monotonic_ns cpu_ns cpu_user_ns cpu_system_ns memory_bytes memory_peak_bytes swap_bytes rss_bytes rss_peak_bytes pss_bytes io_read_bytes io_write_bytes io_read_ops io_write_ops observed_process_count pid start_ticks resource_generation lease_epoch requested_cpu reserved_cpu reserved_memory_bytes allocated_cpu allocated_memory_bytes allocated_disk_bytes sampled_peak_bytes reserved_disk_bytes actual_cpu_ns actual_memory_bytes actual_disk_bytes cpu_count memory_limit_bytes disk_limit_bytes interval_ns device_count host_monotonic_ns runtime_generation process_count allocation_generation requested_memory_bytes requested_disk_bytes reserved_vcpu reserved_ram_bytes measured_cpu_ns measured_memory_bytes vcpus disk_bytes cpu_usage_ns memory_current_bytes memory_limit swap_current_bytes read_bytes write_bytes read_ops write_ops source_sequence cpu memory disk resources
int exit_code value
uint signal
bool counter_reset shared final_complete resource_completed agent_exited available stale requested reserved actual present final main_exited supported populated
time coverage_start coverage_end acquired_at ready_at expires_at stopped_at start_time end_time observed_at observed_time last_seen source_time
missing missing
drop status
enum agent codex claude other
enum state starting running not_started main_exited resources_observing resources_complete resources_unknown observed scope_left ended available provisioning held draining quarantined recovering unavailable
enum object_kind cgroup process_tree process scope vm android
enum accounting direct inclusive inclusive-owner-resource qemu-process-and-threads
enum quality explicit measured observed complete partial unknown
enum scope_quality managed partial unavailable
enum selector 1 2 emulator
enum reservation_semantics allocated_virtual_capacity_held
enum relation native_session
enum source acquire-correlation validated-grant managed_scope_membership process_tree_discovery scope_root_missing scope_root_generation_changed cgroup_missing cgroup_generation_changed native-otlp-attribute native-otlp-attribute/authenticated-header native-otlp-attribute/registered-resource-attribute
enum phase request held allocation acquired baseline draining observed periodic pre_stop stopped quarantined lease.acquire lease.admission_rejected lease.available lease.expired lease.fence lease.held lease.release lease.renew_failed lease.stop_failed lease.stop_succeeded lease.transition
enum reason boot_changed_before_final correlation_generation_mismatch correlation_registration_capacity correlation_registration_conflict correlation_registration_invalid correlation_registration_unavailable cursor_retention_or_reset delegation_unavailable final_tail_missing managed_scope_unavailable observer_capacity process_final_counter_unavailable process_left_managed_scope producer_outbox_capacity run_manifest_capacity run_manifest_unavailable run_reservation_interrupted run_startup_unavailable scope_baseline_unavailable scope_manifest_missing scope_observation_unavailable scope_placement_unavailable scope_removed_before_final source_offline
`

type bodyRule struct {
	kind    string
	choices []string
}

var bodyRules = func() map[string]bodyRule {
	rules := map[string]bodyRule{}
	for line := range strings.Lines(strings.TrimSpace(bodyRuleTable)) {
		parts := strings.Fields(line)
		rule := bodyRule{kind: parts[0]}
		if rule.kind == "enum" {
			rule.choices = parts[2:]
			rules[parts[1]] = rule
			continue
		}
		for _, field := range parts[1:] {
			rules[field] = rule
		}
	}
	return rules
}()

func sanitizeField(rule bodyRule, value any) (any, bool) {
	if rule.kind == "" || rule.kind == "drop" {
		return nil, false
	}
	if value == nil {
		return nil, true
	}
	switch rule.kind {
	case "missing":
		switch v := value.(type) {
		case []any:
			if len(v) > 128 {
				return nil, false
			}
			fields := []any{}
			for _, field := range v {
				if name, ok := field.(string); ok {
					if rule, known := bodyRules[name]; known && rule.kind != "drop" {
						fields = append(fields, name)
					}
				}
			}
			return fields, true
		case string:
			return v, v == "broker_attribution" || v == "final_tail"
		default:
			return sanitizeField(bodyRule{kind: "uint"}, v)
		}
	case "bool":
		_, ok := value.(bool)
		return value, ok
	case "uint", "int":
		switch value.(type) {
		case json.Number, int, int64, uint64:
			number := fmt.Sprint(value)
			if rule.kind == "uint" {
				_, err := strconv.ParseUint(number, 10, 64)
				return value, err == nil
			}
			_, err := strconv.ParseInt(number, 10, 64)
			return value, err == nil
		default:
			return nil, false
		}
	}
	s, ok := value.(string)
	if !ok {
		return nil, false
	}
	switch rule.kind {
	case "enum":
		return s, slices.Contains(rule.choices, s)
	case "time":
		s = canonicalTimestamp(s)
		return s, s != ""
	case "id":
		return s, validID(s)
	case "object":
		return s, objectIDPattern.MatchString(s)
	case "short_id":
		return s, shortIDPattern.MatchString(s)
	case "namespace":
		return s, namespaceIDPattern.MatchString(s)
	default:
		return nil, false
	}
}

var kinds = map[string]bool{"run": true, "session": true, "relation": true, "scope": true, "process": true, "sample": true, "allocation": true, "association": true, "gap": true}

func sanitizeRecord(r Record) (Record, int64, error) {
	if r.Schema != RecordSchema && !(r.Schema == "" && r.SchemaVersion == 1) {
		return r, 0, errors.New("unsupported record schema")
	}
	if !validID(r.SourceID) || !validID(r.EventID) || !kinds[r.Kind] || r.Body == nil || len(r.Body) > 128 {
		return r, 0, errors.New("invalid record identity or kind")
	}
	r.SourceTime = canonicalTimestamp(r.SourceTime)
	if r.SourceTime == "" {
		return r, 0, errors.New("invalid source time")
	}
	var removed int64
	r.SourceVersion, removed = sanitizeSourceVersion(r.SourceVersion)
	r.Schema = RecordSchema
	r.SchemaVersion = 0
	body := make(map[string]any, len(r.Body))
	for key, value := range r.Body {
		rule := bodyRules[key]
		clean, ok := sanitizeField(rule, value)
		if ok {
			body[key] = clean
		} else if r.Kind == "sample" && rule.kind == "uint" {
			return r, removed, errors.New("invalid sample counter")
		} else {
			removed++
		}
	}
	r.Body = body
	if r.Kind == "run" && !validID(stringValue(body, "agent_run_id")) {
		return r, removed, errors.New("run identity missing")
	}
	if r.Kind == "scope" || r.Kind == "process" || r.Kind == "sample" || r.Kind == "allocation" || r.Kind == "association" {
		if stringValue(body, "object_id") == "" {
			return r, removed, errors.New("object identity missing")
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
