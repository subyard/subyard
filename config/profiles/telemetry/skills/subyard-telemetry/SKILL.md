---
name: subyard-telemetry
description: Inspect or export local Subyard telemetry for managed agent runs, native signals, and resource measurements. Use for current telemetry and read-only pre-upgrade telemetry archives.
---

# Subyard telemetry

Run inside the yard as the developer account that owns the data.

```sh
collector=/usr/local/lib/subyard-telemetry/subyard-telemetry
data=/var/lib/subyard/telemetry
"$collector" stats --data-dir "$data"
"$collector" show-run --data-dir "$data" --run RUN_ID
```

The active database is `$data/telemetry.sqlite`. After a schema 4 upgrade,
`$data/archive/telemetry.sqlite` holds the previously retained records and
projections, including run and session metadata, resource measurements, and
native OTLP signals. It keeps the old schema, identifiers and export sequences.
The active store starts a new sequence.

```sh
"$collector" stats --data-dir "$data/archive"
"$collector" query --data-dir "$data/archive" --sql \
  'SELECT kind,count(*) AS records FROM records GROUP BY kind ORDER BY kind'
"$collector" export --data-dir "$data/archive" --after 0 --limit 1000
```

Continue export with the last row's `sequence` as `--after` and keep separate
cursors for the archive and the active store. Use stored run IDs from the
archive's `runs` table for `show-run`. Keep exports private because archive
records retain the capture rules of the previous release.
