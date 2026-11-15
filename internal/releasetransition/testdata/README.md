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
