# Local native continuity acceptance

`acceptance.py` starts a patched Windows CPA executable with the built scheduler
DLL and a synthetic, loopback-only quota service and Codex upstream. It uses
temporary configuration and synthetic API keys. It never sends model requests
to a real provider or uses production credentials. CPA's local-model mode is
enabled, and its background HTTP proxy is pointed to the local fixture, which
rejects external CONNECT requests.

The fixture calculates CPA v8.0.4 and v8.0.44 synthetic credential IDs and indexes and
checks the indexes against CPA's own management API. The plugin reads the fake
`auth-files` and `api-call` endpoints, while generation goes through the real
CPA HTTP or WebSocket handler, plugin DLL, scheduler and native Codex executor
to the fake upstream. Thus this checks the native ABI and routing behavior,
but it does not test a real OAuth account, a production quota endpoint, a
deployed proxy, or external clients' idle timeouts.

Run on Windows with Python 3 and the `websocket-client` package, after building
the scheduler DLL and patched CPA executable:

```powershell
python integration\continuity\acceptance.py `
  --cpa output\continuity\cpa-final.exe `
  --plugin output\continuity\codex-quota-scheduler.dll `
  --output output\continuity\acceptance-final.json
```

The script writes binary and harness SHA-256 values with per-case results.
`--case` selects one case, and `--keep` retains the otherwise temporary CPA
configuration and logs for investigation. A skipped or failed case gives a
nonzero exit status. The seven cases are:

- `recovery`: both observed 5-hour quotas are exhausted; an SSE heartbeat
  arrives while upstream attempts remain zero. After the reset and a new
  positive quota observation, the request completes with exactly one attempt.
- `retry429`: an initially usable account returns a synthetic authoritative
  5-hour 429 before output. The same client stream waits without exposing the
  rejection, receives a heartbeat, and completes after a new positive quota
  observation. Exactly two attempts occur, with the same model and input.
- `weekly`: a mixed pool has one exhausted 5-hour account and one weekly-only
  account. A simulated weekly reset causes a new native probe; the request
  selects that weekly-only account and reports `weekly_only_available`.
- `cancel`: after a heartbeat, the client disconnects. A later successful
  quota observation must not start an upstream attempt for that request.
- `timeout`: a 5-second wait budget produces an SSE error after the heartbeat,
  with no upstream attempt; the elapsed request time is checked.
- `websocket`: a WebSocket ping is observed while waiting, before any upstream
  attempt, followed by one completion after a new positive quota observation.
- `ws_cancel`: after a WebSocket ping, the client sends a close frame and
  disconnects. A later positive quota observation must not cause an upstream
  attempt for that request.

`anti_storm.py` adds a separate concurrent acceptance run using the same
real CPA/plugin and isolated fake endpoints. It uses one or two synthetic credentials,
eight concurrent SSE clients, and disables the account concurrency setting so
that it cannot hide duplicate attempts. Its temporary configuration shortens
`fallback_ban` and `half_open_retry_after` to one second; the explicit upstream
`Retry-After` values still set the longer tested deadlines. No real quota or
credentials are used, and production configuration is never changed.

```powershell
python integration\continuity\anti_storm.py `
  --cpa output\continuity\cpa-final.exe `
  --plugin output\continuity\codex-quota-scheduler.dll `
  --output output\continuity\anti-storm.json
```

The script also runs on Linux with the corresponding CPA executable and `.so`.
Its five cases are selectable with `--case`; `--keep` preserves the temporary
fixture directory. The JSON includes hashes, synthetic event times and failure
evidence. `--case core` runs the first four cases, and `--case all` includes the
weekly-only isolation case. All selected cases must pass for exit code zero.

- `retry_after`: after an authoritative synthetic 5-hour 429, the reset occurs
  before an 18-second `Retry-After`. Eight waiters remain open, a positive new
  quota observation arrives, and no additional generation is permitted before
  `Retry-After` expires.
- `quota_backoff`: native quota is initially exhausted without a generation
  429. Eight requests wait while the first post-reset quota probe returns 429
  with a 40-second `Retry-After`; for the following 33 seconds all clients must
  remain pending, quota probes stay at one, and generation stays at zero. This
  crosses the configured 30-second standby refresh interval.
- `half_open`: after an initial 429 with a 1-second `Retry-After`, a short
  reset and a positive post-reset quota observation, CPA v8.0.4/8.0.44 still
  enforces its 10-second native cooldown. The original request and all eight
  contenders must remain pending until that native deadline. After it expires,
  a generation probe is held in the fake upstream. Exactly one probe may run
  while eight clients remain pending. The probe then returns another 429 and
  the generation count must remain unchanged until shortly before its second
  18-second `Retry-After` expires.
- `shared_account`: two distinct synthetic credentials identify the same
  account in both inventory and native quota responses. Both first report 100%
  5-hour usage and then fresh positive quota. Across eight waiting clients each
  credential gets exactly one held generation trial; after both trials return
  429 no credential may retry before its `Retry-After`. Shared workspace IDs
  alone must not collapse separate credentials into one recovery lock.
- `healthy_weekly`: one credential returns an authoritative 5-hour 429. A
  different credential sharing its workspace ID obtains a fresh weekly-only
  quota observation and must serve a real synthetic SSE completion while the
  first credential is still cooling down; the limited credential is not retried.
  The original seed request must receive an SSE heartbeat and remain pending
  after its 429, then complete on the healthy credential with the same input.

These are deterministic local fault-injection tests, not production traffic
or evidence about real upstream service behavior.
