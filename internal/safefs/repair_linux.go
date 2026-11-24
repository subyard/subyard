package safefs

import (
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// RepairMode narrows an open regular file or directory by clearing the
// permission bits in deny through the same descriptor. A regular file also
// loses set-ID and sticky bits; a directory keeps its set-group-ID and sticky
// bits, which only steer new entries. A state without those bits, including a
// narrower mode, is left unchanged. Any other
// file type is refused. A needed change is refused for another owner and for
// a regular file with more than one hard link, so a repair never changes an
// inode that is not the caller's alone.
func RepairMode(file *os.File, deny fs.FileMode) error {
	return repairMode(int(file.Fd()), file.Name(), deny)
}

// RepairFD applies RepairMode to a raw descriptor the caller keeps open.
func RepairFD(fd int, path string, deny fs.FileMode) error {
	return repairMode(fd, path, deny)
}

// RepairPath applies RepairMode to path without following a final symlink.
func RepairPath(path string, deny fs.FileMode) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return pathError("open", path, err)
	}
	defer unix.Close(fd)
	return repairMode(fd, path, deny)
}

func repairMode(fd int, path string, deny fs.FileMode) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return pathError("fstat", path, err)
	}
	denied := uint32(deny.Perm())
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		denied |= 0o7000
	case unix.S_IFDIR:
	case unix.S_IFLNK:
		return refusal(path, ErrSymlink)
	default:
		return refusal(path, ErrNotRegular)
	}
	actual := stat.Mode & 0o7777
	switch {
	case actual&denied == 0:
		return nil
	case stat.Uid != uint32(os.Geteuid()):
		return refusal(path, ErrUnsafeOwner)
	case stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Nlink != 1:
		return refusal(path, ErrMultipleLinks)
	}
	return (&directory{fd: fd, path: path}).chmod(actual &^ denied)
}
