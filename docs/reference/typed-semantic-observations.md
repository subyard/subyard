# Typed semantic observations

Accepted: 2026-10-11. Historical scope: the now-retired AI Observer integration pilot.
The generic canonicalizer remains supported; the active integration and its native producer
were removed during dashboard replacement. Historical protected fingerprints retain their
release recovery authority. This
contract keeps the existing planner, integration catalog and desired-state
authority. Source and artifact digests remain byte-exact.

## Producer and types

Python transports raw Incus/Docker JSON and retains bounded acquisition and
lifecycle effects. Go owns native schema validation, ownership checks, protected
projection, desired context and semantic fingerprints. Planning and apply use
the same constructor; retirement's second inspect uses the same container
constructor. Python does not parse or reserialize protected native JSON.

`internal/observation` provides closed immutable values, explicit ordered
sequences and unordered multisets, validated snapshots and semantic fingerprints.
Fingerprinting accepts only snapshots, never raw JSON, slices, byte digests or
advisory health. Constructors copy collections; returned canonical bytes are
copies. Raw decoded JSON requires an explicit array-semantics choice in the
owning producer. Types enforce these API boundaries, not projection completeness
or correct external semantics; runtime validation and focused tests cover those.

## Canonical bytes

Encoding is UTF-8 JSON without insignificant whitespace. Object keys sort by
UTF-8 byte order. Strings use Go JSON escaping with HTML escaping disabled:
quotes, backslashes and controls are escaped; `<`, `>` and `&` remain literal;
U+2028/U+2029 use Unicode escapes. There is no Unicode normalization. Malformed
UTF-8, unpaired surrogate escapes, duplicate decoded keys and trailing documents
are rejected before lossy decoding.

Numbers never pass through floating point. Exact decimal coefficients lose
leading/trailing zeros; removing trailing zeros adjusts the base-10 exponent.
A nonzero exponent uses lowercase `e`, without a plus sign or leading zeros.
Every zero encodes as `0`, including negative zero. This is a narrow JSON-domain
contract, not RFC 8785.

| Native JSON | Canonical bytes |
| --- | --- |
| `1`, `1.0`, `1e0` | `1` |
| `100`, `100.0` | `1e2` |
| `12.30` | `123e-1` |
| `-0.00` | `0` |
| `9007199254740993` | `9007199254740993` |
| `{"z":[],"a":null}` | `{"a":null,"z":[]}` |
| ordered `["b","a","a"]` | `["b","a","a"]` |
| multiset `["b","a","a"]` | `["a","a","b"]` |

Multisets sort complete canonical element bytes and retain duplicates. Sequences
preserve order, nulls and duplicates. Null, empty object, empty array and absent
properties are distinct. The legacy selected Config projection and State.Status
explicitly map missing fields to null; missing and null remain equivalent for
those fields. Input and canonical snapshots are bounded to 4 MiB,
nesting to 128, number tokens to 1,024 bytes, and both input and normalized
exponent magnitudes to 1,000,000. Exponents never expand into huge strings.

## Protected AI Observer fields

The constructor protects Id, Image, Config Image/User/Cmd/Env/Labels/Entrypoint/
WorkingDir/Volumes/ExposedPorts, complete HostConfig, complete Mounts, and State
Running/Status. Cmd, Entrypoint and Env remain ordered, as do arrays inside
HostConfig and unknown mount properties. Only the private field-specific
constructor chooses unordered Mounts semantics. Every property and duplicate
mount record remains protected.

Additional HostConfig/mount properties are accepted and protected: adding or
changing one between approval and apply is drift. Version-dependent mount
properties are not all mandatory; present known properties retain their native
types. Unknown Config/State fields remain outside the existing selected
projection. State Health is advisory. Actual also protects provision/proxy
receipts, the route device, managed service-control byte observations and
installation readiness.

Identity, managed labels, inherited receipt/device ownership and same-name
orphans still block before effects. Convergence is unchanged: matching
installation/context/publication returns current with Actual=Desired. Retired
controls may remain installed; true absence has empty digests. Core defers stopped
yards without starting them. The v1 NUL-separated desired-context recipe remains
byte-compatible with its predecessor.

## Delivery, recovery and diagnostics

Additive integration-native declarations reuse existing build mechanics with
separate profile/integration namespaces. Development uses `.build/agents/<id>/`;
amd64/arm64 bundles contain helpers inside their owning integration packages.
Plain source copies containing a validated `go.mod` need no Git metadata.
Installed/sealed helpers take precedence. Existing path guards reject symlink
traversal. Missing/invalid helpers fail closed; observation never builds them.

Actual helper executable bytes join package source binding, rechecked before
effects, and the sealed runtime byte manifest. Byte-exact source proofs have a
distinct type from semantic fingerprints. Fresh plans bind new Actual/Desired;
durable activation scope binds Desired. An Actual encoding change alone does not
prove scope change, while changed producer/package bytes affect source binding.
Unfinished updates retain their exact sealed owner, assets and authorization;
new code does not reinterpret old fingerprints or fix historical owner defects.
Rollback retains existing assessed compatibility and authorization guards.

Successful lifecycle stdout still has only state, actual and desired. A failing
handler may emit one bounded stderr record:

```text
subyard-lifecycle-diagnostic-v1 PHASE REASON
```

PHASE is inspect/apply/verify. REASON is native-unavailable, invalid-schema,
ambiguous-ownership, foreign-ownership, observation-changed, convergence-failed
or operation-failed. Core retains its calling phase for observations; apply
permits the source to distinguish effects from verification. Malformed,
duplicate, contradictory or historical missing records use a fixed generic
failure. Core adds validated yard/component context and a read-only integration
status command through ActivationDiagnostic. Arbitrary native output is private.
Existing stale/conflict classifications and original-failure recovery survive.
Public process, journal and grant schemas are unchanged; a SHA does not provide
a changed-group diff across processes.

## Acceptance and limits

Fast checks cover golden bytes, compiler-negative examples, immutable inputs,
bounded generated multiset invariants, exact numbers, additive native fields and
existing stale/ownership guards. CI fuzzing has a 1,000-execution budget. Cutover
also checks packaged helpers, affected actual released-owner continuation and
rollback, and targeted disposable-VM native acceptance. Exhaustive mount paths,
all-integration rewrites and full P0 on every edit are outside this pilot.
