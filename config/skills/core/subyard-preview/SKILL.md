---
name: subyard-preview
description: Share a clickable web preview of a static website, HTML page, or built frontend with the operator from inside a Subyard yard.
---

Run `subyard-preview <relative-static-dir>` from the Git workspace. Keep the helper
running with your background or async process mechanism, and share its printed URL.
For a frontend build, pass the build output directory after building it.

Check availability with `command -v subyard-preview` and read `subyard-preview --help`
for options. A missing helper or failed probe means availability is unknown; report
the error before attempting a preview.

Owner URLs require network reachability, and Tailnet policy access for Tailscale.
A loopback URL is local to the yard. Provide the operator-machine tunnel recipe from
`subyard-preview --help` only when the operator requests it. `yard code` does not
forward preview ports. The helper must stay running for the URL to work.

Yard lifecycle and owner networking belong to the operator. Use the installed helper
within the yard's existing permissions.
