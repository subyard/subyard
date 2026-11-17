---
name: subyard-agent-bridge
description: Run a worker of the other vendor (Claude from Codex or Codex from Claude), send messages, handle operator approvals, and resume its session. Use native subagent tools within one vendor.
---

# Cross-vendor workers

Run `python3 <this-skill-directory>/bridge.py`. Python 3 and the selected vendor's
CLI must be on PATH; shell functions are not used. The bridge inherits credentials,
configuration and permissions. Claude uses the native stdio permission host without
overriding its configured permission mode. No SDK package or Orca is required.

Create a private worker directory **outside the repository** (for example with
`mktemp -d /tmp/subyard-worker.XXXXXX`), mode `0700`, and put the assignment in
`task.md`. Choose the worker model and effort explicitly:

```sh
python3 <this-skill-directory>/bridge.py start --vendor codex --model MODEL --effort high --cwd /absolute/project --dir /tmp/WORKER
python3 <this-skill-directory>/bridge.py send --dir /tmp/WORKER --message 'Additional instruction'
python3 <this-skill-directory>/bridge.py status --dir /tmp/WORKER
python3 <this-skill-directory>/bridge.py wait --dir /tmp/WORKER --timeout 60
python3 <this-skill-directory>/bridge.py interrupt --dir /tmp/WORKER
```

Use `--vendor claude` for Claude. `send` also reads stdin when `--message` is
omitted. The detached bridge owns the worker's stdio; commands return promptly.
`wait` returns when state changes or the worker is terminal; timeout exits `124`.
Use the coordinator's supported background/event wait for long waits.

`state` is JSON: `status` (`running`, `done`, `failed`, `interrupted`), bridge PID,
session/turn IDs, generation and pending approval filenames without extensions.
`status` additionally lists queued message filenames. `events.jsonl` contains
messages, tool summaries, approval IDs and terminal outcomes. Only `done` publishes
`final.md`. Native stderr is private in `bridge.log`; do not dump logs or protocol
bodies into chat without checking for secrets.

To continue, replace `task.md` and run `resume --dir /tmp/WORKER`. This restarts the
native process with the same session ID, model, effort and cwd. It clears the old
final answer. Messages racing turn completion can remain queued; `status` shows
them and the next resume delivers them. A worker with no session ID cannot resume.

## Operator approvals

The bridge writes `approvals/NNNNNN.json` containing `vendor`, `tool`, `command`,
`reason`, the native `request` and the associated Codex `item` when available (for
example, proposed file changes). It publishes the ID in `state.pending_approvals`.
The request stays pending indefinitely until answered; `interrupt` remains available.

1. Read the request and show the operator the action, reason and relevant details
   (including proposed file changes from the associated item when applicable).
2. Wait for the operator's explicit decision. Never decide on the operator's behalf.
3. Write exactly `allow` or `deny` to the matching `approvals/NNNNNN.reply`, preferably
   by writing a temporary file in that directory and atomically renaming it.

The bridge sends a single-use response and removes both files. It sends no session
allow, rule amendment or permission-mode change. Interruption/failure cancels pending
requests and removes their files. Old approval IDs are not reused on resume.
Invalid, unsolicited or duplicate replies/requests fail the worker closed.

Supported approvals are Codex command execution and file changes, and Claude
`can_use_tool`. Other server requests fail closed, including Codex permission-profile
grants (which grant turn/session access rather than single-operation approval).
This is an explicit coordinator-to-operator flow; it does not inject prompts into
the coordinator's native subagent approval UI.
