# Frozen historical Orca observation inputs

`identities.json` binds the exact handler and registration input hashes from
public releases 0.17.3, 0.18.1, 0.18.6 and 0.19.0. All four send the same
`observer.txt`. The corresponding `contract-*.txt` files freeze their host-side
digest renderers; 0.18.1 and 0.18.6 share one renderer. The released-caller
compatibility corpus runs the actual published producers against this fixture.
