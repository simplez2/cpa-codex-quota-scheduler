# Serial Quota-Balanced Scheduler Design

## Goal

Keep a committed primary for unkeyed requests and independent bindings for identified conversations. Spend usable 5h quota before weekly-only backups, and earlier confirmed reset deadlines before later ones. Weekly allocation weights apply within equal-priority groups. Preserve eligible conversations and in-flight streams, and bound switch/query overhead. The default allocation policy is `sustainable`; default 5h handoff is `429_only`. `weekly_remaining` retains raw-percentage tie-breaking within the same priority group.

## Persisted state

The scheduler persists:

- `serial_active_auth_id` and `serial_selection_source`;
- `serial_selected_at`, switch/fallback counters, and the last switch reason;
- `serial_overdraft` hashed-session bindings;
- `serial_last_selected` per-auth timestamps for deterministic rotation;
- `serial_five_hour_cycle` reset anchors for detecting a real new 5h cycle;
- the existing 429 quarantine, reset-confirmation, and warmup records.

The state contains operational auth identifiers and hashed session identifiers, but no PAT, OAuth token, Cookie, or CPA Management key. Treat the state and Management output as private operational data.

## Request path

For a pure Codex candidate set:

1. Read the committed primary and any existing hashed-session binding.
2. Existing sessions retain their eligible binding independently of the primary. Explicit `serial_soft_continuation=true` additionally permits continuation across a soft threshold, never a hard limit or quarantine.
3. A new session ranks eligible candidates by pool and confirmed reset deadline, followed by within-group weekly allocation. It cannot acquire a new binding to an already exhausted auth.
4. Keep an eligible primary unless a defined preemption boundary is reached.
5. On a required switch, rank eligible candidates deterministically and commit exactly one replacement.
6. If the primary is hard-limited and every backup has crossed only the soft threshold, select the best soft-threshold backup.
7. If no safe or soft-threshold backup exists, return `Handled=false`; CPA host behavior then applies.

## Window evaluation

All active native quota windows are scanned before assigning a reason. Row order cannot change the outcome. Severity is:

~~~text
not_allowed / limit_reached > serial_threshold > eligible
~~~

The drain duration is:

~~~text
min(drain_window_hours, complete_window_duration * 10%)
~~~

Default 5h handoff is `429_only` with `reserve_5h_percent: 0`. A 98% or 99% used reading alone does not cause early 5h handoff; confirmed hard limits, disallowed state, quarantine and upstream 429 still do. Weekly allocation continues independently. In this mode, 5h ranking uses observed remaining quota without static reserve, observation-age debit or forecast deductions. Runway estimates remain diagnostics only.

Explicit `reserve_aware` with a positive reserve (15% reproduces the previous setting) restores optional safety handoffs. In sustainable mode, two positive native probe increments enable observation-age debit and a two-minute burn forecast; reserve is capped at max(static,50%), while cache debit is separate. These soft guards do not predict exact request cost or inflight work. With the six-hour drain configuration, 5h drains only in its final 30 minutes; weekly and monthly windows retain a six-hour drain. Drain may bypass an ordinary configured percentage threshold, but never an enabled reserve, hard limit, disallowed state, quarantine or 429.

## Candidate ordering

Candidate ordering is a strict, deterministic total ordering:

1. known eligible candidates before quota-unknown or soft-threshold fallbacks;
2. configured `window_order` (default 5h before weekly/monthly), then earlier confirmed active resets before dormant placeholders or unknown reset times;
3. within that pool/deadline group, unprotected weekly capacity, known weekly capacity, then sustainable weekly budget per day in pool-relative 5% bands;
4. observed 5h remaining quota times the configured plan prior within that budget band under default `429_only`; optional reserve policies instead use their configured headroom, followed by raw-weekly/5h tie-breakers;
5. drain state and reset-credit availability when enabled;
6. remaining weekly capacity, active-cycle preference, and lower maximum use as tie-breakers;
7. the longest-idle `serial_last_selected` timestamp;
8. CPA priority and stable auth ID.

Tiers are computed before sorting, not with non-transitive pairwise bands. `weekly_remaining` uses raw weekly tiers from `switch_hysteresis_percent` (default 2 percentage points) only after the shared pool/deadline priority.

The same ordering applies to cold starts and replacements. Earlier confirmed 5h resets outrank a larger weekly budget. With equal deadlines, weekly budget and then 5h capacity break ties. The budget denominator has a one-minute floor; zero-use placeholders and missing reset times use a whole week. Plan multipliers never multiply the weekly percentage score. Any fresh hard-exhausted window excludes an account before ranking, including stricter native evidence obscured by late completion headers.

## Committed switch triggers

The global primary changes for:

- a soft threshold or reserve crossing in a window explicitly using such a policy, when a replacement exists; default 5h `429_only` does not use this trigger;
- `allowed=false` or `limit_reached=true`;
- authoritative 429, quarantine, or failed half-open state;
- candidate absence confirmed for at least 90 seconds and three observations;
- a higher-priority window class becoming available;
- weekly reserve protection when an unprotected replacement exists;
- confirmed proactive weekly rebalancing while the current primary is still usable;
- one constrained rotation after a verified 5h reset boundary advances by more than five minutes.

Manual selection disables proactive weekly rebalancing and automatic 5h cycle rotation. Existing hard-limit, 429, quarantine, candidate-loss, higher-priority-window, and weekly-reserve protections remain active. An explicit request-level `pinned_auth_id` does not mutate the primary or its confirmation evidence.

## Proactive weekly rebalancing

For an automatically selected, eligible primary, inspect same-class eligible peers in ranked order. The sustainable challenger must improve normalized weekly daily budget by `serial_budget_rebalance_percent` (default 20% relative); the comparison denominator is at least one weekly percentage point/day. Zero disables budget preemption. Legacy mode requires `serial_weekly_rebalance_percent` (default 10 percentage points) and the raw-weekly hysteresis band. Safety handoffs do not wait for either optimization rule.

Both accounts need inventory-bound auth indices, no failed latest quota poll, a fresh complete snapshot, and actual weekly-window observation timestamps newer than the primary's selection. Two confirmations are required: **both** weekly timestamps must strictly advance for each new confirmation. A request, a new outer cache timestamp or a one-sided refresh is insufficient. The existing polling schedule is reused; this feature adds no upstream requests.

The primary must also have been selected for `serial_weekly_rebalance_min_hold` (default 5m, configurable from 1m to 24h). Confirmations may accumulate during this hold; they remain subject to freshness and advantage checks when the hold expires. Lost advantage, a failed poll, identity/candidate changes, or a true new cycle invalidate previous evidence. Selection time is persisted, while confirmations are deliberately rebuilt after reload. Missing legacy selection times establish a new hold baseline.

The authenticated quota endpoint exposes policy, plan prior, normalized daily budget, per-window runway assessment and `serial_weekly_rebalance` (metric, challenger, advantage, timestamps and count). Budget switches record `weekly_budget_rebalance`; legacy percentage switches record `weekly_rebalance`. Automatic handoffs move bindings without extending their inactivity TTL. Already-running streams are unaffected. Confirmation uses native observation timestamps and conservative quota values, not late completion-header arrival times.

This is a bounded percentage-balancing heuristic for comparable accounts. Quota observation latency, demand bursts, unequal plan capacity and unknown future demand prevent a claim of globally optimal allocation or guaranteed full pool utilization.

Same-request recovery before the first client output also requires the matching CPA host patch. The deployment target is CPA v7.2.152 with `codex.stream-bootstrap-buffering=true` and `max-retry-credentials=0`; the host still enforces its overall retry policy. These are release requirements, not evidence of a completed deployment. Already committed client output cannot be replayed unconditionally.

## Five-hour cycle rotation

The scheduler records the selected auth's 5h reset anchor. A new cycle is recognized only when:

- the previous recorded reset boundary has elapsed; and
- the fresh native quota reset anchor advances by more than five minutes.

This prevents moving full-duration placeholder resets from looking like a new cycle on every refresh. Sustainable rotation cannot lower weekly daily budget by over 5%; legacy rotation cannot lower weekly remaining beyond its hysteresis band. A boundary is consumed once even when rotation is blocked by reserves, budget imbalance or no peer, preventing a delayed mid-cycle switch.

## Existing-session overdraft

When CPA supplies a stable session identifier, the plugin hashes it. Default handoffs move subsequent requests to the replacement. Explicit legacy soft continuation uses a 30-minute sliding inactivity TTL, which is not an absolute conversation-duration bound; active sessions can refresh it. Hard limits and quarantine always end that compatibility behavior. This is local routing policy, not an upstream continuation or billing guarantee.

## Concurrency and hot reload

Selection and primary mutation happen under one process mutex. Cross-generation state commits use a durable generation fence.

Version 0.1.20 stores the generation lock in the stable `state_path.generation.lock` file. Once the append-only `state_path.generation` journal reaches 768 KiB, the next generation I/O compacts it before running its callback; legacy journals up to 16 MiB are recovered and atomically reduced to their last valid monotonic record. The first migration from the older journal-inode lock protocol must use a controlled CPA restart; subsequent same-protocol reloads can use generation ownership normally.

## Warmup boundary

Warmup remains separate from normal scheduling:

- candidates execute one at a time through a pinned auth request;
- warmup never changes the committed normal-traffic primary;
- HTTP success remains pending until native quota confirms a stable reset anchor;
- 429 uses normal quarantine;
- cyber-policy, abuse, auth, and workspace failures are terminal blocked outcomes and are not automatically retried.

## Failure behavior

| Failure | Behavior |
|---|---|
| native quota unavailable or stale | keep the current eligible primary; do not infer a quota transition |
| 429 with quota headers | quarantine until reset, then admit one half-open probe |
| 429 without quota headers | bounded probation, then one half-open probe |
| transient CPA candidate suppression | stable request-local provisional fallback |
| confirmed candidate loss | commit the best eligible replacement |
| hard-limited primary, soft-threshold backup | explicitly commit the best soft-threshold backup |
| all candidates hard-exhausted/quarantined | return `Handled=false` |
| mixed or third-party provider set | return `Handled=false` |

## Security boundary

The plugin registers only authenticated Management API routes. It does not place dynamic state, privileged operations, iframes, or host callbacks under the unauthenticated resource route family. It does not modify credentials, authentication methods, model lists, provider routes, or third-party APIs.
