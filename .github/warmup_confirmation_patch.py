from pathlib import Path


def replace_once(path, old, new):
    p = Path(path)
    text = p.read_text()
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{path}: expected exactly one match, got {count}: {old[:120]!r}")
    p.write_text(text.replace(old, new, 1))


marker = "func (s *schedulerRuntimeState) maybeRequestKeeperQuotaRefresh(ctx context.Context, cfg pluginConfig, password, token string, indexes []string, cache keeperCacheResponse, quotas map[string]quotaSnapshot, now time.Time) error {"
helpers = '''func (s *schedulerRuntimeState) pendingWarmupKeeperRefreshTargets(indexes []string, current map[string]quotaSnapshot) []keeperRefreshTarget {
\tactiveIndexes := make(map[string]struct{}, len(indexes))
\tfor _, rawIndex := range indexes {
\t\tif index := strings.TrimSpace(rawIndex); index != "" {
\t\t\tactiveIndexes[index] = struct{}{}
\t\t}
\n\ts.warmupMu.Lock()
\tentries := make([]warmupEntry, 0, len(s.warmups))
\tfor _, entry := range s.warmups {
\t\tentries = append(entries, entry)
\t}
\ts.warmupMu.Unlock()
\n\ttargets := make([]keeperRefreshTarget, 0)
\tseen := make(map[string]struct{})
\tfor _, entry := range entries {
\t\tif entry.Blocked || entry.CompletedAt.IsZero() || !entry.ActivatedAt.IsZero() {
\t\t\tcontinue
\t\t}
\t\tindex := strings.TrimSpace(entry.AuthIndex)
\t\tif index == "" {
\t\t\tcontinue
\t\t}
\t\tif _, active := activeIndexes[index]; !active {
\t\t\tcontinue
\t\t}
\t\tif _, duplicate := seen[index]; duplicate {
\t\t\tcontinue
\t\t}
\t\tsnapshot, ok := current[index]
\t\tif ok && !snapshot.RefreshedAt.IsZero() && snapshot.RefreshedAt.After(entry.CompletedAt) {
\t\t\t// A post-warmup Keeper observation is already available. The normal
\t\t\t// confirmPendingWarmups pass will either confirm the reset anchor or
\t\t\t// leave the entry under its existing retry/grace semantics.
\t\t\tcontinue
\t\t}
\t\tobservedAt := time.Time{}
\t\tif ok {
\t\t\tobservedAt = snapshot.RefreshedAt
\t\t}
\t\ttargets = append(targets, keeperRefreshTarget{\n\t\t\tAuthIndex: index,\n\t\t\tReason: "warmup_pending_confirmation",\n\t\t\tObservedAt: observedAt,\n\t\t})
\t\tseen[index] = struct{}{}
\t}
\tsort.Slice(targets, func(i, j int) bool { return targets[i].AuthIndex < targets[j].AuthIndex })
\treturn targets
}

'''
replace_once("keeper_refresh.go", marker, helpers + marker)
replace_once(
    "keeper_refresh.go",
    '''\ttargets = mergeKeeperRefreshTargets(\n\t\ttargets,\n\t\tcollectCarriedStaleWindowRefreshTargets(indexes, quotas, previous, now, cfg.StaleAfter),\n\t)\n''',
    '''\ttargets = mergeKeeperRefreshTargets(\n\t\ttargets,\n\t\tcollectCarriedStaleWindowRefreshTargets(indexes, quotas, previous, now, cfg.StaleAfter),\n\t\ts.pendingWarmupKeeperRefreshTargets(indexes, quotas),\n\t)\n''',
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
\t\t"acct|5h": {AuthID: "acct", AuthIndex: "idx", Window: "5h", CompletedAt: completedAt},
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
\t\t"active-done|5h": {AuthID: "active-done", AuthIndex: "done", Window: "5h", CompletedAt: completedAt, ActivatedAt: completedAt},
\t\t"active-blocked|5h": {AuthID: "active-blocked", AuthIndex: "blocked", Window: "5h", CompletedAt: completedAt, Blocked: true},
\t\t"inactive|5h": {AuthID: "inactive", AuthIndex: "inactive", Window: "5h", CompletedAt: completedAt},
\t}}
\tif targets := state.pendingWarmupKeeperRefreshTargets([]string{"done", "blocked"}, nil); len(targets) != 0 {
\t\tt.Fatalf("non-actionable warmup entries requested confirmation refresh: %#v", targets)
\t}
}
'''
p.write_text(text)
