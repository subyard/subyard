# Integration consumer compatibility

Artifact declarations own config and instruction delivery. These existing
consumers have separate policies; new integrations need no common-code branches.

Package `persist` metadata projects into `AGENT_<id>_PERSIST` and selection-derived
`HOST_LINKS`, including prospective selections. Explicit operator links retain
their authority. [Project session linking](../../internal/application/project_environment.go)
accepts directory links under `/mnt/host/agent-sessions`, preserves ordinary files
and directories, and omits `:file` persistence from L2 containers. L1 owns file
persistence. Historical defaults and predecessor inventory paths guard existing
state; declarations grant no ownership of arbitrary files at those destinations.
Preserve explicit-link exclusions, ownership checks and retained data.

[Credential import validation](../../internal/adapters/credentialruntime/records.go)
checks canonical paths, regular files, ownership and permissions. Its `.codex`,
`.claude`, `.pi`, `.config/opencode`, `auth.json` and `credentials/oauth.json`
exclusions remain explicit security policy alongside profile-owned exclusions.
They apply regardless of selection, declarations or package retirement; artifact
metadata cannot authorize importing retained mutable coding-tool or OAuth stores.

[Editor recommendations](../../internal/application/project_actions.go) retain the
Claude, Codex and OpenCode fallback and validated controller-side
`CODE_RECOMMENDED_EXTENSIONS` override. This editor UX default is independent of
yard selection and installation and grants no execution or credential authority.
New integrations require no recommendation; automatic package discovery would
need a separate UX contract. Existing project-action, session-link, credential
denylist and integration inventory tests own these boundaries.
