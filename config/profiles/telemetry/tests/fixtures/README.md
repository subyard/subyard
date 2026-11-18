# Retained telemetry writer

`v0.30.0-native.py.tar.gz` contains the unmodified telemetry writer from Subyard
v0.30.0 (commit `e2b31943dac13e05837e1b392416d023911abfd0`). The native test runs
it to create the receipt and home documents consumed by the one-way migration.

```sh
git archive --format=tar.gz -o config/profiles/telemetry/tests/fixtures/v0.30.0-native.py.tar.gz v0.30.0 \
  config/profiles/telemetry/native.py
```
