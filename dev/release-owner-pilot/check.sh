#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
export TMPDIR=/tmp
out="$PWD/.build/release-owner-pilot"
mkdir -p "$out"
mode="${1:---local}"
case "$mode" in --local|--preflight|--vm) ;; *) exit 2 ;; esac
if [[ -x .build/migrationhistory-pilot/toolchain/cargo/bin/cargo ]]; then
 export CARGO_HOME="$PWD/.build/migrationhistory-pilot/toolchain/cargo"
 export RUSTUP_HOME="$PWD/.build/migrationhistory-pilot/toolchain/rustup"
 export PATH="$CARGO_HOME/bin:$PATH"
fi
if [[ -x "$out/toolchain/cargo/bin/cargo" ]]; then
 export CARGO_HOME="$out/toolchain/cargo" RUSTUP_HOME="$out/toolchain/rustup"
 export PATH="$CARGO_HOME/bin:$PATH"
fi
if ! command -v cargo >/dev/null && [[ "$mode" == --vm ]]; then
 # Explicit disposable VM fixture setup only. No normal-host installer path.
 case "$(uname -m)" in x86_64) triple=x86_64-unknown-linux-gnu ;; aarch64) triple=aarch64-unknown-linux-gnu ;; *) exit 2 ;; esac
 tool="$out/toolchain"
 mkdir -p "$tool"
 chmod 0700 "$tool"
 curl --fail --location --retry 2 --connect-timeout 15 --max-time 180 "https://static.rust-lang.org/rustup/dist/$triple/rustup-init" -o "$tool/rustup-init"
 curl --fail --location --retry 2 --connect-timeout 15 --max-time 180 "https://static.rust-lang.org/rustup/dist/$triple/rustup-init.sha256" -o "$tool/rustup-init.sha256"
 (cd "$tool" && sha256sum --check rustup-init.sha256)
 chmod 0700 "$tool/rustup-init"
 export CARGO_HOME="$tool/cargo" RUSTUP_HOME="$tool/rustup"
 timeout 600 "$tool/rustup-init" -y --no-modify-path --profile minimal --default-toolchain stable
 export PATH="$CARGO_HOME/bin:$PATH"
fi
command -v cargo >/dev/null
command -v go >/dev/null
go version
rustc --version
if [[ "$mode" != --vm ]]; then
cargo fmt --manifest-path dev/release-owner-pilot/Cargo.toml -- --check
cargo test --locked --manifest-path dev/release-owner-pilot/Cargo.toml --target-dir "$out/target"
cargo clippy --locked --manifest-path dev/release-owner-pilot/Cargo.toml --target-dir "$out/target" --all-targets -- -D warnings
go test -race -count=1 ./internal/config ./internal/releasetransition ./dev/release-owner-pilot/native
fi
cargo build --locked --manifest-path dev/release-owner-pilot/Cargo.toml --target-dir "$out/target"
go build -o "$out/native" ./dev/release-owner-pilot/native
python3 dev/release-owner-pilot/acceptance.py --owner "$out/target/debug/release-owner-pilot" --native "$out/native" --preflight
if [[ "$mode" != --preflight ]]; then
 python3 dev/release-owner-pilot/acceptance.py --owner "$out/target/debug/release-owner-pilot" --native "$out/native"
fi
