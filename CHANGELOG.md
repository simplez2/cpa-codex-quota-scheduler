# Changelog

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
