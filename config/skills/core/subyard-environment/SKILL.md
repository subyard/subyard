---
name: subyard-environment
description: Inspect this Subyard yard's runtime version, selected profiles, and capability guidance.
---

You work in a Subyard yard, an isolated development guest. From any project, run:

```sh
python3 /usr/local/share/subyard/skills/subyard-environment/facts.py
```

`last_success.version` is the Subyard version of the last completed guest-helper refresh.
`selected_profiles` is selection, not installed capabilities; readiness remains `unknown`.
For `incomplete` (nonzero exit), use only `last_success`. Missing or invalid facts mean unknown.
These are offline snapshots; owner changes appear after guest refresh.

Find capability instructions in `/usr/local/share/subyard/skills/*/SKILL.md`.
