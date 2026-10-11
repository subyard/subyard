package profile

import (
	"errors"
	"os"
	"path/filepath"
)

type shippedDeclaration struct {
	path string
	data []byte
}

// Both package kinds share one immutable source authority and path boundary.
func loadDeclarations(root, kind, filename string) ([]shippedDeclaration, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if !pinnedRepositoryRoot.MatchString(root) {
		resolved, err := filepath.EvalSymlinks(root)
		if err == nil {
			root = resolved
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	paths, err := filepath.Glob(filepath.Join(root, "config", kind, "*", filename))
	if err != nil {
		return nil, err
	}
	result := make([]shippedDeclaration, 0, len(paths))
	for _, path := range paths {
		definition := Definition{Root: filepath.Dir(path)}
		// Reuse full ancestor/descriptor validation; the manifest need not execute.
		validated, err := definition.sourcePath(filename, false)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(validated)
		if err != nil || info.Size() > 64<<10 {
			return nil, errors.New("shipped declaration must be bounded")
		}
		data, err := os.ReadFile(validated)
		if err != nil {
			return nil, err
		}
		result = append(result, shippedDeclaration{validated, data})
	}
	return result, nil
}
