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

For ordinary Git, check `subyard-github status --git`. With the owner-selected
`GITHUB_GIT_MODE=broker-first`, use ordinary `git clone/fetch/pull/push/ls-remote`;
the profile tries permitted App access before the original HTTPS/SSH authentication.
Reads require Contents read/write; every App push requires Contents and Workflows write.
Backend selection stays fixed after ref discovery: never retry a failed/partial push
under another identity automatically. Saved URLs stay unchanged; pre-push sees the
helper URL as its second argument. LFS/custom URL-parsing hooks and fetch
--negotiate-only are outside the first version's compatibility contract.
Explicit `run` keeps HTTPS broker-only and SSH original, and `gh` behavior is unchanged.
