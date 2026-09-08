# Codex Quota Scheduler

Standalone CPA plugin for balanced concurrent or serial Codex account selection, native quota polling,
5h/weekly/monthly windows, persistent 429 quarantine and optional warmup.
Source version: **0.3.0**. Local builds are not a published release.

## CPA dashboard

After enabling the plugin, refresh CPA Management Center and open **插件 →
Codex 额度调度**. The plugin management list uses the same display name and the
stable ID `codex-quota-scheduler`. The dashboard shows the current account,
5h/weekly remaining quota and resets, daily weekly budget, freshness and warmup
records. Its 15-second refresh reads the existing cache without upstream or
model requests. Opening or refreshing the panel does not change settings.

Daily operations are available directly in this panel; no JSON/YAML editor is
needed after installation:

- **账号与额度**: choose a current account, restore automatic selection, and
  clear one or all local cooldowns with confirmation.
- **调配设置**: edit switching policy, weekly budgeting and default/per-account
  plans, including **均衡并发** (`balanced`) for simultaneous account use. The
  default 5h policy remains zero reserve and hard-limit/429 handoff.
- **预热管理**: enable/disable warmup, select its model, set spacing and daily
  limits, view cycle confirmation, and unblock failed retries. Unblocking does
  not erase confirmed cycles or bypass cooldowns and budgets.
- **连接与高级**: edit polling, cache freshness, recovery timing, CPA connection
  paths and advanced scheduling parameters using labeled controls.

One draft survives tab switches and quota refreshes. **保存并应用** validates the
complete resulting settings, PATCHes only edited fields through CPA, then checks
both saved config and effective runtime values. **放弃并重新读取** discards the
draft. A pre-save comparison detects changes from other pages; CPA has no atomic
compare-and-swap API, so avoid simultaneous edits to the same field. Ambiguous
writes are read back rather than automatically resubmitted. Plugin installation
and host-level enable/disable remain in CPA's plugin list.

The standalone resource entry is `/v0/resource/plugins/codex-quota-scheduler/open`.
It serves static assets only; quota data remains behind CPA management
authentication. The page reuses a remembered CPA login only when its API origin
and path match this server. Otherwise it asks for a management key, held in
page memory only. Keys never appear in links or public resources. Both current
encrypted and scoped CPA storage formats are supported. No sidecar is needed.

UI authentication and settings regressions: `node --test web/*.test.mjs`.
Linux release assets use the Debian 12 glibc baseline for CPA compatibility.

## Dependencies

Only CPA is required. The plugin reads CPA's authenticated auth-files inventory
and queries upstream quota through CPA's authenticated api-call endpoint.
CPA resolves the auth index, substitutes $TOKEN$, and applies its proxy policy.
The plugin never reads or stores upstream OAuth/PAT credentials.

The default endpoint is https://chatgpt.com/backend-api/wham/usage.
The quota_url option can select a trusted alternative implementing the same
native schema. Custom providers without that schema cannot supply fresh quota;
the plugin never invents their percentages.

No external quota service, login session, cache database, refresh queue, pricing
endpoint, or external quota-service password is used.

## Setup and migration

Use SERIAL_CONFIG.example.yaml. Configure cpa_management_url and mount the
cpa_management_key_file; the CPA process/container must reach that address.
Remove the former external-service URL/password configuration and secret mounts.
Unknown legacy YAML fields are ignored and never activate a legacy path.
Retain state.json and generation files to preserve bans and serial history.

## Polling and cache

- Inventory is checked every refresh tick. Disabled/deleted credentials are
  removed from the cache. Temporarily unavailable accounts remain observable
  so cooldown recovery does not require a generation request.
- The active account uses refresh_interval (30s); standby accounts use
  quota_refresh_cooldown (2m). Up to quota_refresh_batch (8) queries execute
  sequentially per tick, active first and oldest attempts next.
- In balanced mode, recently used healthy accounts use a one-minute cadence
  with the default settings; idle accounts use two minutes, and near-depleted
  accounts return to 30 seconds. Overlapping refresh calls are coalesced.
- Recent response quota headers can defer a redundant query, but cannot renew
  the native snapshot. A full probe remains due within the standby interval or
  half the freshness limit. Errors, near-exhaustion, and resets retain their
  independent probe/backoff rules.
- Backoff is per account/auth index and persists across reloads. Failures double
  the interval up to 30 minutes; a longer Retry-After wins. Authentication and
  permission failures wait at least 30 minutes.
- Relative reset values are anchored once per observation. Cache reads never
  move reset timestamps. Older polls cannot replace newer header observations.
- Fresh hard-limit evidence remains effective even if a sibling window is stale.
- State atomically persists quota snapshots, poll guards, bans, serial selection
  and warmup records. Query errors are short codes, never private response bodies.

The native rate_limit primary/secondary windows are classified by duration.
Feature/model-scoped additional limits are not treated as account-wide limits.
The former external pricing and window-cost calibration are no longer available;
serial remains the default and pacing retains built-in cost estimates.

## Routing and recovery

Select **均衡并发** in the panel to spread requests across accounts. This does
not limit the pool to one active request or one account. Equivalent accounts
receive even shares; unequal plans and budgets receive weighted shares. Existing
installations keep their configured mode until it is changed in the panel.

Balanced mode first excludes hard-limited/quarantined accounts and prefers
fresh usable accounts outside the weekly reserve. It then uses smooth weighted
work accounting, with weight approximately
`plan multiplier × min(5h remaining fraction, weekly daily budget / (100/7))`.
Each pace factor has a 0.01 floor, which changes relative share without reserving
5h capacity. The weekly reserve becomes spendable when the entire eligible
pool is inside it. Account-specific pins remain respected.

Every pick immediately debits estimated work under one lock so concurrent
requests cannot all choose a stale best score. Completion token costs correct
that estimate with bounded debt; cost estimates never mark quota exhausted.
The host ABI has no shared selection/completion request ID, so outstanding work
is labeled an estimate and unmatched/expired completions cannot debit new work.
Short-lived fairness history resets on reload; quota/bans/warmup history stays
persisted. Balanced mode postpones optional warmup while foreground requests
are active and for a one-minute quiet interval. These controls reduce redundant
traffic and bursts; they do not guarantee avoidance of upstream risk controls.

The following settings apply to the retained **串行调配** mode:

Traffic stays on one committed account. Default `serial_allocation_policy:
sustainable` first respects hard limits and quarantine, then
ranks same-class peers by `(weekly remaining - weekly reserve) / days to reset`.
The denominator is bounded below by six hours; unused placeholders use a full week.
With equal reset times, 80% weekly outranks 40%. With 40% resetting tomorrow and
80% resetting in six days, the former has more spendable budget per day.

Proactive budget handoffs require a 20% relative advantage, two distinct fresh
weekly readings of **both** accounts, and a 5-minute primary hold. Configure
`serial_budget_rebalance_percent` (0 disables budget preemption) and
`serial_weekly_rebalance_min_hold` (1m-24h). `weekly_remaining` retains the earlier
raw percentage policy and its 10-point `serial_weekly_rebalance_percent` control.
Cached reads, failed polls and mismatched auth indices cannot confirm a switch;
late completion headers cannot override stricter fresh probe evidence.
The quota endpoint exposes `serial_weekly_rebalance` and the committed reason
`weekly_budget_rebalance`, with the comparison metric explicitly labeled.

Default `serial_5h_handoff_mode: 429_only` and `reserve_5h_percent: 0` use the
observed 5h capacity without an early reserve handoff. Reaching 98% or 99% used
alone keeps the current account; a confirmed hard limit, disallowed state or
upstream 429 still triggers server-side handoff. Weekly balancing remains active.
In this mode, 5h ranking uses observed remaining capacity times the plan prior;
it deducts neither a configured static reserve nor forecast/cache-age estimates.
Runway diagnostics remain observational and do not change that decision.

To restore the earlier reserve policy, explicitly select `reserve_aware` and set
`reserve_5h_percent: 15`. Two positive native probe increments then enable a
two-minute forecast reserve and observation-age debit. Dynamic reserve is capped
at max(static reserve, 50%). Those optional safety handoffs do not wait for the
budget hold, and drain cannot bypass them. With no safer peer, soft reserves
remain usable; hard limits always win. `serial_soft_continuation: true` separately
restores the earlier session continuation past a soft handoff.

Plan priors default to `team_standard`. `plus` and `team_standard` use 1;
`pro_5x` and `team_premium` use 5; `pro_20x` uses 20. Set per-auth overrides in
`quota_account_plans`. Ambiguous native plan names are not guessed. Multipliers
only compare 5h available capacity within the same 5% weekly budget band: multiplying them
into the weekly score would prematurely exhaust large accounts. They do not
establish fixed weekly capacity. See the official sources, experiments and
limitations in [the allocation study](research/ALLOCATION_RESEARCH.zh-CN.md).

Polling adds no model requests. An entire refresh, including inventory, has a
1–30s budget bounded by `refresh_interval`; each upstream call has at most 10s.
Timeouts retain cache/backoff and stop further dispatch until another tick.

Automatic preemptions move subsequent session requests to the replacement;
already-running streams finish on their original account. Manual selection
disables proactive weekly balancing and 5h cycle rotation. Hard limits still
force failover immediately. A cycle reset cannot rotate a healthy 80% weekly
account onto a lower-budget peer just to alternate IDs.

Percentages balance reported capacity for comparable accounts. They do not
predict demand or establish equal absolute capacity across different plans.
429 recovery still requires one serialized half-open probe; early quota resets
require two strictly newer observations. Generation ownership and warmup leases
prevent superseded instances from committing state or running model warmups.

Optional warmup defaults to native CPA HostModel and is disabled by default.
Explicit legacy management warmup remains optional and is never required for
quota polling. A native warmup callback must finish before its worker can exit.

Warmup uses conservative admission controls, separate from client failover:

- `warmup_min_interval: 15m` spaces attempts across the entire pool;
  `warmup_max_per_day: 8` caps admissions in a rolling 24-hour window.
  Failures count. The ledger survives restarts, hot reloads and manual retries.
- Each recognized quota window must be allowed, above its configured reserve and
  observed within the last 2 minutes (or a shorter `stale_after`). Active serial
  accounts, failed quota polls and changed auth bindings are skipped. A changed
  binding must first acquire its own fresh quota observation.
- State must be writable: admission is persisted before dispatch. Quota and
  quarantine are checked again immediately before execution. A failed commit
  sends no generation request.
- Retryable failures pause the entire warmup pool with exponential backoff
  starting at `warmup_retry_after` (15m), capped at 6h. A longer Retry-After/reset
  wins. Three failures for an account, or a nonretryable auth/policy failure,
  require explicit repair and `POST /warmup-retry` before auto-warmup resumes.
  Changing auth indexes does not clear that block.
- Cancellation, timeout or an unknown outcome waits at least 5h. Reload never
  grants an immediate retry. Completed activation with no reset header retains
  its original window suppression; a moving zero-usage placeholder does not
  justify another generation after 30 minutes.
- An explicit `max_output_tokens` terminal result counts as executed activation
  and awaits quota confirmation. Other incomplete/failed results remain errors.
  HTTP 429 and SSE quota failures enter the same quarantine.

The authenticated quota status includes `warmup_traffic` (rolling attempt count,
hold reason and earliest budget/backoff time), plus each warmup's failure count
and outcome time. A clear traffic hold is only one admission condition. Actual
execution still requires fresh eligible quota. Old state can only migrate the
attempts that the previous version retained.

These controls reduce optional generation traffic. They do not establish an
upstream risk threshold or guarantee that an account will not be restricted.

Transparent recovery of the same streaming request also requires the matching
CPA host pre-output quota failover patch. The deployment target is CPA v7.2.152
with `codex.stream-bootstrap-buffering: true` and `max-retry-credentials: 0`
(no separate credential-count cap within the host retry policy). Deployment
completion must be verified against the running host; these requirements do not
report a completed rollout. A scheduler plugin cannot replay an already
committed client stream.

## Management and validation

Authenticated routes are under /v0/management/plugins/codex-quota-scheduler/.
GET /quota exposes windows, freshness, per-account quota_polls, errors,
generation state and serial/warmup diagnostics. Existing bans, serial-active
and warmup management operations remain.

Go 1.21+ and a C compiler are required:

~~~sh
go test ./...
go test -race ./...
go build -buildmode=c-shared -o codex-quota-scheduler.dll .
~~~

Build a .so on the deployment's Linux platform; Windows DLLs cannot load there.
See HANDOFF.zh-CN.md for migration acceptance.
