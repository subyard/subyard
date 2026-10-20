package testvms_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestBaseRecipeInstallerContract(t *testing.T) {
	archive, _, err := testvmsruntime.RecipeBundle("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, environment := range []string{"subyard-pair", "android-test"} {
		t.Run(environment, func(t *testing.T) {
			root := testkit.TempDir(t)
			if !strings.HasPrefix(root, "/tmp/") {
				t.Fatal("recipe fixtures require TMPDIR=/tmp")
			}
			compressed, err := gzip.NewReader(bytes.NewReader(archive))
			if err != nil {
				t.Fatal(err)
			}
			defer compressed.Close()
			reader := tar.NewReader(compressed)
			for {
				header, err := reader.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if header.Typeflag != tar.TypeReg || !filepath.IsLocal(header.Name) {
					t.Fatalf("unsafe production recipe entry: %+v", header)
				}
				path := filepath.Join(root, header.Name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, body, 0600)
			}
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			// Only read-only tools and narrowly bounded physical peers are reachable.
			// In particular, a pool/network or filesystem mutation cannot escape via PATH.
			for _, name := range []string{"bash", "dirname", "cut", "awk", "sed", "sort", "dpkg"} {
				path, err := exec.LookPath(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
					t.Fatal(err)
				}
			}
			stub := `#!/bin/bash
set -eu
name="${0##*/}"
printf '%s %s\n' "$name" "$*" >> "$HOME/effects"
case "$name:$*" in
  'id:-u') printf '0\n' ;;
  'id:-u dev') printf '1000\n' ;;
  'id:-gn dev') printf 'dev\n' ;;
  'getent:passwd dev') printf 'dev:x:1000:1000::%s:/bin/bash\n' "$HOME" ;;
  'incus:--version') printf '6.0.6\n' ;;
  'incus:network list --project default --format csv -c n,t') : ;;
  'systemctl:is-active NetworkManager') printf 'inactive\n' ;;
  *) printf 'unexpected physical operation: %s %s\n' "$name" "$*" >&2; exit 99 ;;
esac
`
			for _, name := range []string{"id", "getent", "incus", "systemctl"} {
				testkit.WriteFile(t, filepath.Join(bin, name), []byte(stub), 0700)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "/bin/bash", "-c", `
set -euo pipefail
. ./scripts/e2e-lab/base.sh
base_update_distro() { printf 'distro\n' >> "$HOME/effects"; }
base_finalize_image() { printf 'finalized\n' >> "$HOME/effects"; }
base_main
printf 'caller-complete\n'
`)
			command.Dir = root
			command.Env = []string{"HOME=" + root, "PATH=" + bin, "LC_ALL=C", "SUBYARD_BASE_ENVIRONMENT=" + environment}
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("real bundled recipe/installer failed: %v\n%s", err, output)
			}
			if !bytes.Contains(output, []byte("Incus is 6.0.6.")) || !bytes.Contains(output, []byte("caller-complete")) {
				t.Fatalf("child or caller did not complete: %s", output)
			}
			effects, err := os.ReadFile(filepath.Join(root, "effects"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasSuffix(effects, []byte("finalized\n")) || bytes.Contains(effects, []byte("incus storage")) || bytes.Contains(effects, []byte("incus admin")) || bytes.Contains(effects, []byte("incus network create")) {
				t.Fatalf("unexpected installation-owned effects: %s", effects)
			}
		})
	}
}
