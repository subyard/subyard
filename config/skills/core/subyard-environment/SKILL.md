---
name: subyard-environment
description: Inspect this Subyard yard's runtime version, selected profiles, and capability guidance.
---

Read this yard's offline runtime snapshot from any project:

```sh
python3 /usr/local/share/subyard/skills/subyard-environment/facts.py
```

`last_success.version` is the Subyard version of the last completed guest-helper refresh.
`selected_profiles` is selection, not installed capabilities; readiness remains `unknown`.
For `incomplete` (nonzero exit), use only `last_success`. Missing or invalid facts mean unknown.
These are offline snapshots; owner changes appear after guest refresh.

Find capability instructions in `/usr/local/share/subyard/skills/*/SKILL.md`.

For owner-managed Git approval prompts, see [configuration](https://github.com/Subyard/Subyard/blob/master/docs/configuration.md#commit-and-push-approval-prompts).
