package main

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

const (
	minimumCostSampleCredits = 0.1
	minimumQuantileSamples   = 8
	capacityEWMAAlpha        = 0.25
)

type modelPricing struct {
	Model                string  `json:"model"`
	PricingStyle         string  `json:"pricing_style"`
	PromptPricePer1M     float64 `json:"prompt_price_per_1m"`
	CompletionPricePer1M float64 `json:"completion_price_per_1m"`
	CacheReadPricePer1M  float64 `json:"cache_read_price_per_1m"`
	CacheWritePricePer1M float64 `json:"cache_write_price_per_1m"`
	PriceMultiplier      float64 `json:"price_multiplier"`
}

type capacityEstimate struct {
	Credits   float64
	Samples   int
	UpdatedAt time.Time
}

type quotaCalibrationPoint struct {
	UsedPercent       float64
	WindowUsageCredit float64
	UsageKnown        bool
	ResetAt           time.Time
	RefreshedAt       time.Time
}

type accountPacingState struct {
	DeficitCredits   float64
	LastAccruedAt    time.Time
	Capacities       map[string]capacityEstimate
	LastQuota        map[string]quotaCalibrationPoint
	PendingPredicted []float64
}

type pacingCandidate struct {
	Candidate         pluginapi.SchedulerAuthCandidate
	Snapshot          quotaSnapshot
	Bottleneck        quotaWindow
	PredictedCredits  float64
	ScorePercent      float64
	DebtPercent       float64
	DeficitPercent    float64
	MinimumRemaining  float64
	TargetRate        float64
	ReferenceCapacity float64
	EarliestReset     time.Time
	ResetCreditBonus  bool
}

type stickyBinding struct {
	AuthID        string
	LastUsedAt    time.Time
	ChallengerID  string
	Confirmations int
}

type schedulerCandidateAudit struct {
	AuthHash         string  `json:"auth_hash"`
	ScorePercent     float64 `json:"score_percent"`
	DebtPercent      float64 `json:"debt_percent"`
	DeficitPercent   float64 `json:"deficit_percent"`
	PredictedCredits float64 `json:"predicted_credits"`
	MinimumRemaining float64 `json:"minimum_remaining_percent"`
	Bottleneck       string  `json:"bottleneck"`
	ResetCreditBonus bool    `json:"reset_credit_bonus"`
}

type schedulerDecisionAudit struct {
	At               time.Time                 `json:"at"`
	Mode             string                    `json:"mode"`
	Model            string                    `json:"model"`
	SessionHash      string                    `json:"session_hash,omitempty"`
	LegacyAuthHash   string                    `json:"legacy_auth_hash,omitempty"`
	DynamicAuthHash  string                    `json:"dynamic_auth_hash,omitempty"`
	ReturnedAuthHash string                    `json:"returned_auth_hash,omitempty"`
	Disagreed        bool                      `json:"disagreed"`
	Candidates       []schedulerCandidateAudit `json:"candidates,omitempty"`
}

func normalizeModelName(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if slash := strings.LastIndex(value, "/"); slash >= 0 && slash+1 < len(value) {
		value = value[slash+1:]
	}
	return value
}

func normalizeReasoningEffort(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func builtInFallbackPricing(model string) modelPricing {
	model = normalizeModelName(model)
	switch model {
	case "gpt-5.6-luna":
		return modelPricing{Model: model, PromptPricePer1M: 1, CompletionPricePer1M: 6, CacheReadPricePer1M: 0.1, CacheWritePricePer1M: 1.25, PriceMultiplier: 1}
	case "gpt-5.6-terra", "gpt-5.4":
		return modelPricing{Model: model, PromptPricePer1M: 2.5, CompletionPricePer1M: 15, CacheReadPricePer1M: 0.25, CacheWritePricePer1M: 3.125, PriceMultiplier: 1}
	case "gpt-5.4-mini":
		return modelPricing{Model: model, PromptPricePer1M: 0.75, CompletionPricePer1M: 4.5, CacheReadPricePer1M: 0.075, PriceMultiplier: 1}
	case "gpt-5.3-codex-spark":
		return modelPricing{Model: model, PromptPricePer1M: 1.75, CompletionPricePer1M: 14, CacheReadPricePer1M: 0.175, PriceMultiplier: 1}
	default:
		// Unknown models deliberately use the most conservative currently
		// configured Codex rate rather than underestimating request cost.
		return modelPricing{Model: model, PromptPricePer1M: 5, CompletionPricePer1M: 30, CacheReadPricePer1M: 0.5, CacheWritePricePer1M: 6.25, PriceMultiplier: 1}
	}
}

func usageCredits(record pluginapi.UsageRecord, pricing map[string]modelPricing) (float64, bool) {
	model := normalizeModelName(record.Model)
	rate, ok := pricing[model]
	if !ok {
		rate = builtInFallbackPricing(model)
	}
	multiplier := rate.PriceMultiplier
	if multiplier < 0 {
		multiplier = 1
	}
	input := maxInt64Value(record.Detail.InputTokens, 0)
	output := maxInt64Value(record.Detail.OutputTokens, 0)
	cacheRead := maxInt64Value(record.Detail.CacheReadTokens, 0)
	if cacheRead == 0 {
		cacheRead = maxInt64Value(record.Detail.CachedTokens, 0)
	}
	cacheWrite := maxInt64Value(record.Detail.CacheCreationTokens, 0)
	if input == 0 && output == 0 && cacheRead == 0 && cacheWrite == 0 {
		return 0, false
	}
	uncached := input - cacheRead - cacheWrite
	if uncached < 0 {
		uncached = 0
	}
	credits := (float64(uncached)/1_000_000)*rate.PromptPricePer1M +
		(float64(cacheRead)/1_000_000)*rate.CacheReadPricePer1M +
		(float64(cacheWrite)/1_000_000)*rate.CacheWritePricePer1M +
		(float64(output)/1_000_000)*rate.CompletionPricePer1M
	return credits * multiplier, true
}

func maxInt64Value(value, floor int64) int64 {
	if value < floor {
		return floor
	}
	return value
}

func costSampleKey(model, effort string) string {
	return normalizeModelName(model) + "\x00" + normalizeReasoningEffort(effort)
}

func appendBoundedSample(samples []float64, value float64, limit int) []float64 {
	if limit < 1 {
		limit = 512
	}
	if len(samples) >= limit {
		copy(samples, samples[len(samples)-limit+1:])
		samples = samples[:limit-1]
	}
	return append(samples, value)
}

func sampleQuantile(samples []float64, quantile float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	copyOf := append([]float64(nil), samples...)
	sort.Float64s(copyOf)
	if quantile <= 0 {
		return copyOf[0]
	}
	if quantile >= 1 {
		return copyOf[len(copyOf)-1]
	}
	position := quantile * float64(len(copyOf)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return copyOf[lower]
	}
	weight := position - float64(lower)
	return copyOf[lower]*(1-weight) + copyOf[upper]*weight
}

func bootstrapCost(quantile float64) float64 {
	switch {
	case quantile >= 0.95:
		return 4.0
	case quantile >= 0.90:
		return 3.6
	default:
		return 3.0
	}
}

func (s *schedulerRuntimeState) observeUsageCost(record pluginapi.UsageRecord) {
	if !strings.EqualFold(strings.TrimSpace(record.Provider), providerCodex) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	credits, hasUsage := usageCredits(record, s.pricing)
	if credits < 0 || math.IsNaN(credits) || math.IsInf(credits, 0) {
		return
	}
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" && strings.TrimSpace(record.AuthIndex) != "" {
		authID = s.identities[strings.TrimSpace(record.AuthIndex)]
	}
	if authID != "" {
		state := s.pacingAccountLocked(authID)
		if len(state.PendingPredicted) > 0 {
			predicted := state.PendingPredicted[0]
			state.PendingPredicted = state.PendingPredicted[1:]
			if hasUsage {
				state.DeficitCredits += predicted - credits
			} else {
				// A completed request without token accounting should not leave a
				// speculative debit stuck forever.
				state.DeficitCredits += predicted
			}
		} else if hasUsage {
			state.DeficitCredits -= credits
		}
	}
	if !hasUsage || record.Failed || credits < minimumCostSampleCredits {
		return
	}
	if s.costSamples == nil {
		s.costSamples = make(map[string][]float64)
	}
	limit := s.cfg.CostSampleLimit
	effort := normalizeReasoningEffort(record.ReasoningEffort)
	model := normalizeModelName(record.Model)
	s.costSamples[costSampleKey(model, effort)] = appendBoundedSample(s.costSamples[costSampleKey(model, effort)], credits, limit)
	s.costSamples[costSampleKey(model, "*")] = appendBoundedSample(s.costSamples[costSampleKey(model, "*")], credits, limit)
	s.globalCostSamples = appendBoundedSample(s.globalCostSamples, credits, limit)
}

func (s *schedulerRuntimeState) pacingAccountLocked(authID string) *accountPacingState {
	if s.pacingAccounts == nil {
		s.pacingAccounts = make(map[string]*accountPacingState)
	}
	state := s.pacingAccounts[authID]
	if state == nil {
		state = &accountPacingState{Capacities: make(map[string]capacityEstimate), LastQuota: make(map[string]quotaCalibrationPoint)}
		s.pacingAccounts[authID] = state
	}
	if state.Capacities == nil {
		state.Capacities = make(map[string]capacityEstimate)
	}
	if state.LastQuota == nil {
		state.LastQuota = make(map[string]quotaCalibrationPoint)
	}
	return state
}

func (s *schedulerRuntimeState) updateCalibrationsLocked(quotas map[string]quotaSnapshot, now time.Time) {
	seen := make(map[string]struct{})
	for _, snapshot := range quotas {
		authID := strings.TrimSpace(snapshot.AuthID)
		if authID == "" {
			authID = strings.TrimSpace(snapshot.AuthIndex)
		}
		if authID == "" {
			continue
		}
		if _, ok := seen[authID]; ok {
			continue
		}
		seen[authID] = struct{}{}
		state := s.pacingAccountLocked(authID)
		for _, window := range snapshot.Windows {
			observedAt := window.ObservedAt
			if observedAt.IsZero() {
				observedAt = snapshot.RefreshedAt
			}
			point := quotaCalibrationPoint{
				UsedPercent:       window.UsedPercent,
				WindowUsageCredit: window.WindowUsageCredits,
				UsageKnown:        window.WindowUsageCreditsKnown,
				ResetAt:           window.ResetAt,
				RefreshedAt:       observedAt,
			}
			previous, hadPrevious := state.LastQuota[window.Class]
			if point.UsageKnown && point.UsedPercent >= 1 && (!hadPrevious || !point.RefreshedAt.Equal(previous.RefreshedAt)) {
				s.updateCapacityEstimateLocked(state, window.Class, point.WindowUsageCredit*100/point.UsedPercent, now)
			}
			if hadPrevious && point.UsageKnown && previous.UsageKnown && sameQuotaCycle(previous.ResetAt, point.ResetAt) {
				deltaUsed := point.UsedPercent - previous.UsedPercent
				deltaCredits := point.WindowUsageCredit - previous.WindowUsageCredit
				if deltaUsed >= 0.5 && deltaCredits > 0 {
					s.updateCapacityEstimateLocked(state, window.Class, deltaCredits*100/deltaUsed, now)
				}
			}
			state.LastQuota[window.Class] = point
		}
	}
}

func sameQuotaCycle(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return a.IsZero() && b.IsZero()
	}
	delta := a.Sub(b)
	if delta < 0 {
		delta = -delta
	}
	return delta <= 5*time.Minute
}

func (s *schedulerRuntimeState) updateCapacityEstimateLocked(state *accountPacingState, class string, sample float64, now time.Time) {
	if sample < 1 || sample > 10_000_000 || math.IsNaN(sample) || math.IsInf(sample, 0) {
		return
	}
	current := state.Capacities[class]
	if current.Samples > 0 && (sample < current.Credits*0.2 || sample > current.Credits*5) {
		return
	}
	if current.Samples == 0 {
		current.Credits = sample
	} else {
		current.Credits = current.Credits*(1-capacityEWMAAlpha) + sample*capacityEWMAAlpha
	}
	current.Samples++
	current.UpdatedAt = now
	state.Capacities[class] = current
}

func reserveForWindow(cfg pluginConfig, class string) float64 {
	switch class {
	case "5h":
		return cfg.Reserve5hPercent
	case "weekly":
		return cfg.ReserveWeeklyPercent
	case "monthly":
		return cfg.ReserveMonthlyPercent
	default:
		return math.Max(cfg.ReserveWeeklyPercent, cfg.ReserveMonthlyPercent)
	}
}

func effectiveWindowSeconds(window quotaWindow) int64 {
	if window.WindowSeconds > 0 {
		return window.WindowSeconds
	}
	switch window.Class {
	case "5h":
		return int64((5 * time.Hour).Seconds())
	case "weekly":
		return int64((7 * 24 * time.Hour).Seconds())
	case "monthly":
		return int64((30 * 24 * time.Hour).Seconds())
	default:
		return 0
	}
}

func windowPacingDebtPercent(window quotaWindow, reserve float64, now time.Time) float64 {
	seconds := effectiveWindowSeconds(window)
	if seconds <= 0 || window.ResetAt.IsZero() || !now.Before(window.ResetAt) {
		return 0
	}
	start := window.ResetAt.Add(-time.Duration(seconds) * time.Second)
	fraction := now.Sub(start).Seconds() / float64(seconds)
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	target := math.Max(0, 100-reserve) * fraction
	return target - window.UsedPercent
}

func extractMetadataString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		for candidate, value := range metadata {
			if !strings.EqualFold(strings.TrimSpace(candidate), key) {
				continue
			}
			if text, ok := value.(string); ok {
				return strings.TrimSpace(text)
			}
		}
	}
	for _, value := range metadata {
		if nested, ok := value.(map[string]any); ok {
			if text := extractMetadataString(nested, keys...); text != "" {
				return text
			}
		}
	}
	return ""
}

func requestReasoningEffort(req pluginapi.SchedulerPickRequest) string {
	return normalizeReasoningEffort(extractMetadataString(req.Options.Metadata, "reasoning_effort", "reasoningEffort", "effort"))
}

func highCostRequest(req pluginapi.SchedulerPickRequest) bool {
	effort := requestReasoningEffort(req)
	return effort == "max" || effort == "xhigh" || effort == "high" || strings.Contains(strings.ToLower(req.Model), "max")
}

func (s *schedulerRuntimeState) predictCostLocked(req pluginapi.SchedulerPickRequest, lowQuota bool) float64 {
	quantile := s.cfg.NormalCostQuantile
	if lowQuota {
		quantile = s.cfg.GuardCostQuantile
	}
	if highCostRequest(req) {
		quantile = s.cfg.HighCostQuantile
	}
	model := normalizeModelName(req.Model)
	effort := requestReasoningEffort(req)
	sets := [][]float64{
		s.costSamples[costSampleKey(model, effort)],
		s.costSamples[costSampleKey(model, "*")],
		s.globalCostSamples,
	}
	for _, samples := range sets {
		if len(samples) >= minimumQuantileSamples {
			if predicted := sampleQuantile(samples, quantile); predicted > 0 {
				return predicted
			}
		}
	}
	return bootstrapCost(quantile)
}

func quotaEvaluationWithOrder(snapshot quotaSnapshot, now time.Time, order []string) quotaEvaluation {
	result := quotaEvaluation{}
	for _, window := range snapshot.Windows {
		if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
			continue
		}
		result.ActiveWindows++
		if !window.Allowed {
			result.Known = true
			result.Bottleneck = window
			result.Reason = "not_allowed"
			return result
		}
		if window.LimitReached || window.UsedPercent >= usedPercentThreshold {
			result.Known = true
			result.Bottleneck = window
			result.Reason = "limit_reached"
			return result
		}
		if result.Bottleneck.Class == "" || windowMoreRestrictiveWithOrder(window, result.Bottleneck, order) {
			result.Bottleneck = window
		}
	}
	if result.ActiveWindows == 0 {
		result.Reason = "no_active_windows"
		return result
	}
	result.Known = true
	result.Eligible = true
	result.Reason = "eligible"
	return result
}

func windowMoreRestrictiveWithOrder(a, b quotaWindow, order []string) bool {
	if a.UsedPercent != b.UsedPercent {
		return a.UsedPercent > b.UsedPercent
	}
	return windowRankInOrder(a.Class, order) < windowRankInOrder(b.Class, order)
}

func windowRankInOrder(class string, order []string) int {
	for index, item := range order {
		if item == class {
			return index
		}
	}
	return len(order) + 10
}

func snapshotFreshWithConfig(snapshot quotaSnapshot, now time.Time, cfg pluginConfig) bool {
	return quotaSnapshotFresh(snapshot, now, cfg.StaleAfter)
}

func (s *schedulerRuntimeState) pacingPick(req pluginapi.SchedulerPickRequest, now time.Time) (pluginapi.SchedulerPickResponse, []pacingCandidate) {
	banned := make(map[string]bool, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if !banStore.schedulable(candidate.ID, now) {
			banned[candidate.ID] = true
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	choices := make([]pacingCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if banned[candidate.ID] {
			continue
		}
		snapshot, found := s.quotas[strings.TrimSpace(candidate.ID)]
		if !found || !snapshotFreshWithConfig(snapshot, now, s.cfg) {
			continue
		}
		evaluation := quotaEvaluationWithOrder(snapshot, now, s.cfg.WindowOrder)
		if !evaluation.Known || !evaluation.Eligible {
			continue
		}
		lowQuota := false
		minimumRemaining := 100.0
		for _, window := range snapshot.Windows {
			if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
				continue
			}
			remaining := 100 - window.UsedPercent
			if remaining < minimumRemaining {
				minimumRemaining = remaining
			}
			if remaining <= s.cfg.LowQuotaPercent {
				lowQuota = true
			}
		}
		predicted := s.predictCostLocked(req, lowQuota)
		pacingID := strings.TrimSpace(snapshot.AuthID)
		if pacingID == "" {
			pacingID = candidate.ID
		}
		state := s.pacingAccountLocked(pacingID)
		feasible := true
		debtPercent := math.Inf(1)
		targetRate := math.Inf(1)
		referenceCapacity := math.Inf(1)
		earliestReset := time.Time{}
		active := 0
		allCapacitiesKnown := true
		for _, window := range snapshot.Windows {
			if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
				continue
			}
			active++
			reserve := reserveForWindow(s.cfg, window.Class)
			remaining := 100 - window.UsedPercent
			capacity := state.Capacities[window.Class]
			predictedPercent := 1.0
			if capacity.Credits > 0 {
				predictedPercent = predicted * 100 / capacity.Credits
				if capacity.Credits < referenceCapacity {
					referenceCapacity = capacity.Credits
				}
			} else {
				allCapacitiesKnown = false
			}
			if remaining-predictedPercent < reserve {
				feasible = false
				break
			}
			debt := windowPacingDebtPercent(window, reserve, now)
			if debt < debtPercent {
				debtPercent = debt
			}
			if !window.ResetAt.IsZero() && (earliestReset.IsZero() || window.ResetAt.Before(earliestReset)) {
				earliestReset = window.ResetAt
			}
			if capacity.Credits > 0 && !window.ResetAt.IsZero() {
				secondsLeft := window.ResetAt.Sub(now).Seconds()
				if secondsLeft > 0 {
					rate := capacity.Credits * math.Max(0, remaining-reserve) / 100 / secondsLeft
					if rate < targetRate {
						targetRate = rate
					}
				}
			}
		}
		if !feasible || active == 0 {
			continue
		}
		if math.IsInf(debtPercent, 1) {
			debtPercent = 0
		}
		if math.IsInf(targetRate, 1) || !allCapacitiesKnown {
			targetRate = 0
		}
		if state.LastAccruedAt.IsZero() {
			state.LastAccruedAt = now
		} else if now.After(state.LastAccruedAt) {
			state.DeficitCredits += targetRate * now.Sub(state.LastAccruedAt).Seconds()
			state.LastAccruedAt = now
		}
		if !math.IsInf(referenceCapacity, 1) && referenceCapacity > 0 {
			limit := referenceCapacity * 0.25
			if state.DeficitCredits > limit {
				state.DeficitCredits = limit
			}
			if state.DeficitCredits < -limit {
				state.DeficitCredits = -limit
			}
		}
		deficitPercent := 0.0
		if !math.IsInf(referenceCapacity, 1) && referenceCapacity > 0 {
			deficitPercent = state.DeficitCredits * 100 / referenceCapacity
		} else {
			referenceCapacity = 0
		}
		score := debtPercent + deficitPercent
		resetCreditBonus := s.cfg.PreferResetCredits && snapshot.ResetCredits > 0 && minimumRemaining <= s.cfg.LowQuotaPercent
		if resetCreditBonus {
			score += 0.5
		}
		choices = append(choices, pacingCandidate{
			Candidate:         candidate,
			Snapshot:          snapshot,
			Bottleneck:        evaluation.Bottleneck,
			PredictedCredits:  predicted,
			ScorePercent:      score,
			DebtPercent:       debtPercent,
			DeficitPercent:    deficitPercent,
			MinimumRemaining:  minimumRemaining,
			TargetRate:        targetRate,
			ReferenceCapacity: referenceCapacity,
			EarliestReset:     earliestReset,
			ResetCreditBonus:  resetCreditBonus,
		})
	}
	if len(choices) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, choices
	}
	sort.SliceStable(choices, func(i, j int) bool {
		if choices[i].ScorePercent != choices[j].ScorePercent {
			return choices[i].ScorePercent > choices[j].ScorePercent
		}
		if !choices[i].EarliestReset.Equal(choices[j].EarliestReset) {
			if choices[i].EarliestReset.IsZero() {
				return false
			}
			if choices[j].EarliestReset.IsZero() {
				return true
			}
			return choices[i].EarliestReset.Before(choices[j].EarliestReset)
		}
		if choices[i].MinimumRemaining != choices[j].MinimumRemaining {
			return choices[i].MinimumRemaining < choices[j].MinimumRemaining
		}
		if choices[i].Candidate.Priority != choices[j].Candidate.Priority {
			return choices[i].Candidate.Priority > choices[j].Candidate.Priority
		}
		return choices[i].Candidate.ID < choices[j].Candidate.ID
	})
	selected := s.applyStickyLocked(req, choices, now)
	return pluginapi.SchedulerPickResponse{AuthID: selected.Candidate.ID, Handled: true}, choices
}

func schedulerHeader(options pluginapi.SchedulerOptions, name string) string {
	for key, values := range options.Headers {
		if !strings.EqualFold(strings.TrimSpace(key), name) || len(values) == 0 {
			continue
		}
		return strings.TrimSpace(values[0])
	}
	return ""
}

func schedulerSessionHash(req pluginapi.SchedulerPickRequest) string {
	value := schedulerHeader(req.Options, "X-Session-ID")
	if value == "" {
		value = schedulerHeader(req.Options, "Session-Id")
	}
	if value == "" {
		value = schedulerHeader(req.Options, "Session_id")
	}
	if value == "" {
		value = extractMetadataString(req.Options.Metadata, "session_id", "sessionId", "execution_session_id", "executionSessionId", "conversation_id", "conversationId")
	}
	if value == "" {
		value = schedulerHeader(req.Options, "X-Client-Request-Id")
	}
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}

func (s *schedulerRuntimeState) applyStickyLocked(req pluginapi.SchedulerPickRequest, choices []pacingCandidate, now time.Time) pacingCandidate {
	best := choices[0]
	if s.cfg.StickySeconds <= 0 {
		return best
	}
	session := schedulerSessionHash(req)
	if session == "" {
		return best
	}
	if s.stickyBindings == nil {
		s.stickyBindings = make(map[string]stickyBinding)
	}
	ttl := time.Duration(s.cfg.StickySeconds) * time.Second
	for key, binding := range s.stickyBindings {
		if now.Sub(binding.LastUsedAt) > ttl {
			delete(s.stickyBindings, key)
		}
	}
	binding, exists := s.stickyBindings[session]
	if !exists || now.Sub(binding.LastUsedAt) > ttl {
		s.stickyBindings[session] = stickyBinding{AuthID: best.Candidate.ID, LastUsedAt: now}
		return best
	}
	currentIndex := -1
	for index := range choices {
		if choices[index].Candidate.ID == binding.AuthID {
			currentIndex = index
			break
		}
	}
	if currentIndex < 0 {
		binding.AuthID = best.Candidate.ID
		binding.LastUsedAt = now
		binding.ChallengerID = ""
		binding.Confirmations = 0
		s.stickyBindings[session] = binding
		s.sessionSwitches++
		return best
	}
	current := choices[currentIndex]
	if current.Candidate.ID == best.Candidate.ID || best.ScorePercent-current.ScorePercent < s.cfg.SwitchHysteresisPercent {
		binding.LastUsedAt = now
		binding.ChallengerID = ""
		binding.Confirmations = 0
		s.stickyBindings[session] = binding
		return current
	}
	if binding.ChallengerID == best.Candidate.ID {
		binding.Confirmations++
	} else {
		binding.ChallengerID = best.Candidate.ID
		binding.Confirmations = 1
	}
	if binding.Confirmations < s.cfg.SwitchConfirmations {
		binding.LastUsedAt = now
		s.stickyBindings[session] = binding
		return current
	}
	binding.AuthID = best.Candidate.ID
	binding.LastUsedAt = now
	binding.ChallengerID = ""
	binding.Confirmations = 0
	s.stickyBindings[session] = binding
	s.sessionSwitches++
	return best
}

func (s *schedulerRuntimeState) recordPredictedDebit(authID string, predicted float64) {
	if strings.TrimSpace(authID) == "" || predicted <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.pacingAccountLocked(strings.TrimSpace(authID))
	state.DeficitCredits -= predicted
	state.PendingPredicted = append(state.PendingPredicted, predicted)
	if len(state.PendingPredicted) > 256 {
		state.PendingPredicted = state.PendingPredicted[len(state.PendingPredicted)-256:]
	}
}

func shortIdentityHash(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:8])
}

func (s *schedulerRuntimeState) recordDecisionAudit(req pluginapi.SchedulerPickRequest, mode string, legacy, dynamic, returned pluginapi.SchedulerPickResponse, candidates []pacingCandidate, now time.Time) {
	record := schedulerDecisionAudit{
		At:               now,
		Mode:             mode,
		Model:            req.Model,
		SessionHash:      schedulerSessionHash(req),
		LegacyAuthHash:   shortIdentityHash(legacy.AuthID),
		DynamicAuthHash:  shortIdentityHash(dynamic.AuthID),
		ReturnedAuthHash: shortIdentityHash(returned.AuthID),
		Disagreed:        legacy.Handled && dynamic.Handled && legacy.AuthID != dynamic.AuthID,
	}
	for _, candidate := range candidates {
		record.Candidates = append(record.Candidates, schedulerCandidateAudit{
			AuthHash:         shortIdentityHash(candidate.Candidate.ID),
			ScorePercent:     candidate.ScorePercent,
			DebtPercent:      candidate.DebtPercent,
			DeficitPercent:   candidate.DeficitPercent,
			PredictedCredits: candidate.PredictedCredits,
			MinimumRemaining: candidate.MinimumRemaining,
			Bottleneck:       candidate.Bottleneck.Class,
			ResetCreditBonus: candidate.ResetCreditBonus,
		})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if record.Disagreed {
		s.shadowDisagreements++
	}
	limit := s.cfg.DecisionHistoryLimit
	if limit < 1 {
		limit = 100
	}
	s.decisionHistory = append(s.decisionHistory, record)
	if len(s.decisionHistory) > limit {
		s.decisionHistory = append([]schedulerDecisionAudit(nil), s.decisionHistory[len(s.decisionHistory)-limit:]...)
	}
	if mode == "shadow" && record.Disagreed && s.cfg.ShadowLogInterval > 0 && (s.lastShadowLog.IsZero() || now.Sub(s.lastShadowLog) >= s.cfg.ShadowLogInterval) {
		s.lastShadowLog = now
		slog.Info("codex-quota-scheduler: shadow scheduler differs from legacy", "model", req.Model, "legacy_auth", record.LegacyAuthHash, "dynamic_auth", record.DynamicAuthHash, "disagreements", s.shadowDisagreements)
	}
}
