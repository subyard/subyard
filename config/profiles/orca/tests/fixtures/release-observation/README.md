# Frozen historical Orca observation inputs

`identities.json` binds the exact handler and registration input hashes from
public releases 0.17.3, 0.18.1, 0.18.6 and 0.19.0. All four send the same
`observer.txt`. The corresponding `contract-*.txt` files freeze their host-side
digest renderers; 0.18.1 and 0.18.6 share one renderer. The released-caller
compatibility corpus runs the actual published producers against this fixture.

Release 0.22.0 adds Codex profile and launcher paths. Its separate
`contract-0.22.0.txt` and `observer-0.22.0.txt` preserve the exact digest renderer
functions and observer heredoc from its released handler. Historical fallback
requires both split contract and observer files to be absent.

Releases 0.17.0–0.17.1 share the 0.17.3 renderer; 0.20.0 and 0.21.0 share the
0.19.0 renderer. Releases 0.21.2–0.21.3 share the 0.22.0 renderer with their own
observer. Release 0.23.0 shares the 0.22.0 renderer and observer with different
registration inputs. Exact identities also cover intervening patch releases
that have identical handler and registration bytes. Releases 0.24.0–0.25.1 use
the current split files; both must match the trusted source bytes.
