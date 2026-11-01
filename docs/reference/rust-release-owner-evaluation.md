---
title: Rust release-owner evaluation result
status: evaluation stopped; continuation judged not worthwhile
updated: 2026-11-01
---

# Rust release-owner evaluation result

The [Rust release-transition owner](rust-release-owner-design.md) (boundary B: one Rust session
owns plan, consent, locks, journal, effect ordering and recovery; a scoped Go helper executes
component work and the Incus SDK) was evaluated against the strengthened Go owner. The
evaluation is stopped. Go remains the only production release owner, and the Rust owner will
be removed from `master`.

## What was shown

- A Rust owner can drive complete release transitions, including same-transaction resume,
  checkpoint recovery, rollback and Rust-only authoring of a new settings migration, without
  adding Go migration kinds.
- Host-free functional suites, delivery checks and same-session tamper controls passed on
  evaluation candidates. Physical, native ARM and installed-graph gates were never run.

## Cost

Paired host-free measurements on the same source, Go owner versus Rust owner with its Go helper:

| Workload | Go wall | Rust wall | Rust/Go |
| --- | ---: | ---: | ---: |
| Nominal owner inspection (`yard migrate --check`) | ≈0.33 s | ≈1.0 s | ≈3.0× |
| Checkpoint history recovery, first comparison | ≈51 s | ≈79 s | ≈1.6× |
| Checkpoint history recovery, final candidate after optimisation | ≈9.5 s | ≈33.8 s | ≈3.6× |

The Go baseline improved between the first and final comparison; the ratios are not a single
series. Sampled peak RSS in recovery was several times higher for the Rust path, dominated by
the native helper. Nine optimisation rounds removed duplicate reads and copies; only one
produced a material gain, and an unsafe diagnostic cache still left the Rust path ≈2.4× slower.

The gap is structural. In boundary B, Rust adds an orchestration layer, inter-process requests,
serialisation and repeated fresh validation on top of Go component work that still runs.
Removing the Go owner would have deleted far less code than the evaluation added
(≈48k lines of work in progress, ≈23k of them Go production code).

## Decision

The acceptance criteria for continuing were: affected operations (update, migrate, recovery
and init continuation) no more than 20% slower end to end, and the Rust-owned part no more than
3× slower than the Go equivalent. The final candidate already exceeds the second bound for
recovery, and the structure leaves no realistic path to the first for short operations.

A review of recent migration and upgrade fixes also found that most failures came from
non-canonical observations of external state, the number of supported source releases,
imperative profile scripts, process and IPC races, and synthetic fixtures that drift from real
releases. A different owner language addresses none of these, and boundary B adds processes
and IPC. Continuing is therefore not worthwhile.

## What happens next

- The evaluation work in progress is preserved on the `rust_migration` branch for reference.
  It is not maintained against `master`.
- The Rust owner, its packaging, opt-in routing and workflows will be removed from `master`.
  Historical readers and retained exact owners that protect released installations stay.
- Language-neutral fixes found during the evaluation are moved to `master` separately, with
  their own tests.
- Reliability work continues in Go: closed state types with exhaustive switch checks,
  single-use authorisation, effects bound to journal steps, deterministic plans, crash
  injection at every journal step, upgrades from real released artifacts, and migrations as
  idempotent convergence to the target state rather than history replay.
