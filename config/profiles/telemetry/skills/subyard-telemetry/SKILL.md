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

The active database is `$data/telemetry.sqlite`. The first spool upgrade
archives the previous store under `$data/archive`. If that name is occupied,
the archive uses `$data/archive-2`, `$data/archive-3`, and so on. Each sealed
archive keeps its schema, identifiers, export sequences and retained
telemetry. Use `stats` to identify its schema version and store generation.
The active store starts a fresh sequence.

```sh
for archive in "$data"/archive "$data"/archive-[0-9]*; do
  [ -f "$archive/telemetry.sqlite" ] || continue
  printf '%s\n' "$archive"
  "$collector" stats --data-dir "$archive"
done

archive="$data/archive"
"$collector" query --data-dir "$archive" --sql \
  'SELECT kind,count(*) AS records FROM records GROUP BY kind ORDER BY kind'
"$collector" show-run --data-dir "$archive" --run STORED_RUN_ID
"$collector" export --data-dir "$archive" --after 0 --limit 1000
```

Continue export with the last row's `sequence` as `--after` and keep separate
cursors for every archive and the active store. Use stored run IDs from the
archive's `runs` table for `show-run`. An `interrupted_by_upgrade` gap marks a
historical run with no final sample. Pending legacy outbox records and
health markers stay in retired storage. Archive exports use the previous
release's capture rules. Active exports include raw analytics identifiers
and labels, so keep exports private.
