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
	credential := strings.Repeat("6a", 32)
	const timestamp = "2026-01-01T00:00:00.000000000Z"
	var inputs []map[string]any
	for field, rule := range bodyRules {
		// Walk every field, including IDs normally mandatory for other kinds.
		for _, path := range []string{privatePath, "private/notes", "private notes", credential} {
			r := record("gap", map[string]any{field: path})
			clean, removed, err := identity((identityKey{}).id).record(r)
			if rule.kind == "id" && legacyID.MatchString(path) {
				if err != nil || clean.Body[field] != (identityKey{}).id(path) {
					t.Errorf("%s: legacy path identifier was not pseudonymized: %v", field, err)
				}
			} else if err != nil || len(clean.Body) != 0 || removed != 1 {
				t.Errorf("%s: path was not redacted (removed=%d, err=%v)", field, removed, err)
			}
			inputs = append(inputs, r.Body)
		}
		values := []any{nil, true, 4, -1, 1.5, "4", "+00000000000000000000000000000004", "identifier", timestamp,
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

for body in json.load(sys.stdin):
    print(json.dumps(records.clean(body, b"")[0], sort_keys=True, separators=(',', ':')))
`
	cmd := exec.Command("python3", "-B", "-c", producer)
	cmd.Stdin = bytes.NewReader(data)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("producer parity probe: %v\n%s", err, output)
	}
	lines := bytes.Split(bytes.TrimSuffix(output, []byte("\n")), []byte("\n"))
	if len(lines) != len(inputs) {
		t.Fatalf("producer returned %d rows for %d inputs", len(lines), len(inputs))
	}
	for i, input := range inputs {
		clean, _, err := identity((identityKey{}).id).record(record("gap", input))
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(clean.Body)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(want, []byte(credential)) || bytes.Contains(lines[i], []byte(credential)) {
			t.Errorf("credential persisted for input %d", i)
		}
		if !bytes.Equal(lines[i], want) {
			t.Errorf("input %v: producer %s, collector %s", input, lines[i], want)
		}
	}
}

func TestRecordRejectsPathMetadata(t *testing.T) {
	const privatePath = "/home/fixture/private-notes"
	for _, mutate := range []func(*Record){
		func(r *Record) { r.Schema = privatePath },
		func(r *Record) { r.SourceID = "wrong source" },
		func(r *Record) { r.EventID = "wrong event" },
		func(r *Record) { r.Kind = privatePath },
		func(r *Record) { r.Body["object_id"] = "wrong object" },
	} {
		r := record("allocation", map[string]any{"object_id": "fixture"})
		mutate(&r)
		if _, _, err := identity((identityKey{}).id).record(r); err == nil {
			t.Errorf("accepted invalid required metadata: %+v", r)
		}
	}
	r := record("allocation", map[string]any{"object_id": "fixture"})
	r.SourceVersion, r.SourceTime = privatePath, privatePath
	if clean, removed, err := identity((identityKey{}).id).record(r); err != nil || clean.SourceVersion != "" || clean.SourceTime != unknownSourceTime || removed != 2 {
		t.Fatalf("invalid source version was not redacted (removed=%d, err=%v)", removed, err)
	}
}

func TestMissingFieldRedactionsCountDroppedEntries(t *testing.T) {
	body, removed := identity((identityKey{}).id).body(map[string]any{"missing": []any{"cpu_ns", "private notes", false}})
	if got, ok := body["missing"].([]any); !ok || len(got) != 1 || got[0] != "cpu_ns" || removed != 2 {
		t.Fatalf("missing field redaction: body=%v removed=%d", body, removed)
	}
}
