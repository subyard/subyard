---
name: subyard-private-sync
description: "Manual only: run when the operator explicitly invokes it; never invoke on your own. Commit, rebase and push the private planning overlay in private/."
disable-model-invocation: true
---

# Private overlay sync

Run only on explicit operator invocation, never automatically after edits, at handoff
or before worktree removal.

`private/` is a separate Git repository without branches or pull requests: commit to its
default branch, rebase, push. Its `AGENTS.md` lists *decision* changes that need the
operator's approval; everything else you changed is *progress*.

1. `git -C private fetch`; report behind/ahead and `git -C private status --short`.
2. Classify each uncommitted path: **mine** (changed only by this session), **others'**
   (operator or another session), or **unclear** (including paths that were already dirty
   when you edited them). Split *mine* into progress and decisions.
3. If any decision, others' or unclear path exists, ask the operator once: each decision
   with one line on what it changes, then the others' and unclear paths. Commit your progress
   without waiting; defer rebase/push while any uncommitted paths remain.
4. Commit progress by explicit paths (`git -C private add -- PATH...`, never `-A`).
   Subject: the substance; body: affected tasks and public PRs. Commit approved decisions
   separately. Leave everything else uncommitted.
5. Recheck `git -C private status --porcelain --untracked-files=all`. If nonempty,
   stop sync after your commits and report the remaining paths; never stash, move or
   discard them to make sync possible. When clean, run
   `git -C private -c rebase.autoStash=false pull --rebase --no-autostash`, then
   `git -C private push`. On rebase conflict, abort and ask; on push rejection, stop
   and ask. Never `--force` or rewrite pushed history.
6. Report what was pushed, what awaits approval and what was left for others.

Before removing a worktree containing `private/`, have the operator invoke this skill
manually: `git worktree remove` deletes it silently, including unpushed commits.
