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

`published-rollback-baseline.json` pins profile runtime declarations, the registry-reader flags,
and a SHA-256 inventory of tracked integration asset sources from the latest published release
in `dev/e2e/published-upgrade-baselines.tsv`. Its version and bundle checksum must match that
source of truth. `python3 dev/update-rollback-baseline.py` regenerates it and the exact registry
fixture from the local published tag. The agent inventory hashes sorted relative paths,
the Git executable flag (owner execute, independent of group/other permissions) and file
SHA-256 values. The updater and gate share `internal/testkit/integrationassets`: the existing
integration package/materialization inventory plus shipped defaults, the usage provisioner and
project dispatcher. All declared artifact variants are included, even below `tests/`; unrelated
files are excluded. The gate reads tracked filesystem contents, including uncommitted edits,
and ignores untracked scratch files. Profile runtimes come from `profile.Load` on the same tracked mirror, including package
validation and native discovery rules. The baseline also pins the target predecessor declaration
needed for directional registry admission.
