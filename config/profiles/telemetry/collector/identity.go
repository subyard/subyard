package collector

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const identityKeyFile = ".identity-key"

type identityKey []byte

// Keep only the opaque formats produced by the profile, collector and native
// session exporters. Other identifiers are pseudonymized in the local store.
const uuidPattern = `[0-9a-f]{8}-[0-9a-f]{4}-[47][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`

var opaqueID = regexp.MustCompile(`^(?:` + uuidPattern + `(?::(?:scope|workload))?|[0-9a-f]{32}|[0-9a-f]{64}|(?:allocation|association|id):[0-9a-f]{64}|resource-[0-9a-f]{64}|[0-9a-f]{64}:` + uuidPattern + `:[0-9a-f]{12}:[0-9]{1,20}:[0-9]{1,20})$`)
var legacyID = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,512}$`)

func (key identityKey) id(value string) string {
	if opaqueID.MatchString(value) {
		return value
	}
	if !legacyID.MatchString(value) {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, value)
	return "id:" + hex.EncodeToString(mac.Sum(nil))
}

// Projection relationships are derived only from already opaque references.
func projectionID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "association:" + hex.EncodeToString(sum[:])
}

func (key identityKey) source(value string) string {
	switch value {
	case "native-otlp", "collector:otlp-correlation":
		return value
	default:
		return key.id(value)
	}
}

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

func (key identityKey) origin(origin, source string) string {
	if strings.HasPrefix(origin, "producer:") && strings.TrimPrefix(origin, "producer:") == source {
		if source = key.source(source); source != "" {
			return "producer:" + source
		}
	}
	return key.id(origin)
}
