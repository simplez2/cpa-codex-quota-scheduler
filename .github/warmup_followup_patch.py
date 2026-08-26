from pathlib import Path


def replace_once(path, old, new):
    p = Path(path)
    text = p.read_text()
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{path}: expected exactly one match, got {count}: {old[:120]!r}")
    p.write_text(text.replace(old, new, 1))


replace_once(
    "keeper_refresh.go",
    '''\t\tpreviousSnapshot, ok := previous[index]\n\t\tif !ok {\n\t\t\tcontinue\n\t\t}\n\t\tstaleCarry := false\n''',
    '''\t\tpreviousSnapshot, ok := previous[index]\n\t\tif !ok {\n\t\t\tcontinue\n\t\t}\n\t\t// Match mergePartialQuotaSnapshot: an old snapshot outside the outer\n\t\t// freshness envelope is discarded wholesale and therefore cannot leave\n\t\t// a carried sibling that needs recovery.\n\t\tif previousSnapshot.RefreshedAt.IsZero() || now.Before(previousSnapshot.RefreshedAt) || now.Sub(previousSnapshot.RefreshedAt) > staleAfter {\n\t\t\tcontinue\n\t\t}\n\t\tstaleCarry := false\n''',
)
replace_once(
    "keeper_refresh.go",
    '''\t\t\tif window.ResetAt.IsZero() || !now.Before(window.ResetAt) {\n\t\t\t\tcontinue\n\t\t\t}\n''',
    '''\t\t\tif !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {\n\t\t\t\tcontinue\n\t\t\t}\n''',
)

p = Path("warmup_window_cycles_regression_test.go")
text = p.read_text()
append = '''
func TestCarriedStaleWindowWithoutResetRequestsKeeperRefresh(t *testing.T) {
\tnow := time.Now().UTC().Truncate(time.Second)
\tcurrent := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: now, Windows: []quotaWindow{{Class: "5h", Allowed: true, ObservedAt: now}}}}
\tprevious := map[string]quotaSnapshot{"idx": {
\t\tAuthID: "acct", AuthIndex: "idx", RefreshedAt: now,
\t\tWindows: []quotaWindow{{Class: "weekly", Allowed: true, ObservedAt: now.Add(-16 * time.Minute)}},
\t}}
\ttargets := collectCarriedStaleWindowRefreshTargets([]string{"idx"}, current, previous, now, 15*time.Minute)
\tif len(targets) != 1 || targets[0].Reason != "carried_stale_window" {
\t\tt.Fatalf("stale zero-reset carried sibling was not refreshed: %#v", targets)
\t}
}

func TestStaleOuterSnapshotCannotProduceCarriedRefreshTarget(t *testing.T) {
\tnow := time.Now().UTC().Truncate(time.Second)
\tcurrent := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: now, Windows: []quotaWindow{{Class: "5h", Allowed: true, ObservedAt: now}}}}
\tprevious := map[string]quotaSnapshot{"idx": {
\t\tAuthID: "acct", AuthIndex: "idx", RefreshedAt: now.Add(-16 * time.Minute),
\t\tWindows: []quotaWindow{{Class: "weekly", Allowed: true, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: now.Add(-16 * time.Minute)}},
\t}}
\tif targets := collectCarriedStaleWindowRefreshTargets([]string{"idx"}, current, previous, now, 15*time.Minute); len(targets) != 0 {
\t\tt.Fatalf("window from an outer-stale snapshot cannot be carried but requested refresh: %#v", targets)
\t}
}
'''
if "func TestCarriedStaleWindowWithoutResetRequestsKeeperRefresh" in text:
    raise SystemExit("follow-up tests already present")
p.write_text(text + append)
