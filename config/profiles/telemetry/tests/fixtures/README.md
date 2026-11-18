# Retained agent writers

`v0.30.0-agent-writers.tar.gz` contains the unmodified telemetry and template
writers from Subyard v0.30.0 (commit `e2b31943dac13e05837e1b392416d023911abfd0`).
The native tests execute both released writers to exercise upgrade and rollback
without fetching history in shallow or offline test checkouts.

```sh
git archive --format=tar.gz -o config/profiles/telemetry/tests/fixtures/v0.30.0-agent-writers.tar.gz v0.30.0 \
  config/profiles/telemetry/native.py internal/adapters/configmaterial/guest_json.py
```
