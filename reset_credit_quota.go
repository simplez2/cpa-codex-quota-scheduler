package main

import "time"

const (
	cardQuotaSyncReason = "reset_credit_sync"
	cardQuotaSyncLimit  = 3
	cardQuotaSyncDelay  = 15 * time.Second
)

// A card change requests a usage read for one credential. Only the response
// updates quota. The marker survives reloads so a switch cannot drop the work.
type cardQuotaSync struct {
	After             time.Time
	PreviousWeekly    time.Time
	PreviousUsage     float64
	PreviousFive      time.Time
	PreviousFiveUsage float64
	Attempts          int
	Confirmed         bool
}

func (p quotaPollState) cardQuotaSyncPending() bool {
	return p.CardSync != nil && !p.CardSync.Confirmed && p.CardSync.Attempts < cardQuotaSyncLimit
}

func (p quotaPollState) cardQuotaSyncDue(now time.Time) bool {
	if !p.cardQuotaSyncPending() || !now.After(p.CardSync.After) {
		return false
	}
	if p.CardSync.Attempts == 0 && p.Failures == 0 && p.Error == "" {
		return true
	}
	return !now.Before(p.NextAt)
}

func (s *schedulerRuntimeState) queueCardQuotaSyncLocked(auth cpaAuthFileEntry, credit resetCreditState, previous quotaSnapshot, now time.Time) {
	if auth.AuthIndex == "" || auth.AuthIndex != credit.AuthIndex || credit.QuotaAfter.IsZero() || credit.QuotaAfter.After(now) {
		return
	}
	p := s.quotaPolls[auth.ID]
	if p.AuthIndex != "" && p.AuthIndex != auth.AuthIndex {
		return
	}
	if p.CardSync != nil && !credit.QuotaAfter.After(p.CardSync.After) {
		return
	}
	state := cardQuotaSync{After: credit.QuotaAfter}
	if previous.AuthIndex == auth.AuthIndex && !previous.RefreshedAt.After(credit.QuotaAfter) {
		if week, ok := quotaEpochWeeklyWindow(previous); ok &&
			!quotaWindowHasPlaceholderReset(week, previous.RefreshedAt, previous.RefreshedAt) && !week.ObservedAt.After(credit.QuotaAfter) {
			state.PreviousWeekly = week.ResetAt
			state.PreviousUsage = week.UsedPercent
		}
		if five, ok := fiveHourWindow(previous); ok && five.UsedPercent > 0 &&
			!five.ObservedAt.After(credit.QuotaAfter) {
			state.PreviousFive, state.PreviousFiveUsage = five.ResetAt, five.UsedPercent
		}
	}
	if s.quotaPolls == nil {
		s.quotaPolls = make(map[string]quotaPollState)
	}
	p.AuthIndex, p.CardSync = auth.AuthIndex, &state
	s.quotaPolls[auth.ID] = p
}

func cardQuotaWindowChanged(window quotaWindow, previousReset time.Time, previousUsage float64) bool {
	return previousReset.IsZero() || !quotaRunwaySameReset(window.ResetAt, previousReset) ||
		window.UsedPercent < previousUsage
}

func (p *quotaPollState) finishCardQuotaSync(snapshot quotaSnapshot, started, now time.Time, readOK, mayShortenCooldown bool) (earlyRefill bool) {
	if !p.cardQuotaSyncPending() || !started.After(p.CardSync.After) {
		return false
	}
	state := *p.CardSync
	state.Attempts++
	if readOK && snapshot.AuthIndex == p.AuthIndex {
		week, weekly := observedWindow(snapshot, "weekly", state.After, now, now.Sub(started)+time.Second)
		five, hasFive := observedWindow(snapshot, "5h", state.After, now, now.Sub(started)+time.Second)
		// A complete weekly-only response is valid. A partial response
		// cannot prove the absent 5h slot recovered or disappeared.
		fiveReady := (hasFive && cardQuotaWindowChanged(five, state.PreviousFive, state.PreviousFiveUsage)) ||
			snapshot.FiveHourLimitStatus == "not_reported"
		state.Confirmed = weekly && fiveReady && cardQuotaWindowChanged(week, state.PreviousWeekly, state.PreviousUsage)
		if !state.Confirmed && state.Attempts < cardQuotaSyncLimit && mayShortenCooldown {
			p.NextAt = now.Add(cardQuotaSyncDelay)
		}
	}
	p.CardSync = &state
	return state.Confirmed && state.PreviousWeekly.After(now)
}
