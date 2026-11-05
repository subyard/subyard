package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"

	"github.com/Subyard/Subyard/internal/domain"
)

// AppliedIdentityHint is an offline observation, never mutation authority. Its
// namespace is deliberately separate from executing developer identity markers.
type AppliedIdentityHint struct {
	Schema  int    `json:"schema"`
	Yard    string `json:"yard"`
	UUID    string `json:"uuid"`
	Source  string `json:"source"`
	Release string `json:"release"`
	User    string `json:"user"`
	UID     int    `json:"uid"`
}

func AppliedIdentityHintPath(root, yard string) string {
	return filepath.Join(root, "reconcile", "applied-identities", yard+".json")
}
func ReadAppliedIdentityHint(root, yard string) (*AppliedIdentityHint, PersistentFileSnapshot, error) {
	snapshot, err := ReadPersistentFileSnapshot(root, AppliedIdentityHintPath(root, yard))
	if err != nil || !snapshot.Exists {
		return nil, snapshot, err
	}
	var hint AppliedIdentityHint
	decoder := json.NewDecoder(bytes.NewReader(snapshot.Content))
	decoder.DisallowUnknownFields()
	if snapshot.Identity.Mode&0777 != 0600 || len(snapshot.Content) > 4096 || decoder.Decode(&hint) != nil || decoder.Decode(new(any)) != io.EOF || hint.Schema != 1 || hint.Yard != yard || !domain.SafeName(hint.Yard) || !domain.SafeName(hint.User) || hint.UID <= 0 || !regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`).MatchString(hint.UUID) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hint.Source) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hint.Release) {
		return nil, snapshot, errors.New("unsafe applied developer identity hint")
	}
	return &hint, snapshot, nil
}
func PublishAppliedIdentityHint(root string, hint AppliedIdentityHint, before PersistentFileSnapshot) error {
	payload, err := json.Marshal(hint)
	if err != nil {
		return err
	}
	return CompareAndSwapLocalSettingsFile(root, AppliedIdentityHintPath(root, hint.Yard), before, payload)
}
