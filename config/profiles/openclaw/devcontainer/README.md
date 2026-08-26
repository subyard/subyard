# Reference devcontainer — `openclaw` profile

This directory is a copyable reference for projects that use the toolchain described by
[`profile.conf`](../profile.conf). The current Subyard runtime does not select, copy, or stage these
files automatically. To use the template, copy its `.devcontainer/` and `docker/` directories into
the project, review the resource limits and tool versions, then use the normal "Reopen in Container"
flow after `yard code .` connects through Remote-SSH.

`profile.conf` remains the source for `yard up` project environments. This reference is independent
of that runtime path, so a project that copies it owns future updates to its copy.

## Public-repo rules applied here
This file lives in the **public** repo, so it is generic and English, with **no
secrets, no host paths, no private naming**. It was derived from a proven
OpenClaw devcontainer and cleaned to those rules.

## What it contains
- `docker/dev.Dockerfile` — the dev image: OS toolchain only (Node + corepack +
  arch-scoped pnpm, Python venv, build/dev packages). Version pins mirror
  `profile.conf`.
- `.devcontainer/devcontainer.json` — builds that image, binds the workspace to
  `/workspace`, runs as `dev` (uid 1000), recommends the in-yard coding-agent
  extensions, and hardens the container (`cap-drop=ALL`, `no-new-privileges`).

## Deliberate omissions (don't "fix" these silently)
- **Project test tools are not baked.** Per `profile.conf`, `vitest`/
  `typescript`/`@types/node` and the Python tools come from the project's
  vendored deps (`pnpm --frozen-lockfile`, `pyproject`), so a bump has a single
  source of truth in the project repo. The image carries only the OS toolchain.
- **Coding-agent state is NOT in this container.** The coding agent runs in the
  yard (VS Code Remote-SSH), not in this test container, so no Claude/Codex
  credentials are mounted here. Credentials live per-yard in the yard rootfs; only
  session transcripts are shared host<->yard (the `host-agent-sessions` entry in
  `HOST_MOUNTS`, `config/host.env`) so host-side token stats see the yard's usage.
  The ssh-agent socket is forwarded only when explicitly enabled (`FORWARD_SSH_AGENT=1`);
  see the commented line in `mounts` to use it from the container too.
- **No project lifecycle hooks.** `initializeCommand`/`postCreateCommand` that
  reference a project's own scripts belong in that project's `.devcontainer/`,
  not in this reference.
- **Caches are workspace-local.** The profile's shared `/srv/cache/*` caches are
  for project-env boxes (`yard up`); wire them in per project if you want cross-container
  sharing here.

## Optional heavy features
`browser_tests` and `sandbox_tests` are off by default. Enable them via
`OPTIONAL_FEATURES` in `profile.conf` and bake their system libs into a
project-specific image layer — see the notes in that profile.
