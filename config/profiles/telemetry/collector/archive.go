package collector

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var archiveName = regexp.MustCompile(`^archive(-[1-9][0-9]*)?$`)

const pendingArchive = ".telemetry-archive.pending"

func fenceBeforeUpgrade(dir string) error {
	// A standalone writer cannot stop a service. Require its legacy observer
	// to release the lock before retiring it. The profile does this at quiesce.
	path := filepath.Join(dir, "manager.lock")
	if err := checkPrivateFile(path, 0000); err != nil {
		if err = privateFile(path); err != nil {
			return err
		}
		lock, err := os.OpenFile(path, os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer lock.Close()
		if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return errors.New("stop the legacy telemetry observer before upgrading")
		}
		if err = lock.Chmod(0000); err != nil {
			return err
		}
		if err = lock.Sync(); err != nil {
			return err
		}
	}
	path = filepath.Join(dir, "runs")
	runs, err := os.OpenFile(path, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err == nil {
		defer runs.Close()
		info, err := runs.Stat()
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() ||
			(info.Mode().Perm() != 0700 && info.Mode().Perm() != 0500) {
			return errors.New("invalid legacy run directory")
		}
		if err = runs.Chmod(0500); err != nil {
			return err
		}
		if err = runs.Sync(); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lock, err := lockOutbox(filepath.Join(dir, "outbox"))
	if err != nil {
		return err
	}
	return lock.Close()
}

type metadataWriter interface {
	Exec(string, ...any) (sql.Result, error)
}

type legacyRunManifest struct {
	Run        string `json:"agent_run_id"`
	Boot       string `json:"boot_id"`
	PID        int    `json:"pid"`
	StartTicks uint64 `json:"start_ticks"`
}

func (m legacyRunManifest) alive(boot string) bool {
	if boot == "" || boot != m.Boot || m.PID <= 0 || m.StartTicks == 0 {
		return false
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(m.PID), "stat"))
	if err != nil {
		return false
	}
	end := strings.LastIndex(string(stat), ") ")
	if end < 0 {
		return false
	}
	fields := strings.Fields(string(stat[end+2:]))
	if len(fields) <= 19 || fields[0] == "Z" {
		return false
	}
	return fields[19] == strconv.FormatUint(m.StartTicks, 10)
}

func recordInterruptedRuns(db metadataWriter, dir string, activeKey identityKey) error {
	entries, err := os.ReadDir(filepath.Join(dir, "runs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var runs []string
	var boot string
	identities := []identity{keepID}
	if activeKey != nil {
		data, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
		boot = strings.TrimSpace(string(data))
		identities = []identity{activeKey.id}
	} else if key, err := readIdentityKey(dir); err == nil {
		identities = append(identities, key.legacy)
	}
	for _, entry := range entries {
		run, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !legacyID.MatchString(run) || !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, "runs", entry.Name())
		if checkPrivateFile(path, 0600) != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Size() > 16384 {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var manifest legacyRunManifest
		if json.Unmarshal(data, &manifest) != nil || manifest.Run != run {
			continue
		}
		for _, mapID := range identities {
			runs = append(runs, mapID(run))
		}
		if manifest.alive(boot) {
			// Re-register the living process under its current opaque ID.
			// Historical records and resource counters stay in the archive.
			id := activeKey.id(run)
			body, err := json.Marshal(map[string]string{"agent_run_id": id, "state": "starting"})
			if err != nil {
				return err
			}
			if _, err = db.Exec("INSERT INTO runs(agent_run_id,body,source_time) VALUES(?,?,?)", id, string(body), unknownSourceTime); err != nil {
				return err
			}
		}
	}
	if len(runs) == 0 {
		return nil
	}
	value, err := json.Marshal(runs)
	if err != nil {
		return err
	}
	_, err = db.Exec("INSERT INTO metadata(key,value) VALUES('upgrade_interrupted_runs',?) ON CONFLICT(key) DO NOTHING", string(value))
	return err
}

func archiveStore(dir string, db *sql.DB) error {
	if err := recordInterruptedRuns(db, dir, nil); err != nil {
		return err
	}
	// SQLite checkpoints the WAL when leaving WAL mode. The rename must
	// move a self-contained database even after the previous writer crashed.
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&journal); err != nil || journal != "delete" {
		return errors.New("cannot checkpoint legacy telemetry for archiving")
	}
	if err := db.Close(); err != nil {
		return err
	}
	if err := renameArchive(filepath.Join(dir, databaseName), filepath.Join(dir, pendingArchive)); err != nil {
		return err
	}
	return publishPendingArchive(dir)
}

func renameArchive(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}

func publishPendingArchive(dir string) error {
	pending := filepath.Join(dir, pendingArchive)
	if _, err := os.Lstat(pending); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	// Make the staging rename durable before changing the inode's mode.
	if err := syncArchiveDirectory(dir); err != nil {
		return err
	}
	if err := sealDatabase(pending); err != nil {
		return err
	}
	for sequence := 1; ; sequence++ {
		name := "archive"
		if sequence > 1 {
			name = fmt.Sprintf("archive-%d", sequence)
		}
		archive := filepath.Join(dir, name)
		if err := privateDir(archive); err != nil {
			return err
		}
		target := filepath.Join(archive, databaseName)
		err := renameArchive(pending, target)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		return syncArchiveRename(dir, archive)
	}
}

// Recover archives published by older upgraders and finish directory syncs.
func sealArchives(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if archiveName.MatchString(entry.Name()) {
			if err := sealArchive(dir, filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func sealArchive(dir, archive string) error {
	path := filepath.Join(archive, databaseName)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := privateDir(archive); err != nil {
		return err
	}
	if err := sealDatabase(path); err != nil {
		return err
	}
	return syncArchiveRename(dir, archive)
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
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
