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
	s := &Store{db: db, dir: dir, origin: origin, limits: limits, lock: lock, queue: make(chan writeRequest, limits.QueueRequests), done: make(chan struct{})}
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
	if !validID(id) {
		return false
	}
	var n int
	return s.db.QueryRowContext(ctx, "SELECT 1 FROM runs WHERE agent_run_id=?", id).Scan(&n) == nil
}
func (s *Store) Ingest(ctx context.Context, records []Record) error {
	entries := make([]Envelope, 0, len(records))
	for _, r := range records {
		clean, n, err := sanitizeRecord(r)
		if err != nil {
			return err
		}
		body, err := json.Marshal(clean.Body)
		if err != nil {
			return err
		}
		entries = append(entries, Envelope{Schema: ExportSchema, OriginID: "producer:" + clean.SourceID, EventID: clean.EventID, SourceID: clean.SourceID, SourceVersion: clean.SourceVersion, SourceTime: clean.SourceTime, Kind: clean.Kind, Encoding: "json", PolicyVersion: PolicyVersion, Redactions: n, Body: body})
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
			res, err := tx.Exec(`INSERT INTO records(origin_id,event_id,received_time,source_id,source_version,source_time,kind,encoding,policy_version,redactions,body,protobuf,import_policy_version) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(origin_id,event_id) DO NOTHING`, e.OriginID, e.EventID, e.ReceivedTime, e.SourceID, e.SourceVersion, e.SourceTime, e.Kind, e.Encoding, e.PolicyVersion, e.Redactions, []byte(e.Body), e.Protobuf, e.ImportPolicyVersion)
			if err != nil {
				return ErrStorage
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				var body, proto []byte
				var source, version, sourceTime, kind, encoding, capturePolicy string
				if err = tx.QueryRow("SELECT body,protobuf,source_id,source_version,source_time,kind,encoding,policy_version FROM records WHERE origin_id=? AND event_id=?", e.OriginID, e.EventID).Scan(&body, &proto, &source, &version, &sourceTime, &kind, &encoding, &capturePolicy); err != nil {
					return ErrStorage
				}
				if !bytes.Equal(body, e.Body) || !bytes.Equal(proto, e.Protobuf) || source != e.SourceID || version != e.SourceVersion || sourceTime != e.SourceTime || kind != e.Kind || encoding != e.Encoding || capturePolicy != e.PolicyVersion {
					return errors.New("telemetry event identity collision")
				}
				continue
			}
			if err = project(tx, e); err != nil {
				return ErrStorage
			}
		}
	}
	if err = tx.Commit(); err != nil {
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
		if !validID(run) {
			return errors.New("run identity missing")
		}
		// Outbox filenames and replay delivery order are independent of source
		// chronology. Older records fill missing metadata beneath newer state.
		_, err := tx.Exec(`INSERT INTO runs VALUES(?,?,?) ON CONFLICT(agent_run_id) DO UPDATE SET
			body=CASE WHEN excluded.source_time>=runs.source_time THEN json_patch(runs.body,excluded.body) ELSE json_patch(excluded.body,runs.body) END,
			source_time=max(runs.source_time,excluded.source_time)`, run, body, e.SourceTime)
		return err
	case "scope", "process", "allocation":
		if !validID(object) {
			return errors.New("object identity missing")
		}
		_, err := tx.Exec(`INSERT INTO objects VALUES(?,?,?,?,?,?) ON CONFLICT(object_id) DO UPDATE SET
			kind=CASE WHEN excluded.source_time>=objects.source_time THEN excluded.kind ELSE objects.kind END,
			parent_object_id=CASE WHEN excluded.source_time>=objects.source_time THEN coalesce(nullif(excluded.parent_object_id,''),objects.parent_object_id) ELSE coalesce(nullif(objects.parent_object_id,''),excluded.parent_object_id) END,
			accounting=CASE WHEN excluded.source_time>=objects.source_time THEN coalesce(nullif(excluded.accounting,''),objects.accounting) ELSE coalesce(nullif(objects.accounting,''),excluded.accounting) END,
			body=CASE WHEN excluded.source_time>=objects.source_time THEN json_patch(objects.body,excluded.body) ELSE json_patch(excluded.body,objects.body) END,
			source_time=max(objects.source_time,excluded.source_time)`, object, e.Kind, stringValue(b, "parent_object_id"), stringValue(b, "accounting"), body, e.SourceTime)
		if err != nil {
			return err
		}
		if e.Kind == "allocation" {
			_, err = tx.Exec(`INSERT INTO allocations VALUES(?,?,?) ON CONFLICT(object_id) DO UPDATE SET
				body=CASE WHEN excluded.source_time>=allocations.source_time THEN json_patch(allocations.body,excluded.body) ELSE json_patch(excluded.body,allocations.body) END,
				source_time=max(allocations.source_time,excluded.source_time)`, object, body, e.SourceTime)
		}
		if e.Kind == "scope" && validID(run) {
			quality := stringValue(b, "quality")
			if quality == "" && stringValue(b, "scope_quality") == "managed" {
				quality = "explicit"
			}
			edge, _ := json.Marshal(map[string]any{"object_id": object, "agent_run_id": run, "quality": quality, "start_time": e.SourceTime, "shared": b["shared"] == true, "source": "explicit-object-owner"})
			_, err = tx.Exec(`INSERT INTO associations VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(association_id) DO NOTHING`, e.OriginID+":"+object+":"+run, object, run, quality, e.SourceTime, "", b["shared"] == true, string(edge))
		}
		return err
	case "sample":
		if !validID(object) {
			return errors.New("sample object missing")
		}
		_, err := tx.Exec("INSERT INTO samples VALUES(?,?,?,?,?)", e.OriginID, e.EventID, object, e.SourceTime, body)
		return err
	case "association":
		if !validID(object) {
			return errors.New("association object missing")
		}
		id := stringValue(b, "association_id")
		if id == "" {
			id = e.OriginID + ":" + object + ":" + run + ":" + stringValue(b, "source")
		}
		shared, _ := b["shared"].(bool)
		_, err := tx.Exec(`INSERT INTO associations VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(association_id) DO UPDATE SET start_time=coalesce(nullif(associations.start_time,''),excluded.start_time),end_time=coalesce(nullif(excluded.end_time,''),associations.end_time),quality=excluded.quality,body=json_patch(associations.body,excluded.body)`, id, object, run, stringValue(b, "quality"), stringValue(b, "start_time"), stringValue(b, "end_time"), shared, body)
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
		var e Envelope
		err := json.Unmarshal(scanner.Bytes(), &e)
		if err != nil {
			return err
		}
		if e.Schema != ExportSchema || !validID(e.OriginID) || !validID(e.EventID) {
			return errors.New("unsupported export identity")
		}
		if !validID(e.SourceID) || (e.SourceVersion != "" && !safeName.MatchString(e.SourceVersion)) {
			return errors.New("invalid export source")
		}
		if e.PolicyVersion != "" && !capturePolicyVersion.MatchString(e.PolicyVersion) {
			return errors.New("invalid export capture policy")
		}
		capturePolicy := e.PolicyVersion
		if e.ReceivedTime != "" {
			received, parseErr := time.Parse(time.RFC3339Nano, e.ReceivedTime)
			if parseErr != nil {
				return errors.New("invalid export receive time")
			}
			e.ReceivedTime = received.UTC().Format(timestampLayout)
		}
		if e.Encoding == "json" {
			body, decodeErr := decodeBody(e.Body)
			if decodeErr != nil {
				return decodeErr
			}
			clean, n, err := sanitizeRecord(Record{Schema: RecordSchema, SourceID: e.SourceID, EventID: e.EventID, SourceVersion: e.SourceVersion, SourceTime: e.SourceTime, Kind: e.Kind, Body: body})
			if err != nil {
				return err
			}
			e.Body, _ = json.Marshal(clean.Body)
			e.SourceTime = clean.SourceTime
			e.Redactions += n
			e.PolicyVersion = PolicyVersion
			e.Protobuf = nil
		} else if !strings.HasPrefix(e.Encoding, "otlp/") {
			return errors.New("unsupported export encoding")
		} else {
			if e.Kind != e.Encoding {
				return errors.New("export kind does not match encoding")
			}
			e.Body = nil
			if err = SanitizeEnvelope(&e, DefaultPolicy()); err != nil {
				return err
			}
		}
		// Source provenance is a validated claim, never permission to capture.
		// Ignore incoming import metadata and record only our trusted local policy.
		e.ImportPolicyVersion = e.PolicyVersion
		e.PolicyVersion = capturePolicy
		if err = s.submit(ctx, []Envelope{e}); err != nil {
			return err
		}
	}
	return scanner.Err()
}
