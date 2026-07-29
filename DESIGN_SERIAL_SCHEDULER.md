# Serial Fill-First Scheduler Design

## Goal

Keep exactly one globally active Codex credential for normal traffic. Do not distribute different sessions across different accounts. Switch only when the active credential is no longer safe or available.

## State

The scheduler keeps these persisted fields:

- `serial_active_auth_id`
- `serial_selected_at`
- `serial_switches`
- `serial_last_switch_at`
- `serial_last_switch_reason`

The state file also contains the existing 429 quarantine and warmup records. Writes are serialized, fsynced, and atomically renamed with owner-only permissions.

## Request path

For a pure Codex candidate set:

1. Read the current active auth under the scheduler mutex.
2. If that auth is still a CPA candidate, is not quarantined, and no fresh quota window has reached `serial_switch_percent`, return it immediately.
3. Otherwise exclude it and rank the remaining eligible candidates.
4. Select one candidate, persist it as the new global active auth, and return it for every subsequent request.
5. If the selected auth is in probe-ready state, the existing atomic half-open lease permits only one concurrent probe.

No session header or conversation identifier participates in serial selection.

## Switch triggers

The active auth is released or replaced when any of these is observed:

- authoritative or headerless 429;
- a fresh Keeper window reaches the serial threshold;
- an active window is not allowed or limit-reached;
- CPA no longer includes the auth in the candidate list;
- quarantine prevents scheduling.

Stale Keeper data by itself does not cause a switch. This avoids account churn during a Keeper outage. A real 429 remains an authoritative failover signal.

## Candidate ordering

The next auth is selected deterministically:

1. weekly account class;
2. monthly account class;
3. five-hour-only class;
4. unknown Keeper class;
5. active cycle before dormant cycle, when enabled;
6. reset-credit availability, when enabled;
7. higher current used percentage;
8. higher CPA priority;
9. lexicographically stable auth ID.

The active auth is never preempted merely because another candidate later becomes more attractive.

## Concurrent requests

Selection and active-auth mutation happen under one mutex. The first concurrent request claims the auth; every other request observes the same claim. Requests may execute concurrently, but they all use the same account until a switch trigger occurs.

## Warmup

Warmup follows the same active-auth state:

- if no auth has been selected, warmup may atomically claim one deterministic candidate;
- if an active auth exists, only that auth can be warmed;
- if the active auth already has a running cycle, no backup auth is warmed;
- a warmup 429 uses the normal quarantine path and releases the active auth.

This prevents periodic Keeper refreshes from gradually starting every dormant account.

## Failure behavior

| Failure | Behavior |
|---|---|
| Keeper unavailable | keep current auth; deterministic single-auth selection if none exists |
| Partial/stale quota | do not infer a threshold crossing |
| 429 with quota headers | quarantine until reset, then one half-open probe |
| 429 without quota headers | bounded probation, then one half-open probe |
| Current auth removed by CPA | choose the next eligible auth |
| All candidates quarantined/exhausted | return `Handled=false`; CPA host behavior applies |
| Mixed or third-party provider set | return `Handled=false` |

## Security boundary

The plugin registers only authenticated Management API routes. It does not place dynamic state, privileged operations, iframes, or host callbacks under the unauthenticated resource route family.
