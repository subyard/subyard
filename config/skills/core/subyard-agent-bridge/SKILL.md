---
name: subyard-agent-bridge
description: Run a worker of the other vendor (Claude from Codex or Codex from Claude), send messages, handle operator approvals, and resume its session. Use native subagent tools within one vendor.
---

# Cross-vendor workers

Run `python3 <this-skill-directory>/bridge.py`. Python 3 and the selected vendor's
CLI must be installed; `/usr/local/bin/<vendor>` is preferred over PATH, and shell
functions are not used. `--executable /absolute/path` selects another real CLI.
The bridge needs a reachable systemd user manager. Each `start` or `resume`
runs in a new transient scope.
The bridge inherits credentials,
configuration and permissions. Claude uses the native stdio permission host without
overriding its configured permission mode. No SDK package or Orca is required.

Create a private worker directory **outside the repository** (for example with
`mktemp -d /tmp/subyard-worker.XXXXXX`), mode `0700`, and put the assignment in
`task.md` (or supply `--task FILE`). Choose model/effort explicitly, or use a role:

```sh
python3 <this-skill-directory>/bridge.py start --vendor codex --model MODEL --effort high --cwd /absolute/project --dir /tmp/WORKER
python3 <this-skill-directory>/bridge.py send --dir /tmp/WORKER --message 'Additional instruction'
python3 <this-skill-directory>/bridge.py status --dir /tmp/WORKER
python3 <this-skill-directory>/bridge.py wait --dir /tmp/WORKER --timeout 60
python3 <this-skill-directory>/bridge.py interrupt --dir /tmp/WORKER
python3 <this-skill-directory>/bridge.py stop --dir /tmp/WORKER
```

Use `--vendor claude` for Claude. `send` also reads stdin when `--message` is
omitted. The detached bridge owns the worker's stdio; commands return promptly.
`wait` blocks until terminal state, a pending approval or its timeout. Exit codes:
done `0`, failed `1`, pending approval `2`, interrupted `130`, timeout `124`.
Use one background/event wait call for long waits; no coordinator polling loop is
needed. `--timeout` also bounds lock waits for commands. `stop` waits for
interruption and is idempotent. If waiting for the turn or worker lock times out,
`stop` stops the scope directly and verifies it is empty or gone.
A crashed run reports `failed`. If scope cleanup cannot be verified, `stop` and
`resume` fail with a cleanup diagnostic. If `resume` times out waiting for a
lock, run `stop` to recover the scope before retrying.

`start --role developer|reviewer|observer` supplies model/effort defaults: developer
and reviewer use Codex `gpt-6.1-sol` or Claude `opus`, high effort; observer uses
`gpt-6-luna` or `haiku`, medium effort. Explicit `--model`/`--effort` override them.
Developer and observer inherit native permissions; observer receives a no-source-edit
instruction. Reviewer uses a native Codex read-only profile with writable `/tmp`
and `$TMPDIR`, or Claude's required Bash sandbox plus an Edit deny rule covering
all repository file tools. Reviewer TMPDIR must be outside the repository; use
`--env TMPDIR=/tmp/SCRATCH` if necessary. Native sandbox dependencies are required;
failure does not fall back to unrestricted execution. These presets are defaults,
not a boundary against an operator explicitly overriding native settings.

Repeat `--env K=V` to override child environment values; other values, including
PATH/TMPDIR, are inherited. `--cwd` selects the worktree. Add arbitrary vendor
arguments after `--`, e.g. Codex `-- -c key=value --profile NAME` or Claude
`-- --permission-mode manual --max-turns 4 --append-system-prompt 'Extra context'`.
They override role defaults and are retained on resume. Native flags must preserve
the stdio protocol. The bridge always sets Claude's permission host to stdio.
For Codex, the bridge translates `--sandbox` or `-s` to `sandbox_mode` and checks
the effective sandbox before starting the turn. A mismatch fails the turn.
The reviewer role fixes the sandbox, so `start` rejects an explicit sandbox mode
with that role.

`state` is JSON with `status` (`launching`, `running`, `done`, `failed`, `interrupted`),
launcher or bridge PID, session/turn IDs, scope, generation and pending approval IDs.
`status` additionally lists queued message filenames. `events.jsonl` contains
messages, tool summaries, approval IDs and terminal outcomes. Terminal state
publishes `result_file`, the path to the run's
`result.json` with state, exit code, session ID, model, duration and token usage.
Claude token usage sums top-level integer counters across all results in the run.
Immediately after a crash, `status` and `wait` can report `failed` before recovery
publishes `result_file`.
Only `done` publishes `final_file`, the path to `final.md` with the full report.
Use the paths from the current state. `events.jsonl`, `tail` output, and
`final.md` are private and may contain secrets. The bridge applies best-effort
redaction when writing events. `tail` displays the stored summaries.
Keep native stderr in `bridge.log` and protocol bodies private as well.

Use `tail --dir /tmp/WORKER -n 20` for recent summarized messages/tool calls.
`start --name NAME` names a worker; `list --root /tmp/PRIVATE-WORKER-ROOT` reports
workers under that private root, independently of one another.

`send` requires an active worker. It steers the current Codex turn and carries
rejected messages into the next turn. For Claude, `send` queues the message
for the next user turn of the same session. The bridge starts that turn
automatically after the current turn succeeds and keeps the message queued until
its result arrives. `status` shows the queue.

To continue a finished or stopped worker, run `resume --dir /tmp/WORKER --task FILE`,
or replace `task.md` and omit `--task`. A worker needs a session ID to resume
with its existing model, effort and cwd.

## Operator approvals

The bridge writes `turns/GENERATION/approvals/NNNNNN.json` containing `vendor`, `tool`, `command`,
`reason`, the native `request` and the associated Codex `item` when available (for
example, proposed file changes). It publishes the ID in `state.pending_approvals`.
The request stays pending indefinitely until answered; `interrupt` remains available.

1. Read the request and show the operator the action, reason and relevant details
   (including proposed file changes from the associated item when applicable).
2. Wait for the operator's explicit decision. Never decide on the operator's behalf.
3. Write exactly `allow` or `deny` to the matching `.reply` file beside the request, preferably
   by writing a temporary file in that directory and atomically renaming it.

Alternatively, `approvals --dir /tmp/WORKER` lists pending requests; after obtaining
the operator decision, run `approve NNNNNN --dir /tmp/WORKER` or
`deny NNNNNN --dir /tmp/WORKER`. Each ID can be answered only once.

The bridge sends a single-use response and removes both files. It sends no session
allow, rule amendment or permission-mode change. Interruption/failure cancels pending
requests and removes their files. Old approval IDs are not reused on resume.
Invalid, unsolicited or duplicate replies/requests fail the worker closed.

Supported approvals are Codex command execution and file changes, and Claude
`can_use_tool`. Other server requests fail closed, including Codex permission-profile
grants (which grant turn/session access rather than single-operation approval).
This is an explicit coordinator-to-operator flow; it does not inject prompts into
the coordinator's native subagent approval UI.

This product skill does not use the temporary `agent-bridge-local` installation or
its automatic approval behavior. Retiring that installation belongs to rollout.
