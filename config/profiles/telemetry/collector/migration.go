package collector

import (
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"

	"modernc.org/sqlite"
)

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("telemetry_rekey", -1, migrateIdentity)
}

// The only rewrite of retained content is the identity namespace. Keep the
// original rows, chronology, capture claims and non-identity metadata.
func migrate(db *sql.DB, key identityKey, version int) error {
	if version == 3 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == 0 {
		if _, err = tx.Exec(schema); err != nil {
			return err
		}
		return tx.Commit()
	}
	if version == 1 {
		if _, err = tx.Exec("ALTER TABLE records ADD COLUMN import_policy_version TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	steps := []string{
		`CREATE TEMP TABLE telemetry_event_keys AS
		  WITH mapped AS (
		    SELECT sequence, origin_id AS old_origin, event_id AS old_event,
		           telemetry_rekey(?1,'origin',origin_id,source_id) AS origin,
		           telemetry_rekey(?1,'id',event_id) AS event
		    FROM records
		  )
		  SELECT sequence, old_origin, old_event, origin,
		         CASE WHEN row_number() OVER (PARTITION BY origin,event ORDER BY sequence)=1 THEN event
		              ELSE telemetry_rekey(?1,'duplicate',CAST(sequence AS TEXT)) END AS event
		  FROM mapped`,
		`CREATE UNIQUE INDEX telemetry_event_keys_old ON telemetry_event_keys(old_origin,old_event)`,
		`CREATE UNIQUE INDEX telemetry_event_keys_sequence ON telemetry_event_keys(sequence)`,
		`CREATE TEMP TABLE telemetry_edge_keys AS
		  WITH edges AS (
		    SELECT r.origin_id AS origin, k.origin AS mapped_origin,
		           json_extract(r.body,'$.object_id') AS object,
		           json_extract(r.body,'$.agent_run_id') AS run,
		           CASE WHEN r.kind='association' THEN coalesce(json_extract(r.body,'$.source'),'') END AS evidence
		    FROM records r JOIN telemetry_event_keys k USING(sequence)
		    WHERE r.encoding='json' AND (r.kind='scope' OR
		      (r.kind='association' AND coalesce(json_extract(r.body,'$.association_id'),'')=''))
		  ), keys AS (
		    SELECT telemetry_rekey(?1,'old_edge',origin,object,run,evidence) AS old_edge,
		           telemetry_rekey(?1,'old_edge_hash',origin,object,run,evidence) AS old_edge_hash,
		           telemetry_rekey(?1,'edge',mapped_origin,object,run,evidence) AS edge
		    FROM edges WHERE object IS NOT NULL
		  )
		  SELECT old_edge AS old, edge FROM keys UNION SELECT old_edge_hash AS old, edge FROM keys`,
		`CREATE INDEX telemetry_edge_keys_old ON telemetry_edge_keys(old)`,
	}
	if err := runMigrationSteps(tx, key, steps); err != nil {
		return err
	}
	if err := migrateProjections(tx, key); err != nil {
		return err
	}
	steps = []string{
		`UPDATE samples SET origin_id=k.origin,event_id=k.event,
		    object_id=telemetry_rekey(?1,'id',samples.object_id),body=telemetry_rekey(?1,'body',samples.body)
		  FROM telemetry_event_keys k WHERE samples.origin_id=k.old_origin AND samples.event_id=k.old_event`,
		`UPDATE gaps SET origin_id=k.origin,event_id=k.event,
		    object_id=telemetry_rekey(?1,'id',gaps.object_id),agent_run_id=telemetry_rekey(?1,'id',gaps.agent_run_id),
		    body=telemetry_rekey(?1,'body',gaps.body)
		  FROM telemetry_event_keys k WHERE gaps.origin_id=k.old_origin AND gaps.event_id=k.old_event`,
		`UPDATE relations SET origin_id=k.origin,event_id=k.event,
		    agent_run_id=telemetry_rekey(?1,'id',relations.agent_run_id),
		    session_id=telemetry_rekey(?1,'id',relations.session_id),body=telemetry_rekey(?1,'body',relations.body)
		  FROM telemetry_event_keys k WHERE relations.origin_id=k.old_origin AND relations.event_id=k.old_event`,
		`UPDATE records SET origin_id=k.origin,event_id=k.event,
		    source_id=telemetry_rekey(?1,'id',records.source_id),
		    body=CASE WHEN encoding='json' THEN telemetry_rekey(?1,'body',records.body) ELSE records.body END,
		    protobuf=CASE WHEN encoding LIKE 'otlp/%' THEN telemetry_rekey(?1,'signal',encoding,protobuf) ELSE protobuf END
		  FROM telemetry_event_keys k WHERE records.sequence=k.sequence`,
		`UPDATE metadata SET value=telemetry_rekey(?1,'id',value) WHERE key='origin_id'`,
		`DROP TABLE telemetry_edge_keys`,
		`DROP TABLE telemetry_event_keys`,
		`PRAGMA user_version=3`,
	}
	if err := runMigrationSteps(tx, key, steps); err != nil {
		return err
	}
	return tx.Commit()
}

func runMigrationSteps(tx *sql.Tx, key identityKey, steps []string) error {
	for i, step := range steps {
		var args []any
		if strings.Contains(step, "?1") {
			args = append(args, []byte(key))
		}
		if _, err := tx.Exec(step, args...); err != nil {
			return fmt.Errorf("telemetry identity migration step %d: %w", i+1, err)
		}
	}
	return nil
}

// Two historical spellings can already exist as separate projection rows.
// Collapse them chronologically while retaining all underlying event rows.
func migrateProjections(tx *sql.Tx, key identityKey) error {
	for _, projection := range []struct {
		table, primaryKey, fields, order, update string
	}{
		{"runs", "agent_run_id",
			"telemetry_rekey(?1,'id',agent_run_id),telemetry_rekey(?1,'body',body),source_time",
			"source_time", "source_time=excluded.source_time"},
		{"objects", "object_id",
			"telemetry_rekey(?1,'id',object_id),kind,telemetry_rekey(?1,'id',parent_object_id),accounting,telemetry_rekey(?1,'body',body),source_time",
			"source_time", "kind=excluded.kind,source_time=excluded.source_time," +
				"parent_object_id=coalesce(nullif(excluded.parent_object_id,''),objects.parent_object_id)," +
				"accounting=coalesce(nullif(excluded.accounting,''),objects.accounting)"},
		{"allocations", "object_id",
			"telemetry_rekey(?1,'id',object_id),telemetry_rekey(?1,'body',body),source_time",
			"source_time", "source_time=excluded.source_time"},
		{"associations", "association_id",
			`coalesce((SELECT edge FROM telemetry_edge_keys WHERE old=association_id ORDER BY edge LIMIT 1),
			           telemetry_rekey(?1,'id',association_id)),
			  telemetry_rekey(?1,'id',object_id),telemetry_rekey(?1,'id',agent_run_id),
			  quality,start_time,end_time,shared,telemetry_rekey(?1,'body',body)`,
			"coalesce(nullif(end_time,''),start_time)",
			"start_time=coalesce(nullif(associations.start_time,''),excluded.start_time)," +
				"end_time=coalesce(nullif(excluded.end_time,''),associations.end_time)," +
				"quality=excluded.quality,shared=excluded.shared"},
	} {
		steps := []string{
			"CREATE TEMP TABLE telemetry_projection AS SELECT " + projection.fields + " FROM " + projection.table + " ORDER BY " + projection.order + ",rowid",
			"DELETE FROM " + projection.table,
			"INSERT INTO " + projection.table + " SELECT * FROM telemetry_projection WHERE true ORDER BY rowid ON CONFLICT(" +
				projection.primaryKey + ") DO UPDATE SET " + projection.update + ",body=json_patch(" + projection.table + ".body,excluded.body)",
			"DROP TABLE telemetry_projection",
		}
		if err := runMigrationSteps(tx, key, steps); err != nil {
			return fmt.Errorf("telemetry %s projection: %w", projection.table, err)
		}
	}
	return nil
}

func migrateIdentity(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) < 3 {
		return nil, errInvalidRecord
	}
	secret, ok := args[0].([]byte)
	if !ok || len(secret) != 32 {
		return nil, errInvalidRecord
	}
	key := identityKey(secret)
	text := func(i int) string {
		switch value := args[i].(type) {
		case string:
			return value
		case []byte:
			return string(value)
		default:
			return ""
		}
	}
	switch kind := text(1); kind {
	case "id":
		if args[2] == nil || text(2) == "" {
			return args[2], nil
		}
		return key.id(text(2)), nil
	case "origin":
		if len(args) != 4 {
			return nil, errInvalidRecord
		}
		return identity(key.id).origin(text(2), text(3)), nil
	case "duplicate":
		return fmt.Sprintf("h1:%x", key.digest("legacy-duplicate\x00"+text(2))), nil
	case "old_edge", "old_edge_hash", "edge":
		if len(args) != 6 {
			return nil, errInvalidRecord
		}
		parts := []string{text(2), text(3), text(4)}
		if args[5] != nil {
			parts = append(parts, text(5))
		}
		switch kind {
		case "old_edge":
			return strings.Join(parts, ":"), nil
		case "old_edge_hash":
			return fmt.Sprintf("association:%x", sha256.Sum256([]byte(strings.Join(parts, "\x00")))), nil
		default:
			parts[1], parts[2] = key.id(parts[1]), key.id(parts[2])
			return key.projectionID(parts...), nil
		}
	case "body":
		body, err := decodeBody([]byte(text(2)))
		if err != nil {
			return nil, err
		}
		for name, raw := range body {
			if value, ok := raw.(string); ok && bodyRules[name].kind == "id" && value != "" {
				body[name] = key.id(value)
			}
		}
		data, err := json.Marshal(body)
		if _, plain := args[2].(string); plain {
			return string(data), err
		}
		return data, err
	case "signal":
		if len(args) != 4 {
			return nil, errInvalidRecord
		}
		return transformOTLP(text(2), []byte(text(3)), func(body map[string]any) any {
			visitOTLPAttributes(body, func(attr map[string]any) {
				if attributeRules[stringValue(attr, "key")] == "id" {
					value, _ := attr["value"].(map[string]any)
					if raw := stringValue(value, "stringValue"); raw != "" {
						value["stringValue"] = key.id(raw)
					}
				}
			})
			visitOTLPBinaryIDs(body, wirePolicy{key: key, rekeyBinary: true})
			return body
		})
	}
	return nil, errInvalidRecord
}

func visitOTLPBinaryIDs(value any, policy wirePolicy) {
	switch node := value.(type) {
	case map[string]any:
		for name, child := range node {
			switch name {
			case "traceId":
				if id, ok := child.(string); ok {
					node[name], _ = policy.binaryID(id, 16)
				}
			case "spanId", "parentSpanId":
				if id, ok := child.(string); ok {
					node[name], _ = policy.binaryID(id, 8)
				}
			default:
				visitOTLPBinaryIDs(child, policy)
			}
		}
	case []any:
		for _, child := range node {
			visitOTLPBinaryIDs(child, policy)
		}
	}
}
