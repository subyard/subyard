# OpenLIT native agent telemetry

OpenLIT is disabled by shipped profile policy. It cannot be enabled or selected for
provisioning, native telemetry export, or dashboard publication. Its executable owning tests
and live acceptance lanes are not shipped or run.
Read-only compatibility fixtures and shell-ownership metadata remain for generic release checks.

Historical `ENVIRONMENT_PROFILES` entries may still request `openlit`. Loading configuration
preserves those authored entries and their provenance, but omits OpenLIT from the effective
selection. Explicit enable requests are refused by profile policy.

The profile's runtime and ownership code remains shipped so ordinary reconciliation can
safely disable an existing installation. Running yards reconcile through their normal
initialization or release activation; stopped yards remain stopped until their next normal
initialization. This source change does not operate on an existing yard by itself.

Reconciliation restores the prior Claude Code and Codex telemetry leaves only while their
current values still match the managed values. Unrelated settings and user changes remain.
It disables the managed OpenLIT service and trace-cutover timer, stops and removes only the
three owned Compose services, and removes the owned dashboard route and deferred publication.
Foreign or ambiguous ownership is refused rather than adopted or removed.

Database volumes, SQLite account data, credentials, pinned images and root-private state
under `/var/lib/subyard/openlit` are retained, together with the Compose network. Attached
compatibility or foreign receivers are preserved. Coding-agent session stores and credentials
remain independent. Yard teardown retains its existing storage policy.

The retained implementation is in the [profile declaration](../config/profiles/openlit/profile.json),
[owner transport](../config/profiles/openlit/owner.sh),
[guest lifecycle](../config/profiles/openlit/guest.sh),
[native settings owner](../config/profiles/openlit/native.py), and
[runtime verification](../config/profiles/openlit/runtime.py).

## Predecessor retirement

AI Observer is retired. Compatible reconciliation removes only proven managed service controls,
its stopped container, and its dashboard route/receipts. Persistent databases, session roots and
credentials remain. Foreign or ambiguous controls are preserved with an ownership diagnostic.
Interrupted cleanup can resume from its protected control-hash receipt. Stopped yards are
reconciled during their next ordinary initialization; they are not started for cleanup.

Old persisted selection and port entries do not prevent configuration loading. The original
files retain their source ownership, unrelated selections and explicit empty lists.
OpenLIT remains disabled. AI Observer database history is retained separately.
