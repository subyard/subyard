# Historical owner receipt fixtures

`migration-owner-receipt.json` and `migration-owner-previous-receipt.json` are canonical,
synthetic receipts captured from `migrationOwnerFixtureWithPrevious` on source
`7d19ce117c9e2955e274dd979cbe2a1ba18bec38`, stopping after receipt publication.
They retain the historical registry and typed evidence bindings while using synthetic
release identities. They are not published installation records. Reader tests seed these
bytes and their bound evidence directly, independently of the retired replacement writer.

## Frozen v0.28 history

`v0.28.0-first-transition-verified.json` and
`v0.28.1-first-transition-verified.json` contain exact protected file bytes emitted
by the respective tag engines (`82f3bf42ff49cce5b4e4204d853bc91c15dc9d2e` and
`41e0b89cde25b3cd0d5f423444f99fc85a510081`). The synthetic empty installation
was interrupted at the sixth `after-ledger-verified` hook, before checkpoint
publication. All six journal steps are verified; the ledger is at 3/3/1/3.

`legacy-reader-complete.json` and `legacy-reader-owner-complete.json` were emitted
by the v0.28.0 engine with the original two-step settings/owner test registry,
with absent/present owner state respectively. `noncanonical-history-complete.json`
was emitted by its `TestSourceMetadataAcceptsAuthenticatedHistoricalDomainPrefix`
fixture. These retain the original reader assertions without executing retired
handlers in the current engine. Each JSON maps relative protected paths to their
exact UTF-8 contents; all identities and owner values are synthetic.

## Completed installation retention fixtures

`v0.28.1-completed-install-{plain,checkpointed}.json` and
`v0.29.0-completed-install-{plain,checkpointed}.json` contain complete protected
state produced by the unmodified tag engines, not just a synthesized ledger.
They include a Complete predecessor journal, all 18 Captured/Applied/Verified
records for the six historical ledger steps, the top-epoch ledger (3/3/1/3),
and a checkpoint only in the checkpointed variant.

Production sources came from `git archive` of these commits:

- `v0.28.1`: `58e3d8e2ec1cf187dbdf23d79d9ed9564b340a9e`;
- `v0.29.0`: `53f7bccbddea72417ee222b0b7843fabeb0800c9`.

To reproduce, extract each tag into its own disposable directory under `/tmp`,
copy `generate-completed-install.go.txt` to
`internal/releasetransition/completed_install_export_test.go` there, and run:

```sh
TMPDIR=/tmp GOFLAGS=-p=2 FIXTURE_VERSION=0.28.1 FIXTURE_OUTPUT=/tmp/completed-install-output \
  go test -p 2 -count=1 ./internal/releasetransition -run '^TestExportCompletedInstallation$' -v
```

Create `/tmp/completed-install-output` beforehand; use `FIXTURE_VERSION=0.29.0`
for the other tag. The harness runs each first transition to Ready with no
warnings, once with `MigrationCheckpoint=false` and once with it enabled, then
exports the exact file contents without editing them. The only added tag file
is the test harness; no production source or handler is patched. These are
host-free empty-installation fixtures: release identities, artifact digest and
transaction id are synthetic; no native VM or operator data is involved.

The cleanup tests seed a separate unreferenced closed transaction from
`legacy-reader-complete.json`, including its typed recovery file. It must be
collected; the actual tag predecessor remains retained by its runtime or pinned
projection reference. This distinguishes successful cleanup from accidentally
deleting required history.

SHA256 of the exported JSON containers:

- `v0.28.1-completed-install-checkpointed.json`: `815f64ae682450fb4d27df5d6b8fc705cdb47b01df1bd7ecc997626378c92eb9`;
- `v0.28.1-completed-install-plain.json`: `025a05c05aa0be56715ce6e8cea51247b91bb6bf97c5291e9126571bde992499`;
- `v0.29.0-completed-install-checkpointed.json`: `cbeaa8d51eb1754fa358d55950f752299230abd54f161169c479f98d012ee3d0`;
- `v0.29.0-completed-install-plain.json`: `2058715665abb64a8813e7a02602ca9f8bd58b8c105929c722ef4815097f1f38`;
