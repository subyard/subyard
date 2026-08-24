# Subyard — agent instructions

This is the **public** repository. Keep everything here generic and in English; no private
data. Project background, specs, and planning live in a separate private repo.

## Private overlay
If `private/AGENTS.md` exists locally (it is gitignored and lives in the separate private repo),
read and follow it **in addition** to this file. It carries private, non-public working rules

## Validation

Run `make build` to compile the development binary at `.build/yard`; `go.mod` selects the Go
toolchain. Run `./tests/run.sh` before finishing shell or CLI changes. CI additionally runs
`shellcheck -x -S warning` over the CLI, scripts, provision hooks, tests, and Bash completion.

Before choosing change-specific checks, use the advisory selector described in
[`docs/testing.md`](docs/testing.md). It recommends checks without executing them; required
validation and the existing E2E/release gates still apply.

## Agent E2E workflow

The operator owns outer-yard `start`, `stop` and teardown. The root broker owns inner slot
create/start/stop; agents only acquire leases.

Before first use, run `dev/agent-e2e.sh --prepare`. Standard callers reach the bounded facade
through the provisioned yard-to-yard route. The persistent controller key stays under the agent
user's `~/.subyard/e2e/`; every lease uses a separate ephemeral guest key.

Run checks from the current public worktree with:

```sh
dev/agent-e2e.sh --purpose version-check -- ./bin/yard --version
dev/agent-e2e.sh --purpose real-host-check --vm 1 -- ./tests/some-real-host-check.sh
```

The runner filters private/ignored files, verifies the bundle and removes its guest worktree.
Use human `--status`, explicit `--status --json`, bounded `--wait` and `--ssh 1|2` for diagnostics.
Every invocation acquires a new broker slot and prints `yard + project + run + purpose`;
`e2e-vm-1/2` are relative to that lease, not physical slot names. Optional `--slot N` is an atomic
broker request that fails without fallback; it is not permission to enter a physical VM directly.
Keep stateful steps in one wrapper invocation or interactive SSH lease. Raw OpenSSH configuration is
not an agent API. Run `--verify-boundary` after transport or admission changes. Never use the
privileged outer yard as an agent workspace.
Run `dev/e2e/p0-acceptance.sh` for the full allocated two-VM matrix.

If there is any doubt that behavior is covered or a problem is reproduced, use the allocated
`test-vms` to reproduce and verify it on real GNU/Linux hosts. A green host-free test is not a
substitute for the available VM check. Do not change the operator-owned allocation lifecycle.

VM1 must test legacy convergence before current `yard init`:

```sh
SUBYARD_E2E_LEGACY_FIXTURE=1 \
  dev/e2e/seed-test-vms-legacy-state.sh subyard-test-yard yard-test-yard
```

The fixture is restricted to disposable VM1 candidate yards.
