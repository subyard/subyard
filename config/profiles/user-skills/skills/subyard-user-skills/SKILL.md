---
name: subyard-user-skills
description: See, repair or update the user skills that Subyard clones from the operator's skill sources into this yard, such as dev-flow.
---

[Profile guide](https://github.com/Subyard/Subyard/blob/master/docs/user-skills.md).

Subyard clones each configured source once into `~/.local/share/subyard-user-skills/<name>`
and links every `skills/<skill>` directory into the agent skill directories. It never updates,
rewrites or deletes a clone.

```sh
helper=/usr/local/share/subyard/skills/subyard-user-skills/sync.py
python3 "$helper" list    # sources, clone directories, linked and skipped skills
python3 "$helper" sync    # clone missing sources and recreate missing links
```

- Sources: `~/.local/share/subyard-user-skills/sources.json`. Edits there last only until the
  next `yard init`; adding or removing a source permanently is the operator's
  `USER_SKILL_SOURCES` setting.
- `sync` exit 3: a source is not reachable. Ask the operator to unlock an SSH key
  (`yard ssh-agent unlock`) or connect GitHub, then run `sync` again. Exit 4: another sync is running.
- Update a clone with plain Git, for example `git -C DIR pull`.
- A skill name starting with `subyard-`, or one already provided by an earlier source, is skipped.
  An existing file or foreign link at a skill path is never replaced.
