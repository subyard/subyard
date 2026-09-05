# AI Observer

AI Observer is included in the default `CODING_TOOL_INTEGRATIONS` as `aiobserver`.
`yard init` installs and starts its dashboard inside the yard. The integration pins
the upstream `0.5.0` multi-architecture Docker image by SHA-256 digest.

The observer reads Claude Code and Codex session files through read-only mounts.
It imports existing sessions on the first start and watches for new records. Agent
credentials are not mounted. OpenCode and pi sessions are not collected by this
file-based integration. The database lives in `/srv/agents/ai-observer` on the yard's
persistent `/srv` volume.

Inspect one yard and open the dashboard URL printed in its detailed status:

```sh
yard -Y default status
yard -Y default config show CODING_TOOL_INTEGRATIONS
yard -Y default config show AI_OBSERVER_HOST_PORT
```

Bare `yard status` shows the summary of all known yards. Detailed status lists selected
profiles, enabled agents and shared resources. A selected profile can have stopped
resources; use `yard -Y default emu status` for Android emulator details.

For container yards, the dashboard is published only on the owner's `127.0.0.1`.
Its default port is the SSH port plus 20000, wrapped into the range 1024–65535
(SSH port 2222 gives dashboard port 22222). Choose an explicit unused port if another
service uses it:

```sh
yard -Y default config set AI_OBSERVER_HOST_PORT 18080 --scope yard
yard -Y default init
```

For a remote owner or a VM yard, detailed status prints the SSH tunnel command and
the browser URL to use after starting that tunnel. Status itself does not start a
tunnel or open the browser.

Existing configurations with an explicit agent list keep that selection. Include
`aiobserver` in the complete list and rerun `yard init` to enable it. For example:

```sh
yard -Y default config set CODING_TOOL_INTEGRATIONS "claude codex opencode pi aiobserver" --scope yard
yard -Y default init
```

Removing `aiobserver` from that list and rerunning `yard init` stops the managed
service and removes its dashboard proxy. The database is retained. Re-enabling the
integration resumes collection from the saved positions.

Inside the yard, `ai-observer status` and `ai-observer logs` inspect the service.
`ai-observer-check` is the bounded readiness check used during reconciliation.

Upstream: [AI Observer](https://github.com/tobilg/ai-observer),
[watch mode](https://github.com/tobilg/ai-observer/blob/v0.5.0/README.md#watch-command).
