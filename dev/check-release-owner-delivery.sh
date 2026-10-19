#!/usr/bin/env bash
# Opt-in package/sealing delivery gate. Cross-building ARM is not ARM execution.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
out="${1:-$ROOT/.build/release-owner-delivery/check}"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
export TMPDIR=/tmp
export CARGO_TARGET_DIR="${CARGO_TARGET_DIR:-$ROOT/.build/release-owner-delivery/target}"
version=1.0.0-release-owner-delivery
for arch in amd64 arm64; do
  ./dev/package-engine.sh --experimental-release-owner --version "$version" --arch "$arch" --output-dir "$out/$arch"
  bundle="$out/$arch/subyard-$version-linux-$arch.tar.gz"
  python3 - "$bundle" "$arch" "$ROOT" <<'PY'
import hashlib, json, pathlib, sys, tarfile
bundle, arch, root = sys.argv[1:]
with tarfile.open(bundle, "r:gz") as archive:
    files = {m.name.removeprefix("./"): m for m in archive.getmembers() if m.isfile()}
    def read(path):
        return archive.extractfile(files[path]).read()
    inventory = {}
    for line in read("runtime-files.sha256").decode().splitlines():
        sha, path = line.split("  ./", 1)
        assert path not in inventory, path
        inventory[path] = sha
    assert set(inventory) == set(files) - {"runtime-files.sha256"}
    for path, sha in inventory.items():
        assert hashlib.sha256(read(path)).hexdigest() == sha, path
        assert files[path].mode == (0o755 if files[path].mode & 0o111 else 0o644), path
    assert not any(path.endswith((".rs", "/Cargo.toml", "/Cargo.lock")) for path in files)
    declaration = json.loads(read("config/release-owner.json"))
    assert declaration == dict(schemaVersion=1, kind="rust-release-owner", protocolVersion=1,
        owner="bin/subyard-release-owner", helper="bin/yard-engine",
        schema="config/release-owner.schema.json", buildInputs="config/release-owner-build.json")
    for key in ("owner", "helper", "schema", "buildInputs"):
        assert declaration[key] in inventory
    metadata = json.loads(read(declaration["buildInputs"]))
    assert metadata["schemaVersion"] == 1 and metadata["kind"] == "rust-release-owner-build"
    assert metadata["imageSHA256"] == inventory[declaration["owner"]]
    assert metadata["target"] == {"amd64":"x86_64", "arm64":"aarch64"}[arch] + "-unknown-linux-musl"
    assert metadata["features"] == [] and metadata["locked"] is True and metadata["staticELF"] is True
    assert metadata["compiler"].startswith("rustc ") and "\nhost: " in metadata["compiler"]
    inputs = metadata["inputs"]
    assert [v["path"] for v in inputs] == sorted({v["path"] for v in inputs})
    for item in inputs:
        source = pathlib.Path(root) / item["path"]
        assert not source.is_symlink() and source.is_file(), item["path"]
        assert hashlib.sha256(source.read_bytes()).hexdigest() == item["sha256"], item["path"]
    assert next(v["sha256"] for v in inputs if v["path"] == "release-owner/Cargo.lock") == metadata["cargoLockSHA256"]
    for path in ("bin/subyard-release-owner", "bin/yard-engine"):
        image = read(path)
        assert image[:4] == b"\x7fELF" and image[4:6] == b"\x02\x01", path
        assert int.from_bytes(image[18:20], "little") == {"amd64":62, "arm64":183}[arch], path
        output = pathlib.Path(bundle).parent / pathlib.Path(path).name
        output.write_bytes(image)
        output.chmod(0o755)
    print(f"{arch}: exact inventory, schema/metadata inputs, no Rust source, ELF architecture verified")
PY
  for image in subyard-release-owner yard-engine; do
    readelf -h "$out/$arch/$image" >/dev/null
    program_headers="$(readelf -l "$out/$arch/$image")"
    dynamic_entries="$(readelf -d "$out/$arch/$image")"
    if grep -q INTERP <<< "$program_headers" || grep -q '(NEEDED)' <<< "$dynamic_entries"; then
      printf 'delivery: dynamic dependency in %s/%s\n' "$arch" "$image" >&2
      exit 1
    fi
  done
done
case "$(uname -m)" in x86_64) host_arch=amd64 ;; aarch64|arm64) host_arch=arm64 ;; *) exit 2 ;; esac
fixture="$(mktemp -d /tmp/subyard-owner-delivery.XXXXXX)"
trap 'rm -rf -- "$fixture"' EXIT
mkdir -m 0700 "$fixture/home" "$fixture/no-toolchain"
for tool in bash dirname readlink; do ln -s "$(command -v "$tool")" "$fixture/no-toolchain/$tool"; done
bundle="$out/$host_arch/subyard-$version-linux-$host_arch.tar.gz"
env HOME="$fixture/home" SUBYARD_HOME="$fixture/home/data" SUBYARD_CONFIG_HOME="$fixture/home/config" \
  bash "$out/$host_arch/subyard-install-runtime-release.sh" --runtime-root "$fixture/runtime" \
  --bundle "$bundle" --checksum "$bundle.sha256" --manifest "$bundle.manifest.json" --provenance "$bundle.provenance.json"
SUBYARD_RUST_DELIVERY_RUNTIME_ROOT="$fixture/runtime" \
  go test -count=1 ./internal/adapters/releaseruntime -run '^(TestReleaseOwnerRealPublishedCandidate|TestRustOwnerRealPackagedRetainedSession|TestRustOwnerRealPackagedArtifactTamper)$'
SUBYARD_RUST_DELIVERY_RUNTIME_ROOT="$fixture/runtime" \
  go test -count=1 ./internal/cli -run '^TestRustOwnerFrozenFacadeProcess$'
SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT="$fixture/runtime/current" \
  go test -count=1 ./internal/adapters/releaseruntime \
    -run '^TestRustSourceQuery(VerifiedTooling|ToolingIdentityGuards)$'
SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT="$fixture/runtime/current" \
  go test -race -count=1 ./internal/cli \
    -run '^(TestConfigAliasRepair|TestQueuedSourceRepair|TestQueuedHostRename|TestPendingSourceRenameRecovery|TestCompletedSourceAuthoringFixture|TestRustCompletedRollbackAdmitsProfileInitThroughCLI)'
env -i PATH="$fixture/no-toolchain" HOME="$fixture/home" bash -c '
  for tool in go cargo rustc; do ! command -v "$tool" >/dev/null || exit 91; done
  "$1" --version
' delivery "$fixture/runtime/current/bin/yard" | tee "$out/installed-no-toolchain-version.txt"
grep -Fx "yard $version" "$out/installed-no-toolchain-version.txt" >/dev/null
printf 'delivery: installed launcher and typed retained Rust session passed without Go/Rust\n'
sha256sum "$out"/*/subyard-*.tar.gz
