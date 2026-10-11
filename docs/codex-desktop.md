# Export projects to Codex Desktop

Run Subyard and Codex Desktop natively on the same GNU/Linux desktop. Export
uses an already prepared SSH connection into the selected yard. Project files
and execution stay in that yard's L1, including projects with a profile target.
No project environment is started.

```sh
yard codex export --check
yard codex export
yard -Y <HostID>/<yard> codex open
```

`export` adds every registered project of exactly one selected yard to the
desktop declaration. `open` refreshes that inventory, exports it, then requests
import in the application. Choose a project through the application's normal
interface. A successful launch reports only an import request; check the
projects in the GUI to confirm the result.

Use the usual `-Y` selection for the current/default yard, a named local yard,
a registered remote alias, or a canonical `<HostID>/<yard>`. The default selects
one yard. Ambiguous bare yard names require a canonical selector. A canonical
selector fetches only the selected owner's fresh, trusted inventory; another
unavailable owner does not prevent that export.

`--check` shows the owner/yard, SSH alias, project count, target file and planned
changes without writes, the desktop publication lock or GUI launch. Subyard's
usual shared configuration read lock still protects coherent settings reads.
An unavailable owner,
missing SSH route, unknown host key, stopped yard or alias that reaches the
owner host instead of L1 fails before the declaration is written. Prepare
SSH/Tailscale and trust separately; export does not register, repair or start
anything.

The default declaration is `$CODEX_HOME/codex-app/config.json`, or
`~/.codex/codex-app/config.json` when `CODEX_HOME` is empty or unset. Select an
absolute path with `--config PATH` if the GUI uses another home. Changing
`CODEX_HOME` in a terminal does not change a running GUI's home.

The writer preserves other connections, timeout preferences and existing
nonempty labels. New labels include the project and canonical owner/yard.
Repeated export adds no duplicates; a semantic no-op leaves the file and backup
untouched. Before replacing an existing declaration, Subyard saves a protected
`<config-path>.subyard-backup`, then publishes the new JSON atomically. Unsafe
files, unsupported schemas, duplicate JSON members, normalized path collisions
and concurrent drift are refused. New files use mode `0600` and new directories
use `0700`.

Missing or moved projects remain in the declaration and GUI. Export does not
edit the application's database or session history. Restoring the backup
restores the input JSON; it does not undo projects already imported into the GUI.

`open` sends `codex://codex-app/apply-config` through `xdg-open`, including after
an unchanged export. If the desktop handler fails, the declaration remains
saved and the command fails. Open Codex manually or fix the handler and retry
`open`; that explicit retry requests import again.

This integration follows the v1 declaration used by the
[pinned Railway producer](https://github.com/railwayapp/cli/blob/e6570d7d2fbfc4d94f1100fb27871bbbd2168188/src/commands/cloud_agent/desktop/codex_config.rs).
It is a client compatibility boundary rather than a stable public OpenAI API.
Acceptance runs on disposable GNU/Linux VMs with real local and remote L1 SSH
routes and an emulated desktop URL handler. It covers declaration contents,
preservation, fresh inventory, repeated import requests and pre-write refusals.
Physical desktop hardware and a live VPN account are not acceptance prerequisites.
