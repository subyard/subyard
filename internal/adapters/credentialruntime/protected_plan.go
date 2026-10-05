package credentialruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
)

// Protected input is observed as metadata only. Its value is supplied to the
// native workflow after consent and never enters the public plan or RPC.
type protectedFileFact struct {
	Path           string
	Exists         bool
	Mode           os.FileMode
	Size, Modified int64
	Device, Inode  uint64
	UID, GID       uint32
	Changed        syscall.Timespec
}

func protectedFileMetadata(path string) (protectedFileFact, error) {
	fact := protectedFileFact{Path: path}
	if path == "" {
		return fact, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fact, nil
	}
	if err != nil {
		return fact, err
	}
	if !info.Mode().IsRegular() || (info.Mode().Perm() != 0o600 && info.Mode().Perm() != 0o400) {
		return fact, errors.New("unsafe protected credential target")
	}
	fact.Exists, fact.Mode, fact.Size, fact.Modified = true, info.Mode(), info.Size(), info.ModTime().UnixNano()
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		fact.Device, fact.Inode, fact.UID, fact.GID, fact.Changed = uint64(st.Dev), st.Ino, st.Uid, st.Gid, st.Ctim
	}
	return fact, nil
}

type protectedApproval struct {
	source, destination string
	binding             string
}

func (runtime *Runtime) protectedBinding(ctx context.Context, source, destination string) (string, error) {
	records, err := runtime.allRecords(ctx)
	if err != nil {
		return "", err
	}
	peers, err := runtime.peers()
	if err != nil {
		return "", err
	}
	identity, err := runtime.Identity()
	if err != nil {
		return "", err
	}
	input, err := protectedFileMetadata(source)
	if err != nil {
		return "", err
	}
	consumer, err := protectedFileMetadata(destination)
	if err != nil {
		return "", err
	}
	return metadataDigest([]any{records, peers, identity, input, consumer, runtime.consumers}), nil
}

func (runtime *Runtime) captureProtectedApproval(ctx context.Context, source, consumer, zone string) (*protectedApproval, error) {
	destination, _, err := runtime.consumerPath(consumer, zone)
	if err != nil {
		return nil, err
	}
	binding, err := runtime.protectedBinding(ctx, source, destination)
	if err != nil {
		return nil, err
	}
	return &protectedApproval{source: source, destination: destination, binding: binding}, nil
}

func (approval *protectedApproval) check(ctx context.Context, runtime *Runtime) error {
	if approval == nil {
		return nil
	}
	fresh, err := runtime.protectedBinding(ctx, approval.source, approval.destination)
	if err != nil || fresh != approval.binding {
		return fmt.Errorf("%w: protected credential source, consumer or recipient metadata changed", domain.ErrPlanStale)
	}
	return nil
}
