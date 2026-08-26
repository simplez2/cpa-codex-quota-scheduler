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
    "\ttargets = mergeKeeperRefreshTargets(\n\t\ttargets,\n\t\tcollectCarriedStaleWindowRefreshTargets(indexes, quotas, previous, now, cfg.StaleAfter),\n\t)\n",
    "\ttargets = mergeKeeperRefreshTargets(\n\t\ttargets,\n\t\tcollectCarriedStaleWindowRefreshTargets(indexes, quotas, previous, now, cfg.StaleAfter),\n\t\ts.pendingWarmupKeeperRefreshTargets(indexes, quotas),\n\t)\n",
)

replace_once(
    "keeper_refresh_test.go",
    '''\tstatus := state.status()\n\tif status.KeeperRefreshTargets != 0 || status.KeeperRefreshRequests != 1 || status.KeeperRefreshError != "" {\n\t\tt.Fatalf("keeper refresh status = %#v", status)\n\t}\n''',
    '''\tif got := refreshCalls.Load(); got != 2 {\n\t\tt.Fatalf("pending warmup confirmation refresh calls = %d; want 2 total", got)\n\t}\n\tstatus := state.status()\n\tif status.KeeperRefreshTargets != 1 || status.KeeperRefreshRequests != 2 || status.KeeperRefreshError != "" {\n\t\tt.Fatalf("keeper refresh status = %#v", status)\n\t}\n''',
)

p = Path("warmup_window_cycles_regression_test.go")
text = p.read_text()
if "func TestPendingWarmupRequestsFreshKeeperConfirmation" in text:
    raise SystemExit("confirmation tests already present")
text += '''
func TestPendingWarmupRequestsFreshKeeperConfirmation(t *testing.T) {
\tnow := time.Now().UTC().Truncate(time.Second)
\tcompletedAt := now.Add(-time.Minute)
\tstate := schedulerRuntimeState{warmups: map[string]warmupEntry{
\t\t"acct|5h":     {AuthID: "acct", AuthIndex: "idx", Window: "5h", CompletedAt: completedAt},
\t\t"acct|weekly": {AuthID: "acct", AuthIndex: "idx", Window: "weekly", CompletedAt: completedAt},
\t}}
\tcurrent := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: completedAt.Add(-time.Second)}}
\ttargets := state.pendingWarmupKeeperRefreshTargets([]string{"idx"}, current)
\tif len(targets) != 1 || targets[0].AuthIndex != "idx" || targets[0].Reason != "warmup_pending_confirmation" {
\t\tt.Fatalf("pending warmup confirmation targets = %#v", targets)
\t}
}

func TestPostWarmupKeeperObservationDoesNotRequestRedundantConfirmation(t *testing.T) {
\tnow := time.Now().UTC().Truncate(time.Second)
\tcompletedAt := now.Add(-time.Minute)
\tstate := schedulerRuntimeState{warmups: map[string]warmupEntry{
\t\t"acct|5h": {AuthID: "acct", AuthIndex: "idx", Window: "5h", CompletedAt: completedAt},
\t}}
\tcurrent := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: completedAt.Add(time.Second)}}
\tif targets := state.pendingWarmupKeeperRefreshTargets([]string{"idx"}, current); len(targets) != 0 {
\t\tt.Fatalf("post-warmup Keeper observation requested redundant refresh: %#v", targets)
\t}
}

func TestWarmupConfirmationRefreshSkipsActivatedBlockedAndInactiveAuths(t *testing.T) {
\tnow := time.Now().UTC().Truncate(time.Second)
\tcompletedAt := now.Add(-time.Minute)
\tstate := schedulerRuntimeState{warmups: map[string]warmupEntry{
\t\t"active-done|5h":    {AuthID: "active-done", AuthIndex: "done", Window: "5h", CompletedAt: completedAt, ActivatedAt: completedAt},
\t\t"active-blocked|5h": {AuthID: "active-blocked", AuthIndex: "blocked", Window: "5h", CompletedAt: completedAt, Blocked: true},
\t\t"inactive|5h":       {AuthID: "inactive", AuthIndex: "inactive", Window: "5h", CompletedAt: completedAt},
\t}}
\tif targets := state.pendingWarmupKeeperRefreshTargets([]string{"done", "blocked"}, nil); len(targets) != 0 {
\t\tt.Fatalf("non-actionable warmup entries requested confirmation refresh: %#v", targets)
\t}
}
'''
p.write_text(text)
