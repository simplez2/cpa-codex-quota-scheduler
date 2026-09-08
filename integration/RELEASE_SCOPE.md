# v0.2.0 release scope

This release combines the user's preceding requests in this same task: remove
Keeper dependency, implement native CPA quota queries, persist warmup deduplication,
balance weekly allowances, and switch accounts on quota failure without a 5h reserve.
The user subsequently authorized updating GitHub and deploying to 157.254.234.196.

The staged source contains only this plugin, its tests/documentation, deterministic
offline research, and a host patch. The large line count is dominated by 11,809
lines of simulation JSON and the host patch (934 lines). No auth files, runtime
state, private configuration, build directory or binary is included in the source
commit. The deleted Keeper files are replaced by native quota polling and tests.

The host patch targets pristine official CPA v7.2.152 and passes `git apply --check`.
Its SHA-256 is recorded in `host-patch.json`. Unified-diff context lines deliberately
start with a space; the patch file has a Git whitespace attribute to preserve that
format. Source-file `git diff --check` passes separately.

Validation: plugin unit tests, race tests and vet pass. All four modified CPA
execution/handler packages pass their full race suites. Both host and plugin build
on Linux. The built image with the plugin loaded completes HTTP and SSE quota
failover on fake accounts with HTTP 200, no exposed quota error, and zero 5h reserve.
The full CPA suite has one unrelated Home timeout reproduced on pristine upstream;
see README.md. The production change will preserve the prior image and configuration
and recreate only the CPA service after candidate verification.
