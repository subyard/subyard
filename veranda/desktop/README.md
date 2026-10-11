# Veranda GTK desktop

This initial Linux shell links gtk4-rs and the existing native Rust client in one
GUI process. It displays the local owner and saved remote connections, host/yard
navigation, Overview and a names-only project list. Selection preferences use the
client's private app-local store. Missing saved selections fall back to the local
current yard. Saved remotes load on selection; this slice does not add connections
or change host trust.

Refresh and native event hints trigger authoritative inventory reads. Event
payloads never replace inventory: a read invalidated while in flight is discarded
and refreshed, and returning to an owner refreshes its cached view. A failed read
remains explicit until Refresh or a native recovery hint. Navigation stays disabled
until initial selection restoration finishes. Reads and
preference writes run on a bounded worker outside the GTK thread. Closing the
window calls native shutdown and waits off-thread for the worker and owned
engine/SSH cleanup before allowing GTK to close. No timer polls the owner while idle.

## Build and focused checks

Use Rust 1.88 or newer, `pkg-config`, and system GTK4 development files
(`libgtk-4-dev` on Debian/Ubuntu). The Rust bindings use GTK's baseline APIs,
including Ubuntu 24.04's GTK4.14. Build output stays outside the source directory:

```sh
CARGO_TARGET_DIR=.build/veranda-desktop cargo test --manifest-path veranda/desktop/Cargo.toml --locked
CARGO_TARGET_DIR=.build/veranda-desktop cargo build --manifest-path veranda/desktop/Cargo.toml --locked
```

The selection/error model and worker can also compile without GTK development files:

```sh
CARGO_TARGET_DIR=.build/veranda-desktop cargo test --manifest-path veranda/desktop/Cargo.toml --locked --no-default-features
```

For a source-development launch, build the matching engine, then put the source
launcher on PATH. Both debug defaults identify the same development version:

```sh
dev/build-engine.sh
PATH="$PWD/bin:$PATH" .build/veranda-desktop/debug/veranda
```

An explicit stable `VERANDA_PRODUCT_VERSION` at GUI build time must match the
engine's `YARD_BUILD_VERSION`. Incompatible versions/capabilities remain errors;
the shell does not parse CLI output or provide a compatibility bypass. App data
uses `$XDG_DATA_HOME/subyard/veranda`, falling back to
`$HOME/.local/share/subyard/veranda`.

## Disposable Wayland SHORT

`wayland-short.py GUI_ELF ENGINE_ELF` runs only as the unprivileged user inside an
allocated disposable VM. It reuses public lease-fixture, typed owner-inventory,
process-identity and cleanup helpers. It installs VM-only dependencies, prepares
20 configured **NOT_CREATED** yards with 200 actual project records, then runs one
same-build GUI lifetime on labwc headless/Pixman. AT-SPI only reads accessibility;
`wtype` delivers physical Refresh, Projects selection and close. A new inventory
observation and ten visible project names are checked before natural GUI close,
observed child death and fixture cleanup. Configured records do not establish
container start/provision lifecycle. A captured screenshot is separate from any
latency or resource measurement.

Use the existing [candidate-bundle transport](../../docs/test-vms.md) to include
prebuilt ELFs with the current public source. For example, after the builds above:

```sh
python3 - <<'PY'
import subprocess, tarfile
paths = subprocess.check_output(['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard']).decode().split('\0')
with tarfile.open('.build/veranda-gtk-short.tar.gz', 'w:gz') as archive:
    for path in sorted(set(paths) - {''}):
        archive.add(path, arcname=path, recursive=False)
    archive.add('.build/veranda-desktop/debug/veranda', arcname='.subyard-acceptance/veranda', recursive=False)
    archive.add('.build/yard', arcname='.subyard-acceptance/yard', recursive=False)
PY
SUBYARD_E2E_CANDIDATE_BUNDLE="$PWD/.build/veranda-gtk-short.tar.gz" \
SUBYARD_E2E_CANDIDATE_SHA256="$(sha256sum .build/veranda-gtk-short.tar.gz | cut -d' ' -f1)" \
dev/agent-e2e.sh --slot 1 --vm-count 1 --purpose veranda-gtk-short -- \
  python3 veranda/desktop/wayland-short.py .subyard-acceptance/veranda .subyard-acceptance/yard
```

Choose an available owned slot using the VM guide; the example does not authorize
another holder's slot. Keep the original exit, full logs, cleanup and exact release
outcomes. The helper's evidence covers a local read-only GTK SHORT. Remote SSH UI,
full UX/IME/platform/package behavior and resource budgets remain unverified.
The client does not retain each engine/SSH child's original wait status; observed
identity death after app-directed shutdown must not be reported as a child exit0.
