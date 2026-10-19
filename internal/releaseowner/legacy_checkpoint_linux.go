package releaseowner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/sys/unix"
)

type OriginalV1Expectation struct {
	Release, Checkpoint, Transaction string
	RuntimePair                      struct{ Current, Previous string }
}

type OriginalV1FileIdentity struct {
	Device, Inode  uint64
	Mode, UID, GID uint32
	Links          uint64
}

type OriginalV1Record struct {
	Identity OriginalV1FileIdentity `json:"identity"`
	Digest   string                 `json:"digest"`
}

type OriginalV1Checkpoint struct {
	Expectation OriginalV1Expectation `json:"expectation"`
	Journal     OriginalV1Record      `json:"journal"`
	Registry    OriginalV1Record      `json:"registry"`
}

func originalV1Identity(s unix.Stat_t) OriginalV1FileIdentity {
	return OriginalV1FileIdentity{uint64(s.Dev), s.Ino, s.Mode, s.Uid, s.Gid, uint64(s.Nlink)}
}

func sameOriginalV1Record(a, b unix.Stat_t) bool {
	return originalV1Identity(a) == originalV1Identity(b) && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}

func readOriginalV1Record(file *os.File, original OriginalV1Record, mode uint32) ([]byte, error) {
	if file == nil || !fingerprint(original.Digest) {
		return nil, errors.New("native original v1 record scope")
	}
	var before unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &before); err != nil {
		return nil, err
	}
	if originalV1Identity(before) != original.Identity || before.Mode != unix.S_IFREG|mode || before.Uid != uint32(os.Getuid()) || before.Size < 1 || before.Size > 1<<20 {
		return nil, errors.New("native original v1 record identity")
	}
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return nil, err
	}
	if before.Nlink == 0 {
		seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
		required := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
		if err != nil || seals&required != required {
			return nil, errors.New("native original v1 record seals")
		}
	} else if before.Nlink != 1 || flags&unix.O_ACCMODE != unix.O_RDONLY {
		return nil, errors.New("native original v1 record descriptor")
	}
	raw := make([]byte, before.Size)
	for offset := 0; offset < len(raw); {
		n, err := unix.Pread(int(file.Fd()), raw[offset:], int64(offset))
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrUnexpectedEOF
		}
		offset += n
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &after); err != nil {
		return nil, err
	}
	if !sameOriginalV1Record(before, after) || digest(raw) != original.Digest {
		return nil, errors.New("native original v1 record changed")
	}
	return raw, nil
}

func (s *session) observeLegacyCheckpoint(ctx context.Context, request Request, files []*os.File) (Reply, error) {
	value := request.OriginalV1
	if value == nil || len(files) != 2 {
		return Reply{}, errors.New("native original v1 descriptor roles")
	}
	expectation := value.Expectation
	if !domain.SafeID(expectation.Release) || !domain.SafeID(expectation.Checkpoint) || !domain.SafeID(expectation.Transaction) {
		return Reply{}, errors.New("native original v1 expectation syntax")
	}
	for _, role := range []string{expectation.RuntimePair.Current, expectation.RuntimePair.Previous} {
		if !strings.HasPrefix(role, "releases/") || filepath.Clean(role) != role || !domain.SafeID(strings.TrimPrefix(role, "releases/")) {
			return Reply{}, errors.New("native original v1 runtime syntax")
		}
	}
	journal, err := readOriginalV1Record(files[0], value.Journal, 0600)
	if err != nil {
		return Reply{}, err
	}
	registry, err := readOriginalV1Record(files[1], value.Registry, 0644)
	if err != nil {
		return Reply{}, err
	}
	reader, ok := s.components.(interface {
		ObserveOriginalV1Checkpoint(context.Context, OriginalV1Expectation, []byte, []byte) (string, error)
	})
	if !ok {
		return Reply{}, errors.New("native original v1 fact reader unavailable")
	}
	actual, err := reader.ObserveOriginalV1Checkpoint(ctx, expectation, journal, registry)
	if err != nil {
		return Reply{Code: "unavailable"}, nil
	}
	if !fingerprint(actual) {
		return Reply{}, errors.New("native original v1 fact fingerprint")
	}
	if _, err := readOriginalV1Record(files[0], value.Journal, 0600); err != nil {
		return Reply{}, err
	}
	if _, err := readOriginalV1Record(files[1], value.Registry, 0644); err != nil {
		return Reply{}, err
	}
	return Reply{Actual: actual}, nil
}
