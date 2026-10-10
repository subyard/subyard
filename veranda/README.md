# Subyard Veranda

Veranda is Subyard's graphical client. Its object model is owner host → yard → project.
Go owns the domain rules, plans and operations; the native Rust client owns versioned
RPC, local/SSH transport, app-local connections, pinned host keys and fixed session launches.
The UI presents validated facts and exact owner plans. It receives no arbitrary shell/file
capability, private keys, passphrases or tokens.

Tauri/WebKitGTK has been rejected because its idle memory footprint is unacceptable.
Its shell, frontend, dependencies and desktop packaging have been removed. Historical
[memory measurements](../docs/veranda/webview-memory-reference.md) remain for reference.
Slint is the Linux migration finalist after a [matched native comparison](../docs/veranda/native-framework-comparison.md)
with the actual Rust/Go backend: 62.48 MiB summed desktop RSS, versus GTK4's 76.72 MiB and
Qt Widgets' 86.93 MiB. The result applies to a pinned prototype with local correctness patches.
The public native shell, full product qualification and cross-platform delivery remain pending.

## Native client checks

The UI-independent Rust crate lives in [`client/`](client/). Shared transport, trust,
session and permission tests remain in the crate. Generic icons live in [`assets/`](assets/).
Host-free checks need Python 3 and Rust 1.88 or newer, without Node.js, npm or WebKit:

```sh
make verify-veranda
```

The [shared entrypoint](../dev/check-veranda.py) runs its temporary-path contract, Linux
resource probe isolation and native Rust tests with the committed lockfile.
`python3 dev/check-veranda.py --rust-only` selects the Rust suite. Linux uses a child-only
`TMPDIR` alias under a private `/tmp` fixture, without changing the parent environment.
Set `PYTHON` for another interpreter, such as `make verify-veranda PYTHON=python`.

The [CI workflow](../.github/workflows/veranda.yml) runs these native client contracts
on Linux amd64/arm64, Windows and macOS. It does not build or upload desktop candidates.
Core, shipped-profile and Go RPC acceptance remain independent and required.

## Delivery gates

Linux supports local and remote Linux owner hosts. Windows and macOS are intended as
remote clients; neither needs the Go engine or Incus locally. Remote access uses the system
SSH client and an existing SSH agent. Yard and Veranda share a product version and tag,
with separate installable artifacts. An incompatible version, schema or capability produces
an actionable message recommending installation from the same release.

Complete native UI, keyboard/accessibility/IME, useful-screen startup, loaded-fleet and
resource checks before shipping the native UI. Retain local/SSH trust, cancellation,
reconnect and natural child-process cleanup coverage. Package installation, upgrades,
rollback, signing and platform acceptance are separate delivery work; client tests alone
do not establish readiness. The [UX contract](../docs/veranda/README.md) and
[resource budgets](../docs/reference/control-plane-architecture.md#veranda-resource-budgets)
remain applicable to the replacement.

Before a long VM scenario, check the same build in its target environment with preparation,
launch, one real product action, normal closure and cleanup. A failure ends that short attempt;
classify the fixture/product cause, repair it and repeat the short check before a long run.
Verify actual API responses, UI states, nonzero test counts and the offered/dispatched/completed
load. Preserve the original acceptance criteria and reuse unaffected passing results.

## Historical diagnostic helpers

The shared process/cgroup/resource helpers in [`dev/measure-veranda.py`](../dev/measure-veranda.py)
and the owner/SSH fixtures remain available to native qualification. Legacy WebView diagnostic
entrypoints are historical tools, not current candidates or required Tauri gates. Their results
cannot be transferred to the native UI. The historical CLI probe requires an explicit `--binary`;
it does not select a retired desktop build by default. Physical checks run only through the
[leased disposable VM workflow](../docs/test-vms.md#agent-workflow).
