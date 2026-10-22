# Migration history comparison

This dev-only crate evaluates the pure history seam in
[`internal/migrationhistory`](../../internal/migrationhistory). It is an experiment
inside the future [whole release owner](../../docs/reference/rust-release-owner-design.md),
with no runtime caller, storage, Incus client, activation or authorization.
Go remains the production owner. The adapter now re-exports the single history
implementation from the [production foundation](../../release-owner/README.md);
it does not maintain another Rust history model.

Build the comparison executable with the root stable Rust toolchain using the
[shared Cargo cache workflow](../../docs/development.md#toolchain), keeping final
artifacts under `.build`. Then run the opt-in comparison with Go from `go.mod`:

```sh
SUBYARD_MIGRATIONHISTORY_RUST=/absolute/path/to/artifact TMPDIR=/tmp \
  go test -race -count=1 ./internal/migrationhistory -run '^TestRustPilotCorpus$'
```

Plain Go package tests check the independent oracle against Go without Rust.

The Go test generates all completed subsets of five ordered three-migration
registries, with and without cross-domain dependency, and every ID sequence of
length zero through three over the migration IDs and an unknown ID. This includes
all historical cuts, non-global domain prefixes, illegal cuts, duplicate IDs,
and noncanonical future suffixes. The existing independent bitset oracle supplies
the expected frontiers and dependency closure; the trace oracle walks those same
sets. Both implementations must match those expectations. Registry, frontier,
projection and retired-pin/registry-binding mutations add explicit invalid cases.
There is no maintained Rust oracle or expected JSON fixture.

Canonical comparison normalizes JSON object ordering and empty arrays at this dev
adapter boundary. It establishes semantic equality on the generated corpus, not
compatibility of persisted product bytes. The adapter accepts one tagged JSON
request, bounded to 1 MiB, and emits one response. Unknown fields/tags, malformed,
trailing, oversized and out-of-range numeric documents fail with nonzero exit and
no result. It has no path, shell or effect operation. Its DTO schema is not a
frozen production protocol.

The adapter preflights the complete DTO before constructing checked registries or
semantic result trees. Limits are 10,000 cases, 1–10,000 iterations, 2,000,000,000
aggregate estimated work units, 4 MiB estimated serialized results, and 100,000
aggregate retained plus peak live frontier/ID cells. Saturating arithmetic rejects
estimates that exceed a limit. The result estimate includes pending/next fields,
two snapshots and edge IDs per trace, six bytes per input string byte for JSON
escaping, and fixed field allowances. Cell estimates also include the longest
trace's retained semantic edges and their cloned migration/dependency IDs. Work estimates include registry strings and
entries, dependency IDs, the domain-by-migration history scans, snapshot copying,
trace rewind/advance steps, both binding registries, and every requested iteration
plus the final response evaluation. These conservative admission estimates bound
expansion; they are not exact operation, allocator/RSS or wall-clock accounting.
Budget failures emit no result. The complete comparison corpus fits these limits;
future larger corpora must split requests without dropping cases.

Checked values have private constructors/contents and no `Deserialize` implementation.
Untrusted inputs can be copied, but must pass constructors to become checked values.
Registry identity is checked separately from complete semantic registry equality,
including retired pins. Completion says only that one-time history is complete.
The borrowed checked map cannot be mutated by callers; this is a concrete compiler
guarantee beyond Go's existing detached defensive copies. Authority snapshots may
legitimately branch. `Edge::into_after` consumes a non-Clone local semantic step;
reconstructing the same step from an immutable snapshot is allowed. Neither move
semantics nor registry identity prevents durable replay or authorizes external effects.

The library's standard compile-fail doctests cover borrowed mutation, moved steps,
projection/authority confusion, field forging and direct checked deserialization,
with runnable positive examples. Rustdoc checks failure; the error-code annotations
document the intended compiler category. Runtime validation and owning tests remain
necessary. No fake activation `Ready` or process authority is introduced.

The test reports repeated pure evaluation time for the identical generated batch,
including registry validation, history/trace evaluation and result construction,
excluding admission preflight, JSON encoding/decoding and process startup. Timing from a race-instrumented run establishes no performance comparison. These small allocations/workloads do not measure release latency, RSS,
IPC, Incus, recovery or production speed. No production improvement is claimed.

These API choices follow Rust's [type safety guidance](https://rust-lang.github.io/api-guidelines/type-safety.html)
and [state transition example](https://doc.rust-lang.org/book/ch18-03-oo-design-patterns.html#encoding-states-and-behavior-as-types).
The codec uses Serde's [unknown-field checks](https://serde.rs/container-attrs.html)
and [internally tagged enums](https://serde.rs/enum-representations.html).
Future writer work still requires owner custody, surviving native-work quiescence,
old-writer fencing, intent/consent/effect verification and crash-resume acceptance.
