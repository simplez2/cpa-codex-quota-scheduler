# Serial Quota-Balanced Scheduler Design

## Goal

Keep exactly one globally committed Codex credential for normal traffic. Do not distribute new requests randomly or concurrently across the pool. At committed switch boundaries, preserve weekly capacity and use available 5h cycles efficiently.

## Persisted state

The scheduler persists:

- `serial_active_auth_id` and `serial_selection_source`;
- `serial_selected_at`, switch/fallback counters, and the last switch reason;
- `serial_overdraft` hashed-session bindings;
- `serial_last_selected` per-auth timestamps for deterministic rotation;
- `serial_five_hour_cycle` reset anchors for detecting a real new 5h cycle;
- the existing 429 quarantine, reset-confirmation, and warmup records.

The state contains operational auth identifiers and hashed session identifiers, but no PAT, OAuth token, Cookie, Keeper password, or CPA Management key. Treat the state and Management output as private operational data.

## Request path

For a pure Codex candidate set:

1. Read the committed primary and any existing hashed-session binding.
2. An existing session may keep its previous auth during a bounded overdraft continuation, provided that auth is still present and not quarantined.
3. A new session is bound only after the current primary is confirmed eligible; it cannot acquire a new binding to an already exhausted auth.
4. Keep an eligible primary unless a defined preemption boundary is reached.
5. On a required switch, rank eligible candidates deterministically and commit exactly one replacement.
6. If the primary is hard-limited and every backup has crossed only the soft threshold, select the best soft-threshold backup.
7. If no safe or soft-threshold backup exists, return `Handled=false`; CPA host behavior then applies.

## Window evaluation

All active Keeper windows are scanned before assigning a reason. Row order cannot change the outcome. Severity is:

~~~text
not_allowed / limit_reached > serial_threshold > eligible
~~~

The drain duration is:

~~~text
min(drain_window_hours, complete_window_duration * 10%)
~~~

With the default six-hour configuration, 5h drains only in its final 30 minutes. Weekly and monthly windows retain a six-hour drain. Drain may cross the soft threshold, but not a hard limit, disallowed state, quarantine, or 429.

## Candidate ordering

Candidate ordering is a strict, deterministic total ordering:

1. unprotected weekly capacity before accounts at or below `reserve_weekly_percent`;
2. drain state;
3. configured `window_order`;
4. reset-credit availability when enabled;
5. on the first pool cold start only, CPA priority and historical fill-first compatibility;
6. a pool-relative weekly balance tier derived from `switch_hysteresis_percent`;
7. lower 5h used percentage inside the best weekly tier;
8. remaining weekly capacity, active-cycle preference, and lower maximum use as tie-breakers;
9. the longest-idle `serial_last_selected` timestamp;
10. CPA priority and stable auth ID.

The weekly tier is computed from the whole candidate pool before sorting. It is not a pairwise hysteresis comparison, because pairwise bands are non-transitive and can make a sort depend on input order.

## Committed switch triggers

The global primary changes for:

- a soft `serial_switch_percent` crossing outside drain, when a replacement exists;
- `allowed=false` or `limit_reached=true`;
- authoritative 429, quarantine, or failed half-open state;
- candidate absence confirmed for at least 90 seconds and three observations;
- a higher-priority window class becoming available;
- weekly reserve protection when an unprotected replacement exists;
- one constrained rotation after a verified 5h reset boundary advances by more than five minutes.

Manual selection disables only automatic 5h cycle rotation. Existing hard-limit, 429, quarantine, candidate-loss, higher-priority-window, and weekly-reserve protections remain active.

## Five-hour cycle rotation

The scheduler records the selected auth's 5h reset anchor. A new cycle is recognized only when:

- the previous recorded reset boundary has elapsed; and
- the fresh Keeper reset anchor advances by more than five minutes.

This prevents moving full-duration placeholder resets from looking like a new cycle on every refresh. A detected boundary is consumed once even if rotation is blocked by weekly reserve or the absence of a peer, preventing a delayed mid-cycle switch.

## Existing-session overdraft

When CPA supplies a stable session identifier, the plugin hashes it and records the auth that already served it. After a global threshold or hard-limit switch, that existing session may continue on the old auth while new sessions use the replacement. Bindings have a 30-minute sliding inactivity TTL and are removed on candidate disappearance or quarantine. This mechanism preserves observed in-flight continuation behavior without claiming that upstream quota or billing behavior is guaranteed.

## Concurrency and hot reload

Selection and primary mutation happen under one process mutex. Cross-generation state commits use a durable generation fence.

Version 0.1.20 stores the generation lock in the stable `state_path.generation.lock` file. Once the append-only `state_path.generation` journal reaches 768 KiB, the next generation I/O compacts it before running its callback; legacy journals up to 16 MiB are recovered and atomically reduced to their last valid monotonic record. The first migration from the older journal-inode lock protocol must use a controlled CPA restart; subsequent same-protocol reloads can use generation ownership normally.

## Warmup boundary

Warmup remains separate from normal scheduling:

- candidates execute one at a time through a pinned auth request;
- warmup never changes the committed normal-traffic primary;
- HTTP success remains pending until Keeper confirms a stable reset anchor;
- 429 uses normal quarantine;
- cyber-policy, abuse, auth, and workspace failures are terminal blocked outcomes and are not automatically retried.

## Failure behavior

| Failure | Behavior |
|---|---|
| Keeper unavailable or stale | keep the current eligible primary; do not infer a quota transition |
| 429 with quota headers | quarantine until reset, then admit one half-open probe |
| 429 without quota headers | bounded probation, then one half-open probe |
| transient CPA candidate suppression | stable request-local provisional fallback |
| confirmed candidate loss | commit the best eligible replacement |
| hard-limited primary, soft-threshold backup | explicitly commit the best soft-threshold backup |
| all candidates hard-exhausted/quarantined | return `Handled=false` |
| mixed or third-party provider set | return `Handled=false` |

## Security boundary

The plugin registers only authenticated Management API routes. It does not place dynamic state, privileged operations, iframes, or host callbacks under the unauthenticated resource route family. It does not modify credentials, authentication methods, model lists, provider routes, or third-party APIs.
