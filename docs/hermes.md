# Hermes Agent profile

The `hermes` profile installs a pinned, headless Hermes Agent in a dedicated
named yard. Hermes listens only on the yard's loopback interface. Remote access
uses the existing Subyard SSH path and a localhost-only forward; the profile
does not install Hermes Desktop, expose port 9119 on the owner host, or run the
Hermes gateway.

## Create and provision the yard

Bootstrap the named yard from the shipped, non-secret Hermes preset, then run
profile provisioning as its own confirmed mutation:

```sh
yard -Y hermes init --profile hermes
yard -Y hermes provision

yard -Y hermes config show YARD_PROFILES
yard -Y hermes config show AGENTS
yard -Y hermes config show HOST_MOUNTS
yard -Y hermes config show FORWARD_SSH_AGENT
yard -Y hermes config show SSH_PORT
ss -H -ltn 'sport = :2224'
```

`init` and `provision` are separate mutations and each displays its own plan and
confirmation. Bare `provision` is deliberate: `YARD_PROFILES=hermes` limits the
selection to this profile. The bootstrap validates the complete preset before
creating its mode-0600 named-yard definition. To customize a setting afterwards,
use the supported authoring command, for example
`yard -Y hermes config set SSH_PORT 2225 --scope yard`, then run plain
`yard -Y hermes init`.

The profile installs Hermes Agent 0.19.0 from an exact source commit, uses a
pinned `uv` and Python, and resolves only the committed lock file. The
root-owned runtime under `/opt/hermes-agent` also includes Node.js 22.20.0 with
its bundled npm 10.9.3, locked `agent-browser` 0.26.0, and Chromium installed by
Playwright 1.62.1. No manual Node or browser setup is required. Persistent Hermes
state remains under `/srv/hermes`, so the reproducible Node/browser runtime does
not inflate its backups. Re-provisioning the same pins preserves state and the
dashboard session token. `AGENTS=codex` also installs the pinned Codex CLI and
its config, but not Claude, OpenCode, pi, or Paseo. `HOST_LINKS=` keeps Codex
authorization and sessions inside this yard rather than mounting owner-host
state.

Verify the profile-owned browser runtime from an ordinary yard shell:

```sh
node --version
npm --version
agent-browser --version
agent-browser doctor --offline --quick
agent-browser --session hermes-smoke open about:blank
agent-browser --session hermes-smoke close
```

Provisioning leaves `hermes-serve.service` disabled until a provider has been
configured and tested. Enter the yard as `dev`, authorize the yard-local Codex
CLI, and let Hermes import that authorization for its native `openai-codex`
provider:

```sh
yard -Y hermes shell
cd /srv/hermes/workspace
codex login --device-auth
codex login status
hermes setup model
hermes doctor
hermes chat
```

Choose `openai-codex` during model setup and import the current Codex CLI
authorization. `hermes chat` must complete one real inference. Then leave the
interactive shell and approve the service through Subyard's explicit root
surface (the yard has `DEV_SUDO=0`):

```sh
yard -Y hermes shell --root -- hermes-provider-ready --inference-ok
yard -Y hermes shell -- systemctl is-enabled hermes-serve.service
yard -Y hermes shell -- systemctl is-active hermes-serve.service
yard -Y hermes shell -- curl -fsS http://127.0.0.1:9119/api/status
```

Codex authorization remains in `/home/dev/.codex`; never copy or mount the
owner host's Codex home into this yard. The Hermes backup contract does not
include that CLI authorization, so a restored yard must run the login/status
and provider import steps again.

The approval marker is bound to the installed commit. A restore or pin change
invalidates it.

## Connect Hermes Desktop

On the client machine, register the remote owner host using its existing SSH
alias:

```sh
yard remote add hermes OWNER_SSH_ALIAS --yard hermes
ssh -NT \
  -o ExitOnForwardFailure=yes \
  -o ServerAliveInterval=30 \
  -o ServerAliveCountMax=3 \
  -L 127.0.0.1:19119:127.0.0.1:9119 \
  yard-hermes
```

Set the official Hermes Desktop Remote URL to
`http://127.0.0.1:19119`. Transfer the value from
`/srv/hermes/.serve.env` through a separate secure operator channel and enter
it as the remote session token. Do not paste the token into shell arguments,
configuration repositories, task files, or logs. Closing the foreground SSH
process closes remote access.

## Encrypted backups

Hermes disaster-recovery backups are stopped-service full backups. The
profile-owned owner-host helper validates the archive twice and commits the ZIP
and metadata to an already initialized, encrypted restic repository:

```sh
runtime_root="$(cd "$(dirname "$(readlink -f "$(command -v yard)")")/.." && pwd)"
sudo "$runtime_root/config/profiles/hermes/backup-to-restic.sh" \
  --yard hermes \
  --restic-env /root/.config/subyard/hermes-restic.env \
  --type scheduled
```

The selected environment file must be root-owned, non-symlinked, and mode
`0600` or stricter. It supplies normal `RESTIC_REPOSITORY` and password-source
variables. Keep the repository outside the yard's removable storage,
preferably off-host. The helper reports a verified snapshot ID and applies
retention of 7 daily, 4 weekly, and 6 monthly snapshots for that yard. Schedule
regular `restic check` separately.

Create a `pre-update` backup before changing the pin and a `pre-teardown`
backup before destructive teardown. Provision refuses a commit change without
a verified backup marker for the currently installed commit.

For a restore, provision a clean yard with the same exact profile commit, copy
the verified ZIP into that yard, then run:

```sh
yard -Y hermes shell --root -- hermes-restore /path/to/hermes-backup.zip EXPECTED_SHA256
```

The restore leaves the service disabled and removes provider approval. Recheck
external credentials, repeat `codex login --device-auth`, verify `codex login
status`, import `openai-codex` again, run `hermes doctor`, make a real inference,
and approve the provider again. Do not use cross-version import as rollback:
restore the old runtime pin and its matching backup together.

## Maintainer acceptance

The disposable-host acceptance creates two isolated named yards, verifies
loopback REST/WebSocket authentication and SSH tunnel closure/reconnect, writes
representative persistent state, commits a stopped-service backup to an
encrypted restic repository, and restores it into the second clean yard:

```sh
dev/agent-e2e.sh --purpose hermes-profile --vm 1 -- \
  ./dev/e2e/hermes-profile.sh
```

That default lane uses a provider fixture. A release candidate must also pass
the secure maintainer lane with `HERMES_E2E_REQUIRE_CODEX=1`: stage the
maintainer's Codex auth only inside the same disposable lease and outside the
filtered worktree archive, never as an argument or environment value and never
in tracked files or logs. The lane performs a real terminal-tool inference
both before backup and after clean restore.
