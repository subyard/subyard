# User skills from Git sources

The `user-skills` profile clones skill repositories into the yard and links their skills for
every coding agent. It is selected by default in the yard named `default`; select it in another
yard with `yard -Y NAME config set ENVIRONMENT_PROFILES '... user-skills' --scope yard`, then
`yard -Y NAME init`. The shipped source is the [dev-flow](https://github.com/Dmitry-Borodin/dev-flow) skill.

`USER_SKILL_SOURCES` is a space-separated list of Git URLs:

```sh
yard [-Y NAME] config set USER_SKILL_SOURCES 'https://github.com/OWNER/skills git@example.org:team/skills' --scope yard
yard [-Y NAME] init
```

Init clones each source once, as the yard developer, into
`~/.local/share/subyard-user-skills/<repository path>` (for example `OWNER/skills` or
`group/subgroup/skills`), with ordinary Git access and the `subyard-github` broker for GitHub
HTTPS. Update clones with plain Git. Each `skills/<skill>/SKILL.md` is linked into
`~/.agents/skills`, `~/.claude/skills`, `~/.config/opencode/skills`, `~/.pi/agent/skills` and
`~/.hermes/skills` where that path is free. Skills named `subyard-*` are skipped. Two sources
that map to one clone directory are a configuration error: init warns and skips the later one,
and status shows `ready: false`.

Subyard never restores a clone or skill folder. To turn off one skill, delete its folder in the
clone; the next init removes its dangling links. Removing only a link gets it re-added.
`yard profile disable user-skills`, or deselecting the profile and running `yard init`, removes
only the links that point into the clone store and keeps the clones.

A clone tries plain Git, then `subyard-github run`; each attempt is stopped after 60 seconds
(killed 10 seconds later), so a source takes at most about 140 seconds. If both fail, init
prints a warning naming the repository and completes, as does `yard profile enable user-skills`;
the next enable or init retries.
`yard profile status user-skills` shows `ready: false` while a configured clone directory is
missing or empty, a free link path has no link, or a link into the clone store is dangling.
