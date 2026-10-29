package main

import "time"

// Quota priority is shared by all selectors. A weekly budget can distribute
// equal-deadline work, but cannot spend a weekly-only backup ahead of usable 5h
// quota or make an expiring window wait behind a later reset.
type quotaSelectionPriority struct {
	Tier            int
	Window          string
	WindowRank      int
	DeadlineKind    int // confirmed reset, dormant placeholder, unknown reset
	ResetAt         time.Time
	WeeklyProtected bool
}

func quotaPriorityFor(choice serialCandidate, cfg pluginConfig, now time.Time) quotaSelectionPriority {
	p := quotaSelectionPriority{Window: choice.WindowClass, DeadlineKind: 2, WeeklyProtected: choice.WeeklyProtected}
	if !choice.QuotaKnown {
		p.Tier, p.Window = 1, "unknown"
	} else if !choice.Eligible {
		p.Tier = 2
	}
	p.WindowRank = windowRankInOrder(p.Window, cfg.WindowOrder)
	for _, window := range choice.Snapshot.Windows {
		if normalizeWindowClass(window.Class) != p.Window || (!window.ResetAt.IsZero() && !window.ResetAt.After(now)) {
			continue
		}
		if quotaWindowHasPlaceholderReset(window, choice.Snapshot.RefreshedAt, now) {
			if p.DeadlineKind > 1 {
				p.DeadlineKind = 1
			}
			continue
		}
		if !window.ResetAt.IsZero() && quotaWindowCycleStarted(window, choice.Snapshot.RefreshedAt, now) &&
			(p.ResetAt.IsZero() || window.ResetAt.Before(p.ResetAt)) {
			p.DeadlineKind, p.ResetAt = 0, window.ResetAt
		}
	}
	return p
}

func compareQuotaPriority(a, b quotaSelectionPriority) int {
	for _, pair := range [][2]int{{a.Tier, b.Tier}, {a.WindowRank, b.WindowRank}, {a.DeadlineKind, b.DeadlineKind}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if !a.ResetAt.Equal(b.ResetAt) {
		if a.ResetAt.Before(b.ResetAt) {
			return -1
		}
		return 1
	}
	if a.WeeklyProtected != b.WeeklyProtected {
		if !a.WeeklyProtected {
			return -1
		}
		return 1
	}
	return 0
}

// A rejected atomic reservation advances to the next priority group. It must
// never bypass the reservation for that group or silently fall back to CPA.
func nextQuotaPriorityGroup(choices []serialCandidate, cfg pluginConfig, now time.Time) (group, remaining []serialCandidate) {
	if len(choices) == 0 {
		return nil, nil
	}
	best := quotaPriorityFor(choices[0], cfg, now)
	for _, choice := range choices[1:] {
		if priority := quotaPriorityFor(choice, cfg, now); compareQuotaPriority(priority, best) < 0 {
			best = priority
		}
	}
	for _, choice := range choices {
		if compareQuotaPriority(quotaPriorityFor(choice, cfg, now), best) == 0 {
			group = append(group, choice)
		} else {
			remaining = append(remaining, choice)
		}
	}
	return group, remaining
}

type runtimeQuotaPriorityStatus struct {
	Rank    int    `json:"rank"`
	Window  string `json:"window"`
	Reason  string `json:"reason"`
	ResetAt string `json:"reset_at,omitempty"`
	Cached  bool   `json:"cached"`
}

func quotaPriorityStatus(choice serialCandidate, cfg pluginConfig, now time.Time) runtimeQuotaPriorityStatus {
	p := quotaPriorityFor(choice, cfg, now)
	out := runtimeQuotaPriorityStatus{Window: p.Window, Cached: !quotaSnapshotFresh(choice.Snapshot, now, cfg.StaleAfter)}
	switch {
	case !choice.QuotaKnown:
		out.Reason = "quota_unknown"
	case !choice.Eligible:
		out.Reason = choice.Reason
	case p.Window == "5h" && p.DeadlineKind == 0:
		out.Reason = "five_hour_reset_first"
	case p.Window == "5h" && p.DeadlineKind == 1:
		out.Reason = "five_hour_dormant"
	case p.Window == "5h":
		out.Reason = "five_hour_reset_unknown"
	case poolFiveHourLimitStatus(quotaSnapshot{}, choice.Snapshot, now, cfg.StaleAfter) == "not_reported":
		out.Reason = "weekly_only_backup"
	default:
		out.Reason = "five_hour_unconfirmed"
	}
	if !p.ResetAt.IsZero() {
		out.ResetAt = p.ResetAt.Format(time.RFC3339)
	}
	return out
}
