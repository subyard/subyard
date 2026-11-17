---
name: subyard-preview
description: Share a clickable web preview of a static website, HTML page, or built frontend with the operator from inside a Subyard yard.
---

Run `subyard-preview <relative-static-dir>` from the Git workspace; keep it running
and share its printed URL. See `subyard-preview --help` for options and tunnels.
Give the operator-machine tunnel recipe only when the operator asks for it.

Owner URLs require network reachability and, for Tailscale, Tailnet policy access.
Loopback URLs are yard-local; `yard code` does not forward preview ports.
A missing helper or failed probe leaves availability unknown. Yard lifecycle and
owner networking belong to the operator.
