# CPA v8.0.4 continuity host patch

`CLIProxyAPI-v8.0.4-continuity.patch` applies to official CLIProxyAPI v8.0.4,
commit `d33f63f8e3d98428440ebca5a5b6a981a61ff71e`. Verify its SHA-256 against
`continuity-host-patch.json`, apply it to a clean copy of that exact revision,
and build CPA. The result is a custom host, not the unchanged official image.
This is a separate patch from the v7.2.152 quota-failover patch; do not apply
one to the other version. A different CPA version requires a fresh port and
validation.

```sh
git checkout --detach d33f63f8e3d98428440ebca5a5b6a981a61ff71e
git apply --check /path/to/CLIProxyAPI-v8.0.4-continuity.patch
git apply /path/to/CLIProxyAPI-v8.0.4-continuity.patch
go test ./sdk/cliproxy/auth ./internal/pluginhost ./sdk/api/handlers ./sdk/api/handlers/openai
go build -o CLIProxyAPI ./cmd/server
```

Load a scheduler that advertises `scheduler_continuity` and enable its
continuity settings. The host asks the plugin for a nonblocking assessment,
then uses the original request context to wait and rebuilds eligible
credentials after every wake-up. The first configured maximum wait establishes
a single deadline for that execution and its bootstrap retries; later settings
cannot extend it. A shorter configured maximum is applied on recheck. Cancellation
stops the wait and plugin reload/disable is observed on the next recheck. The
original CPA credential filter is applied again before upstream dispatch.

For Codex streams, a confirmed quota 429 before any generated output can enter
the same gate. To recognize a quota rejection that follows initial handshake
frames rather than generated output, use the host's bootstrap buffering setting.
Set nonstream keepalive if non-stream responses must survive idle proxy timeouts.
These are excerpts to merge into an existing v8 configuration:

```yaml
routing:
  retry:
    max-retry-credentials: 0
requests:
  nonstream-keepalive-interval: 15
  streaming:
    keepalive-seconds: 15
oauth:
  providers:
    codex:
      stream-bootstrap-buffering: true
```

The host sends SSE comments or WebSocket Ping frames while waiting. Once an SSE
comment is sent the HTTP status and headers have been committed, so a subsequent
failure is reported as an SSE error. Streaming client, intermediary, and absolute
request deadlines still apply, and connection-scoped upstream continuation or an
error after output cannot be transparently replayed. Home routing does not use the
legacy scheduler gate. A missing or unconfirmed quota observation is never treated
as proof that a 5h limit has recovered. Real overnight operation requires testing
the actual client and proxy path as well as deploying the custom host and plugin.

The early heartbeat path is enabled only when the model registry resolves the
request exclusively to Codex and the active scheduler advertises continuity.
Dynamic model-router overrides and mixed-provider routes are outside this heartbeat
integration; verify their transport separately.

Local validation: the four affected Go package suites passed and a Windows server
build succeeded. The four suites also passed `go test -race -gcflags=all=-d=checkptr=0`;
an existing Windows loader test fails default race checkptr on an unchanged clean
v8.0.4 checkout. `go vet -unsafeptr=false` passed for those packages. Ordinary
Windows `go vet` reports existing unsafe-pointer warnings in the unmodified
`internal/pluginhost/loader_windows.go` and its tests. The patch was checked and
applied to a fresh checkout of the exact base commit. No production deployment is
implied by these local checks.
