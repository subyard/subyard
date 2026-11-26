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

Update clones with plain Git. Subyard never updates or deletes a clone; it clones a configured
source only when its directory is missing or empty, so a deleted clone or store comes back on
the next apply while that source is configured and reachable.

## Rules

`guest.sh` runs as the yard developer in a running yard; a stopped yard is a no-op that
reports converged. Check reports drift, apply converges, and the post-apply check (verify)
accepts skipped sources.

- **Name.** From each URL, the query and fragment are dropped first, then the authority with
  any userinfo, or the SCP host. The name is the last two path segments, without a trailing
  `.git`, and must match `SEG/SEG` with `SEG` = `[A-Za-z0-9][A-Za-z0-9._-]*`. For example,
  `https://github.com/OWNER/skills` and `git@example.org:group/sub/skills` give `OWNER/skills`
  and `sub/skills`; a bracketed SCP host such as `git@[::1]:OWNER/skills` is stripped whole.
  Credentials in URLs are unsupported: use public or SSH-key repositories.
- **Skipped sources.** A source is skipped when its name is invalid, equals an earlier name, or
  its clone fails. Apply prints a warning that names only the source position or the validated
  name, never URL text; check reports drift, so status shows `ready: false`; verify accepts it,
  so init and `yard profile enable user-skills` complete and the next enable or init retries.
- **Clone.** Only when `~/.local/share/subyard-user-skills/<name>` is missing or empty: one
  `git clone` with `GIT_ALLOW_PROTOCOL=https:ssh:file:subyard-github` (`subyard-github` is the
  GitHub profile's broker transport), no terminal prompt and no input, stopped after 60 seconds
  (killed 10 seconds later), into a sibling temporary directory that is then moved into place.
  A non-empty directory is never touched.
- **Link.** Each `skills/<skill>/SKILL.md`, except `subyard-*` names, is linked as
  `<harness>/<skill>` → `<store>/<name>/skills/<skill>` in `~/.agents/skills`,
  `~/.claude/skills`, `~/.config/opencode/skills`, `~/.pi/agent/skills` and `~/.hermes/skills`,
  only where that path is free. An existing entry is never replaced.
- **Harness directories** are used as given: a symlinked harness directory is followed for
  linking and cleanup. A harness directory that cannot be created fails apply.
- **Ours.** A link is ours only when its text is exactly `<store>/<name>/skills/<link name>`
  with a valid name. Paths are compared as strings and never resolved, so hand-made, relative or
  differently spelled links are never touched.
- **Order.** Apply first clones every source, then unlinks, then links, so one run converges
  when a freed link path is taken over by another source.
- **Unlink.** Apply unlinks each of our links unless its name is a configured, non-skipped
  source and its target still has `SKILL.md`. So `yard profile disable user-skills`,
  deselecting the profile and running `yard init`, removing a source or deleting a skill folder
  removes those links and keeps the clones. To disable one skill, delete its folder in the
  clone; removing only its link gets it re-added.
- Concurrent edits by the same user during a run are not guarded.

Out of scope:

- the same-user race between reading and unlinking a link: there is no atomic primitive, and
  the user already has the same rights;
- relative or differently spelled links into the store;
- a hand-made link with our exact spelling, which is treated as ours;
- updating clones or deciding whether a source is trusted;
- a `.tmp-*` directory left in the store after the clone is killed with SIGKILL.
