# Subyard Veranda

Veranda is Subyard's Tauri 2 desktop client. The current read-only slice starts a fixed local
`yard rpc --stdio` child, negotiates Yard RPC v1, requires `owner-inventory-v1`, and displays the
owner host, its local yards, and the registered project names for the selected yard.

The frontend has no shell or filesystem plugin. Rust validates the bounded owner inventory and
returns a narrow IPC DTO; host paths, SSH details, source keys, credentials, and raw RPC payloads do
not cross into TypeScript. Remote connections, mutations, project launch actions, and profiles are
not part of this slice.

Node.js and npm are development/build tools only: they compile and test the Svelte frontend. They do
not run inside, or ship with, the installed desktop application.

## Develop

Install the [Tauri 2 prerequisites](https://v2.tauri.app/start/prerequisites/) for your operating
system, including Rust 1.88 or newer, then run:

```sh
cd veranda
npm ci
npm test
npm run check
npm run build
npm run tauri dev
```

The committed npm policy keeps dependency lifecycle scripts disabled; this slice does not require
any of them.

As of 2026-08-12, `npm audit` reports the low-severity `GHSA-pxg6-pf52-xh8x` advisory through
SvelteKit's build-only `cookie@0.6.0`. Veranda disables SSR, emits a static frontend, and neither
sets nor accepts cookies, so the affected API is not reachable in the shipped application. Review
this deferral with the next SvelteKit update, no later than 2026-09-12.

`yard` must be on `PATH` for the desktop application. From a source checkout, build it with
`make build` at the repository root and prepend the repository's `bin/` directory when starting
Tauri.

For browser-only layout inspection, `npm run dev:fixture` enables the checked-in non-production
fleet fixture. Normal development and production builds always invoke the native command.

The product and interaction contract lives in [`../docs/veranda/README.md`](../docs/veranda/README.md).
