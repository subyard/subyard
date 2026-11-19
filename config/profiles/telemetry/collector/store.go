package collector

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

var ErrCapacity = errors.New("telemetry capacity exceeded")
var ErrStorage = errors.New("telemetry storage unavailable")
var errInvalidRecord = errors.New("invalid telemetry record")

type Limits struct {
	QueueRequests int
	QueueBytes    int64
	RequestBytes  int64
	DiskBytes     int64
	Retention     time.Duration
	BatchDelay    time.Duration
}

func DefaultLimits() Limits {
	return Limits{128, 16 << 20, 2 << 20, 1 << 30, 7 * 24 * time.Hour, 5 * time.Millisecond}
}

type writeRequest struct {
	entries []Envelope
	bytes   int64
	result  chan error
	advance *spoolAdvance
}
type Store struct {
	db              *sql.DB
	dir             string
	origin          string
	limits          Limits
	lock            *os.File
	mu              sync.Mutex
	pendingBytes    int64
	pendingRequests int
	closed          bool
	queue           chan writeRequest
	done            chan struct{}
}

func privateDir(path string) error {
	_, before := os.Lstat(path)
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("data directory must be a real directory")
	}
	if os.IsNotExist(before) {
		if err := os.Chmod(path, 0700); err != nil {
			return err
		}
		st, err = os.Lstat(path)
		if err != nil {
			return err
		}
	}
	if st.Mode().Perm() != 0700 {
		return errors.New("data directory must have mode 0700")
	}
	if x, ok := st.Sys().(*syscall.Stat_t); !ok || int(x.Uid) != os.Geteuid() {
		return errors.New("data directory has another owner")
	}
	return nil
}
func privateFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err == nil {
		if err = f.Chmod(0600); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	if !os.IsExist(err) {
		return err
	}
	return checkPrivateFile(path, 0600)
}

func checkPrivateFile(path string, mode os.FileMode) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != mode {
		return fmt.Errorf("telemetry file must be regular with mode %04o", mode)
	}
	if x, ok := st.Sys().(*syscall.Stat_t); !ok || int(x.Uid) != os.Geteuid() || x.Nlink != 1 {
		return errors.New("telemetry file has unsafe ownership or links")
	}
	return nil
}
func Open(dir string, limits Limits) (*Store, error) {
	return openStore(dir, limits)
}

func openStore(dir string, limits Limits) (*Store, error) {
	var absoluteErr error
	dir, absoluteErr = filepath.Abs(dir)
	if absoluteErr != nil {
		return nil, absoluteErr
	}
	if limits.QueueRequests < 1 || limits.QueueBytes < 1 || limits.RequestBytes < 1 || limits.DiskBytes < 1 || limits.Retention <= 0 || limits.BatchDelay <= 0 {
		return nil, errors.New("invalid telemetry bounds")
	}
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, "writer.lock")
	if err := privateFile(lockPath); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("telemetry writer already running")
	}
	fail := func(e error) (*Store, error) { lock.Close(); return nil, e }
	cutover, err := readCutover(dir)
	if err != nil {
		return fail(err)
	}
	if cutover != nil {
		if err = cutover.publishArchive(dir); err != nil {
			return fail(err)
		}
	}
	db, version, err := openWriterDatabase(filepath.Join(dir, databaseName))
	if err != nil {
		return fail(err)
	}
	bad := func(e error) (*Store, error) { db.Close(); return fail(e) }
	if version != 0 && version < storeVersion {
		return bad(errors.New("legacy telemetry requires profile cutover"))
	}
	var journal string
	if err = db.QueryRow("PRAGMA journal_mode=WAL").Scan(&journal); err != nil || journal != "wal" {
		return bad(errors.New("cannot enable telemetry WAL"))
	}
	var syncMode int
	if err = db.QueryRow("PRAGMA synchronous").Scan(&syncMode); err != nil || syncMode != 2 {
		return bad(errors.New("cannot enable FULL synchronization"))
	}
	var pageSize int64
	if err = db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		return bad(err)
	}
	maxPages := limits.DiskBytes / 3 / pageSize
	if maxPages < 64 {
		return bad(errors.New("telemetry disk bound too small"))
	}
	var actualMax int64
	if err = db.QueryRow(fmt.Sprintf("PRAGMA max_page_count=%d", maxPages)).Scan(&actualMax); err != nil {
		return bad(err)
	}
	if actualMax > maxPages {
		return bad(errors.New("existing database exceeds capacity bound"))
	}
	if _, err = db.Exec("PRAGMA wal_autocheckpoint=128"); err != nil {
		return bad(err)
	}
	if version == 0 {
		if err = initializeStore(db); err != nil {
			return bad(err)
		}
		if _, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return bad(err)
		}
	}
	var origin string
	if err = db.QueryRow("SELECT value FROM metadata WHERE key='store_generation_id'").Scan(&origin); err != nil {
		return bad(err)
	}
	s := &Store{db: db, dir: dir, origin: origin, limits: limits, lock: lock, queue: make(chan writeRequest, limits.QueueRequests), done: make(chan struct{})}
	go s.writer()
	return s, nil
}

const databaseName = "telemetry.sqlite"
const storeVersion = 5

func openWriterDatabase(path string) (*sql.DB, int, error) {
	if err := privateFile(path); err != nil {
		return nil, 0, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?_pragma=busy_timeout(1000)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, 0, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version < 0 || (version != 0 && !readableVersion(version)) {
		db.Close()
		return nil, 0, errors.New("unsupported telemetry schema")
	}
	return db, version, nil
}

func initializeStore(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(schema + spoolSchema); err != nil {
		return err
	}
	generation := newID()
	if _, err = tx.Exec("INSERT INTO metadata(key,value) VALUES('origin_id',?),('store_generation_id',?),('producer_id',?)", generation, generation, newID()); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", storeVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

const schema = `CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS records(sequence INTEGER PRIMARY KEY AUTOINCREMENT,origin_id TEXT NOT NULL,event_id TEXT NOT NULL,received_time TEXT NOT NULL,source_id TEXT NOT NULL,source_version TEXT NOT NULL,source_time TEXT,kind TEXT NOT NULL,encoding TEXT NOT NULL,policy_version TEXT NOT NULL,redactions INTEGER NOT NULL,body BLOB,protobuf BLOB,import_policy_version TEXT NOT NULL DEFAULT '',UNIQUE(origin_id,event_id));
CREATE INDEX IF NOT EXISTS records_time ON records(received_time);
CREATE TABLE IF NOT EXISTS filtered_events(origin_id TEXT NOT NULL,event_id TEXT NOT NULL,expires_at TEXT NOT NULL,PRIMARY KEY(origin_id,event_id));
CREATE INDEX IF NOT EXISTS filtered_expiry ON filtered_events(expires_at);
CREATE TABLE IF NOT EXISTS runs(agent_run_id TEXT NOT NULL,body TEXT NOT NULL,source_time TEXT NOT NULL,origin_id TEXT NOT NULL,PRIMARY KEY(origin_id,agent_run_id));
CREATE TABLE IF NOT EXISTS objects(object_id TEXT NOT NULL,kind TEXT NOT NULL,parent_object_id TEXT,accounting TEXT,body TEXT NOT NULL,source_time TEXT NOT NULL,origin_id TEXT NOT NULL,PRIMARY KEY(origin_id,object_id));
CREATE TABLE IF NOT EXISTS samples(origin_id TEXT NOT NULL,event_id TEXT NOT NULL,object_id TEXT NOT NULL,source_time TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(origin_id,event_id));
CREATE INDEX IF NOT EXISTS samples_object ON samples(origin_id,object_id,source_time);
CREATE TABLE IF NOT EXISTS associations(association_id TEXT NOT NULL,object_id TEXT NOT NULL,agent_run_id TEXT,quality TEXT,start_time TEXT,end_time TEXT,shared INTEGER NOT NULL,body TEXT NOT NULL,origin_id TEXT NOT NULL,PRIMARY KEY(origin_id,association_id));
CREATE INDEX IF NOT EXISTS associations_run ON associations(origin_id,agent_run_id);
CREATE TABLE IF NOT EXISTS allocations(object_id TEXT NOT NULL,body TEXT NOT NULL,source_time TEXT NOT NULL,origin_id TEXT NOT NULL,PRIMARY KEY(origin_id,object_id));
CREATE TABLE IF NOT EXISTS gaps(origin_id TEXT NOT NULL,event_id TEXT NOT NULL,object_id TEXT,agent_run_id TEXT,source_time TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(origin_id,event_id));
CREATE TABLE IF NOT EXISTS relations(origin_id TEXT NOT NULL,event_id TEXT NOT NULL,agent_run_id TEXT,session_id TEXT,body TEXT NOT NULL,PRIMARY KEY(origin_id,event_id));
CREATE VIEW IF NOT EXISTS sample_values AS SELECT origin_id,event_id,object_id,source_time,json_extract(body,'$.cpu_ns') AS cpu_ns,json_extract(body,'$.memory_bytes') AS memory_bytes,json_extract(body,'$.rss_bytes') AS rss_bytes,json_extract(body,'$.pss_bytes') AS pss_bytes,json_extract(body,'$.io_read_bytes') AS io_read_bytes,json_extract(body,'$.io_write_bytes') AS io_write_bytes,json_extract(body,'$.generation') AS generation,json_extract(body,'$.counter_generation') AS counter_generation,json_extract(body,'$.monotonic_ns') AS monotonic_ns FROM samples;`

func (s *Store) OriginID() string { return s.origin }
func (s *Store) Limits() Limits   { return s.limits }
func (s *Store) RegisteredRun(ctx context.Context, id string) bool {
	if !(valueFilter{}).id(id) {
		return false
	}
	var n int
	return s.db.QueryRowContext(ctx, "SELECT 1 FROM runs WHERE origin_id=? AND agent_run_id=?", s.origin, id).Scan(&n) == nil
}
func (s *Store) Ingest(ctx context.Context, records []Record) error {
	return s.ingest(ctx, records, DefaultPolicy())
}

func (s *Store) ingest(ctx context.Context, records []Record, policy Policy) error {
	entries := make([]Envelope, 0, len(records))
	for _, r := range records {
		e, err := recordEnvelope(r)
		if err != nil {
			return err
		}
		entries = append(entries, e)
	}
	return s.capture(ctx, entries, policy)
}

func recordEnvelope(r Record) (Envelope, error) {
	if r.Schema != RecordSchema && !(r.Schema == "" && r.SchemaVersion == 1) {
		return Envelope{}, errInvalidRecord
	}
	body, err := json.Marshal(r.Body)
	if err != nil {
		return Envelope{}, errInvalidRecord
	}
	e := Envelope{OriginID: r.OriginID, EventID: r.EventID,
		SourceID: r.SourceID, SourceVersion: r.SourceVersion, SourceTime: r.SourceTime,
		Kind: r.Kind, Encoding: "json", Body: body, Redactions: r.Redactions, filtered: r.Filtered, filter: r.filter}
	return e, nil
}

func (s *Store) capture(ctx context.Context, input []Envelope, p Policy) error {
	return s.captureAt(ctx, input, p, nil)
}

func (s *Store) captureAt(ctx context.Context, input []Envelope, p Policy, advance *spoolAdvance) error {
	entries := make([]Envelope, 0, len(input))
	var replay bool
	for _, e := range input {
		replay = e.replay
		if e.OriginID == "" {
			e.OriginID = s.origin
		}
		if e.EventID == "" && e.native {
			e.EventID = newID()
			e.ReceivedTime = spoolTime()
		}
		e.filter = maps.Clone(e.filter)
		if e.filter == nil {
			e.filter = valueFilter{}
		}
		if secrets, ok := ctx.Value(sensitiveContextKey{}).([]string); ok {
			for _, value := range secrets {
				e.filter[value] = struct{}{}
			}
		}
		filter := maps.Clone(e.filter)
		projected, err := sanitizeEnvelope(&e, p)
		if err != nil {
			if !e.replay || !e.filter.id(e.OriginID) || !e.filter.id(e.EventID) {
				return errInvalidRecord
			}
			// An already claimed pair wins even if its retry would now fail
			// validation. Queue only the pair, never the rejected payload.
			e = Envelope{OriginID: e.OriginID, EventID: e.EventID, invalid: true}
		}
		// Derived session relations pass the same sanitizer as every record;
		// a rejected relation counts as a redaction of its source record.
		relations := make([]Envelope, 0, len(projected))
		for _, relation := range projected {
			relation.filter = maps.Clone(filter)
			if _, err := sanitizeEnvelope(&relation, p); err == nil {
				relations = append(relations, relation)
			} else {
				e.Redactions++
			}
		}
		entries = append(entries, e)
		entries = append(entries, relations...)
	}
	return s.submit(ctx, entries, replay, advance)
}

func (s *Store) submit(ctx context.Context, entries []Envelope, replay bool, advance *spoolAdvance) error {
	if len(entries) == 0 && advance == nil {
		return nil
	}
	var size int64
	for i := range entries {
		e := &entries[i]
		e.Schema = ExportSchema
		if e.OriginID == "" {
			e.OriginID = s.origin
		}
		if e.EventID == "" {
			e.EventID = newID()
		}
		if e.ReceivedTime == "" {
			e.ReceivedTime = time.Now().UTC().Format(timestampLayout)
		}
		size += int64(len(e.Body) + len(e.Protobuf) + 512)
	}
	if !replay && size > s.limits.RequestBytes {
		return ErrCapacity
	}
	r := writeRequest{entries, size, make(chan error, 1), advance}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrStorage
	}
	if s.pendingBytes+size > s.limits.QueueBytes || s.pendingRequests >= s.limits.QueueRequests {
		s.mu.Unlock()
		return ErrCapacity
	}
	select {
	case s.queue <- r:
		s.pendingBytes += size
		s.pendingRequests++
		s.mu.Unlock()
	default:
		s.mu.Unlock()
		return ErrCapacity
	}
	select {
	case err := <-r.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Store) writer() {
	defer close(s.done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		var first writeRequest
		select {
		case r, ok := <-s.queue:
			if !ok {
				return
			}
			first = r
		case <-ticker.C:
			_ = s.prune()
			continue
		}
		batch := []writeRequest{first}
		timer := time.NewTimer(s.limits.BatchDelay)
	collect:
		for first.advance == nil && len(batch) < 64 {
			select {
			case r, ok := <-s.queue:
				if !ok {
					break collect
				}
				batch = append(batch, r)
			case <-timer.C:
				break collect
			}
		}
		timer.Stop()
		err := s.commit(batch)
		for _, r := range batch {
			result := err
			if errors.Is(err, errInvalidRecord) && len(batch) > 1 {
				result = s.commit([]writeRequest{r})
			}
			s.mu.Lock()
			s.pendingBytes -= r.bytes
			s.pendingRequests--
			r.result <- result
			s.mu.Unlock()
		}
	}
}
func (s *Store) diskSize() (int64, error) {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		st, err := os.Stat(filepath.Join(s.dir, databaseName) + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += st.Size()
	}
	return total, nil
}
func (s *Store) commit(batch []writeRequest) error {
	// ponytail: one writer serializes bounded batches; shard only if measured throughput requires it.
	size, err := s.diskSize()
	if err != nil {
		return ErrStorage
	}
	var incoming int64
	for _, r := range batch {
		incoming += r.bytes
	}
	if size+incoming*8+128*1024 > s.limits.DiskBytes {
		if err = s.prune(); err != nil {
			return ErrStorage
		}
		size, err = s.diskSize()
		if err != nil {
			return ErrStorage
		}
		if size+incoming*8+128*1024 > s.limits.DiskBytes {
			return ErrCapacity
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	for _, r := range batch {
		if r.advance != nil {
			if err = r.advance.apply(tx); err != nil {
				return err
			}
		}
		for _, e := range r.entries {
			if err = s.putRecord(tx, e); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}

func (s *Store) putRecord(tx *sql.Tx, e Envelope) error {
	if e.invalid {
		var found bool
		err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM records WHERE origin_id=? AND event_id=?
			UNION ALL SELECT 1 FROM filtered_events WHERE origin_id=? AND event_id=?)`,
			e.OriginID, e.EventID, e.OriginID, e.EventID).Scan(&found)
		if err != nil {
			return ErrStorage
		}
		if !found {
			return errInvalidRecord
		}
		return nil
	}
	if e.filtered {
		_, err := tx.Exec(`INSERT INTO filtered_events
			SELECT ?,?,? WHERE NOT EXISTS(SELECT 1 FROM records WHERE origin_id=? AND event_id=?)
			ON CONFLICT(origin_id,event_id) DO NOTHING`, e.OriginID, e.EventID,
			time.Now().Add(s.limits.Retention).UTC().Format(timestampLayout), e.OriginID, e.EventID)
		if err != nil {
			return ErrStorage
		}
		return nil
	}
	res, err := tx.Exec(`INSERT INTO records(origin_id,event_id,received_time,source_id,source_version,source_time,kind,encoding,policy_version,redactions,body,protobuf,import_policy_version)
		SELECT ?,?,?,?,?,?,?,?,?,?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM filtered_events WHERE origin_id=? AND event_id=?)
		ON CONFLICT(origin_id,event_id) DO NOTHING`, e.OriginID, e.EventID, e.ReceivedTime, e.SourceID, e.SourceVersion,
		e.SourceTime, e.Kind, e.Encoding, e.PolicyVersion, e.Redactions, []byte(e.Body), e.Protobuf, e.ImportPolicyVersion, e.OriginID, e.EventID)
	if err != nil {
		return ErrStorage
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ErrStorage
	}
	if n == 0 {
		return nil
	}
	if err = project(tx, e); err != nil {
		return ErrStorage
	}
	return nil
}

func (s *Store) prune() error {
	cutoff := time.Now().Add(-s.limits.Retention).UTC().Format(timestampLayout)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Lifecycle responsibility outlives high-volume samples. In particular, an
	// expired lease or exited agent is not evidence that its resource stopped.
	if _, err = tx.Exec("DELETE FROM filtered_events WHERE expires_at<=?", spoolTime()); err != nil {
		return err
	}
	_, err = tx.Exec(`WITH expired AS MATERIALIZED (
		SELECT sequence,origin_id,event_id FROM records WHERE received_time<?
			AND (kind='sample' OR kind LIKE 'otlp/%' OR source_id='collector:otlp-correlation')
			ORDER BY sequence LIMIT 1000
	) DELETE FROM records WHERE sequence IN (SELECT sequence FROM expired)
		OR (source_id='collector:otlp-correlation' AND (origin_id,substr(event_id,1,instr(event_id,':')-1))
			IN (SELECT origin_id,event_id FROM expired))`, cutoff)
	if err != nil {
		return err
	}
	// Provisional receiver gaps have no canonical record and are resolved
	// by run updates, independently of sample retention.
	for _, table := range []string{"samples", "relations"} {
		if _, err = tx.Exec("DELETE FROM " + table + " WHERE NOT EXISTS(SELECT 1 FROM records r WHERE r.origin_id=" + table + ".origin_id AND r.event_id=" + table + ".event_id)"); err != nil {
			return err
		}
	}
	for table, key := range map[string]string{"runs": "agent_run_id", "objects": "object_id", "allocations": "object_id"} {
		query := "DELETE FROM " + table + " WHERE NOT EXISTS(SELECT 1 FROM records r WHERE r.origin_id=" + table + ".origin_id AND r.encoding='json' AND json_extract(r.body,'$." + key + "')=" + table + "." + key + ")"
		if _, err = tx.Exec(query); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DELETE FROM associations WHERE (origin_id,object_id) NOT IN(SELECT origin_id,object_id FROM objects)"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	_, err = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}
func (s *Store) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()
	<-s.done
	err := s.db.Close()
	s.lock.Close()
	return err
}
func stringValue(m map[string]any, k string) string { v, _ := m[k].(string); return v }
func project(tx *sql.Tx, e Envelope) error {
	if e.Encoding != "json" {
		return nil
	}
	b, decodeErr := decodeBody(e.Body)
	if decodeErr != nil {
		return decodeErr
	}
	body := string(e.Body)
	object := stringValue(b, "object_id")
	run := stringValue(b, "agent_run_id")
	switch e.Kind {
	case "run":
		if run == "" {
			return errors.New("run identity missing")
		}
		// Outbox filenames and replay delivery order are independent of source
		// chronology. Older records fill missing metadata beneath newer state.
		_, err := tx.Exec(`INSERT INTO runs VALUES(?,?,?,?) ON CONFLICT(origin_id,agent_run_id) DO UPDATE SET
			body=CASE WHEN excluded.source_time>=runs.source_time THEN json_patch(runs.body,excluded.body) ELSE json_patch(excluded.body,runs.body) END,
			source_time=max(runs.source_time,excluded.source_time)`, run, body, e.SourceTime, e.OriginID)
		return err
	case "scope", "process", "allocation":
		if object == "" {
			return errors.New("object identity missing")
		}
		_, err := tx.Exec(`INSERT INTO objects VALUES(?,?,?,?,?,?,?) ON CONFLICT(origin_id,object_id) DO UPDATE SET
			kind=CASE WHEN excluded.source_time>=objects.source_time THEN excluded.kind ELSE objects.kind END,
			parent_object_id=CASE WHEN excluded.source_time>=objects.source_time THEN coalesce(nullif(excluded.parent_object_id,''),objects.parent_object_id) ELSE coalesce(nullif(objects.parent_object_id,''),excluded.parent_object_id) END,
			accounting=CASE WHEN excluded.source_time>=objects.source_time THEN coalesce(nullif(excluded.accounting,''),objects.accounting) ELSE coalesce(nullif(objects.accounting,''),excluded.accounting) END,
			body=CASE WHEN excluded.source_time>=objects.source_time THEN json_patch(objects.body,excluded.body) ELSE json_patch(excluded.body,objects.body) END,
			source_time=max(objects.source_time,excluded.source_time)`,
			object, e.Kind, stringValue(b, "parent_object_id"), stringValue(b, "accounting"), body, e.SourceTime, e.OriginID)
		if err != nil {
			return err
		}
		if e.Kind == "allocation" {
			if _, err = tx.Exec(`INSERT INTO allocations VALUES(?,?,?,?) ON CONFLICT(origin_id,object_id) DO UPDATE SET
				body=CASE WHEN excluded.source_time>=allocations.source_time THEN json_patch(allocations.body,excluded.body) ELSE json_patch(excluded.body,allocations.body) END,
				source_time=max(allocations.source_time,excluded.source_time)`, object, body, e.SourceTime, e.OriginID); err != nil {
				return err
			}
		}
		if e.Kind == "scope" && run != "" {
			quality := stringValue(b, "quality")
			if quality == "" && stringValue(b, "scope_quality") == "managed" {
				quality = "explicit"
			}
			body, _ := (valueFilter{}).body(map[string]any{"object_id": object, "agent_run_id": run, "quality": quality, "start_time": e.SourceTime, "shared": b["shared"] == true, "source": "explicit-object-owner"})
			edge, _ := json.Marshal(body)
			_, err = tx.Exec(`INSERT INTO associations VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(origin_id,association_id) DO NOTHING`, projectionID(e.OriginID, object, run), object, run, quality, e.SourceTime, "", b["shared"] == true, string(edge), e.OriginID)
		}
		return err
	case "sample":
		if object == "" {
			return errors.New("sample object missing")
		}
		_, err := tx.Exec("INSERT INTO samples VALUES(?,?,?,?,?)", e.OriginID, e.EventID, object, e.SourceTime, body)
		return err
	case "association":
		if object == "" {
			return errors.New("association object missing")
		}
		id := stringValue(b, "association_id")
		if id == "" {
			id = projectionID(e.OriginID, object, run, stringValue(b, "source"))
		}
		// Later updates keep the first identity, start and sharing, and may only close the edge.
		shared, _ := b["shared"].(bool)
		_, err := tx.Exec(`INSERT INTO associations VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(origin_id,association_id) DO UPDATE SET
			agent_run_id=coalesce(nullif(associations.agent_run_id,''),excluded.agent_run_id),
			start_time=coalesce(nullif(associations.start_time,''),excluded.start_time),
			end_time=coalesce(nullif(excluded.end_time,''),associations.end_time),
			quality=excluded.quality,body=json_patch(associations.body,excluded.body)`,
			id, object, run, stringValue(b, "quality"), stringValue(b, "start_time"), stringValue(b, "end_time"), shared, body, e.OriginID)
		return err
	case "gap":
		_, err := tx.Exec("INSERT INTO gaps VALUES(?,?,?,?,?,?)", e.OriginID, e.EventID, object, run, e.SourceTime, body)
		return err
	case "relation", "session":
		_, err := tx.Exec("INSERT INTO relations VALUES(?,?,?,?,?)", e.OriginID, e.EventID, run, stringValue(b, "session_id"), body)
		return err
	}
	return nil
}

func projectionID(parts ...string) string {
	return fmt.Sprintf("association:%x", sha256.Sum256([]byte(strings.Join(parts, "\x00"))))
}

func firstNonempty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func readableVersion(version int) bool {
	switch version {
	case 1, 2, 3, 4, storeVersion:
		return true
	}
	return false
}

func readVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || !readableVersion(version) {
		return 0, errors.New("unsupported telemetry schema")
	}
	return version, nil
}

func OpenReadOnly(dir string) (*sql.DB, error) {
	var absoluteErr error
	dir, absoluteErr = filepath.Abs(dir)
	if absoluteErr != nil {
		return nil, absoluteErr
	}
	db, err := sql.Open("subyard_telemetry_readonly", (&url.URL{Scheme: "file", Path: filepath.Join(dir, databaseName)}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(500)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	if _, err := readVersion(context.Background(), db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
func Export(ctx context.Context, db *sql.DB, w io.Writer, after int64, limit int) (int64, error) {
	if limit < 1 || limit > 10000 {
		limit = 1000
	}
	version, err := readVersion(ctx, db)
	if err != nil {
		return after, err
	}
	importPolicyColumn := "import_policy_version"
	if version == 1 {
		importPolicyColumn = "''"
	}
	rows, err := db.QueryContext(ctx, `SELECT sequence,origin_id,event_id,received_time,source_id,source_version,source_time,kind,encoding,policy_version,redactions,body,protobuf,`+importPolicyColumn+` FROM records WHERE sequence>? ORDER BY sequence LIMIT ?`, after, limit)
	if err != nil {
		return after, err
	}
	defer rows.Close()
	encoder := json.NewEncoder(w)
	for rows.Next() {
		e := Envelope{Schema: ExportSchema}
		var body []byte
		if err = rows.Scan(&e.Sequence, &e.OriginID, &e.EventID, &e.ReceivedTime, &e.SourceID, &e.SourceVersion, &e.SourceTime, &e.Kind, &e.Encoding, &e.PolicyVersion, &e.Redactions, &body, &e.Protobuf, &e.ImportPolicyVersion); err != nil {
			return after, err
		}
		e.Body = body
		if err = encoder.Encode(e); err != nil {
			return after, err
		}
		after = e.Sequence
	}
	return after, rows.Err()
}
func (s *Store) Replay(ctx context.Context, r io.Reader) error {
	return s.replay(ctx, r, DefaultPolicy())
}

func (s *Store) replay(ctx context.Context, r io.Reader, p Policy) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), int(s.limits.QueueBytes*2+65536))
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		e, err := replayEnvelope(scanner.Bytes())
		if err != nil {
			return err
		}
		if err = s.capture(ctx, []Envelope{e}, p); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func replayEnvelope(data []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return Envelope{}, errInvalidRecord
	}
	if e.Schema != ExportSchema || e.OriginID == "" || e.EventID == "" {
		return Envelope{}, errInvalidRecord
	}
	fields, err := decodeBody(data)
	if err != nil {
		return Envelope{}, errInvalidRecord
	}
	e.filter = valueFilter{}
	e.filter.discover(fields, false)
	e.replay = true
	return e, nil
}

func sanitizeEnvelope(e *Envelope, p Policy) ([]Envelope, error) {
	f := e.filter
	claim := e.PolicyVersion
	e.ImportPolicyVersion = ""
	e.Redactions = min(max(e.Redactions, 0), 1<<31)
	var projected []Envelope
	if e.Encoding != "json" {
		if e.Kind != e.Encoding || !f.safe(e.Encoding) {
			return nil, errInvalidRecord
		}
		var err error
		projected, err = sanitizeOTLP(e, p, f)
		if err != nil {
			return nil, errInvalidRecord
		}
	}
	if !f.id(e.OriginID) || !f.id(e.EventID) || !f.id(e.SourceID) {
		return nil, errInvalidRecord
	}
	if e.ReceivedTime != "" {
		if at := canonicalTimestamp(e.ReceivedTime); at != "" && f.safe(e.ReceivedTime) {
			e.ReceivedTime = at
		} else {
			e.ReceivedTime = unknownSourceTime
			e.Redactions++
		}
	}
	e.PolicyVersion = policyFingerprint(p)
	if e.Encoding == "json" {
		body, err := decodeBody(e.Body)
		if err != nil {
			return nil, errInvalidRecord
		}
		r := Record{Schema: RecordSchema, OriginID: e.OriginID, SourceID: e.SourceID, EventID: e.EventID,
			SourceVersion: e.SourceVersion, SourceTime: e.SourceTime, Kind: e.Kind, Body: body, Redactions: e.Redactions}
		clean, err := sanitizeRecord(r, p, f)
		if err != nil {
			return nil, err
		}
		e.Kind, e.SourceTime, e.SourceVersion, e.Redactions = clean.Kind, clean.SourceTime, clean.SourceVersion, clean.Redactions
		e.filtered = e.filtered || clean.Filtered
		e.Body, _ = json.Marshal(clean.Body)
		e.Protobuf = nil
		if !r.hasSessionMetadata() {
			e.PolicyVersion = PolicyVersion
		}
	} else {
		e.Body = nil
		if version := sanitizeSourceVersion(e.SourceVersion); version != e.SourceVersion || !f.safe(e.SourceVersion) {
			e.SourceVersion = ""
			e.Redactions++
		}
		if e.SourceTime != "" {
			at := canonicalTimestamp(e.SourceTime)
			if at == "" || !f.safe(e.SourceTime) {
				e.Redactions++
				at = ""
			}
			e.SourceTime = at
		}
	}
	if e.replay {
		e.ImportPolicyVersion = policyFingerprint(p)
		e.PolicyVersion = PolicyVersion
		if capturePolicyVersion.MatchString(claim) && f.safe(claim) {
			e.PolicyVersion = claim
		}
	}
	e.filter = nil
	return projected, nil
}
