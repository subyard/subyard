package collector

import "testing"

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
		if _, _, err := (valueFilter{}).cleanRecord(r); err == nil {
			t.Errorf("accepted invalid required metadata: %+v", r)
		}
	}
	r := record("allocation", map[string]any{"object_id": "fixture"})
	r.SourceVersion, r.SourceTime = privatePath, privatePath
	if clean, removed, err := (valueFilter{}).cleanRecord(r); err != nil || clean.SourceVersion != "" || clean.SourceTime != unknownSourceTime || removed != 2 {
		t.Fatalf("invalid source version was not redacted (removed=%d, err=%v)", removed, err)
	}
}

func TestMissingFieldRedactionsCountDroppedEntries(t *testing.T) {
	body, removed := (valueFilter{}).body(map[string]any{"missing": []any{"cpu_ns", "private notes", false}})
	if got, ok := body["missing"].([]any); !ok || len(got) != 1 || got[0] != "cpu_ns" || removed != 2 {
		t.Fatalf("missing field redaction: body=%v removed=%d", body, removed)
	}
}
