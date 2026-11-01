package releaseowner

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// This capability is minted only from the running Serve image. Its private
// implementation never accepts an image path, descriptor, or digest from IPC.
type nativeSelfExecutable struct {
	mu     sync.Mutex
	file   *os.File
	digest string
}

func retainSelfExecutable() (*nativeSelfExecutable, error) {
	file, err := os.Open("/proc/self/exe")
	if err != nil {
		return nil, err
	}
	hasher := sha256.New()
	size, err := io.Copy(hasher, io.LimitReader(file, (128<<20)+1))
	if err != nil || size > 128<<20 {
		file.Close()
		return nil, errors.New("native image bound")
	}
	return &nativeSelfExecutable{file: file, digest: fmt.Sprintf("%x", hasher.Sum(nil))}, nil
}
func (image *nativeSelfExecutable) Borrow() (*os.File, string, error) {
	image.mu.Lock()
	defer image.mu.Unlock()
	if image.file == nil {
		return nil, "", errors.New("native self image closed")
	}
	file, err := retain(image.file)
	return file, image.digest, err
}
func (image *nativeSelfExecutable) Close() error {
	image.mu.Lock()
	defer image.mu.Unlock()
	if image.file == nil {
		return nil
	}
	err := image.file.Close()
	image.file = nil
	return err
}
