package main

import (
	"math"
	"time"
)

// normalizeQuotaWindowSet keeps one authoritative row per recognized quota
// class. CPA can briefly expose duplicate primary/secondary rows while a reset
// or header overlay is converging; allowing both rows into routing makes the
// result depend on slice order.
func normalizeQuotaWindowSet(windows []quotaWindow, fallbackObservedAt, now time.Time, staleAfter time.Duration) []quotaWindow {
	if len(windows) < 2 {
		out := append([]quotaWindow(nil), windows...)
		for index := range out {
			if class := normalizeWindowClass(out[index].Class); class != "" {
				out[index].Class = class
			}
			if out[index].ObservedAt.IsZero() {
				out[index].ObservedAt = fallbackObservedAt
			}
		}
		return out
	}
	if staleAfter <= 0 {
		staleAfter = 15 * time.Minute
	}
	out := make([]quotaWindow, 0, len(windows))
	recognized := make(map[string]int)
	for _, candidate := range windows {
		class := normalizeWindowClass(candidate.Class)
		if class == "" {
			if candidate.ObservedAt.IsZero() {
				candidate.ObservedAt = fallbackObservedAt
			}
			out = append(out, candidate)
			continue
		}
		candidate.Class = class
		if candidate.ObservedAt.IsZero() {
			candidate.ObservedAt = fallbackObservedAt
		}
		index, exists := recognized[class]
		if !exists {
			recognized[class] = len(out)
			out = append(out, candidate)
			continue
		}
		current := out[index]
		if quotaWindowPreferred(candidate, current, fallbackObservedAt) {
			candidate = retainQuotaWindowMetadata(candidate, current, now, staleAfter)
			out[index] = candidate
		} else {
			out[index] = retainQuotaWindowMetadata(current, candidate, now, staleAfter)
		}
	}
	return out
}

func quotaWindowPreferred(candidate, current quotaWindow, fallbackObservedAt time.Time) bool {
	candidateAt := candidate.ObservedAt
	if candidateAt.IsZero() {
		candidateAt = fallbackObservedAt
	}
	currentAt := current.ObservedAt
	if currentAt.IsZero() {
		currentAt = fallbackObservedAt
	}
	if !candidateAt.Equal(currentAt) {
		return candidateAt.After(currentAt)
	}
	candidateHard := quotaWindowHardRestriction(candidate)
	currentHard := quotaWindowHardRestriction(current)
	if candidateHard != currentHard {
		return candidateHard
	}
	if candidateHard && math.Abs(candidate.UsedPercent-current.UsedPercent) > 1e-9 {
		return candidate.UsedPercent > current.UsedPercent
	}
	candidateSource := quotaSourcePriority(candidate.Source)
	currentSource := quotaSourcePriority(current.Source)
	if candidateSource != currentSource {
		return candidateSource > currentSource
	}
	if candidate.WindowUsageCreditsKnown != current.WindowUsageCreditsKnown {
		return candidate.WindowUsageCreditsKnown
	}
	if candidate.WindowSeconds != current.WindowSeconds {
		return candidate.WindowSeconds > current.WindowSeconds
	}
	if !candidate.ResetAt.Equal(current.ResetAt) {
		return candidate.ResetAt.After(current.ResetAt)
	}
	return false
}

func quotaWindowHardRestriction(window quotaWindow) bool {
	return !window.Allowed || window.LimitReached || window.UsedPercent >= usedPercentThreshold
}

func quotaSourcePriority(source quotaSource) int {
	switch source {
	case quotaSourceMixed:
		return 3
	case quotaSourceProbe:
		return 2
	case quotaSourceHeader:
		return 1
	default:
		return 0
	}
}

func retainQuotaWindowMetadata(winner, older quotaWindow, now time.Time, staleAfter time.Duration) quotaWindow {
	if winner.WindowSeconds <= 0 && older.WindowSeconds > 0 {
		winner.WindowSeconds = older.WindowSeconds
	}
	sameCycle := winner.ResetAt.IsZero() || older.ResetAt.IsZero() ||
		math.Abs(winner.ResetAt.Sub(older.ResetAt).Seconds()) <= (2*time.Minute).Seconds()
	observedAt := older.ObservedAt
	if observedAt.IsZero() {
		observedAt = now
	}
	metadataFresh := !now.Before(observedAt) && now.Sub(observedAt) <= staleAfter
	if sameCycle && metadataFresh {
		if !winner.ResetAfterSecondsKnown && older.ResetAfterSecondsKnown {
			winner.ResetAfterSeconds = older.ResetAfterSeconds
			winner.ResetAfterSecondsKnown = true
		}
		if !winner.WindowUsageCreditsKnown && older.WindowUsageCreditsKnown && older.WindowUsageCredits >= 0 {
			winner.WindowUsageCredits = older.WindowUsageCredits
			winner.WindowUsageCreditsKnown = true
			winner.Source = mergeQuotaSource(winner.Source, older.Source)
		}
	}
	return winner
}
