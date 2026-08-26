# Host-side encrypted credential ledger

`yard keys` versions selected static staging and QA credentials outside the guest yard and every
repository. It is not a shared directory and does not synchronize coding-agent OAuth/session stores.
The command name is shorthand: ledger payloads are secret values or config files, commonly staging
`.env` variables, messenger bot tokens, and QA pool credentials. Only `identity/` contains actual
cryptographic keys used to encrypt and sign the ledger.

## Storage and trust

Each physical owner host has one store under `$SUBYARD_CONFIG_HOME/keys/`:

- `shared/` is an append-only signed Git ledger containing SOPS/age ciphertext;
- `local/` is a physically separate versioned ledger that is never exported;
- `identity/` contains the host's dedicated age and Ed25519 signing identities with mode `0600`;
- `peers/`, `state/`, and `quarantine/` contain trust, sync telemetry, and rejected ciphertext.

Yard names are consumer and assignment contexts, not cryptographic identities. Several local yards
share the host ledger while materializing only files authorized for their selected context. The store
is never mounted wholesale into a yard. `yard teardown` preserves it. Back it up together
with `$SUBYARD_CONFIG_HOME`; losing the age identity makes old revisions undecryptable. Treat a copied
identity as credential compromise and rotate every upstream credential it could decrypt.

Credential trust currently resolves a safe-name remote-yard context rather than an OwnerHost
inventory selector. Create that compatibility route with `yard remote add`; the command and project
`sync` remain secret-free. One confirmed `yard keys trust @peer` exchanges both owner hosts' public
age recipients and signing identities, creating reciprocal cryptographic trust without transferring
either private identity. The side with the known local/SSH route is `active` and initiates automatic
ciphertext sync unless `--manual-only` is selected. The reciprocal side is `passive` (`respond-only`)
when it has no reverse route. If that side later enrolls its own route, both sides may be active; an
inbound refresh never erases an existing active route.

## Typical workflow

```bash
yard init
yard remote add srv1 me@srv1
yard -Y srv1 init
yard keys trust @srv1

yard keys import ~/.config/subyard/secrets/legacy/staging/canonical.env --dry-run
yard keys import ~/.config/subyard/secrets/legacy/staging/canonical.env
yard keys materialize canonical
yard keys status
```

Ledger creation, pinned tools, and the automatic timer are all reconciled by ordinary `yard init`;
there is no separate credential-ledger initialization step.

Import accepts only a regular, non-symlink mode-`0600`/`0400` file. Preview reads metadata only. A real
import keeps the legacy source; verify the materialized consumer and its service before separately
removing that duplicate. Secret input otherwise comes from a silent TTY, stdin, or `--file`, never an
argument or environment variable. Supported static consumers are `staging-env`, `qa-secrets`, and
`qa-pool`. Broad `.codex`, `.claude`, OAuth, and mutable staging-runner credential paths are rejected.

## Merge and recovery rules

The record DAG uses immutable signed revisions. Independent credentials and ancestor/descendant
updates merge automatically. Concurrent revisions with the same decrypted value and compatible
metadata receive an automatic merge revision. Revoke or tombstone wins over a concurrent active update.
Concurrent different values remain multiple heads: materialization leaves the last verified file
unchanged until `yard keys resolve <id> --choose <revision>` or `--rotate` creates an explicit successor.

Use `history` to inspect revision IDs, `rollback <id> <revision>` to publish an old encrypted value as a
new successor, `rotate` to replace a value, `revoke` to disable it, and `delete` to publish a tombstone and
remove its current consumer copy. History remains encrypted and append-only. Removing a peer publishes
new heads without that recipient, but cannot erase data already received; stop consumers and rotate or
revoke the real upstream credential.

## Automatic synchronization

The user timer runs at least every six hours with boot catch-up and jitter. `yard keys status` is
read-only and reports each peer as `role=active` or `role=passive`, together with policy, last
attempt/success, errors, staleness, and unresolved heads. Only active peers initiate sync; passive
peers accept the active side's exchange. A reachable automatic peer should not remain unsynchronized
for 24 hours. Use `auto-sync pause|resume` for an active route's explicit policy change and
`yard keys sync --now` for an immediate explicit attempt. Opening VS Code or a shell never starts
credential synchronization.

Exclusive credentials have an authority host and a `host-id/yard-context` assignment epoch.
`move <id> @peer` first verifies the old supported staging consumer stopped, publishes the new assignment,
then syncs/materializes the target.
Supported `yard staging start` fails closed without a fresh authority grant. This is cooperative control-
plane fencing; a manually kept-alive process on a compromised or partitioned host requires an external
epoch-checking proxy or distinct pool identity.
