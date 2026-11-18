package collector

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestRecordFieldParity(t *testing.T) {
	const privatePath = "/home/fixture/private-notes"
	const timestamp = "2026-01-01T00:00:00.000000000Z"
	var inputs []map[string]any
	for field, rule := range bodyRules {
		// Walk every field, including IDs normally mandatory for other kinds.
		for _, path := range []string{privatePath, "private/notes", "private notes"} {
			r := record("gap", map[string]any{field: path})
			clean, removed, err := sanitizeRecord(r)
			if err != nil || len(clean.Body) != 0 || removed != 1 {
				t.Errorf("%s: path was not redacted (removed=%d, err=%v)", field, removed, err)
			}
			inputs = append(inputs, r.Body)
		}
		values := []any{nil, true, 4, -1, 1.5, "4", "identifier", timestamp,
			"9999-12-31T23:59:59-01:00", json.Number("18446744073709551615"), json.Number("18446744073709551616"),
			strings.Repeat("a", 64), strings.Repeat("a", 65), strings.Repeat("a", 128),
			strings.Repeat("a", 129), strings.Repeat("a", 224), strings.Repeat("a", 225)}
		for _, choice := range rule.choices {
			values = append(values, choice)
		}
		for _, value := range values {
			inputs = append(inputs, map[string]any{field: value})
		}
	}
	inputs = append(inputs,
		map[string]any{"unknown": privatePath, "process_namespace_id": "pid:[123]", "cgroup_namespace_id": "cgroup:[456]"},
		map[string]any{"purpose": "agent-e2e", "allocated_cpu": 4},
		map[string]any{"missing": []any{"cpu_ns", privatePath, true}},
		map[string]any{"missing": "broker_attribution"},
		map[string]any{"missing": "final_tail"})
	data, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	const producer = `
import json
import sys

sys.path.insert(0, '..')
import records

print(json.dumps(records.BODY_RULE_TABLE.strip()))
for body in json.load(sys.stdin):
    print(json.dumps(records.clean(body), sort_keys=True, separators=(',', ':')))
`
	cmd := exec.Command("python3", "-B", "-c", producer)
	cmd.Stdin = bytes.NewReader(data)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("producer parity probe: %v\n%s", err, output)
	}
	lines := bytes.Split(bytes.TrimSuffix(output, []byte("\n")), []byte("\n"))
	var producerTable string
	if len(lines) != len(inputs)+1 || json.Unmarshal(lines[0], &producerTable) != nil ||
		producerTable != strings.TrimSpace(bodyRuleTable) {
		t.Fatal("producer and collector field tables differ")
	}
	for i, input := range inputs {
		clean, _, err := sanitizeRecord(record("gap", input))
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(clean.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(lines[i+1], want) {
			t.Errorf("input %v: producer %s, collector %s", input, lines[i+1], want)
		}
	}
}

func TestRecordRejectsPathMetadata(t *testing.T) {
	const privatePath = "/home/fixture/private-notes"
	for _, mutate := range []func(*Record){
		func(r *Record) { r.Schema = privatePath },
		func(r *Record) { r.SourceID = privatePath },
		func(r *Record) { r.EventID = privatePath },
		func(r *Record) { r.SourceTime = privatePath },
		func(r *Record) { r.Kind = privatePath },
		func(r *Record) { r.Body["object_id"] = privatePath },
	} {
		r := record("allocation", map[string]any{"object_id": "fixture"})
		mutate(&r)
		if _, _, err := sanitizeRecord(r); err == nil {
			t.Errorf("accepted invalid required metadata: %+v", r)
		}
	}
	r := record("allocation", map[string]any{"object_id": "fixture"})
	r.SourceVersion = privatePath
	if clean, removed, err := sanitizeRecord(r); err != nil || clean.SourceVersion != "" || removed != 1 {
		t.Fatalf("invalid source version was not redacted (removed=%d, err=%v)", removed, err)
	}
}
