# CPA v8.0.44 continuity host patch

`CLIProxyAPI-v8.0.44-continuity.patch` applies to official CLIProxyAPI
v8.0.44 at commit `1f917e77c4562623aa49e9804d999524151a2f96`. Verify the
patch SHA-256 against `continuity-host-patch-v8.0.44.json`, apply it to a clean
copy of that revision, and build using Go 1.26.10 or later. The result is a
custom host rather than the unchanged official image. The v8.0.4 and v7.2.152
patches have different bases and must not be applied to v8.0.44.

```sh
git checkout --detach 1f917e77c4562623aa49e9804d999524151a2f96
git apply --check /path/to/CLIProxyAPI-v8.0.44-continuity.patch
git apply /path/to/CLIProxyAPI-v8.0.44-continuity.patch
go test ./sdk/cliproxy/auth ./internal/pluginhost ./sdk/api/handlers ./sdk/api/handlers/openai
go build -o CLIProxyAPI ./cmd/server
```

Load a scheduler advertising `scheduler_continuity` and enable its continuity
settings. The host reassesses eligible Codex credentials during a cancellable
wait, retains one request lifecycle ID and one bounded wait budget across
bootstrap retries, and permits a rejected 429 attempt to wait only after the
scheduler confirms five-hour exhaustion. The existing credential filter is
applied again before dispatch. No generated output is replayed.

To try all eligible credentials and allow safe bootstrap retry before output,
merge the following into the existing v8 configuration. Bootstrap buffering
holds initial handshake frames so a later quota rejection can be detected
before the response is exposed. The configured client and proxy timeouts still
apply.

```yaml
routing:
  retry:
    max-retry-credentials: 0
requests:
  nonstream-keepalive-interval: 15
  streaming:
    keepalive-seconds: 15
    bootstrap-retries: 1
upstream:
  codex:
    stream-bootstrap-buffering: true
```

During a confirmed continuity wait, Responses SSE sends comment heartbeats and
Responses WebSocket sends Ping frames. Once an SSE heartbeat commits headers,
subsequent failure is delivered as an SSE error. Home routing does not use this
gate. Early heartbeat coverage requires an exclusively Codex model-registry
route; dynamic model-router overrides and mixed-provider routes need separate
transport validation. Connection-scoped upstream continuation cannot be safely
replayed after an error or after generated output. A missing or unconfirmed
quota observation does not establish that a five-hour limit has recovered.

The native local acceptance fixture is described in
[`continuity/README.md`](continuity/README.md). The seven Windows loopback
scenarios passed with the v8.0.44 host and scheduler plugin: recovery, 429
retry, weekly recovery, SSE cancellation, timeout, WebSocket heartbeat, and
WebSocket cancellation. Those checks use synthetic upstreams and credentials;
they do not establish behavior for a real client, proxy, account or deployed
service. The Windows run used CPA SHA-256
`a9df6ea4b32f814c5609f30510075c0c9cd338321514dd07085d0ef247b6c36e`,
plugin SHA-256
`3c3be86dc713279c16b4ea50d50aab5db0768568856f3543fa5aaf9e01e4ba9f`,
and the then-current fixture SHA-256
`46b4d8432627476305278386b7117ce8df4ed8fb3cf44a4ef75e8b11600db7dc`.

On Windows, the four affected package suites passed normally when excluding
`TestResponsesInterruptStopsHTTPUpstream`, whose immediate cancellation
assertion also failed intermittently on the unmodified v8.0.44 baseline. The
four suites passed with `-race -gcflags=all=-d=checkptr=0` when also excluding
`TestIssue6332ReasoningReplayTwoTurns`; its Gin request reuse race was reproduced
on that baseline as well. These are excluded-test results, not a clean full
suite. The same race remains an upstream risk and has not been repaired by this
integration. The default Windows race run also hit a checkptr failure in the
unmodified v8.0.44 plugin loader; the results above disable that check only.

An additional Windows CPA build with `-race` and `checkptr` disabled passed
the recovery, 429 retry, WebSocket heartbeat and WebSocket cancellation
synthetic scenarios with the scheduler enabled; no race reports appeared in
those runs. This does not establish race freedom for other paths. These four
runs used fixture SHA-256
`7c5f089e314680095e28fe25b5714fa3bb3b2b6d666c6e15e497b184dc617e6b`.
