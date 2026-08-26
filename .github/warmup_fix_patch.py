from pathlib import Path


def replace_once(path, old, new):
    p = Path(path)
    text = p.read_text()
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{path}: expected exactly one match, got {count}: {old[:120]!r}")
    p.write_text(text.replace(old, new, 1))


# Keeper primary/secondary labels are positional. A recognized duration is the
# authoritative cycle class, matching the response-header path.
replace_once(
    "runtime.go",
    '''\tfor _, row := range response.Quota {\n\t\tclass := normalizeWindowClass(row.Label)\n\t\tseconds := int64(0)\n\t\tif row.Window != nil && row.Window.Seconds != nil {\n\t\t\tseconds = *row.Window.Seconds\n\t\t}\n\t\tif class == "" {\n\t\t\tclass = windowClassFromSeconds(seconds)\n\t\t}\n''',
    '''\tfor _, row := range response.Quota {\n\t\tseconds := int64(0)\n\t\tif row.Window != nil && row.Window.Seconds != nil {\n\t\t\tseconds = *row.Window.Seconds\n\t\t}\n\t\t// Keeper's primary/secondary labels describe positions, not stable\n\t\t// quota classes. Weekly-only and monthly-only plans can place their\n\t\t// long window in the primary slot, so a recognized duration wins.\n\t\tclass := windowClassFromSeconds(seconds)\n\t\tif class == "" {\n\t\t\tclass = normalizeWindowClass(row.Label)\n\t\t}\n''',
)

# resetAfterSeconds is relative to the Keeper observation that produced the
# cache row, not to the later time when this plugin reads /quota/cache.
replace_once(
    "runtime.go",
    '''\t\tresetAt := parseKeeperTime(row.ResetAt)\n\t\tif resetAt.IsZero() && row.ResetAfterSeconds != nil && *row.ResetAfterSeconds > 0 {\n\t\t\tresetAt = now.Add(time.Duration(*row.ResetAfterSeconds) * time.Second)\n\t\t}\n''',
    '''\t\tresetAt := parseKeeperTime(row.ResetAt)\n\t\tif resetAt.IsZero() && row.ResetAfterSeconds != nil && *row.ResetAfterSeconds > 0 {\n\t\t\tresetAnchor := refreshedAt\n\t\t\tif resetAnchor.IsZero() {\n\t\t\t\tresetAnchor = now\n\t\t\t}\n\t\t\tresetAt = resetAnchor.Add(time.Duration(*row.ResetAfterSeconds) * time.Second)\n\t\t}\n''',
)

# A moving placeholder should be approximately one full window. Do not accept
# arbitrary oversized resetAfter values as an unstarted cycle.
replace_once(
    "warmup.go",
    '''\tif window.ResetAfterSecondsKnown {\n\t\treturn window.ResetAfterSeconds >= window.WindowSeconds-toleranceSeconds\n\t}\n''',
    '''\tif window.ResetAfterSecondsKnown {\n\t\tdelta := window.ResetAfterSeconds - window.WindowSeconds\n\t\tif delta < 0 {\n\t\t\tdelta = -delta\n\t\t}\n\t\treturn delta <= toleranceSeconds\n\t}\n''',
)

# If a fresh partial cache response omits a recognized sibling that would be
# carried from runtime state with a stale per-window observation, actively ask
# Keeper to refresh that auth before warmup. The existing warmupSnapshotFresh
# gate remains fail-closed and will not send a model request until all
# recognized windows are fresh.
marker = "func (s *schedulerRuntimeState) maybeRequestKeeperQuotaRefresh(ctx context.Context, cfg pluginConfig, password, token string, indexes []string, cache keeperCacheResponse, quotas map[string]quotaSnapshot, now time.Time) error {"
helpers = '''func collectCarriedStaleWindowRefreshTargets(indexes []string, current, previous map[string]quotaSnapshot, now time.Time, staleAfter time.Duration) []keeperRefreshTarget {
\tif staleAfter <= 0 {
\t\tstaleAfter = 15 * time.Minute
\t}
\ttargets := make([]keeperRefreshTarget, 0)
\tfor _, rawIndex := range indexes {
\t\tindex := strings.TrimSpace(rawIndex)
\t\tif index == "" {
\t\t\tcontinue
\t\t}
\t\tcurrentSnapshot, ok := current[index]
\t\tif !ok {
\t\t\t// A completely missing cache item is already handled by the normal
\t\t\t// cache-recovery collector.
\t\t\tcontinue
\t\t}
\t\tpresent := make(map[string]struct{}, len(currentSnapshot.Windows))
\t\tfor _, window := range currentSnapshot.Windows {
\t\t\tif class := normalizeWindowClass(window.Class); class != "" {
\t\t\t\tpresent[class] = struct{}{}
\t\t\t}
\t\t}
\t\tpreviousSnapshot, ok := previous[index]
\t\tif !ok {
\t\t\tcontinue
\t\t}
\t\tstaleCarry := false
\t\toldestObservedAt := time.Time{}
\t\tfor _, window := range previousSnapshot.Windows {
\t\t\tclass := normalizeWindowClass(window.Class)
\t\t\tif class == "" {
\t\t\t\tcontinue
\t\t\t}
\t\t\tif _, exists := present[class]; exists {
\t\t\t\tcontinue
\t\t\t}
\t\t\t// Keep this exactly aligned with mergePartialQuotaSnapshot: only a
\t\t\t// missing window whose old reset is still in the future would be
\t\t\t// carried into the new runtime snapshot.
\t\t\tif window.ResetAt.IsZero() || !now.Before(window.ResetAt) {
\t\t\t\tcontinue
\t\t\t}
\t\t\tobservedAt := window.ObservedAt
\t\t\tif observedAt.IsZero() {
\t\t\t\tobservedAt = previousSnapshot.RefreshedAt
\t\t\t}
\t\t\tif !observedAt.IsZero() && !now.Before(observedAt) && now.Sub(observedAt) <= staleAfter {
\t\t\t\tcontinue
\t\t\t}
\t\t\tstaleCarry = true
\t\t\tif oldestObservedAt.IsZero() || (!observedAt.IsZero() && observedAt.Before(oldestObservedAt)) {
\t\t\t\toldestObservedAt = observedAt
\t\t\t}
\t\t}
\t\tif staleCarry {
\t\t\ttargets = append(targets, keeperRefreshTarget{\n\t\t\t\tAuthIndex: index,\n\t\t\t\tReason: "carried_stale_window",\n\t\t\t\tObservedAt: oldestObservedAt,\n\t\t\t})
\t\t}
\t}
\treturn targets
}

func mergeKeeperRefreshTargets(groups ...[]keeperRefreshTarget) []keeperRefreshTarget {
\tbyIndex := make(map[string]keeperRefreshTarget)
\tfor _, group := range groups {
\t\tfor _, target := range group {
\t\t\tindex := strings.TrimSpace(target.AuthIndex)
\t\t\tif index == "" {
\t\t\t\tcontinue
\t\t\t}
\t\t\tif _, exists := byIndex[index]; exists {
\t\t\t\tcontinue
\t\t\t}
\t\t\ttarget.AuthIndex = index
\t\t\tbyIndex[index] = target
\t\t}
\t}
\tindexes := make([]string, 0, len(byIndex))
\tfor index := range byIndex {
\t\tindexes = append(indexes, index)
\t}
\tsort.Strings(indexes)
\tout := make([]keeperRefreshTarget, 0, len(indexes))
\tfor _, index := range indexes {
\t\tout = append(out, byIndex[index])
\t}
\treturn out
}

'''
replace_once("keeper_refresh.go", marker, helpers + marker)
replace_once(
    "keeper_refresh.go",
    '''\ttargets := collectKeeperRefreshTargets(indexes, cache, quotas, now, cfg.StaleAfter)\n\tif len(targets) == 0 {\n''',
    '''\ttargets := collectKeeperRefreshTargets(indexes, cache, quotas, now, cfg.StaleAfter)\n\ts.mu.RLock()\n\tprevious := make(map[string]quotaSnapshot, len(s.quotas))\n\tfor key, snapshot := range s.quotas {\n\t\tprevious[key] = snapshot\n\t}\n\ts.mu.RUnlock()\n\ttargets = mergeKeeperRefreshTargets(\n\t\ttargets,\n\t\tcollectCarriedStaleWindowRefreshTargets(indexes, quotas, previous, now, cfg.StaleAfter),\n\t)\n\tif len(targets) == 0 {\n''',
)

Path("warmup_window_cycles_regression_test.go").write_text('''package main

import (
\t"testing"
\t"time"
)

func TestNormalizeQuotaSnapshotUsesDurationOverPositionalLabel(t *testing.T) {
\tnow := time.Now().UTC().Truncate(time.Second)
\tzero := 0.0
\tallowed := true
\ttests := []struct {
\t\tname string
\t\tseconds int64
\t\twant string
\t}{
\t\t{name: "weekly-only-primary", seconds: int64((7 * 24 * time.Hour).Seconds()), want: "weekly"},
\t\t{name: "monthly-only-primary", seconds: int64((30 * 24 * time.Hour).Seconds()), want: "monthly"},
\t}
\tfor _, tc := range tests {
\t\tt.Run(tc.name, func(t *testing.T) {
\t\t\tseconds := tc.seconds
\t\t\tresponse := keeperCheckResponse{Quota: []keeperQuotaRow{{
\t\t\t\tLabel: "primary", UsedPercent: &zero, Allowed: &allowed,
\t\t\t\tWindow: &keeperQuotaWindow{Seconds: &seconds},
\t\t\t}}}
\t\t\tsnapshot := normalizeQuotaSnapshot("idx", "acct", response, now, now)
\t\t\tif len(snapshot.Windows) != 1 || snapshot.Windows[0].Class != tc.want {
\t\t\t\tt.Fatalf("normalized positional window = %#v; want class %q", snapshot.Windows, tc.want)
\t\t\t}
\t\t})
\t}
}

func TestNormalizeQuotaSnapshotFallsBackToExplicitLabelWithoutDuration(t *testing.T) {
\tnow := time.Now().UTC().Truncate(time.Second)
\tzero := 0.0
\tallowed := true
\tresponse := keeperCheckResponse{Quota: []keeperQuotaRow{{Label: "weekly", UsedPercent: &zero, Allowed: &allowed}}}
\tsnapshot := normalizeQuotaSnapshot("idx", "acct", response, now, now)
\tif len(snapshot.Windows) != 1 || snapshot.Windows[0].Class != "weekly" {
\t\tt.Fatalf("explicit label fallback = %#v", snapshot.Windows)
\t}
}

func TestNormalizeQuotaSnapshotAnchorsResetAfterToKeeperObservation(t *testing.T) {
\tnow := time.Now().UTC().Truncate(time.Second)
\tobservedAt := now.Add(-10 * time.Minute)
\tzero := 0.0
\tallowed := true
\tfive := int64((5 * time.Hour).Seconds())
\tremaining := int64((4*time.Hour + 30*time.Minute).Seconds())
\tresponse := keeperCheckResponse{Quota: []keeperQuotaRow{{
\t\tLabel: "primary", UsedPercent: &zero, Allowed: &allowed,
\t\tWindow: &keeperQuotaWindow{Seconds: &five}, ResetAfterSeconds: &remaining,
\t}}}
\tsnapshot := normalizeQuotaSnapshot("idx", "acct", response, observedAt, now)
\tif len(snapshot.Windows) != 1 {
\t\tt.Fatalf("normalized windows = %#v", snapshot.Windows)
\t}
\twant := observedAt.Add(time.Duration(remaining) * time.Second)
\tif !snapshot.Windows[0].ResetAt.Equal(want) {
\t\tt.Fatalf("reset_at=%v want Keeper-observation anchor %v", snapshot.Windows[0].ResetAt, want)
\t}
}

func TestCarriedStaleWindowRequestsKeeperRefresh(t *testing.T) {
\tnow := time.Now().UTC().Truncate(time.Second)
\tcurrent := map[string]quotaSnapshot{"idx": {
\t\tAuthID: "acct", AuthIndex: "idx", RefreshedAt: now,
\t\tWindows: []quotaWindow{{Class: "5h", Allowed: true, ObservedAt: now}},
\t}}
\tprevious := map[string]quotaSnapshot{"idx": {
\t\tAuthID: "acct", AuthIndex: "idx", RefreshedAt: now,
\t\tWindows: []quotaWindow{
\t\t\t{Class: "5h", Allowed: true, ObservedAt: now},
\t\t\t{Class: "weekly", Allowed: true, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: now.Add(-16 * time.Minute)},
\t\t},
\t}}
\ttargets := collectCarriedStaleWindowRefreshTargets([]string{"idx"}, current, previous, now, 15*time.Minute)
\tif len(targets) != 1 || targets[0].AuthIndex != "idx" || targets[0].Reason != "carried_stale_window" {
\t\tt.Fatalf("carried stale refresh targets = %#v", targets)
\t}
}

func TestFreshCarriedWindowDoesNotRequestKeeperRefresh(t *testing.T) {
\tnow := time.Now().UTC().Truncate(time.Second)
\tcurrent := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: now, Windows: []quotaWindow{{Class: "5h", Allowed: true, ObservedAt: now}}}}
\tprevious := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: now, Windows: []quotaWindow{{Class: "weekly", Allowed: true, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: now.Add(-5 * time.Minute)}}}}
\tif targets := collectCarriedStaleWindowRefreshTargets([]string{"idx"}, current, previous, now, 15*time.Minute); len(targets) != 0 {
\t\tt.Fatalf("fresh carried sibling requested refresh: %#v", targets)
\t}
}

func TestExpiredMissingWindowDoesNotRequestKeeperRefresh(t *testing.T) {
\tnow := time.Now().UTC().Truncate(time.Second)
\tcurrent := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: now, Windows: []quotaWindow{{Class: "5h", Allowed: true, ObservedAt: now}}}}
\tprevious := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: now, Windows: []quotaWindow{{Class: "weekly", Allowed: true, ResetAt: now.Add(-time.Minute), ObservedAt: now.Add(-16 * time.Minute)}}}}
\tif targets := collectCarriedStaleWindowRefreshTargets([]string{"idx"}, current, previous, now, 15*time.Minute); len(targets) != 0 {
\t\tt.Fatalf("expired non-carried sibling requested refresh: %#v", targets)
\t}
}

func TestKeeperRefreshTargetMergeDeduplicatesCarriedWindow(t *testing.T) {
\tbase := []keeperRefreshTarget{{AuthIndex: "idx", Reason: "stale"}}
\tcarried := []keeperRefreshTarget{{AuthIndex: "idx", Reason: "carried_stale_window"}}
\tmerged := mergeKeeperRefreshTargets(base, carried)
\tif len(merged) != 1 || merged[0].Reason != "stale" {
\t\tt.Fatalf("merged refresh targets = %#v", merged)
\t}
}

func TestPlaceholderResetRequiresApproximatelyFullCycle(t *testing.T) {
\tnow := time.Now().UTC()
\tseconds := int64((5 * time.Hour).Seconds())
\tbase := quotaWindow{Class: "5h", WindowSeconds: seconds, ResetAfterSecondsKnown: true, Allowed: true, ResetAt: now.Add(5 * time.Hour), ObservedAt: now}
\twithin := base
\twithin.ResetAfterSeconds = seconds - 2
\tif !quotaWindowHasPlaceholderReset(within, now, now) {
\t\tt.Fatalf("near-full resetAfter was not accepted as placeholder: %#v", within)
\t}
\toversized := base
\toversized.ResetAfterSeconds = seconds + 60
\toversized.ResetAt = now.Add(5*time.Hour + time.Minute)
\tif quotaWindowHasPlaceholderReset(oversized, now, now) {
\t\tt.Fatalf("oversized resetAfter was accepted as placeholder: %#v", oversized)
\t}
}
''')
