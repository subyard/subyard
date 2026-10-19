# Release owner measurements

`dev/measure-release-owner.py` imports the existing release upgrade fixture and
Veranda process-tree sampler. It runs ordinary installed package launchers. Setup,
the synthetic Incus observation server and Python observers are outside the measured
tree. Disposable state lives under `/tmp`; retained evidence belongs under `.build`.
Use the script's `--help` for package, predecessor and prebuilt observation-server
arguments. A new output directory is required, to preserve previous results.

The three independent workloads are `nominal-owner-inspection`,
`checkpoint-history-recovery` and `legacy-history-recovery`. Nominal configuration
startup is recorded separately and excluded from owner latency. History workloads
reuse authentic released predecessor migrations and the existing controlled apply
failure, then measure a fresh packaged inspection, resume, and ready inspection.
Their sum of command wall/CPU times is reported; RSS is the largest simultaneous
sample within any command. Setup and time between commands are excluded. Full
stdout, stderr and binding-bearing journal meanings remain available; the semantic
comparison selects public outcomes, verified migration states and retained data.
Transaction IDs, timestamps, version identities, recipe facts and plan fingerprints
are deliberately excluded from cross-implementation equality, never rewritten.

The plan, package/source hashes, seed and randomized arm order are written before
results. Each arm retains its controlled fixture path and restores its hash-bound
baseline outside timed commands before every sample. `--fixture-lifecycle fresh`
explicitly selects independently constructed fixtures instead. Both lifecycles
require actual Rust-owner census, including during `--preflight`. The requirement is at
least 100 pairs **for each** workload. A fixed 10,000 paired bootstrap resamples
estimate two-sided 95% intervals for Rust/Go p95 wall and total user+system CPU.
The upper endpoint must be at most 1.10. An interval crossing the limit remains
INCONCLUSIVE. RSS must be within Go + max(10% Go, 8 MiB). Fewer pairs are preflight
evidence only. Neither a preflight nor an instrumented run establishes acceptance.

The isolated collector is a Linux child subreaper. It uses `wait4(-1)` to reap the
product and adopted orphans once each; each rusage already includes the descendants
that process waited for. It never sums sampled CPU or mixes a server child's usage
into the product. The process tree is drained even after the product exits; a
timeout invalidates CPU completeness and kills identity-bound owned descendants.
This follows [getrusage](https://man7.org/linux/man-pages/man2/getrusage.2.html) and
[wait4](https://man7.org/linux/man-pages/man2/wait4.2.html). `ru_maxrss` is never used
as simultaneous tree RSS. Run `python3 dev/release-owner-measure/test_measure.py`
for waited-child, detached orphan, double-fork and timeout controls.

RSS sampling uses PID/starttime identities and includes observed helpers in new
process groups and adopted orphans. The requested interval and actual maximum gap
are recorded. Helpers born and reaped between samples can be invisible, with no
finite memory bound; a zero count of unsampled adopted children does **not** prove
complete helper coverage. Wall completion also has sampling-loop observation delay.
Consequently this collector reports sampled RSS as a lower bound and keeps the
overall result INCONCLUSIVE until a separate trustworthy helper/RSS coverage witness
and physical native-I/O scenario exist. Synthetic fixture I/O is never physical
native-I/O acceptance. Fresh processes with warm/uncontrolled OS page caches are
never called cold-cache runs. This harness never drops host caches or uses sudo.
Root identity is seeded before exec without measuring the forked Python scaffold.
A CLOEXEC pipe permits RSS sampling only after the product exec boundary; product
wall and wait4 CPU still include startup. A tiny native sleep control checks that
its peak excludes the inherited collector footprint. Earlier preflights preceding
this handshake retain their raw receipts and have contaminated RSS, not valid
product memory measurements.

Allocation instrumentation is a separate experiment. `instrument.py` accepts an
isolated extracted source snapshot and inserts developer-only Go runtime statistics
and a Rust System allocator wrapper, preserving original and changed hashes in a
receipt. Package that snapshot using its existing packager, then use
`--instrumented`. The generator refuses the active checkout. Instrumented package
timings are not normal product latency. Statistics cover only processes that emit
their final report; helpers terminated with SIGKILL cannot do so, making whole-tree
allocation/GC coverage INCONCLUSIVE. No production debug API or allocator is added.

`reset-preflight.py` separately prepares each authentic history once and attempts
two exact-path cycles. It preserves original directories and restores bytes, modes,
owners and mtimes, records changed file identities, and requires fresh public
read-only inspection, durable-grant resume, ready inspection and equal public
outcomes. A replaced directory refuses reset. Changed file IDs are exposed because
source inventories and settings recipes bind IDs; byte restoration cannot promise
the original full readset. Journal CAS itself uses byte revisions. A Go-routed
reset pass proves only Go fixture plumbing. Rust reuse needs the final selected
Rust package and its own binding validation. The paired runner uses this reset
lifecycle by default, checks repeat outcomes within each arm, and rejects missing
Rust process evidence. `--supporting-copy-receipt` records temporary routing-copy
provenance and requires preflight mode; it cannot establish final product acceptance.

`--nominal-cycle` is a separate preflight-only diagnostic variant: inspect,
execute local migrations, then inspect readiness in a fresh process. It does not
change the declared nominal inspection workload. Per-process sampled CPU ticks
and observed lifetimes are diagnostic lower bounds; full drained `wait4` CPU
remains the sole CPU comparison metric. A public semantic refusal stops dependent
commands immediately and retains protected fixture evidence. Rust history setup
requires original bundle/native-record existence before fresh public validation;
the harness never manufactures or repairs private records.
