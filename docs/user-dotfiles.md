# Dotfiles repository in the yard

The `user-dotfiles` profile follows the dotfiles convention of GitHub Codespaces and VS Code Dev
Containers: it clones one Git repository of yard dotfiles into the yard and runs the
repository's install script once. The repository, not Subyard, decides what to install. The yard
has its own minimal repository, written for agents in the yard and separate from host dotfiles,
which carry host configuration a yard does not need; Subyard never copies host instruction files
into a yard.

The profile is selected by default in the yard named `default`; select it in another yard with
`yard -Y NAME profile enable user-dotfiles`, and remove it with
`yard -Y NAME profile disable user-dotfiles`. A yard whose `ENVIRONMENT_PROFILES` is set
explicitly uses only that list. The shipped source is the public
`https://github.com/subyard/yard-dotfiles`: its `install.sh` links its `agents/AGENTS.md` to
`~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md` and `~/.config/opencode/AGENTS.md`, and its
`uninstall.sh` removes those links. A private source needs
[GitHub](github.md#ordinary-git-with-original-authentication-fallback) or other Git access in the
yard; otherwise apply warns and the profile is not ready. To use another repository:

```sh
yard [-Y NAME] config set USER_DOTFILES_SOURCE https://github.com/OWNER/yard-dotfiles --scope yard
yard [-Y NAME] config set USER_DOTFILES_INSTALL_COMMAND script/install --scope yard   # optional
yard [-Y NAME] init   # in another yard: yard -Y NAME profile enable user-dotfiles
```

## Install

Right after the first clone, the profile runs, as the yard developer in the clone directory:

1. the file that `USER_DOTFILES_INSTALL_COMMAND` names, a path relative to the clone; or
2. the first that exists of `install.sh`, `install`, `bootstrap.sh`, `bootstrap`,
   `script/bootstrap`, `setup.sh`, `setup` and `script/setup`; or
3. when none exists, it links each top-level entry of the clone whose name starts with `.`,
   except `.git`, into `~`, leaving an existing path in place with a warning.

An executable script runs directly, any other through `bash`, with no input and a 300-second
limit. A failure is an init warning with the exit status and the last 50 lines of the script's
output. The install runs once per enable: `yard init` never runs it again, also after a failure.
Fix the script and run it in the clone yourself.

## Updating

Subyard never pulls, resets or deletes the clone, `~/.local/share/subyard-user-dotfiles/OWNER/REPO`.
To update, run `git pull` in the clone, then run its install script again yourself, for example
`./install.sh`; agents read changed instruction files from their next session. `yard init` is not
needed.

## Disable

`yard profile disable user-dotfiles`, deselecting the profile and running `yard init`, or
emptying `USER_DOTFILES_SOURCE` runs the clone's `uninstall.sh` when it has one, the same way as
the install, and then forgets the install. Without `uninstall.sh` the installed files stay in
place, with a warning. The clone is always kept, so enabling the profile again reuses it and
runs the install again. Changing `USER_DOTFILES_SOURCE` to another repository uninstalls the old
clone the same way before the new one is cloned and installed.

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
- **Clone.** Only when the clone directory is missing or empty: one `git clone` with
  `GIT_ALLOW_PROTOCOL=https:ssh:file:subyard-github`, no terminal prompt and no input, stopped
  after 60 seconds, into a sibling temporary directory that is then moved into place. A failed
  clone is skipped with a warning and retried by the next apply. An existing clone is reused as
  it is.
- **Install record.** `~/.local/share/subyard-user-dotfiles/.installed` names the installed
  `OWNER/REPO`. Apply writes it before the install runs and removes it after the uninstall; a
  missing record or clone is drift that the next apply installs again.
- **Install command.** `USER_DOTFILES_INSTALL_COMMAND` is a relative path whose segments match
  the name pattern. Another value, or a missing file, installs nothing, with a warning.
- **Earlier copies.** `yard init` first removes an unchanged instruction copy that a release
  through 0.32.0 made, so the same init can install a file at that path.

Before rolling back to a release that copies host instruction files (0.32.0 and older), run
`yard -Y NAME profile disable user-dotfiles` in each yard that uses the profile, then move any
file or link left at `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md` or
`~/.config/opencode/AGENTS.md` away. While a file or link remains there, such a rollback is
refused with "the retained release cannot provide a safe rollback plan", the paths and this
recovery.

Out of scope: updating the clone, deciding whether a source is trusted, and what the
repository's scripts do.
