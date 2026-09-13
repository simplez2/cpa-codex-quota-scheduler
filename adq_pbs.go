package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// ADQ-PBS is the quota decision core. It intentionally works on normalized
// values so it can be unit-tested without CPA or credential material.
// Percentages are represented as quota-capacity units, not provider prices.
type adqPolicy struct {
	FiveHourWindow        time.Duration
	WeeklyWindow          time.Duration
	MaxQuotaRatio5h       float64
	CacheGuaranteedWindow time.Duration
	CacheHighValueWindow  time.Duration
	PhaseBucketSize       time.Duration
	ForecastHorizon       time.Duration
	// WeeklySafetyMargin protects against delayed weekly telemetry. Five-hour
	// quota intentionally has no implicit reserve: callers can still set an
	// explicit FiveHourSafetyMargin when a provider contract requires one.
	WeeklySafetyMargin   float64
	FiveHourSafetyMargin float64
	// QuotaSafetyMargin is kept as a legacy alias for weekly safety only. It is
	// never applied to the five-hour window, so old configuration cannot create
	// a hidden 5h reserve.
	QuotaSafetyMargin     float64
	LockSLAConfidence     float64
	LockSLAMaxWindows     int
	PhaseDemandMixEta     float64
	EWMAAlpha             float64
	MWeekWarning          float64
	M5hWarning            float64
	MPhaseWarning         float64
	RiskModeThreshold     float64
	BridgeModeThreshold   float64
	MonteCarloScenarios   int
	ReservationTimeout    time.Duration
	SettlementTimeout     time.Duration
	ResetJitterPercentile float64
}

func defaultADQPolicy() adqPolicy {
	return adqPolicy{
		FiveHourWindow:        5 * time.Hour,
		WeeklyWindow:          7 * 24 * time.Hour,
		MaxQuotaRatio5h:       0.16,
		CacheGuaranteedWindow: 30 * time.Minute,
		CacheHighValueWindow:  20 * time.Minute,
		PhaseBucketSize:       15 * time.Minute,
		ForecastHorizon:       5 * time.Hour,
		WeeklySafetyMargin:    0.01,
		FiveHourSafetyMargin:  0,
		QuotaSafetyMargin:     0,
		LockSLAConfidence:     0.95,
		LockSLAMaxWindows:     6,
		PhaseDemandMixEta:     0.2,
		EWMAAlpha:             0.25,
		MWeekWarning:          1,
		M5hWarning:            1,
		MPhaseWarning:         1,
		RiskModeThreshold:     0.8,
		BridgeModeThreshold:   1,
		MonteCarloScenarios:   128,
		ReservationTimeout:    45 * time.Second,
		SettlementTimeout:     2 * time.Minute,
		ResetJitterPercentile: 0.95,
	}
}

type adqProviderState string

const (
	adqProviderHealthy  adqProviderState = "healthy"
	adqProviderOverload adqProviderState = "overload"
	adqProviderQuota    adqProviderState = "quota_exhausted"
	adqProviderAuth     adqProviderState = "auth_failed"
	adqProviderUnknown  adqProviderState = "unknown"
)

// classifyADQProviderError keeps overload independent from quota exhaustion.
// The classifier is deliberately conservative: a generic 503 never consumes
// the weekly/5h budget in the local model.
func classifyADQProviderError(status int, code, message string) adqProviderState {
	text := strings.ToLower(strings.TrimSpace(code + " " + message))
	// HTTP status is the strongest signal. In particular, CPA can wrap an
	// upstream usage_limit_reached/auth_unavailable message in a 503 when every
	// provider attempt is temporarily unavailable. Treating that wrapper as a
	// hard quota ban would hide otherwise recoverable accounts.
	switch status {
	case 429:
		return adqProviderQuota
	case 401, 403:
		return adqProviderAuth
	case 408, 425, 500, 502, 503, 504, 522:
		return adqProviderOverload
	}
	// For status-less executor errors, retain explicit semantic markers.
	switch {
	case strings.Contains(text, "auth_unavailable"), strings.Contains(text, "no_auth_available"), strings.Contains(text, "no auth available"):
		return adqProviderOverload
	case strings.Contains(text, "invalid_token"), strings.Contains(text, "authentication"):
		return adqProviderAuth
	case strings.Contains(text, "usage_limit"), strings.Contains(text, "quota"), strings.Contains(text, "rate_limit"):
		return adqProviderQuota
	case strings.Contains(text, "overloaded"), strings.Contains(text, "timeout"), strings.Contains(text, "upstream_unavailable"):
		return adqProviderOverload
	case status > 0:
		return adqProviderUnknown
	default:
		return adqProviderUnknown
	}
}

type adqAccountInput struct {
	ID                    string
	AuthIndex             string
	Plan                  string
	WeeklyCapacity        float64
	WeeklyRemaining       float64
	FiveHourCapacity      float64
	FiveHourRemaining     float64
	WeeklyResetAt         time.Time
	FiveHourResetAt       time.Time
	BurnMean              float64 // capacity units/hour
	BurnP90               float64
	BurnP95               float64
	CacheHitProbability   float64
	CacheAge              time.Duration
	CacheWarmBurn         float64
	CacheColdBurn         float64
	ReservationFiveHour   float64
	ReservationWeekly     float64
	SettlingFiveHour      float64
	SettlingWeekly        float64
	PendingFiveHour       float64
	PendingWeekly         float64
	ProviderState         adqProviderState
	ProviderRetryAt       time.Time
	Healthy               bool
	Sticky                bool
	PhaseAnchor           time.Time
	PhaseAnchorMode       string
	WeeklyDebt            float64
	LastRoutingReason     string
	AbsoluteCapacityKnown bool
	CapacitySource        string
}

type adqAccountMetrics struct {
	AuthID                     string           `json:"auth_id"`
	AuthIndex                  string           `json:"auth_index,omitempty"`
	Plan                       string           `json:"plan,omitempty"`
	CapacitySource             string           `json:"capacity_source,omitempty"`
	State                      string           `json:"state"`
	W                          float64          `json:"weekly_capacity"`
	w                          float64          `json:"-"`
	H                          float64          `json:"five_hour_capacity"`
	h                          float64          `json:"-"`
	WEffective                 float64          `json:"weekly_effective"`
	HEffective                 float64          `json:"five_hour_effective"`
	Kappa                      float64          `json:"kappa"`
	FullWidthContribution      float64          `json:"full_width_contribution"`
	EffectiveWidthContribution float64          `json:"effective_width_contribution"`
	Next5hReset                time.Time        `json:"next_5h_reset,omitempty"`
	NextWeekReset              time.Time        `json:"next_week_reset,omitempty"`
	PhaseBucket                int              `json:"phase_bucket"`
	PhaseAnchorMode            string           `json:"phase_anchor_mode,omitempty"`
	BurnMean                   float64          `json:"burn_mean"`
	BurnP90                    float64          `json:"burn_p90"`
	BurnP95                    float64          `json:"burn_p95"`
	CacheState                 string           `json:"cache_state"`
	CacheAgeSeconds            float64          `json:"cache_age_seconds"`
	CacheHitProbability        float64          `json:"cache_hit_probability"`
	PFillCurrent               float64          `json:"p_fill_current"`
	PFillFull                  float64          `json:"p_fill_full"`
	QLockMean                  float64          `json:"q_lock_mean"`
	QLockP95                   float64          `json:"q_lock_p95"`
	ProjectedKappa             float64          `json:"projected_kappa"`
	Runway5hSeconds            float64          `json:"runway_5h_seconds"`
	RunwayWeekSeconds          float64          `json:"runway_week_seconds"`
	RunwayFinalSeconds         float64          `json:"runway_final_seconds"`
	WeeklyWaterline            float64          `json:"weekly_waterline"`
	WeeklyDebt                 float64          `json:"weekly_debt"`
	Regime                     string           `json:"regime"`
	Tail                       bool             `json:"tail"`
	ProviderState              adqProviderState `json:"provider_state"`
	ProviderRetryAt            time.Time        `json:"provider_retry_at,omitempty"`
	Eligible                   bool             `json:"eligible"`
	Reason                     string           `json:"reason,omitempty"`
	ReservationFiveHour        float64          `json:"reservation_5h"`
	ReservationWeekly          float64          `json:"reservation_weekly"`
	SettlingFiveHour           float64          `json:"settling_5h"`
	SettlingWeekly             float64          `json:"settling_weekly"`
}

type adqPoolMetrics struct {
	FullWidth            int                 `json:"FullWidth"`
	EffectiveWidth       float64             `json:"EffectiveWidth"`
	MWeek                float64             `json:"M_week"`
	M5h                  float64             `json:"M_5h"`
	MPhase               float64             `json:"M_phase"`
	MSystem              float64             `json:"M_system"`
	Bottleneck           string              `json:"bottleneck"`
	PhaseHealth          float64             `json:"phase_health"`
	Gen0Coverage         float64             `json:"gen0_coverage"`
	CapacityFailure      bool                `json:"capacity_failure"`
	SchedulerFailure     bool                `json:"scheduler_failure"`
	CurrentDemandPerHour float64             `json:"current_demand_per_hour"`
	ForecastDemandP95    float64             `json:"forecast_demand_p95"`
	Accounts             []adqAccountMetrics `json:"accounts,omitempty"`
}

type adqDecision struct {
	AuthID              string
	Reason              string
	Metrics             adqAccountMetrics
	PoolBefore          adqPoolMetrics
	PoolAfter           adqPoolMetrics
	CandidateCount      int
	ReservationID       string
	ReservationFiveHour float64
	ReservationWeekly   float64
}

func finiteADQ(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func clampADQ(v, lo, hi float64) float64 {
	if !finiteADQ(v) {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
func maxADQ(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func adqWeeklySafety(p adqPolicy) float64 {
	margin := p.WeeklySafetyMargin
	if margin == 0 {
		// Preserve callers that still populate the old field, while keeping its
		// scope explicit: legacy safety is weekly-only.
		margin = p.QuotaSafetyMargin
	}
	return clampADQ(margin, 0, .5)
}

func adqFiveHourSafety(p adqPolicy) float64 {
	return clampADQ(p.FiveHourSafetyMargin, 0, .5)
}

// adqHardEffectiveQuota is the actual admission gate. It accounts for work
// that is still running, but it intentionally excludes completed requests
// whose provider telemetry has not caught up yet. That distinction lets a
// healthy sticky conversation continue without treating telemetry lag as a
// provider hard limit.
func adqHardEffectiveQuota(in adqAccountInput, p adqPolicy) (w, h float64) {
	weeklyCap := maxADQ(in.WeeklyCapacity, 0)
	fiveCap := maxADQ(in.FiveHourCapacity, weeklyCap*p.MaxQuotaRatio5h)
	weeklySafety := weeklyCap * adqWeeklySafety(p)
	fiveSafety := fiveCap * adqFiveHourSafety(p)
	w = maxADQ(0, in.WeeklyRemaining-in.ReservationWeekly-in.PendingWeekly-weeklySafety)
	h = maxADQ(0, in.FiveHourRemaining-in.ReservationFiveHour-in.PendingFiveHour-fiveSafety)
	return w, h
}

// adqEffectiveQuota is the scheduling waterline. A completed request remains a
// bounded local debt until a newer quota observation acknowledges it or the
// settlement timeout expires. This prevents stale provider percentages from
// repeatedly sending fresh sessions to the same account.
func adqEffectiveQuota(in adqAccountInput, p adqPolicy) (w, h float64) {
	w, h = adqHardEffectiveQuota(in, p)
	w = maxADQ(0, w-in.SettlingWeekly)
	h = maxADQ(0, h-in.SettlingFiveHour)
	return w, h
}

func adqNormalCDF(x float64) float64 {
	if x <= -8 {
		return 0
	}
	if x >= 8 {
		return 1
	}
	return 0.5 * (1 + math.Erf(x/math.Sqrt2))
}

// adqFillProbability treats burn P95 as mu+1.645*sigma. It returns the
// probability that stochastic burn reaches a quota amount before reset.
func adqFillProbability(remaining float64, horizon time.Duration, mean, p95 float64) float64 {
	if remaining <= 0 {
		return 1
	}
	if horizon <= 0 || !finiteADQ(mean) || !finiteADQ(p95) || p95 <= 0 {
		return 0
	}
	if mean < 0 {
		mean = 0
	}
	if p95 < mean {
		p95 = mean
	}
	sigma := (p95 - mean) / 1.645
	if sigma <= 1e-9 {
		sigma = maxADQ(p95*.05, 1e-9)
	}
	hours := horizon.Hours()
	z := (remaining/hours - mean) / sigma
	return clampADQ(1-adqNormalCDF(z), 0, 1)
}

func adqLockSLAProbability(confidence float64, maxWindows int) float64 {
	confidence = clampADQ(confidence, 0, .999999)
	if maxWindows < 1 {
		maxWindows = 1
	}
	return 1 - math.Pow(1-confidence, 1/float64(maxWindows))
}

func adqLockCost(capacity, pFill, meanFail float64) float64 {
	capacity = maxADQ(capacity, 0)
	pFill = clampADQ(pFill, 0, 1)
	meanFail = clampADQ(meanFail, 0, capacity)
	if capacity <= 0 {
		return 0
	}
	if pFill <= 1e-9 {
		// A planner must still be able to compare candidates when the estimated
		// fill probability is zero. Keep the diagnostic bounded and conservative;
		// the account remains ineligible when its actual effective quota is zero.
		return capacity + maxADQ(meanFail, capacity)
	}
	q := capacity + ((1-pFill)/pFill)*meanFail
	if !finiteADQ(q) {
		return capacity + maxADQ(meanFail, capacity)
	}
	return maxADQ(capacity, q)
}

func adqRunway(capacity, remaining float64, period, untilReset time.Duration, burn float64) float64 {
	if remaining <= 0 {
		return 0
	}
	if burn <= 1e-9 || !finiteADQ(burn) {
		return math.Inf(1)
	}
	raw := remaining / burn * 3600
	if untilReset <= 0 {
		return raw
	}
	if raw < untilReset.Seconds() {
		return raw
	}
	if burn*period.Hours() <= capacity+1e-9 {
		return math.Inf(1)
	}
	return untilReset.Seconds() + capacity/burn*3600
}

func adqCacheState(age time.Duration, p adqPolicy) string {
	if age < 0 {
		age = 0
	}
	if p.CacheGuaranteedWindow > 0 && age <= p.CacheGuaranteedWindow {
		return "warm"
	}
	if p.CacheHighValueWindow > 0 && age <= p.CacheGuaranteedWindow+p.CacheHighValueWindow {
		return "high_value"
	}
	return "cold"
}

func adqExpectedBurn(in adqAccountInput) float64 {
	mean := maxADQ(in.BurnMean, 0)
	warm := in.CacheWarmBurn
	cold := in.CacheColdBurn
	if warm <= 0 {
		warm = mean
	}
	if cold <= 0 {
		cold = mean
	}
	probability := clampADQ(in.CacheHitProbability, 0, 1)
	if probability <= 0 && in.CacheAge <= 0 {
		probability = 0
	}
	expected := probability*warm + (1-probability)*cold
	if expected <= 0 {
		expected = mean
	}
	return maxADQ(expected, 0)
}

func adqPhaseBucket(anchor, now time.Time, p adqPolicy) int {
	if p.PhaseBucketSize <= 0 {
		return -1
	}
	cycle := p.FiveHourWindow
	if cycle <= 0 {
		cycle = 5 * time.Hour
	}
	if anchor.IsZero() || now.Before(anchor) {
		return -1
	}
	elapsed := now.Sub(anchor) % cycle
	n := int(math.Ceil(cycle.Seconds() / p.PhaseBucketSize.Seconds()))
	if n < 1 {
		n = 1
	}
	index := int(elapsed / p.PhaseBucketSize)
	if index >= n {
		index = n - 1
	}
	return index
}

func adqAssessAccount(in adqAccountInput, p adqPolicy, now time.Time) adqAccountMetrics {
	m := adqAccountMetrics{AuthID: strings.TrimSpace(in.ID), AuthIndex: strings.TrimSpace(in.AuthIndex), Plan: in.Plan, CapacitySource: in.CapacitySource,
		W: maxADQ(in.WeeklyCapacity, 0), H: maxADQ(in.FiveHourCapacity, maxADQ(in.WeeklyCapacity, 0)*p.MaxQuotaRatio5h),
		Next5hReset: in.FiveHourResetAt, NextWeekReset: in.WeeklyResetAt, PhaseAnchorMode: in.PhaseAnchorMode,
		WeeklyDebt: in.WeeklyDebt, ProviderState: in.ProviderState, ProviderRetryAt: in.ProviderRetryAt,
		ReservationFiveHour: in.ReservationFiveHour, ReservationWeekly: in.ReservationWeekly,
		SettlingFiveHour: in.SettlingFiveHour, SettlingWeekly: in.SettlingWeekly}
	if m.H <= 0 {
		m.H = m.W * p.MaxQuotaRatio5h
	}
	hardW, hardH := adqHardEffectiveQuota(in, p)
	m.w, m.h = adqEffectiveQuota(in, p)
	m.WEffective, m.HEffective = m.w, m.h
	m.Kappa = 0
	if m.H > 0 {
		// Kappa describes future complete 5h chunks backed by the weekly
		// reservoir. Current 5h headroom remains an independent hard gate.
		m.Kappa = m.w / m.H
	}
	m.FullWidthContribution = 0
	if m.H > 0 && m.w >= m.H-1e-9 {
		m.FullWidthContribution = 1
	}
	if m.H > 0 {
		m.EffectiveWidthContribution = clampADQ(m.w/m.H, 0, 1)
	}
	m.WeeklyWaterline = 0
	if m.W > 0 {
		m.WeeklyWaterline = clampADQ(m.w/m.W, 0, 1)
	}
	m.BurnMean = maxADQ(in.BurnMean, 0)
	m.BurnP90 = maxADQ(in.BurnP90, m.BurnMean)
	m.BurnP95 = maxADQ(in.BurnP95, m.BurnP90)
	expected := adqExpectedBurn(in)
	if expected > 0 {
		m.BurnP95 = maxADQ(m.BurnP95, expected)
	}
	if m.BurnMean <= 0 {
		m.BurnMean = expected
	}
	if m.BurnP90 <= 0 {
		m.BurnP90 = m.BurnMean
	}
	if m.BurnP95 <= 0 {
		m.BurnP95 = m.BurnP90
	}
	age := in.CacheAge
	if age < 0 {
		age = 0
	}
	m.CacheAgeSeconds = age.Seconds()
	m.CacheHitProbability = clampADQ(in.CacheHitProbability, 0, 1)
	m.CacheState = adqCacheState(age, p)
	m.PhaseBucket = adqPhaseBucket(in.PhaseAnchor, now, p)
	d5 := time.Duration(0)
	if !in.FiveHourResetAt.IsZero() && in.FiveHourResetAt.After(now) {
		d5 = in.FiveHourResetAt.Sub(now)
	}
	if d5 <= 0 {
		d5 = p.FiveHourWindow
	}
	m.PFillCurrent = adqFillProbability(m.h, d5, m.BurnMean, m.BurnP95)
	m.PFillFull = adqFillProbability(m.H, p.FiveHourWindow, m.BurnMean, m.BurnP95)
	muFail := minADQ(m.H, maxADQ(m.BurnMean, 0)*p.FiveHourWindow.Hours()*.5)
	m.QLockMean = adqLockCost(m.H, m.PFillFull, muFail)
	if !finiteADQ(m.QLockMean) {
		m.QLockP95 = math.Inf(1)
	} else {
		m.QLockP95 = maxADQ(m.QLockMean, m.QLockMean*1.645)
	}
	m.Runway5hSeconds = adqRunway(m.H, m.h, p.FiveHourWindow, d5, m.BurnP95)
	dw := time.Duration(0)
	if !in.WeeklyResetAt.IsZero() && in.WeeklyResetAt.After(now) {
		dw = in.WeeklyResetAt.Sub(now)
	}
	if dw <= 0 {
		dw = p.WeeklyWindow
	}
	m.RunwayWeekSeconds = adqRunway(m.W, m.w, p.WeeklyWindow, dw, m.BurnP95)
	m.RunwayFinalSeconds = minADQ(m.Runway5hSeconds, m.RunwayWeekSeconds)
	bw := 0.0
	b5 := 0.0
	if p.WeeklyWindow > 0 {
		bw = m.W / p.WeeklyWindow.Hours()
	}
	if p.FiveHourWindow > 0 {
		b5 = m.H / p.FiveHourWindow.Hours()
	}
	switch {
	case m.BurnP95 <= bw+1e-9:
		m.Regime = "SUSTAINABLE_STICKY"
	case m.BurnP95 < b5-1e-9:
		m.Regime = "WEEK_DEATH_LOCK_IN"
	default:
		m.Regime = "ROTATABLE"
	}
	m.Tail = m.w > 0 && m.h > 0 && m.H > 0 && (m.w < m.H || m.h < m.H)
	providerState := in.ProviderState
	if providerState == adqProviderOverload && !in.ProviderRetryAt.IsZero() && !now.Before(in.ProviderRetryAt) {
		// The local circuit has cooled down. Let the next request probe the
		// provider again instead of permanently hiding the account.
		providerState = adqProviderHealthy
		m.ProviderState = providerState
	}
	providerReady := providerState == "" || providerState == adqProviderHealthy
	gateW, gateH := m.w, m.h
	if in.Sticky {
		gateW, gateH = hardW, hardH
	}
	m.Eligible = in.Healthy && providerReady && gateW > 0 && gateH > 0
	if !in.Healthy {
		m.Reason = "unhealthy"
		m.State = "UNHEALTHY"
	} else if gateW <= 0 {
		m.Reason = "weekly_exhausted"
		m.State = "ACCOUNT_DEAD"
	} else if gateH <= 0 {
		m.Reason = "five_hour_exhausted"
		m.State = "5H_COOLDOWN"
	} else if providerState == adqProviderOverload && now.Before(in.ProviderRetryAt) {
		m.Reason = "provider_overload"
		m.State = "PROVIDER_OVERLOAD"
	} else if providerState == adqProviderQuota {
		m.Reason = "quota_exhausted"
		m.State = "ACCOUNT_DEAD"
	} else if in.Sticky && (m.w <= 0 || m.h <= 0) {
		m.State = "READY"
		m.Reason = "sticky_settlement_pending"
	} else {
		m.State = "READY"
		m.Reason = "eligible"
	}
	if providerState == adqProviderAuth {
		m.Eligible = false
		m.State = "AUTH_FAILED"
		m.Reason = "auth_failed"
	}
	if providerState == adqProviderOverload && now.Before(in.ProviderRetryAt) {
		m.Eligible = false
	}
	return m
}

func minADQ(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func adqProjectedVector(accounts []adqAccountMetrics, candidate string, debitWeekly, debitFiveHour float64) []float64 {
	values := make([]float64, 0, len(accounts))
	for _, m := range adqProjectedMetrics(accounts, candidate, debitWeekly, debitFiveHour) {
		values = append(values, m.Kappa)
	}
	sort.Float64s(values)
	return values
}

// adqProjectedMetrics applies one candidate's expected request debit to both
// quota windows. The result is a pure copy, so callers can compare candidates
// without mutating the live snapshot.
func adqProjectedMetrics(accounts []adqAccountMetrics, candidate string, debitWeekly, debitFiveHour float64) []adqAccountMetrics {
	projected := make([]adqAccountMetrics, len(accounts))
	copy(projected, accounts)
	for i := range projected {
		m := &projected[i]
		if m.AuthID != candidate {
			continue
		}
		m.WEffective = maxADQ(0, m.WEffective-maxADQ(debitWeekly, 0))
		m.HEffective = maxADQ(0, m.HEffective-maxADQ(debitFiveHour, 0))
		m.ProjectedKappa = 0
		m.FullWidthContribution = 0
		m.EffectiveWidthContribution = 0
		if m.H > 0 {
			m.ProjectedKappa = m.WEffective / m.H
			m.Kappa = m.ProjectedKappa
			if m.WEffective >= m.H-1e-9 {
				m.FullWidthContribution = 1
			}
			m.EffectiveWidthContribution = clampADQ(m.WEffective/m.H, 0, 1)
		}
		m.Tail = m.WEffective > 0 && m.HEffective > 0 && m.H > 0 && (m.WEffective < m.H || m.HEffective < m.H)
	}
	return projected
}

func compareADQLeximin(a, b []float64) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			if a[i] > b[i] {
				return 1
			}
			return -1
		}
	}
	if len(a) > len(b) {
		return 1
	}
	if len(a) < len(b) {
		return -1
	}
	return 0
}

func adqComputePool(accounts []adqAccountMetrics, demandPerHour float64, p adqPolicy, now time.Time) adqPoolMetrics {
	pool := adqPoolMetrics{Accounts: append([]adqAccountMetrics(nil), accounts...), CurrentDemandPerHour: maxADQ(demandPerHour, 0)}
	totalW, totalH := 0.0, 0.0
	totalCapacity := 0.0
	for _, m := range accounts {
		pool.FullWidth += int(math.Round(m.FullWidthContribution))
		pool.EffectiveWidth += m.EffectiveWidthContribution
		totalW += m.WEffective
		totalH += m.HEffective
		totalCapacity += minADQ(m.WEffective, m.H)
	}
	horizon := p.ForecastHorizon
	if horizon <= 0 {
		horizon = p.FiveHourWindow
	}
	demand5 := maxADQ(demandPerHour, 0) * horizon.Hours()
	demandWeek := maxADQ(demandPerHour, 0) * p.WeeklyWindow.Hours()
	pool.ForecastDemandP95 = demand5
	if demandWeek > 0 {
		pool.MWeek = totalW / demandWeek
	} else {
		pool.MWeek = math.Inf(1)
	}
	if demand5 > 0 {
		pool.M5h = totalH / demand5
	} else {
		pool.M5h = math.Inf(1)
	}
	buckets := 1
	if p.PhaseBucketSize > 0 && p.FiveHourWindow > 0 {
		buckets = int(math.Ceil(p.FiveHourWindow.Seconds() / p.PhaseBucketSize.Seconds()))
		if buckets < 1 {
			buckets = 1
		}
	}
	bucketCap := make([]float64, buckets)
	for _, m := range accounts {
		if m.PhaseBucket >= 0 && m.PhaseBucket < buckets {
			bucketCap[m.PhaseBucket] += minADQ(m.HEffective, m.WEffective)
		} else {
			for i := range bucketCap {
				bucketCap[i] += minADQ(m.HEffective, m.WEffective) / float64(buckets)
			}
		}
	}
	bucketDemand := maxADQ(demandPerHour, 0) * p.PhaseBucketSize.Hours()
	if bucketDemand <= 0 {
		pool.MPhase = math.Inf(1)
	} else {
		pool.MPhase = math.Inf(1)
		for _, c := range bucketCap {
			pool.MPhase = minADQ(pool.MPhase, c/bucketDemand)
		}
	}
	pool.MSystem = minADQ(pool.MWeek, minADQ(pool.M5h, pool.MPhase))
	pool.PhaseHealth = pool.MPhase
	switch {
	case pool.MWeek < p.RiskModeThreshold:
		pool.Bottleneck = "WEEK_CONSTRAINED"
	case pool.MPhase < p.RiskModeThreshold:
		pool.Bottleneck = "PHASE_CONSTRAINED"
	case pool.M5h < p.RiskModeThreshold:
		pool.Bottleneck = "RISK"
	case pool.FullWidth == 0 && pool.EffectiveWidth > 0:
		pool.Bottleneck = "TAIL_DRAIN"
	default:
		pool.Bottleneck = "BALANCED"
	}
	if pool.MSystem < p.RiskModeThreshold {
		pool.CapacityFailure = totalCapacity < demand5
	}
	if !finiteADQ(pool.MSystem) {
		pool.MSystem = 1e9
	}
	_ = now
	return pool
}

// adqChoose is kept as a compatibility wrapper for callers that do not yet
// have a request-cost estimate. Runtime routing should call adqChooseWithDebit
// so projected Leximin sees the actual expected debit before reservation.
func adqChoose(accounts []adqAccountInput, demandPerHour float64, p adqPolicy, now time.Time) (adqDecision, bool) {
	return adqChooseWithDebit(accounts, demandPerHour, 0, p, now)
}

func adqChooseWithDebit(accounts []adqAccountInput, demandPerHour, requestCost float64, p adqPolicy, now time.Time) (adqDecision, bool) {
	requestCost = maxADQ(requestCost, 0)
	metrics := make([]adqAccountMetrics, 0, len(accounts))
	for _, in := range accounts {
		metrics = append(metrics, adqAssessAccount(in, p, now))
	}
	before := adqComputePool(metrics, demandPerHour, p, now)
	candidates := make([]int, 0, len(accounts))
	for i, m := range metrics {
		if m.Eligible {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return adqDecision{PoolBefore: before, CandidateCount: 0}, false
	}
	sticky := -1
	for _, i := range candidates {
		if accounts[i].Sticky {
			sticky = i
			break
		}
	}
	chosen := -1
	if sticky >= 0 {
		chosen = sticky
	}
	bestVec := []float64(nil)
	bestAfter := adqPoolMetrics{}
	if chosen < 0 {
		for _, i := range candidates {
			debitWeek, debitFive := requestCost, requestCost
			vec := adqProjectedVector(metrics, metrics[i].AuthID, debitWeek, debitFive)
			afterMetrics := adqProjectedMetrics(metrics, metrics[i].AuthID, debitWeek, debitFive)
			after := adqComputePool(afterMetrics, demandPerHour, p, now)
			better := false
			if bestVec == nil || compareADQLeximin(vec, bestVec) > 0 {
				better = true
			} else if compareADQLeximin(vec, bestVec) == 0 {
				if after.FullWidth > bestAfter.FullWidth || (after.FullWidth == bestAfter.FullWidth && after.MSystem > bestAfter.MSystem+1e-9) {
					better = true
				} else if after.FullWidth == bestAfter.FullWidth && math.Abs(after.MSystem-bestAfter.MSystem) < 1e-9 && metrics[i].AuthID < metrics[chosen].AuthID {
					better = true
				}
			}
			if better {
				chosen = i
				bestVec = vec
				bestAfter = after
			}
		}
	}
	if chosen < 0 {
		return adqDecision{PoolBefore: before, CandidateCount: len(candidates)}, false
	}
	if bestAfter.Accounts == nil {
		afterMetrics := adqProjectedMetrics(metrics, metrics[chosen].AuthID, requestCost, requestCost)
		bestAfter = adqComputePool(afterMetrics, demandPerHour, p, now)
	}
	m := metrics[chosen]
	reason := fmt.Sprintf("选择 %s：保持 FullWidth=%d，当前 p_fill=%.0f%%，完整 5h p_fill=%.0f%%，预计 Q_lock P95=%.3g，weekly waterline=%.1f%%，runway=%.0f 分钟。", m.AuthID, before.FullWidth, m.PFillCurrent*100, m.PFillFull*100, m.QLockP95, m.WeeklyWaterline*100, m.RunwayFinalSeconds/60)
	if sticky >= 0 {
		reason = fmt.Sprintf("保持 %s：session 粘性仍然有效；当前账号周/5h有效额度均大于0。", m.AuthID)
	}
	return adqDecision{AuthID: m.AuthID, Reason: reason, Metrics: m, PoolBefore: before, PoolAfter: bestAfter, CandidateCount: len(candidates)}, true
}

// Reset detection is based on a lower-used observation followed by a fresh
// reset anchor. A full dormant placeholder alone never starts an epoch.
func adqResetTransition(previous, current adqAccountMetrics, now time.Time) bool {
	if previous.AuthID == "" || previous.AuthID != current.AuthID {
		return false
	}
	if previous.NextWeekReset.IsZero() || current.NextWeekReset.IsZero() {
		return false
	}
	if !current.NextWeekReset.After(now) {
		return false
	}
	if !previous.NextWeekReset.After(now) || current.NextWeekReset.After(previous.NextWeekReset.Add(2*time.Minute)) {
		return true
	}
	return current.w > previous.w+0.10*maxADQ(previous.W, 1) && current.w > 0.5*maxADQ(current.W, 1)
}

func adqEpochID(resetAt time.Time, fallback time.Time) string {
	if resetAt.IsZero() {
		resetAt = fallback
	}
	if resetAt.IsZero() {
		return "epoch-unknown"
	}
	return fmt.Sprintf("week-%d", resetAt.UTC().Unix()/60)
}

type adqReservationStatus string

const (
	adqReservationActive     adqReservationStatus = "active"
	adqReservationReleased   adqReservationStatus = "released"
	adqReservationSettling   adqReservationStatus = "settling"
	adqReservationReconciled adqReservationStatus = "reconciled"
	adqReservationExpired    adqReservationStatus = "expired"
)

type adqSettlementWindowBaseline struct {
	Known             bool
	ObservedAt        time.Time
	ResetAt           time.Time
	UsedPercent       float64
	UsageCredits      float64
	UsageCreditsKnown bool
}

type adqSettlementBaseline struct {
	FiveHour adqSettlementWindowBaseline
	Weekly   adqSettlementWindowBaseline
}

type adqReservation struct {
	ID               string                `json:"reservation_id"`
	AuthID           string                `json:"auth_id"`
	SessionKey       string                `json:"session_key,omitempty"`
	Model            string                `json:"model,omitempty"`
	FiveHour         float64               `json:"reserved_5h"`
	Weekly           float64               `json:"reserved_week"`
	CreatedAt        time.Time             `json:"created_at"`
	ExpiresAt        time.Time             `json:"expires_at"`
	Status           adqReservationStatus  `json:"status"`
	ActualFiveHour   float64               `json:"actual_5h,omitempty"`
	ActualWeekly     float64               `json:"actual_week,omitempty"`
	SettledAt        time.Time             `json:"settled_at,omitempty"`
	SettleExpiresAt  time.Time             `json:"settle_expires_at,omitempty"`
	SettlementReason string                `json:"settlement_reason,omitempty"`
	Baseline         adqSettlementBaseline `json:"-"`
}

type adqReservationCapacity struct{ FiveHour, Weekly float64 }

type adqReservationRequest struct {
	AuthID     string
	SessionKey string
	Model      string
	FiveHour   float64
	Weekly     float64
	TTL        time.Duration
}

type adqReservationBook struct {
	mu           sync.Mutex
	version      uint64
	next         uint64
	capacities   map[string]adqReservationCapacity
	reservations map[string]adqReservation
}

func newADQReservationBook() *adqReservationBook {
	return &adqReservationBook{capacities: map[string]adqReservationCapacity{}, reservations: map[string]adqReservation{}}
}
func (b *adqReservationBook) ensure() {
	if b.capacities == nil {
		b.capacities = map[string]adqReservationCapacity{}
	}
	if b.reservations == nil {
		b.reservations = map[string]adqReservation{}
	}
}
func (b *adqReservationBook) SetCapacity(authID string, fiveHour, weekly float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensure()
	b.capacities[strings.TrimSpace(authID)] = adqReservationCapacity{maxADQ(fiveHour, 0), maxADQ(weekly, 0)}
	b.version++
}
func (b *adqReservationBook) expireLocked(now time.Time) int {
	if now.IsZero() {
		now = time.Now()
	}
	n := 0
	for id, r := range b.reservations {
		switch r.Status {
		case adqReservationActive:
			if r.ExpiresAt.IsZero() || now.Before(r.ExpiresAt) {
				continue
			}
			r.Status = adqReservationExpired
		case adqReservationSettling:
			if r.SettleExpiresAt.IsZero() || now.Before(r.SettleExpiresAt) {
				continue
			}
			r.Status = adqReservationReconciled
			r.ActualFiveHour = 0
			r.ActualWeekly = 0
			r.SettlementReason = "ttl"
		default:
			continue
		}
		b.reservations[id] = r
		n++
	}
	if n > 0 {
		b.version++
	}
	return n
}

func (b *adqReservationBook) reservedLocked(authID string, now time.Time) (fiveHour, weekly float64) {
	b.expireLocked(now)
	all := strings.TrimSpace(authID) == ""
	for _, r := range b.reservations {
		if (!all && r.AuthID != strings.TrimSpace(authID)) || r.Status != adqReservationActive {
			continue
		}
		fiveHour += r.FiveHour
		weekly += r.Weekly
	}
	return
}
func (b *adqReservationBook) Reserved(authID string, now time.Time) (float64, float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensure()
	return b.reservedLocked(strings.TrimSpace(authID), now)
}

func (b *adqReservationBook) settlingLocked(authID string, now time.Time) (fiveHour, weekly float64) {
	b.expireLocked(now)
	all := strings.TrimSpace(authID) == ""
	for _, r := range b.reservations {
		if (!all && r.AuthID != strings.TrimSpace(authID)) || r.Status != adqReservationSettling {
			continue
		}
		fiveHour += r.ActualFiveHour
		weekly += r.ActualWeekly
	}
	return
}

func (b *adqReservationBook) Settling(authID string, now time.Time) (float64, float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensure()
	return b.settlingLocked(strings.TrimSpace(authID), now)
}

// CoversPending reports whether the reservation still represents a running
// request. Once the short reservation lease expires, balanced pending state
// resumes the predicted debit until the matching completion arrives.
func (b *adqReservationBook) CoversPending(id string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensure()
	b.expireLocked(now)
	r, ok := b.reservations[strings.TrimSpace(id)]
	return ok && r.Status == adqReservationActive
}

func (b *adqReservationBook) TryReserve(req adqReservationRequest, now time.Time) (adqReservation, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensure()
	authID := strings.TrimSpace(req.AuthID)
	if authID == "" {
		return adqReservation{}, false
	}
	cap, ok := b.capacities[authID]
	if !ok {
		return adqReservation{}, false
	}
	five, week := b.reservedLocked(authID, now)
	fiveReq, weekReq := maxADQ(req.FiveHour, 0), maxADQ(req.Weekly, 0)
	if five+fiveReq > cap.FiveHour+1e-9 || week+weekReq > cap.Weekly+1e-9 {
		return adqReservation{}, false
	}
	b.next++
	id := fmt.Sprintf("adq-%d-%d", now.UnixNano(), b.next)
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 45 * time.Second
	}
	r := adqReservation{ID: id, AuthID: authID, SessionKey: req.SessionKey, Model: req.Model, FiveHour: fiveReq, Weekly: weekReq, CreatedAt: now, ExpiresAt: now.Add(ttl), Status: adqReservationActive}
	b.reservations[id] = r
	b.version++
	return r, true
}
func (b *adqReservationBook) transition(id string, status adqReservationStatus, actualFive, actualWeek float64) bool {
	b.ensure()
	r, ok := b.reservations[id]
	if !ok {
		return false
	}
	if r.Status == adqReservationReleased || r.Status == adqReservationReconciled {
		return true
	}
	r.Status = status
	r.ActualFiveHour = maxADQ(actualFive, 0)
	r.ActualWeekly = maxADQ(actualWeek, 0)
	if status == adqReservationReleased || status == adqReservationReconciled {
		r.ActualFiveHour = 0
		r.ActualWeekly = 0
	}
	b.reservations[id] = r
	b.version++
	return true
}
func (b *adqReservationBook) Release(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.transition(strings.TrimSpace(id), adqReservationReleased, 0, 0)
}
func (b *adqReservationBook) Reconcile(id string, actualFive, actualWeek float64) bool {
	return b.Settle(id, actualFive, actualWeek, quotaSnapshot{}, time.Now(), 2*time.Minute)
}

func adqSettlementBaselineForClass(snapshot quotaSnapshot, class string) adqSettlementWindowBaseline {
	class = normalizeWindowClass(class)
	for _, window := range snapshot.Windows {
		if normalizeWindowClass(window.Class) != class {
			continue
		}
		observedAt := window.ObservedAt
		if observedAt.IsZero() {
			observedAt = snapshot.RefreshedAt
		}
		return adqSettlementWindowBaseline{
			Known: true, ObservedAt: observedAt, ResetAt: window.ResetAt,
			UsedPercent: window.UsedPercent, UsageCredits: window.WindowUsageCredits,
			UsageCreditsKnown: window.WindowUsageCreditsKnown,
		}
	}
	return adqSettlementWindowBaseline{}
}

func adqSettlementBaselineFromSnapshot(snapshot quotaSnapshot) adqSettlementBaseline {
	return adqSettlementBaseline{
		FiveHour: adqSettlementBaselineForClass(snapshot, "5h"),
		Weekly:   adqSettlementBaselineForClass(snapshot, "weekly"),
	}
}

func adqSettlementTTL(cooldown time.Duration) time.Duration {
	if cooldown <= 0 {
		cooldown = 2 * time.Minute
	}
	if cooldown < 30*time.Second {
		return 30 * time.Second
	}
	if cooldown > 10*time.Minute {
		return 10 * time.Minute
	}
	return cooldown
}

// Settle converts an active or lease-expired reservation into a bounded local
// debt. The caller passes actual usage when CPA reported tokens, otherwise its
// original prediction. A later quota observation clears each quota layer
// independently.
func (b *adqReservationBook) Settle(id string, actualFive, actualWeek float64, snapshot quotaSnapshot, now time.Time, ttl time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensure()
	id = strings.TrimSpace(id)
	r, ok := b.reservations[id]
	if !ok {
		return false
	}
	if r.Status == adqReservationReleased || r.Status == adqReservationReconciled {
		return true
	}
	if r.Status == adqReservationSettling {
		return true
	}
	if now.IsZero() {
		now = time.Now()
	}
	actualFive = maxADQ(actualFive, 0)
	actualWeek = maxADQ(actualWeek, 0)
	if actualFive <= 0 && actualWeek <= 0 {
		r.Status = adqReservationReconciled
		r.ActualFiveHour = 0
		r.ActualWeekly = 0
		r.SettlementReason = "zero"
		b.reservations[id] = r
		b.version++
		return true
	}
	r.Status = adqReservationSettling
	r.ActualFiveHour = actualFive
	r.ActualWeekly = actualWeek
	r.SettledAt = now
	r.SettleExpiresAt = now.Add(adqSettlementTTL(ttl))
	r.SettlementReason = ""
	r.Baseline = adqSettlementBaselineFromSnapshot(snapshot)
	b.reservations[id] = r
	b.version++
	return true
}

func adqSettlementWindowObserved(baseline adqSettlementWindowBaseline, current quotaWindow, fallbackObservedAt, settledAt time.Time) bool {
	if !baseline.Known {
		return false
	}
	observedAt := current.ObservedAt
	if observedAt.IsZero() {
		observedAt = fallbackObservedAt
	}
	boundary := baseline.ObservedAt
	if settledAt.After(boundary) {
		boundary = settledAt
	}
	if observedAt.IsZero() || !observedAt.After(boundary) {
		return false
	}
	if current.UsedPercent > baseline.UsedPercent+1e-9 || current.UsedPercent < baseline.UsedPercent-1e-9 {
		return true
	}
	if baseline.UsageCreditsKnown && current.WindowUsageCreditsKnown && current.WindowUsageCredits > baseline.UsageCredits+1e-9 {
		return true
	}
	if !baseline.ResetAt.IsZero() && !current.ResetAt.IsZero() && !sameQuotaCycle(baseline.ResetAt, current.ResetAt) {
		return true
	}
	return false
}

func adqSettlementCurrentWindow(snapshot quotaSnapshot, class string) (quotaWindow, bool) {
	class = normalizeWindowClass(class)
	for _, window := range snapshot.Windows {
		if normalizeWindowClass(window.Class) == class {
			return window, true
		}
	}
	return quotaWindow{}, false
}

// ObserveQuota acknowledges settling debt only when a strictly newer
// observation changes usage, usage credits, or the reset cycle. An unchanged
// snapshot therefore cannot erase local debt merely because it was polled.
func (b *adqReservationBook) ObserveQuota(authID string, snapshot quotaSnapshot, now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensure()
	b.expireLocked(now)
	authID = strings.TrimSpace(authID)
	changed := 0
	for id, r := range b.reservations {
		if r.Status != adqReservationSettling || r.AuthID != authID {
			continue
		}
		if r.ActualFiveHour > 0 {
			if current, ok := adqSettlementCurrentWindow(snapshot, "5h"); ok &&
				adqSettlementWindowObserved(r.Baseline.FiveHour, current, snapshot.RefreshedAt, r.SettledAt) {
				r.ActualFiveHour = 0
				changed++
			}
		}
		if r.ActualWeekly > 0 {
			if current, ok := adqSettlementCurrentWindow(snapshot, "weekly"); ok &&
				adqSettlementWindowObserved(r.Baseline.Weekly, current, snapshot.RefreshedAt, r.SettledAt) {
				r.ActualWeekly = 0
				changed++
			}
		}
		if r.ActualFiveHour <= 0 && r.ActualWeekly <= 0 {
			r.Status = adqReservationReconciled
			r.SettlementReason = "telemetry"
		}
		b.reservations[id] = r
	}
	if changed > 0 {
		b.version++
	}
	return changed
}
func (b *adqReservationBook) Expire(now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensure()
	return b.expireLocked(now)
}
func (b *adqReservationBook) Get(id string) (adqReservation, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensure()
	r, ok := b.reservations[strings.TrimSpace(id)]
	return r, ok
}
func (b *adqReservationBook) Snapshot(now time.Time) []adqReservation {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensure()
	b.expireLocked(now)
	out := make([]adqReservation, 0, len(b.reservations))
	for _, r := range b.reservations {
		if r.Status == adqReservationActive || now.Sub(r.CreatedAt) < 24*time.Hour {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}
