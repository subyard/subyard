# Release notes

## Unreleased

- `DEV_SUDO` now takes `yes` (default), `no` or `prompt`. `prompt` keeps the passwordless
  grant and makes Claude, Codex and OpenCode ask before `sudo`, `su`, `doas`, `pkexec`,
  `sudoedit` and `run0`; it is a guardrail, not a boundary. The values `1` and `0` are
  refused and not migrated: replace them with `yes` and `no` in each named file.
  See [passwordless sudo inside the yard](configuration.md#passwordless-sudo-inside-the-yard).
- Claude, Codex, and OpenCode now default to running Git commits and pushes
  without a managed approval prompt. Existing yards keep their installed policy
  until the next `yard init`. Set `CODING_TOOL_GIT_APPROVAL=prompt` to keep or
  restore prompts. Restart agent sessions after init to load the new policy.
  See [commit and push approval prompts](configuration.md#commit-and-push-approval-prompts).
