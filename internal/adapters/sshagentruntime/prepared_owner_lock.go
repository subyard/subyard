package sshagentruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

const preparedOwnerLockRecordLimit = 4096

// The closed record contains only the original grant's control identity. Byte
// columns preserve arbitrary Go pathname bytes and the existing expiry JSON;
// neither key material nor an observation recipe crosses this boundary.
type preparedOwnerLockRecord struct {
	Version                            int
	Directory, Identity, State, Expires []byte
}

func preparedOwnerLockDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func preparedOwnerLockState(state string) bool {
	return state == "locked" || state == "pending" || state == "granted"
}

// CaptureRecord retains the original prepared lock without inspecting the live
// worker. A nil preparation remains absent rather than becoming a locked grant.
func (p *PreparedLock) CaptureRecord() ([]byte, error) {
	if p == nil {
		return nil, nil
	}
	if p.directory == "" || !preparedOwnerLockDigest(p.identity) || !preparedOwnerLockState(p.state) {
		return nil, domain.ErrPlanStale
	}
	expires, err := json.Marshal(p.expires)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(preparedOwnerLockRecord{1, []byte(p.directory), []byte(p.identity), []byte(p.state), expires})
	if err != nil {
		return nil, err
	}
	if len(raw) > preparedOwnerLockRecordLimit {
		return nil, domain.ErrPlanStale
	}
	return raw, nil
}

// RestorePreparedLockRecord restores only original preparation fields. The
// existing LockPrepared boundary checks the current grant under its manager
// lock, including the permitted convergence to locked; restoration does not
// observe a worker or choose a replacement identity or expiry.
func (m Manager) RestorePreparedLockRecord(raw []byte, expectedBinding, expectedState string) (*PreparedLock, error) {
	if len(raw) == 0 {
		if expectedBinding != "" || expectedState != "" {
			return nil, domain.ErrPlanStale
		}
		return nil, nil
	}
	if len(raw) > preparedOwnerLockRecordLimit || !preparedOwnerLockDigest(expectedBinding) || !preparedOwnerLockState(expectedState) {
		return nil, domain.ErrPlanStale
	}
	var record preparedOwnerLockRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, domain.ErrPlanStale
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, domain.ErrPlanStale
	}
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(canonical, raw) || record.Version != 1 || len(record.Directory) == 0 || string(record.Directory) != m.Config.Directory || !preparedOwnerLockDigest(string(record.Identity)) || string(record.State) != expectedState {
		return nil, domain.ErrPlanStale
	}
	var expires time.Time
	if err := json.Unmarshal(record.Expires, &expires); err != nil {
		return nil, domain.ErrPlanStale
	}
	canonicalExpiry, err := json.Marshal(expires)
	if err != nil || !bytes.Equal(canonicalExpiry, record.Expires) {
		return nil, domain.ErrPlanStale
	}
	p := &PreparedLock{directory: string(record.Directory), identity: string(record.Identity), state: string(record.State), expires: expires}
	if p.Binding() != expectedBinding {
		return nil, domain.ErrPlanStale
	}
	return p, nil
}
