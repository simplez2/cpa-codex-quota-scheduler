from pathlib import Path
import subprocess


def replace_once(path, old, new):
    p = Path(path)
    text = p.read_text()
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{path}: expected exactly one match, got {count}: {old[:120]!r}")
    p.write_text(text.replace(old, new, 1))


replace_once(
    "warmup.go",
    '''\t\tsnapshot, ok := canonical[strings.TrimSpace(entry.AuthID)]\n\t\tif !ok || !snapshot.RefreshedAt.After(entry.CompletedAt) {\n\t\t\tcontinue\n\t\t}\n\t\tfor _, window := range snapshot.Windows {\n\t\t\tif normalizeWindowClass(window.Class) != normalizeWindowClass(entry.Window) || window.ResetAt.IsZero() ||\n\t\t\t\t!now.Before(window.ResetAt) || quotaWindowHasPlaceholderReset(window, snapshot.RefreshedAt, now) ||\n\t\t\t\t!quotaWindowCycleStarted(window, snapshot.RefreshedAt, now) {\n\t\t\t\tcontinue\n\t\t\t}\n''',
    '''\t\tsnapshot, ok := canonical[strings.TrimSpace(entry.AuthID)]\n\t\tif !ok {\n\t\t\tcontinue\n\t\t}\n\t\tfor _, window := range snapshot.Windows {\n\t\t\tif normalizeWindowClass(window.Class) != normalizeWindowClass(entry.Window) {\n\t\t\t\tcontinue\n\t\t\t}\n\t\t\t// Confirmation is window-scoped. A fresh 5h observation must not\n\t\t\t// make an older carried weekly row look post-warmup (or vice versa).\n\t\t\tobservedAt := window.ObservedAt\n\t\t\tif observedAt.IsZero() || !observedAt.After(entry.CompletedAt) || window.ResetAt.IsZero() ||\n\t\t\t\t!now.Before(window.ResetAt) || quotaWindowHasPlaceholderReset(window, observedAt, now) ||\n\t\t\t\t!quotaWindowCycleStarted(window, observedAt, now) {\n\t\t\t\tcontinue\n\t\t\t}\n''',
)

p = Path("warmup_confirmation_test.go")
text = p.read_text()
if "func TestConfirmPendingWarmupRejectsStaleCarriedWeeklyWindow" not in text:
    text += '''
func TestConfirmPendingWarmupRejectsStaleCarriedWeeklyWindow(t *testing.T) {
\tnow := time.Now().UTC().Truncate(time.Second)
\tcompletedAt := now.Add(-time.Minute)
\tstate := schedulerRuntimeState{warmups: map[string]warmupEntry{
\t\t"acct|weekly": {AuthID: "acct", AuthIndex: "idx", Window: "weekly", CompletedAt: completedAt},
\t}}
\tquotas := map[string]quotaSnapshot{"idx": {
\t\tAuthID: "acct", AuthIndex: "idx", RefreshedAt: completedAt.Add(time.Second),
\t\tWindows: []quotaWindow{{
\t\t\tClass: "weekly", WindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
\t\t\tUsedPercent: 10, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour),
\t\t\tObservedAt: completedAt.Add(-time.Second),
\t\t}},
\t}}
\tif changed := state.confirmPendingWarmups(quotas, now); changed {
\t\tt.Fatal("stale carried weekly row incorrectly confirmed pending warmup")
\t}
\tif entry := state.warmups["acct|weekly"]; !entry.ActivatedAt.IsZero() {
\t\tt.Fatalf("stale weekly row activated pending warmup: %#v", entry)
\t}
}

func TestConfirmPendingWarmupAcceptsFreshMatchingWindow(t *testing.T) {
\tnow := time.Now().UTC().Truncate(time.Second)
\tcompletedAt := now.Add(-time.Minute)
\tresetAt := now.Add(4 * 24 * time.Hour)
\tstate := schedulerRuntimeState{warmups: map[string]warmupEntry{
\t\t"acct|weekly": {AuthID: "acct", AuthIndex: "idx", Window: "weekly", CompletedAt: completedAt},
\t}}
\tquotas := map[string]quotaSnapshot{"idx": {
\t\tAuthID: "acct", AuthIndex: "idx", RefreshedAt: completedAt.Add(2 * time.Second),
\t\tWindows: []quotaWindow{{
\t\t\tClass: "weekly", WindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
\t\t\tUsedPercent: 10, Allowed: true, ResetAt: resetAt,
\t\t\tObservedAt: completedAt.Add(time.Second),
\t\t}},
\t}}
\tif changed := state.confirmPendingWarmups(quotas, now); !changed {
\t\tt.Fatal("fresh post-warmup weekly observation did not confirm pending warmup")
\t}
\tentry := state.warmups["acct|weekly"]
\tif !entry.ActivatedAt.Equal(completedAt) || !entry.ResetAt.Equal(resetAt) || !entry.SuppressUntil.Equal(resetAt) {
\t\tt.Fatalf("fresh weekly confirmation state = %#v", entry)
\t}
}
'''
p.write_text(text)

# Remove the abandoned workflow/script pair from the prior validation attempt.
for path in (
    ".github/warmup_confirm_pending_patch.py",
    ".github/workflows/warmup-confirm-pending-bot.yml",
):
    p = Path(path)
    if p.exists():
        p.unlink()

subprocess.run(["gofmt", "-w", "warmup.go", "warmup_confirmation_test.go"], check=True)
