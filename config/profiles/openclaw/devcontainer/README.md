# Devcontainer — `openclaw` profile

The `openclaw` profile's devcontainer, alongside its contract in
[`profile.conf`](../profile.conf): `profile.conf` says *what toolchain* an agent
container needs; this is the ready-to-use `.devcontainer/` that delivers it for the
"Reopen in Container" flow (`yard code .` → Remote-SSH into the yard → Reopen in
Container).

A profile uses the devcontainer in its own folder if present; otherwise it falls
back to `config/profiles/default/devcontainer/`. A project may also ship its own
`.devcontainer/`, which overrides both. This is the committed source for the
devcontainer template staged at `/srv/stacks/devcontainers/templates/openclaw-default/`.

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
  not in this default.
- **Caches are workspace-local.** The profile's shared `/srv/cache/*` caches are
  for project-env boxes (`yard up`); wire them in per project if you want cross-container
  sharing here.

## Optional heavy features
`browser_tests` and `sandbox_tests` are off by default. Enable them via
`OPTIONAL_FEATURES` in `profile.conf` and bake their system libs into a
project-specific image layer — see the notes in that profile.
