package testvmsruntime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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

// RecipeBundle produces the exact trusted archive installed for base builders.
func RecipeBundle(root, dispatcher string) ([]byte, []byte, error) {
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	manifest, err := recipeManifest(root, dispatcher, writer)
	if err != nil {
		return nil, nil, err
	}
	if err := writer.WriteHeader(&tar.Header{Name: "manifest.sha256", Mode: 0644, Size: int64(len(manifest)), Typeflag: tar.TypeReg}); err != nil {
		return nil, nil, err
	}
	if _, err := writer.Write(manifest); err != nil {
		return nil, nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, nil, err
	}
	if err := compressed.Close(); err != nil {
		return nil, nil, err
	}
	return archive.Bytes(), manifest, nil
}

const recipeEngine = "bin/yard-engine"

func recipeManifest(root, dispatcher string, archive *tar.Writer) ([]byte, error) {
	var manifest bytes.Buffer
	appendFile := func(name, path string, mode int64) error {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("recipe must be a regular file: %s", name)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		digest := sha256.New()
		var destination io.Writer = digest
		if archive != nil {
			if err := archive.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			destination = io.MultiWriter(archive, digest)
		}
		if _, err := io.Copy(destination, file); err != nil {
			return err
		}
		fmt.Fprintf(&manifest, "%x  %s\n", digest.Sum(nil), name)
		return nil
	}
	for _, name := range recipeFiles {
		if err := appendFile(name, filepath.Join(root, filepath.FromSlash(name)), 0644); err != nil {
			return nil, err
		}
	}
	if err := appendFile(recipeEngine, dispatcher, 0755); err != nil {
		return nil, err
	}
	return manifest.Bytes(), nil
}

func recipeBundleDigest(root, dispatcher string) (string, error) {
	manifest, err := recipeManifest(root, dispatcher, nil)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(manifest)
	return hex.EncodeToString(digest[:]), nil
}

func (backend *Backend) installRecipes(ctx context.Context) error {
	archive, manifest, err := RecipeBundle(backend.RepositoryRoot, backend.Dispatcher)
	if err != nil {
		return err
	}
	return backend.installRecipeBundle(ctx, archive, manifest)
}

func (backend *Backend) installRecipeBundle(ctx context.Context, archive, manifest []byte) error {
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
	_, _, err := backend.Runner.Run(ctx, "incus", []string{"exec", backend.Instance, "--project", backend.Project, "--", "sh", "-c", script, "install-recipes", generation}, nil, bytes.NewReader(archive))
	return err
}
