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

## In the yard

| Path | Contents |
| --- | --- |
| `~/.local/share/subyard-user-skills/<name>` | One ordinary clone per source; `<name>` is the repository name plus a hash of the URL |
| `~/.local/share/subyard-user-skills/sources.json` | The configured sources, rewritten by each init |
| `~/.agents/skills`, `~/.claude/skills`, `~/.config/opencode/skills`, `~/.pi/agent/skills`, `~/.hermes/skills` | Links to each clone's `skills/<skill>` directories |

Each `skills/<skill>` directory with a `SKILL.md` becomes one link. Names that start with
`subyard-` are reserved for shipped skills. When two sources provide the same name, the earlier
source wins. An existing file or foreign link at a skill path is never replaced; it is reported
and skipped.

The `subyard-user-skills` skill tells agents how to list sources and repair links:

```sh
python3 /usr/local/share/subyard/skills/subyard-user-skills/sync.py list
python3 /usr/local/share/subyard/skills/subyard-user-skills/sync.py sync
```

## Access

The clone runs as the developer account, like `yard clone`. SSH sources use the yard's agent
socket, so unlock a key with `yard ssh-agent unlock --key PATH --ttl DURATION`. HTTPS sources use
anonymous access or existing Git credentials; a `github.com` source is retried through
`subyard-github` when the GitHub profile is installed. Each attempt is bounded to 60 seconds and
one sync to 300 seconds.

When a source cannot be reached, init still succeeds and delivers the profile skill;
`yard profile status user-skills` reports `diagnostic: source-unavailable`. After access is
granted, an agent runs `sync.py sync`, or the next `yard init` clones the source.

## Disable

Disabling or deselecting the profile removes the links it created and `sources.json`, and
retires the `subyard-user-skills` skill. Clones stay in place; delete them by hand if they are
no longer needed. Changing a source URL leaves the old clone and removes its links.
