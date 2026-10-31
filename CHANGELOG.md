# Changelog

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
