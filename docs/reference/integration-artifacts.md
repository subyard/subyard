# Integration artifact declarations

Accepted: 2026-10-22. Scope: shipped coding-tool artifacts. OpenCode established the
contract; Claude, Codex and pi use the same package declarations. Selection, planning and execution keep their existing owners.

`integration.json` schema 2 adds `artifacts`; schema 1 remains unchanged and cannot
contain artifacts. Unknown schemas, fields and references fail closed. Older engines
reject schema 2 before effects, including downgrade onto a schema 2 package. A rollback
uses its own sealed runtime and frozen recipe; it never reinterprets a newer package.
No settings, inventory, journal, recipe or fingerprint encoding changes are required.
Absent artifact fields retain their previous JSON omission and source binding bytes.
Adding a declaration intentionally changes approved package source identity, even
when delivered bytes match; old journals retain their frozen runtime authority.

Each artifact has a package-local stable `id`, `kind` (`config` or `instructions`),
an optional diagnostic `label`, a developer-home-relative `destination`, and explicit `format` (`raw`, `json`, `toml`, `destination`).
The `destination` policy retains legacy CONFIG behavior: effective `.json` and
`.toml` destinations use structured ownership; all other extensions remain raw.
Source filenames do not determine the format. OpenCode defaults remain raw JSONC;
operator destination overrides retain their previous structured behavior.
Config IDs CONFIG/RULES retain `AGENT_<id>_CONFIG/RULES[_DEST]` file-setting ingress,
import/edit support and existing artifact names. Their package-relative `source` is
the lowest precedence default. Explicit empty sources disable config materialization;
normal host/yard/file/command precedence remains unchanged. Config destination overrides
are validated again by the common resolver. Config sources never follow symlinks.

Instructions reference a typed `absolute-path` scalar `source_setting`. New integrations
declare that optional scalar themselves. Claude, Codex and OpenCode retain `HOST_CLAUDE_MD`, `HOST_CODEX_AGENTS_MD` and
`HOST_OPENCODE_AGENTS_MD` respectively as compatibility ingress, referencing the
existing core path settings without
redeclaring or shadowing it. There is no new alias or second source authority. Other
core settings cannot be referenced: a compatibility reference is restricted to a core
optional absolute-path setting owned by host-files. Instructions use raw bytes, permit
explicit operator symlinks to regular files, and always merge generated preview content,
including missing or empty sources. They remain outside config-status scope. Raw config
is optional when missing at runtime and strict in config status. Files retain existing
raw-file mode 0644 and developer ownership. Structured writers retain their existing
ownership/baseline policy; declarations cannot choose different permissions.

The operation-local catalog validates references and collisions among files and managed
persistence links, including equal and ancestor/descendant paths. The resolver repeats
collision validation for effective destination and selected persistence overrides before
guest effects. Explicit operator HOST_LINKS retain their separate ownership policy. The existing physical
package loader validates package files and includes default artifact bytes in package
source binding and validation inputs. Package paths stay canonical, bounded, regular,
non-symlink and singly linked. Immutable declarations and default sources are rechecked
through existing plan/package guards before effects. Captured config handles also
retain native package source identities, rejecting even equal-byte replacements. Operator sources use the existing
protected/captured source reader and instruction link policy. The resolver only selects
source identities and policy; it never reads payloads or bypasses prospective readers.

One resolver serves config status, runtime, integration inventory and prepared/release
consumers. Converted declarations suppress matching legacy CONFIG/RULES defaults and
instruction mappings for their owning integration. Explicit operator file settings
for undeclared CONFIG/RULES kinds retain their existing compatibility ingress;
they do not add shipped defaults. Undeclared integrations retain the existing ingress. Shipped defaults for Claude,
Codex, OpenCode and pi live entirely outside agents.env; provision/command/persist use the
existing bounded projection consumed by Go and shell. Predecessor adoption paths derive
from declarations; remaining historical adapters and credential exclusions remain closed.
Destinations, identities and effective policy enter existing desired/source bindings.
Captured payloads and frozen recipes retain authority; no native source reread is added
to materialization planning. Independent observation still verifies resulting state.


## Consumer ownership after the coding-tool cutover

| Consumer | Owning input |
| --- | --- |
| Config status/apply and runtime delivery | Package artifacts through the same materialization resolver |
| CONFIG/RULES import/edit, sync, rename and overrides | Existing file-setting contract; shared mappings derive declared logical paths from artifacts, with trace fallback only for undeclared kinds |
| Host instruction paths | Existing typed core compatibility settings or an integration-owned absolute-path scalar |
| Legacy adoption file paths and session IDs | Package declarations; selected inventory still verifies native ownership |
| Selected session links and explicit HOST_LINKS | Existing selection/persistence normalizer; package persist declarations own defaults |
| Prepared config and integration execution | Existing captured-source maps, package bindings and recipes |

No Claude/Codex instruction map or four-tool artifact/persistence path inventory
remains in the common runtime. Pi gains no new installation or probe requirements;
Claude remains configuration-only for CLI distribution and adds a native managed-policy
provision/check hook. A configuration-only provision hook may omit `command` only with an
explicit safe `check` that establishes convergence; hooks declaring an executable retain
their existing command validation. Init, prepared integration and release activation all
use that same check. Codex retains its existing provision/command/check hooks. Credential
exclusions and editor recommendations retain their separate
[compatibility contracts](integration-consumer-compatibility.md).
