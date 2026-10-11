---
name: subyard-github
description: Request temporary GitHub App authorization for commands.
---

<!-- subyard-managed-v1 -->
Request a temporary GitHub token for a command: `subyard-github run -- <command> [args...]`.
Direct `gh` commands select the repository from `-R`/`--repo`, `gh repo view/clone`,
repository API endpoints, `GH_REPO`, or the checkout's `origin`.
For an arbitrary command or an ambiguous target, use
`subyard-github run --repo owner/repo -- <command> [args...]`.
The selected repository becomes the child's `GH_REPO`. Git HTTPS credentials route
by their actual URL, including across installations. Direct Git commands do not
mint an initial CLI token; use `--repo` when a Git hook also needs `GH_TOKEN`.
The App's installed repository and permission grants apply. No token combines installations.
