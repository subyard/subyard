# GitHub App access

The `github` profile installs the ordinary `gh` CLI and a small Subyard client for short-lived
GitHub App installation tokens. The profile is available by default to the default yard when
`ENVIRONMENT_PROFILES` is unset, and the Hermes preset selects `hermes github`. An explicit
`ENVIRONMENT_PROFILES` list remains authoritative: keep its entries and append `github` when the
profile is wanted. An explicit empty value disables it for that yard.
Named yards do not receive the profile unless their explicit list includes `github`.
The `test-vms` backend does not receive the broker profile.

For a fresh yard, run `yard init` once to create its core configuration. For an existing running
yard, enable or disable the broker independently:

```sh
yard -Y default profile enable github
yard -Y default profile status github --json
yard -Y default profile disable github
```

Enable preserves the other entries in the yard's effective `ENVIRONMENT_PROFILES` and writes the
selection locally, including when Git supplies the previous list. It invokes only the GitHub
profile's owner/guest hook; it does not run general yard initialization or reconcile other profiles.
Enable and disable require an existing running yard and never start it automatically. Repeat the
same command after an interrupted installation: the desired selection is retained for repair.
Ordinary init still reconciles selected profiles as part of its broader work.

Status reports `enabled` (effective profile selection), `configured` (valid protected owner
connection), `runtime` (profile hook convergence) and `ready` (all three ready). A stopped or missing
yard reports runtime `unavailable`; status never starts it. Readiness does not test repository
permissions against the real GitHub API.

Disable removes this yard's broker service and managed guest client/skill. It preserves the shared
App connection, encrypted credentials, other yards and ordinary `gh` authentication. Already issued
tokens remain valid until their expiry or external revocation; disable stops new issuance here.

Implementation, owner-service hooks, setup declarations and tests live in
[`config/profiles/github/`](../config/profiles/github/). The profile ships its own native broker/client
binary; the core invokes it through the [profile extension contract](control-plane.md#profile-extensions).

## First-time setup

Run `yard -Y default profile enable github` in a terminal **on the owner host**. If the App
connection is missing, enable offers a short setup conversation before its single confirmation.
To prepare the connection separately, including while the yard is stopped, run:

```sh
yard -Y default profile setup github
```

Setup does not select the profile or change the yard runtime. The connection is shared by enabled
yards on this owner; a second yard reuses it. Ordinary interactive init also offers this setup for
selected profiles. Profile commands run on the owner host, where the protected download is available.

1. Create a GitHub App, choose its repository permissions, install it on the intended repositories,
   and download its RSA private key. Follow [GitHub’s App setup](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app).
2. Enter the numeric **App ID** from the App settings (not the client ID), then a positive
   default **installation ID** from an installation URL, for example the numeric suffix of
   `https://github.com/settings/installations/12345678`.
3. Enter the downloaded PEM's path on that owner host. `~/` is supported. If the encrypted ledger
   already has the complete connection, setup restores it without asking for identifiers or another
   download. A legacy key-only entry can reuse the protected local App settings.
4. Review the connection/profile plan and confirm once. Setup protects the selected PEM and stores it
   together with both identifiers in one encrypted revision. It materializes the complete connection
   as one mode-`0600` owner-side JSON file.

No manual JSON, import flags or separate materialization command is needed. Input collection
reads source metadata only; the downloaded key is read and its permissions tightened only after
confirmation. The source must be an operator-owned regular file, not a symbolic or hard link.
The original download is kept. The complete connection follows the ledger's normal synchronization
to trusted credential peers; use `--local-only` when adding or importing it if it must stay on this host.

Enter at any field postpones setup; run interactive profile setup again to resume. Enable does not
change the selection when required setup is skipped. End-of-input cancels
preparation without applying setup. Existing valid settings are reused, and missing default key
material can be restored without duplicating a ledger entry. Existing malformed settings or
unsafe key files are reported and preserved for explicit repair. Connection rotation remains a
`yard keys rotate` operation.

`--yes`, `ASSUME_YES=1`, non-interactive runs, RPC planning, `init --configs` and `init --reset`
do not collect setup input. A synchronized connection can be restored non-interactively, and
ordinary init can migrate a valid legacy key plus protected local settings under its existing
confirmation. When required inputs are missing, non-interactive init reports how to finish setup
on the owner host. Non-interactive profile enable/setup require a complete connection or a ledger
entry that can be restored without input. Remote controllers do not receive the App key or prompt
for a local download.
For a fresh Hermes yard, its preset already selects `hermes github`; after creating the yard,
plain `yard -Y hermes init` also resumes any postponed setup.

The approved key setup runs before Incus provisioning can restart init in a new group session.
If later provisioning fails, the saved App setup remains available to the next init. Invalid PEM
contents are rejected before publishing a connection; source permissions may already
have been tightened. Setup validates the local key and config, but does not contact GitHub or mint
a token. Verify actual installation permissions afterward with a wrapped command against an
intended repository, as shown below.

## Manual setup and automation

The `github-app-key` consumer uses zone `global` and materializes the complete connection at
`$SUBYARD_KEYS_CONSUMER_ROOT/github/connection.json` (normally
`~/.config/subyard/generated/github/connection.json`). It contains one coherent version of the
identifiers and private key:

```json
{
  "schema_version": 1,
  "settings": {
    "app_id": "123456",
    "installation_id": 12345678
  },
  "private_key": "<complete RSA private-key PEM, with JSON-escaped newlines>"
}
```

`app_id` contains digits and `installation_id` is a positive integer. The example above shows the
schema; replace the private-key placeholder with the complete PEM. Keep an import file as an
operator-owned, non-symlink mode-`0600`/`0400` file and import it through [yard keys](keys.md):

```sh
chmod 0600 /secure/path/github-connection.json
yard keys import /secure/path/github-connection.json --label github-app --consumer github-app-key
yard keys materialize global
```

Interactive `yard profile setup github` initializes the encrypted ledger when needed; ordinary
`yard init` also initializes it. The broker reads the generated connection directly,
including a custom consumer root. Import keeps the original file; remove that duplicate separately
after verifying the consumer.

The whole connection uses existing encrypted ledger synchronization: enroll an owner peer with
`yard keys trust @peer`, then run `yard keys sync @peer --now`, or let the enrolled automatic route
sync it. The peer materializes the same App ID, installation ID and key without manual setup.
The installation ID selects the default for commands without a repository target. The same App
may be installed on personal accounts and multiple organizations; targeted commands discover the
matching installation with the owner-side App key, without another connection or key import.
Default and Hermes yards on one owner share the connection. Use `--local-only` on import to keep
the whole connection out of peer sync. Plaintext connection files and the ledger identity stay
outside guest delivery, config sync, host mounts and backups that enter a yard. The App key never
enters L1; broker commands, status,
service logs and diagnostics do not print it. A wrapped command still controls its own output.

Existing key-only ledgers and `$SUBYARD_CONFIG_HOME/github-app.json` remain supported migration
inputs. `yard profile setup github` and ordinary `yard init` combine a valid protected local JSON and the existing key into the
encrypted connection, preserving its credential ID and local-only scope. Repeating init does not
publish another migration revision. After verifying a matching legacy connection, setup replaces
its local settings with the protected delegation marker `{"use_credential_settings":true}` so later
revisions can change identifiers and key together. Invalid files or conflicting local and
synchronized settings are preserved for explicit repair. Local absolute `private_key_file` overrides remain supported;
their paths and transport settings are not included in the synchronized connection.

Use `yard keys list` to find the credential ID. Rotating with a raw PEM keeps both identifiers;
rotating with a complete connection JSON changes all three values together.
After `yard keys rotate <id> --file <new.pem>`, run
`yard keys materialize global` locally and `yard keys sync @peer --now` for peers. The broker reloads
the complete connection on each request. `yard keys revoke <id>` removes the local consumer;
synchronize to propagate revocation. Missing or invalid key material makes status unconfigured
and rejects new token requests.
Revoking a ledger entry does not revoke the key in GitHub or tokens already issued; remove the old
App key in GitHub when retiring it. Removing peer trust cannot erase copies already received.
Concurrent ledger conflicts retain the last verified consumer until resolved, as described in
[merge and recovery rules](keys.md#merge-and-recovery-rules).

`yard init` reconciles the selected profile. It installs the client and ordinary `gh`, provides an ephemeral Git HTTPS helper, and enables the owner-side
`subyard-github-<YARD>.service`. The service uses a pinned yard identity over its protected reverse
SSH transport, starts again after stop/start or reboot, and does not require an interactive owner
SSH session. The profile also installs the short local skill for supported agents.

## Ordinary Git with original-authentication fallback

The opt-in yard modes `GITHUB_GIT_MODE=broker-first` and `GITHUB_GIT_MODE=broker-last` route
ordinary system Git `clone`, `fetch`, `pull`, `push` and `ls-remote` for canonical `https://github.com/owner/repo`,
`git@github.com:owner/repo` and `ssh://git@github.com/owner/repo` URLs (with optional `.git`).
Choose `broker-first` to prefer App authentication, or `broker-last` to prefer existing Git/SSH access.
Enable the profile on the owner for a running yard, then set the mode and authorize runtime repair:

```sh
yard -Y default profile enable github
yard -Y default config set GITHUB_GIT_MODE broker-last --scope yard
yard -Y default migrate
yard -Y default profile status github --json
```

Inside the yard, `subyard-github status --git` reports the effective Git mode and whether a
custom global-config location overrides the managed wiring. This local check mints no token.
To turn routing off while retaining the broker and `gh` wrapper:

```sh
yard -Y default config set GITHUB_GIT_MODE off --scope yard
yard -Y default migrate
```

Changing the mode changes the desired installed runtime. Run `yard migrate` after setting it;
ordinary mutations remain gated until that fresh assessment and confirmation reconcile the wiring.
Use the same repair command for managed fragment drift reported by profile status.

The mode defaults to `off`. `yard profile disable github` removes this yard's managed router,
include and fragment as well as its broker. Repair and release activation use the same profile
lifecycle; repeated application preserves current files. The owned include is in the developer's
`~/.config/git/config`; its fragment is `~/.config/subyard/github-git.conf`. Delivery of
`~/.gitconfig` preserves that include. Foreign settings and credential helpers remain intact;
conflicting GitHub rewrites or unsafe owned paths refuse installation. `GIT_CONFIG_GLOBAL` or a
custom `XDG_CONFIG_HOME` can bypass this standard XDG location; inspect `status --git` from the
same environment as the affected command. Command and repository configuration retain Git's
ordinary precedence. Aliases and other hosts are outside this routing scope.

With `broker-last`, ref discovery first tries the original transport, including ordinary HTTPS
credentials or anonymous access, and SSH keys/configuration. Successful original discovery never
requests broker access or a token. Only failed discovery tries the broker; cancellation stops
selection. If that broker attempt fails, the command fails without retrying the original transport.

When tried, the broker checks the selected repository's installation and effective token
permissions through `/git-access/v1`; legacy `/token` is unchanged. Reads require Contents read
or write. Every push conservatively requires both Contents write and Workflows write, including
code-only pushes. Missing or unknown grants, an unavailable/unconfigured broker, an old broker,
or failed read-only broker discovery choose the original transport in `broker-first` before refs
reach parent Git.
Targeted requests never retry the default installation, and token scope remains installation-wide.

Broker HTTPS uses stock Git with an ephemeral credential helper isolated from ordinary helpers,
stores, authorization headers and `.netrc`, including inherited command configuration. Original
HTTPS retains its existing credential configuration; original SSH uses native
Git and the developer's SSH command, config, agent and host trust. Saved remote URLs, push URLs
and refspecs are unchanged. After publishing refs, the selected identity stays fixed: rejection,
partial push, cancellation or a lost response is an error, with no automatic replay under another
identity. Atomic pushes, explicit leases and push options retain native Git semantics.
Switching authentication prints a fixed reason without upstream details. Tentative discovery is silent;
after selection, native progress and errors are shown with broker credentials redacted.

Parent Git runs `pre-push` once, but its second argument is the effective
`subyard-github::...` helper URL. Hooks that parse this URL, including Git LFS integration, require
separate compatibility work; transparent LFS/custom-hook support is not promised. The helper
supports fetch/push/option rather than connect/stateless-connect, so `fetch --negotiate-only`
is unsupported. Git's pack engine remains native. Submodules allow only the narrowly validated
`subyard-github` protocol; the profile never broadly enables arbitrary protocols. Live GitHub
permission policy and external authentication still require verification with the intended account.

Explicit `subyard-github run -- git ...` retains its earlier policy in either mode: HTTPS uses
only the broker and SSH uses its original transport. `gh` wrapper behavior is unchanged.

## Use and verify access

Use the client wrapper for GitHub CLI commands against a repository available to the App:

```sh
subyard-github status
subyard-github run -- gh repo view owner/repo
subyard-github run -- gh run list -R organization/repo
subyard-github run -- gh api repos/organization/repo/actions/runs
subyard-github run --repo organization/repo -- sh -c 'gh run list'
```

`subyard-github status` reports `{"configured":false}` until protected App settings are installed. No token is minted by this check.

Direct `gh` commands select from `-R`/`--repo`, the repository argument of `gh repo view` or
`gh repo clone`, or a literal `repos/owner/repo/...` API endpoint (also as an
`https://api.github.com/repos/...` URL). Native `repos/{owner}/{repo}/...` placeholders use
the selected `GH_REPO`. Otherwise commands use `GH_REPO`,
then the current checkout's `origin`. SSH origins are used only to identify a repository;
their transport is preserved. Conflicting explicit repository selectors are rejected.
For other programs, aliases, shell scripts or ambiguous targets, pass the wrapper's
`run --repo owner/repo -- COMMAND`. This also sets `GH_REPO` in the child, so nested `gh`
commands use the selected repository. With no target, commands use the configured default
installation. A token serves one installation; run separate wrappers for `gh` commands against
repositories in different installations.

For Git HTTPS, use `subyard-github run -- git clone https://github.com/owner/repo.git` (or
wrap `git fetch` / `git push`). The wrapper supplies an ephemeral credential helper through the
child process environment. Each GitHub credential URL selects its own installation, including
when a command accesses multiple remotes. Direct Git commands obtain tokens only through that
helper; they do not mint an initial `GH_TOKEN`. Pass `run --repo owner/repo` if a Git hook also
needs GitHub CLI authorization. With the default `GITHUB_GIT_MODE=off`, ordinary unwrapped commands keep their existing
authentication. The GitHub CLI auth store is unchanged. The optional mode below owns a separate
Git configuration include. Token requests have no per-call approval or
permission overrides: GitHub App installation permissions determine the accessible
repositories and actions. Repository selection uses
[GitHub's App-authenticated installation lookup](https://docs.github.com/en/rest/apps/apps#get-a-repository-installation-for-the-authenticated-app),
so every repository granted to this App can select its personal or organization installation.
A missing installation or unavailable repository fails without falling back to the default.
Installation lookup and token rejection have separate diagnostics; a successful mint does not
prove an operation's permission. Each token request creates a new token, including when a previous one is
still valid. GitHub installation tokens expire after one hour; rerun the wrapper to refresh authorization
for a later command. See [GitHub’s installation-token contract](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app). The read-only status surface reports a configured boolean; it does not
expose token material or per-request authorization details.

Use matching owner and guest profile versions for repository routing. A new client fails closed
against an older broker that accepts only empty token requests; it never retries against the
default installation. Existing empty requests retain their default-installation behavior.

`yard -Y default profile disable github` removes the profile wiring and service. Tokens already
issued remain valid until GitHub expiry or [explicit GitHub revocation](https://docs.github.com/en/rest/apps/installations#revoke-an-installation-access-token). The owner service loads the
protected connection for each request, so materializing a new revision takes effect on
the next request without placing credentials in the yard.

For transport failures, inspect `systemctl --user status subyard-github-default.service` on the
owner host. For installed-runtime drift, run `yard -Y default migrate` with fresh authorization;
repeat `yard -Y default profile enable github` to resume profile installation when release readiness permits it.
Remote controllers use the same `init` path on the owner; no App credential belongs on the controller.
