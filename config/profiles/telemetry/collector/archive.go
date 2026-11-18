package collector

import (
	"database/sql"
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

	"golang.org/x/sys/unix"
)

var archiveName = regexp.MustCompile(`^archive(-[1-9][0-9]*)?$`)

const (
	cutoverFile    = ".spool-cutover.json"
	pendingArchive = ".telemetry-archive.pending"
)

type cutover struct {
	Device  uint64 `json:"device"`
	Inode   uint64 `json:"inode"`
	Archive string `json:"archive"`
	Nonce   string `json:"nonce"`
	Started string `json:"started"`
}

func readCutover(dir string) (*cutover, error) {
	data, err := readLegacyFile(filepath.Join(dir, cutoverFile), 4096)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c cutover
	if json.Unmarshal(data, &c) != nil || !archiveName.MatchString(c.Archive) ||
		!hex32.MatchString(c.Nonce) || canonicalTimestamp(c.Started) == "" {
		return nil, errors.New("invalid telemetry cutover receipt")
	}
	return &c, nil
}

func (c cutover) owns(path string, mode os.FileMode) bool {
	info, err := os.Lstat(path)
	if err != nil || checkPrivateFile(path, mode) != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Ino == c.Inode && st.Dev == c.Device
}

func (c cutover) publishArchive(dir string) error {
	if _, err := os.Lstat(filepath.Join(dir, pendingArchive)); !os.IsNotExist(err) {
		return errors.New("legacy archive staging collision")
	}
	archive := filepath.Join(dir, c.Archive)
	if _, err := os.Lstat(archive); !os.IsNotExist(err) {
		if err := privateDir(archive); err != nil {
			return err
		}
		if !c.owns(filepath.Join(archive, databaseName), 0400) {
			return errors.New("legacy archive collision")
		}
		return syncArchiveRename(dir, archive)
	}
	if c.Inode == 0 {
		return nil
	}
	if _, err := retiredOutbox(dir); err != nil {
		return err
	}
	stage := filepath.Join(dir, pendingArchive+"-"+c.Nonce)
	stagedDB := filepath.Join(stage, databaseName)
	if c.owns(stagedDB, 0600) || c.owns(stagedDB, 0400) {
		if err := privateDir(stage); err != nil {
			return err
		}
	} else {
		path := filepath.Join(dir, databaseName)
		if !c.owns(path, 0600) {
			return errors.New("legacy telemetry database changed")
		}
		if err := privateDir(stage); err != nil {
			return err
		}
		entries, err := os.ReadDir(stage)
		if err != nil || len(entries) != 0 {
			return errors.New("legacy archive staging collision")
		}
		if err = c.checkpointArchive(dir, path); err != nil {
			return err
		}
		if err = renameArchive(path, stagedDB); err != nil {
			return err
		}
		if err = syncArchiveRename(dir, stage); err != nil {
			return err
		}
	}
	if err := sealDatabase(stagedDB); err != nil {
		return err
	}
	if err := renameArchive(stage, archive); err != nil {
		return err
	}
	return syncArchiveRename(dir, archive)
}

func (c cutover) checkpointArchive(dir, path string) error {
	db, version, err := openWriterDatabase(path)
	if err != nil {
		return err
	}
	defer db.Close()
	if version == storeVersion {
		return errors.New("active telemetry store cannot be archived by legacy cutover")
	}
	if version != 0 {
		var key identityKey
		if version == 4 {
			key, err = storeIdentityKey(db, dir, version)
		} else {
			key, err = loadIdentityKey(dir)
		}
		if err != nil {
			return err
		}
		if err = c.recordHistoricalGaps(db, dir, key, version); err != nil {
			return err
		}
	}
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&journal); err != nil || journal != "delete" {
		return errors.New("cannot checkpoint legacy telemetry for archiving")
	}
	return db.Close()
}

type legacyRun struct {
	ID         string `json:"agent_run_id"`
	Boot       string `json:"boot_id"`
	PID        int    `json:"pid"`
	Reserved   string `json:"manifest_state"`
	unreadable bool
}

func legacyRuns(dir string) ([]legacyRun, error) {
	var runs []legacyRun
	for _, name := range []string{"runs", "runs-v4"} {
		root := filepath.Join(dir, name)
		info, err := os.Lstat(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || info.Mode() != os.ModeDir|0500 {
			return nil, errors.New("legacy run directory is not retired")
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			id, ok := strings.CutSuffix(entry.Name(), ".json")
			if !ok || !legacyID.MatchString(id) || !entry.Type().IsRegular() {
				continue
			}
			data, err := readLegacyFile(filepath.Join(root, entry.Name()), 16384)
			if err != nil {
				return nil, fmt.Errorf("read legacy run manifest: %w", err)
			}
			var run legacyRun
			if json.Unmarshal(data, &run) != nil || run.ID != id {
				// The filename still identifies the run in the old store.
				run = legacyRun{ID: id, unreadable: true}
			}
			runs = append(runs, run)
		}
	}
	return runs, nil
}

func (c cutover) recordHistoricalGaps(db *sql.DB, dir string, key identityKey, version int) error {
	runs, err := legacyRuns(dir)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	store := Store{key: key}
	for _, run := range runs {
		ids := []string{run.ID, key.legacy(run.ID), key.id(run.ID)}
		for _, id := range slices.Compact(ids) {
			var present int
			err = tx.QueryRow(`SELECT 1 FROM runs WHERE agent_run_id=?
				AND json_extract(body,'$.resource_completed') IS NOT 1
				AND coalesce(json_extract(body,'$.state'),'') NOT IN ('not_started','resources_complete','resources_unknown')`, id).Scan(&present)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			e := store.localRecord("gap", "legacy-cutover", "upgrade:"+id, c.Started,
				map[string]any{"agent_run_id": id, "reason": "interrupted_by_upgrade", "quality": "partial", "missing": "final_tail"})
			columns := "origin_id,event_id,received_time,source_id,source_version,source_time,kind,encoding,policy_version,redactions,body,protobuf"
			res, err := tx.Exec("INSERT INTO records("+columns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,NULL) ON CONFLICT(origin_id,event_id) DO NOTHING",
				e.OriginID, e.EventID, c.Started, e.SourceID, e.SourceVersion, e.SourceTime, e.Kind, e.Encoding, PolicyVersion, 0, []byte(e.Body))
			if err != nil {
				return err
			}
			count, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if count == 1 {
				if err = key.project(tx, e); err != nil {
					return err
				}
			}
		}
	}
	// Old synthetic markers may be present after a released archive upgrade.
	// The sealed rows now carry the historical evidence.
	if version == 4 {
		if _, err = tx.Exec("DELETE FROM metadata WHERE key='upgrade_interrupted_runs'"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func readLegacyFile(path string, limit int64) ([]byte, error) {
	f, err := openSpoolFile(path, os.O_RDONLY, limit)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit+1))
}

func retiredOutbox(dir string) (string, error) {
	path := filepath.Join(dir, "outbox")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() || info.Mode() != os.ModeDir|os.ModeSticky|0500 {
		return "", errors.New("legacy producer outbox is not retired")
	}
	return path, nil
}

func (c cutover) importLegacy(tx *sql.Tx, dir string, key identityKey, policy Policy) error {
	runs, err := legacyRuns(dir)
	if err != nil {
		return err
	}
	store := Store{key: key}
	unreadable := func(event, run string) error {
		body := map[string]any{"reason": "legacy_history_unreadable", "quality": "partial"}
		if run != "" {
			body["agent_run_id"] = run
		}
		e := store.localRecord("gap", "legacy-cutover", "unreadable:"+event, c.Started, body)
		e.PolicyVersion, e.ReceivedTime = PolicyVersion, c.Started
		return store.putRecord(tx, e)
	}
	boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	for _, run := range runs {
		if run.unreadable {
			if err := unreadable("run:"+run.ID, key.id(run.ID)); err != nil {
				return err
			}
		} else if run.Boot == strings.TrimSpace(string(boot)) && run.PID > 0 && run.Reserved != "reserved" {
			// An unfinished manifest can still have descendants after the main PID exits.
			id := key.id(run.ID)
			e := store.localRecord("run", "legacy-cutover", "registration:"+id, c.Started,
				map[string]any{"agent_run_id": id, "state": "running", "start_time": c.Started})
			e.PolicyVersion, e.ReceivedTime = PolicyVersion, c.Started
			if err = store.putRecord(tx, e); err != nil {
				return err
			}
		}
	}
	producer := "legacy-producer"
	data, err := readLegacyFile(filepath.Join(dir, "producer.json"), frameBytes)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read legacy producer identity: %w", err)
	}
	if err == nil {
		var meta struct {
			Source string `json:"source_id"`
		}
		if json.Unmarshal(data, &meta) == nil && legacyID.MatchString(meta.Source) {
			producer = meta.Source
		}
	}
	add := func(r Record) error {
		e, err := recordEnvelope(r)
		if err != nil {
			return nil // Invalid legacy files remain in the retired directory.
		}
		id := identity(key.id)
		if e.local {
			id = keepID
		}
		e.OriginID, e.SourceID, e.EventID = id.origin(e.OriginID, e.SourceID), id(e.SourceID), id(e.EventID)
		claim := e.PolicyVersion
		if _, err = store.sanitizePayload(&e, policy, id, !e.local); err != nil {
			return nil // Invalid legacy files remain in the retired directory.
		}
		if e.local && claim != "" {
			e.PolicyVersion = claim
		}
		if e.Kind != "" {
			e.ReceivedTime = c.Started
			if err = store.putRecord(tx, e); errors.Is(err, errEventCollision) {
				return nil
			}
		}
		return err
	}
	outbox, err := retiredOutbox(dir)
	if err != nil {
		return err
	}
	if outbox != "" {
		entries, err := os.ReadDir(outbox)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			data, err := readLegacyFile(filepath.Join(outbox, entry.Name()), frameBytes)
			if errors.Is(err, errInvalidRecord) || errors.Is(err, unix.ELOOP) {
				continue
			}
			if err != nil {
				return err
			}
			record, err := key.decodeRecord(data)
			if err == nil && legacyID.MatchString(record.EventID) && legacyID.MatchString(record.SourceID) &&
				entry.Name() == record.EventID+".json" {
				if err = add(record); err != nil {
					return err
				}
			}
		}
	}
	for _, marker := range []struct{ path, reason string }{
		{"runs/health", "run_manifest_capacity"}, {"runs-v4/health", "run_manifest_capacity"},
		{"runs-v4/startup-health", "run_startup_unavailable"}, {"runs-v4/observer-health", "scope_observation_unavailable"},
		{"producer-health.json", "producer_outbox_capacity"},
	} {
		data, err := readLegacyFile(filepath.Join(dir, marker.path), frameBytes)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read legacy coverage marker: %w", err)
		}
		var health struct {
			ID    string `json:"event_id"`
			Count int64  `json:"count"`
			First string `json:"first_time"`
			Last  string `json:"last_time"`
		}
		if json.Unmarshal(data, &health) == nil && legacyID.MatchString(health.ID) && health.Count > 0 &&
			canonicalTimestamp(health.First) != "" && canonicalTimestamp(health.Last) != "" {
			if err = add(Record{Schema: RecordSchema, SourceID: producer, EventID: health.ID,
				SourceVersion: "1", Kind: "gap", SourceTime: health.First, Body: map[string]any{
					"reason": marker.reason, "quality": "partial", "missing": health.Count,
					"coverage_start": health.First, "coverage_end": health.Last,
				}}); err != nil {
				return err
			}
		} else if err = unreadable(marker.path, ""); err != nil {
			return err
		}
	}
	return nil
}

func renameArchive(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}

func syncArchiveRename(dir, archive string) error {
	if err := syncArchiveDirectory(archive); err != nil {
		return err
	}
	return syncArchiveDirectory(dir)
}

func sealDatabase(path string) error {
	sealed := checkPrivateFile(path, 0400) == nil
	if !sealed {
		if err := checkPrivateFile(path, 0600); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	if !sealed {
		err = file.Chmod(0400)
	}
	return errors.Join(err, file.Sync(), file.Close())
}

func syncArchiveDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open telemetry directory for sync: %w", err)
	}
	return errors.Join(dir.Sync(), dir.Close())
}
