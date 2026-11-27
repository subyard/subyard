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
	dispatcher := filepath.Join(testkit.TempDir(t), "yard-engine")
	build := exec.Command("bash", "../../../dev/build-engine.sh", "--output", dispatcher)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build recipe engine: %v\n%s", err, output)
	}
	archive, _, err := testvmsruntime.RecipeBundle("../../..", dispatcher)
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
				if header.Name == "scripts/lib-power.sh" {
					// Redirect the host-only paths while keeping the recipe and installer intact.
					source := strings.NewReplacer(
						"/usr/sbin/NetworkManager", filepath.Join(root, "fixture-bin/NetworkManager"),
						"/etc/NetworkManager/conf.d/", filepath.Join(root, "nm/conf.d")+"/",
						"/var/lib/subyard/nm-guard-state", filepath.Join(root, "nm/state"),
					).Replace(string(body))
					body = []byte(source)
				}
				testkit.WriteFile(t, path, body, os.FileMode(header.Mode))
			}
			bin := filepath.Join(root, "fixture-bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			// Only read-only tools and narrowly bounded physical peers are reachable.
			// In particular, a pool/network or filesystem mutation cannot escape via PATH.
			for _, name := range []string{"bash", "dirname", "cut", "awk", "sed", "sort", "dpkg", "mktemp", "chmod", "rm", "tr", "grep", "sha256sum", "stat", "cat", "install", "sync", "mv"} {
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
  'incus:--version') printf '6.0.7\n' ;;
  'incus:network list --project default --format csv -c n,t') : ;;
  'systemctl:is-active NetworkManager') printf 'inactive\n' ;;
  'NetworkManager:--config-dir '*" --print-config") exec cat "$2/zz-subyard-unmanaged.conf" ;;
  *) printf 'unexpected physical operation: %s %s\n' "$name" "$*" >&2; exit 99 ;;
esac
`
			for _, name := range []string{"id", "getent", "incus", "systemctl", "NetworkManager"} {
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
			if !bytes.Contains(output, []byte("Incus is 6.0.7.")) || !bytes.Contains(output, []byte("caller-complete")) {
				t.Fatalf("child or caller did not complete: %s", output)
			}
			effects, err := os.ReadFile(filepath.Join(root, "effects"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(effects, []byte("NetworkManager --config-dir ")) {
				t.Fatalf("recipe skipped NetworkManager preflight: %s", effects)
			}
			if !bytes.HasSuffix(effects, []byte("finalized\n")) || bytes.Contains(effects, []byte("incus storage")) || bytes.Contains(effects, []byte("incus admin")) || bytes.Contains(effects, []byte("incus network create")) {
				t.Fatalf("unexpected installation-owned effects: %s", effects)
			}
		})
	}
}

func TestBaseRecipeFinalizeDisablesGuestAutoUpgrade(t *testing.T) {
	source, err := os.ReadFile("../../../scripts/e2e-lab/base.sh")
	if err != nil {
		t.Fatal(err)
	}
	root := testkit.TempDir(t)
	if !strings.HasPrefix(root, "/tmp/") {
		t.Fatal("recipe fixtures require TMPDIR=/tmp")
	}
	// Run the production finalizer against fixture copies of its absolute guest paths.
	body := strings.NewReplacer(
		"/home/dev/", filepath.Join(root, "home/dev")+"/",
		"/var/lib/subyard", filepath.Join(root, "var/lib/subyard"),
	).Replace(string(source))
	for _, directory := range []string{"home/dev/.cache/subyard-e2e-platform", "var/lib/subyard", "bin", "recipe"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(root, "base.sh"), []byte(body), 0600)
	testkit.WriteFile(t, filepath.Join(root, "recipe/manifest.sha256"), []byte("recipe\n"), 0600)
	bin := filepath.Join(root, "bin")
	for _, name := range []string{"bash", "cut", "sort", "sha256sum"} {
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
  'getent:group kvm') printf 'kvm:x:993:\n' ;;
  'dpkg-query:'*) printf 'fixture=1\n' ;;
  'usermod:-aG kvm dev' | 'install:'* | 'chown:'* | 'apt-get:clean') : ;;
  'systemctl:disable --now apt-daily.timer apt-daily-upgrade.timer' | 'systemctl:mask apt-daily.timer apt-daily-upgrade.timer') : ;;
  *) printf 'unexpected physical operation: %s %s\n' "$name" "$*" >&2; exit 99 ;;
esac
`
	for _, name := range []string{"getent", "usermod", "install", "chown", "systemctl", "dpkg-query", "apt-get"} {
		testkit.WriteFile(t, filepath.Join(bin, name), []byte(stub), 0700)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/bash", "-c", `
set -euo pipefail
. ./base.sh
recipe_root="$PWD/recipe"
base_finalize_image
`)
	command.Dir = root
	command.Env = []string{"HOME=" + root, "PATH=" + bin, "LC_ALL=C", "SUBYARD_BASE_ENVIRONMENT=subyard-pair"}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("base finalizer failed: %v\n%s", err, output)
	}
	effects, err := os.ReadFile(filepath.Join(root, "effects"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(effects), "systemctl disable --now apt-daily.timer apt-daily-upgrade.timer\nsystemctl mask apt-daily.timer apt-daily-upgrade.timer\n") {
		t.Fatalf("finalizer must mask the guest auto-upgrade timers: %s", effects)
	}
	if _, err := os.Stat(filepath.Join(root, "var/lib/subyard/base-manifest.sha256")); err != nil {
		t.Fatalf("finalizer did not write the base manifest: %v", err)
	}
}
