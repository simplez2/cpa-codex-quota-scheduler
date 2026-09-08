# CPA host quota failover

The scheduler selects accounts and quarantines exhausted credentials. Retrying the
same request before delivering output also requires the accompanying host patch,
based on official CLIProxyAPI v7.2.152 (`c76dfd4`).

Apply `CLIProxyAPI-v7.2.152-quota-failover.patch` to that exact source version and build
the server. Enable these host settings alongside the scheduler configuration:

```yaml
codex:
  stream-bootstrap-buffering: true
max-retry-credentials: 0
```

HTTP/SSE and WebSocket quota rejections before output retry another eligible
credential without exposing the rejected attempt. Repeated handshake metadata does
not commit the stream. WebSocket quota frames followed immediately by connection
closure preserve the downstream socket while retrying. Full, self-contained Codex
WebSocket requests retain soft affinity without forcing an exhausted account.

An explicit account pin, an exhausted pool, a request requiring upstream-only
continuation state, or an error after generated output can still prevent transparent
replay. Do not replay emitted tool calls or pretend a failed stream completed.

Validation includes real executor/manager HTTP and WebSocket tests, same-socket
account handoff, immediate-close races, continuation boundaries, and server build.
The patched server is a custom build based on the official release; it is not the
unchanged official image. `host-patch.json` records the source and patch digest.

Local validation on 2026-09-08: scheduler tests and race tests passed; CPA executor,
auth manager, generic handlers and OpenAI handlers passed their complete race test
suites. Server build passed. The full CPA suite was also attempted: an unrelated
`internal/home/TestEnsureClientsWaitsForPreviousTargetClose` timeout reproduces on
the unmodified v7.2.152 archive. A management test timeout passed when rerun alone.
The full suite must therefore not be described as entirely passing.

Linux release validation also passed against the built image and loaded scheduler:
both an HTTP 429 and a quota failure following 32 SSE handshake events retried a
different fake account, completed with HTTP 200, and exposed no quota error. The
management API confirmed `429_only` and zero 5h reserve. No real model calls were
used for this isolated check.
