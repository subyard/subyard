# Subyard Veranda

Veranda is Subyard's graphical client. The Go engine owns domain rules and operations;
the [native Rust client](client/) handles RPC, local/SSH connections and pinned host keys.

The desktop shell uses GTK4 through gtk4-rs, with the existing Rust client in the GUI process.
Framework research and comparative measurements are complete; the
[native framework comparison](../docs/veranda/native-framework-comparison.md) preserves the alternatives.
Native UI implementation and desktop packages are in progress. The historical GTK measurement
used a C frontend with a Rust bridge; it does not establish the final gtk4-rs footprint.
The [GTK desktop](desktop/README.md) implements supported Host/Yard pages,
Connect/trust, typed sessions, owner exact-plan review and nonsecret persistent drafts.
Its implementation notes describe the precise scope and remaining checks; native
UI delivery and acceptance remain incomplete.

## Native client checks

Host-free checks require Python 3 and stable Rust with Clippy/rustfmt (selected by
[`rust-toolchain.toml`](../rust-toolchain.toml)) for formatting, Clippy and tests:

```sh
make verify-veranda
```

This is the shared local and CI entrypoint. It runs the Python runner contract and
Linux resource probe first, stopping on a failed prerequisite, then formatting,
Clippy, and native tests in order while reporting all independent Rust failures.
Use `python3 dev/check-veranda.py --rust-only` to skip the Python prerequisites and
run formatting, Clippy, and native tests. See [testing](../docs/testing.md#run-veranda-host-free-checks)
for setup and coverage.

## Delivery gates

Linux supports local and remote Linux owners. Windows and macOS are remote clients.
Remote connections use the system SSH client and an existing SSH agent. Yard and Veranda
share a product version; incompatible releases require matching installations.

Before shipping the native UI, complete UI/accessibility, resource, packaging and platform
acceptance. The [UX contract](../docs/veranda/README.md) and
[resource budgets](../docs/reference/control-plane-architecture.md#veranda-resource-budgets)
define those requirements. Native client checks establish backend behavior; desktop delivery
needs its own acceptance.
