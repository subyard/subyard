# Historical owner receipt fixtures

`migration-owner-receipt.json` and `migration-owner-previous-receipt.json` are canonical,
synthetic receipts captured from `migrationOwnerFixtureWithPrevious` on source
`7d19ce117c9e2955e274dd979cbe2a1ba18bec38`, stopping after receipt publication.
They retain the historical registry and typed evidence bindings while using synthetic
release identities. They are not published installation records. Reader tests seed these
bytes and their bound evidence directly, independently of the retired replacement writer.
