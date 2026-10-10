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
yard -Y <yard> profile enable github
yard -Y <yard> profile status github --json
yard -Y <yard> profile disable github
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

Run `yard -Y <yard> profile enable github` in a terminal **on the owner host**. If the App
connection is missing, enable offers a short setup conversation before its single confirmation.
To prepare the connection separately, including while the yard is stopped, run:

```sh
yard -Y <yard> profile setup github
```

Setup does not select the profile or change the yard runtime. The connection is shared by enabled
yards on this owner; a second yard reuses it. Ordinary interactive init also offers this setup for
selected profiles. Profile commands run on the owner host, where the protected download is available.

1. Create a GitHub App, choose its repository permissions, install it on the intended repositories,
   and download its RSA private key. Follow [GitHub’s App setup](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app).
2. Enter the numeric **App ID** from the App settings (not the client ID), then the positive
   **installation ID** from the installation URL, for example the numeric suffix of
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
Different broker instances using the same GitHub App installation use the same identifiers.
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

## Use and verify access

Use the client wrapper for GitHub CLI commands against a repository available to the App:

```sh
subyard-github status
subyard-github run -- gh repo view owner/repo
```

`subyard-github status` reports `{"configured":false}` until protected App settings are installed. No token is minted by this check.

For Git HTTPS, use `subyard-github run -- git clone https://github.com/owner/repo.git` (or
wrap `git fetch` / `git push`). The wrapper supplies an ephemeral credential helper through the
child process environment. Ordinary unwrapped commands keep their existing authentication;
the profile does not modify Git configuration or the GitHub CLI auth store. Token requests have no per-call approval or
repository/operation scope: GitHub App installation permissions determine the accessible
repositories and actions. Each invocation requests a new token, including when a previous one is
still valid. GitHub installation tokens expire after one hour; rerun the wrapper to refresh authorization
for a later command. See [GitHub’s installation-token contract](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app). The read-only status surface reports a configured boolean; it does not
expose token material or per-request authorization details.

`yard -Y <yard> profile disable github` removes the profile wiring and service. Tokens already
issued remain valid until GitHub expiry or [explicit GitHub revocation](https://docs.github.com/en/rest/apps/installations#revoke-an-installation-access-token). The owner service loads the
protected connection for each request, so materializing a new revision takes effect on
the next request without placing credentials in the yard.

For transport failures, inspect `systemctl --user status subyard-github-<YARD>.service` on the
owner host and repeat `yard -Y <yard> profile enable github` to repair the managed files and pinned yard transport.
Remote controllers use the same `init` path on the owner; no App credential belongs on the controller.
