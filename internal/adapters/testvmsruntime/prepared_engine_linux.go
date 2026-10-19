package testvmsruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// SealPreparedEngine retains immutable approved bytes for the broker and its
// two owning host installers. The caller still checks the original source
// recipe; retaining a disk descriptor alone would permit an in-place rewrite.
func SealPreparedEngine(source *os.File, expected string) (*os.File, error) {
	if source == nil || len(expected) != sha256.Size*2 {
		return nil, errors.New("invalid prepared engine digest")
	}
	fd, err := unix.MemfdCreate("subyard-prepared-component-engine", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING|unix.MFD_EXEC)
	if errors.Is(err, unix.EINVAL) {
		fd, err = unix.MemfdCreate("subyard-prepared-component-engine", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	}
	if err != nil {
		return nil, err
	}
	sealed := os.NewFile(uintptr(fd), "prepared component engine")
	fail := func(err error) (*os.File, error) { sealed.Close(); return nil, err }
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(sealed, hash), io.NewSectionReader(source, 0, (128<<20)+1))
	if err != nil {
		return fail(err)
	}
	if size > 128<<20 || hex.EncodeToString(hash.Sum(nil)) != expected {
		return fail(errors.New("prepared engine changed while retaining approved bytes"))
	}
	if err := sealed.Chmod(0o500); err != nil {
		return fail(err)
	}
	if _, err := unix.FcntlInt(sealed.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		return fail(err)
	}
	if _, err := sealed.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return sealed, nil
}
