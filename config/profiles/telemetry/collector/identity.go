package collector

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const identityKeyFile = ".identity-key"

type identityKey []byte

type identity func(string) string

func (key identityKey) digest(value string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, value)
	return mac.Sum(nil)
}

func (key identityKey) verifier() string {
	return hex.EncodeToString(key.digest("subyard.telemetry/identity-key/v1"))
}

func storeIdentityKey(db *sql.DB, dir string, version int) (identityKey, error) {
	if version == 0 {
		return loadIdentityKey(dir)
	}
	key, err := readIdentityKey(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("telemetry identity key is missing from an existing store")
	}
	if err != nil {
		return nil, err
	}
	var verifier string
	if err = db.QueryRow("SELECT value FROM metadata WHERE key='identity_key_verifier'").Scan(&verifier); err != nil {
		return nil, errors.New("telemetry identity key verifier is unavailable")
	}
	if !hmac.Equal([]byte(key.verifier()), []byte(verifier)) {
		return nil, errors.New("telemetry identity key does not match the store")
	}
	return key, nil
}

func (key identityKey) legacy(value string) string {
	if recordPatterns["legacy_opaque"].MatchString(value) {
		return value
	}
	return "id:" + hex.EncodeToString(key.digest(value))
}

func (key identityKey) id(value string) string {
	if !legacyID.MatchString(value) {
		return ""
	}
	// Pending records can use the prior producer's spelling.
	return "h1:" + hex.EncodeToString(key.digest(key.legacy(value)))
}

func (key identityKey) projectionID(parts ...string) string {
	return "association:" + hex.EncodeToString(key.digest(strings.Join(parts, "\x00")))
}

func (id identity) origin(origin, source string) string {
	if origin == "producer:"+source {
		return "producer:" + id(source)
	}
	return id(origin)
}

func keepID(value string) string { return value }

func readIdentityKey(dir string) (identityKey, error) {
	path := filepath.Join(dir, identityKeyFile)
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != 32 {
		return nil, errors.New("invalid telemetry identity key file")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 || int(st.Uid) != os.Geteuid() {
		return nil, errors.New("invalid telemetry identity key owner or links")
	}
	key := make(identityKey, 32)
	_, err = io.ReadFull(f, key)
	return key, err
}

func loadIdentityKey(dir string) (identityKey, error) {
	key, err := readIdentityKey(dir)
	if !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if err = unix.Flock(int(parent.Fd()), unix.LOCK_EX); err != nil {
		return nil, err
	}
	key, err = readIdentityKey(dir)
	if !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	key = make(identityKey, 32)
	if _, err = rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".key-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(key)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, identityKeyFile)); err != nil {
		return nil, err
	}
	return key, parent.Sync()
}
