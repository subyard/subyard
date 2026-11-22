# Historical owner receipt fixtures

`migration-owner-receipt.json` and `migration-owner-previous-receipt.json` are canonical,
synthetic receipts captured from `migrationOwnerFixtureWithPrevious` on source
`7d19ce117c9e2955e274dd979cbe2a1ba18bec38`, stopping after receipt publication.
They retain the historical registry and typed evidence bindings while using synthetic
release identities. They are not published installation records. Reader tests seed these
bytes and their bound evidence directly, independently of the retired replacement writer.

`v0.30.0-release-transition.json` is the exact published registry (SHA-256
`3ee060450263c840853ecb639200a6e4a02b52c7a70e7e8c5d35eade99091de2`). The production
reader embeds these frozen bytes to bound its additional retirement language independently
of future live registries. `v0.30.0-readable-retirements.txt` enumerates its complete accepted
set: each row lists owner-registration, power-metadata and settings floors, whether the
definition-free project-state prefix is declared, and the accepting rule. The 25 rows cover
5 strictly readable and 20 additional dependency-closed retirements out of 71 candidates.
