# Security Policy

## Supported version

Security fixes target the latest release on the default branch.

## Reporting

Please report a vulnerability privately through GitHub Security Advisories for this repository. Do not include live PATs, OAuth tokens, cookies, CPA Management keys, or production auth files in an issue.

## Secret handling

The plugin expects secrets through mounted files:

- CPA Management key: `cpa_management_key_file`

Secret values are not accepted as ordinary plugin configuration fields, are not
written to the state file, and must not be logged. Quota and warmup requests use
CPA's token substitution. Expiry diagnostics do read credential JSON through
`host.auth.get`, and the default-enabled, narrowly verified opaque-PAT repair
can upload that document with a stale top-level expiry removed. Raw documents
are not persisted in scheduler state or returned to the panel.

See [CPA_PERMISSIONS.md](CPA_PERMISSIONS.md) for the exact capabilities,
management API access, filesystem permissions and host requirements. Native
plugins inherit the CPA process's privileges; capability flags are not a sandbox.

## Exposed routes

Dynamic data and operations use CPA-authenticated routes under:

`/v0/management/plugins/codex-quota-scheduler/...`

Public `/v0/resource/plugins/codex-quota-scheduler/...` routes serve explicitly
listed embedded static assets only. They do not expose account state or keys.

The panel's 24-hour remembered login is selected by default and stores the CPA
management key using reversible browser encoding, scoped to the CPA origin and
proxy path. Uncheck it for page-memory-only manual login. Browser encoding is
not encryption; only trusted scripts should share the CPA origin.
