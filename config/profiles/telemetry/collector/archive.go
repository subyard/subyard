package collector

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func archiveStore(dir string, db *sql.DB) error {
	archive := filepath.Join(dir, "archive")
	if err := privateDir(archive); err != nil {
		return err
	}
	target := filepath.Join(archive, databaseName)
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		return errors.New("telemetry archive already exists or is unavailable")
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
	if err := unix.Renameat2(unix.AT_FDCWD, filepath.Join(dir, databaseName), unix.AT_FDCWD, target, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	return sealArchive(dir)
}

// Finish a rename interrupted before the mode change or directory syncs.
// Publish the new active database only after the archive is durable.
func sealArchive(dir string) error {
	archive := filepath.Join(dir, "archive")
	path := filepath.Join(archive, databaseName)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := privateDir(archive); err != nil {
		return err
	}
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
	if err = errors.Join(err, file.Sync(), file.Close()); err != nil {
		return err
	}
	for _, path := range []string{archive, dir} {
		parent, err := os.Open(path)
		if err != nil {
			return err
		}
		if err = errors.Join(parent.Sync(), parent.Close()); err != nil {
			return err
		}
	}
	return nil
}
