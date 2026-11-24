package credentialmeta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/credential"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/safefs"
)

const maximumRecordSize = 1024 * 1024

// Reader projects only the public metadata fields from the two protected
// credential ledgers. Unknown SOPS and payload fields are deliberately ignored.
type Reader struct {
	Root string
}

func (reader Reader) ReadCredentialStatus(ctx context.Context) (domain.CredentialStatus, error) {
	metadata, err := reader.ListMetadata(ctx)
	if err != nil {
		return domain.CredentialStatus{}, err
	}
	summaries, err := credential.Summarize(metadata)
	if err != nil {
		return domain.CredentialStatus{}, err
	}
	peers, err := reader.readPeerStatus(ctx)
	if err != nil {
		return domain.CredentialStatus{}, err
	}
	return domain.CredentialStatus{Credentials: summaries, Peers: peers}, nil
}

func (reader Reader) ListMetadata(ctx context.Context) ([]domain.CredentialMetadata, error) {
	if !filepath.IsAbs(reader.Root) {
		return nil, errors.New("credential root must be absolute")
	}
	info, err := os.Lstat(reader.Root)
	if errors.Is(err, os.ErrNotExist) {
		return []domain.CredentialMetadata{}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("credential root is not a real directory")
	}
	var result []domain.CredentialMetadata
	for _, ledger := range []string{"shared", "local"} {
		records := filepath.Join(reader.Root, ledger, "records")
		err := filepath.WalkDir(records, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if errors.Is(walkErr, os.ErrNotExist) && path == records {
					return fs.SkipDir
				}
				return walkErr
			}
			if err := context.Cause(ctx); err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("credential metadata path is a symlink: %s", path)
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				return nil
			}
			metadata, err := readMetadata(path)
			if err != nil {
				return err
			}
			result = append(result, metadata)
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read %s credential metadata: %w", ledger, err)
		}
	}
	if err := credential.ValidateRevisions(result); err != nil {
		return nil, err
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].CredentialID != result[right].CredentialID {
			return result[left].CredentialID < result[right].CredentialID
		}
		return result[left].RevisionID < result[right].RevisionID
	})
	return result, nil
}

func readMetadata(path string) (domain.CredentialMetadata, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return domain.CredentialMetadata{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return domain.CredentialMetadata{}, errors.New("credential metadata is not a regular file")
	}
	if info.Size() > maximumRecordSize {
		return domain.CredentialMetadata{}, errors.New("credential metadata exceeds size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return domain.CredentialMetadata{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maximumRecordSize+1))
	var metadata domain.CredentialMetadata
	if err := decoder.Decode(&metadata); err != nil {
		return domain.CredentialMetadata{}, fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return domain.CredentialMetadata{}, fmt.Errorf("credential metadata has trailing data: %s", path)
	}
	if err := credential.ValidateMetadata(metadata); err != nil {
		return domain.CredentialMetadata{}, fmt.Errorf("invalid credential metadata %s: %w", path, err)
	}
	if filepath.Base(filepath.Dir(path)) != metadata.CredentialID ||
		strings.TrimSuffix(filepath.Base(path), ".json") != metadata.RevisionID {
		return domain.CredentialMetadata{}, fmt.Errorf("credential metadata path does not match identity: %s", path)
	}
	return metadata, nil
}

func (reader Reader) readPeerStatus(ctx context.Context) ([]domain.CredentialPeerStatus, error) {
	if !filepath.IsAbs(reader.Root) {
		return nil, errors.New("credential root must be absolute")
	}
	directory := filepath.Join(reader.Root, "peers")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []domain.CredentialPeerStatus{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]domain.CredentialPeerStatus, 0, len(entries))
	for _, entry := range entries {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		var peer domain.CredentialPeer
		if err := readProtectedJSON(path, &peer); err != nil {
			return nil, fmt.Errorf("read credential peer %s: %w", entry.Name(), err)
		}
		if peer.Name != strings.TrimSuffix(entry.Name(), ".json") {
			return nil, fmt.Errorf("invalid credential peer %s: path does not match peer name", entry.Name())
		}
		if err := credential.ValidatePeer(peer); err != nil {
			return nil, fmt.Errorf("invalid credential peer %s: %w", entry.Name(), err)
		}
		state := domain.CredentialSyncState{}
		statePath := filepath.Join(reader.Root, "state", peer.Name+".json")
		if err := readProtectedJSON(statePath, &state); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read credential sync state %s: %w", peer.Name, err)
		}
		if state.Peer != "" && state.Peer != peer.Name {
			return nil, fmt.Errorf("credential sync state %s belongs to another peer", peer.Name)
		}
		if err := credential.ValidateSyncState(state); err != nil {
			return nil, fmt.Errorf("invalid credential sync state %s: %w", peer.Name, err)
		}
		role := "passive"
		if peer.Transport == "local" || peer.Transport == "ssh" {
			role = "active"
		}
		result = append(result, domain.CredentialPeerStatus{
			Name: peer.Name, Role: role, ManualOnly: peer.ManualOnly, Trusted: peer.Trusted,
			LastAttempt: state.LastAttempt, LastSuccess: state.LastSuccess,
			ConsecutiveFailures: state.ConsecutiveFailures, NextRetry: state.NextRetry,
			Failed: state.Error != "",
		})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result, nil
}

func readProtectedJSON(path string, target any) error {
	_ = safefs.RepairPath(path, 0o077)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("record is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("record permissions are too broad")
	}
	if info.Size() > maximumRecordSize {
		return errors.New("record exceeds size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maximumRecordSize+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("record has trailing data")
	}
	return nil
}
