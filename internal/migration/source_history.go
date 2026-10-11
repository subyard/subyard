package migration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

// InspectSourceHistory binds the existing legacy discovery inputs independently
// of V2 completion. Closed records remain history; they confer no authorization.
func InspectSourceHistory(ctx context.Context, options ReleaseOptions) (releasetransition.Fingerprint, error) {
	gate, err := InspectMutationGate(ctx, options)
	if err != nil || gate != nil {
		return "", errors.New("protected legacy migration must be resolved by its original owner before source repair")
	}
	type input struct {
		Present              bool
		Digest               releasetransition.Fingerprint
		Device, Inode, Links uint64
		Mode, UID, GID       uint32
	}
	inputs := map[string]input{}
	read := func(path string, directory bool) error {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			inputs[path] = input{}
			return nil
		}
		if err != nil {
			return err
		}
		if directory {
			if err := validateV1CompatibilityDirectory(path); err != nil {
				return err
			}
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("legacy metadata identity is unavailable")
		}
		value := input{Present: true, Device: uint64(stat.Dev), Inode: stat.Ino, Links: uint64(stat.Nlink), Mode: stat.Mode, UID: stat.Uid, GID: stat.Gid}
		if !directory {
			payload, err := readV1ImportProtectedFile(path, maximumV1CompatibilityBytes)
			if err != nil {
				return err
			}
			value.Digest = fingerprintV1ImportPayload(payload)
		}
		after, err := os.Lstat(path)
		if err != nil || !os.SameFile(info, after) || info.Mode() != after.Mode() || info.Size() != after.Size() || info.ModTime() != after.ModTime() {
			return errors.New("legacy metadata changed during inspection")
		}
		inputs[path] = value
		return nil
	}
	root := migrationRoot(options.ConfigHome)
	for _, path := range []string{root, filepath.Join(root, "transactions")} {
		if err := read(path, true); err != nil {
			return "", err
		}
	}
	if err := read(filepath.Join(root, "state.json"), false); err != nil {
		return "", err
	}
	if !inputs[filepath.Join(root, "state.json")].Present && !inputs[filepath.Join(root, "transactions")].Present {
		// The shared release lock may create this coordination-only directory
		// after consent. Absence of actual history/authorization stays bound.
		delete(inputs, root)
	}
	entries, err := os.ReadDir(filepath.Join(root, "transactions"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if len(entries) > maximumV1CompatibilityJournals {
		return "", errors.New("legacy metadata inventory exceeds its existing bound")
	}
	for _, entry := range entries {
		if !safeRelativePath(entry.Name()) || !entry.IsDir() {
			return "", errors.New("legacy transaction inventory is unsupported")
		}
		directory := filepath.Join(root, "transactions", entry.Name())
		if err := read(directory, true); err != nil {
			return "", err
		}
		path := filepath.Join(directory, "transaction.json")
		tx, _, exists, err := readV1CompatibilityJournal(path)
		if err != nil || !exists || tx.SchemaVersion != migrationStateSchema || tx.Phase != "committed" || path != transactionPath(options.ConfigHome, tx.ToRelease) {
			return "", errors.New("legacy transaction is not closed canonical history")
		}
		if err := read(path, false); err != nil {
			return "", err
		}
		for _, recorded := range tx.Entries {
			if !safeRelativePath(recorded.Recovery) {
				return "", errors.New("legacy recovery input is unsupported")
			}
			if err := read(filepath.Join(directory, filepath.FromSlash(recorded.Recovery)), false); err != nil {
				return "", err
			}
		}
	}
	payload, err := json.Marshal(inputs)
	return fingerprintV1ImportPayload(payload), err
}
