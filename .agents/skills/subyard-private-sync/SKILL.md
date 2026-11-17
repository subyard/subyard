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

Run this skill before removing a worktree that contains `private/`: `git worktree remove`
deletes it silently, including unpushed commits.
