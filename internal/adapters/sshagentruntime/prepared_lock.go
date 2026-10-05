package sshagentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

// PreparedLock captures only the grant's control identity and deadline. No key
// material is opened, and expiry may converge to locked before execution.
type PreparedLock struct {
	directory string
	identity  string
	state     string
	expires   time.Time
}

func grantState(status Status) string {
	if status.State == "unlocked" || status.State == "reconnecting" {
		return "granted"
	}
	return status.State
}

func (m Manager) PrepareLock(ctx context.Context) (*PreparedLock, error) {
	status, err := m.Status(ctx)
	if err != nil {
		return nil, err
	}
	prepared := &PreparedLock{directory: m.Config.Directory, state: grantState(status), expires: status.ExpiresAt}
	if prepared.state != "locked" && prepared.state != "pending" && prepared.state != "granted" {
		return nil, errors.New("SSH agent grant state is invalid")
	}
	var identities []struct {
		Device uint64
		Inode  uint64
		Mode   uint32
		UID    uint32
		GID    uint32
	}
	for _, path := range []string{m.Config.Directory, filepath.Join(m.Config.Directory, "control.sock")} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, errors.New("SSH agent control identity unavailable")
		}
		identities = append(identities, struct {
			Device uint64
			Inode  uint64
			Mode   uint32
			UID    uint32
			GID    uint32
		}{uint64(stat.Dev), stat.Ino, stat.Mode, stat.Uid, stat.Gid})
	}
	payload, _ := json.Marshal(identities)
	digest := sha256.Sum256(payload)
	prepared.identity = hex.EncodeToString(digest[:])
	return prepared, nil
}

func (p *PreparedLock) State() string { return p.state }
func (p *PreparedLock) Binding() string {
	payload, _ := json.Marshal([]any{p.directory, p.identity, p.state, p.expires})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func (p *PreparedLock) Check(fresh *PreparedLock) error {
	if fresh == nil || p.directory != fresh.directory {
		return domain.ErrPlanStale
	}
	if fresh.state == "locked" {
		return nil
	}
	if p.Binding() != fresh.Binding() {
		return domain.ErrPlanStale
	}
	return nil
}

// LockPrepared checks under the existing manager lock, which also serializes
// grants. A plan to revoke one grant cannot revoke a replacement grant.
func (m Manager) LockPrepared(ctx context.Context, p *PreparedLock) error {
	if p == nil || p.directory != m.Config.Directory {
		return domain.ErrPlanStale
	}
	fresh, err := m.PrepareLock(ctx)
	if err != nil {
		return err
	}
	if err = p.Check(fresh); err != nil {
		return err
	}
	if fresh.state == "locked" {
		return nil
	}
	lock, err := acquireLock(ctx, filepath.Join(m.Config.Directory, "manager.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	fresh, err = m.PrepareLock(ctx)
	if err != nil {
		return err
	}
	if err = p.Check(fresh); err != nil {
		return err
	}
	if fresh.state == "locked" {
		return nil
	}
	if err = m.stop(ctx); err != nil {
		return err
	}
	status, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if status.State != "locked" {
		return errors.New("SSH signing grant remains after revoke")
	}
	return nil
}
