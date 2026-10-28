# Optional background Git fetch

The `git-fetch` profile is off by default and installs a systemd service and timer
inside the yard. Enable it from the owner host for an existing running yard:

```sh
yard -Y default profile enable git-fetch --yes
yard -Y default profile status git-fetch
yard -Y default profile disable git-fetch --yes
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

When the effective Git setting selects `GITHUB_GIT_MODE=broker-first` or `broker-last`, the worker uses ordinary
Git and the [GitHub router](github.md#ordinary-git-with-original-authentication-fallback), following
the selected authentication order. With routing off, the existing `subyard-github run` wrapper
supplies GitHub HTTPS authorization when installed. SSH uses the developer's existing forwarded-agent socket and known
host trust. Authentication is noninteractive. Journal diagnostics contain counts
and generic failures, without remote URLs or Git output.

Ordinary release installation, update, migrate and compatible rollback refresh
already installed worker and unit files through `git-fetch-runtime`. Native
ownership, exact public assets and loaded systemd policy are checked before and
after delivery. A stale assessment is refused before mutation; interrupted
delivery can be retried through the same ordinary operation. Refresh preserves a
disabled timer and never installs the profile into an absent or stopped yard.
Stopped yards converge on the next supported operation after they start.
Repeated `yard init` also checks and refreshes installed assets. Disable stops the timer and the
entire active fetch process group; installed files remain for re-enabling.
Lifecycle hooks validate owned files and loaded units before mutation. OS-wide
container service policy is supported; the worker enforces no new privileges
with `setpriv`. Conflicting unit-specific overrides are refused and left intact.

Run `config/profiles/git-fetch/tests/run.sh` for host-free checks. The disposable
component check is `config/profiles/git-fetch/tests/e2e/acceptance.sh --slot N`.
Supply the frozen candidate through `SUBYARD_E2E_CANDIDATE_BUNDLE` and
`SUBYARD_E2E_CANDIDATE_SHA256`. The check first installs the actual service directly
in the leased systemd guest and tests local remotes, nested discovery, serial
fetching, recurrence and disable. In the same lease, it bootstraps one isolated
native yard through the installed candidate, then verifies ordinary migrate
repairs worker drift, repeats without rewriting current files, and retains
disabled and deselected intent. Capacity and the cached image's configured
developer UID are checked before init. The payload has a 20-minute bound plus
two minutes to drain; it does not verify external GitHub/SSH accounts.
Run `config/profiles/git-fetch/tests/e2e/acceptance.sh --preflight` for fixture
syntax checks without acquiring a lease; native Incus and image prerequisites
are verified at the start of the installed-yard check.
