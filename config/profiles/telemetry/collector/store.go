package collector

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

var ErrCapacity = errors.New("telemetry capacity exceeded")
var ErrStorage = errors.New("telemetry storage unavailable")
var errInvalidRecord = errors.New("invalid telemetry record")
var errEventCollision = errors.New("telemetry event identity collision")

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
}
type Store struct {
	db              *sql.DB
	dir             string
	origin          string
	key             identityKey
	limits          Limits
	lock            *os.File
	mu              sync.Mutex
	pendingBytes    int64
	pendingRequests int
	closed          bool
	queue           chan writeRequest
	done            chan struct{}
	outboxMu        sync.Mutex
	outboxPath      string
	outboxAfter     string
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
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return errors.New("telemetry file must be regular with mode 0600")
	}
	if x, ok := st.Sys().(*syscall.Stat_t); !ok || int(x.Uid) != os.Geteuid() || x.Nlink != 1 {
		return errors.New("telemetry file has unsafe ownership or links")
	}
	return nil
}
func Open(dir string, limits Limits) (*Store, error) {
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
	path := filepath.Join(dir, "telemetry.sqlite")
	if err = privateFile(path); err != nil {
		return fail(err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?_pragma=busy_timeout(1000)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)")
	if err != nil {
		return fail(err)
	}
	db.SetMaxOpenConns(1)
	bad := func(e error) (*Store, error) { db.Close(); return fail(e) }
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version > 2 {
		return bad(errors.New("unsupported telemetry schema"))
	}
	var journal string
	if err = db.QueryRow("PRAGMA journal_mode=WAL").Scan(&journal); err != nil || journal != "wal" {
		return bad(errors.New("cannot enable telemetry WAL"))
	}
	var syncMode int
	if err = db.QueryRow("PRAGMA synchronous").Scan(&syncMode); err != nil || syncMode != 2 {
		return bad(errors.New("cannot enable FULL synchronization"))
	}
	if version < 2 {
		tx, e := db.Begin()
		if e != nil {
			return bad(e)
		}
		migration := `ALTER TABLE records ADD COLUMN import_policy_version TEXT NOT NULL DEFAULT ''; PRAGMA user_version=2;`
		if version == 0 {
			migration = schema
		}
		if _, e = tx.Exec(migration); e != nil {
			tx.Rollback()
			return bad(fmt.Errorf("telemetry migration: %w", e))
		}
		if e = tx.Commit(); e != nil {
			return bad(e)
		}
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
	var origin string
	err = db.QueryRow("SELECT value FROM metadata WHERE key='origin_id'").Scan(&origin)
	if errors.Is(err, sql.ErrNoRows) {
		origin = newID()
		_, err = db.Exec("INSERT INTO metadata(key,value) VALUES('origin_id',?)", origin)
	}
	if err != nil {
		return bad(err)
	}
	key, err := loadIdentityKey(dir)
	if err != nil {
		return bad(err)
	}
	origin = key.id(origin)
	if origin == "" {
		return bad(errors.New("invalid telemetry origin"))
	}
	s := &Store{db: db, dir: dir, origin: origin, key: key, limits: limits, lock: lock, queue: make(chan writeRequest, limits.QueueRequests), done: make(chan struct{})}
	go s.writer()
	return s, nil
}

const schema = `CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS records(sequence INTEGER PRIMARY KEY AUTOINCREMENT,origin_id TEXT NOT NULL,event_id TEXT NOT NULL,received_time TEXT NOT NULL,source_id TEXT NOT NULL,source_version TEXT NOT NULL,source_time TEXT,kind TEXT NOT NULL,encoding TEXT NOT NULL,policy_version TEXT NOT NULL,redactions INTEGER NOT NULL,body BLOB,protobuf BLOB,import_policy_version TEXT NOT NULL DEFAULT '',UNIQUE(origin_id,event_id));
CREATE INDEX IF NOT EXISTS records_time ON records(received_time);
CREATE TABLE IF NOT EXISTS runs(agent_run_id TEXT PRIMARY KEY,body TEXT NOT NULL,source_time TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS objects(object_id TEXT PRIMARY KEY,kind TEXT NOT NULL,parent_object_id TEXT,accounting TEXT,body TEXT NOT NULL,source_time TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS samples(origin_id TEXT NOT NULL,event_id TEXT NOT NULL,object_id TEXT NOT NULL,source_time TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(origin_id,event_id));
CREATE INDEX IF NOT EXISTS samples_object ON samples(object_id,source_time);
CREATE TABLE IF NOT EXISTS associations(association_id TEXT PRIMARY KEY,object_id TEXT NOT NULL,agent_run_id TEXT,quality TEXT,start_time TEXT,end_time TEXT,shared INTEGER NOT NULL,body TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS associations_run ON associations(agent_run_id);
CREATE TABLE IF NOT EXISTS allocations(object_id TEXT PRIMARY KEY,body TEXT NOT NULL,source_time TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS gaps(origin_id TEXT NOT NULL,event_id TEXT NOT NULL,object_id TEXT,agent_run_id TEXT,source_time TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(origin_id,event_id));
CREATE TABLE IF NOT EXISTS relations(origin_id TEXT NOT NULL,event_id TEXT NOT NULL,agent_run_id TEXT,session_id TEXT,body TEXT NOT NULL,PRIMARY KEY(origin_id,event_id));
CREATE VIEW IF NOT EXISTS sample_values AS SELECT origin_id,event_id,object_id,source_time,json_extract(body,'$.cpu_ns') AS cpu_ns,json_extract(body,'$.memory_bytes') AS memory_bytes,json_extract(body,'$.rss_bytes') AS rss_bytes,json_extract(body,'$.pss_bytes') AS pss_bytes,json_extract(body,'$.io_read_bytes') AS io_read_bytes,json_extract(body,'$.io_write_bytes') AS io_write_bytes,json_extract(body,'$.generation') AS generation,json_extract(body,'$.counter_generation') AS counter_generation,json_extract(body,'$.monotonic_ns') AS monotonic_ns FROM samples;
PRAGMA user_version=2;`

func (s *Store) OriginID() string { return s.origin }
func (s *Store) Limits() Limits   { return s.limits }
func (s *Store) RegisteredRun(ctx context.Context, id string) bool {
	clean := s.key.id(id)
	if clean == "" {
		return false
	}
	var n int
	return s.db.QueryRowContext(ctx, "SELECT 1 FROM runs WHERE agent_run_id IN (?,?) LIMIT 1", clean, id).Scan(&n) == nil
}
func (s *Store) Ingest(ctx context.Context, records []Record) error {
	entries := make([]Envelope, 0, len(records))
	for _, r := range records {
		clean, n, err := s.key.record(r)
		if err != nil {
			return fmt.Errorf("%w: %v", errInvalidRecord, err)
		}
		body, err := json.Marshal(clean.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", errInvalidRecord, err)
		}
		e := Envelope{Schema: ExportSchema, OriginID: "producer:" + clean.SourceID, EventID: clean.EventID,
			SourceID: clean.SourceID, SourceVersion: clean.SourceVersion, SourceTime: clean.SourceTime,
			Kind: clean.Kind, Encoding: "json", PolicyVersion: PolicyVersion, Redactions: n, Body: body}
		e.rememberLegacyKey("producer:"+r.SourceID, r.EventID)
		entries = append(entries, e)
	}
	return s.submit(ctx, entries)
}
func (s *Store) submit(ctx context.Context, entries []Envelope) error {
	if len(entries) == 0 {
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
	if size > s.limits.RequestBytes {
		return ErrCapacity
	}
	r := writeRequest{entries, size, make(chan error, 1)}
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
		for len(batch) < 64 {
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
		s.mu.Lock()
		for _, r := range batch {
			s.pendingBytes -= r.bytes
			s.pendingRequests--
			r.result <- err
		}
		s.mu.Unlock()
	}
}
func (s *Store) diskSize() (int64, error) {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		st, err := os.Stat(filepath.Join(s.dir, "telemetry.sqlite") + suffix)
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
		for _, e := range r.entries {
			if e.legacyOriginID != "" {
				exists, err := s.checkDuplicate(tx, e, e.legacyOriginID, e.legacyEventID)
				if err != nil {
					return err
				}
				if exists {
					continue
				}
			}
			res, err := tx.Exec(`INSERT INTO records(origin_id,event_id,received_time,source_id,source_version,source_time,kind,encoding,policy_version,redactions,body,protobuf,import_policy_version) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(origin_id,event_id) DO NOTHING`, e.OriginID, e.EventID, e.ReceivedTime, e.SourceID, e.SourceVersion, e.SourceTime, e.Kind, e.Encoding, e.PolicyVersion, e.Redactions, []byte(e.Body), e.Protobuf, e.ImportPolicyVersion)
			if err != nil {
				return ErrStorage
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				exists, err := s.checkDuplicate(tx, e, e.OriginID, e.EventID)
				if err != nil {
					return err
				}
				if !exists {
					return ErrStorage
				}
				continue
			}
			if err = s.key.project(tx, e); err != nil {
				return ErrStorage
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}

func (e *Envelope) rememberLegacyKey(origin, event string) {
	if e.OriginID != origin || e.EventID != event {
		e.legacyOriginID, e.legacyEventID = origin, event
	}
}

func (s *Store) checkDuplicate(tx *sql.Tx, e Envelope, origin, event string) (bool, error) {
	old := Envelope{EventID: event}
	err := tx.QueryRow("SELECT body,protobuf,source_id,source_version,source_time,kind,encoding,policy_version FROM records WHERE origin_id=? AND event_id=?", origin, event).
		Scan((*[]byte)(&old.Body), &old.Protobuf, &old.SourceID, &old.SourceVersion, &old.SourceTime, &old.Kind, &old.Encoding, &old.PolicyVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, ErrStorage
	}
	if s.key.source(old.SourceID) != e.SourceID || old.Kind != e.Kind || old.Encoding != e.Encoding || old.PolicyVersion != e.PolicyVersion {
		return false, errEventCollision
	}
	if !samePayload(old, e) {
		// Compare any retained pre-upgrade bytes under the current policy.
		if err = s.key.sanitizeReplayPayload(&old); err != nil || !samePayload(old, e) {
			return false, errEventCollision
		}
	}
	return true, nil
}

func samePayload(a, b Envelope) bool {
	// A stable producer event may acquire an unknown time on retry after an upgrade.
	producerRetry := b.Encoding == "json" && b.ImportPolicyVersion == "" && b.OriginID == "producer:"+b.SourceID
	sameTime := a.SourceTime == b.SourceTime || (producerRetry && b.SourceTime == unknownSourceTime)
	return a.SourceVersion == b.SourceVersion && sameTime &&
		bytes.Equal(a.Body, b.Body) && bytes.Equal(a.Protobuf, b.Protobuf)
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
	_, err = tx.Exec(`DELETE FROM records WHERE sequence IN (SELECT sequence FROM records WHERE received_time<? AND (kind='sample' OR kind LIKE 'otlp/%') ORDER BY sequence LIMIT 1000)`, cutoff)
	if err != nil {
		return err
	}
	for _, table := range []string{"samples", "gaps", "relations"} {
		if _, err = tx.Exec("DELETE FROM " + table + " WHERE NOT EXISTS(SELECT 1 FROM records r WHERE r.origin_id=" + table + ".origin_id AND r.event_id=" + table + ".event_id)"); err != nil {
			return err
		}
	}
	for table, key := range map[string]string{"runs": "agent_run_id", "objects": "object_id", "allocations": "object_id"} {
		if _, err = tx.Exec("DELETE FROM " + table + " WHERE NOT EXISTS(SELECT 1 FROM records r WHERE r.encoding='json' AND json_extract(r.body,'$." + key + "')=" + table + "." + key + ")"); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DELETE FROM associations WHERE object_id NOT IN(SELECT object_id FROM objects)"); err != nil {
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
func (key identityKey) project(tx *sql.Tx, e Envelope) error {
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
		merged, at, err := key.mergeTimedProjection(tx, "SELECT body,source_time FROM runs WHERE agent_run_id=?", run, b, e.SourceTime)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO runs VALUES(?,?,?) ON CONFLICT(agent_run_id) DO UPDATE SET
			body=excluded.body, source_time=excluded.source_time`, run, merged, at)
		return err
	case "scope", "process", "allocation":
		if object == "" {
			return errors.New("object identity missing")
		}
		kind, parent, accounting := e.Kind, stringValue(b, "parent_object_id"), stringValue(b, "accounting")
		var oldBody, oldTime, oldKind string
		var oldParent, oldAccounting sql.NullString
		err := tx.QueryRow("SELECT body,source_time,kind,parent_object_id,accounting FROM objects WHERE object_id=?", object).
			Scan(&oldBody, &oldTime, &oldKind, &oldParent, &oldAccounting)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		oldTime = canonicalTimestamp(oldTime)
		newer := e.SourceTime >= oldTime
		oldParent.String = key.id(oldParent.String)
		if _, ok := key.field(bodyRules["accounting"], oldAccounting.String); !ok {
			oldAccounting.String = ""
		}
		if newer {
			parent = firstNonempty(parent, oldParent.String)
			accounting = firstNonempty(accounting, oldAccounting.String)
		} else {
			parent = firstNonempty(oldParent.String, parent)
			accounting = firstNonempty(oldAccounting.String, accounting)
			switch oldKind {
			case "scope", "process", "allocation":
				kind = oldKind
			}
		}
		merged, err := key.mergeProjection(oldBody, b, newer)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO objects VALUES(?,?,?,?,?,?) ON CONFLICT(object_id) DO UPDATE SET
			kind=excluded.kind,parent_object_id=excluded.parent_object_id,accounting=excluded.accounting,
			body=excluded.body,source_time=excluded.source_time`,
			object, kind, parent, accounting, merged, max(oldTime, e.SourceTime))
		if err != nil {
			return err
		}
		if e.Kind == "allocation" {
			merged, at, err := key.mergeTimedProjection(tx, "SELECT body,source_time FROM allocations WHERE object_id=?", object, b, e.SourceTime)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(`INSERT INTO allocations VALUES(?,?,?) ON CONFLICT(object_id) DO UPDATE SET
				body=excluded.body,source_time=excluded.source_time`, object, merged, at); err != nil {
				return err
			}
		}
		if e.Kind == "scope" && run != "" {
			quality := stringValue(b, "quality")
			if quality == "" && stringValue(b, "scope_quality") == "managed" {
				quality = "explicit"
			}
			body, _ := key.body(map[string]any{"object_id": object, "agent_run_id": run, "quality": quality, "start_time": e.SourceTime, "shared": b["shared"] == true, "source": "explicit-object-owner"})
			edge, _ := json.Marshal(body)
			_, err = tx.Exec(`INSERT INTO associations VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(association_id) DO NOTHING`, projectionID(e.OriginID, object, run), object, run, quality, e.SourceTime, "", b["shared"] == true, string(edge))
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
		var oldBody, oldObject string
		var oldRun, oldStart, oldEnd sql.NullString
		shared, _ := b["shared"].(bool)
		err := tx.QueryRow("SELECT body,object_id,agent_run_id,start_time,end_time,shared FROM associations WHERE association_id=?", id).
			Scan(&oldBody, &oldObject, &oldRun, &oldStart, &oldEnd, &shared)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			object = firstNonempty(key.id(oldObject), object)
			run = key.id(oldRun.String)
		}
		start := firstNonempty(canonicalTimestamp(oldStart.String), stringValue(b, "start_time"))
		end := firstNonempty(stringValue(b, "end_time"), canonicalTimestamp(oldEnd.String))
		merged, err := key.mergeProjection(oldBody, b, true)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO associations VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(association_id) DO UPDATE SET
			object_id=excluded.object_id,agent_run_id=excluded.agent_run_id,shared=excluded.shared,
			start_time=excluded.start_time,end_time=excluded.end_time,quality=excluded.quality,body=excluded.body`,
			id, object, run, stringValue(b, "quality"), start, end, shared, merged)
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

func (key identityKey) mergeTimedProjection(tx *sql.Tx, query, id string, body map[string]any, at string) (string, string, error) {
	var oldBody, oldTime string
	err := tx.QueryRow(query, id).Scan(&oldBody, &oldTime)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	oldTime = canonicalTimestamp(oldTime)
	merged, err := key.mergeProjection(oldBody, body, at >= oldTime)
	return merged, max(at, oldTime), err
}

func (key identityKey) mergeProjection(oldBody string, incoming map[string]any, newer bool) (string, error) {
	merged := incoming
	if oldBody != "" {
		old, err := decodeBody([]byte(oldBody))
		if err != nil {
			return "", err
		}
		base, patch := old, incoming
		if !newer {
			base, patch = incoming, old
		}
		merged = make(map[string]any, len(bodyRules))
		maps.Copy(merged, base)
		// The schema retains only scalars and lists. Match JSON Merge Patch's
		// deletion semantics for an explicitly null field in the newer patch.
		for name, value := range patch {
			if value == nil {
				delete(merged, name)
			} else {
				merged[name] = value
			}
		}
	}
	clean, _ := key.body(merged)
	data, err := json.Marshal(clean)
	return string(data), err
}

func firstNonempty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func OpenReadOnly(dir string) (*sql.DB, error) {
	var absoluteErr error
	dir, absoluteErr = filepath.Abs(dir)
	if absoluteErr != nil {
		return nil, absoluteErr
	}
	db, err := sql.Open("subyard_telemetry_readonly", (&url.URL{Scheme: "file", Path: filepath.Join(dir, "telemetry.sqlite")}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(500)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	return db, nil
}
func Export(ctx context.Context, db *sql.DB, w io.Writer, after int64, limit int) (int64, error) {
	if limit < 1 || limit > 10000 {
		limit = 1000
	}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return after, err
	}
	// Read-only export remains usable before a v1 database's writer migration.
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
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), int(s.limits.RequestBytes*2+65536))
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		e, err := s.key.replayEnvelope(scanner.Bytes())
		if err != nil {
			return err
		}
		if err = s.submit(ctx, []Envelope{e}); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (key identityKey) replayEnvelope(data []byte) (Envelope, error) {
	var input map[string]json.RawMessage
	if err := json.Unmarshal(data, &input); err != nil {
		return Envelope{}, err
	}
	str := func(field string) string { return jsonString(input[field]) }
	origin, event, source := str("origin_id"), str("event_id"), str("source_id")
	e := Envelope{
		Schema: ExportSchema, OriginID: key.origin(origin, source), EventID: key.id(event),
		SourceID: key.source(source), SourceVersion: str("source_version"), SourceTime: str("source_time"),
		Kind: str("kind"), Encoding: str("encoding"), ReceivedTime: canonicalTimestamp(str("received_time")),
	}
	if str("schema") != ExportSchema || e.OriginID == "" || e.EventID == "" || e.SourceID == "" {
		return Envelope{}, errors.New("unsupported export identity")
	}
	e.rememberLegacyKey(origin, event)
	capturePolicy := str("policy_version")
	if capturePolicy != "" && !capturePolicyVersion.MatchString(capturePolicy) {
		return Envelope{}, errors.New("invalid export capture policy")
	}
	_ = json.Unmarshal(input["redactions"], &e.Redactions)
	e.Redactions = min(max(e.Redactions, 0), math.MaxInt64/2)
	if e.ReceivedTime == "" && str("received_time") != "" {
		e.ReceivedTime = unknownSourceTime
		e.Redactions++
	}
	if e.Encoding == "json" {
		e.Body = input["body"]
	} else if err := json.Unmarshal(input["protobuf"], &e.Protobuf); err != nil {
		return Envelope{}, errors.New("invalid export protobuf")
	}
	if err := key.sanitizeReplayPayload(&e); err != nil {
		return Envelope{}, err
	}
	// Source provenance is a validated claim. Only the local policy controls import.
	e.ImportPolicyVersion = e.PolicyVersion
	e.PolicyVersion = capturePolicy
	return e, nil
}

func (key identityKey) sanitizeReplayPayload(e *Envelope) error {
	if e.Encoding == "json" {
		body, err := decodeBody(e.Body)
		if err != nil {
			return err
		}
		clean, n, err := key.record(Record{Schema: RecordSchema, SourceID: e.SourceID, EventID: e.EventID,
			SourceVersion: e.SourceVersion, SourceTime: e.SourceTime, Kind: e.Kind, Body: body})
		if err != nil {
			return err
		}
		e.Body, _ = json.Marshal(clean.Body)
		e.SourceTime = clean.SourceTime
		e.SourceVersion = clean.SourceVersion
		e.Redactions += n
		e.PolicyVersion = PolicyVersion
		return nil
	}
	if e.Kind != e.Encoding {
		return errors.New("export kind does not match encoding")
	}
	if version := sanitizeSourceVersion(e.SourceVersion); version != e.SourceVersion {
		e.SourceVersion = version
		e.Redactions++
	}
	if sourceTime := canonicalTimestamp(e.SourceTime); sourceTime != e.SourceTime {
		if sourceTime == "" {
			e.Redactions++
		}
		e.SourceTime = sourceTime
	}
	return key.sanitizeOTLP(e, DefaultPolicy())
}
