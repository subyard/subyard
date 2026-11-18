# Release notes

## Unreleased

- Claude, Codex, and OpenCode now default to running Git commits and pushes
  without a managed approval prompt. Existing yards keep their installed policy
  until the next `yard init`. Set `CODING_TOOL_GIT_APPROVAL=prompt` to keep or
  restore prompts. Restart agent sessions after init to load the new policy.
  See [commit and push approval prompts](configuration.md#commit-and-push-approval-prompts).
