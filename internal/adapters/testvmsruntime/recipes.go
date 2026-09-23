package testvmsruntime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Only product-owned recipes cross this boundary. Never archive a checkout or
// discover files recursively: local settings and credentials are not recipes.
var recipeFiles = []string{
	"scripts/e2e-lab/base.sh",
	"scripts/01-install-incus.sh",
	"scripts/lib/runtime.sh",
	"scripts/lib/engine-context.sh",
	"scripts/lib/ui.sh",
	"scripts/lib/host.sh",
	"scripts/lib/download.sh",
	"scripts/lib-power.sh",
}

func recipeBundle(root string) ([]byte, []byte, error) {
	var archive, manifest bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	appendFile := func(name string, body []byte) error {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := writer.Write(body)
		return err
	}
	for _, name := range recipeFiles {
		path := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Lstat(path)
		if err != nil {
			return nil, nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("recipe must be a regular file: %s", name)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}
		digest := sha256.Sum256(body)
		fmt.Fprintf(&manifest, "%x  %s\n", digest, name)
		if err := appendFile(name, body); err != nil {
			return nil, nil, err
		}
	}
	if err := appendFile("manifest.sha256", manifest.Bytes()); err != nil {
		return nil, nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, nil, err
	}
	if err := compressed.Close(); err != nil {
		return nil, nil, err
	}
	return archive.Bytes(), manifest.Bytes(), nil
}

func recipeBundleDigest(root string) (string, error) {
	_, manifest, err := recipeBundle(root)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(manifest)
	return hex.EncodeToString(digest[:]), nil
}

func (backend *Backend) installRecipes(ctx context.Context) error {
	archive, manifest, err := recipeBundle(backend.RepositoryRoot)
	if err != nil {
		return err
	}
	// Immutable content-addressed directories keep running builders insulated
	// from a simultaneous broker upgrade. The symlink swap is atomic.
	digest := sha256.Sum256(manifest)
	generation := fmt.Sprintf("/usr/local/libexec/subyard/e2e-recipes-%x", digest)
	script := `set -eu
root="$1"
install -d -m 0755 /usr/local/libexec/subyard
if [ ! -d "$root" ]; then
  candidate=$(mktemp -d /usr/local/libexec/subyard/.e2e-recipes.XXXXXXXX)
  trap 'rm -rf -- "$candidate"' EXIT
  cat > "$candidate/recipe.tar.gz"
  tar -xOf "$candidate/recipe.tar.gz" manifest.sha256 > "$candidate/manifest.sha256"
  chmod 0644 "$candidate/recipe.tar.gz" "$candidate/manifest.sha256"
  chmod 0755 "$candidate"
  mv -T -- "$candidate" "$root"
else
  cat >/dev/null
fi
link=$(mktemp -u /usr/local/libexec/subyard/.e2e-recipes-link.XXXXXXXX)
ln -s -- "$root" "$link"
mv -Tf -- "$link" /usr/local/libexec/subyard/e2e-recipes
`
	_, _, err = backend.Runner.Run(ctx, "incus", []string{"exec", backend.Instance, "--project", backend.Project, "--", "sh", "-c", script, "install-recipes", generation}, nil, bytes.NewReader(archive))
	return err
}
