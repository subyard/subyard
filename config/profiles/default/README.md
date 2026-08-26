# `default` — reserved profile assets

This directory is reserved for future profile-wide assets. It is intentionally empty today.

The current runtime does not select, copy, or stage a default devcontainer from this path. Projects
that use VS Code Dev Containers must carry their own `.devcontainer/`. The OpenClaw profile contains
a [manual reference template](../openclaw/devcontainer/README.md), not an automatic fallback.
