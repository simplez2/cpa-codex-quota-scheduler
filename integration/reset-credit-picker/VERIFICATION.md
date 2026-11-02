# Manual reset-credit picker acceptance

Deployed 2026-11-02 14:30 America/New_York (19:30 UTC).

## Build and contract checks

- Keeper frontend: TypeScript, ESLint and production build passed; 83 focused tests passed.
- Keeper Linux build: quota-provider, quota-service and API test packages passed; quota vet passed.
- Management Center Linux build: all 1872 tests, ESLint, TypeScript and the single-file production build passed.
- Added checks cover explicit second-credit forwarding, fresh account-specific validation,
  stale/expired/invalid selection rejection without a consume POST, and API request binding.
- One pre-existing Management Center test expected an unescaped apostrophe inside an HTML
  attribute. The patch makes that assertion compare the rendered attribute consistently.
- Patch reverse-apply checks passed against the modified pinned source trees.

## Live browser checks

| Surface | Selection | Submitted `credit_id` matches selected row | Real redemption forwarded |
| --- | --- | --- | --- |
| Keeper | Second | Yes | No, intercepted |
| Keeper | Third | Yes | No, intercepted |
| CPA Management Center | Second | Yes | No, intercepted |
| CPA Management Center | Third | Yes | No, intercepted |

Each dialog defaulted to the earliest expiry, supported clicking a different native radio
control, and retained the selected ID through its frontend submission path. Browser runs
reported no page errors. Actual quota redemption success is not claimed by these tests.
During the earlier investigation one legacy consume attempt returned HTTP 401; subsequent
read-only verification confirmed that all three credits remained. This patch changes
selection and forwarding, not upstream authorization policy.

## Production and rollback

- CPA container identity and process PID stayed unchanged.
- Keeper kept its original data volume, environment, networks and private IPs.
- SQLite `quick_check` returned `ok`; all 73547 existing usage events remained at cutover.
- A consistent database backup and original Keeper compose configuration were retained
  in the private deployment stage before replacement.
- Management Center is a persistent static overlay at the existing `/management.html`
  public entry; it remains a single HTML file and preserves the native v8 API routes.
  Cloudflare's injected telemetry changes the full public HTML hash, so browser acceptance
  also verifies the exact embedded application-module hash.
- To roll back, restore the previous Keeper image/compose configuration and remove the
  dedicated Management Center static overlay route, then validate and reload OpenResty.
  Do not restore an old database over newer usage events during an ordinary UI rollback.
- Scheduler plugin remains v0.3.38; its automatic redemption configuration and journal
  were not changed by this UI deployment.
