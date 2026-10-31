# CPA permissions and compatibility

The scheduler is a native library loaded into the CPA process. Run CPA as a
dedicated unprivileged user. CPA capability flags describe callbacks; they do
not sandbox the library or create a restricted management API key. Installing
the library grants it the operating-system access of the CPA process.

## Registered capabilities

| Capability | Why the plugin needs it |
| --- | --- |
| `scheduler` | Select an eligible Codex credential and reject confirmed blocked selections. |
| `scheduler_continuity` | Return immediate wait/pacing advice to a compatible patched host. |
| `request_interceptor` | Correlate native request IDs and enforce credential admission before dispatch. |
| `request_lifecycle_plugin` | Release concurrency, pacing and recovery reservations on terminal events. |
| `usage_plugin` | Observe Codex quota headers, completion outcomes and consumption. |
| `management_api` | Serve authenticated diagnostics/actions and the static panel assets. |

The plugin does not register an executor, model router, translator, login
provider, command-line capability or response/stream interceptor. CPA still
owns upstream execution and routing. Interceptor callbacks can carry request
content; the scheduler uses execution/routing identifiers and does not retain
prompts or generated text in its state.

## Management key and host callbacks

`cpa_management_key_file` contains the plaintext CPA **management** key that
matches the host configuration. A client API key or a GitHub token is not a
substitute. The mounted file must be readable by the CPA user and hidden from
other users. Prefer a read-only mount. The management key authorizes broader
CPA administration than the scheduler needs; CPA v8.0.44 has no per-endpoint
scope for this key.

| Interface | Access and purpose |
| --- | --- |
| `GET /v0/management/auth-files` | Read credential inventory and eligibility. |
| `POST /v0/management/api-call` | Query native quotas and reset-credit lists; optionally perform explicitly enabled, budgeted warmup or pre-expiry credit redemption using CPA's `$TOKEN$` substitution. |
| `host.auth.list` | Read native credential eligibility for dispatch checks. |
| `host.auth.get` | Read a physical credential JSON document for expiry diagnosis. This can include OAuth/PAT material. |
| `host.auth.get_runtime` | Recheck the exact credential before expiry repair. |
| `POST /v0/management/auth-files?name=...` | When `auth_expiry_auto_repair: true`, upload only the verified opaque-PAT document with a stale top-level `expired` field removed. |
| `host.routing.reset_cooldown` | Reset one matching native credential cooldown after verified quota recovery. Requires host support. |
| `/v0/management/plugins/codex-quota-scheduler/...` | Authenticated status, settings/config, manual selection, cooldown reset and warmup retry. |

Expiry repair defaults to enabled. It requires two successful native quota
observations of the unchanged document, eligibility checks and durable state.
OAuth/JWT expiry and refresh tokens are never rewritten. Setting
`auth_expiry_auto_repair: false` prevents repair uploads; **expiry diagnostics
still read credential JSON**. Raw documents remain in memory during diagnosis
and are excluded from plugin state, browser responses and logs. This setting
is a behavior switch, not an operating-system permission boundary.

Configure `cpa_management_url` to a trusted CPA endpoint, preferably loopback
inside the CPA container. Authenticated plugin HTTP calls do not follow
redirects. Use HTTPS when management traffic crosses an untrusted network.
`quota_url` also receives a substituted upstream bearer token through CPA;
change it only to an endpoint trusted to receive that credential.

Reset-credit lists use `GET https://chatgpt.com/backend-api/wham/rate-limit-reset-credits`.
`reset_credits_auto_use` defaults to **false**. Enabling it permits a real
`POST .../rate-limit-reset-credits/consume` with a unique `redeem_request_id`
shortly before expiry (default `10m`), after fresh eligibility/identity checks.
The server chooses the consumed card; the plugin cannot specify a card ID.
This is an opt-in upstream mutation, separate from model warmup. A generation-
fenced journal at `state_path + ".reset-credits.json"` prevents write replay;
uncertain outcomes require review and are not automatically retried. The journal
contains hashed identifiers and operation UUIDs, no bearer tokens or raw credit
IDs. Preserve it across upgrades, rollback and state migration. Only verified
quota recovery can trigger the matching native credential cooldown reset.

## Public resources and browser storage

`/v0/resource/plugins/codex-quota-scheduler/open` and its explicitly listed
CSS/JavaScript resources are public static assets. They contain no live quota,
credentials or management key. All dynamic reads and writes use CPA's
authenticated management namespace. Resource responses set a same-origin CSP,
`nosniff`, `no-referrer` and `no-store`.

The panel can reuse a CPA login only for the same origin and proxy base path.
The login form's 24-hour remember checkbox is selected by default; uncheck it
for a page-memory-only manual login. Remembered keys use CPA-compatible browser
encoding, **not encryption**. Same-origin scripts and browser extensions can
read that storage. Use a trusted browser profile, and do not host untrusted
applications or scripts on the CPA origin. HTTP 401 clears a rejected panel
session; HTTP 403 is treated as an access restriction and preserves it.

## Filesystem access

The CPA process needs:

- Read access to the platform library and search access to its parent folders.
  A Linux `.so` normally needs mode `0644`; the CPA executable needs execute
  permission. The plugin directory must not be writable by untrusted users.
- Read access to the management key, normally owner-only `0400` or `0600`.
- Read/write/search access to the private state directory, normally `0700`.
  State, generation ownership, lock, reset-credit and warmup journal files are created with
  owner-only permissions. Preserve these together across plugin updates.
- Existing CPA access to its auth directory. Expiry reads and repair writes use
  CPA's native callbacks/API rather than a separate auth-directory mount.

Windows deployments need equivalent NTFS permissions for the CPA service
identity; Unix mode bits alone do not establish Windows ACL protection. No
administrator/root, Docker socket, SSH key, external database or separate
sidecar is required by the scheduler. Warmup defaults to disabled and uses
model quota when enabled.

## Host compatibility

- Native credential concurrency requires CPA v8.0.4+ request IDs and terminal
  lifecycle callbacks. Disable `account_concurrency_enabled` on older hosts.
- Automatic long waits require the matching continuity host integration.
  Advertising `scheduler_continuity` in the plugin does not add it to an
  unchanged official host. The panel distinguishes configured settings from
  observed host callbacks.
- For v8.0.44 use only
  [`CLIProxyAPI-v8.0.44-continuity.patch`](integration/CONTINUITY-v8.0.44.md)
  against the exact revision and SHA-256 in its manifest. The v8.0.4 and
  v7.2.152 patches have different bases. Host test exclusions and upstream
  Windows race/checkptr limitations are documented in that integration note.
- The DLL/SO/dylib must match the CPA OS and CPU architecture. Linux release
  builds use Debian 12's glibc baseline. Plugin source remains Go 1.21 syntax
  compatible; release builds use the maintained Go 1.26.10 toolchain.
- Client/proxy absolute timeouts still apply to the configured recovery wait.
  No callback makes emitted output or tool calls safe to replay.

The host callback semantics are documented in CPA's
[official v8.0.44 example](https://github.com/router-for-me/CLIProxyAPI/blob/v8.0.44/examples/plugin/host-callback-auth-files/README.md).
The host plugin configuration is in
[`internal/config/config_types.go`](https://github.com/router-for-me/CLIProxyAPI/blob/v8.0.44/internal/config/config_types.go).
