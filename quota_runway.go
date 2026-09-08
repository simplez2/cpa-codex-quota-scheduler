package main

import (
	"math"
	"strings"
	"time"
)

const (
	quotaRunwayForecastHorizon = 2 * time.Minute
	quotaRunwayMaxSampleGap    = 15 * time.Minute
	quotaRunwayResetTolerance  = 5 * time.Second
	quotaRunwayRecentSamples   = 8
	quotaRunwayMinimumSamples  = 2
	quotaRunwayMaximumReserve  = 50.0
)

type quotaRunwayKey struct {
	authID, authIndex, window string
}

type quotaRunwaySample struct {
	at   time.Time
	rate float64
}

type quotaRunwayObservation struct {
	observedAt    time.Time
	resetAt       time.Time
	windowSeconds int64
	usedPercent   float64
	ewma          float64
	samples       []quotaRunwaySample
}

// quotaRunwayTracker learns percentage points per hour separately for each
// credential and window. It is deliberately not persisted: a reload starts with
// the configured static reserves. The caller serializes access with runtime.mu.
// Neither these rates nor the two-minute forecast represent an inflight count,
// an absolute account capacity, or a guarantee about the next request's cost.
type quotaRunwayTracker struct {
	bindings map[string]string
	windows  map[quotaRunwayKey]quotaRunwayObservation
}

type quotaRunwayWindowAssessment struct {
	Window                  string      `json:"window"`
	Source                  quotaSource `json:"source"`
	ObservedAt              time.Time   `json:"observed_at"`
	ResetAt                 time.Time   `json:"reset_at"`
	Samples                 int         `json:"samples"`
	RateKnown               bool        `json:"rate_known"`
	BurnPercentPerHour      float64     `json:"burn_percent_per_hour"`
	ObservationAgeSeconds   float64     `json:"observation_age_seconds"`
	RemainingPercent        float64     `json:"remaining_percent"`
	StaticReservePercent    float64     `json:"static_reserve_percent"`
	CacheDebitPercent       float64     `json:"cache_debit_percent"`
	ForecastReservePercent  float64     `json:"forecast_reserve_percent"`
	EffectiveReservePercent float64     `json:"effective_reserve_percent"`
	HeadroomPercent         float64     `json:"headroom_percent"`
	RunwaySeconds           *float64    `json:"runway_seconds"`
	HardLimited             bool        `json:"hard_limited"`
	Fresh                   bool        `json:"fresh"`
	Reason                  string      `json:"reason"`
}

func (t *quotaRunwayTracker) ForgetAuth(authID string) {
	authID = strings.TrimSpace(authID)
	delete(t.bindings, authID)
	for key := range t.windows {
		if key.authID == authID {
			delete(t.windows, key)
		}
	}
}

func quotaRunwayFinitePercent(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 100
}

func quotaRunwaySameReset(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	difference := a.Sub(b)
	return difference >= -quotaRunwayResetTolerance && difference <= quotaRunwayResetTolerance
}

// Observe accepts only an original probe snapshot, before header/partial merges.
// Equal timestamps do not add samples. Discontinuities establish a new baseline
// and require two subsequent positive increments before predicting again.
func (t *quotaRunwayTracker) Observe(snapshot quotaSnapshot, now time.Time) {
	for _, window := range snapshot.Windows {
		if window.Source != quotaSourceProbe {
			return
		}
	}
	authID, authIndex := strings.TrimSpace(snapshot.AuthID), strings.TrimSpace(snapshot.AuthIndex)
	if authID == "" {
		return
	}
	if authIndex == "" || snapshot.RefreshedAt.IsZero() || snapshot.RefreshedAt.After(now) ||
		now.Sub(snapshot.RefreshedAt) > quotaRunwayMaxSampleGap {
		t.ForgetAuth(authID)
		return
	}
	if t.bindings == nil {
		t.bindings = make(map[string]string)
		t.windows = make(map[quotaRunwayKey]quotaRunwayObservation)
	}
	if previous, ok := t.bindings[authID]; ok && previous != authIndex {
		t.ForgetAuth(authID)
	}
	t.bindings[authID] = authIndex
	seen := make(map[quotaRunwayKey]bool)
	for _, window := range snapshot.Windows {
		class := normalizeWindowClass(window.Class)
		key := quotaRunwayKey{authID: authID, authIndex: authIndex, window: class}
		if seen[key] {
			// Ambiguous duplicate classes are not independent measurements.
			delete(t.windows, key)
			continue
		}
		seen[key] = true
		if class == "" || !quotaRunwayFinitePercent(window.UsedPercent) || window.WindowSeconds <= 0 ||
			window.ObservedAt.IsZero() || !window.ObservedAt.Equal(snapshot.RefreshedAt) ||
			window.ResetAt.IsZero() || !now.Before(window.ResetAt) ||
			quotaWindowHasPlaceholderReset(window, snapshot.RefreshedAt, now) {
			delete(t.windows, key)
			continue
		}
		baseline := quotaRunwayObservation{observedAt: window.ObservedAt, resetAt: window.ResetAt,
			windowSeconds: window.WindowSeconds, usedPercent: window.UsedPercent}
		previous, exists := t.windows[key]
		elapsed := window.ObservedAt.Sub(previous.observedAt)
		if !exists || !quotaRunwaySameReset(previous.resetAt, window.ResetAt) ||
			previous.windowSeconds != window.WindowSeconds || elapsed < 0 || elapsed > quotaRunwayMaxSampleGap ||
			window.UsedPercent < previous.usedPercent {
			t.windows[key] = baseline
			continue
		}
		if elapsed == 0 {
			if window.UsedPercent != previous.usedPercent {
				t.windows[key] = baseline
			}
			continue
		}
		// Retain only a short history. Idle or stopped traffic cannot leave a
		// historical burst classified as a current consumption rate forever.
		kept := previous.samples[:0]
		for _, sample := range previous.samples {
			if window.ObservedAt.Sub(sample.at) <= quotaRunwayMaxSampleGap {
				kept = append(kept, sample)
			}
		}
		previous.samples = kept
		if len(kept) == 0 {
			previous.ewma = 0
		}
		delta := window.UsedPercent - previous.usedPercent
		if delta > 0 {
			rate := delta / elapsed.Hours()
			if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
				t.windows[key] = baseline
				continue
			}
			if len(previous.samples) == 0 {
				previous.ewma = rate
			} else {
				previous.ewma = previous.ewma*0.75 + rate*0.25
			}
			previous.samples = append(previous.samples, quotaRunwaySample{at: window.ObservedAt, rate: rate})
			if len(previous.samples) > quotaRunwayRecentSamples {
				previous.samples = previous.samples[len(previous.samples)-quotaRunwayRecentSamples:]
			}
		}
		previous.observedAt, previous.usedPercent = window.ObservedAt, window.UsedPercent
		// Keep the original cycle anchor so small reset drift cannot accumulate.
		t.windows[key] = previous
	}
	for key := range t.windows {
		if key.authID == authID && !seen[key] {
			delete(t.windows, key)
		}
	}
}

func quotaRunwayStaticReserve(cfg pluginConfig, class string) float64 {
	reserve := reserveForWindow(cfg, class)
	if !quotaRunwayFinitePercent(reserve) {
		return 100
	}
	return reserve
}

// Assess does not teach the estimator. Header/mixed windows may retain a
// stricter observed usage, but their timestamp never makes the original probe
// rate fresher. Safety headroom is a heuristic; hard limits remain authoritative.
func (t *quotaRunwayTracker) Assess(snapshot quotaSnapshot, cfg pluginConfig, now time.Time) []quotaRunwayWindowAssessment {
	out := make([]quotaRunwayWindowAssessment, 0, len(snapshot.Windows))
	authID, authIndex := strings.TrimSpace(snapshot.AuthID), strings.TrimSpace(snapshot.AuthIndex)
	for _, window := range snapshot.Windows {
		class := normalizeWindowClass(window.Class)
		if class == "" {
			class = "unknown"
		}
		reserve := quotaRunwayStaticReserve(cfg, class)
		item := quotaRunwayWindowAssessment{Window: class, Source: window.Source, ObservedAt: window.ObservedAt,
			ResetAt: window.ResetAt, StaticReservePercent: reserve, EffectiveReservePercent: reserve,
			Reason: "static_reserve"}
		validPercent := quotaRunwayFinitePercent(window.UsedPercent)
		if validPercent {
			item.RemainingPercent = 100 - window.UsedPercent
		}
		item.HardLimited = !window.Allowed || window.LimitReached || (validPercent && window.UsedPercent >= usedPercentThreshold)
		item.Fresh = validPercent && quotaSnapshotFresh(snapshot, now, cfg.StaleAfter) &&
			!window.ObservedAt.IsZero() && !window.ObservedAt.After(now) &&
			now.Sub(window.ObservedAt) <= cfg.StaleAfter &&
			(window.ResetAt.IsZero() || now.Before(window.ResetAt))
		if !window.ObservedAt.IsZero() && !window.ObservedAt.After(now) {
			item.ObservationAgeSeconds = now.Sub(window.ObservedAt).Seconds()
		}
		observation, found := t.windows[quotaRunwayKey{authID: authID, authIndex: authIndex, window: class}]
		trusted := found && authID != "" && authIndex != "" && t.bindings[authID] == authIndex &&
			quotaRunwaySameReset(observation.resetAt, window.ResetAt) && observation.windowSeconds == window.WindowSeconds &&
			!observation.observedAt.After(now) && now.Before(observation.resetAt)
		if trusted {
			// A lagging completion header must not lower known probe usage.
			item.RemainingPercent = math.Min(item.RemainingPercent, 100-observation.usedPercent)
			item.ObservedAt = observation.observedAt
			age := now.Sub(observation.observedAt)
			item.ObservationAgeSeconds = age.Seconds()
			peak := 0.0
			for _, sample := range observation.samples {
				if !sample.at.After(now) && now.Sub(sample.at) <= quotaRunwayMaxSampleGap {
					item.Samples++
					peak = math.Max(peak, sample.rate)
				}
			}
			if item.Fresh && age <= cfg.StaleAfter && age <= quotaRunwayMaxSampleGap && item.Samples >= quotaRunwayMinimumSamples {
				item.RateKnown = true
				item.BurnPercentPerHour = math.Max(peak, observation.ewma)
				// Deduct predicted consumption since the original probe, without
				// charging again for a stricter header that already observes it.
				alreadyObserved := math.Max(0, window.UsedPercent-observation.usedPercent)
				item.CacheDebitPercent = math.Max(0, item.BurnPercentPerHour*age.Hours()-alreadyObserved)
				item.ForecastReservePercent = item.BurnPercentPerHour * quotaRunwayForecastHorizon.Hours()
				item.EffectiveReservePercent = math.Max(reserve, math.Min(quotaRunwayMaximumReserve, reserve+item.ForecastReservePercent))
				item.Reason = "probe_rate_guard"
			}
		}
		item.HeadroomPercent = math.Max(0, item.RemainingPercent-item.CacheDebitPercent-item.EffectiveReservePercent)
		if !item.Fresh {
			item.Reason = "quota_stale_or_invalid"
		}
		if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
			item.Reason = "reset_unconfirmed"
		}
		if item.HardLimited {
			item.HeadroomPercent = 0
			item.Reason = "hard_limit"
		}
		if item.RateKnown && item.BurnPercentPerHour > 0 {
			seconds := item.HeadroomPercent / item.BurnPercentPerHour * 3600
			item.RunwaySeconds = &seconds
		}
		out = append(out, item)
	}
	return out
}
