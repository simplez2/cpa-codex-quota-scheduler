# Changelog

## 0.3.53 — 2026-11-21

- Include CPA credential and model cooldowns, verified exhausted windows, and pending quota recovery in the account recovery panel. Mark missing or stale host details so they do not look like an empty recovery queue. Label the bulk action as clearing plugin state and only offer host quota cooldown recovery for an eligible credential with fresh native quota.

## 0.3.52 — 2026-11-20

- Refresh a credential's native quota after a likely manual reset-card redemption, even when the credential is idle or in standby. Honor provider backoff and retry briefly if the usage endpoint still reports the old windows. Replay pending changes saved by the previous release.

## 0.3.51 — 2026-11-19

- Bring forward manual card opportunities within the ordinary dual-limited share. Compare the discrete 5h refill schedule and a steady-arrival scenario; count only cards a customer could actually exercise in sequence. Preserve natural weekly deadlines, sticky sessions and admission reserves. The panel distinguishes projected progress from a natively observed weekly reserve. Card redemption settings are unchanged.

## 0.3.50 — 2026-11-19

- Add an opt-in capacity/deadline policy for dual-limited and weekly-only accounts. Compare current 5h stock against later refill opportunities, preserve confirmed natural weekly sprints, and handle an optional uncertain group reset or an already scheduled account-specific reset card without crediting either as supply.

## 0.3.49 — 2026-11-19

- Place flexible urgent weekly work on the earliest spendable reset group.
  Within that group, account for work already assigned to pinned and sticky
  turns when distributing new work. Track service per credential and weekly
  cycle, discount older work, and reconcile usage or cancelled admissions.

## 0.3.48 — 2026-11-19

- On the next unpinned turn after both cache age bands, allow an idle 5h
  binding to move to an available weekly-only credential in the final priority
  window. Protect in-flight work and sources already in their own final weekly
  window. Once selected, prefer the bound urgent weekly credential even when
  5h returns, with normal quota and slot checks. Report cold moves in status.

## 0.3.47 — 2026-11-18

- Within the final weekly priority window, distribute new work by the effective
  weekly quota left per hour until reset. Include the weekly reserve and
  outstanding debits; bound the denominator at 30 minutes. Report the target
  rate per credential in scheduler status. Keep the same admission and affinity rules.

## 0.3.46 — 2026-11-18

- Allow reset-weighted routing to prioritize confirmed weekly-only headroom for
  new sessions in a configurable final 1–24 hours before weekly reset. Keep the
  usual catch-up share outside that window. Existing bindings and weekly reserves
  still apply. An unreservable priority candidate falls back to the ordinary pool.

## 0.3.45 — 2026-11-18

- In reset-weighted balanced mode, give active weekly fallback conversations a
  fixed five-minute grace period on their first deferred handback to usable 5h
  credentials. Keep the existing catch-up share and new-session priority.
  Deferral requires fresh weekly-only telemetry, a free slot, and an atomic
  weekly reservation. Report deferrals in scheduler status.
- Apply the affinity validated by the balanced selector during quota reservation,
  without recreating a binding the selector dropped.

## 0.3.44 — 2026-11-18

- Refresh confirmed weekly-only catch-up candidates on real routing or usage
  activity before standby snapshots lose freshness. Coalesce requests, retain
  independent error backoff, and never invent quota freshness or account usage.
  Idle pools still do not poll for catch-up alone.

## 0.3.43 — 2026-11-18

- Add opt-in balanced reset weighting: share work across usable 5h credentials
  with bounded urgency as weekly reset approaches, rather than restricting all
  allocation to the earliest 5h deadline. Credential expiry does not affect this.
- Allow confirmed weekly-only credentials to participate at a limited combined
  new-session target share in the final 48h before weekly reset (15% by default).
  Preserve full backup use when 5h candidates cannot admit work, existing session
  affinity, atomic reservations and configured weekly handoff boundaries.
- Expose the policy, catch-up horizon and target share in the panel. Retain strict
  deadline ordering as the compatibility default for existing configurations.

## 0.3.42 — 2026-11-16

- Enforce the configured weekly handoff across balanced last-resort selection,
  serial recovery fallback, concurrency admission, and native scheduler fallback.
  A weekly balance at the default 2% boundary cannot be spent merely because
  every other eligible credential is exhausted. Explicit existing-session soft
  continuation remains configurable; new sessions cannot inherit that exception.
- Include configured weekly headroom in ADQ atomic reservations, reject a request
  when every candidate lacks unreserved capacity, and align ADQ pool diagnostics
  with the configured weekly gate. Five-hour zero-reserve behavior is unchanged.
- Stabilize the phase-detection test's weekly anchor across UTC midnight.

## 0.3.41 — 2026-11-16

- Restrict native duplex replay recovery to upstream quota/cooldown errors.
  Keep client access-limit 429 events on the existing connection and preserve
  per-turn authorization checks. The maintained CPA image must pass both quota
  replay and client-authorization regression tests before publishing.

## 0.3.40 — 2026-11-16

- Recover an individual credential's unstarted quota cycle from an obsolete
  successful warmup receipt, without requiring a pool-wide reset quorum. Require
  two spaced native observations with moving full-window anchors; preserve recent
  activations, uncertain outcomes, failures, credential identity and daily budgets.
- Persist the reset confirmation so hot reloads and older cross-generation outcome
  journals cannot restore obsolete warmup suppression. One activation still covers
  both weekly and five-hour windows.
- Actually dispatch due warmup preflight observations before a successful idle
  polling cooldown or header-cache deferral, retaining upstream failure backoff.
- Build release assets with Go 1.26.11, including the standard-library security
  fix required by the vulnerability scan.
- Refresh the maintained 1Panel image integration to CPA v8.0.73. Native duplex
  quota error frames now consult the existing alternative-credential replay
  guard after response.created, instead of bypassing recovery and leaking 429.
  Keep genuine pool exhaustion and request errors visible.

## 0.3.39 — 2026-11-03

- Present reset credits in a full-width account summary with independent official
  and plan-supported counts, the next expiry and automatic-use status. Keep
  complete expiry details and credential identity details expandable; preserve
  expansion and keyboard focus across refreshes and filtering.
- Include reproducible continuity integration for CPA v8.0.52 and selected-credit
  integration for Keeper v2.0.3, preserving the newer Claude grant flow.

## 0.3.38 — 2026-11-02

- Match CPA's native reset eligibility: use fresh available, unexpired,
  plan-supported reset cards instead of blocking on a zero or missing usage
  applicability hint. Preserve expiry timing, identity rechecks and durable
  duplicate protection. Display plan-supported card counts without claiming
  that the usage hint is a redemption permission.
- Stop background activation when weekly quota reaches the configured handoff
  threshold. Refresh stale due-activation preflights before a successful idle
  polling cooldown, while retaining failure backoff and warmup budgets.
- Immediately request a native quota observation after a completed warmup so
  confirmation does not wait for the idle polling cooldown.

## 0.3.37 — 2026-11-02

- Use x capacity units throughout weekly/5h balances, details, editors and the
  status API. Display remaining capacity as e.g. 2.5x instead of Team equivalents.
- Align reporting defaults and plan capacity priors to 1x, 10x and 20x:
  Pro 5x and Team Premium now use 10x, and Pro 20x uses 20x. Preserve explicit
  custom reporting weights and keep weekly/5h reporting independently configurable.

## 0.3.36 — 2026-11-02

- Keep automatic weekly-only fallback affinity and its concurrency queue target
  behind usable higher-priority 5h credentials, including after Pro/Premium quota
  changes. Preserve affinity within the 5h pool and explicit CPA account pins.
- Admit confirmed weekly-only accounts through atomic weekly reservations and
  settlement, without inventing 5h capacity or bypassing weekly admission gates.
- Exclude an account's 5h capacity and remaining balance when its backing weekly
  quota reaches the configured handoff threshold or a hard limit. Show blocked
  and unconfirmed backing separately, and re-include only after a usable observation.

## 0.3.35 — 2026-11-02

- Unify weekly bootstrap and subsequent five-hour rotation under one activation
  switch. A generation jointly starts unstarted windows; first-round clustering
  is allowed, later activation staggers best effort, and real traffic wins.
- Scope foreground warmup protection to the credential, preventing an active
  sibling from starving the whole pool. Advance idle activation when observed
  demand cannot bridge the wait; preserve budgets, backoff and duplicate guards.
- Connect cadence-aware native-quota forecasts without weakening admission
  reserves. Represent unknown demand honestly, evaluate coverage along confirmed
  reset events, and cap future refills by remaining weekly quota.
- Protect future full-width sources before same-tier fair-share tie breaking;
  retain session affinity and five-hour/deadline placement priorities. Confirmed
  weekly-only backups no longer appear as five-hour exhausted or add fake width.
- Keep seven days / 256 activation outcomes independently of live deduplication
  state; combine sibling windows from one actual call in the panel.

## 0.3.34 — 2026-11-01

- Dismiss successful panel notifications automatically after three seconds,
  including the saved-and-applied confirmation. New messages replace the prior
  dismissal timer so an earlier success cannot hide a newer error. Clear the
  timer when returning to login.

## 0.3.33 — 2026-11-01

- Recognize nested native `rate_limit_reset_credits.available_count` and keep
  backward compatibility with the old flat count. Distinguish total credits,
  current applicability, explicit zero and missing optional metadata.
- Read the official reset-credit list and display each available credit's exact
  expiry in GMT+8, cache freshness and independent automatic-use status.
- Add default-off `reset_credits_auto_use` and a configurable pre-expiry window
  (default 10 minutes). Verify identity, plan support, current eligibility and
  consumed quota before any redemption; custom quota adapters stay read-only.
- Persist a generation-fenced redemption intent and native request UUID before
  dispatch. Never replay an uncertain write after a timeout or hot reload. Retry
  read verification of acknowledged redemptions without spending another card.
- Verify fresh quota before targeted native cooldown recovery. Keep unrelated or
  newer local bans and preserve existing routing, affinity and concurrency rules.
- Test the actual nested/list schemas and simulated redemptions, including
  unknown outcomes, expiry boundaries, manual consumption, reloads and backoff.
  Real credits are not consumed by the test suite.

## 0.3.32 — 2026-10-31

- Preserve v0.3.31 scheduling: usable 5h quota first, earlier confirmed resets
  first, weekly-only fallback, existing eligible session affinity, concurrency
  admission and conservative quota recovery.
- Include MIT licensing, attribution, permission requirements, Chinese
  installation instructions and the example configuration in every platform ZIP.
- Validate the actual ELF/PE/Mach-O platform, archive contents, library digest,
  version and source commit before publishing. Publish `release-verification.json`
  alongside the four archives and `checksums.txt`.
- Add Chinese project documentation and remove the obsolete single-version
  deployment claim from the main README. Clarify host patch requirements,
  default-enabled expiry repair and proxy timeout diagnostics.
- This release does not include workstation gateway settings, personal SSH
  access or production credentials. Publication does not hot-load a running CPA.

## 0.3.31 — 2026-10-29

- Share quota priority across balanced, serial, legacy, shadow and enforce modes:
  spend usable 5h quota before weekly-only backups, then prefer earlier resets.
- Keep native admission and reservations effective when advancing to another
  priority group; preserve eligible sticky conversations.
- Report official reset credits independently from local observed reset cycles,
  and distinguish dormant, cached, unknown and weekly-only observations.

## 0.3.30 — 2026-10-28

- Reconcile native credential read/repair permissions, registry and source
  versions, supported host integrations and cross-platform release checks.
- Retain bounded continuity waiting, weekly-only recovery and anti-storm controls.

Earlier development details and host-specific validation limits remain in the
repository's integration and audit documents.
