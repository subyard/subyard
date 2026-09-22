package ownerinventory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

// HostMutationBusyError means an owner mutation cannot safely proceed while a
// project execution is still using that owner. Callers should retry after the
// in-progress command completes.
type HostMutationBusyError struct {
	HostID string
}

func (err *HostMutationBusyError) Error() string {
	return fmt.Sprintf("OwnerHost %q has a project mutation in progress; retry after it completes", err.HostID)
}

func (store Connections) hostMutationPath(hostID string) string {
	return filepath.Join(store.Root, "mutations", hostID+".lock")
}

func (store Connections) acquireHostMutation(
	ctx context.Context, hostID string, exclusive, nonBlocking bool,
) (func(), error) {
	if err := validateHostID(hostID); err != nil {
		return nil, err
	}
	if err := context.Cause(ctx); err != nil {
		return nil, fmt.Errorf("wait for OwnerHost %q mutation: %w", hostID, err)
	}

	directory := filepath.Dir(store.hostMutationPath(hostID))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	path := store.hostMutationPath(hostID)
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("OwnerHost %q mutation lock is not a regular file", hostID)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("OwnerHost %q mutation lock is not a regular file", hostID)
	}
	operation := syscall.LOCK_SH
	if exclusive {
		operation = syscall.LOCK_EX
	}
	for {
		err = syscall.Flock(int(file.Fd()), operation|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, fmt.Errorf("lock OwnerHost %q mutation: %w", hostID, err)
		}
		if nonBlocking {
			_ = file.Close()
			return nil, &HostMutationBusyError{HostID: hostID}
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("wait for OwnerHost %q mutation: %w", hostID, context.Cause(ctx))
		case <-time.After(5 * time.Millisecond):
		}
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func (store Connections) acquireHostMutations(
	ctx context.Context, hostIDs []string, exclusive, nonBlocking bool,
) (func(), error) {
	hostIDs = append([]string(nil), hostIDs...)
	sort.Strings(hostIDs)
	unique := hostIDs[:0]
	for _, hostID := range hostIDs {
		if len(unique) == 0 || unique[len(unique)-1] != hostID {
			unique = append(unique, hostID)
		}
	}
	releases := make([]func(), 0, len(unique))
	for _, hostID := range unique {
		release, err := store.acquireHostMutation(ctx, hostID, exclusive, nonBlocking)
		if err != nil {
			for index := len(releases) - 1; index >= 0; index-- {
				releases[index]()
			}
			return nil, err
		}
		releases = append(releases, release)
	}
	return func() {
		for index := len(releases) - 1; index >= 0; index-- {
			releases[index]()
		}
	}, nil
}

// BeginHostMutation reserves a registered owner for one project execution.
// The lease validates the whole connection so a prepared route cannot act on a
// removed or repaired owner. Its release function must be called after the
// project state commit or abort.
func (store Connections) BeginHostMutation(ctx context.Context, expected Connection) (func(), error) {
	if err := expected.Validate(); err != nil {
		return nil, err
	}
	// Recovery obtains exclusive host ownership non-blockingly while the store
	// lock is held. Do it before taking the shared project lease so a stale
	// removal journal cannot delete routing state beneath a new execution.
	connectionsMu.Lock()
	releaseStore, err := store.lock()
	if err == nil {
		err = store.recoverPendingLocked()
	}
	if err == nil {
		err = store.validateConnectionLocked(expected)
	}
	if releaseStore != nil {
		releaseStore()
	}
	connectionsMu.Unlock()
	if err != nil {
		return nil, err
	}

	releaseShared, err := store.acquireHostMutation(ctx, expected.HostID, false, false)
	if err != nil {
		return nil, err
	}
	connectionsMu.Lock()
	releaseStore, err = store.lock()
	if err == nil {
		err = store.pendingRecoveryLocked()
	}
	if err == nil {
		err = store.validateConnectionLocked(expected)
	}
	if releaseStore != nil {
		releaseStore()
	}
	connectionsMu.Unlock()
	if err != nil {
		releaseShared()
		return nil, err
	}
	return releaseShared, nil
}

func containsHostID(hostIDs []string, hostID string) bool {
	for _, candidate := range hostIDs {
		if candidate == hostID {
			return true
		}
	}
	return false
}

func (store Connections) acquireRecoveryMutation(heldHostIDs, hostIDs []string) (func(), error) {
	needed := make([]string, 0, len(hostIDs))
	for _, hostID := range hostIDs {
		if !containsHostID(heldHostIDs, hostID) {
			needed = append(needed, hostID)
		}
	}
	if len(needed) == 0 {
		return func() {}, nil
	}
	return store.acquireHostMutations(context.Background(), needed, true, true)
}

func (store Connections) validateConnectionLocked(expected Connection) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	connections, err := store.list()
	if err != nil {
		return err
	}
	expectedDigest, err := connectionDigest(expected)
	if err != nil {
		return err
	}
	for _, current := range connections {
		if current.HostID != expected.HostID {
			continue
		}
		currentDigest, err := connectionDigest(current)
		if err != nil {
			return err
		}
		if currentDigest == expectedDigest {
			return nil
		}
		return fmt.Errorf("%w: owner connection changed before project execution", domain.ErrPlanStale)
	}
	return fmt.Errorf("%w: OwnerHost %q is not registered", domain.ErrPlanStale, expected.HostID)
}

func (store Connections) pendingRecoveryLocked() error {
	for _, path := range []string{
		store.registrationPath(), store.connectionRepairPath(), store.hostIDAdoptionPath(), store.removalPath(),
	} {
		if _, err := os.Lstat(path); err == nil {
			return errors.New("owner inventory recovery is pending; retry project execution")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
