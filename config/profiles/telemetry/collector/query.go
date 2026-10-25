package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func Stats(ctx context.Context, db *sql.DB) (map[string]any, error) {
	var origin, version string
	var count, last int64
	if err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='origin_id'").Scan(&origin); err != nil {
		return nil, err
	}
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
		return nil, err
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*),coalesce(max(sequence),0) FROM records").Scan(&count, &last); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT kind,count(*) FROM records GROUP BY kind ORDER BY kind")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var kind string
		var n int64
		if err = rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		counts[kind] = n
	}
	return map[string]any{"origin_id": origin, "sqlite_version": version, "records": count, "last_sequence": last, "kinds": counts}, rows.Err()
}

// ShowRun selects each measured object once and keeps its explicit temporal
// edges. Detail covered by inclusive ancestors is never charged a second time.
func ShowRun(ctx context.Context, db *sql.DB, run string) (map[string]any, error) {
	return showRun(ctx, db, run, false)
}

// ShowRunIncludingChildren follows only explicitly recorded parent_run_id edges.
// Object selection is performed once across the tree, before totals are formed.
func ShowRunIncludingChildren(ctx context.Context, db *sql.DB, run string) (map[string]any, error) {
	return showRun(ctx, db, run, true)
}

func showRun(ctx context.Context, db *sql.DB, run string, includeChildren bool) (map[string]any, error) {
	var raw string
	if err := db.QueryRowContext(ctx, "SELECT body FROM runs WHERE agent_run_id=?", run).Scan(&raw); err != nil {
		return nil, err
	}
	body, err := decodeBody([]byte(raw))
	if err != nil {
		return nil, err
	}
	runIDs := []string{run}
	mode := "direct_run"
	if includeChildren {
		mode = "including_child_runs"
		runIDs, err = descendantRuns(ctx, db, run)
		if err != nil {
			return nil, err
		}
	}
	literals := make([]string, len(runIDs))
	for i, id := range runIDs {
		literals[i] = sqlLiteral(id)
	}
	runSet := "(" + strings.Join(literals, ",") + ")"
	rows, err := db.QueryContext(ctx, `SELECT o.object_id,o.parent_object_id,o.accounting,o.body,a.body,a.start_time,a.end_time,a.shared,a.quality,a.agent_run_id FROM associations a JOIN objects o ON o.object_id=a.object_id WHERE a.agent_run_id IN `+runSet+` ORDER BY o.object_id,a.start_time,a.agent_run_id LIMIT 10001`)
	if err != nil {
		return nil, err
	}
	type object struct {
		id, parent, accounting string
		body                   map[string]any
		edges                  []map[string]any
	}
	objects := map[string]*object{}
	order := []string{}
	n := 0
	for rows.Next() {
		n++
		if n > 10000 {
			rows.Close()
			return nil, errors.New("association window exceeded")
		}
		var id, parent, accounting, data, assoc, start, end, quality, originalRun string
		var shared bool
		if err = rows.Scan(&id, &parent, &accounting, &data, &assoc, &start, &end, &shared, &quality, &originalRun); err != nil {
			rows.Close()
			return nil, err
		}
		o := objects[id]
		if o == nil {
			b, e := decodeBody([]byte(data))
			if e != nil {
				rows.Close()
				return nil, e
			}
			o = &object{id: id, parent: parent, accounting: accounting, body: b}
			objects[id] = o
			order = append(order, id)
		}
		edge, e := decodeBody([]byte(assoc))
		if e != nil {
			rows.Close()
			return nil, e
		}
		edge["start_time"] = start
		edge["end_time"] = end
		edge["shared"] = shared
		edge["quality"] = quality
		edge["agent_run_id"] = originalRun
		o.edges = append(o.edges, edge)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	selected := []any{}
	shared := []any{}
	unknown := []any{}
	overlap := []any{}
	for _, id := range order {
		o := objects[id]
		entry := map[string]any{"object": o.body, "associations": o.edges}
		isShared := false
		trusted := true
		for i, edge := range o.edges {
			v, _ := edge["shared"].(bool)
			isShared = isShared || v
			trusted = trusted && trustedEdge(edge)
			for _, previous := range o.edges[:i] {
				if stringValue(previous, "agent_run_id") != stringValue(edge, "agent_run_id") && intervalsOverlap(previous, edge) {
					isShared = true
				}
			}
		}
		other, err := db.QueryContext(ctx, `SELECT start_time,end_time,agent_run_id FROM associations WHERE object_id=? AND agent_run_id NOT IN `+runSet+` LIMIT 10001`, id)
		if err != nil {
			return nil, err
		}
		count := 0
		for other.Next() {
			count++
			if count > 10000 {
				other.Close()
				return nil, errors.New("association window exceeded")
			}
			var start, end, otherRun string
			if err = other.Scan(&start, &end, &otherRun); err != nil {
				other.Close()
				return nil, err
			}
			edge := map[string]any{"start_time": start, "end_time": end}
			for _, ours := range o.edges {
				if intervalsOverlap(ours, edge) {
					isShared = true
				}
			}
		}
		if err = other.Err(); err != nil {
			other.Close()
			return nil, err
		}
		other.Close()
		disposition := "selected"
		if isShared {
			entry["quality"] = "shared"
			disposition = "shared"
		} else if !trusted {
			disposition = "unknown"
		}
		coveredEdges := 0
		for _, edge := range o.edges {
			parent := stringValue(edge, "parent_object_id")
			if parent == "" {
				parent = o.parent
			}
			seen := map[string]bool{id: true}
			covered := false
			for parent != "" {
				if seen[parent] {
					return nil, errors.New("object hierarchy cycle")
				}
				seen[parent] = true
				p := objects[parent]
				if p == nil {
					break
				}
				if p.accounting == "inclusive" {
					for _, pe := range p.edges {
						v, _ := pe["shared"].(bool)
						if !v && trustedEdge(pe) && intervalContains(pe, edge) {
							covered = true
							break
						}
					}
				}
				if covered {
					break
				}
				parent = p.parent
			}
			if covered {
				coveredEdges++
			}
		}
		if disposition == "selected" {
			if coveredEdges == len(o.edges) {
				disposition = "overlap"
			} else if coveredEdges > 0 {
				entry["quality"] = "partial_hierarchy_coverage"
				disposition = "unknown"
			}
		}
		samples, err := db.QueryContext(ctx, `SELECT s.body,s.source_time,s.origin_id,s.event_id,r.source_id,r.received_time,r.sequence
			FROM samples s JOIN records r ON r.origin_id=s.origin_id AND r.event_id=s.event_id
			WHERE s.object_id=? ORDER BY s.source_time,s.origin_id,s.event_id LIMIT 10001`, id)
		if err != nil {
			return nil, err
		}
		measurements := []any{}
		sampleRefs := []any{}
		times := []string{}
		var latestOrigin, latestSource, latestReceived string
		var latestSequence int64
		count = 0
		for samples.Next() {
			count++
			if count > 10000 {
				samples.Close()
				return nil, errors.New("sample window exceeded")
			}
			var data, at, origin, event, source, received string
			var sequence int64
			if err = samples.Scan(&data, &at, &origin, &event, &source, &received, &sequence); err != nil {
				samples.Close()
				return nil, err
			}
			inside := false
			for _, edge := range o.edges {
				if pointWithin(edge, at) {
					inside = true
					break
				}
			}
			if !inside {
				continue
			}
			measurement, e := decodeBody([]byte(data))
			if e != nil {
				samples.Close()
				return nil, e
			}
			measurements = append(measurements, measurement)
			sampleRefs = append(sampleRefs, map[string]any{"origin_id": origin, "event_id": event, "source_time": at})
			times = append(times, at)
			latestOrigin, latestSource, latestReceived, latestSequence = origin, source, received, sequence
		}
		if err = samples.Err(); err != nil {
			samples.Close()
			return nil, err
		}
		samples.Close()
		entry["samples"] = measurements
		entry["sample_refs"] = sampleRefs
		entry["latest_sample"] = nil
		entry["stale"] = true
		entry["cpu_ns_delta"] = nil
		if len(measurements) > 0 {
			entry["latest_sample"] = measurements[len(measurements)-1]
			entry["last_seen"] = times[len(times)-1]
			at, e := time.Parse(time.RFC3339Nano, times[len(times)-1])
			entry["stale"] = e != nil || time.Since(at) > 30*time.Second
			offline, e := offlineAfterSample(ctx, db, o.body, measurements[len(measurements)-1].(map[string]any), latestOrigin, latestSource, latestReceived, latestSequence)
			if e != nil {
				return nil, e
			}
			if offline {
				entry["stale"] = true
			}
		}
		summarizeObject(entry, measurements, o.edges)
		if disposition == "unknown" {
			entry["attributed"] = false
		}
		switch disposition {
		case "shared":
			shared = append(shared, entry)
		case "unknown":
			unknown = append(unknown, entry)
		case "overlap":
			overlap = append(overlap, entry)
		default:
			selected = append(selected, entry)
		}

	}
	sessionRows, err := sessionObservations(ctx, db, runSet)
	if err != nil {
		return nil, err
	}
	childRows, err := Query(ctx, db, "SELECT agent_run_id,body FROM runs WHERE json_extract(body,'$.parent_run_id')="+sqlLiteral(run)+" ORDER BY agent_run_id")
	if err != nil {
		return nil, err
	}
	processRows, err := Query(ctx, db, "SELECT DISTINCT o.object_id,o.body FROM objects o JOIN associations a ON a.object_id=o.object_id WHERE o.kind='process' AND a.agent_run_id IN "+runSet+" ORDER BY o.object_id")
	if err != nil {
		return nil, err
	}
	gaps := []any{}
	gapRows, err := db.QueryContext(ctx, `SELECT body FROM gaps WHERE agent_run_id IN `+runSet+`
		OR object_id IN(SELECT object_id FROM associations WHERE agent_run_id IN `+runSet+`)
		OR nullif(json_extract(body,'$.owner_id'),'') IN(
			SELECT nullif(json_extract(o.body,'$.owner_id'),'') FROM objects o JOIN associations a ON a.object_id=o.object_id WHERE a.agent_run_id IN `+runSet+`)
		ORDER BY source_time LIMIT 10001`)
	if err != nil {
		return nil, err
	}
	defer gapRows.Close()
	for gapRows.Next() {
		if len(gaps) >= 10000 {
			return nil, errors.New("gap window exceeded")
		}
		var raw string
		if err = gapRows.Scan(&raw); err != nil {
			return nil, err
		}
		b, e := decodeBody([]byte(raw))
		if e != nil {
			return nil, e
		}
		gaps = append(gaps, b)
	}
	return map[string]any{"run": body, "attribution_mode": mode, "included_runs": runIDs, "hierarchy_source": "run.parent_run_id", "selected_nonoverlapping": selected, "shared": shared, "unknown": unknown, "excluded_overlapping_detail": overlap, "gaps": gaps, "totals": runTotals(selected, overlap, shared, unknown), "sessions": sessionRows, "child_runs": childRows, "processes": processRows, "accounting_note": "Samples belong once to objects and explicit time intervals. CPU deltas require compatible monotonic/counter domains. Missing values remain null; native retry duplicates are not usage totals."}, gapRows.Err()
}

func sessionObservations(ctx context.Context, db *sql.DB, runSet string) ([]map[string]any, error) {
	observations, err := Query(ctx, db, `SELECT r.agent_run_id,r.session_id,r.origin_id,r.event_id,r.body,e.source_id,e.source_version,e.source_time,e.received_time
		FROM relations r JOIN records e ON e.origin_id=r.origin_id AND e.event_id=r.event_id
		WHERE r.session_id<>'' AND r.agent_run_id IN `+runSet+` ORDER BY r.agent_run_id,r.session_id,e.source_time,r.origin_id,r.event_id LIMIT 10001`)
	if err != nil {
		return nil, err
	}
	type identity struct{ run, session string }
	byID := map[identity]map[string]any{}
	sessions := []map[string]any{}
	for _, observation := range observations {
		body, err := decodeBody([]byte(stringValue(observation, "body")))
		if err != nil {
			return nil, err
		}
		id := identity{stringValue(observation, "agent_run_id"), stringValue(observation, "session_id")}
		session := byID[id]
		if session == nil {
			session = map[string]any{"agent_run_id": id.run, "session_id": id.session,
				"first_observed_source_time": observation["source_time"], "last_observed_source_time": observation["source_time"],
				"observation_semantics": "recorded_observation_points_not_session_lifetime", "relation_refs": []any{}}
			byID[id] = session
			sessions = append(sessions, session)
		}
		session["last_observed_source_time"] = observation["source_time"]
		ref := map[string]any{"origin_id": observation["origin_id"], "event_id": observation["event_id"],
			"source_id": observation["source_id"], "source_version": observation["source_version"],
			"source_time": observation["source_time"], "received_time": observation["received_time"],
			"source": body["source"], "quality": body["quality"]}
		session["relation_refs"] = append(session["relation_refs"].([]any), ref)
	}
	return sessions, nil
}

func offlineAfterSample(ctx context.Context, db *sql.DB, object, sample map[string]any, origin, source, received string, sequence int64) (bool, error) {
	// Compare receipt chronology only within the exact original producer/source.
	// Owner sample timestamps and the observer's offline clock may differ.
	rows, err := db.QueryContext(ctx, `SELECT g.body FROM gaps g JOIN records r ON r.origin_id=g.origin_id AND r.event_id=g.event_id
		WHERE r.origin_id=? AND r.source_id=? AND json_extract(g.body,'$.reason')='source_offline'
		AND (g.object_id=? OR (?<>'' AND json_extract(g.body,'$.owner_id')=?))
		AND (r.received_time>? OR (r.received_time=? AND r.sequence>?)) LIMIT 10001`,
		origin, source, stringValue(object, "object_id"), stringValue(object, "owner_id"), stringValue(object, "owner_id"), received, received, sequence)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > 10000 {
			return false, errors.New("gap window exceeded")
		}
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return false, err
		}
		gap, err := decodeBody([]byte(raw))
		if err != nil {
			return false, err
		}
		if sameMeasuredGeneration(gap, sample) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func descendantRuns(ctx context.Context, db *sql.DB, run string) ([]string, error) {
	// UNION visits an identity once, including malformed cyclic producer graphs.
	// The finite CTE limit detects oversized trees without materializing all runs.
	rows, err := db.QueryContext(ctx, `WITH RECURSIVE selected_runs(agent_run_id) AS (
		SELECT ? UNION SELECT r.agent_run_id FROM runs r JOIN selected_runs s ON json_extract(r.body,'$.parent_run_id')=s.agent_run_id LIMIT 1001
	) SELECT r.agent_run_id,coalesce(json_extract(r.body,'$.parent_run_id'),'') FROM runs r JOIN selected_runs s USING(agent_run_id) ORDER BY r.agent_run_id`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	parents := map[string]string{}
	for rows.Next() {
		if len(ids) >= 1000 {
			return nil, errors.New("run hierarchy window exceeded (1000 runs)")
		}
		var id, parent string
		if err = rows.Scan(&id, &parent); err != nil {
			return nil, err
		}
		ids = append(ids, id)
		parents[id] = parent
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// Every selected descendant has a single explicit parent. A reachable cycle
	// therefore includes the requested root and its recorded parent.
	if _, cycle := parents[parents[run]]; cycle {
		return nil, errors.New("run hierarchy cycle")
	}
	return ids, nil
}
func trustedEdge(e map[string]any) bool {
	q := stringValue(e, "quality")
	_, err := time.Parse(time.RFC3339Nano, stringValue(e, "start_time"))
	return err == nil && (q == "explicit" || q == "managed" || q == "exact")
}
func pointWithin(edge map[string]any, point string) bool {
	start, end := stringValue(edge, "start_time"), stringValue(edge, "end_time")
	return (start == "" || point >= start) && (end == "" || point <= end)
}
func intervalContains(parent, child map[string]any) bool {
	ps, pe := stringValue(parent, "start_time"), stringValue(parent, "end_time")
	cs, ce := stringValue(child, "start_time"), stringValue(child, "end_time")
	return ps != "" && cs != "" && ps <= cs && (pe == "" || (ce != "" && pe >= ce))
}
func intervalsOverlap(a, b map[string]any) bool {
	as, ae := stringValue(a, "start_time"), stringValue(a, "end_time")
	bs, be := stringValue(b, "start_time"), stringValue(b, "end_time")
	return (ae == "" || bs == "" || bs <= ae) && (be == "" || as == "" || as <= be)
}
func integerValue(value any) (*big.Int, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return nil, false
	}
	v, ok := new(big.Int).SetString(number.String(), 10)
	return v, ok
}

const queryByteLimit = 16 << 20

func Query(ctx context.Context, db *sql.DB, query string) ([]map[string]any, error) {
	connection, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	// Apply the native scalar/encoded-row limit before preparing or evaluating
	// SQL, not after the driver has already materialized a generated value.
	previous, err := sqlite.Limit(connection, sqlite3.SQLITE_LIMIT_LENGTH, -1)
	if err != nil {
		return nil, err
	}
	if previous > queryByteLimit {
		if _, err = sqlite.Limit(connection, sqlite3.SQLITE_LIMIT_LENGTH, queryByteLimit); err != nil {
			return nil, err
		}
		defer sqlite.Limit(connection, sqlite3.SQLITE_LIMIT_LENGTH, previous)
	}
	rows, err := connection.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	var bytesRead int
	for rows.Next() {
		if len(result) >= 10000 {
			return nil, errors.New("query row limit exceeded")
		}
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err = rows.Scan(pointers...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, k := range columns {
			if b, ok := values[i].([]byte); ok {
				bytesRead += len(b)
				if bytesRead > queryByteLimit {
					return nil, errors.New("query byte window exceeded")
				}
				row[k] = string(b)
			} else {
				if value, ok := values[i].(string); ok {
					bytesRead += len(value)
				}
				row[k] = values[i]
			}
		}
		if bytesRead > queryByteLimit {
			return nil, errors.New("query byte window exceeded")
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func sqlLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
