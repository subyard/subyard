package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/config"
)

func (runtime Runtime) previewSourceHash() (string, error) {
	payload, err := (config.MaterializedAsset{
		Source: filepath.Join(runtime.RepositoryRoot, "config", "preview", "subyard-preview"),
	}).ReadSource()
	if err != nil {
		return "", fmt.Errorf("static preview helper source is unavailable: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(payload)), nil
}

func (runtime Runtime) previewConverged(ctx context.Context) (bool, error) {
	digest, err := runtime.previewSourceHash()
	if err != nil {
		return false, err
	}
	return runtime.guestCheck(ctx, []string{"sh", "-eu", "-c", `
helper=/usr/local/bin/subyard-preview
[ ! -L "$helper" ]
[ "$(stat -c '%F|%a|%u:%g' "$helper")" = 'regular file|755|0:0' ]
[ "$(sha256sum "$helper" | cut -d ' ' -f 1)" = "$1" ]
`, "subyard", digest})
}
