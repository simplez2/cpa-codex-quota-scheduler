# CPA v8.0.57 continuity integration

Apply `CLIProxyAPI-v8.0.57-continuity.patch` to official v8.0.57 at commit
`c2f402b45643f0740d70b9010fd7d8d0aab7ef65`. The patch applies the existing
cancellable recovery, SSE heartbeat, WebSocket ping and request-scoped wait
contract to this upstream version. Its checksum is recorded in
`continuity-host-patch-v8.0.57.json`.

The published `ghcr.io/simplez2/cli-proxy-api:latest` channel supports 1Panel
container upgrades while retaining the continuity integration. Its build uses
Go 1.26.10 with CGO and Debian Bookworm for dynamic plugin compatibility.
The publishing workflow requires the affected Go suites, race checks, vet and
synthetic original-request recovery, scheduling, concurrency and reset-credit
acceptance before updating the image tag.

See [1Panel usage](images/README.zh-CN.md) and the existing
[continuity contract](CONTINUITY-v8.0.44.md).
