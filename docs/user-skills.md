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
`~/.local/share/subyard-user-skills/<repository name>`, with ordinary Git access and the
`subyard-github` broker for GitHub HTTPS. Update clones with plain Git. Each
`skills/<skill>/SKILL.md` is linked into `~/.agents/skills`, `~/.claude/skills`,
`~/.config/opencode/skills`, `~/.pi/agent/skills` and `~/.hermes/skills` where that path is free.
Skills named `subyard-*` are skipped.

If a source cannot be cloned within 60 seconds, init prints a warning and completes;
the next init retries. `yard profile status user-skills` shows `ready: false` until every
configured clone and its links exist.
