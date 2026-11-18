package main

import (
	"strings"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

// Strict ordering is retained for serial/legacy selectors. Balanced mode can
// opt into bounded reset weights and limited weekly-only catch-up allocation.
type quotaSelectionPriority struct {
	Tier            int
	Window          string
	WindowRank      int
	DeadlineKind    int // confirmed reset, dormant placeholder, unknown reset
	ResetAt         time.Time
	WeeklyProtected bool
	Weighted        bool
	WeeklyCatchup   bool
	WeeklyUrgent    bool
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
	p.Weighted = balancedResetWeighted(cfg)
	p.WeeklyCatchup = weeklyOnlyCatchup(choice, cfg, now)
	p.WeeklyUrgent = weeklyOnlyUrgent(choice, cfg, now)
	if p.WeeklyCatchup {
		p.WindowRank = windowRankInOrder("5h", cfg.WindowOrder)
	}
	return p
}

// The panel describes placement of new sessions. Established bindings still
// use the ordinary grouping and their existing affinity and recovery rules.
func compareQuotaPlacementPriority(a, b quotaSelectionPriority) int {
	if a.WeeklyUrgent != b.WeeklyUrgent {
		if a.WeeklyUrgent {
			return -1
		}
		return 1
	}
	return compareQuotaPriority(a, b)
}

func compareQuotaPriority(a, b quotaSelectionPriority) int {
	for _, pair := range [][2]int{{a.Tier, b.Tier}, {a.WindowRank, b.WindowRank}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if a.Weighted != b.Weighted {
		if !a.Weighted {
			return -1
		}
		return 1
	}
	if !a.Weighted && a.DeadlineKind != b.DeadlineKind {
		if a.DeadlineKind < b.DeadlineKind {
			return -1
		}
		return 1
	}
	if !a.Weighted && !a.ResetAt.Equal(b.ResetAt) {
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

// Affinity keeps conversations within a quota pool. A binding created while
// using a weekly-only backup must yield when usable, higher-priority 5h quota
// returns. A plan name alone never establishes either quota class.
func quotaAffinityMustYield(current serialCandidate, choices []serialCandidate, cfg pluginConfig) bool {
	if !current.QuotaKnown || current.FiveHourKnown || current.Priority.WeeklyCatchup || current.Priority.WeeklyUrgent || current.flexSprint {
		return false
	}
	for _, choice := range choices {
		if choice.Eligible && choice.QuotaKnown && choice.FiveHourKnown &&
			windowRankInOrder("5h", cfg.WindowOrder) < windowRankInOrder(current.WindowClass, cfg.WindowOrder) {
			return true
		}
	}
	return false
}

// Caller owns s.mu. Use the same host candidate, quota and provider gates as
// routing before giving up an old weekly-only affinity or its queue target.
func (s *schedulerRuntimeState) quotaAffinityMustYieldLocked(current serialCandidate, req pluginapi.SchedulerPickRequest, now time.Time) bool {
	if !current.QuotaKnown || current.FiveHourKnown {
		return false
	}
	for _, candidate := range req.Candidates {
		if strings.TrimSpace(candidate.ID) == current.Candidate.ID || !banStore.schedulable(candidate.ID, now) ||
			s.adqProviderCircuitActiveLockedForConcurrency(candidate, now) {
			continue
		}
		snapshot, found := s.lookupQuotaLocked(candidate.ID, candidateAuthIndex(candidate))
		snapshot = s.serialConservativeQuotaLocked(snapshot, now)
		choice := inspectSerialCandidate(candidate, snapshot, found, s.cfg, now)
		if quotaAffinityMustYield(current, []serialCandidate{choice}, s.cfg) {
			return true
		}
	}
	return false
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
	Rank                       int     `json:"rank"`
	Window                     string  `json:"window"`
	Reason                     string  `json:"reason"`
	ResetAt                    string  `json:"reset_at,omitempty"`
	Cached                     bool    `json:"cached"`
	WeeklyDeadlineUnitsPerHour float64 `json:"weekly_deadline_units_per_hour,omitempty"`
}

func quotaPriorityStatus(choice serialCandidate, cfg pluginConfig, now time.Time) runtimeQuotaPriorityStatus {
	p := quotaPriorityFor(choice, cfg, now)
	out := runtimeQuotaPriorityStatus{Window: p.Window, Cached: !quotaSnapshotFresh(choice.Snapshot, now, cfg.StaleAfter)}
	switch {
	case !choice.QuotaKnown:
		out.Reason = "quota_unknown"
	case !choice.Eligible:
		out.Reason = choice.Reason
	case p.WeeklyUrgent:
		out.Reason = "weekly_only_urgent"
	case p.WeeklyCatchup:
		out.Reason = "weekly_only_catchup"
	case p.Weighted && p.Window == "5h":
		out.Reason = "five_hour_weighted"
		if horizon, known := weeklyResetHorizon(choice, cfg, now); known && horizon <= cfg.WeeklyCatchupWindow && serialBudgetEnabled(cfg) {
			out.Reason = "five_hour_weekly_catchup"
		}
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
