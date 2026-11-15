package testyardmigration

import (
	"github.com/Subyard/Subyard/internal/ports"
	"golang.org/x/sys/unix"
)

type PreparedOwnerMember = ports.NativeOwnerMember

func retainedOwnerMember(fd int, before Prepared) (PreparedOwnerMember, error) {
	if err := publishedRegistrationFDMatches(fd, "original owner member", before); err != nil {
		return PreparedOwnerMember{}, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return PreparedOwnerMember{}, err
	}
	return PreparedOwnerMember{Device: st.Dev, Inode: st.Ino, UID: st.Uid, GID: st.Gid, Mode: st.Mode, Links: uint64(st.Nlink), Size: st.Size, ModifiedSeconds: st.Mtim.Sec, ModifiedNanos: st.Mtim.Nsec, ChangedSeconds: st.Ctim.Sec, ChangedNanos: st.Ctim.Nsec, Digest: before.RegistrationDigest}, nil
}

func retainedArchiveMember(object *boundObject, directory bool, before Prepared) (int, error) {
	var fd int
	var err error
	if directory {
		fd, err = unix.Openat(object.fd, "config.env", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	} else {
		fd, err = unix.FcntlInt(uintptr(object.fd), unix.F_DUPFD_CLOEXEC, 0)
	}
	if err != nil {
		return -1, err
	}
	if _, err := retainedOwnerMember(fd, before); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}
