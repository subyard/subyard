# Frozen systemd contracts

The v0.19.0 files below are byte-for-byte copies from public tag `v0.19.0`,
commit `2a7b6da7400b28b44f3326bffddb5c65b14ea365`. They are historical
inputs, independent of current templates. Do not regenerate them from current policy.

| Fixture | Original path | SHA-256 |
| --- | --- | --- |
| `subyard-power-reconcile.service-v0.19.0.in` | `config/systemd/subyard-power-reconcile.service.in` | `64f8489b10dc10fb63dc67b858c2cde6d39ed447c6752a50000981daa3afaf21` |
| `subyard-keys-sync.timer-v0.19.0.in` | `config/systemd/subyard-keys-sync.timer.in` | `b271fe9c185f508d1b388182ce64bd8def7628b92a0d7164a0a919d690e75137` |

`subyard-keys-sync-v0.21.2.service.in` retains the public pre-refresh keys
service contract from tag `v0.21.2` (`d7c08a2d5f9f752e81aa01080cc44c5138fd9d9e`).
The production historical producer allowlist is the independent hash oracle.
