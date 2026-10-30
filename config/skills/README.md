# Guest capability skills

Core sources live in `core/<skill-name>/SKILL.md`. Optional profiles own
`config/profiles/<profile>/skills/<skill-name>/SKILL.md` and supporting references.
A package retaining an existing source layout may instead provide `skills.json`,
a mapping from skill name to `{installed-relative-file: package-relative-source}`.
The GitHub package uses that manifest to keep its existing `SKILL.md` canonical.
Names are globally unique across providers.

`scripts/refresh-agent-skills.sh` transports these public static sources to the guest.
`deliver.py` installs a single root-owned canonical directory per skill under
`/usr/local/share/subyard/skills/`, with mode-0644 files and mode-0755 directories.
Each provider has a protected mode-0600 ownership receipt in
`/var/lib/subyard/agent-skills/`. Receipts list only managed content paths and hashes.
Writes to managed content repair drift; unexpected files, foreign links and symlinked
parents cause a diagnostic before the provider's artifacts change. User skills remain
outside these receipts.

Profile provisioning invokes `/usr/local/libexec/subyard/agent-skills apply` only
**after** successfully provisioning the capability. Use provider `profile-<profile>`
and pass `--source`, `--home`, `--uid`, and `--gid`. Profile transports include the
current shared helper as an ephemeral `agent-skills.py` next to the hook. The native
resolved provision binding captures this additional source with its file and parent
identities; the common archive producer revalidates it before guest effects. Hooks prefer
that bound helper, allowing profile activation before a predecessor guest's first core
helper refresh. The GitHub owner transport follows the same contract; its runtime
identity binds the shared helper bytes. Prepared profile recipes capture this dependency.
Historical source roots without it retain their original closed input set. `check` uses exit 0 for current,
10 for expected drift, and 1 for invalid/unsafe input. `check-registered` checks only
protected successful-provider evidence, independently of current content/link drift.
The Android capability checker uses this mode, so static guidance drift leaves
SDK/pool provisioning current. Generic static refresh owns content and link repair.
An absent or unsafe registration still requires successful profile provisioning.
`remove` and `check-removed`
retire or inspect only that provider's artifacts. A provider never registers merely
because its profile is selected. A failed first provision leaves no advertised skill.

Core provisioning and installed runtime refresh install/update the shared helper and
core skills. Static refresh updates only registered profiles, removes providers no
longer selected, and performs no service restart, SDK installation or emulator
allocation. The GitHub owner also refreshes its already registered skill before its
ordinary runtime check, returning before engine, unit or manager changes when only
static skill artifacts were stale. Source identity and the read-only `probe.py` include helper, package content,
file modes, missing content and link drift. New sources join the prepared runtime's
closed inventory; historical frozen recipes retain their original inventory and bytes.

Installed runtime readiness includes static skill drift, including missing links and
artifacts from a deselected provider. Completed release history does not grant new
repair authority to ordinary initialization. Run `yard migrate --check`, then
`yard migrate --yes` on the owner host without a yard selector to assess and authorize
that repair. Static repair and skill retirement do not restart capability services;
capability removal follows its owning lifecycle separately.

All providers link the same content into `~/.agents/skills` (Codex), `~/.claude/skills`,
and `~/.config/opencode/skills`. Links are installed ahead of client installation and
persist through client enable/disable; capability/provider removal owns retirement.
This supports late installation without reprovisioning the capability. Core and the GitHub
provider additionally use pi (`~/.pi/agent/skills`) and Hermes (`~/.hermes/skills`) links. Recognized legacy `.codex/skills`
links to the same canonical directory are removed to avoid duplicate Codex discovery.
Foreign links or skill directories are preserved and diagnosed.

Current prepared materialized owners bind preview cutover explicitly and preflight
owned blocks before retiring services or writing configurations. Their inventory apply
performs the same preview-only migration before file retirement, so missing host sources
and deselected clients preserve unmarked guest text even when materialization precedes
core runtime refresh. An optional intent field distinguishes new recipes from historical
recipes, including historical empty instruction selections; absent historical intent
keeps the original mutation and retirement semantics without rewriting frozen bytes.

`migrate-preview.py` also removes only either exact shipped preview block from an
inventory-owned global instruction file with matching digest and metadata. It preserves
all other bytes and publishes retry identities before replacement. The inventory retains
unmarked text when the host instruction source disappears or the client is disabled,
and records released ownership. Missing ownership evidence, changed guest text, unknown
block bodies and ambiguous markers are preserved; conflicts require inspection rather
than automatic adoption. Host source files are never edited. New instruction payloads
omit recognized preview blocks; frozen historical materialization recipes retain their
original preview composition and source guards for recovery.

Run shared contracts with `bash tests/agent-skills.sh`. Owning profile test runners
cover Android content and GitHub's existing canonical source/discovery migration.
File/readability checks establish delivery only. Native client discovery and model
selection are separate acceptance results.

The [subyard-environment](core/subyard-environment/SKILL.md) skill reads `/etc/subyard/guest-runtime.json`.
Refresh atomically marks it `incomplete` before helper mutations and `current` after delivery
and convergence markers succeed; `last_success` records only completed refreshes. Facts bind the
engine version and effective selection to the guest's yard/machine identity. Capabilities and
readiness remain unknown. Service yards omit core skills but still receive facts.
