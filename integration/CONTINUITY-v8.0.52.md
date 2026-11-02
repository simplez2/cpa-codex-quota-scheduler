# CPA v8.0.52 continuity integration

Apply `CLIProxyAPI-v8.0.52-continuity.patch` to official CLIProxyAPI v8.0.52
at commit `6e6afab99a1939525ecb6bb1f0873c8e7a9df877`. Verify its SHA-256
against `continuity-host-patch-v8.0.52.json`. Build with Go 1.26.10 and CGO
enabled to retain dynamic plugin support.

```sh
git checkout --detach 6e6afab99a1939525ecb6bb1f0873c8e7a9df877
git apply --check /path/to/CLIProxyAPI-v8.0.52-continuity.patch
git apply /path/to/CLIProxyAPI-v8.0.52-continuity.patch
go test ./internal/pluginhost ./sdk/cliproxy/auth ./sdk/api/handlers ./sdk/api/handlers/openai
go build -o CLIProxyAPI ./cmd/server
```

This preserves the v8.0.44 integration's cancellable pre-admission wait,
candidate reassessment, request-scoped wait budget, SSE heartbeats and
WebSocket pings, while retaining the newer upstream plugin capabilities.
See [the original contract and configuration](CONTINUITY-v8.0.44.md) and
[the synthetic acceptance fixtures](continuity/README.md).

Linux tests and vet passed for the four affected packages. The complete
Windows run retained the upstream `TestResponsesInterruptStopsHTTPUpstream`
timing failure; the four suites passed with only that test excluded. Windows
vet also reports existing unsafe.Pointer uses in the native plugin loader.
These qualifications do not apply to the successful Linux checks.

For production upgrades, retain the old image, configuration, auth/state,
reset-credit journal and Keeper database snapshot. Queue new ingress and drain
active/waiting requests before replacing CPA. Keep existing network addresses
when host-side TCP bridges refer to them. Verify all plugins, original-request
continuity, a real stream and Keeper collection after cutover.
