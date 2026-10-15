package githubbroker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sync"
)

const runtimeProtocol = 1

var loadedRuntime = sync.OnceValues(func() (string, error) {
	// The mapped executable survives atomic replacement of its installed path.
	file, err := os.Open("/proc/self/exe")
	if err != nil {
		return "", errors.New("cannot inspect loaded GitHub runtime")
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", errors.New("cannot inspect loaded GitHub runtime")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
})

func runtimeStatus(configured bool) (map[string]any, error) {
	digest, err := loadedRuntime()
	return map[string]any{"configured": configured, "protocol": runtimeProtocol, "executable_sha256": digest}, err
}

func validRuntimeDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}
