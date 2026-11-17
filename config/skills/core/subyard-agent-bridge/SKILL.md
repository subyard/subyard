---
name: subyard-agent-bridge
description: Run a worker of the other vendor (Claude from Codex or Codex from Claude), send messages, handle operator approvals, and resume its session. Use native subagent tools within one vendor.
---

# Cross-vendor workers

Run `python3 <this-skill-directory>/bridge.py`. Python 3 and the selected vendor's
CLI must be installed; `/usr/local/bin/<vendor>` is preferred over PATH, and shell
functions are not used. `--executable /absolute/path` selects another real CLI.
The bridge needs a reachable systemd user manager and runs each turn in its own
transient scope.
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
needed. `stop` waits for interruption, verifies the turn's scope is empty or gone,
and is idempotent. The `--timeout` budget covers waiting for the turn and its lock.
When it expires, `stop` stops the scope directly and verifies cleanup.
A crashed turn reports `failed`. If scope cleanup cannot be verified, `stop` and
`resume` fail with a cleanup diagnostic.

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

`state` is JSON with `status` (`launching`, `running`, `done`, `failed`, `interrupted`),
launcher or bridge PID, session/turn IDs, scope, generation and pending approval IDs.
`status` additionally lists queued message filenames. `events.jsonl` contains
messages with recognized credentials redacted, tool summaries, approval IDs and terminal
outcomes. Terminal state publishes `result_file`, the path to the turn's
`result.json` with state, exit code, session ID, model, duration and token usage.
Immediately after a crash, `status` and `wait` can report `failed` before recovery
publishes `result_file`.
Only `done` publishes `final_file`, the path to `final.md` with the full report.
Use the paths from the current state and keep the report private. Native stderr
is private in `bridge.log`; do not dump logs or protocol
bodies into chat without checking for secrets.

Use `tail --dir /tmp/WORKER -n 20` for recent summarized messages/tool calls.
`start --name NAME` names a worker; `list --root /tmp/PRIVATE-WORKER-ROOT` reports
workers under that private root, independently of one another.

To continue, run `resume --dir /tmp/WORKER --task FILE`, or replace `task.md` and
omit `--task`. On a stopped worker, `send` and `resume` restart the native process
with the same session ID, model, effort and cwd. An active Codex worker steers
with `send` and carries rejected messages into the next turn. An active Claude
worker queues each `send` for the next user turn in the same session and retains
the inbox file until its result arrives. `status` shows the queue. A worker with
no session ID cannot resume.

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
the operator decision, run `approve --dir /tmp/WORKER NNNNNN` or
`deny --dir /tmp/WORKER NNNNNN`. Each ID can be answered only once.

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
