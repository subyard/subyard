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
archives the previous store under `$data/archive`. Subsequent legacy stores are
archived under `$data/archive-2`, `$data/archive-3`, and so on. Each archive
keeps its old schema, identifiers, export sequences and retained telemetry.
Each new active store starts a fresh sequence.

```sh
for archive in "$data"/archive "$data"/archive-[0-9]*; do
  [ -f "$archive/telemetry.sqlite" ] || continue
  printf '%s\n' "$archive"
  "$collector" stats --data-dir "$archive"
done

archive="$data/archive"
"$collector" query --data-dir "$archive" --sql \
  'SELECT kind,count(*) AS records FROM records GROUP BY kind ORDER BY kind'
"$collector" export --data-dir "$archive" --after 0 --limit 1000
```

Continue export with the last row's `sequence` as `--after` and keep separate
cursors for every archive and the active store. Use stored run IDs from the
archive's `runs` table for `show-run`. Keep exports private because archive
records retain the capture rules of the previous release.
