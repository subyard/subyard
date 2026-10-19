# Native release owner experiment

This is a dev-only vertical experiment for the
[whole release owner](../../docs/reference/rust-release-owner-design.md).
Rust owns one prepared session, consent consumption, the exact durable intent,
lock custody, native ordering, verification and fresh-process recovery. Go owns
the existing config loader, assignment editing and protected local settings CAS.
There is no production caller, Go `Converge` wrapper, Incus operation or JournalV2
compatibility claim. Existing Go journals and release entrypoints are unchanged.

The closed component pins the effective persistent Git `SSH_PORT` of one eligible
local yard. It retains duplicate-port, precedence, local assignment and canonical/
flat ambiguity checks. The target parent must already exist. Production's wider
missing-parent, multi-yard and overlapping migration grammar remains future work.
Effects require a private direct `/tmp/subyard-owner-*` root with the exact
`owner.marker` fixture marker. Source/cache and unrelated local records are retained.
This scope does not fence production or arbitrary same-UID writers.

`make verify-release-owner-pilot` runs the owning Go race packages, Rust unit and
compiler contracts, strict codec golden, Clippy and independent-process fault cuts.
Logs belong in `.build/release-owner-pilot`; default final Cargo artifacts and the
native Go helper share one invocation-owned `target/run-*` directory beneath it.
Ordinary local/preflight modes inherit developer Cargo/Rustup homes and use the
[shared Cargo intermediate cache](../../docs/development.md#toolchain), without
selecting historical task toolchains or private caches. Explicit `CARGO_TARGET_DIR`
overrides remain caller-owned and require serialization if shared. Snapshots default
to `CARGO_INCREMENTAL=0`. Fixtures always use `/tmp`.
`bash dev/release-owner-pilot/check.sh --preflight` builds and checks the same bounded
artifacts without executing process faults. After allocating a disposable VM, run
`bash dev/release-owner-pilot/check.sh --vm` in its transported current source tree.
The VM entrypoint builds locked artifacts and runs the artifact preflight and real
process matrix; it does not repeat the local logical/unit/race/compiler matrix.
It may install the official checksum-verified minimal stable Rust toolchain under
its ignored task directory when missing and reuse that VM-owned installation on a
later invocation (installer timeout: ten minutes; bounded
curl downloads); normal local/preflight modes never install it. No operator config, Incus, credentials or release bundle is required.

The v1 inner protocol is canonical sorted-key JSON with integer precision, strict
fields/operations and a 1 MiB request/response cap. One inherited root FD and one
verified open native artifact are passed to each scoped helper. The private channel
carries fingerprints, identities and semantic pin, not arbitrary paths or commands.
The shared golden belongs to this dev protocol; it is not frozen product bytes.
A recipe captures at most 2,048 entries and 16 MiB of source bytes, with 128-byte safe
ASCII names, 512-byte relative paths and at most 16 separators. Planning copies
that authenticated FD snapshot into private `.owner/planning-*` scratch via `os.Root`,
then runs the unchanged loader there. All actual source/target revalidation and CAS
traversal use the original pinned root FD under its exclusive flock. Source closure
is rechecked before returning the recipe and immediately before publication. Source
enumeration excludes only the owner directory, exact target and exact CAS pending
name. Existing wrappers preserve their original path/open/flock behavior.

Prepared values are private, non-deserializable local handles. Consent binds owner,
helper, transaction, yard, recipe and five-minute expiry before consumption. The
root lock is released while waiting for consent, then reacquired and the complete
recipe reobserved before durable intent. Canonical private records contain bindings,
typed before-absent, intent/verified phase and semantic pin; raw configuration is
never persisted or printed. Record updates use protected openat, exclusive pending
creation, fsync, atomic rename and directory fsync. Corrupt, symlink or unsafe-mode
state refuses effects. Unknown pending storage bytes require refusal, not guessing.

Fault cuts use a separate bounded private control descriptor: the helper reports
its exact phase/PID, then waits for one release byte. SIGSTOP timing is not used.
The control descriptor is absent from normal native calls; the five-second drain
deadline remains in force.

The native helper borrows the same open-file description and never calls `LOCK_UN`.
The live owner drains and reaps it before releasing custody, including timeouts.
After owner death a surviving helper retains the lock until it exits; restart must
acquire that lock before observing. Expected permits the exact CAS, desired permits
verification without replay, and conflict refuses. Durable intent carries the original
consent through recovery; a new session cannot replace it. Advisory flock serializes
cooperating writers, not arbitrary external writes, bind mounts or malicious same-UID
mutation. These file-CAS results prove no asynchronous Incus quiescence.

| Format | Reader/writer and continuation |
| --- | --- |
| Existing ProcessV1, JournalV2, checkpoint and schema-2 contracts | Exact Go owners remain unchanged; the pilot neither reads nor imports these records |
| Private pilot record v1 | Rust only; continuation requires the exact owner/helper artifacts and scope; Go never reads the store; unknown versions or mismatches refuse effects |
| Future production Rust records/protocol | Unsupported and not enabled by this fixture |

Fixture rollback/cleanup requires draining all owned processes first. It removes only
the disposable fixture; it never rewrites a journal or repairs production manually.

Rust uses [rustix safe openat](https://docs.rs/rustix/1.1.5/rustix/fs/fn.openat.html)
and [flock](https://docs.rs/rustix/1.1.5/rustix/fs/fn.flock.html) for rooted storage and
custody, and [sha2](https://docs.rs/sha2/0.11.1/sha2/) because std has no SHA-256.
Both Rust library and binary forbid unsafe code. The staging uses
[Go Root](https://pkg.go.dev/os#Root) only after private authenticated creation;
its own API does not supply nofollow/type/mode validation of the original source.
Production requires the complete release grammar, verified delivery/facade, retained
owner compatibility, exact old-writer admission/custody and same-candidate VM gates.
