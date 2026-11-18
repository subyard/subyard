package collector

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
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
	"slices"
	"strings"
	"syscall"
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
	streamID   = regexp.MustCompile(`^(broker|h1:[0-9a-f]{64})$`)
	preparedID = regexp.MustCompile(`^h1:[0-9a-f]{64}$`)
	hex32      = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hex64      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	segmentID  = regexp.MustCompile(`^(broker|h1:[0-9a-f]{64})\.[0-9a-f]{32}\.[0-9]{16}$`)
)

type spoolAdvance struct {
	id, origin, event, payload string
	old, next                  int64
	terminal                   bool
}

const spoolSchema = `
CREATE TABLE IF NOT EXISTS spool_offsets(id TEXT PRIMARY KEY, offset INTEGER NOT NULL, terminal INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS spool_inputs(origin TEXT NOT NULL,event TEXT NOT NULL,payload TEXT NOT NULL,PRIMARY KEY(origin,event));
`

func (a spoolAdvance) apply(tx *sql.Tx) (bool, error) {
	if _, err := tx.Exec("INSERT INTO spool_offsets VALUES(?,0,0) ON CONFLICT DO NOTHING", a.id); err != nil {
		return false, ErrStorage
	}
	res, err := tx.Exec("UPDATE spool_offsets SET offset=?,terminal=? WHERE id=? AND offset=?", a.next, a.terminal, a.id, a.old)
	if err != nil {
		return false, ErrStorage
	}
	if count, err := res.RowsAffected(); err != nil || count != 1 {
		return false, ErrStorage
	}
	if a.payload == "" {
		return false, nil
	}
	return registerInput(tx, a.origin, a.event, a.payload)
}

func registerInput(tx *sql.Tx, origin, event, input string) (bool, error) {
	res, err := tx.Exec("INSERT INTO spool_inputs VALUES(?,?,?) ON CONFLICT DO NOTHING", origin, event, input)
	if err != nil {
		return false, ErrStorage
	}
	count, err := res.RowsAffected()
	if err != nil {
		return false, ErrStorage
	}
	if count == 1 {
		return false, nil
	}
	var payload string
	if err := tx.QueryRow("SELECT payload FROM spool_inputs WHERE origin=? AND event=?", origin, event).Scan(&payload); err != nil {
		return false, ErrStorage
	}
	if input != payload {
		return false, errEventCollision
	}
	return true, nil
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
	if err := checkPrivateFile(path, 0600); err != nil && !os.IsNotExist(err) {
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
	return syncArchiveDirectory(dir)
}

func (s *Store) publishCapturePolicy(policy Policy) error {
	lock, err := lockDirectory(s.dir, unix.LOCK_EX)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = privateDir(filepath.Join(s.dir, "spool")); err != nil {
		return err
	}
	producer := filepath.Join(s.dir, "producer.json")
	if err = checkPrivateFile(producer, 0600); os.IsNotExist(err) {
		data, _ := json.Marshal(map[string]string{"source_id": newID()})
		err = publishPrivate(producer, data)
	}
	if err != nil {
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
	names := append(slices.Clone(sessionAttributeNames), "subyard.agent_run.id", "subyard.agent_run.evidence")
	names = slices.DeleteFunc(names, func(name string) bool { return !slices.Contains(policy.Attributes, name) })
	data, _ := json.Marshal(map[string]any{
		"attributes": names, "key_verifier": s.key.verifier(), "policy_version": policyFingerprint(policy),
	})
	snapshot, _ := json.Marshal(map[string]any{"settings": json.RawMessage(data),
		"signature": fmt.Sprintf("%x", s.key.digest("spool-policy/v1\x00"+string(data)))})
	return publishPrivate(filepath.Join(s.dir, "spool-policy.json"), snapshot)
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
	if err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 ||
			info.Size() > limit || st.Nlink != 1 || int(st.Uid) != os.Geteuid() {
			err = errInvalidRecord
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func segmentHeader(reader *bufio.Reader) (string, string, int64, error) {
	header, err := reader.ReadSlice('\n')
	if err != nil {
		return "", "", 0, err
	}
	fields := strings.Fields(string(header))
	if len(fields) != 4 || fields[0] != spoolVersion {
		return "", "", 0, errInvalidRecord
	}
	id := strings.Join(fields[1:], ".")
	if !segmentID.MatchString(id) {
		return "", "", 0, errInvalidRecord
	}
	return fields[1], id, int64(len(header)), nil
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
	stream, id, _, err := segmentHeader(bufio.NewReaderSize(f, 256))
	if err != nil || stream+".open" != name {
		return errInvalidRecord
	}
	// Holding the inode exclusively makes any incomplete tail safe to retire.
	dir, err := lockDirectory(spool, unix.LOCK_EX)
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
		if name, ok := strings.CutSuffix(entry.Name(), ".open"); ok && streamID.MatchString(name) {
			err := sealSegment(spool, entry.Name())
			if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.EWOULDBLOCK) {
				failed = err
			}
		}
	}
	dir, err := lockDirectory(spool, unix.LOCK_SH)
	if err != nil {
		return err
	}
	entries, err = os.ReadDir(spool)
	err = errors.Join(err, dir.Sync(), dir.Close())
	if err != nil {
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
	stream, id, start, err := segmentHeader(reader)
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
				if errors.Is(err, errInvalidRecord) || errors.Is(err, errEventCollision) {
					reason := "spool_invalid_frame"
					if errors.Is(err, errEventCollision) {
						reason = "spool_event_conflict"
					}
					err = s.captureAt(ctx, []Envelope{s.spoolGap(id, a.next, "broker", reason, 1, "", "")}, policy, &a)
				}
			case !a.terminal:
				return readErr
			case length > 0:
				err = s.captureAt(ctx, []Envelope{s.spoolGap(id, a.next, "broker", "spool_torn_tail", 1, "", "")}, policy, &a)
			default:
				err = s.submit(ctx, nil, policy, false, &a)
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
	if err := syncArchiveDirectory(spool); err != nil {
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
	r, err := s.key.decodeRecord(data)
	if err != nil || !r.authenticated || !hex64.MatchString(r.inputID) || r.Schema != RecordSchema ||
		!kinds[r.Kind] || !preparedID.MatchString(r.SourceID) || !preparedID.MatchString(r.EventID) {
		return errInvalidRecord
	}
	bodyRun := stringValue(r.Body, "agent_run_id")
	if stream != "broker" && bodyRun != "" && bodyRun != stream {
		return errInvalidRecord
	}
	a.origin, a.event, a.payload = "producer:"+r.SourceID, r.EventID, r.inputID
	if r.filtered {
		if len(r.Body) != 0 || !r.hasSessionMetadata() {
			return errInvalidRecord
		}
		return s.submit(ctx, nil, policy, false, &a)
	}
	id := identity(func(value string) string {
		if preparedID.MatchString(value) {
			return value
		}
		return ""
	})
	if _, removed, err := id.cleanRecord(r); err != nil || removed != 0 {
		return errInvalidRecord
	}
	e, err := recordEnvelope(r)
	if err != nil {
		return err
	}
	return s.captureAt(ctx, []Envelope{e}, policy, &a)
}

func (s *Store) spoolGap(id string, offset int64, stream, reason string, count int64, first, last string) Envelope {
	body := map[string]any{"reason": reason, "quality": "partial", "missing": count}
	if stream != "broker" {
		body["agent_run_id"] = stream
	} else {
		body["object_id"] = s.key.id("producer-spool")
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
	source = s.key.id(source)
	return Envelope{OriginID: "producer:" + source, SourceID: source, EventID: s.key.id(event),
		Kind: kind, Encoding: "json", SourceVersion: "1", SourceTime: at, Body: data, local: true}
}

type lossMarker struct {
	Revision int64  `json:"revision"`
	Count    int64  `json:"count"`
	ID       string `json:"id"`
	Stream   string `json:"stream"`
	First    string `json:"first"`
	Last     string `json:"last"`
}

func (key identityKey) readLoss(f *os.File) (lossMarker, error) {
	data := make([]byte, markerSlot)
	var marker lossMarker
	for _, offset := range []int64{0, markerSlot} {
		if _, err := f.ReadAt(data, offset); err != nil {
			return marker, err
		}
		digest, value, _ := bytes.Cut(bytes.TrimRight(data, "\x00"), []byte(" "))
		var candidate lossMarker
		proof, _ := hex.DecodeString(string(digest))
		verified := hmac.Equal(proof, key.digest("spool-loss/v1\x00"+string(value)))
		checksum := sha256.Sum256(value)
		if (verified || bytes.Equal(proof, checksum[:])) && json.Unmarshal(value, &candidate) == nil && candidate.Revision > marker.Revision {
			if !verified {
				candidate.Stream = "broker"
			}
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
	marker, err := s.key.readLoss(f)
	if err = errors.Join(err, unix.Flock(fd, unix.LOCK_UN)); err != nil {
		return err
	}
	id := "loss:" + marker.ID
	if marker.Count < 0 || (marker.Count > 0 && (!hex32.MatchString(marker.ID) ||
		!streamID.MatchString(marker.Stream) || canonicalTimestamp(marker.First) == "" || canonicalTimestamp(marker.Last) == "")) {
		return errInvalidRecord
	}
	if marker.Count > 0 {
		old, _, err := s.offset(id)
		if err != nil {
			return err
		}
		if marker.Count > old {
			gap := s.spoolGap(id, marker.Count, marker.Stream, "producer_spool_capacity", marker.Count-old, marker.First, marker.Last)
			if err = s.captureAt(ctx, []Envelope{gap}, policy, &spoolAdvance{id: id, old: old, next: marker.Count, terminal: true}); err != nil {
				return err
			}
		}
		if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return err
		}
		current, err := s.key.readLoss(f)
		if err != nil || current != marker {
			return err // A new count remains available for the next drain.
		}
		value, _ := json.Marshal(lossMarker{Revision: marker.Revision + 1})
		data := make([]byte, markerSlot)
		prefix := fmt.Sprintf("%x ", s.key.digest("spool-loss/v1\x00"+string(value)))
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
