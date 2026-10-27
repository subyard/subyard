package collector

import (
	"context"
	"fmt"
	"testing"
)

func TestRunOwnerGapsAndSessionObservations(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	records := []Record{
		record("run", map[string]any{"agent_run_id": "parent"}),
		record("run", map[string]any{"agent_run_id": "child", "parent_run_id": "parent"}),
	}
	for _, id := range []string{"parent", "child", "unrelated"} {
		records = append(records,
			record("allocation", map[string]any{"object_id": id + "-vm", "owner_id": id + "-owner", "object_kind": "vm"}),
			record("association", map[string]any{"object_id": id + "-vm", "agent_run_id": id, "quality": "explicit", "start_time": "2026-10-24T00:00:00Z"}),
			record("gap", map[string]any{"owner_id": id + "-owner", "reason": "source_offline", "quality": "unknown", "stale": true}))
	}
	// A gap with no owner must not match objects that also lack owner metadata.
	records = append(records,
		record("scope", map[string]any{"object_id": "ownerless", "agent_run_id": "parent", "quality": "explicit"}),
		record("gap", map[string]any{"reason": "source_offline", "quality": "unknown"}))
	var sessionIDs []string
	for _, at := range []string{"2026-10-24T02:00:00Z", "2026-10-24T01:00:00Z"} {
		r := record("relation", map[string]any{"agent_run_id": "parent", "session_id": "native-session", "source": "native-otlp-attribute/authenticated-header", "quality": "explicit"})
		r.SourceTime = at
		sessionIDs = append(sessionIDs, r.EventID)
		records = append(records, r)
	}
	childSession := record("relation", map[string]any{"agent_run_id": "child", "session_id": "child-native-session", "source": "native-otlp-attribute/registered-resource-attribute", "quality": "explicit"})
	records = append(records, childSession)
	brokerRelation := record("relation", map[string]any{"agent_run_id": "parent", "correlation_id": "native-acquire", "source": "acquire-correlation", "quality": "explicit"})
	records = append(records, brokerRelation)
	if err := s.Ingest(ctx, records); err != nil {
		t.Fatal(err)
	}
	db, err := OpenReadOnly(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var retained int
	if err = db.QueryRow("SELECT count(*) FROM relations WHERE event_id=? AND session_id=''", brokerRelation.EventID).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("raw broker correlation relation lost", err)
	}
	for _, includeChildren := range []bool{false, true} {
		t.Run(fmt.Sprintf("include_children=%t", includeChildren), func(t *testing.T) {
			var result map[string]any
			var err error
			if includeChildren {
				result, err = ShowRunIncludingChildren(ctx, db, "parent")
			} else {
				result, err = ShowRun(ctx, db, "parent")
			}
			if err != nil {
				t.Fatal(err)
			}
			gaps := result["gaps"].([]any)
			wantCount := 1
			if includeChildren {
				wantCount = 2
			}
			if len(gaps) != wantCount {
				t.Fatalf("owner gaps omitted or unrelated owner included: %+v", gaps)
			}
			for _, value := range gaps {
				gap := value.(map[string]any)
				owner := stringValue(gap, "owner_id")
				if owner != "parent-owner" && !(includeChildren && owner == "child-owner") || gap["quality"] != "unknown" || gap["stale"] != true {
					t.Fatalf("incorrect owner gap evidence: %+v", gap)
				}
			}
			sessions := result["sessions"].([]map[string]any)
			if len(sessions) != wantCount {
				t.Fatalf("session scope incorrect: %+v", sessions)
			}
			var parent map[string]any
			for _, session := range sessions {
				if session["session_id"] == "native-session" {
					parent = session
				}
			}
			if parent == nil || parent["agent_run_id"] != "parent" || parent["first_observed_source_time"] != "2026-10-24T01:00:00.000000000Z" || parent["last_observed_source_time"] != "2026-10-24T02:00:00.000000000Z" || parent["observation_semantics"] != "recorded_observation_points_not_session_lifetime" {
				t.Fatalf("observation bounds or semantics incorrect: %+v", parent)
			}
			refs := parent["relation_refs"].([]any)
			if len(refs) != 2 {
				t.Fatalf("relation refs missing: %+v", refs)
			}
			for i, value := range refs {
				ref := value.(map[string]any)
				if ref["origin_id"] != "producer:fixture" || ref["event_id"] != sessionIDs[1-i] || ref["source_id"] != "fixture" || ref["source_version"] != "fixture/1" || ref["source"] != "native-otlp-attribute/authenticated-header" || ref["quality"] != "explicit" || ref["received_time"] == "" {
					t.Fatalf("relation provenance lost: %+v", ref)
				}
			}
		})
	}
}
