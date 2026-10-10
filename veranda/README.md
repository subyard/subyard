# Subyard Veranda

Veranda is Subyard's graphical client. The Go engine owns domain rules and operations;
the [native Rust client](client/) handles RPC, local/SSH connections and pinned host keys.

The desktop shell is being replaced. Slint is the Linux finalist; see the
[native framework comparison](../docs/veranda/native-framework-comparison.md).
The native UI and desktop packages are still pending.

## Native client checks

Host-free checks require Python 3 and Rust 1.88 or newer:

```sh
make verify-veranda
```

This is the shared local and CI entrypoint. Use `python3 dev/check-veranda.py --rust-only`
for just the Rust suite. See [testing](../docs/testing.md#run-veranda-host-free-checks)
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
