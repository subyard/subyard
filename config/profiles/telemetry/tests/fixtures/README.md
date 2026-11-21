# Retained telemetry writer

`v0.30.0-native.py.tar.gz` contains the unmodified telemetry writer from Subyard
v0.30.0 (commit `e2b31943dac13e05837e1b392416d023911abfd0`). The native test runs
it to create the receipt and home documents consumed by the one-way migration.
`v0.30.0-guest.py.tar.gz` contains that release's unmodified guest runtime. The
rollback test runs it, with the writer, against the state that `rollback-apply`
prepares.

```sh
git archive --format=tar.gz -o config/profiles/telemetry/tests/fixtures/v0.30.0-native.py.tar.gz v0.30.0 \
  config/profiles/telemetry/native.py
git archive --format=tar.gz -o config/profiles/telemetry/tests/fixtures/v0.30.0-guest.py.tar.gz v0.30.0 \
  config/profiles/telemetry/guest.py
```
