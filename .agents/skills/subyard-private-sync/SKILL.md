---
name: subyard-private-sync
description: Commit, rebase and push the private planning overlay in private/ when it exists. Use after editing private task files, before the final answer, before removing a worktree, and when asked to sync the private repo.
---

# Private overlay sync

`private/` is a separate Git repository without branches or pull requests: commit to its
default branch, rebase, push. Its `AGENTS.md` lists *decision* changes that need the
operator's approval; everything else you changed is *progress*.

1. `git -C private fetch`; report behind/ahead and `git -C private status --short`.
2. Classify each uncommitted path: **mine** (changed only by this session), **others'**
   (operator or another session), or **unclear** (including paths that were already dirty
   when you edited them). Split *mine* into progress and decisions.
3. If any decision, others' or unclear path exists, ask the operator once: each decision
   with one line on what it changes, then the others' and unclear paths. Sync progress
   without waiting; run this skill again after the answer.
4. Commit progress by explicit paths (`git -C private add -- PATH...`, never `-A`).
   Subject: the substance; body: affected tasks and public PRs. Commit approved decisions
   separately. Leave everything else uncommitted.
5. `git -C private pull --rebase --autostash`, then `git -C private push`. On a conflict
   (including rename/rename) or a rejected push, abort the rebase and ask. Never `--force`
   or rewrite pushed history.
6. Report what was pushed, what awaits approval and what was left for others.

Run this skill before removing a worktree that contains `private/`: `git worktree remove`
deletes it silently, including unpushed commits.
