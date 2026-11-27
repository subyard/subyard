# Agent instructions from a dotfiles repository

The `user-dotfiles` profile clones one Git repository of yard dotfiles into the yard and links
its shared agent instruction file for Claude, Codex and OpenCode. Subyard provides the file but
does not own or update it. The yard has its own minimal instruction repository, written for
agents in the yard and separate from host dotfiles, which carry host configuration a yard does
not need; Subyard never copies host instruction files into a yard.

The profile is selected by default in the yard named `default`; select it in another yard with
`yard -Y NAME profile enable user-dotfiles`, and remove it with
`yard -Y NAME profile disable user-dotfiles`. A yard whose `ENVIRONMENT_PROFILES` is set
explicitly uses only that list. The shipped source is the public
`https://github.com/subyard/yard-dotfiles`. A private source needs
[GitHub](github.md#ordinary-git-with-original-authentication-fallback) or other Git access in the
yard; otherwise apply warns and the profile is not ready. To use another repository:

```sh
yard [-Y NAME] config set USER_DOTFILES_SOURCE https://github.com/OWNER/yard-dotfiles --scope yard
yard [-Y NAME] config set USER_DOTFILES_SUBDIR yard --scope yard   # optional
yard [-Y NAME] init   # in another yard: yard -Y NAME profile enable user-dotfiles
```

## Layout

The repository keeps one shared instruction file at `agents/AGENTS.md`, below
`USER_DOTFILES_SUBDIR` when it is set. A root `AGENTS.md` belongs to the repository itself, for
agents that work on it, and is never linked. The profile links:

| Agent | Path in the yard | Link target |
| --- | --- | --- |
| Claude | `~/.claude/CLAUDE.md` | `<clone>[/SUBDIR]/agents/AGENTS.md` |
| Codex | `~/.codex/AGENTS.md` | `<clone>[/SUBDIR]/agents/AGENTS.md` |
| OpenCode | `~/.config/opencode/AGENTS.md` | `<clone>[/SUBDIR]/agents/AGENTS.md` |

`<clone>` is `~/.local/share/subyard-user-dotfiles/OWNER/REPO`.

## Updating

Subyard clones the repository once and never pulls, resets or deletes the clone. To update, ask
an agent in the yard to run `git pull` in the clone; agents read the new text from their next
session. `yard init` is not needed. A deleted clone comes back on the next apply while the source
is configured and reachable.

## Rules

`guest.sh` runs as the yard developer in a running yard; a stopped yard is a no-op that reports
converged. Check reports drift, apply converges, and the post-apply check (verify) accepts
skipped work, so init and `yard profile enable user-dotfiles` complete and the next enable or
init retries. Apply prints a warning for each skipped item; check reports drift, so status shows
`ready: false`.

- **Name.** `OWNER/REPO` comes from the URL as for [user skills](user-skills.md#rules): the
  query, fragment, authority or SCP host are dropped and the last two path segments are kept,
  each matching `[A-Za-z0-9][A-Za-z0-9._-]*`. An invalid name skips the source; warnings never
  show URL text. Credentials in URLs are unsupported: use public, SSH-key or GitHub broker
  (`subyard-github::`) repositories.
- **Subdirectory.** `USER_DOTFILES_SUBDIR` is a relative path whose segments match the same
  pattern; leading and trailing slashes are ignored. Any other value skips the source.
- **Clone.** Only when the clone directory is missing or empty: one `git clone` with
  `GIT_ALLOW_PROTOCOL=https:ssh:file:subyard-github`, no terminal prompt and no input, stopped
  after 60 seconds, into a sibling temporary directory that is then moved into place. A failed
  clone is skipped with a warning.
- **Missing file.** A clone without `[SUBDIR/]agents/AGENTS.md` gets no links and a warning.
- **Link.** Each path in the table is linked only where it is free. An existing file, directory
  or other link is left in place with a warning; move it away and run `yard init` to link it.
  `yard init` first removes an unchanged instruction copy that an earlier release made, so the
  same init links that path.
- **Ours.** A link is ours only when its text is exactly
  `~/.local/share/subyard-user-dotfiles/OWNER/REPO[/SUBDIR]/agents/AGENTS.md` with plain
  segments. Paths are compared as strings and never resolved; relative or differently spelled
  links are never touched.
- **Unlink.** Apply removes each of our links that is not the configured target: after
  `yard profile disable user-dotfiles`, deselecting the profile and running `yard init`, emptying
  `USER_DOTFILES_SOURCE`, changing the source or subdirectory, or deleting the file. The clone is
  kept. Apply moves a link aside and checks it again before removing it; anything else found
  there is moved back, or kept beside the path with a warning when the path was re-created.

Before rolling back to a release that copies host instruction files (0.32.0 and older), run
`yard -Y NAME profile disable user-dotfiles` in each yard that uses the profile, or move any
other file at these paths away. While a file or link remains there, such a rollback is refused
with "the retained release cannot provide a safe rollback plan", the three paths and this
recovery.

Out of scope: updating the clone, deciding whether a source is trusted, other dotfiles than the
agent instruction file, and a hand-made link with our exact spelling, which is treated as ours.
