# GitHub App access

The `github` profile installs the ordinary `gh` CLI and a small Subyard client for short-lived
GitHub App installation tokens. The profile is available by default to the default yard when
`ENVIRONMENT_PROFILES` is unset, and the Hermes preset selects `hermes github`. An explicit
`ENVIRONMENT_PROFILES` list remains authoritative: keep its entries and append `github` when the
profile is wanted. An explicit empty value disables it for that yard.
Named yards do not receive the profile unless their explicit list includes `github`.
The `test-vms` backend does not receive the broker profile.

For a fresh default yard, run `yard init` to converge the shipped selection. For an existing yard,
edit its registered yard setting through the supported yard configuration path, preserving the
current list, then run `yard init` (or the normal `yard config sync` workflow for a synced source).
For example, the yard-scoped writer documented in [configuration](configuration.md) is:

```sh
yard -Y <yard> config set ENVIRONMENT_PROFILES "<current entries> github" --scope yard
yard -Y <yard> init
```

Create a GitHub App, choose its repository permissions, install it on the intended repositories,
and generate an RSA private key in the App settings. Subyard neither creates the App nor grants
additional access. Follow [GitHub’s App setup](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app).

The owner host keeps the GitHub App configuration in `$SUBYARD_CONFIG_HOME/github-app.json` (normally `~/.config/subyard/github-app.json`). Its
schema is:

```json
{
  "app_id": "123456",
  "installation_id": 12345678
}
```

`app_id` contains digits and `installation_id` is a positive integer. Keep the JSON as an
operator-owned, non-symlink mode-`0600`/`0400` file. Import the downloaded PEM on the owner host
through [yard keys](keys.md):

```sh
chmod 0600 /secure/path/github-app.pem
yard keys import /secure/path/github-app.pem --label github-app --consumer github-app-key
yard keys materialize global
```

`yard init` initializes the encrypted ledger. The `github-app-key` consumer uses the `global` zone
and writes `$SUBYARD_KEYS_CONSUMER_ROOT/github/github-app.pem` (normally
`~/.config/subyard/generated/github/github-app.pem`) with mode `0600`. The broker reads this path
automatically, including a custom consumer root. An explicit absolute `private_key_file` in the
App JSON remains supported as an override. Import keeps the original download; remove that duplicate
separately after verifying the consumer.

The key uses the existing encrypted ledger synchronization: enroll an owner peer with
`yard keys trust @peer`, then run `yard keys sync @peer --now`. The peer materializes the key through
the same consumer; configure its App ID and installation ID locally. Default and Hermes yards on
one owner share the materialized key. Use `--local-only` on import to keep a key out of peer sync.
Plaintext keys, the App JSON and the ledger identity stay outside guest delivery, config sync,
host mounts and backups that enter a yard. The App key never enters L1; broker commands, status,
service logs and diagnostics do not print it. A wrapped command still controls its own output.

Use `yard keys list` to find the credential ID. After `yard keys rotate <id> --file <new.pem>`, run
`yard keys materialize global` locally and `yard keys sync @peer --now` for peers. The broker reloads
the key on each request. `yard keys revoke <id>` removes the local consumer; synchronize to propagate
revocation. Missing or invalid key material makes status unconfigured and rejects new token requests.
Revoking a ledger entry does not revoke the key in GitHub or tokens already issued; remove the old
App key in GitHub when retiring it. Removing peer trust cannot erase copies already received.
Concurrent ledger conflicts retain the last verified consumer until resolved, as described in
[merge and recovery rules](keys.md#merge-and-recovery-rules).

`yard init` reconciles the selected profile. It installs the client and ordinary `gh`, provides an ephemeral Git HTTPS helper, and enables the owner-side
`subyard-github-<YARD>.service`. The service uses a pinned yard identity over its protected reverse
SSH transport, starts again after stop/start or reboot, and does not require an interactive owner
SSH session. The profile also installs the short local skill for supported agents.

Use the client wrapper for GitHub CLI commands:

```sh
subyard-github run -- gh repo view
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

Disabling `github` and running `yard init` removes the profile wiring and service. Tokens already
issued remain valid until GitHub expiry or [explicit GitHub revocation](https://docs.github.com/en/rest/apps/installations#revoke-an-installation-access-token). The owner service loads the
protected key and configuration for each request, so replacing the key or JSON takes effect after
the next request without placing credentials in the yard.

For transport failures, inspect `systemctl --user status subyard-github-<YARD>.service` on the
owner host and repeat `yard -Y <yard> init` to repair the managed files and pinned yard transport.
Remote controllers use the same `init` path on the owner; no App credential belongs on the controller.
