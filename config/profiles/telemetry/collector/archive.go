package collector

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"golang.org/x/sys/unix"
)

var archiveName = regexp.MustCompile(`^archive(-[1-9][0-9]*)?$`)

func archiveStore(dir string, db *sql.DB) error {
	// SQLite checkpoints the WAL when leaving WAL mode. The rename must
	// move a self-contained database even after the previous writer crashed.
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&journal); err != nil || journal != "delete" {
		return errors.New("cannot checkpoint legacy telemetry for archiving")
	}
	if err := db.Close(); err != nil {
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
		err := unix.Renameat2(unix.AT_FDCWD, filepath.Join(dir, databaseName), unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		return sealArchive(dir, archive)
	}
}

// Finish a rename interrupted before the mode change or directory syncs.
// Publish the new active database only after the archive is durable.
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
