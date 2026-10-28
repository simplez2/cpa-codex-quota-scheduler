package main

import (
	"strings"
	"sync"
	"time"
)

// recoveryGuard is independent of ordinary concurrency limits. A native 100%
// observation creates a recovery requirement even when no 429 was recorded.
// Reservations use CPA's execution ID; dispatched requests never age out.
type recoveryGuard struct {
	mu       sync.Mutex
	accounts map[string]recoveryEntry
}

type recoveryEntry struct {
	AuthIndex       string
	ObservedAt      time.Time
	ResetAt         time.Time
	Ready           bool
	Owner           string
	OwnerAuthID     string
	ReservedAt      time.Time
	Dispatched      bool
	Failures        int
	RetryAt         time.Time
	Generation      uint64
	OwnerGeneration uint64
}

func (g *recoveryGuard) relatedLocked(id, index string) (string, recoveryEntry, bool) {
	e, ok := g.accounts[id]
	return id, e, ok
}

func (g *recoveryGuard) observe(snapshot quotaSnapshot, now time.Time, stale time.Duration) {
	id, index := strings.TrimSpace(snapshot.AuthID), strings.TrimSpace(snapshot.AuthIndex)
	if id == "" || index == "" || snapshot.RefreshedAt.IsZero() || snapshot.RefreshedAt.After(now) {
		return
	}
	var full *quotaWindow
	for i := range snapshot.Windows {
		w := &snapshot.Windows[i]
		if normalizeWindowClass(w.Class) == "5h" && w.UsedPercent >= usedPercentThreshold &&
			!w.ObservedAt.IsZero() && !w.ObservedAt.After(now) {
			full = w
			break
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.accounts == nil {
		g.accounts = make(map[string]recoveryEntry)
	}
	key, e, exists := g.relatedLocked(id, index)
	if exists && e.AuthIndex != index {
		if key == id {
			// A replacement credential binding needs a new observation before
			// the old recovery state can be forgotten or reassigned.
			e.AuthIndex, e.Ready = index, false
		}
	}
	if !exists {
		key = id
	}
	if full != nil {
		if !exists {
			e = recoveryEntry{AuthIndex: index, ObservedAt: full.ObservedAt, ResetAt: full.ResetAt, Generation: 1}
		} else {
			// A native quota request may have started before an asynchronous
			// 429 arrived; it still supplies the exhausted cycle's reset anchor.
			if full.ObservedAt.After(e.ObservedAt) {
				e.ObservedAt = full.ObservedAt
			}
			e.Generation++
			if full.ResetAt.After(e.ResetAt) {
				e.ResetAt = full.ResetAt
			}
			e.Ready = false
		}
		g.accounts[key] = e
		return
	}
	if !exists || e.Dispatched || !quotaSnapshotFresh(snapshot, now, stale) ||
		!snapshot.RefreshedAt.After(e.ObservedAt) {
		return
	}
	// A complete native no-5h response proves the shape changed; otherwise a
	// new, still active 5h cycle must be observed after the recorded reset.
	valid := snapshot.FiveHourLimitStatus == "not_reported"
	if !valid && (e.ResetAt.IsZero() || !snapshot.RefreshedAt.Before(e.ResetAt)) {
		for _, w := range snapshot.Windows {
			if normalizeWindowClass(w.Class) == "5h" && w.Allowed && !w.LimitReached &&
				w.UsedPercent < usedPercentThreshold && !w.ObservedAt.Before(e.ObservedAt) &&
				(e.ResetAt.IsZero() || !w.ObservedAt.Before(e.ResetAt)) &&
				!w.ResetAt.IsZero() && now.Before(w.ResetAt) {
				valid = true
			}
		}
	}
	for _, w := range snapshot.Windows {
		if !w.Allowed || w.LimitReached || w.UsedPercent >= usedPercentThreshold ||
			(!w.ResetAt.IsZero() && !now.Before(w.ResetAt)) {
			valid = false
		}
	}
	if valid {
		e.Ready = true
		g.accounts[key] = e
	}
}

func (g *recoveryGuard) reserve(id, index, requestID string, now time.Time) (bool, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key, e, exists := g.relatedLocked(id, index)
	if !exists {
		return true, false
	}
	if index != "" && e.AuthIndex != index {
		// A replacement binding must obtain its own fresh native observation.
		return false, true
	}
	if !e.Ready || now.Before(e.RetryAt) || requestID == "" {
		return false, true
	}
	if e.Owner != "" && e.Owner != requestID {
		// An un-dispatched reservation has a bounded lease; once dispatched,
		// only the terminal lifecycle event may release it.
		if e.Dispatched || now.Before(e.ReservedAt.Add(2*time.Minute)) {
			return false, true
		}
		e.Owner = ""
	}
	if e.Owner == requestID && e.OwnerAuthID != "" && e.OwnerAuthID != id {
		return false, true
	}
	if e.Owner == "" {
		e.Owner, e.OwnerAuthID, e.ReservedAt, e.Dispatched, e.OwnerGeneration = requestID, id, now, false, e.Generation
		g.accounts[key] = e
	}
	return true, true
}

func (g *recoveryGuard) dispatched(id, requestID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	key, e, exists := g.relatedLocked(id, "")
	if !exists {
		return true
	}
	if e.Owner != requestID || requestID == "" || (e.OwnerAuthID != "" && e.OwnerAuthID != id) {
		return false
	}
	e.Dispatched = true
	g.accounts[key] = e
	return true
}

func recoveryRetryDelay(failures int, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = 2 * time.Minute
	}
	if max <= 0 {
		max = 24 * time.Hour
	}
	if base > max {
		base = max
	}
	delay := base
	for i := 1; i < failures && delay < max; i++ {
		if delay > max/2 {
			return max
		}
		delay *= 2
	}
	return delay
}

func (g *recoveryGuard) finish(requestID string, success bool, now time.Time, base, max time.Duration) bool {
	return g.finishExcept(requestID, "", success, false, now, base, max)
}

func (g *recoveryGuard) finishExcept(requestID, exceptID string, success, preserveDispatched bool, now time.Time, base, max time.Duration) bool {
	if requestID == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	changed := false
	for id, e := range g.accounts {
		if e.Owner != requestID || id == exceptID || e.OwnerAuthID == exceptID && exceptID != "" {
			continue
		}
		if preserveDispatched && e.Dispatched {
			continue
		}
		changed = true
		if success && e.Dispatched && e.OwnerGeneration == e.Generation {
			delete(g.accounts, id)
			continue
		}
		if e.Dispatched {
			e.Failures++
			delay := recoveryRetryDelay(e.Failures, base, max)
			if until := now.Add(delay); until.After(e.RetryAt) {
				e.RetryAt = until
			}
			e.Ready = false
		}
		e.Owner, e.OwnerAuthID, e.ReservedAt, e.Dispatched = "", "", time.Time{}, false
		g.accounts[id] = e
	}
	return changed
}

func (g *recoveryGuard) require(id, index string, now, retryAt time.Time) {
	id, index = strings.TrimSpace(id), strings.TrimSpace(index)
	if id == "" || index == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.accounts == nil {
		g.accounts = make(map[string]recoveryEntry)
	}
	key, e, exists := g.relatedLocked(id, index)
	if !exists {
		key = id
		e = recoveryEntry{AuthIndex: index}
	}
	e.Generation++
	e.ObservedAt, e.Ready = now, false
	if retryAt.After(e.RetryAt) {
		e.RetryAt = retryAt
	}
	g.accounts[key] = e
}

func (g *recoveryGuard) clear(id string) {
	g.mu.Lock()
	id = strings.TrimSpace(id)
	key, _, exists := g.relatedLocked(id, "")
	if exists {
		delete(g.accounts, key)
	}
	g.mu.Unlock()
}

func (g *recoveryGuard) finishOther(requestID, selectedID string, now time.Time, base, max time.Duration) {
	g.mu.Lock()
	key, _, exists := g.relatedLocked(selectedID, "")
	g.mu.Unlock()
	if !exists {
		key = selectedID
	}
	g.finishExcept(requestID, key, false, false, now, base, max)
}

func (g *recoveryGuard) hasAny() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.accounts) != 0
}

func (g *recoveryGuard) hasAuth(id, index string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, _, ok := g.relatedLocked(strings.TrimSpace(id), strings.TrimSpace(index))
	return ok
}

func (g *recoveryGuard) knowsCredential(id, index string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.accounts[strings.TrimSpace(id)]
	return ok && index != "" && e.AuthIndex == strings.TrimSpace(index)
}

// blockedForPick never claims a permit. A changed or absent binding remains
// unknown and must retain the host's existing fallback behavior.
func (g *recoveryGuard) blockedForPick(id, index, requestID string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.accounts[strings.TrimSpace(id)]
	if !ok || index == "" || e.AuthIndex != strings.TrimSpace(index) {
		return false
	}
	if !e.Ready || now.Before(e.RetryAt) {
		return true
	}
	return e.Owner != "" && e.Owner != requestID && (e.Dispatched || now.Before(e.ReservedAt.Add(2*time.Minute)))
}

func (g *recoveryGuard) clearAll() {
	g.mu.Lock()
	g.accounts = make(map[string]recoveryEntry)
	g.mu.Unlock()
}

func (g *recoveryGuard) snapshot() map[string]recoveryEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]recoveryEntry, len(g.accounts))
	for id, e := range g.accounts {
		out[id] = e
	}
	return out
}

func (g *recoveryGuard) restore(entries map[string]recoveryEntry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.accounts == nil {
		g.accounts = make(map[string]recoveryEntry)
	}
	for id, incoming := range entries {
		if id == "" || incoming.AuthIndex == "" {
			continue
		}
		if existing, ok := g.accounts[id]; !ok || (!existing.Dispatched && incoming.ObservedAt.After(existing.ObservedAt)) {
			// A saved positive result is not a new observation in this process.
			// An in-flight lease is retained across a hot reload; it never expires
			// while its original model execution may still be running.
			incoming.Ready = false
			g.accounts[id] = incoming
		}
	}
}
