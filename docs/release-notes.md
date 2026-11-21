# Release notes

## Unreleased

- New yards inherit the server's timezone when available in the guest and otherwise default to UTC.
- `DEV_SUDO` now takes `prompt` (default), `yes` or `no`. `prompt` keeps the passwordless
  grant and makes Claude, Codex and OpenCode ask before `sudo`, `su`, `doas`, `pkexec`,
  `sudoedit` and `run0`; it is a guardrail, not a boundary. `yes` keeps the earlier
  behaviour: passwordless sudo without prompts. Existing yards change: after `yard update`
  reprovisions a running yard's agents, or the next `yard init`, agents ask before sudo unless
  `DEV_SUDO=yes`. The values `1` and `0` are refused and not migrated, and older runtimes
  refuse the new values. Before `yard update`, remove every `DEV_SUDO` assignment with the
  older runtime; after it, set `yes`, or `no` where you had `0`, and run `yard init`. Publish a
  shared value only after every owner host is updated. Before downgrading a yard that applied
  `yes` or `no`, run `yard init` without the assignment. See
  [passwordless sudo inside the yard](configuration.md#passwordless-sudo-inside-the-yard).
- With `CODING_TOOL_GIT_APPROVAL=prompt`, the Claude and Codex hook now refuses any literal
  mention of `git commit` or `git push` that the client's own prompt does not cover: anything
  other than a direct command, optionally after a plain prefix such as `cd <directory> &&`, or text
  in a plain read-only command such as `rg` or `echo`. Codex also refuses them in a command with
  a redirect, variable, `$(…)` or backtick substitution, newline or comment, which its prompt
  rules do not match. Some commands that only name them, such as
  `gh pr create --title 'git push fix'`, are refused as well; run the direct command or a plain
  form instead.
- Claude, Codex, and OpenCode now default to running Git commits and pushes
  without a managed approval prompt. Existing yards keep their installed policy
  until the next `yard init`. Set `CODING_TOOL_GIT_APPROVAL=prompt` to keep or
  restore prompts. Restart agent sessions after init to load the new policy.
  See [commit and push approval prompts](configuration.md#commit-and-push-approval-prompts).
