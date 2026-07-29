# Security Policy

## Supported version

Security fixes target the latest release on the default branch.

## Reporting

Please report a vulnerability privately through GitHub Security Advisories for this repository. Do not include live PATs, OAuth tokens, cookies, Keeper passwords, CPA Management keys, or production auth files in an issue.

## Secret handling

The plugin expects secrets through mounted files:

- Keeper login password: `keeper_password_file`
- CPA Management key: `cpa_management_key_file`

Secret values are not accepted as ordinary plugin configuration fields, are not written to the state file, and must not be logged.

## Exposed routes

The plugin registers only CPA-authenticated routes under:

`/v0/management/plugins/codex-quota-scheduler/...`

It intentionally registers no dynamic `/v0/resource/plugins/...` handlers.
