package releasetransition

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const MaxProtectedRecordBytes = 1 << 20

var (
	ErrProtectedStoreStale  = errors.New("protected release transition record is stale")
	ErrProtectedStoreExists = errors.New("protected release transition record already exists")
)

type ProtectedSnapshot struct {
	Exists      bool
	Payload     []byte
	Fingerprint Fingerprint
}

type POSIXV2Store struct {
	configHome string
	fault      func(string) error
}

func NewPOSIXV2Store(configHome string) (*POSIXV2Store, error) {
	clean := filepath.Clean(configHome)
	if !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
		return nil, invalid("release transition config home must be an absolute non-root path")
	}
	return &POSIXV2Store{configHome: clean}, nil
}

func (store *POSIXV2Store) ReadLedger() (ProtectedSnapshot, error) {
	return store.readRecord([]string{"release-transition", "v2"}, "ledger.json")
}

// CompareAndSwapLedger durably replaces the exact observed ledger snapshot.
// The caller must hold Lock across its final observation and this mutation.
func (store *POSIXV2Store) CompareAndSwapLedger(expected ProtectedSnapshot, payload []byte) error {
	return store.compareAndSwap(
		[]string{"release-transition", "v2"}, "ledger.json", expected, payload,
	)
}

func (store *POSIXV2Store) ReadCurrentJournal() (ProtectedSnapshot, error) {
	return store.readRecord([]string{"release-transition", "v2"}, "journal.json")
}

func (store *POSIXV2Store) CompareAndSwapCurrentJournal(
	expected ProtectedSnapshot,
	payload []byte,
) error {
	return store.compareAndSwap(
		[]string{"release-transition", "v2"}, "journal.json", expected, payload,
	)
}

func (store *POSIXV2Store) CreateCheckpointEvidence(
	transaction TransactionID,
	step string,
	checkpoint EvidenceCheckpoint,
	payload []byte,
) error {
	name, err := checkpointEvidenceName(transaction, step, checkpoint)
	if err != nil {
		return err
	}
	return store.createImmutable(
		[]string{"release-transition", "v2", "transactions", string(transaction), "evidence"},
		name, payload,
	)
}

func (store *POSIXV2Store) ReadCompatibilityEvidence(
	identity Fingerprint,
) (ProtectedSnapshot, error) {
	if err := validateFingerprint(identity, "compatibility evidence identity"); err != nil {
		return ProtectedSnapshot{}, err
	}
	return store.readRecord(
		[]string{"release-transition", "v2", "compatibility"}, string(identity)+".json",
	)
}

func (store *POSIXV2Store) CreateCompatibilityEvidence(
	identity Fingerprint,
	payload []byte,
) error {
	if err := validateFingerprint(identity, "compatibility evidence identity"); err != nil {
		return err
	}
	return store.createImmutable(
		[]string{"release-transition", "v2", "compatibility"}, string(identity)+".json", payload,
	)
}

func (store *POSIXV2Store) ReadRecovery(
	transaction TransactionID,
	step string,
) (ProtectedSnapshot, error) {
	name, err := recoveryName(transaction, step)
	if err != nil {
		return ProtectedSnapshot{}, err
	}
	return store.readRecord(
		[]string{"release-transition", "v2", "transactions", string(transaction), "recovery"},
		name,
	)
}

func (store *POSIXV2Store) CreateRecovery(
	transaction TransactionID,
	step string,
	payload []byte,
) error {
	name, err := recoveryName(transaction, step)
	if err != nil {
		return err
	}
	return store.createImmutable(
		[]string{"release-transition", "v2", "transactions", string(transaction), "recovery"},
		name, payload,
	)
}

// CleanupTransactions removes a bounded number of superseded recovery
// transactions. The verified current transaction is retained as the active
// compatibility horizon. Cleanup is deliberately separate from correctness:
// callers run it only after publishing a verified terminal journal.
func (store *POSIXV2Store) CleanupTransactions(current TransactionID) error {
	if store == nil {
		return errors.New("release transition store is required")
	}
	if err := validateTransactionID(current); err != nil {
		return err
	}
	parent, present, err := store.openParent(
		[]string{"release-transition", "v2", "transactions"}, false,
	)
	if err != nil || !present {
		return err
	}
	defer unix.Close(parent)
	root := filepath.Join(store.configHome, "release-transition", "v2", "transactions")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	const cleanupLimit = 32
	removed := 0
	for _, entry := range entries {
		if entry.Name() == string(current) {
			continue
		}
		if removed == cleanupLimit {
			break
		}
		if err := validateTransactionID(TransactionID(entry.Name())); err != nil {
			return errors.New("release transition recovery directory contains an invalid transaction")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			return errors.New("release transition recovery transaction has an unsafe type or mode")
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
		removed++
	}
	if removed != 0 {
		return unix.Fsync(parent)
	}
	return nil
}

// Lock acquires the compatibility-horizon update lock also used by migration
// v1. It is a convergence-only operation and may create the protected lock
// directory and file.
func (store *POSIXV2Store) Lock() (func(), error) {
	if store == nil {
		return nil, errors.New("release transition store is required")
	}
	parent, present, err := store.openParent([]string{"migrations"}, true)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, errors.New("protected lock parent is unavailable")
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(
		parent, "update.lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600,
	)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = unix.Close(fd)
		}
	}()
	if err := validateProtectedFileFD(fd, false); err != nil {
		return nil, err
	}
	if err := unix.Fsync(parent); err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return nil, err
	}
	closeOnError = false
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
	}, nil
}

func (store *POSIXV2Store) compareAndSwap(
	parents []string,
	name string,
	expected ProtectedSnapshot,
	payload []byte,
) error {
	if store == nil {
		return errors.New("release transition store is required")
	}
	if err := validateProtectedPayload(payload); err != nil {
		return err
	}
	if !validProtectedSnapshot(expected) {
		return errors.New("expected protected record snapshot is invalid")
	}
	parent, present, err := store.openParent(parents, true)
	if err != nil {
		return err
	}
	if !present {
		return errors.New("protected record parent is unavailable")
	}
	defer unix.Close(parent)
	if err := discardOrphanedCASPendingAt(parent, name, expected, payload); err != nil {
		return err
	}
	return store.publishAt(parent, name, expected, payload, ErrProtectedStoreStale)
}

func discardOrphanedCASPendingAt(
	parent int,
	name string,
	expected ProtectedSnapshot,
	payload []byte,
) error {
	pendingName := "." + name + ".pending"
	pending, err := readProtectedAt(parent, pendingName)
	if err != nil || !pending.Exists || bytes.Equal(pending.Payload, payload) {
		return err
	}
	current, err := readProtectedAt(parent, name)
	if err != nil {
		return err
	}
	if !sameProtectedSnapshot(current, expected) {
		return ErrProtectedStoreStale
	}
	if err := unix.Unlinkat(parent, pendingName, 0); err != nil {
		return err
	}
	return unix.Fsync(parent)
}

func (store *POSIXV2Store) createImmutable(
	parents []string,
	name string,
	payload []byte,
) error {
	if store == nil {
		return errors.New("release transition store is required")
	}
	if err := validateProtectedPayload(payload); err != nil {
		return err
	}
	parent, present, err := store.openParent(parents, true)
	if err != nil {
		return err
	}
	if !present {
		return errors.New("protected immutable record parent is unavailable")
	}
	defer unix.Close(parent)
	current, err := readProtectedAt(parent, name)
	if err != nil {
		return err
	}
	if current.Exists {
		if bytes.Equal(current.Payload, payload) {
			return unix.Fsync(parent)
		}
		return ErrProtectedStoreExists
	}
	return store.publishAt(parent, name, current, payload, ErrProtectedStoreExists)
}

func checkpointEvidenceName(
	transaction TransactionID,
	step string,
	checkpoint EvidenceCheckpoint,
) (string, error) {
	if err := validateTransactionID(transaction); err != nil {
		return "", err
	}
	if err := validateSafeID(step, "evidence step ID"); err != nil {
		return "", err
	}
	if checkpoint != EvidenceCaptured && checkpoint != EvidenceApplied &&
		checkpoint != EvidenceVerified {
		return "", invalid("unknown evidence checkpoint %q", checkpoint)
	}
	return step + "." + string(checkpoint) + ".json", nil
}

func recoveryName(transaction TransactionID, step string) (string, error) {
	if err := validateTransactionID(transaction); err != nil {
		return "", err
	}
	if err := validateSafeID(step, "recovery step ID"); err != nil {
		return "", err
	}
	return step + ".json", nil
}

func (store *POSIXV2Store) readRecord(parents []string, name string) (ProtectedSnapshot, error) {
	if store == nil {
		return ProtectedSnapshot{}, errors.New("release transition store is required")
	}
	parent, present, err := store.openParent(parents, false)
	if err != nil {
		return ProtectedSnapshot{}, err
	}
	if !present {
		return absentProtectedSnapshot(), nil
	}
	defer unix.Close(parent)
	return readProtectedAt(parent, name)
}

func (store *POSIXV2Store) openParent(parts []string, create bool) (int, bool, error) {
	if store == nil {
		return -1, false, errors.New("release transition store is required")
	}
	root, err := unix.Open(
		store.configHome, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if errors.Is(err, unix.ENOENT) && !create {
		return -1, false, nil
	}
	if err != nil {
		return -1, false, err
	}
	if err := validateProtectedDirectoryFD(root, false); err != nil {
		_ = unix.Close(root)
		return -1, false, err
	}
	current := root
	for _, part := range parts {
		if err := validateSafeID(part, "protected directory component"); err != nil {
			_ = unix.Close(current)
			return -1, false, err
		}
		next, openErr := unix.Openat(
			current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
		)
		if errors.Is(openErr, unix.ENOENT) && !create {
			_ = unix.Close(current)
			return -1, false, nil
		}
		if errors.Is(openErr, unix.ENOENT) && create {
			if mkdirErr := unix.Mkdirat(current, part, 0o700); mkdirErr != nil &&
				!errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(current)
				return -1, false, mkdirErr
			}
			if syncErr := unix.Fsync(current); syncErr != nil {
				_ = unix.Close(current)
				return -1, false, syncErr
			}
			next, openErr = unix.Openat(
				current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
			)
		}
		if openErr != nil {
			_ = unix.Close(current)
			return -1, false, openErr
		}
		if err := validateProtectedDirectoryFD(next, true); err != nil {
			_ = unix.Close(next)
			_ = unix.Close(current)
			return -1, false, err
		}
		_ = unix.Close(current)
		current = next
	}
	return current, true, nil
}

func (store *POSIXV2Store) publishAt(
	parent int,
	name string,
	expected ProtectedSnapshot,
	payload []byte,
	conflict error,
) error {
	current, err := readProtectedAt(parent, name)
	if err != nil {
		return err
	}
	if !sameProtectedSnapshot(current, expected) {
		return conflict
	}
	pending := "." + name + ".pending"
	if err := preparePendingAt(parent, pending, payload); err != nil {
		if errors.Is(err, ErrProtectedStoreExists) {
			return conflict
		}
		return err
	}
	if err := store.inject("after-pending-fsync"); err != nil {
		return err
	}
	current, err = readProtectedAt(parent, name)
	if err != nil {
		return err
	}
	if !sameProtectedSnapshot(current, expected) {
		return conflict
	}
	if err := store.inject("before-publish"); err != nil {
		return err
	}
	if expected.Exists {
		err = unix.Renameat(parent, pending, parent, name)
	} else {
		err = unix.Renameat2(parent, pending, parent, name, unix.RENAME_NOREPLACE)
	}
	if errors.Is(err, unix.EEXIST) {
		return conflict
	}
	if err != nil {
		return err
	}
	if err := store.inject("after-publish-before-dir-fsync"); err != nil {
		return err
	}
	return unix.Fsync(parent)
}

func (store *POSIXV2Store) inject(point string) error {
	if store != nil && store.fault != nil {
		return store.fault(point)
	}
	return nil
}

func preparePendingAt(parent int, name string, payload []byte) error {
	fd, err := unix.Openat(
		parent, name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if errors.Is(err, unix.EEXIST) {
		current, readErr := readProtectedAt(parent, name)
		if readErr != nil {
			return readErr
		}
		if current.Exists && bytes.Equal(current.Payload, payload) {
			return nil
		}
		return ErrProtectedStoreExists
	}
	if err != nil {
		return err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = unix.Close(fd)
		}
	}()
	if err := validateProtectedFileFD(fd, false); err != nil {
		return err
	}
	for remaining := payload; len(remaining) > 0; {
		written, writeErr := unix.Write(fd, remaining)
		if writeErr != nil {
			return writeErr
		}
		remaining = remaining[written:]
	}
	if err := unix.Fsync(fd); err != nil {
		return err
	}
	if err := unix.Close(fd); err != nil {
		return err
	}
	closeOnError = false
	return unix.Fsync(parent)
}

func readProtectedAt(parent int, name string) (ProtectedSnapshot, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return absentProtectedSnapshot(), nil
	}
	if err != nil {
		return ProtectedSnapshot{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if err := validateProtectedFileFD(fd, true); err != nil {
		return ProtectedSnapshot{}, err
	}
	payload, err := io.ReadAll(io.LimitReader(file, MaxProtectedRecordBytes+1))
	if err != nil {
		return ProtectedSnapshot{}, err
	}
	if len(payload) == 0 || len(payload) > MaxProtectedRecordBytes {
		return ProtectedSnapshot{}, errors.New("protected release transition record size is outside the allowed bound")
	}
	return ProtectedSnapshot{
		Exists: true, Payload: payload, Fingerprint: fingerprintPayload(payload),
	}, nil
}

func validateProtectedDirectoryFD(fd int, exactMode bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	mode := stat.Mode & 0o777
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Getuid()) ||
		(exactMode && mode != 0o700) || (!exactMode && mode&0o022 != 0) {
		return errors.New("protected release transition directory has unsafe type, mode, or ownership")
	}
	return nil
}

func validateProtectedFileFD(fd int, bounded bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 ||
		stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return errors.New("protected release transition record has unsafe type, mode, ownership, or links")
	}
	if bounded && (stat.Size <= 0 || stat.Size > MaxProtectedRecordBytes) {
		return errors.New("protected release transition record size is outside the allowed bound")
	}
	return nil
}

func validateProtectedPayload(payload []byte) error {
	if len(payload) == 0 || len(payload) > MaxProtectedRecordBytes {
		return errors.New("protected release transition record size is outside the allowed bound")
	}
	return nil
}

func absentProtectedSnapshot() ProtectedSnapshot {
	return ProtectedSnapshot{Fingerprint: fingerprintPayload([]byte("protected-record-absent-v1"))}
}

func validProtectedSnapshot(snapshot ProtectedSnapshot) bool {
	if snapshot.Exists {
		return len(snapshot.Payload) > 0 && len(snapshot.Payload) <= MaxProtectedRecordBytes &&
			snapshot.Fingerprint == fingerprintPayload(snapshot.Payload)
	}
	return len(snapshot.Payload) == 0 && snapshot.Fingerprint == absentProtectedSnapshot().Fingerprint
}

func sameProtectedSnapshot(left, right ProtectedSnapshot) bool {
	return left.Exists == right.Exists && left.Fingerprint == right.Fingerprint &&
		bytes.Equal(left.Payload, right.Payload)
}
