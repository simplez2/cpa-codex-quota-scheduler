package main

import (
	"math"
	"time"
)

// Deadlines within 5% of the earliest remaining time share a group, up to
// 30 minutes. The tolerance contracts near reset; a two-minute difference
// matters in the final minutes, even if it was negligible half a day ago.
func weeklyDeadlineSlack(earliest time.Duration) time.Duration {
	return min(30*time.Minute, earliest/20)
}

// Compact these fresh, aligned slices in place. Only spendable offers can set
// the earliest deadline. Reservation collisions rebuild the group on retry.
func weeklyDeadlineGroup(choices []serialCandidate, inputs []adqAccountInput, cost float64, cfg pluginConfig, policy adqPolicy, now time.Time) ([]serialCandidate, []adqAccountInput) {
	var earliest time.Duration
	horizons := make([]time.Duration, len(inputs))
	for i, in := range inputs {
		horizon, known := weeklyResetHorizon(choices[i], cfg, now)
		m := adqAssessAccount(in, policy, now)
		if !known || !m.Eligible || m.WEffective < cost {
			continue
		}
		horizons[i] = horizon
		if earliest == 0 || horizon < earliest {
			earliest = horizon
		}
	}
	cutoff := earliest + weeklyDeadlineSlack(earliest)
	keptChoices, keptInputs := choices[:0], inputs[:0]
	for i, horizon := range horizons {
		if horizon > 0 && horizon <= cutoff {
			keptChoices = append(keptChoices, choices[i])
			keptInputs = append(keptInputs, inputs[i])
		}
	}
	return keptChoices, keptInputs
}

// Recent service ranks flexible work only. ADQ owns the independent quota
// balance. Keep epochs separate across credential replacement and weekly reset.
type weeklyServiceEpoch struct {
	index string
	reset time.Time
}

func (e weeklyServiceEpoch) matches(other weeklyServiceEpoch) bool {
	return e.index != "" && e.index == other.index && quotaRunwaySameReset(e.reset, other.reset)
}

type weeklyService struct {
	epoch weeklyServiceEpoch
	at    time.Time
	work  float64
}

const weeklyServiceHalfLife = 30 * time.Minute

func weeklyServiceDiscount(age time.Duration) float64 {
	return math.Exp2(-float64(max(0, age)) / float64(weeklyServiceHalfLife))
}

func (s weeklyService) atTime(epoch weeklyServiceEpoch, now time.Time) float64 {
	if !s.epoch.matches(epoch) || !now.Before(epoch.reset) {
		return 0
	}
	return s.work * weeklyServiceDiscount(now.Sub(s.at))
}

func (s *weeklyService) assign(epoch weeklyServiceEpoch, cost float64, now time.Time) {
	*s = weeklyService{epoch: epoch, at: now, work: s.atTime(epoch, now) + cost}
}

// Replace the original estimate at its original service time. A late
// completion contributes the same discounted work as an on-time correction.
func (s *weeklyService) correct(epoch weeklyServiceEpoch, delta float64, started, now time.Time) {
	if !s.epoch.matches(epoch) || !now.Before(epoch.reset) {
		return
	}
	s.work = max(0, s.atTime(epoch, now)+delta*weeklyServiceDiscount(now.Sub(started)))
	s.at = now
}

// For target pace w, recent assigned work S and an indivisible request c,
// minimizing the increment of sum(S_i^2 / (2*w_i)) chooses the smallest
// (S_i+c/2)/w_i. Sticky and pinned turns already contribute to S.
func (s *schedulerRuntimeState) weeklyDeadlineScoresLocked(choices []serialCandidate, inputs []adqAccountInput, cost float64, policy adqPolicy, now time.Time) (map[string]adqAllocationScore, map[string]float64) {
	scores := make(map[string]adqAllocationScore, len(inputs))
	weights := make(map[string]float64, len(inputs))
	for i, in := range inputs {
		horizon, _ := weeklyResetHorizon(choices[i], s.cfg, now)
		effective, _ := adqEffectiveQuota(in, policy)
		pace := weeklyDeadlinePace(effective, horizon)
		account := s.balancedAccounts[choices[i].Candidate.ID]
		epoch := weeklyServiceEpoch{in.AuthIndex, now.Add(horizon)}
		recent := account.weeklyService.atTime(epoch, now)
		scores[in.ID] = adqAllocationScore{Credit: -(recent + cost/2) / pace, LastPicked: account.LastPicked}
		weights[in.ID] = pace
	}
	return scores, weights
}
