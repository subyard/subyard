# OpenClaw in this yard — L1 build / test / live (self-serve)

You are in an **L1 yard**: several non-isolated agents share ONE machine and ONE set of caches.
Build and unit tests run from your own checkout after dependency installation. Live model tests
need a provisioned staging runner and test credentials. The model-only command below does not
start a gateway or connect Telegram.

Exact script names below follow the project's own `package.json`; confirm them with `pnpm run` if a
command is reported missing (the vendored project moves).

## Build
Run from your project workspace (the checkout you were given):

    pnpm install --frozen-lockfile      # uses the shared store; first run is slow on a cold cache
    pnpm build

## Test suite

    pnpm test:unit:fast                 # fast unit suite

Run one test suite at a time per checkout. Use `OPENCLAW_VITEST_MAX_WORKERS` to bound concurrency
on the shared yard. For a git-submodule checkout, use `OPENCLAW_HEAVY_CHECK_LOCK_SCOPE=worktree`
when running heavy checks: its `.git` is a file, so directory-based lock placement can fail.

The fuller matrices (e2e / Docker / browser / sandbox) need the corresponding optional features
(`browser_tests` / `sandbox_tests`). Their scripts may start local gateways or containers. A timeout
is an incomplete result; check for surviving test processes and containers before another run.

For browser tests, install the binaries required by the checkout's own Playwright version into a
versioned shared cache. System libraries are supplied by the profile; dependency downloads run as
the development user. With the system Chromium provided by `browser_tests`:

    playwright_version=$(node -p 'require("playwright/package.json").version')
    export PLAYWRIGHT_BROWSERS_PATH="/srv/cache/playwright/$(sy-cache key browser "$playwright_version")"
    export PLAYWRIGHT_SKIP_BROWSER_GC=1
    export PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium
    sy-cache browser "$playwright_version" -- pnpm exec playwright install ffmpeg
    pnpm test:ui:e2e

Playwright's ffmpeg binary is needed for video capture even when Chromium is installed system-wide.
If the checkout does not support the system Chromium override, install its pinned browser with
`sy-cache browser "$playwright_version" -- pnpm exec playwright install chromium` and unset `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH`.

## Live model move, no gateway (operator-gated)

`sy-stage` appears on PATH ONLY after the operator runs `yard staging up <zone>` for a staging zone.
If it is absent, the live lane is not provisioned here yet — build and the unit suite still work.
When present, use the project's model-only script, with a provider/model selection matching the
staged credentials. For an Anthropic staging key:

    sy-stage test -- sh -c '
      if [ "${SUBYARD_LIVE_MODEL:-}" != 1 ]; then
        echo "SKIP: live model prerequisite missing (no staging provider key); suite not run"
        exit 0
      fi
      exec env OPENCLAW_LIVE_TEST=1 OPENCLAW_LIVE_PROVIDERS=anthropic \
        OPENCLAW_LIVE_MODELS=modern pnpm test:live:models-profiles
    '

`sy-stage test` runs your command in the staging runner (cwd `/workspace`) and injects the
host-config staging provider key as `ANTHROPIC_API_KEY` for that subprocess only. It does not
decide whether the command should run or turn failures into skips. The wrapper above reports a
missing prerequisite explicitly; that is not a passing live model test. With a key, require a
successful model call and retain the suite's exit status.

`pnpm test:live` is the full live matrix, including gateway suites. Enabling it without credentials
can fail; it is not the model-only command and has no universal clean-skip guarantee. Confirm the
scripts and provider/model selectors against the checkout's own testing documentation on upgrades.

- The project's own live suites gate on the provider key and on the project's own switch (e.g.
  `OPENCLAW_LIVE_TEST=1`, plus per-suite `OPENCLAW_LIVE_*` knobs) — pass those in your command. They
  do NOT read `SUBYARD_LIVE_MODEL`; that flag is only a Subyard convenience signal (set to 1 when a
  key is present) for your own wrappers to branch on, not a project contract.
- Keep keys out of commands, commits and logs; do not print the subprocess environment. Live tests
  may also read existing provider profiles, so a credential-free diagnostic requires an empty
  temporary `HOME`/`USERPROFILE` and an explicit environment allowlist, not just an unset API key.

## Shared caches (you must understand this)

The provisioned pnpm/npm/npx entrypoints and `/opt/venv` pip entrypoints (including
`/opt/venv/bin/python -m pip`) coordinate on the existing `/srv/cache/.sy-cache.lock` inode.
Normal installs hold shared locks, so installs can overlap. Cache maintenance holds an exclusive
lock, waits for installs and blocks new installs. Lock errors stop the command. Run
`sy-cache clean` for npm or `sy-cache purge` for pip; raw `npm cache` / `pip cache` and
`pnpm store` commands also take the exclusive lock. Maintenance inside a shared install/build
fails instead of deadlocking; run it separately. `sy-cache prune` and `all` refuse because a
cross-mount pnpm store can have no hardlinked references: pruning empties the shared download
store. An intentional raw `pnpm store prune` still locks exclusively.

pnpm keys its store by OS, architecture, Node ABI and pnpm version. pip keys by OS, architecture
and interpreter cache tag; pip's own wheel tags and `http-v2` namespace distinguish native cache
formats. npm's integrity-addressed download blobs can be shared across tool versions. Browser
binaries use OS, architecture and Playwright version: use `sy-cache browser VERSION -- CMD...`
for installs and export the matching path for tests as above. Browser GC remains disabled; the
exclusive install lock provides serialization. Direct Node/Playwright API calls must use that
helper too.
Browser commands with workspace/filter or package selectors, global execution, `pnpm dlx`,
package-qualified executables, or ambiguous options require that helper with the selected package's
exact Playwright version. [npm exec](https://docs.npmjs.com/cli/v11/commands/npm-exec/) and
[pnpm dlx](https://pnpm.io/cli/dlx) can select a different package from the local dependency, so the
local version cannot determine their shared browser key.

Keep the same numeric UID/GID for every cache writer. A participating L2 image must contain the
profile's pnpm/npm/npx adapters and the pip hook in its own venv, with the L1 helper and **same lock
file** mounted read-only. The profile's mounts supply the helper and lock; arbitrary base images
do not acquire hooks merely by mounting cache directories. Use private/read-only caches until
those images adopt the contract, or wrap every writer with `sy-cache shared -- CMD...` and every
maintenance command with `sy-cache exclusive -- CMD...`. Other Python environments, pip invoked
as an embedded API, manually bypassed entrypoints and direct cache filesystem edits are outside
this cooperative contract. pip `--isolated` ignores the keyed environment and is rejected unless
`--no-cache-dir` is supplied. This is coordination among cooperating tools, not isolation between
hostile processes running as the same L1 user.

`node_modules`, `.venv`, browser profiles, credentials and Docker data roots stay private to the
checkout/container. Shared directories contain reusable downloads, never sessions or secrets.

## This lane does NOT
- start a gateway or connect Telegram (that is the staging/qa lane, operator-provisioned);
- give you any master credential — a live key, if present, is injected per-run and you never see it.
