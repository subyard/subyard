# User skills from Git sources

The `user-skills` profile clones skill repositories that the operator configures into the yard
and links their skills for every coding agent. It is selected by default in the yard named
`default`; named yards select it only when asked. The shipped source is the
[dev-flow](https://github.com/Dmitry-Borodin/dev-flow) skill.

Subyard only downloads and links. It never updates, rewrites or deletes a clone, and it pins
no revision. Agents in the yard update clones with plain Git.

## Select the profile

| Yard | Without the profile | With the profile |
| --- | --- | --- |
| `default` | `yard config set ENVIRONMENT_PROFILES 'github' --scope yard`, then `yard init` | selected while no `ENVIRONMENT_PROFILES` is saved: `yard init` |
| New named yard | `yard -Y demo init` | `yard -Y demo init --environment-profiles 'github user-skills'` |
| Existing yard | `yard [-Y demo] profile disable user-skills` | `yard [-Y demo] profile enable user-skills` |

`--environment-profiles` sets the whole selection of a new named yard. Rerunning the same
command after an interrupted creation continues as plain init.

## Sources

`USER_SKILL_SOURCES` is a space-separated list of up to eight Git URLs:

```sh
yard [-Y demo] config set USER_SKILL_SOURCES 'https://github.com/OWNER/skills git@example.org:team/skills' --scope yard
yard [-Y demo] init
```

Accepted forms are `https://host/path`, `ssh://[user@]host[:port]/path` and `user@host:path`.
Credentials, queries, fragments, other schemes, local paths and duplicates are refused; a
refusal names the item by position, never by value. An empty value means no sources.

## Clone and link behavior

Each source is an ordinary Git clone under `~/.local/share/subyard-user-skills/<name>`,
where `<name>` combines the repository name and a hash of the configured URL.
Subyard clones only a missing or empty target directory. It never pulls, updates or deletes an existing clone.
Each real `skills/<skill>` directory containing `SKILL.md` is linked into
`~/.agents/skills`, `~/.claude/skills`, `~/.config/opencode/skills`,
`~/.pi/agent/skills` and `~/.hermes/skills`.

Shipped `subyard-` names are reserved. The first source wins duplicate skill names;
existing files, foreign links and symlinked harness directories are skipped.
Disabling the profile, deselecting it or emptying the source list removes only its
exact-target links. Clones and local edits remain.

Cloning runs as the yard developer, with the same Git access as other yard repositories.
A failed attempt prints the source name and a next step, leaves no clone directory and
allows init to complete. The next init retries. `yard profile status user-skills` shows
`ready: false` while a configured clone or link is missing; this is derived from the
filesystem, without a state file or lock. Each clone attempt is bounded to 60 seconds,
with a 300-second total clone budget per init.
The next init also removes abandoned temporary clone directories owned by the hook.
