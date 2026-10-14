# Optional background Git fetch

The `git-fetch` profile is off by default and installs a systemd service and timer
inside the yard. Enable it from the owner host for an existing running yard:

```sh
yard -Y demo profile enable git-fetch --yes
yard -Y demo profile status git-fetch
yard -Y demo profile disable git-fetch --yes
```

A cycle starts five minutes after boot, then five minutes after the previous cycle
finishes. It runs as the yard developer account and fetches configured remotes
strictly one at a time. Each remote has a one-minute timeout; failures do not stop
later remotes or repositories. There is no total cycle cutoff and no overlap, so
slow cycles extend the cadence. Git parallel jobs and submodule recursion are
disabled. Fetch updates remote tracking refs without changing checked-out files.

The worker walks managed `src` directories identified by the existing
`/srv/workspaces/<projectId>/.subyard-meta.json` contract. It includes main and
nested Git repositories, including ignored private overlays. It skips symlinks
and `.git`, `.build`, `.cache`, `node_modules`, `vendor`, `.venv`, `venv` and
`__pycache__` directories. Worktrees sharing a Git common directory are fetched
once per cycle. Discovery keeps no persistent state and is independent of Orca.

The existing `subyard-github run` wrapper supplies GitHub HTTPS authorization when
installed. SSH uses the developer's existing forwarded-agent socket and known
host trust. Authentication is noninteractive. Journal diagnostics contain counts
and generic failures, without remote URLs or Git output.

Repeated `yard init` checks and refreshes installed assets, including after a
release update or interrupted installation. Disable stops the timer and the
entire active fetch process group; installed files remain for re-enabling.
Lifecycle hooks validate owned files and loaded units before mutation. OS-wide
container service policy is supported; the worker enforces no new privileges
with `setpriv`. Conflicting unit-specific overrides are refused and left intact.

Run `config/profiles/git-fetch/tests/run.sh` for host-free checks. The disposable
component check is `config/profiles/git-fetch/tests/e2e/acceptance.sh --slot N`.
It installs the actual service directly in a leased systemd guest and tests local
remotes, nested discovery, serial fetching, recurrence and disable. It does not
bootstrap a full yard or verify external GitHub/SSH accounts.
