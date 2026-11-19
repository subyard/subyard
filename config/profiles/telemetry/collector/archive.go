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
	raw, err := decodeBody(data)
	if err != nil {
		return nil, errors.New("invalid telemetry cutover receipt")
	}
	clean, err := sanitizeState("cutover", raw)
	if err != nil || len(raw) != len(clean) || json.Unmarshal(data, &c) != nil {
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
		if err = c.checkpointArchive(path); err != nil {
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

func (c cutover) checkpointArchive(path string) error {
	db, version, err := openWriterDatabase(path)
	if err != nil {
		return err
	}
	defer db.Close()
	if version == storeVersion {
		return errors.New("active telemetry store cannot be archived by legacy cutover")
	}
	if version != 0 {
		if err = c.recordHistoricalGaps(db); err != nil {
			return err
		}
	}
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&journal); err != nil || journal != "delete" {
		return errors.New("cannot checkpoint legacy telemetry for archiving")
	}
	return db.Close()
}

func (c cutover) recordHistoricalGaps(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var origin string
	if err = tx.QueryRow("SELECT value FROM metadata WHERE key='origin_id'").Scan(&origin); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT agent_run_id,coalesce((
		SELECT origin_id FROM records WHERE kind='run' AND json_extract(records.body,'$.agent_run_id')=runs.agent_run_id
		ORDER BY source_time DESC,sequence DESC LIMIT 1),?) FROM runs
		WHERE json_extract(body,'$.resource_completed') IS NOT 1
		AND json_extract(body,'$.state') IN ('starting','running','main_exited','resources_observing') ORDER BY agent_run_id`, origin)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, runOrigin string
		if err = rows.Scan(&id, &runOrigin); err != nil {
			return err
		}
		store := Store{origin: runOrigin}
		e := store.localRecord("gap", "legacy-cutover", "upgrade:"+id, c.Started,
			map[string]any{"agent_run_id": id, "reason": "interrupted_by_upgrade", "quality": "partial", "missing": "final_tail"})
		e.ReceivedTime, e.filter = c.Started, valueFilter{}
		if _, err = sanitizeEnvelope(&e, DefaultPolicy()); err != nil || !(valueFilter{}).id(id) {
			continue
		}
		res, err := tx.Exec(`INSERT INTO records(origin_id,event_id,received_time,source_id,source_version,source_time,kind,encoding,policy_version,redactions,body,protobuf)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,NULL) ON CONFLICT(origin_id,event_id) DO NOTHING`, e.OriginID, e.EventID, e.ReceivedTime,
			e.SourceID, e.SourceVersion, e.SourceTime, e.Kind, e.Encoding, e.PolicyVersion, e.Redactions, []byte(e.Body))
		if err != nil {
			return err
		}
		count, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if count == 1 {
			if _, err = tx.Exec("INSERT INTO gaps VALUES(?,?,?,?,?,?)", e.OriginID, e.EventID, "", id, e.SourceTime, string(e.Body)); err != nil {
				return err
			}
		}
	}
	if err = rows.Err(); err != nil {
		return err
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
