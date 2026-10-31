# Changelog

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
