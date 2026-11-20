package collector

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	spoolVersion = "subyard-spool/1"
	frameBytes   = 64 << 10
	segmentBytes = 256 << 10
	markerSlot   = 4096
)

var (
	streamID  = regexp.MustCompile(`^(broker|[0-9a-f]{32})$`)
	hex32     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	segmentID = regexp.MustCompile(`^(broker|[0-9a-f]{32})\.[0-9a-f]{32}\.[0-9]{16}$`)
)

type spoolAdvance struct {
	id        string
	old, next int64
	terminal  bool
}

const spoolSchema = `
CREATE TABLE IF NOT EXISTS spool_offsets(id TEXT PRIMARY KEY, offset INTEGER NOT NULL, terminal INTEGER NOT NULL);
`

func (a spoolAdvance) apply(tx *sql.Tx) error {
	if _, err := tx.Exec("INSERT INTO spool_offsets VALUES(?,0,0) ON CONFLICT DO NOTHING", a.id); err != nil {
		return ErrStorage
	}
	res, err := tx.Exec("UPDATE spool_offsets SET offset=?,terminal=? WHERE id=? AND offset=?", a.next, a.terminal, a.id, a.old)
	if err != nil {
		return ErrStorage
	}
	if count, err := res.RowsAffected(); err != nil || count != 1 {
		return ErrStorage
	}
	return nil
}

func lockDirectory(path string, mode int) (*os.File, error) {
	dir, err := os.OpenFile(path, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(dir.Fd()), mode); err != nil {
		dir.Close()
		return nil, err
	}
	return dir, nil
}

func publishPrivate(path string, data []byte) error {
	if err := checkPrivateFile(path, 0); err != nil && !os.IsNotExist(err) {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".telemetry-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectories(dir)
}

func (s *Store) publishCapturePolicy(ctx context.Context, policy Policy) error {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	lock, err := lockDirectory(s.dir, unix.LOCK_EX|unix.LOCK_NB)
	for errors.Is(err, unix.EWOULDBLOCK) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
		lock, err = lockDirectory(s.dir, unix.LOCK_EX|unix.LOCK_NB)
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = privateDir(filepath.Join(s.dir, "spool")); err != nil {
		return err
	}
	var source string
	if err = s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='producer_id'").Scan(&source); err != nil {
		return err
	}
	producer := map[string]any{"source_id": source, "origin_id": s.origin}
	if err = publishState(s.dir, "producer.json", "producer", producer); err != nil {
		return err
	}
	f, err := openSpoolFile(filepath.Join(s.dir, "spool-loss"), os.O_RDWR|os.O_CREATE, 2*markerSlot)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err == nil && info.Size() != 2*markerSlot {
		err = unix.Fallocate(int(f.Fd()), 0, 0, 2*markerSlot)
		if err == nil {
			err = f.Sync()
		}
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	settings := map[string]any{
		"origin_id": s.origin, "policy_version": PolicyVersion,
		"attributes":  toValues(selectedStrings(policy.Attributes, DefaultPolicy().Attributes)),
		"span_names":  toValues(selectedStrings(policy.SpanNames, wireVocabulary["spanName"])),
		"event_names": toValues(selectedStrings(policy.EventNames, wireVocabulary["eventName"])),
	}
	return publishState(s.dir, "spool-policy.json", "producer_policy", settings)
}

func toValues(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func publishState(dir, file, family string, value map[string]any) error {
	clean, err := sanitizeState(family, value)
	if err != nil {
		return err
	}
	data, err := json.Marshal(clean)
	if err != nil {
		return err
	}
	return publishPrivate(filepath.Join(dir, file), data)
}

func openSpoolFile(path string, flags int, limit int64) (*os.File, error) {
	if flags&os.O_CREATE != 0 {
		if err := privateFile(path); err != nil {
			return nil, err
		}
		flags &^= os.O_CREATE
	}
	f, err := os.OpenFile(path, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && (checkPrivateInfo(info, 0600) != nil || info.Size() > limit) {
		err = errInvalidRecord
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func streamForRun(run string) string {
	if run == "" {
		return "broker"
	}
	digest := sha256.Sum256([]byte(run))
	return hex.EncodeToString(digest[:16])
}

func segmentHeader(reader *bufio.Reader) (string, string, string, int64, error) {
	header, err := reader.ReadSlice('\n')
	if err != nil {
		return "", "", "", 0, err
	}
	fields := strings.Fields(string(header))
	if (len(fields) != 4 && len(fields) != 5) || fields[0] != spoolVersion {
		return "", "", "", 0, errInvalidRecord
	}
	id := strings.Join(fields[1:4], ".")
	run := ""
	if len(fields) == 5 {
		run = fields[4]
	}
	if !segmentID.MatchString(id) || (run != "" && !(valueFilter{}).id(run)) || streamForRun(run) != fields[1] {
		return "", "", "", 0, errInvalidRecord
	}
	return fields[1], id, run, int64(len(header)), nil
}

// The launcher publishes the starting frame before exec. An exporter can
// arrive before the periodic drain projects it into runs.
func (s *Store) ensureRun(ctx context.Context, run string, policy Policy) bool {
	if s.RegisteredRun(ctx, run) {
		return true
	}
	spool := filepath.Join(s.dir, "spool")
	stream := streamForRun(run)
	if !s.registerSpoolRun(ctx, spool, stream+".open", run, policy) {
		// The producer or drainer may have rotated the open file. If it
		// retired the sealed file too, the final lookup sees its commit.
		entries, _ := os.ReadDir(spool)
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, stream+".") && strings.HasSuffix(name, ".sealed") &&
				s.registerSpoolRun(ctx, spool, name, run, policy) {
				break
			}
		}
	}
	return s.RegisteredRun(ctx, run)
}

func (s *Store) registerSpoolRun(ctx context.Context, spool, name, run string, policy Policy) bool {
	f, err := openSpoolFile(filepath.Join(spool, name), os.O_RDONLY, segmentBytes)
	if err != nil {
		return false
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, frameBytes)
	stream, id, owner, _, err := segmentHeader(reader)
	if err != nil || owner != run || (name != stream+".open" && name != id+".sealed") {
		return false
	}
	// The first frame is immutable and fsynced before the producer links
	// the file. Read only that frame, even as other writers append to it.
	frame, err := reader.ReadSlice('\n')
	if err != nil {
		return false
	}
	r, err := s.decodeSpoolRecord(stream, frame)
	if err != nil || r.Kind != "run" || stringValue(r.Body, "state") != "starting" ||
		stringValue(r.Body, "agent_run_id") != run {
		return false
	}
	// A request's secret filter applies only to its OTLP payload. The
	// normal drainer will deduplicate this event and advance its offset.
	ctx = context.WithValue(ctx, sensitiveContextKey{}, []string(nil))
	return s.ingest(ctx, []Record{r}, policy) == nil
}

func sealSegment(spool, name string) error {
	path := filepath.Join(spool, name)
	f, err := openSpoolFile(path, os.O_RDONLY, segmentBytes)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return err
	}
	stream, id, _, _, err := segmentHeader(bufio.NewReaderSize(f, 1024))
	if err != nil || stream+".open" != name {
		return errInvalidRecord
	}
	// Holding the inode exclusively makes any incomplete tail safe to retire.
	dir, err := lockDirectory(spool, unix.LOCK_EX|unix.LOCK_NB)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = f.Sync(); err != nil {
		return err
	}
	if err = renameArchive(path, filepath.Join(spool, id+".sealed")); err != nil {
		return err
	}
	return dir.Sync()
}

func (s *Store) drainSpool(ctx context.Context, policy Policy) error {
	spool := filepath.Join(s.dir, "spool")
	failed := s.drainLoss(ctx, policy)
	if errors.Is(failed, unix.EWOULDBLOCK) {
		failed = nil
	}
	entries, err := os.ReadDir(spool)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if name, ok := strings.CutSuffix(entry.Name(), ".open"); ok && streamID.MatchString(name) {
			err := sealSegment(spool, entry.Name())
			if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.EWOULDBLOCK) {
				failed = err
			}
		}
	}
	entries, err = os.ReadDir(spool)
	if err != nil {
		return err
	}
	// Seal publication can race this listing. Sync after the listing so each
	// visible sealed name is durable before advancing its checkpoint.
	if err = syncDirectories(spool); err != nil {
		return err
	}
	blocked := map[string]bool{}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".sealed")
		if !ok || !segmentID.MatchString(id) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		stream, _, _ := strings.Cut(id, ".")
		if !blocked[stream] {
			if err := s.drainSealed(ctx, spool, id, policy); err != nil {
				failed = err
				// Unknown or foreign segments stay in place. Other streams
				// continue even when this stream hits a storage error.
				blocked[stream] = true
			}
		}
	}
	if err := s.reclaimOffsets(spool); err != nil {
		return err
	}
	return failed
}

func (s *Store) offset(id string) (offset int64, terminal bool, err error) {
	err = s.db.QueryRow("SELECT offset,terminal FROM spool_offsets WHERE id=?", id).Scan(&offset, &terminal)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

func (s *Store) drainSealed(ctx context.Context, spool, id string, policy Policy) error {
	path := filepath.Join(spool, id+".sealed")
	f, err := openSpoolFile(path, os.O_RDONLY, segmentBytes)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := s.drainSegment(ctx, f, id, policy); err != nil {
		return err
	}
	original, err := f.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(original, current) {
		return errors.Join(err, errors.New("sealed spool segment changed"))
	}
	return os.Remove(path)
}

func (s *Store) drainSegment(ctx context.Context, f *os.File, name string, policy Policy) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	prefix := io.NewSectionReader(f, 0, info.Size())
	reader := bufio.NewReaderSize(prefix, frameBytes)
	stream, id, run, start, err := segmentHeader(reader)
	if err != nil || id != name {
		return errInvalidRecord
	}
	offset, terminal, err := s.offset(id)
	if err != nil {
		return err
	}
	if !terminal {
		if offset > 0 {
			if _, err = prefix.Seek(offset, io.SeekStart); err != nil {
				return err
			}
			reader.Reset(prefix)
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			frame, readErr := reader.ReadSlice('\n')
			length := len(frame)
			for errors.Is(readErr, bufio.ErrBufferFull) {
				var more []byte
				more, readErr = reader.ReadSlice('\n')
				length += len(more)
			}
			a := spoolAdvance{id: id, old: offset, next: max(start, offset), terminal: errors.Is(readErr, io.EOF)}
			switch {
			case readErr == nil:
				a.next += int64(length)
				if length <= frameBytes {
					err = s.ingestFrame(ctx, a, stream, frame, policy)
				} else {
					err = errInvalidRecord
				}
				if errors.Is(err, errInvalidRecord) {
					err = s.captureAt(ctx, []Envelope{s.spoolGap(id, a.next, run, "spool_invalid_frame", 1, "", "")}, policy, &a)
				}
			case !a.terminal:
				return readErr
			case length > 0:
				err = s.captureAt(ctx, []Envelope{s.spoolGap(id, a.next, run, "spool_torn_tail", 1, "", "")}, policy, &a)
			default:
				err = s.submit(ctx, nil, false, &a)
			}
			if err != nil {
				return err
			}
			offset = a.next
			if a.terminal {
				break
			}
		}
	}
	return nil
}

// Sync removals before dropping their checkpoints, including after a crash
// between unlink and directory sync.
func (s *Store) reclaimOffsets(spool string) error {
	if err := syncDirectories(spool); err != nil {
		return err
	}
	rows, err := s.db.Query("SELECT id FROM spool_offsets WHERE terminal=1 AND id NOT LIKE 'loss:%'")
	if err != nil {
		return err
	}
	var retired []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if _, err := os.Lstat(filepath.Join(spool, id+".sealed")); os.IsNotExist(err) {
			retired = append(retired, id)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range retired {
		if _, err = s.db.Exec("DELETE FROM spool_offsets WHERE id=?", id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ingestFrame(ctx context.Context, a spoolAdvance, stream string, data []byte, policy Policy) error {
	r, err := s.decodeSpoolRecord(stream, data)
	if err != nil {
		return err
	}
	e, err := recordEnvelope(r)
	if err != nil {
		return err
	}
	return s.captureAt(ctx, []Envelope{e}, policy, &a)
}

func (s *Store) decodeSpoolRecord(stream string, data []byte) (Record, error) {
	var r Record
	if err := json.Unmarshal(data, &r); err != nil || r.Schema != RecordSchema || r.OriginID != s.origin {
		return Record{}, errInvalidRecord
	}
	if run := stringValue(r.Body, "agent_run_id"); stream != "broker" && run != "" && streamForRun(run) != stream {
		return Record{}, errInvalidRecord
	}
	if r.Filtered && (len(r.Body) != 0 || !r.hasSessionMetadata()) {
		return Record{}, errInvalidRecord
	}
	return r, nil
}

func (s *Store) spoolGap(id string, offset int64, run, reason string, count int64, first, last string) Envelope {
	body := map[string]any{"reason": reason, "quality": "partial", "missing": count}
	if run != "" && (valueFilter{}).id(run) {
		body["agent_run_id"] = run
	} else {
		body["object_id"] = "producer-spool"
	}
	if first != "" {
		body["coverage_start"], body["coverage_end"] = first, last
	}
	at := first
	if at == "" {
		at = unknownSourceTime
	}
	return s.localRecord("gap", "producer-spool", fmt.Sprintf("%s:%d:%s", id, offset, reason), at, body)
}

func (s *Store) localRecord(kind, source, event, at string, body map[string]any) Envelope {
	data, _ := json.Marshal(body)
	event = fmt.Sprintf("%x", sha256.Sum256([]byte(source+"\x00"+event)))
	return Envelope{OriginID: s.origin, SourceID: source, EventID: event,
		Kind: kind, Encoding: "json", SourceVersion: "1", SourceTime: at, Body: data}
}

type lossMarker struct {
	Revision int64  `json:"revision"`
	Count    int64  `json:"count"`
	ID       string `json:"id"`
	Run      string `json:"run,omitempty"`
	First    string `json:"first"`
	Last     string `json:"last"`
}

func readLoss(f *os.File) (lossMarker, error) {
	data := make([]byte, markerSlot)
	var marker lossMarker
	for _, offset := range []int64{0, markerSlot} {
		if _, err := f.ReadAt(data, offset); err != nil {
			return marker, err
		}
		digest, value, _ := bytes.Cut(bytes.TrimRight(data, "\x00"), []byte(" "))
		var candidate lossMarker
		proof, _ := hex.DecodeString(string(digest))
		checksum := sha256.Sum256(value)
		if !bytes.Equal(proof, checksum[:]) || json.Unmarshal(value, &candidate) != nil || candidate.Revision <= marker.Revision {
			continue
		}
		fields, err := decodeBody(value)
		if err != nil {
			continue
		}
		clean, err := sanitizeState("loss_slot", fields)
		if err == nil && len(fields) == len(clean) {
			marker = candidate
		}
	}
	return marker, nil
}

func (s *Store) drainLoss(ctx context.Context, policy Policy) error {
	f, err := openSpoolFile(filepath.Join(s.dir, "spool-loss"), os.O_RDWR, 2*markerSlot)
	if err != nil {
		return err
	}
	defer f.Close()
	fd := int(f.Fd())
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return err
	}
	marker, err := readLoss(f)
	if err = errors.Join(err, unix.Flock(fd, unix.LOCK_UN)); err != nil {
		return err
	}
	id := "loss:" + marker.ID
	if marker.Count < 0 || (marker.Count > 0 && (!hex32.MatchString(marker.ID) ||
		(marker.Run != "" && !(valueFilter{}).id(marker.Run)) || canonicalTimestamp(marker.First) == "" || canonicalTimestamp(marker.Last) == "")) {
		return errInvalidRecord
	}
	if marker.Count > 0 {
		old, _, err := s.offset(id)
		if err != nil {
			return err
		}
		if marker.Count > old {
			gap := s.spoolGap(id, marker.Count, marker.Run, "producer_spool_capacity", marker.Count-old, marker.First, marker.Last)
			if err = s.captureAt(ctx, []Envelope{gap}, policy, &spoolAdvance{id: id, old: old, next: marker.Count, terminal: true}); err != nil {
				return err
			}
		}
		if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return err
		}
		current, err := readLoss(f)
		if err != nil || current != marker {
			return err // A new count remains available for the next drain.
		}
		clean, err := sanitizeState("loss_slot", map[string]any{"revision": marker.Revision + 1})
		if err != nil {
			return err
		}
		value, _ := json.Marshal(clean)
		data := make([]byte, markerSlot)
		prefix := fmt.Sprintf("%x ", sha256.Sum256(value))
		copy(data[copy(data, prefix):], value)
		if _, err = f.WriteAt(data, ((marker.Revision+1)%2)*markerSlot); err != nil {
			return err
		}
		if err = errors.Join(f.Sync(), unix.Flock(fd, unix.LOCK_UN)); err != nil {
			return err
		}
	}
	_, err = s.db.Exec("DELETE FROM spool_offsets WHERE id LIKE 'loss:%'")
	return err
}

func spoolTime() string { return time.Now().UTC().Format(timestampLayout) }
