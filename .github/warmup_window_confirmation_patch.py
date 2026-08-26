from pathlib import Path


def replace_once(path, old, new):
    p = Path(path)
    text = p.read_text()
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{path}: expected exactly one match, got {count}: {old[:120]!r}")
    p.write_text(text.replace(old, new, 1))


path = "warmup_window_cycles_regression_test.go"
replace_once(
    path,
    '''\tcurrent := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: completedAt.Add(time.Second)}}\n\tif targets := state.pendingWarmupKeeperRefreshTargets([]string{"idx"}, current); len(targets) != 0 {\n''',
    '''\tobservedAt := completedAt.Add(time.Second)\n\tcurrent := map[string]quotaSnapshot{"idx": {\n\t\tAuthID: "acct", AuthIndex: "idx", RefreshedAt: observedAt,\n\t\tWindows: []quotaWindow{{Class: "5h", ObservedAt: observedAt}},\n\t}}\n\tif targets := state.pendingWarmupKeeperRefreshTargets([]string{"idx"}, current); len(targets) != 0 {\n''',
)

p = Path(path)
text = p.read_text()
if "func TestPostWarmupFiveHourObservationDoesNotConfirmMissingWeeklySibling" in text:
    raise SystemExit("per-window sibling test already present")
text += '''
func TestPostWarmupFiveHourObservationDoesNotConfirmMissingWeeklySibling(t *testing.T) {
\tnow := time.Now().UTC().Truncate(time.Second)
\tcompletedAt := now.Add(-time.Minute)
\tobservedAt := completedAt.Add(time.Second)
\tstate := schedulerRuntimeState{warmups: map[string]warmupEntry{
\t\t"acct|5h":     {AuthID: "acct", AuthIndex: "idx", Window: "5h", CompletedAt: completedAt},
\t\t"acct|weekly": {AuthID: "acct", AuthIndex: "idx", Window: "weekly", CompletedAt: completedAt},
\t}}
\tcurrent := map[string]quotaSnapshot{"idx": {
\t\tAuthID: "acct", AuthIndex: "idx", RefreshedAt: observedAt,
\t\tWindows: []quotaWindow{{Class: "5h", ObservedAt: observedAt}},
\t}}
\ttargets := state.pendingWarmupKeeperRefreshTargets([]string{"idx"}, current)
\tif len(targets) != 1 || targets[0].AuthIndex != "idx" || targets[0].Reason != "warmup_pending_confirmation" {
\t\tt.Fatalf("fresh 5h incorrectly confirmed missing weekly sibling: %#v", targets)
\t}
}
'''
p.write_text(text)
